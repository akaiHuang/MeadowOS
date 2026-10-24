"""Browser composition ordering contract; native GTK ingress is a separate gate."""
from pathlib import Path
import os
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent


@unittest.skipUnless(shutil.which('node'), 'Node required')
class DesktopIMEContract(unittest.TestCase):
    def test_preedit_commit_cancellation_unicode_and_disconnect(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for path in ('app', 'core/input', 'core/util'):
                (root / path).mkdir(parents=True)
            (root / 'package.json').write_text('{"type":"module"}')
            shutil.copyfile(ROOT / 'assets/desktop-ime.js', root / 'app/meadow-ime.js')
            shutil.copyfile(ROOT / 'assets/desktop-ime-native.js', root / 'app/desktop-ime-native.js')
            (root / 'core/input/keyboard.js').write_text('export default class Keyboard { grab(){} ungrab(){} }')
            (root / 'core/input/keysymdef.js').write_text('export default {lookup:p=>p<256?p:(0x01000000|p)}')
            (root / 'core/util/browser.js').write_text('export const isTouchDevice=false;export const isMac=()=>true;')
            (root / 'core/input/util.js').write_text('export const getKeysym=e=>e.key?.codePointAt(0);')
            result = subprocess.run([shutil.which('node'), '--input-type=module', '-e', r'''
import assert from 'node:assert/strict';
import { installDesktopIME } from './app/meadow-ime.js';
class Element extends EventTarget {
  constructor(){super();this.value='';this.style={cssText:''};}
  setAttribute(){} focus(){} remove(){this.removed=true;}
}
let input;
globalThis.document={createElement:()=>input=new Element(),body:{appendChild(){}}};
globalThis.innerWidth=1280;globalThis.innerHeight=800;globalThis.window=new EventTarget();
const container=new Element(),rfb=new EventTarget(),keys=[];
rfb.sendKey=k=>keys.push(k);
installDesktopIME(rfb,container);
function event(type, fields={}) {
  const event=new Event(type);
  for(const [k,v] of Object.entries(fields))Object.defineProperty(event,k,{value:v});
  input.dispatchEvent(event);
}
const settle=()=>new Promise(resolve=>setTimeout(resolve,5));
event('compositionstart');input.value='zhong';event('input',{isComposing:true});
assert.deepEqual(keys,[],'preedit must stay on host');
input.value='中文😀𠮷';event('input',{isComposing:false});
assert.deepEqual(keys,[],'final input before end stays pending');
event('compositionend',{data:input.value});
event('input',{inputType:'insertFromComposition'});await settle();
assert.deepEqual(keys,[0x01004e2d,0x01006587,0x0101f600,0x01020bb7]);
input.value='中文😀𠮷';event('input',{inputType:'insertFromComposition'});
assert.equal(keys.length,4,'late composition input cannot duplicate commit');
event('compositionstart');input.value='取消';event('compositionend',{data:''});await settle();
assert.equal(keys.length,4,'cancelled composition must not transmit preedit');
event('compositionstart');input.value='中';event('compositionend',{data:'中'});await settle();
event('compositionstart');input.value='中';event('compositionend',{data:'中'});await settle();
assert.deepEqual(keys.slice(4),[0x01004e2d,0x01004e2d],'same character can be committed twice');
input.value='hello\u{1f600}';event('input',{inputType:'insertText'});
assert.deepEqual(keys.slice(-6),[104,101,108,108,111,0x0101f600]);
const raceStart=keys.length;
event('compositionstart');input.value='中';event('compositionend',{data:'中'});
input.value='中文';event('input',{inputType:'insertText',data:'文'});await settle();
assert.deepEqual(keys.slice(raceStart),[0x01004e2d,0x01006587],
  'ordinary tail before commit timer must not disappear or duplicate prefix');
const cancelStart=keys.length;
event('compositionstart');input.value='取消';event('compositionend',{data:''});
input.value='文';event('input',{inputType:'insertText',data:'文'});await settle();
assert.deepEqual(keys.slice(cancelStart),[0x01006587],
  'ordinary input after cancelled composition must not disappear');
const before=keys.length;
event('compositionstart');input.value='不傳';event('compositionend',{data:'不傳'});
rfb.dispatchEvent(new Event('disconnect'));await settle();
assert.equal(keys.length,before,'disconnect cancels pending commit');
assert.equal(input.removed,true);
console.log('preedit, ordering, cancellation, scalar Unicode, repeat and disconnect passed');
'''], cwd=root, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


    def _run_real_vendor_script(self, script, platform="mac"):
        vendor = Path(os.environ.get('NOVNC_VENDOR_DIR', '/usr/share/novnc'))
        if not (vendor / 'core/input/keyboard.js').is_file():
            self.skipTest('Installed noVNC vendor keyboard required; run in native lab')
        from prepare_novnc import patch_keyboard
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / 'app').mkdir()
            (root / 'core/util').mkdir(parents=True)
            (root / 'package.json').write_text('{"type":"module"}')
            shutil.copytree(vendor / 'core/input', root / 'core/input')
            original = (vendor / 'core/input/keyboard.js').read_text()
            (root / 'core/input/keyboard.js').write_text(patch_keyboard(original))
            shutil.copyfile(ROOT / 'assets/desktop-ime.js', root / 'app/meadow-ime.js')
            shutil.copyfile(ROOT / 'assets/desktop-ime-native.js', root / 'app/desktop-ime-native.js')
            # Only environment adapters are stubbed. The vendor keyboard,
            # keysyms, physical codes and event/preventDefault logic are real.
            (root / 'core/util/browser.js').write_text(
                'export const isTouchDevice=false; export const isIOS=()=>false;'
                + 'export const isMac=()=>'+str(platform == 'mac').lower()+';'
                + 'export const isWindows=()=>'+str(platform == 'windows').lower()+';')
            (root / 'core/util/events.js').write_text('export function stopEvent(e){e.stopPropagation();e.preventDefault();}')
            (root / 'core/util/logging.js').write_text('export const Debug=()=>{};')
            result = subprocess.run(
                [shutil.which('node'), '--input-type=module', '-e', script],
                cwd=root, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_shortcuts_with_real_vendor_keyboard(self):
        self._run_real_vendor_script(r"""
import assert from 'node:assert/strict';
import { installDesktopIME } from './app/meadow-ime.js';
class Element extends EventTarget {
  constructor(){super();this.value='';this.style={cssText:''};}
  setAttribute(){} focus(){} remove(){this.removed=true;}
}
let input;
globalThis.document={createElement:()=>input=new Element(),body:{appendChild(){}}};
globalThis.window=new EventTarget();globalThis.innerWidth=1280;globalThis.innerHeight=800;
const container=new Element(),rfb=new EventTarget(),keys=[];
rfb.sendKey=(...args)=>keys.push(args.slice(0,3));
installDesktopIME(rfb,container);
function event(type, fields={}) {
  const event=new Event(type,{cancelable:true});
  for(const [k,v]of Object.entries(fields))Object.defineProperty(event,k,{value:v});
  input.dispatchEvent(event);return event;
}
function key(type,key,code,ctrlKey){return event(type,{key,code,ctrlKey,shiftKey:false,altKey:false,metaKey:false,getModifierState:()=>false});}
const a=key('keydown','a','KeyA',true);
key('keyup','a','KeyA',true);key('keyup','Control','ControlLeft',false);
assert.equal(a.defaultPrevented,true,'vendor must suppress the host textarea default');
assert.deepEqual(keys,[[0xffe3,'ControlLeft',true],[97,'KeyA',true],[97,'KeyA',false],[0xffe3,'ControlLeft',false]],'flag-only Ctrl+A must remain a shortcut');
keys.length=0;
key('keydown','Control','ControlLeft',true);key('keydown','a','KeyA',true);
key('keyup','a','KeyA',true);key('keyup','Control','ControlLeft',false);
assert.deepEqual(keys,[[0xffe3,'ControlLeft',true],[97,'KeyA',true],[97,'KeyA',false],[0xffe3,'ControlLeft',false]],'normal modifier event stream must not duplicate Control');
keys.length=0;
event('compositionstart');input.value='中';event('compositionend',{data:'中'});
key('keydown','a','KeyA',true);
assert.deepEqual(keys.slice(0,3),[[0x01004e2d],[0xffe3,'ControlLeft',true],[97,'KeyA',true]],'pending text must precede new shortcut modifier');
key('keyup','a','KeyA',true);event('blur');
assert.deepEqual(keys.at(-1),[0xffe3,'ControlLeft',false],'textarea blur releases synthetic Control');
keys.length=0;
key('keydown','a','KeyA',true);key('keyup','a','KeyA',true);
window.dispatchEvent(new Event('blur'));
assert.deepEqual(keys.at(-1),[0xffe3,'ControlLeft',false],'window blur releases synthetic Control');
keys.length=0;
key('keydown','a','KeyA',true);key('keyup','a','KeyA',true);
event('compositionstart');
assert.deepEqual(keys.at(-1),[0xffe3,'ControlLeft',false],'host composition releases synthetic Control');
event('compositionend',{data:''});await new Promise(resolve=>setTimeout(resolve,5));
keys.length=0;
key('keydown','a','KeyA',true);key('keyup','a','KeyA',true);
rfb.dispatchEvent(new Event('disconnect'));
assert.deepEqual(keys.at(-1),[0xffe3,'ControlLeft',false],'disconnect releases synthetic Control');
console.log('real vendor keyboard: Ctrl chord, normal modifiers, pending ordering, blur/composition/disconnect passed');
""")


    def test_first_key_and_text_ownership_with_real_vendor_keyboard(self):
        # Synthesized event-order regression, not physical macOS IME coverage.
        # In particular, start BEFORE compositionstart: cancelling the first
        # printable key's default action can stop host composition altogether.
        self._run_real_vendor_script(r"""
import assert from 'node:assert/strict';
import { installDesktopIME } from './app/meadow-ime.js';
class Element extends EventTarget {
  constructor(){super();this.value='';this.style={cssText:''};}
  setAttribute(){} focus(){} remove(){this.removed=true;}
}
let input;
globalThis.document={createElement:()=>input=new Element(),body:{appendChild(){}}};
globalThis.window=new EventTarget();globalThis.innerWidth=1280;globalThis.innerHeight=800;
const container=new Element(),rfb=new EventTarget(),keys=[];
rfb.sendKey=(...args)=>keys.push(args.slice(0,3));
installDesktopIME(rfb,container);
function event(type, fields={}) {
  const e=new Event(type,{cancelable:true});
  for(const [k,v]of Object.entries(fields))Object.defineProperty(e,k,{value:v});
  input.dispatchEvent(e);return e;
}
function key(type,key,code,more={}) {
  return event(type,{key,code,keyCode:0,ctrlKey:false,shiftKey:false,
    altKey:false,metaKey:false,getModifierState:()=>false,...more});
}
const settle=()=>new Promise(resolve=>setTimeout(resolve,5));
for(const [character,code,keyCode]of [['ㄋ','KeyS',83],['s','KeyS',83]]) {
  const first=key('keydown',character,code,{keyCode});
  assert.equal(first.defaultPrevented,false,'first printable key must permit host IME onset');
  assert.deepEqual(keys,[],'initial phonetic/physical key must not leak to RFB');
  key('keyup',character,code,{keyCode});
}
const latin=key('keydown','a','KeyA',{keyCode:65});
assert.equal(latin.defaultPrevented,false,'browser owns ordinary text editing');
input.value='a';event('input',{data:'a',inputType:'insertText'});
key('keyup','a','KeyA',{keyCode:65});
assert.deepEqual(keys,[[97]],'ordinary text commits through input once, not down/up plus input');
assert.equal(input.value,'');keys.length=0;
const dead=key('keydown','Dead','KeyE',{keyCode:69,altKey:true});
assert.equal(dead.defaultPrevented,false,'Option dead key must allow native accent composition');
assert.deepEqual(keys,[],'dead-key precursor must not be sent to remote');
event('compositionstart');input.value='´';
event('input',{data:'´',inputType:'insertCompositionText',isComposing:true});
key('keyup','Dead','KeyE',{keyCode:69,altKey:true,isComposing:true});
input.value='é';event('compositionend',{data:'é'});await settle();
assert.deepEqual(keys,[[233]],'accent composition commits exactly once');keys.length=0;
key('keydown','a','KeyA',{keyCode:65});
input.value='a';event('input',{data:'a',inputType:'insertText'});
key('keydown','Control','ControlLeft',{ctrlKey:true});
const chord=key('keydown','a','KeyA',{keyCode:65,ctrlKey:true,repeat:true});
assert.equal(chord.defaultPrevented,true,'a held host text key can become a remote Ctrl chord');
key('keyup','a','KeyA',{keyCode:65,ctrlKey:true});
key('keyup','Control','ControlLeft',{ctrlKey:false});
assert.deepEqual(keys,[[97],[0xffe3,'ControlLeft',true],[97,'KeyA',true],
  [97,'KeyA',false],[0xffe3,'ControlLeft',false]],'ownership handoff must release remote keys');
keys.length=0;
key('keydown','Control','ControlLeft',{ctrlKey:true});
key('keydown','a','KeyA',{keyCode:65,ctrlKey:true});
key('keyup','Control','ControlLeft',{ctrlKey:false});
key('keyup','a','KeyA',{keyCode:65,ctrlKey:false});
assert.deepEqual(keys,[[0xffe3,'ControlLeft',true],[97,'KeyA',true],
  [0xffe3,'ControlLeft',false],[97,'KeyA',false]],
  'releasing Ctrl first must not strand the remote letter');keys.length=0;
event('compositionstart');input.value='ㄋㄧˇ';
const arrow=key('keydown','ArrowRight','ArrowRight',{keyCode:39,isComposing:true});
assert.equal(arrow.defaultPrevented,false,'candidate navigation belongs to host IME');
input.value='你';event('compositionend',{data:'你'});
const arrowUp=key('keyup','ArrowRight','ArrowRight',{keyCode:39});
assert.equal(arrowUp.defaultPrevented,false,'late candidate keyup retains host ownership');
event('input',{data:'你',inputType:'insertFromComposition'});await settle();
assert.deepEqual(keys,[[0x01004f60]],'candidate navigation cannot move remote cursor');keys.length=0;
const phonetic=key('keydown','ㄋ','KeyS',{keyCode:83});
assert.equal(phonetic.defaultPrevented,false);
event('compositionstart');input.value='ㄋㄧˇ';
event('input',{data:input.value,inputType:'insertCompositionText',isComposing:true});
key('keyup','ㄋ','KeyS',{keyCode:83,isComposing:true});
const enter=key('keydown','Enter','Enter',{keyCode:13,isComposing:true});
assert.equal(enter.defaultPrevented,false,'Enter confirms host candidate');
input.value='你';event('compositionend',{data:'你'});
key('keyup','Enter','Enter',{keyCode:13});await settle();
assert.deepEqual(keys,[[0x01004f60]],'confirming Chinese must not also submit remote chat');
rfb.dispatchEvent(new Event('disconnect'));
console.log('real vendor keyboard: first key, text ingress, dead key, shortcut ownership and candidate navigation passed');
""")


    def test_browser_reload_stays_local_without_breaking_remote_shortcuts(self):
        self._run_real_vendor_script(r"""
import assert from 'node:assert/strict';
import { installDesktopIME } from './app/meadow-ime.js';
class Element extends EventTarget {
  constructor(){super();this.value='';this.style={cssText:''};}
  setAttribute(){} focus(){} remove(){this.removed=true;}
}
let input;
globalThis.document={createElement:()=>input=new Element(),body:{appendChild(){}}};
globalThis.window=new EventTarget();globalThis.innerWidth=1280;globalThis.innerHeight=800;
const rfb=new EventTarget(),keys=[];rfb.sendKey=(...args)=>keys.push(args.slice(0,3));
installDesktopIME(rfb,new Element());
function event(type,fields={}) {
  const e=new Event(type,{cancelable:true});
  for(const[k,v]of Object.entries(fields))Object.defineProperty(e,k,{value:v});
  input.dispatchEvent(e);return e;
}
function key(type,key,code,more={}) {
  return event(type,{key,code,ctrlKey:false,shiftKey:false,altKey:false,
    metaKey:false,getModifierState:()=>false,...more});
}
key('keydown','Meta','MetaLeft',{metaKey:true});
const reload=key('keydown','r','KeyR',{metaKey:true});
assert.equal(reload.defaultPrevented,false,'Command+R must keep browser reload default');
key('keyup','Meta','MetaLeft');key('keyup','r','KeyR');
assert.deepEqual(keys,[],
  'local reload must not send deferred Command or R to the guest');keys.length=0;
key('keydown','Meta','MetaLeft',{metaKey:true});
key('keydown','Shift','ShiftLeft',{metaKey:true,shiftKey:true});
const hardReload=key('keydown','R','KeyR',{metaKey:true,shiftKey:true});
assert.equal(hardReload.defaultPrevented,false,'Command+Shift+R must stay in browser');
key('keyup','R','KeyR',{metaKey:true,shiftKey:true});
key('keyup','Shift','ShiftLeft',{metaKey:true});key('keyup','Meta','MetaLeft');
assert.deepEqual(keys,[[0xffe1,'ShiftLeft',true],[0xffe1,'ShiftLeft',false]],
 'hard reload must release both held remote modifiers and never send R');keys.length=0;
key('keydown','Meta','MetaLeft',{metaKey:true});
const selectAll=key('keydown','a','KeyA',{metaKey:true});
key('keyup','a','KeyA',{metaKey:true});key('keyup','Meta','MetaLeft');
assert.equal(selectAll.defaultPrevented,true,'other Command shortcuts remain remote');
assert.deepEqual(keys,[[0xffe3,'ControlLeft',true],[97,'KeyA',true],
 [97,'KeyA',false],[0xffe3,'ControlLeft',false]]);keys.length=0;
const ctrlReload=key('keydown','r','KeyR',{ctrlKey:true});
key('keyup','r','KeyR',{ctrlKey:true});key('keyup','Control','ControlLeft');
assert.equal(ctrlReload.defaultPrevented,true,'local Command policy must not hijack Ctrl+R');
assert.deepEqual(keys,[[0xffe3,'ControlLeft',true],[114,'KeyR',true],
 [114,'KeyR',false],[0xffe3,'ControlLeft',false]]);keys.length=0;
event('compositionstart');input.value='ㄋㄧˇ';
key('keydown','Meta','MetaLeft',{metaKey:true,isComposing:true});
const composingKey=key('keydown','r','KeyR',{metaKey:true,isComposing:true});
key('keyup','r','KeyR',{metaKey:true,isComposing:true});
key('keyup','Meta','MetaLeft',{isComposing:true});
assert.equal(composingKey.defaultPrevented,false,'IME retains default event handling');
assert.deepEqual(keys,[],'browser-reload policy must not re-grab keyboard during composition');
input.value='你';event('compositionend',{data:'你'});
await new Promise(resolve=>setTimeout(resolve,5));
assert.deepEqual(keys,[[0x01004f60]],'composition still commits once after local shortcut policy');
rfb.dispatchEvent(new Event('disconnect'));
console.log('real vendor keyboard: Command reload stays local, modifiers release, other shortcuts and IME preserved');
""")


    def test_mac_editing_allowlist_shift_and_paste_domains(self):
        self._run_real_vendor_script(r"""
import assert from 'node:assert/strict';
import { installDesktopIME } from './app/meadow-ime.js';
class Element extends EventTarget {
  constructor(){super();this.value='';this.style={cssText:''};}
  setAttribute(){} focus(){} remove(){}
}
let input;
globalThis.document={createElement:()=>input=new Element(),body:{appendChild(){}}};
globalThis.window=new EventTarget();globalThis.innerWidth=1280;globalThis.innerHeight=800;
const rfb=new EventTarget(),keys=[];rfb.sendKey=(...args)=>keys.push(args.slice(0,3));
installDesktopIME(rfb,new Element());
function event(type,fields={}) {
  const e=new Event(type,{cancelable:true});
  for(const[k,v]of Object.entries(fields))Object.defineProperty(e,k,{value:v});
  input.dispatchEvent(e);return e;
}
function key(type,key,code,more={}) {return event(type,{key,code,ctrlKey:false,
  shiftKey:false,altKey:false,metaKey:false,getModifierState:()=>false,...more});}
const cases=[['a','KeyA'],['c','KeyC'],['x','KeyX'],['v','KeyV'],['z','KeyZ'],['y','KeyY'],
 ['f','KeyF'],['s','KeyS'],['o','KeyO'],['p','KeyP'],['n','KeyN'],['Enter','Enter'],
 ['+','Equal'],['=','Equal'],['-','Minus'],['0','Digit0']];
for(const physical of [false,true]) for(const[k,c]of cases) {
  keys.length=0;
  if(physical)key('keydown','Meta','MetaLeft',{metaKey:true});
  const e=key('keydown',k,c,{metaKey:true});key('keyup',k,c,{metaKey:true});
  if(physical)key('keyup','Meta','MetaLeft');
  assert.equal(e.defaultPrevented,true,k+' must route to the native app');
  assert.equal(keys[0][0],0xffe3,k+' must begin Control, not Super');
  assert.equal(keys.at(-1)[0],0xffe3);assert.equal(keys.at(-1)[2],false);
  assert.equal(keys.some(row=>row[0]===0xffeb||row[0]===0xffec),false);
}
keys.length=0;
key('keydown','Meta','MetaLeft',{metaKey:true});
key('keydown','Shift','ShiftLeft',{metaKey:true,shiftKey:true});
key('keydown','Z','KeyZ',{metaKey:true,shiftKey:true});
key('keyup','Z','KeyZ',{metaKey:true,shiftKey:true});
key('keyup','Shift','ShiftLeft',{metaKey:true});key('keyup','Meta','MetaLeft');
let ctrl=false,shift=false,redo=false;
for(const[s,c,down]of keys){if(c==='ControlLeft')ctrl=down;if(c==='ShiftLeft')shift=down;
  if(c==='KeyZ'&&down)redo=ctrl&&shift;}
assert.equal(redo,true,'Command+Shift+Z needs actual guest Ctrl+Shift at Z');
assert.equal(ctrl||shift,false,'no remapped modifier may remain held');
keys.length=0;
key('keydown','Meta','MetaLeft',{metaKey:true});
key('keydown','a','KeyA',{metaKey:true});key('keyup','Meta','MetaLeft');
// macOS commonly omits A's keyup while Command is held. The next actual
// Ctrl+A must still release its own A, rather than a stale Command owner.
keys.length=0;
key('keydown','Control','ControlLeft',{ctrlKey:true});
key('keydown','a','KeyA',{ctrlKey:true});key('keyup','a','KeyA',{ctrlKey:true});
key('keyup','Control','ControlLeft');
assert.deepEqual(keys,[[0xffe3,'ControlLeft',true],[97,'KeyA',true],
 [97,'KeyA',false],[0xffe3,'ControlLeft',false]],'missing old Command keyup cannot swallow a new Ctrl chord release');
keys.length=0;
key('keydown','Meta','MetaRight',{metaKey:true});
key('keydown',' ','Space',{metaKey:true});key('keyup',' ','Space',{metaKey:true});
key('keyup','Meta','MetaRight');
assert.deepEqual(keys,[[0xffec,'MetaRight',true],[32,'Space',true],
 [32,'Space',false],[0xffec,'MetaRight',false]],'unrelated Command shortcuts retain OS Super');
keys.length=0;
key('keydown','Meta','MetaLeft',{metaKey:true});key('keyup','Meta','MetaLeft');
assert.deepEqual(keys,[[0xffeb,'MetaLeft',true],[0xffeb,'MetaLeft',false]],'a lone Command tap retains Super');
keys.length=0;
key('keydown','Meta','MetaLeft',{metaKey:true});
const paste=key('keydown','v','KeyV',{metaKey:true});
assert.equal(paste.defaultPrevented,true,'Command+V must paste the same guest clipboard as C/X');
key('keyup','v','KeyV',{metaKey:true});key('keyup','Meta','MetaLeft');
assert.deepEqual(keys,[[0xffe3,'ControlLeft',true],[118,'KeyV',true],
 [118,'KeyV',false],[0xffe3,'ControlLeft',false]],'guest paste chord remains balanced');
keys.length=0;
input.value='explicit browser paste';event('input',{data:'explicit browser paste',inputType:'insertFromPaste'});
assert.deepEqual(keys,Array.from('explicit browser paste',c=>[c.codePointAt(0)]),
 'an explicit browser Paste action still uses one existing input commit');
keys.length=0;
key('keydown','Shift','ShiftLeft',{shiftKey:true});
key('keydown','Insert','Insert',{shiftKey:true});key('keyup','Insert','Insert',{shiftKey:true});
key('keyup','Shift','ShiftLeft');
assert.deepEqual(keys,[[0xffe1,'ShiftLeft',true],[0xff63,'Insert',true],
 [0xff63,'Insert',false],[0xffe1,'ShiftLeft',false]],'Shift+Insert stays native guest clipboard paste');
rfb.dispatchEvent(new Event('disconnect'));
console.log('Mac allowlist, Command Enter, actual Shift redo, OS Super, explicit browser input and guest clipboard routes passed');
""")

    def test_clipboard_only_edits_and_keyboard_deduplication(self):
        for platform in ('mac', 'windows'):
            with self.subTest(platform=platform):
                self._run_real_vendor_script(r"""
import assert from 'node:assert/strict';
import { installDesktopIME } from './app/meadow-ime.js';
class Element extends EventTarget {
  constructor(){super();this.value='';this.style={cssText:''};}
  setAttribute(){} focus(){} remove(){this.removed=true;}
}
let input;
globalThis.document={createElement:()=>input=new Element(),body:{appendChild(){}},activeElement:null};
globalThis.window=new EventTarget();globalThis.innerWidth=1280;globalThis.innerHeight=800;
Object.defineProperty(globalThis,'navigator',{configurable:true,value:{
  get clipboard(){throw new Error('host clipboard API must not be accessed');}
}});
const container=new Element(),rfb=new EventTarget(),keys=[];
rfb.sendKey=(...args)=>keys.push(args.slice(0,3));
installDesktopIME(rfb,container);document.activeElement=input;
function event(type, fields={}) {
  const e=new Event(type,{cancelable:true});
  for(const [k,v]of Object.entries(fields))Object.defineProperty(e,k,{value:v});
  Object.defineProperty(e,'clipboardData',{get(){throw new Error('clipboard contents must not be accessed');}});
  input.dispatchEvent(e);return e;
}
function key(type,key,code,mods={}) {return event(type,{key,code,ctrlKey:false,
  metaKey:false,shiftKey:false,altKey:false,getModifierState:()=>false,...mods});}
const settle=()=>new Promise(resolve=>setTimeout(resolve,5));
const action=letter=>[[0xffe3,'ControlLeft',true],[letter.codePointAt(0),'Key'+letter.toUpperCase(),true],
 [letter.codePointAt(0),'Key'+letter.toUpperCase(),false],[0xffe3,'ControlLeft',false]];
const downCount=letter=>keys.filter(row=>row[0]===letter.codePointAt(0)&&row[2]===true).length;
assert.equal(event('copy').defaultPrevented,true);
assert.deepEqual(keys,action('c'),'clipboard-only Copy becomes a balanced guest edit');
keys.length=0;
event('cut');event('cut');
assert.deepEqual(keys,[...action('x'),...action('x')],'separate clipboard-only Cut actions never suppress each other');
keys.length=0;
for(const [type,letter]of [['copy','c'],['cut','x']]) {
  // Ctrl is handled by actual vendor code; Mac Command uses the bridge chord.
  for(const modifier of ['ctrlKey',...('PLATFORM'==='mac'?['metaKey']:[])]) {
    keys.length=0;
    key('keydown',letter,'Key'+letter.toUpperCase(),{[modifier]:true});
    const duplicate=event(type);
    assert.equal(duplicate.defaultPrevented,true);
    assert.equal(downCount(letter),1,'keyboard then ClipboardEvent must not double-edit');
    key('keyup',letter,'Key'+letter.toUpperCase(),{[modifier]:true});
    event('blur');await settle();
    keys.length=0;event(type);
    assert.deepEqual(keys,action(letter),'a later menu edit survives previous keyboard ownership');
    await settle();
  }
}
for(const [type,letter]of [['copy','C'],['cut','X']]) {
  keys.length=0;
  key('keydown',letter,'Key'+letter,{ctrlKey:true});
  event(type);
  assert.equal(keys.filter(row=>(row[0]===letter.codePointAt(0)||
    row[0]===letter.toLowerCase().codePointAt(0))&&row[2]===true).length,1,
    'CapsLock Control edit plus ClipboardEvent must not double-edit');
  key('keyup',letter,'Key'+letter,{ctrlKey:true});event('blur');await settle();
}
if('PLATFORM'==='mac') {
  keys.length=0;
  key('keydown','Meta','MetaLeft',{metaKey:true});event('copy');
  key('keyup','Meta','MetaLeft');
  assert.deepEqual(keys,action('c'),'clipboard-only Command edit must not become a later lone Super tap');
}
keys.length=0;
key('keydown','x','KeyX',{metaKey:'PLATFORM'==='mac',ctrlKey:'PLATFORM'!=='mac'});
await settle(); // Even a missing Mac letter keyup cannot retain suppression.
event('cut');
assert.equal(downCount('x'),2,'task boundary expires keyboard deduplication');
event('blur');keys.length=0;
event('compositionstart');input.value='host preedit';
assert.equal(event('copy').defaultPrevented,false,'host preedit Copy keeps its browser default');
assert.equal(event('cut').defaultPrevented,false,'host preedit Cut keeps its browser default');
assert.deepEqual(keys,[]);assert.equal(input.value,'host preedit');
event('compositionend',{data:'中'});input.value='中';event('copy');await settle();
assert.deepEqual(keys,[[0x01004e2d],...action('c')],'pending composition commits before the guest edit');
keys.length=0;document.activeElement=null;
assert.equal(event('copy').defaultPrevented,false,'inactive bridge cannot claim another edit');
assert.deepEqual(keys,[]);document.activeElement=input;
rfb.dispatchEvent(new Event('disconnect'));
assert.equal(event('copy').defaultPrevented,false,'disconnect removes clipboard listener');
assert.deepEqual(keys,[]);
console.log('clipboard-only edits, no host clipboard access, keyboard dedup, IME and lifecycle passed');
""".replace('PLATFORM', platform), platform=platform)

    def test_windows_control_and_guest_paste_stay_native(self):
        self._run_real_vendor_script(r"""
import assert from 'node:assert/strict';
import { installDesktopIME } from './app/meadow-ime.js';
class Element extends EventTarget {
  constructor(){super();this.value='';this.style={cssText:''};}
  setAttribute(){} focus(){} remove(){}
}
let input;
globalThis.document={createElement:()=>input=new Element(),body:{appendChild(){}}};
globalThis.window=new EventTarget();globalThis.innerWidth=1280;globalThis.innerHeight=800;
const rfb=new EventTarget(),keys=[];rfb.sendKey=(...args)=>keys.push(args.slice(0,3));
installDesktopIME(rfb,new Element());
function event(type,fields={}) {
  const e=new Event(type,{cancelable:true});
  for(const[k,v]of Object.entries(fields))Object.defineProperty(e,k,{value:v});
  input.dispatchEvent(e);return e;
}
function key(type,key,code,more={}) {return event(type,{key,code,timeStamp:1000,ctrlKey:false,
  shiftKey:false,altKey:false,metaKey:false,getModifierState:()=>false,...more});}
for(const[k,c]of [['a','KeyA'],['c','KeyC'],['x','KeyX'],['v','KeyV'],['z','KeyZ'],['Enter','Enter']]) {
  keys.length=0;
  key('keydown','Control','ControlLeft',{ctrlKey:true});
  key('keydown',k,c,{ctrlKey:true});key('keyup',k,c,{ctrlKey:true});
  key('keyup','Control','ControlLeft');
  assert.equal(keys.filter(row=>row[0]===0xffe3&&row[2]===true).length,1,
   'Windows AltGr-deferred Control must not duplicate the reconciled Control');
  assert.equal(keys[0][0],0xffe3);assert.equal(keys.at(-1)[2],false);
}
keys.length=0;
key('keydown','Control','ControlLeft',{ctrlKey:true});
const paste=key('keydown','v','KeyV',{ctrlKey:true});
assert.equal(paste.defaultPrevented,true,'Windows Ctrl+V stays native guest paste');
key('keyup','v','KeyV',{ctrlKey:true});key('keyup','Control','ControlLeft');
assert.deepEqual(keys,[[0xffe3,'ControlLeft',true],[118,'KeyV',true],
 [118,'KeyV',false],[0xffe3,'ControlLeft',false]],'normal Ctrl+V uses guest clipboard once');
keys.length=0;
input.value='explicit paste';event('input',{data:'explicit paste',inputType:'insertFromPaste'});
assert.equal(keys.filter(row=>row.length===1).map(row=>String.fromCodePoint(row[0])).join(''),'explicit paste');
keys.length=0;
key('keydown','Meta','MetaLeft',{metaKey:true});key('keydown','e','KeyE',{metaKey:true});
key('keyup','e','KeyE',{metaKey:true});key('keyup','Meta','MetaLeft');
assert.equal(keys[0][0],0xffeb,'Windows OS Super must remain Super');
rfb.dispatchEvent(new Event('disconnect'));
console.log('Windows Control, no AltGr duplicate, guest paste and OS Super passed');
""", platform='windows')

    def test_send_key_repeat_and_composition_confirmation_never_resubmit(self):
        for platform in ('mac', 'windows'):
            with self.subTest(platform=platform):
                self._run_real_vendor_script(r"""
import assert from 'node:assert/strict';
import { installDesktopIME } from './app/meadow-ime.js';
class Element extends EventTarget {
  constructor(){super();this.value='';this.style={cssText:''};}
  setAttribute(){} focus(){} remove(){}
}
let input;
globalThis.document={createElement:()=>input=new Element(),body:{appendChild(){}}};
globalThis.window=new EventTarget();globalThis.innerWidth=1280;globalThis.innerHeight=800;
const rfb=new EventTarget(),keys=[];rfb.sendKey=(...args)=>keys.push(args.slice(0,3));
installDesktopIME(rfb,new Element());
function event(type,fields={}) {
  const e=new Event(type,{cancelable:true});
  for(const[k,v]of Object.entries(fields))Object.defineProperty(e,k,{value:v});
  input.dispatchEvent(e);return e;
}
function key(type,key,code,more={}) {return event(type,{key,code,keyCode:0,
  ctrlKey:false,shiftKey:false,altKey:false,metaKey:false,repeat:false,
  getModifierState:()=>false,...more});}
const mods='PLATFORM'==='mac'?{metaKey:true}:{ctrlKey:true};
const settle=()=>new Promise(resolve=>setTimeout(resolve,5));
const returnDowns=()=>keys.filter(row=>row[0]===0xff0d&&row[2]===true).length;
for(const code of ['Enter','NumpadEnter']) {
  keys.length=0;
  assert.equal(key('keydown','Enter',code,mods).defaultPrevented,true);
  for(let count=0;count<4;count++)
    assert.equal(key('keydown','Enter',code,{...mods,repeat:true}).defaultPrevented,true);
  key('keydown','Enter',code,mods); // Duplicate down without a browser repeat flag.
  assert.equal(returnDowns(),1,'one held submit key may produce only one guest Return');
  key('keyup','Enter',code,mods);event('blur');
  key('keydown','Enter',code,mods);key('keyup','Enter',code,mods);event('blur');
  assert.equal(returnDowns(),2,'a fresh physical submit press remains usable');
}
keys.length=0;
const modifier='PLATFORM'==='mac'?['Meta','MetaLeft']:['Control','ControlLeft'];
key('keydown',...modifier,mods);key('keydown','Enter','Enter',mods);
key('keyup',...modifier); // Enter-up can be omitted while Command is held.
assert.equal(key('keydown','Enter','Enter',{repeat:true}).defaultPrevented,true,
  'releasing the modifier cannot turn held Enter repeats into newlines');
assert.equal(returnDowns(),1);
key('keydown',...modifier,mods);key('keydown','Enter','Enter',mods);
assert.equal(returnDowns(),2,'a fresh shortcut survives an omitted earlier Enter keyup');
key('keyup',...modifier);key('keydown','Enter','Enter');key('keyup','Enter','Enter');
assert.equal(returnDowns(),3,'a fresh plain Enter remains available after the modifier boundary');
event('blur');
keys.length=0;
key('keydown','z','KeyZ',mods);key('keydown','z','KeyZ',{...mods,repeat:true});
key('keyup','z','KeyZ',mods);event('blur');
assert.equal(keys.filter(row=>row[0]===122&&row[2]===true).length,2,
  'undo repetition remains an editing action');
keys.length=0;
event('compositionstart');input.value='ㄋㄧˇ';
const confirm=key('keydown','Enter','Enter',{...mods,isComposing:true,keyCode:13});
assert.equal(confirm.defaultPrevented,false,'IME owns its confirming key default');
input.value='你';event('compositionend',{data:'你'});
const held=key('keydown','Enter','Enter',{...mods,repeat:true,keyCode:13});
assert.equal(held.defaultPrevented,true,'held candidate Enter cannot insert a host newline after composition');
key('keydown','Enter','Enter',{...mods,keyCode:13});
key('keyup','Enter','Enter',mods);await settle();
assert.equal(returnDowns(),0,'candidate confirmation and its held repeats never submit');
assert.deepEqual(keys,[[0x01004f60]],'confirmed text commits once without an accidental newline');
key('keydown','Enter','Enter',mods);key('keyup','Enter','Enter',mods);event('blur');
assert.equal(returnDowns(),1,'an intentional new submit press works after confirmation keyup');
keys.length=0;
// The first phonetic key can precede compositionstart. Its held key remains
// IME-owned through compositionend, even if Control/Command is then pressed.
key('keydown','z','KeyZ');event('compositionstart');input.value='中';
event('compositionend',{data:'中'});
assert.equal(key('keydown','z','KeyZ',{...mods,repeat:true}).defaultPrevented,true);
key('keyup','z','KeyZ',mods);await settle();
assert.deepEqual(keys,[[0x01004e2d]],'the initial composition key cannot escape into a guest edit');
keys.length=0;
event('compositionstart');input.value='中';
key('keydown',...modifier,{...mods,isComposing:true});
key('keydown','Enter','Enter',{...mods,isComposing:true});
event('compositionend',{data:'中'});key('keyup',...modifier);
key('keydown','Enter','Enter',{repeat:true});await settle();
assert.deepEqual(keys,[[0x01004e2d]],'modifier release keeps held candidate repeats suppressed');
key('keydown',...modifier,mods);key('keydown','Enter','Enter',mods);
key('keyup','Enter','Enter',mods);key('keyup',...modifier);
assert.equal(returnDowns(),1,'fresh submit recovers after an omitted candidate keyup');
rfb.dispatchEvent(new Event('disconnect'));
console.log('PLATFORM: submit repeat, IME confirmation ownership, intentional next send and undo repeat passed');
""".replace('PLATFORM', platform), platform=platform)


if __name__ == '__main__':
    unittest.main()
