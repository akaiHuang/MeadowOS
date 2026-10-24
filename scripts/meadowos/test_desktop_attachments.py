"""Isolated host-file ingress contracts; this does not exercise a live VM or GTK."""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent

PRELUDE = r"""
import assert from 'node:assert/strict';
import { File } from 'node:buffer';
import { setImmediate as nextTurn, setTimeout as realDelay } from 'node:timers/promises';
import { installDesktopAttachments } from './desktop-attachments.js';

let timerId=0;
const timers=new Map();
globalThis.setTimeout=(callback,delay=0)=>{const id=++timerId;timers.set(id,{callback,delay});return id;};
globalThis.clearTimeout=id=>timers.delete(id);
globalThis.setInterval=(callback,delay=0)=>{const id=++timerId;timers.set(id,{callback,delay,interval:true});return id;};
globalThis.clearInterval=id=>timers.delete(id);
class Element extends EventTarget {
  constructor(tag='div'){super();this.tagName=tag.toUpperCase();this.style={};this.children=[];this.dataset={};this.attributes={};this.textContent='';}
  appendChild(child){child.parentElement=this;this.children.push(child);return child;}
  append(...children){for(const child of children)this.appendChild(child);}
  remove(){if(this.parentElement)this.parentElement.children=this.parentElement.children.filter(child=>child!==this);}
  setAttribute(name,value){this.attributes[name]=value;}
  querySelector(selector){return selector.includes('canvas')?canvas:null;}
  getBoundingClientRect(){return {left:10,top:20,width:960,height:540,right:970,bottom:560};}
}
const canvas=new Element('canvas');canvas.width=1920;canvas.height=1080;
const container=new Element();container.appendChild(canvas);
const body=new Element('body');
const documentTarget=new EventTarget();
Object.assign(documentTarget,{hidden:false,body,activeElement:null,createElement:tag=>new Element(tag),getElementById:id=>id==='noVNC_container'?container:null});
globalThis.document=documentTarget;globalThis.window=new EventTarget();
const rfb=new EventTarget();
const response=(value,ok=true,status=200)=>({ok,status,json:async()=>value});
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return{promise,resolve};};
const requests=[];let handler;
globalThis.fetch=(url,options={})=>{
  const parsed=new URL(url),headers=options.headers||{};
  const contentType=headers['Content-Type']||headers['content-type']||'';
  const body=contentType.startsWith('application/json')&&typeof options.body==='string'?JSON.parse(options.body):options.body;
  const request={url,path:parsed.pathname,query:parsed.searchParams,headers,body,method:options.method||'GET',options};
  requests.push(request);return handler(request);
};
const state=(epoch='first',active=true)=>({version:1,epoch,active,rect:{x:40,y:60,width:1000,height:800},maxFileBytes:20*1024*1024,updatedAt:Date.now()/1000});
const flush=async(turns=20)=>{for(let i=0;i<turns;i++){for(let j=0;j<6;j++)await Promise.resolve();await nextTurn();}};
const waitFor=async(predicate)=>{for(let i=0;i<100&&!predicate();i++)await realDelay(5);await flush();assert.equal(Boolean(predicate()),true,'asynchronous browser operation did not settle');};
const runTimer=async()=>{const entry=timers.entries().next().value;if(!entry)return;const[id,timer]=entry;if(!timer.interval)timers.delete(id);timer.callback();await flush();};
const dropEvent=(files,x=80,y=100,type='drop')=>{
  const event=new Event(type,{cancelable:true,bubbles:true});
  Object.defineProperties(event,{clientX:{value:x},clientY:{value:y},dataTransfer:{value:{types:['Files'],files,items:files.map(file=>({kind:'file',type:file.type,getAsFile:()=>file})),dropEffect:'none'}}});
  return event;
};
const textOf=element=>[element.textContent,...element.children.map(textOf)].join('\n');
const allText=()=>textOf(body);
const overlay=()=>body.children.find(child=>child.attributes.role==='status');
const uploadRequests=()=>requests.filter(request=>request.path==='/attachments/upload');
const dispose=()=>rfb._meadowAttachmentsDispose?.();
"""


@unittest.skipUnless(shutil.which('node'), 'Node required')
class DesktopAttachmentContract(unittest.TestCase):
    def run_probe(self, script):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / 'package.json').write_text('{"type":"module"}')
            shutil.copyfile(ROOT / 'assets/desktop-attachments.js', root / 'desktop-attachments.js')
            result = subprocess.run(
                [shutil.which('node'), '--input-type=module', '-e', PRELUDE + script],
                cwd=root, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_original_bytes_native_geometry_and_final_receipt(self):
        self.run_probe(r"""
handler=async request=>response(request.path==='/attachments/state'?state():request.path==='/attachments/lease'?{owned:true}:request.path==='/attachments/receipt'?{status:'staged'}:{accepted:true,status:'accepted'});
installDesktopAttachments(rfb,container);await flush();
const bytes=new Uint8Array([0,1,2,3,255,10]);
const file=new File([bytes],'原始 圖片.png',{type:'image/png'});
Object.defineProperty(file,'webkitRelativePath',{value:'/Users/private/Desktop/secret.png'});
const drop=dropEvent([file]);container.dispatchEvent(drop);await flush();
assert.equal(drop.defaultPrevented,true);
assert.equal(uploadRequests().length,1);
const upload=uploadRequests()[0];
assert.equal(upload.method,'POST');assert.equal(upload.headers['Content-Type'],'application/octet-stream');
assert.equal(upload.headers['X-Meadow-Attachments'],'1');
assert.deepEqual(new Uint8Array(upload.body),bytes,'original File bytes must remain unchanged');
const descriptor=JSON.parse(decodeURIComponent(upload.headers['X-Meadow-Attachments-Meta']));
assert.equal(descriptor.name,'原始 圖片.png');assert.equal(descriptor.mime,'image/png');assert.equal(descriptor.size,bytes.length);
assert.equal(descriptor.sha256,'0ace3c180294ea88af3dcb1e1f75d125296ec98cb07f0501ce22dc87cf9a5ccf');
assert.match(descriptor.eventId,/^[a-f0-9]{32}$/);assert.equal(descriptor.epoch,'first');
assert.equal(JSON.stringify(descriptor).includes('/Users/'),false,'host filesystem paths are not ingress metadata');
const lease=requests.find(request=>request.path==='/attachments/lease');
assert.deepEqual(lease.body.point,{x:140,y:160},'CSS drop point must map into native framebuffer pixels');
assert.equal(lease.body.clientId,upload.headers['X-Meadow-Attachments-Client']);
assert.equal(requests.some(request=>request.path==='/attachments/receipt'&&request.query.get('eventId')===descriptor.eventId),true);
assert.equal(overlay().style.display,'none','staged completion does not leave a success toast');
dispose();assert.equal(body.children.length,0);assert.equal(timers.size,0);
""")

    def test_only_foreground_native_chat_admits_files_and_prevents_file_navigation(self):
        self.run_probe(r"""
let active=true;
handler=async request=>response(request.path==='/attachments/state'?state('first',active):{owned:true});
installDesktopAttachments(rfb,container);await flush();
const file=new File(['image'],'test.png',{type:'image/png'});
const outside=dropEvent([file],0,0);container.dispatchEvent(outside);await flush();
assert.equal(outside.defaultPrevented,true);assert.equal(uploadRequests().length,0);
assert.equal(requests.some(request=>request.path==='/attachments/lease'),false);
active=false;
const inactive=dropEvent([file]);container.dispatchEvent(inactive);await flush();
assert.equal(inactive.defaultPrevented,true);assert.equal(uploadRequests().length,0);
assert.equal(requests.some(request=>request.path==='/attachments/lease'),false);
const text=new Event('drop',{cancelable:true});Object.defineProperty(text,'dataTransfer',{value:{types:['text/plain'],files:[]}});
container.dispatchEvent(text);await flush();assert.equal(text.defaultPrevented,false,'ordinary text/link interactions remain native');
dispose();
""")

    def test_forty_one_file_collection_has_no_item_cap_and_reads_only_after_staging(self):
        self.run_probe(r"""
const firstReceipt=deferred();let receiptCount=0,readCount=0;
handler=async request=>{
 if(request.path==='/attachments/state')return response(state());
 if(request.path==='/attachments/lease')return response({owned:true});
 if(request.path==='/attachments/receipt'&&++receiptCount===1)return firstReceipt.promise;
 return response(request.path==='/attachments/receipt'?{status:'staged'}:{accepted:true,status:'accepted'});
};
const files=Array.from({length:41},(_,i)=>({name:`image-${i}.png`,type:'image/png',size:3,arrayBuffer:async()=>{readCount++;return new Uint8Array([i,2,3]).buffer;}}));
installDesktopAttachments(rfb,container);await flush();container.dispatchEvent(dropEvent(files));await flush();
assert.equal(readCount,1,'the next file is not eagerly read while native receipt is unresolved');
assert.equal(uploadRequests().length,1);
firstReceipt.resolve(response({status:'staged'}));await flush(250);
assert.equal(readCount,41);assert.equal(uploadRequests().length,41);assert.equal(receiptCount,41);
const collections=new Set(uploadRequests().map(request=>JSON.parse(decodeURIComponent(request.headers['X-Meadow-Attachments-Meta'])).collectionId));
assert.equal(collections.size,1,'one host drop retains one opaque collection');
assert.equal(overlay().style.display,'none');dispose();
""")

    def test_per_file_bounds_and_types_reject_before_reading_and_allow_twenty_mib(self):
        self.run_probe(r"""
handler=async request=>response(request.path==='/attachments/state'?state():request.path==='/attachments/lease'?{owned:true}:request.path==='/attachments/receipt'?{status:'staged'}:{accepted:true});
let invalidReads=0,validReads=0;
const invalid=(name,type,size)=>({name,type,size,arrayBuffer:async()=>{invalidReads++;throw new Error('must not read rejected file');}});
const maximum=20*1024*1024;
const valid={name:'full.png',type:'image/png',size:maximum,arrayBuffer:async()=>{validReads++;return new ArrayBuffer(maximum);}};
installDesktopAttachments(rfb,container);await flush();
container.dispatchEvent(dropEvent([invalid('zero.png','image/png',0),invalid('large.png','image/png',maximum+1),invalid('other.txt','text/plain',5),valid]));
await waitFor(()=>uploadRequests().length===1);
assert.equal(invalidReads,0);assert.equal(validReads,1);assert.equal(uploadRequests().length,1);
assert.equal(uploadRequests()[0].body.byteLength,maximum);
dispose();
""")

    def test_native_failure_receipt_remains_visible_and_is_not_reported_as_staged(self):
        self.run_probe(r"""
handler=async request=>response(request.path==='/attachments/state'?state():request.path==='/attachments/lease'?{owned:true}:request.path==='/attachments/receipt'?{status:'failed',error:'native_staging_failed',message:'Private cache staging failed'}:{accepted:true});
installDesktopAttachments(rfb,container);await flush();
container.dispatchEvent(dropEvent([new File(['image'],'broken.png',{type:'image/png'})]));await flush();
assert.equal(uploadRequests().length,1);assert.equal(overlay().style.display,'block');
assert.match(allText(),/broken\.png: Private cache staging failed/);
assert.equal(allText().includes('1 / 1'),false,'mailbox acceptance is not counted as native staging');
dispose();
""")

    def test_mailbox_accepted_receipt_keeps_next_file_unread_until_native_stage(self):
        self.run_probe(r"""
let readCount=0,receiptCount=0;
handler=async request=>{
 if(request.path==='/attachments/state')return response(state());
 if(request.path==='/attachments/lease')return response({owned:true});
 if(request.path==='/attachments/receipt')return response({status:++receiptCount===1?'accepted':'staged'});
 return response({accepted:true,status:'accepted'});
};
const files=Array.from({length:2},(_,i)=>({name:`image-${i}.png`,type:'image/png',size:3,arrayBuffer:async()=>{readCount++;return new Uint8Array([i,2,3]).buffer;}}));
installDesktopAttachments(rfb,container);await flush();container.dispatchEvent(dropEvent(files));await flush();
assert.equal(receiptCount,1);assert.equal(readCount,1);assert.equal(uploadRequests().length,1);
assert.equal(overlay().style.display,'block');assert.match(allText(),/0 \/ 2/);
const waiting=[...timers.entries()].find(([,timer])=>timer.delay===150);
assert.ok(waiting,'accepted mailbox requires a later native receipt poll');
timers.delete(waiting[0]);waiting[1].callback();await flush();
assert.equal(receiptCount,3);assert.equal(readCount,2);assert.equal(uploadRequests().length,2);
assert.equal(overlay().style.display,'none');dispose();
""")

    def test_disconnect_aborts_inflight_request_and_does_not_read_remaining_files(self):
        self.run_probe(r"""
let readCount=0,uploadSignal;
handler=async request=>{
 if(request.path==='/attachments/state')return response(state());
 if(request.path==='/attachments/lease')return response({owned:true});
 if(request.path==='/attachments/upload'){
  uploadSignal=request.options.signal;
  return new Promise((resolve,reject)=>uploadSignal.addEventListener('abort',()=>reject(new Error('aborted')),{once:true}));
 }
 return response({status:'staged'});
};
const files=Array.from({length:3},(_,i)=>({name:`image-${i}.png`,type:'image/png',size:3,arrayBuffer:async()=>{readCount++;return new Uint8Array([i,2,3]).buffer;}}));
installDesktopAttachments(rfb,container);await flush();container.dispatchEvent(dropEvent(files));await flush();
assert.equal(readCount,1);assert.equal(uploadRequests().length,1);
rfb.dispatchEvent(new Event('disconnect'));await flush();
assert.equal(uploadSignal.aborted,true);assert.equal(readCount,1);assert.equal(uploadRequests().length,1);
assert.equal(body.children.length,0);assert.equal(timers.size,0);
container.dispatchEvent(dropEvent(files));await flush();assert.equal(readCount,1,'disposed drop listener must be removed');
""")

    def test_native_session_epoch_change_stops_remaining_collection(self):
        self.run_probe(r"""
let readCount=0,uploaded=0;
handler=async request=>{
 if(request.path==='/attachments/state')return response(state(uploaded?'second':'first'));
 if(request.path==='/attachments/lease')return response({owned:true});
 if(request.path==='/attachments/upload'){uploaded++;return response({accepted:true});}
 return response({status:'staged'});
};
const files=Array.from({length:3},(_,i)=>({name:`image-${i}.png`,type:'image/png',size:3,arrayBuffer:async()=>{readCount++;return new Uint8Array([i,2,3]).buffer;}}));
installDesktopAttachments(rfb,container);await flush();container.dispatchEvent(dropEvent(files));await flush();
assert.equal(uploadRequests().length,1);assert.equal(readCount,1);
assert.match(allText(),/原生對話已切換/);dispose();
""")


if __name__ == '__main__':
    unittest.main()
