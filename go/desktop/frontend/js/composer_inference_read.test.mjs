import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import { JSDOM } from 'jsdom';
const source=readFileSync(new URL('./composer.js',import.meta.url),'utf8');
function section(start,end){return source.slice(source.indexOf(start),source.indexOf(end,source.indexOf(start)));}
function deferred(){let resolve;return {promise:new Promise(r=>resolve=r),resolve:v=>resolve(v)};}
test('send-time inference read awaits the latest same-session read',async()=>{
 const first=deferred(),second=deferred();let reads=0;
 const context={state:{sessionId:'canonical'},inferenceControls:null,inferenceControlsSaving:false,inferenceControlsRevision:0,inferenceControlsLoad:null,
 daemon:{inferenceControls:()=>++reads===1?first.promise:second.promise},setTimeout,clearTimeout,paintInferenceControls(){}};
 vm.runInNewContext(section('async function loadInferenceControls()','async function updateInferenceControls(')+'\nglobalThis.read=loadInferenceControls;',context);
 const sending=context.read();
 const refresh=context.read();
 let resolved=false;sending.then(()=>resolved=true);
 first.resolve({sessionId:'canonical',maturity:'experimental',reasoningEffort:'medium'});
 await new Promise(setImmediate);
 const staleReturnedBeforeOwnedResponse=resolved && context.inferenceControls===null;
 second.resolve({sessionId:'canonical',maturity:'experimental',reasoningEffort:'medium'});
 await refresh;await sending;
 assert.equal(staleReturnedBeforeOwnedResponse,false,'send sees null controls and shows task quota unavailable while newer same-session GET is pending');
});
test('empty picker setup remains enabled before workspace/session',()=>{
 const dom=new JSDOM('<main></main>',{url:'https://fixture.invalid'});let emitted=0;
 const context={document:dom.window.document,window:dom.window,URLSearchParams,setTimeout,clearTimeout,
 state:{sessionId:null,cwd:''},modelModalRef:null,ResizeObserver:class{observe(){}},
 $:()=>null,positionModelModal(){},closeModelModal(){},syncCommercialModelMode(){},paintModelRows(){},reconcileImageCapability(){},
 bus:{emit(name){if(name==='settings:open-model')emitted++;}},};
 vm.runInNewContext(section('function buildModelModal()','function syncCommercialModelMode()')+'\nbuildModelModal();',context);
 const button=dom.window.document.querySelector('#modelSetupButton');
 assert.equal(button.disabled,false);button.click();assert.equal(emitted,1);
 dom.window.close();
});

test('fresh session read remains owned while stale session finishes',async()=>{
 const old=deferred(),fresh=deferred();let paints=0;
 const context={state:{sessionId:'old'},inferenceControls:null,inferenceControlsLoad:null,inferenceControlsSaving:false,inferenceControlsRevision:0,
 daemon:{inferenceControls:sid=>sid==='old'?old.promise:fresh.promise},setTimeout,clearTimeout,paintInferenceControls(){paints++;}};
 vm.runInNewContext(section('async function loadInferenceControls()','async function updateInferenceControls(')+'\nglobalThis.read=loadInferenceControls;',context);
 const prior=context.read();context.state.sessionId='fresh';const latest=context.read();
 old.resolve({sessionId:'old',reasoningEffort:'max'});await prior;
 assert.equal(context.inferenceControls,null);assert.equal(context.inferenceControlsLoad.sessionId,'fresh');
 fresh.resolve({sessionId:'fresh',reasoningEffort:'low'});await latest;
 assert.equal(context.inferenceControls.sessionId,'fresh');assert.equal(context.inferenceControls.reasoningEffort,'low');assert.equal(context.inferenceControlsLoad,null);
});
