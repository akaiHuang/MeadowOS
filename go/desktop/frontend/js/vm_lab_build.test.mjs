import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
const source=readFileSync(new URL('./core.js',import.meta.url),'utf8').replace(/^import[^\n]+\n/gm,'').replace(/^export /gm,'');
function harness(lab,capability){
 const calls=[];
 const ctx={VM_LAB_BUILD:lab,console,setTimeout,clearTimeout,document:{documentElement:{dataset:{frontendMode:'production'}}},window:{location:{protocol:'wails:',hostname:'wails.localhost'}},fetch:async path=>{calls.push(path);return {ok:true,json:async()=>path==='/agent/client/capabilities'?capability:{sessionId:'real-session'}}}};
 vm.runInNewContext(source+'\nglobalThis.api={daemon,hasClientAccess};',ctx);
 return {...ctx.api,calls};
}
test('production frontend needs both compiled lab profile and native lab capability',async()=>{
 const labCap={protected:false,authenticationRequired:false,vmLab:true};
 for(const [compiled,cap]of [[false,labCap],[true,{...labCap,vmLab:false}],[true,{...labCap,protected:true}]]){
  const h=harness(compiled,cap);await assert.rejects(h.daemon.clientCapabilities(),{code:'CLIENT_CAPABILITIES_UNAVAILABLE'});assert.equal(h.hasClientAccess(),false);
 }
 const h=harness(true,labCap);await h.daemon.clientCapabilities();assert.equal(h.hasClientAccess(),true);await h.daemon.createSession('/workspace');assert.equal(h.calls.at(-1),'/session');
});
