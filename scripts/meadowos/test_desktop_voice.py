"""Host voice lifecycle contracts using browser/permission doubles.

These tests run the actual noVNC companion module. They never open a microphone,
start an Agent, call speech/provider services, or use a live desktop. Physical
browser microphone permission and playback require separate acceptance.
"""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent

COMMON = r"""
import assert from 'node:assert/strict';
import {installDesktopVoice} from './subject.mjs';
const flush=async()=>{for(let n=0;n<8;n++)await new Promise(r=>setImmediate(r));};
class Target {
 constructor(tag=''){this.tagName=tag.toUpperCase();this.listeners=new Map();this.style={};this.dataset={};this.attributes=new Map();}
 addEventListener(type,fn){if(!this.listeners.has(type))this.listeners.set(type,new Set());this.listeners.get(type).add(fn);}
 removeEventListener(type,fn){this.listeners.get(type)?.delete(fn);}
 dispatch(type,fields={}){const e={type,target:this,isTrusted:false,stopPropagation(){},preventDefault(){},...fields};for(const fn of [...(this.listeners.get(type)||[])])fn(e);return e;}
 setAttribute(k,v){this.attributes.set(k,v);}
 getAttribute(k){return this.attributes.get(k);}
 remove(){this.removed=true;}
 contains(el){return el===this||el?.parentElement===this;}
 closest(selector){if(selector.includes('canvas')&&this.tagName==='CANVAS')return this;return null;}
}
function world({requested=true, stateFailure=false}={}) {
 const w={mediaPending:[],leaseCalls:0,buttons:[],timers:[],tracks:[],nodes:[],actions:[],requests:[],getMedia:0,state:{epoch:'epoch-test',active:true,microphoneRequested:requested,speech:null},stateFailure};
 const doc=new Target(); doc.hidden=false; doc.visibilityState='visible';doc.body=new Target('body'); doc.body.appendChild=()=>{};
 doc.createElement=tag=>{const el=new Target(tag);if(tag==='button')w.buttons.push(el);return el;};
 doc.querySelector=selector=>selector.includes('canvas')?w.canvas:null;doc.getElementById=id=>id==='noVNC_container'?w.container:null;
 globalThis.document=doc;globalThis.window=new Target();w.now=1000;Object.defineProperty(globalThis,'performance',{configurable:true,value:{now:()=>w.now}});
 const canvas=w.canvas=new Target('canvas'),container=w.container=new Target('div');canvas.parentElement=container;
 globalThis.setTimeout=(fn,delay)=>{const t={fn,delay,active:true};w.timers.push(t);return t;};globalThis.clearTimeout=t=>{if(t)t.active=false;};
 Object.defineProperty(globalThis,'navigator',{configurable:true,value:{mediaDevices:{getUserMedia:async()=>{w.getMedia++;if(w.mediaError)throw w.mediaError;const result=()=>{const t=new Target();t.stopped=false;t.stop=()=>{t.stopped=true;};w.tracks.push(t);return{getTracks:()=>[t]};};if(w.deferMedia)return new Promise(resolve=>w.mediaPending.push(()=>resolve(result())));return result();}}}});
 class Context{constructor(){this.sampleRate=48000;this.destination={};this.audioWorklet={addModule:async()=>{}};}async resume(){}async close(){}createMediaStreamSource(){return{connect(){},disconnect(){}};}}
 window.AudioContext=Context;
 globalThis.AudioWorkletNode=class{constructor(){this.port={postMessage(){}};w.nodes.push(this);}connect(){}disconnect(){}};
 globalThis.Audio=class{};
 globalThis.fetch=async(url,opts={})=>{
  const route=new URL(url).pathname;let body=typeof opts.body==='string'?JSON.parse(opts.body):null;
  w.requests.push({route,body});
  if(route==='/voice/state'){if(w.stateFailure)throw new Error('fixture service unavailable');return{ok:true,json:async()=>({native:structuredClone(w.state),capabilities:{asr:true,tts:true}})};}
  if(route==='/voice/lease'){w.leaseCalls++;if(w.leaseErrorAt===w.leaseCalls)throw new Error('fixture lease request failed');return{ok:true,json:async()=>({owned:!body.release})};}
  if(route==='/voice/action'){w.actions.push(body);return{ok:true,json:async()=>({accepted:true})};}
  if(route==='/voice/transcribe')return{ok:true,json:async()=>({text:'這是隔離測試',epoch:w.state.epoch})};
  throw Error('Unexpected route '+route);
 };
 w.rfb=new Target();w.rfb._canvas=canvas;w.rfb._target=container;installDesktopVoice(w.rfb);
 w.tick=async()=>{const t=w.timers.find(t=>t.active);assert(t,'scheduled poll');t.active=false;await t.fn();await flush();};
 w.tickPending=async()=>{const t=w.timers.find(t=>t.active);assert(t,'scheduled poll');t.active=false;let done=false;Promise.resolve(t.fn()).then(()=>done=true);await flush();return ()=>done;};
 w.pointer=async(isTrusted=true)=>{const fields={target:canvas,isTrusted};canvas.dispatch('pointerdown',fields);container.dispatch('pointerdown',fields);doc.dispatch('pointerdown',fields);window.dispatch('pointerdown',fields);await flush();};
 w.feed=async(rms,count)=>{for(let i=0;i<count;i++){w.now+=20;const pcm=new Int16Array(960).fill(Math.round(rms*32767));w.nodes.at(-1).port.onmessage({data:{type:'audio',pcm:pcm.buffer,rms,peak:rms}});await flush();}};
 w.finish=()=>w.rfb._meadowVoiceDispose();return w;
}
"""


@unittest.skipUnless(shutil.which("node"), "Node required")
class DesktopVoiceContract(unittest.TestCase):
    def run_contract(self, script):
        with tempfile.TemporaryDirectory(prefix="meadow-voice-contract-") as temporary:
            root = Path(temporary)
            shutil.copyfile(ROOT / "assets/desktop-voice.js", root / "subject.mjs")
            harness = root / "contract.mjs"
            harness.write_text(COMMON + "\n" + script)
            result = subprocess.run(
                [shutil.which("node"), str(harness)], cwd=root,
                capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_pending_permission_keeps_polling_and_closing_stops_late_media(self):
        self.run_contract(r"""
const w=world(); await flush();
try {
  w.deferMedia=true;
  await w.pointer(true); await w.tick();
  assert.equal(w.getMedia,1);
  assert.equal(w.buttons[0].textContent,'請允許瀏覽器使用麥克風');
  assert.equal(w.buttons[0].style.display,'block');
  for(let i=0;i<3;i++){ w.now+=3000; await w.tick(); }
  assert.equal(w.getMedia,1,'pending permission must not request a second microphone');
  assert(w.leaseCalls>1,'the owned lease must renew while permission is pending');
  w.state.active=false; w.state.epoch='epoch-closed'; await w.tick();
  assert.equal(w.buttons[0].style.display,'none');
  for(const resolve of w.mediaPending.splice(0)) resolve();
  await flush();
  assert.equal(w.tracks.length,1);
  assert(w.tracks.every(track=>track.stopped),'late approved media must stop after native close');
  assert(!w.actions.some(action=>action.type==='microphone'&&action.status==='listening'));
} finally { w.finish(); }
""")

    def test_new_epoch_releases_old_owner_and_rejects_late_permission(self):
        self.run_contract(r"""
const w=world(); await flush();
try {
  w.deferMedia=true; await w.pointer(true); await w.tick();
  w.state.epoch='epoch-new-activation'; await w.tick();
  assert.equal(w.getMedia,1);
  assert(w.requests.some(request=>request.route==='/voice/lease'
    && request.body?.release===true && request.body.epoch==='epoch-test'),
    'old ownership release must carry the old epoch');
  for(const resolve of w.mediaPending.splice(0)) resolve();
  await flush();
  assert.equal(w.tracks.length,1);
  assert(w.tracks.every(track=>track.stopped));
} finally { w.finish(); }
""")

    def test_lease_failure_after_permission_stops_local_media(self):
        self.run_contract(r"""
const w=world(); await flush();
try {
  w.leaseErrorAt=2; await w.pointer(true); await w.tick();
  assert.equal(w.getMedia,1); assert.equal(w.tracks.length,1);
  assert(w.tracks.every(track=>track.stopped),
    'media obtained before the post-permission lease failure must not leak');
  assert(w.actions.some(action=>action.type==='microphone'&&action.status==='error'));
} finally { w.finish(); }
""")

    def test_disconnect_while_permission_pending_cleans_up_late_media(self):
        self.run_contract(r"""
const w=world(); await flush();
w.deferMedia=true; await w.pointer(true); await w.tick();
w.rfb.dispatch('disconnect');
for(const resolve of w.mediaPending.splice(0)) resolve();
await flush();
assert.equal(w.getMedia,1);
assert(w.buttons[0].removed,'disconnect removes the permission control');
assert(w.tracks.every(track=>track.stopped));
assert.equal(w.timers.filter(timer=>timer.active).length,0);
""")

    def test_service_failure_while_permission_pending_cleans_up_late_media(self):
        self.run_contract(r"""
const w=world(); await flush();
try {
  w.deferMedia=true; await w.pointer(true); await w.tick();
  w.stateFailure=true; await w.tick();
  assert.equal(w.buttons[0].style.display,'block');
  assert.equal(w.buttons[0].textContent,'語音連線中斷，按一下重試');
  for(const resolve of w.mediaPending.splice(0)) resolve();
  await flush();
  assert.equal(w.getMedia,1); assert(w.tracks.every(track=>track.stopped));
} finally { w.finish(); }
""")

    def test_denied_permission_retries_only_on_explicit_new_epoch(self):
        self.run_contract(r"""
const w=world(); await flush();
try {
  w.mediaError=Object.assign(new Error('fixture microphone denied'),{name:'NotAllowedError'});
  await w.pointer(true); await w.tick(); assert.equal(w.getMedia,1);
  w.mediaError=null; await w.pointer(true); await w.tick();
  assert.equal(w.getMedia,1,'ordinary desktop clicks must not retry denied permission');
  w.state.epoch='epoch-explicit-retry'; await w.tick();
  assert.equal(w.getMedia,2);
  assert(w.actions.some(action=>action.type==='microphone'&&action.status==='denied'));
} finally { w.finish(); }
""")

    def test_untrusted_gesture_and_unrequested_microphone_never_capture(self):
        self.run_contract(r"""
let w=world(); await flush();
try { await w.pointer(false); await w.tick(); assert.equal(w.getMedia,0); }
finally { w.finish(); }
w=world({requested:false}); await flush();
try { await w.pointer(true); await w.tick(); assert.equal(w.getMedia,0); }
finally { w.finish(); }
""")

    def test_trusted_gesture_pcm_energy_and_exact_transcript_handoff(self):
        self.run_contract(r"""
const w=world({requested:false}); await flush();
try {
  await w.pointer(true); await w.tick(); assert.equal(w.getMedia,0);
  w.state.microphoneRequested=true; await w.tick(); await w.tick();
  assert.equal(w.getMedia,1,'a prior real desktop gesture arms later native activation once');
  await w.feed(.05,16); await w.feed(0,55);
  assert(w.actions.some(action=>action.type==='energy'&&action.energy>0));
  assert.equal(w.requests.filter(request=>request.route==='/voice/transcribe').length,1);
  assert.deepEqual(w.actions.filter(action=>action.type==='transcript').map(action=>action.text),['這是隔離測試']);
} finally { w.finish(); }
""")


if __name__ == "__main__":
    unittest.main()
