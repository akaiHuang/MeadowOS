import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { webcrypto } from 'node:crypto';
import { JSDOM } from 'jsdom';
import { applyMemberBackgroundUI } from '../../../../scripts/member-web/background-ui.mjs';

const root = new URL('../', import.meta.url);
const names = ['rightpanel.js', 'diffpanel.js', 'preview.js', 'composer.js', 'composer_queue.js'];
const sources = Object.fromEntries(await Promise.all(names.map(async name => [name, await readFile(new URL('js/'+name, root), 'utf8')])));
const html = await readFile(new URL('index.html', root), 'utf8');
const css = await readFile(new URL('style.css', root), 'utf8');
const imported = source => source.replace(/^import[\s\S]*?from ['"][^'"]+['"];\n/gm, '').replace(/^export /gm, '');
const flush = async () => { for (let n=0;n<3;n++) await new Promise(setImmediate); };

function fixture(member, t) {
  const dom = new JSDOM(html, { url: 'https://owned-fixture.invalid/' });
  t.after(() => dom.window.close());
  dom.window.matchMedia = () => ({ matches: false, addEventListener() {} });
  const handlers = new Map(), votes = [];
  const bus = { on(name, fn) { if (!handlers.has(name)) handlers.set(name, []); handlers.get(name).push(fn); }, emit(name, data) { for (const fn of handlers.get(name)||[]) fn(data); } };
  const state = { cwd: '/owned/work', sessionId: 'active-session', runId: 'run-one', busy: false };
  const context = vm.createContext({
    document: dom.window.document, window: dom.window, Event: dom.window.Event,
    localStorage: dom.window.localStorage, location: dom.window.location,
    bus, state, md: value => value, crypto: webcrypto,
    hasClientAccess: () => true, clientAccountGeneration: () => 0,
    esc: value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;'),
    daemon: { queueList: async () => ({ items: [] }), vote: async (...args) => { votes.push(args); } },
    AbortController, URL, setTimeout, clearTimeout, console,
    ResizeObserver: class { observe() {} },
    fetch: async path => ({ ok: true, status: 200, json: async () => path === '/vcs/diff' ? { diff: '+raw changed content\n' } : { contents: 'raw written artifact', binary: false } }),
  });
  const actual = name => member ? applyMemberBackgroundUI(sources[name], name) : sources[name];
  const load = (name, exports) => vm.runInContext('(()=>{\n'+imported(actual(name))+'\nObject.assign(globalThis,{'+exports.join(',')+'});})();', context);
  load('rightpanel.js', ['setRightPanelOpen', 'showRightPane', 'toggleRightPane', 'hideRightPane', 'resetRightPanes', 'hasUserRightPaneSelection']);
  load('diffpanel.js', ['initDiffPanel']);
  load('preview.js', ['initPreview']);
  load('composer_queue.js', ['renderComposerQueue']);
  load('composer.js', ['initInput', 'initPermissionCards']);
  context.initDiffPanel(); context.initPreview(); context.initInput(); context.initPermissionCards();
  const doc = dom.window.document, panel = doc.querySelector('#rightPanel');
  const open = () => panel.classList.contains('open');
  return { dom, doc, context, bus, state, votes, panel, open, close: () => context.setRightPanelOpen(false), dot: () => doc.documentElement.hasAttribute('data-member-task-active') };
}

for (const member of [false, true]) {
  const surface = member ? 'member export' : 'canonical Native';
  test(`${surface}: automatic draft/write/completion/preview remains quiet and explicit opens retain content`, async t => {
    const h = fixture(member, t);
    const scope = { sessionId: 'active-session', runId: 'run-one', toolCallId: 'write-one' };
    const quiet = label => { assert.equal(h.open(), false, label); };
    h.bus.emit('update:tool_call_draft', { ...scope, draftId: 'draft-one', title: 'write_file', argumentsDelta: '{"file_path":"/owned/work/file.txt","content":"streamed draft', bytes: 80 });
    quiet('streamed draft');
    assert.match(h.doc.querySelector('#diffBody').textContent, /streamed draft/);
    h.bus.emit('update:tool_call', { ...scope, draftId: 'draft-one', kind: 'write_file', rawInput: { file_path: '/owned/work/file.txt' } });
    quiet('write tool call');
    h.bus.emit('update:tool_call_update', { ...scope, kind: 'write_file', status: 'completed', changedPaths: ['/owned/work/file.txt'] });
    quiet('write completion');
    assert.match(h.doc.querySelector('#rpFileList').textContent, /file.txt/);
    h.bus.emit('update:tool_call_draft', { ...scope, draftId: 'draft-two', title: 'write_file', argumentsDelta: '{"file_path":"/owned/work/next.txt","content":"unfinished', bytes: 70 });
    h.bus.emit('turn_completed', scope);
    quiet('completion fallback');
    h.bus.emit('preview:open', { path: 'file.txt', automatic: true });
    quiet('automatic preview');
    h.context.showRightPane('rpPageProg', { automatic: true });
    quiet('automatic progress');
    h.bus.emit('diff:open', { path: 'file.txt' });
    assert.equal(h.open(), true, 'explicit diff open'); h.close();
    h.bus.emit('preview:open', { path: 'file.txt' });
    assert.equal(h.open(), true, 'explicit preview open');
    await flush();
    assert.match(h.doc.querySelector('.pv-body').textContent, /raw written artifact/);
    h.close();
    h.doc.querySelector('#rpTabProg').click();
    assert.equal(h.open(), true, 'explicit progress click'); h.close();
    // Automatic browser destinations share the same closed-panel gate.
    const browserPage = h.doc.createElement('div'); browserPage.id='rpPageBrowser'; browserPage.className='rp-page';
    h.doc.querySelector('#rpWrap').appendChild(browserPage);
    h.context.showRightPane(browserPage.id, { automatic: true }); quiet('automatic browser destination');
    h.context.showRightPane(browserPage.id); assert.equal(h.open(), true, 'explicit browser destination'); h.close();
    h.bus.emit('update:tool_call_update', { ...scope, toolCallId: 'failed-write', kind: 'write_file', status: 'failed', rawInput: { file_path: '/owned/work/failed.txt' }, rawOutput: 'original storage error' });
    quiet('failed write does not steal drawer focus');
    assert.match(h.doc.querySelector('#diffBody').textContent, /儲存失敗：original storage error/);
    h.context.showRightPane('rpPageOut');
    assert.match(h.doc.querySelector('#diffBody').textContent, /original storage error/, 'failure remains available when opened');
  });

  test(`${surface}: actual composer events keep busy dot, queue and permission controls through session changes`, async t => {
    const h = fixture(member, t);
    h.bus.emit('run_state', { sessionId: 'active-session', status: 'running' });
    assert.equal(h.state.busy, true); assert.equal(h.dot(), true); assert.equal(h.open(), false);
    h.bus.emit('queue_changed', { sessionId: 'active-session', items: [{ id: 'q-one', status: 'pending', text: 'original queued text' }] });
    assert.equal(h.doc.querySelector('#taskQueue').hidden, false);
    assert.match(h.doc.querySelector('#taskQueue').textContent, /original queued text/);
    h.bus.emit('permission_request', { sessionId: 'active-session', requestId: 'permission-one', toolCall: { title: 'run_shell_command', rawInput: { command: 'fixture only' } } });
    const card = h.doc.querySelector('#pendingArea .perm');
    assert.ok(card); assert.match(card.textContent, /fixture only/);
    card.querySelector('button.y').click(); await flush();
    assert.deepEqual(h.votes, [['active-session', 'permission-one', 'approve']]);
    assert.equal(h.open(), false, 'approval uses its own card without forcing drawer');
    h.state.sessionId='idle-session'; h.state.runId='';
    h.bus.emit('thread:switched', { sessionId: 'idle-session', running: false }); await flush();
    assert.equal(h.dot(), false); assert.equal(h.state.busy, false);
    h.bus.emit('run_state', { sessionId: 'active-session', status: 'running' });
    assert.equal(h.dot(), false, 'other session activity cannot light idle selected chat');
    h.state.sessionId='active-session';
    h.bus.emit('thread:switched', { sessionId: 'active-session', running: true }); await flush();
    assert.equal(h.dot(), true); assert.equal(h.open(), false);
    h.bus.emit('run_state', { sessionId: 'active-session', status: 'idle' });
    assert.equal(h.dot(), false);
    assert.doesNotMatch(h.doc.querySelector('#btnProgress').title, /任務執行中/);
  });
}

test('canonical dot CSS and member export each retain only one quiet implementation', () => {
  assert.match(css, /\[data-member-task-active\] #btnProgress::after/);
  assert.match(css, /background: #4ae48a/);
  for (const name of ['rightpanel.js', 'diffpanel.js', 'preview.js', 'composer.js']) {
    const native=sources[name], exported=applyMemberBackgroundUI(native,name);
    assert.equal(native.startsWith('/* native-background-ui */'),true);
    assert.equal(exported.replace('/* member-background-ui */','/* native-background-ui */'),native);
    assert.throws(()=>applyMemberBackgroundUI(exported,name),/already applied/);
  }
});
