package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

// Inspect a disposable page. Observations are never acceptance evidence.
type MeadowBrowserInspect struct {
	Checker *MeadowBrowserInteractionCheck
}

func (*MeadowBrowserInspect) Name() string                     { return "meadow_browser_inspect" }
func (*MeadowBrowserInspect) Kind() Kind                       { return KindExecute }
func (*MeadowBrowserInspect) ParallelSafe(map[string]any) bool { return false }
func (t *MeadowBrowserInspect) Description() string {
	surface := "全新隔離瀏覽器"
	if t.Checker != nil && t.Checker.BaseURL == desktopBrowserPrivateBase {
		surface = "本對話的右側瀏覽器（重新載入受測頁面）"
	}
	return "在" + surface + "觀察已儲存 HTML 的真實控制項與狀態。可提供 setup_steps（fill/click/press_key）先操作，再觀察動態控制項與狀態路徑，不必為探索硬寫斷言。只觀察，不會宣稱驗收通過。Canvas 角色不等於 DOM 元素；需要內部狀態時使用實際同步唯讀 meadowTest.getState()，不能發明選擇器或成功旗標。"
}
func (*MeadowBrowserInspect) Schema() map[string]any {
	return obj(map[string]any{"file_path": str("Saved local HTML artifact to inspect"),
		"setup_steps": map[string]any{"type": "array", "minItems": 1, "maxItems": 10, "description": "Optional inputs on a fresh local artifact before observation. No assertions or arbitrary JS. Page event handlers can have network side effects: use only inputs authorized by the task; the worker is not a network sandbox.", "items": obj(map[string]any{
			"action":   map[string]any{"type": "string", "enum": []string{"fill", "click", "press_key"}},
			"selector": str("Observed CSS selector; optional for keyboard input"), "text": str("fill text"), "key": str("KeyboardEvent.key, e.g. Enter or ArrowLeft"),
		}, "action")}}, "file_path")
}
func (t *MeadowBrowserInspect) Execute(ctx context.Context, args map[string]any) Result {
	if t.Checker == nil || t.Checker.WS == nil {
		return errResultf("inspection workspace unavailable")
	}
	ctx = desktopBrowserPageContext(ctx, t.Checker.BaseURL)
	for key := range args {
		if key != "file_path" && key != "setup_steps" {
			return errResultf("inspect accepts only file_path/setup_steps")
		}
	}
	var setup []interactionStep
	if raw, exists := args["setup_steps"]; exists {
		items, ok := raw.([]any)
		if !ok || len(items) < 1 || len(items) > 10 {
			return interactionInvalidCall("setup_steps requires 1..10 inputs")
		}
		for _, item := range items {
			step, ok := item.(map[string]any)
			if !ok {
				return interactionInvalidCall("setup step must be an object")
			}
			for key := range step {
				if key != "action" && key != "selector" && key != "text" && key != "key" {
					return interactionInvalidCall("unknown setup field: " + key)
				}
			}
			switch strArg(step, "action") {
			case "fill", "click", "press_key":
			default:
				return interactionInvalidCall("setup only allows fill/click/press_key; no assertions")
			}
		}
		var err error
		setup, err = parseInteractionSteps(raw, false)
		if err != nil {
			return interactionInvalidCall(err.Error())
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := t.Checker
	h := &MeadowBrowserVisualCheck{WS: c.WS, BaseURL: c.BaseURL, HTTP: c.HTTP, StartLocal: c.StartLocal, RuntimeIdentity: c.RuntimeIdentity}
	_, path, err := h.targetURL(args)
	if err != nil {
		return errResult(err)
	}
	raw, err := readBehaviorFile(path, 32*1024*1024)
	if err != nil {
		return errResult(err)
	}
	target, remove, err := snapshotLocalWebArtifact(path)
	if err != nil {
		return errResult(err)
	}
	defer remove()
	base := h.base()
	ownedWorker := c.NewWorker != nil || (c.BaseURL == "" && c.HTTP == nil && c.StartLocal == nil)
	if ownedWorker {
		factory := c.NewWorker
		if factory == nil {
			factory = func(ctx context.Context) (string, func(), error) { return startInteractionWorker(ctx, h) }
		}
		workerBase, cleanup, err := factory(ctx)
		if err != nil {
			return errResult(err)
		}
		defer cleanup()
		base = workerBase
	} else if _, _, err := h.ensureAvailable(ctx, base); err != nil {
		return browserRuntimeGuardResult(err)
	}
	if err = h.postJSON(ctx, base+"/goto", map[string]any{"url": target, "wait_until": "load", "wait_strategy": "paint", "settle_ms": 120}, nil); err != nil {
		return errResult(err)
	}
	if err = verifyInteractionClock(ctx, h, base); err != nil {
		return errResultf("INSPECT_ENVIRONMENT_UNAVAILABLE: %v; no application assertion tested", err)
	}
	for i, step := range setup {
		switch step.Action {
		case "fill":
			err = h.postJSON(ctx, base+"/fill", map[string]any{"selector": step.Selector, "text": step.Text}, nil)
		case "click":
			err = h.postJSON(ctx, base+"/click", map[string]any{"selector": step.Selector}, nil)
		case "press_key":
			payload := map[string]any{"key": step.Key}
			if step.Selector != "" {
				payload["selector"] = step.Selector
			}
			err = h.postJSON(ctx, base+"/press", payload, nil)
		}
		if err != nil {
			return errResult(&RecoveryError{Code: "browser_inspection_setup_unavailable", Message: fmt.Sprintf("INSPECT_SETUP_UNAVAILABLE at input %d: %v; no acceptance assertion ran", i+1, err), RequiredAction: "inspect_actual_controls_and_correct_setup", RequiredArgs: map[string]any{"setup_inputs_executed": i, "failed_input": i + 1, "assertions_executed": 0}, PreserveDraft: true})
		}
		timer := time.NewTimer(120 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errResult(ctx.Err())
		case <-timer.C:
		}
	}
	// JSON string envelope reuses strict scalar transport decoding. Bound work,
	// paths and output; do not evaluate caller-supplied code or call debug setters.
	code := `(()=>{` + strictObservationJSONJS + `const out={controls:[],state:null,state_error:"",paths:[],truncated:false};
 const nodes=Array.from(document.querySelectorAll('button,input,select,textarea,a[href],[role="button"],[id]'));
 out.truncated=nodes.length>64;
 for(const el of nodes.slice(0,64)){let parts=[],p=el;while(p&&p.nodeType===1&&parts.length<12){if(p.id){parts.unshift('#'+CSS.escape(p.id));break;}let n=1,q=p;while(q=q.previousElementSibling)if(q.tagName===p.tagName)n++;parts.unshift(p.tagName.toLowerCase()+':nth-of-type('+n+')');p=p.parentElement;}const r=el.getBoundingClientRect(),s=getComputedStyle(el);out.controls.push({selector:parts.join(' > '),tag:el.tagName.toLowerCase(),text:(el.innerText||el.getAttribute('aria-label')||'').slice(0,80),type:el.getAttribute('type')||'',visible:!!(r.width&&r.height&&s.display!=='none'&&s.visibility!=='hidden'),disabled:!!el.disabled});}
 try{if(!window.meadowTest||typeof window.meadowTest.getState!=='function')throw Error('read-only window.meadowTest.getState() unavailable');const state=window.meadowTest.getState();if(!state||typeof state!=='object'||typeof state.then==='function')throw Error('getState must return synchronous JSON object');const encoded=meadowJSON(state);if(encoded.length>6000)throw Error('state exceeds 6000 characters; expose a compact raw projection');out.state=JSON.parse(encoded);const walk=(v,p,d)=>{if(out.paths.length>=128){out.truncated=true;return;}out.paths.push({path:p,type:Array.isArray(v)?'array':v===null?'null':typeof v});if(v&&typeof v==='object'){const keys=Array.isArray(v)?['length',...Object.keys(v)]:Object.keys(v);if(d>=5||keys.length>32)out.truncated=true;if(d<5)for(const k of keys.slice(0,32))walk(v[k],p?p+'.'+k:k,d+1);}};walk(out.state,'',0);}catch(e){out.state_error=String(e.message||e).slice(0,300);}
 return {__meadow_observation:{found:true,value:JSON.stringify(out),error:""}};})()`
	var response json.RawMessage
	if err = h.postJSON(ctx, base+"/js", map[string]any{"code": code}, &response); err != nil {
		return errResult(err)
	}
	// Inspection output has a separate larger bound than individual scalar reads.
	value, err := decodeInspectionEnvelope(response)
	if err != nil {
		return errResult(err)
	}
	scope := "僅觀察；沒有行為驗收通過。控制項和狀態是在隔離頁面完成所列 setup inputs 後取得；app state 為頁面回報。"
	if c.BaseURL == desktopBrowserPrivateBase {
		scope = "僅觀察；沒有行為驗收通過。控制項和狀態是在本對話的右側瀏覽器重新載入頁面並完成所列 setup inputs 後取得；app state 為頁面回報。"
	}
	data, _ := json.Marshal(map[string]any{"scope": scope, "setup_inputs_executed": len(setup), "assertions_executed": 0, "file_path": c.WS.ModelPath(path), "source_sha256": fmt.Sprintf("%x", sha256.Sum256(raw)), "animation_clock_verified": true, "observed": value, "state_contract": "app path 是真實 own-property 路徑。陣列使用 items.length 或 items.0.text，items.text 不會隱式映射。只使用觀察到的路徑。DOM text 永遠是字串，DOM count 是數字。沒有 hook 時可直接使用 DOM；不能捏造成功旗標。", "timing": "每次呼叫獨立重新載入；驗收案例須重新包含自己的前置動作。setup 僅派送瀏覽器事件，不是 OS trusted input 或持續按住。"})
	return Result{LLMContent: string(data), Display: "已讀取真實控制項與狀態（尚未驗收）"}
}

// Reject lossy JSON coercions before serialization: undefined, holes, NaN,
// custom objects/accessors and cycles must never become passing null values.
const strictObservationJSONJS = `const meadowJSON=(root)=>{let count=0;const seen=new Set();const check=(v,d)=>{if(++count>4096||d>12)throw Error('state structure exceeds bound');if(v===null||typeof v==='string'||typeof v==='boolean')return;if(typeof v==='number'){if(!Number.isFinite(v))throw Error('nonfinite state');return;}if(typeof v!=='object')throw Error('state contains unsupported JSON value');if(seen.has(v))throw Error('cyclic state');const a=Array.isArray(v);if(!a&&Object.getPrototypeOf(v)!==Object.prototype&&Object.getPrototypeOf(v)!==null)throw Error('state must use plain JSON objects');seen.add(v);if(a){if(v.length>4096)throw Error('array too large');for(let i=0;i<v.length;i++){const p=Object.getOwnPropertyDescriptor(v,String(i));if(!p||!('value' in p))throw Error('sparse/accessor state');check(p.value,d+1);}}else{const keys=Object.keys(v);if(keys.length>4096)throw Error('object too large');for(const k of keys){const p=Object.getOwnPropertyDescriptor(v,k);if(!p||!('value' in p))throw Error('accessor state');check(p.value,d+1);}}seen.delete(v);};check(root,0);return JSON.stringify(root);};`

func decodeInspectionEnvelope(raw json.RawMessage) (any, error) {
	if len(raw) > 128*1024 {
		return nil, fmt.Errorf("inspection exceeds 128 KiB")
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, fmt.Errorf("invalid inspection envelope")
	}
	for n := 0; n < 8; n++ {
		switch x := v.(type) {
		case string:
			if json.Unmarshal([]byte(x), &v) != nil {
				return nil, fmt.Errorf("invalid inspection encoding")
			}
		case map[string]any:
			if o, ok := x["__meadow_observation"].(map[string]any); ok {
				if o["found"] != true || o["error"] != "" {
					return nil, fmt.Errorf("inspection observation failed")
				}
				s, ok := o["value"].(string)
				if !ok || len(s) > 48000 {
					return nil, fmt.Errorf("invalid inspection payload")
				}
				var result map[string]any
				if json.Unmarshal([]byte(s), &result) != nil {
					return nil, fmt.Errorf("invalid inspection JSON")
				}
				return result, nil
			}
			if len(x) != 1 {
				return nil, fmt.Errorf("unknown inspection envelope")
			}
			found := false
			for _, k := range []string{"result", "v", "value"} {
				if child, ok := x[k]; ok {
					v = child
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("missing inspection envelope")
			}
		default:
			return nil, fmt.Errorf("invalid inspection envelope")
		}
	}
	return nil, fmt.Errorf("inspection envelope nesting exceeded")
}
