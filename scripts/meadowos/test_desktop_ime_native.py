"""Browser/native preedit transport contract; no real OS IME or GTK is implied."""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent

PRELUDE = r"""
import assert from 'node:assert/strict';
import { createNativePreedit } from './desktop-ime-native.js';
const flush=async()=>{for(let i=0;i<12;i++)await Promise.resolve();};
const response=(value,ok=true)=>({ok,json:async()=>value});
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
let clock=1000;Date.now=()=>clock;
const callbacks=new Set();
globalThis.setInterval=callback=>{callbacks.add(callback);return{unref(){},callback};};
globalThis.clearInterval=timer=>callbacks.delete(timer.callback);
class Input extends EventTarget {value='';selectionStart=0;}
const input=new Input(),canvas={width:1920,height:1080,getBoundingClientRect:()=>({left:10,top:20,width:960,height:540})};
const container={querySelector:name=>name==='canvas'?canvas:null};
globalThis.document={activeElement:input,hidden:false};globalThis.window=new EventTarget();
const requests=[];let handler;
globalThis.fetch=(url,options)=>{
  const path=new URL(url).pathname,body=options.body?JSON.parse(options.body):null;
  requests.push({path,body,headers:options.headers,method:options.method});
  return handler(path,body,options);
};
const focus=async()=>{input.dispatchEvent(new Event('focus'));await flush();};
const state=(epoch='first',active=true)=>({epoch,active,caret:{x:200,y:400,width:2,height:30}});
let changedCount=0;const bridge=createNativePreedit(input,container,()=>changedCount++);
"""


@unittest.skipUnless(shutil.which('node'), 'Node required')
class NativePreeditContract(unittest.TestCase):
    def run_probe(self, script):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / 'package.json').write_text('{"type":"module"}')
            shutil.copyfile(ROOT / 'assets/desktop-ime-native.js', root / 'desktop-ime-native.js')
            result = subprocess.run(
                [shutil.which('node'), '--input-type=module', '-e', PRELUDE + script],
                cwd=root, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_owner_lease_sequence_unicode_geometry_and_clear_without_commit(self):
        self.run_probe(r"""
handler=async path=>response(path==='/ime/state'?state():path==='/ime/lease'?{owned:true}:{accepted:true});
await focus();assert.deepEqual(bridge.geometry(),{x:110,y:220,height:15});
input.value='ㄋ😀ㄧˇ';input.selectionStart=4;bridge.begin();await flush();
assert.equal(bridge.presenting,true);
const lease=requests.find(r=>r.path==='/ime/lease');
const first=requests.find(r=>r.path==='/ime/preedit');
assert.equal(lease.body.clientId,lease.headers['X-Meadow-IME-Client']);
assert.equal(lease.headers['X-Meadow-IME'],'1');
assert.deepEqual(first.body,{epoch:'first',sequence:1,text:'ㄋ😀ㄧˇ',cursor:3,active:true});
input.value='😀'.repeat(300);input.selectionStart=input.value.length;bridge.update();await flush();
const second=requests.filter(r=>r.path==='/ime/preedit').at(-1);
assert.equal(Array.from(second.body.text).length,256);assert.equal(second.body.cursor,256);
assert.equal(second.body.sequence,2);
// Committed Chinese is transmitted by the main IME module over RFB only.
input.value='你好';bridge.end();await flush();
assert.deepEqual(requests.filter(r=>r.path==='/ime/preedit').at(-1).body,
 {epoch:'first',sequence:3,text:'',cursor:0,active:false});
assert.equal(bridge.presenting,false);
assert.equal(requests.some(r=>r.body?.text==='你好'),false,'bridge must never send final commit text');
assert.equal(requests.filter(r=>r.path==='/ime/lease').length,1);
bridge.dispose();assert.equal(callbacks.size,0);
""")

    def test_denied_lease_and_transport_failure_do_not_claim_presentation(self):
        self.run_probe(r"""
let allow=false,fail=false;
handler=async path=>{
 if(fail&&path==='/ime/preedit')throw new Error('offline');
 return response(path==='/ime/state'?state():path==='/ime/lease'?{owned:allow}:{accepted:true});
};
await focus();input.value='ㄋ';bridge.begin();await flush();
assert.equal(bridge.presenting,false);
assert.equal(requests.some(r=>r.path==='/ime/preedit'),false,'foreign lease forbids preedit');
allow=true;bridge.update();await flush();assert.equal(bridge.presenting,true);
fail=true;input.value='ㄋㄧ';bridge.update();await flush();
assert.equal(Boolean(bridge.presenting),false);assert.equal(bridge.geometry(),null);
bridge.dispose();
""")

    def test_stale_lease_response_cannot_publish_to_previous_focus_epoch(self):
        self.run_probe(r"""
let epoch='first';const firstLease=deferred();let leaseCount=0;
handler=async path=>{
 if(path==='/ime/state')return response(state(epoch));
 if(path==='/ime/lease'&&++leaseCount===1)return firstLease.promise;
 return response(path==='/ime/lease'?{owned:true}:{accepted:true});
};
await focus();input.value='ㄋ';input.selectionStart=1;bridge.begin();await flush();
assert.equal(requests.filter(r=>r.path==='/ime/preedit').length,0);
epoch='second';await focus();firstLease.resolve(response({owned:true}));await flush();
const writes=requests.filter(r=>r.path==='/ime/preedit');
assert.equal(writes.length,1);assert.equal(writes[0].body.epoch,'second');
assert.equal(leaseCount,2,'new epoch requires a fresh ownership lease');
bridge.end();await flush();bridge.dispose();
""")

    def test_pending_updates_coalesce_and_composition_end_clears_after_inflight(self):
        self.run_probe(r"""
const blocked=deferred();let writes=0;
handler=async path=>{
 if(path==='/ime/state')return response(state());
 if(path==='/ime/lease')return response({owned:true});
 if(++writes===1)return blocked.promise;
 return response({accepted:true});
};
await focus();input.value='ㄋ';bridge.begin();await flush();
input.value='ㄋㄧ';bridge.update();input.value='ㄋㄧˇ';bridge.update();
assert.equal(requests.filter(r=>r.path==='/ime/preedit').length,1,'serialize one in-flight write');
input.value='你';bridge.end();blocked.resolve(response({accepted:true}));await flush();
const bodies=requests.filter(r=>r.path==='/ime/preedit').map(r=>r.body);
assert.deepEqual(bodies,[{epoch:'first',sequence:1,text:'ㄋ',cursor:0,active:true},
 {epoch:'first',sequence:2,text:'',cursor:0,active:false}]);
assert.equal(bridge.presenting,false);bridge.dispose();
""")

    def test_dispose_during_preedit_write_still_clears_native_presentation(self):
        self.run_probe(r"""
const blocked=deferred();let writes=0;
handler=async path=>{
 if(path==='/ime/state')return response(state());
 if(path==='/ime/lease')return response({owned:true});
 if(++writes===1)return blocked.promise;
 return response({accepted:true});
};
await focus();input.value='ㄋ';bridge.begin();await flush();
bridge.dispose();blocked.resolve(response({accepted:true}));await flush();
const bodies=requests.filter(r=>r.path==='/ime/preedit').map(r=>r.body);
assert.equal(bodies.length,2,'disconnect must clear even if an active preedit write was in flight');
assert.deepEqual(bodies[1],{epoch:'first',sequence:2,text:'',cursor:0,active:false});
assert.equal(callbacks.size,0);
""")

    def test_lease_renewal_and_focus_visibility_gates(self):
        self.run_probe(r"""
handler=async path=>response(path==='/ime/state'?state():path==='/ime/lease'?{owned:true}:{accepted:true});
document.activeElement=null;await focus();assert.equal(requests.length,0);
document.activeElement=input;document.hidden=true;await focus();assert.equal(requests.length,0);
document.hidden=false;await focus();input.value='ㄋ';bridge.begin();await flush();
clock+=2500;for(const callback of callbacks)callback();await flush();
assert.equal(requests.filter(r=>r.path==='/ime/lease').length,2,'active preedit renews lease');
bridge.end();await flush();bridge.dispose();
""")


if __name__ == '__main__':
    unittest.main()
