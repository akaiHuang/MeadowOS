import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

// Run the real navigation, account listeners, and timeline/notice helpers.
// Only unrelated sidebar layout and polling are replaced; no Wails, member,
// filesystem, or model transport is contacted by these deferred fixtures.
const source = readFileSync(new URL('./sidebar.js', import.meta.url), 'utf8')
  .replace(/^import[\s\S]*?from '[^']+';\n/gm, '')
  .replace(/^export /gm, '');
const deferred = () => {
  let resolve;
  let reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
};
const flush = async () => {
  for (let index = 0; index < 12; index += 1) await Promise.resolve();
  await new Promise(setImmediate);
};
const snapshot = value => JSON.parse(JSON.stringify(value));
const created = (sessionId = 'created-session', extra = {}) => ({
  sessionId, modelId: 'created-model', connectionId: 'created-provider', ...extra,
});
const resumed = (sessionId = 'resumed-session', extra = {}) => ({
  sessionId, chatId: 'chosen-chat', modelId: 'chosen-model', connectionId: 'chosen-provider',
  messages: [{ role: 'user', content: 'Chosen history' }], files: [], tools: [],
  replayAfterEventId: 7, ...extra,
});

class Surface {
  listeners = new Map();
  addEventListener(name, callback) {
    const entries = this.listeners.get(name) || [];
    entries.push(callback);
    this.listeners.set(name, entries);
  }
  removeEventListener(name, callback) {
    this.listeners.set(name, (this.listeners.get(name) || []).filter(entry => entry !== callback));
  }
  dispatchEvent(event) {
    for (const callback of this.listeners.get(event.type) || []) callback(event);
  }
}
class Node extends Surface {
  children = [];
  className = '';
  textContent = '';
  dataset = {};
  style = {};
  disabledWrites = [];
  classList = { add() {}, remove() {}, toggle() {} };
  get disabled() { return this.isDisabled || false; }
  set disabled(value) { this.isDisabled = value; this.disabledWrites.push(value); }
  get childElementCount() { return this.children.length; }
  get innerHTML() { return this.html || ''; }
  set innerHTML(value) {
    this.html = value;
    this.children = [];
    if (value.includes('class="pill"')) {
      const pill = new Node(); pill.className = 'pill'; this.children.push(pill);
    }
  }
  appendChild(node) {
    if (node.fragment) this.children.push(...node.children);
    else this.children.push(node);
    return node;
  }
  append(...nodes) { for (const node of nodes) this.appendChild(node); }
  querySelector(selector) {
    return descendants(this).find(node => selector === `.${node.className}`) || null;
  }
  setAttribute() {}
  remove() { this.removed = true; }
}
const descendants = node => [node, ...node.children.flatMap(descendants)];
const nodeSnapshot = node => ({
  className: node.className, text: node.textContent, html: node.innerHTML,
  children: node.children.map(nodeSnapshot),
});

function harness() {
  let access = true;
  let generation = 3;
  let nextTimer = 0;
  const authorizationGates = [];
  const createGates = [];
  const resumeGates = [];
  const calls = { authorization: [], create: [], resume: [], connections: [], emitted: [], errors: [] };
  const state = {
    cwd: '/project/original', sessionId: 'original-session', runId: 'original-run',
    model: 'original-model', modelConnectionID: 'original-provider',
  };
  const nodes = new Map();
  const body = new Node();
  for (const id of ['newChatBtn', 'timeline', 'threadTitle', 'pillFolderName', 'scroll']) {
    const node = new Node(); node.id = id; nodes.set(`#${id}`, node); body.appendChild(node);
  }
  nodes.get('#timeline').innerHTML = 'Original history';
  nodes.get('#threadTitle').textContent = 'Original title';
  const storage = new Map([
    ['meadow.projects', JSON.stringify([{ cwd: state.cwd }])],
    ['meadow.activeCwd', state.cwd], ['meadow.cwd', state.cwd],
  ]);
  const document = {
    body, querySelector: selector => nodes.get(selector) || null, querySelectorAll: () => [],
    getElementById: id => descendants(body).find(node => node.id === id) || null,
    createElement: () => new Node(),
    createDocumentFragment: () => { const node = new Node(); node.fragment = true; return node; },
  };
  const window = new Surface();
  const bus = new Surface();
  bus.on = bus.addEventListener.bind(bus);
  bus.emit = (name, data) => {
    calls.emitted.push({ name, data: snapshot(data) });
    for (const callback of bus.listeners.get(name) || []) callback(data);
  };
  const context = {
    window, document, bus, state,
    console: { error: (...args) => calls.errors.push(args), warn: (...args) => calls.errors.push(args) },
    localStorage: {
      getItem: key => storage.get(key) ?? null,
      setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key),
    },
    hasClientAccess: () => access, clientAccountGeneration: () => generation,
    authorizeNewConversation: () => {
      const authorization = { requestID: `authorization-${calls.authorization.length + 1}` };
      calls.authorization.push(authorization);
      return authorizationGates.shift()?.promise || Promise.resolve(authorization);
    },
    daemon: {
      createSession: (cwd, authorization) => {
        calls.create.push({ cwd, authorization });
        return createGates.shift()?.promise || Promise.resolve(created());
      },
      listSessions: () => { throw new Error('Unrelated sidebar polling must not run.'); },
    },
    connectEvents: (sid, replayAfterEventId) => calls.connections.push({ sid, replayAfterEventId }),
    fetch: async (path, options) => {
      assert.equal(path, '/session/resume');
      const body = JSON.parse(options.body);
      calls.resume.push({ path, body });
      const response = await (resumeGates.shift()?.promise || Promise.resolve(resumed()));
      return { ok: true, status: 200, json: async () => response };
    },
    md: value => String(value), mountCommercialAccount() {}, sidebarListingNotice: () => '',
    createSidebarSessionCache: () => ({ snapshot: () => ({ rows: [], status: 'ready' }), sync() {}, refresh() {} }),
    setTimeout: () => ++nextTimer, clearTimeout() {}, setInterval: () => ++nextTimer, clearInterval() {},
  };
  vm.runInNewContext(source + `
    buildResizer = () => {}; buildSearchRow = () => {};
    buildProjHead = () => {}; buildFooter = () => {};
    refreshAll = () => {}; scheduleRefresh = () => {}; refreshUsageLines = () => {};
    initSidebar();
    globalThis.api = { startNewThread, openThread, setActiveCwd };
  `, context, { filename: 'sidebar.js' });
  const hold = queue => { const gate = deferred(); queue.push(gate); return gate; };
  return {
    ...context.api, state, calls, window, bus, storage,
    button: nodes.get('#newChatBtn'), timeline: nodes.get('#timeline'), body,
    delayAuthorization: () => hold(authorizationGates), delayCreate: () => hold(createGates),
    delayResume: () => hold(resumeGates),
    account(authenticated, { bump = true, event = 'commercial:change' } = {}) {
      access = authenticated;
      if (bump) generation += 1;
      if (event === 'authentication_required') bus.emit(event, {});
      else window.dispatchEvent({ type: event, detail: { authenticated } });
    },
    refreshStatus(detail = { authenticated: true }) {
      window.dispatchEvent({ type: 'commercial:change', detail });
    },
    generationOnly() { generation += 1; },
    view: () => snapshot({
      state, timeline: nodeSnapshot(nodes.get('#timeline')),
      title: nodes.get('#threadTitle').textContent, storage: [...storage],
      connections: calls.connections, emitted: calls.emitted,
    }),
  };
}

test('manual resume preserves its selection and history when an older create response arrives', async () => {
  const h = harness();
  const createGate = h.delayCreate();
  const pending = h.startNewThread(); await flush();
  assert.equal(h.calls.create.length, 1);
  await h.openThread('chosen-chat', 'Chosen title', '/project/chosen');
  assert.equal(h.state.sessionId, 'resumed-session');
  assert.equal(h.state.modelConnectionID, 'chosen-provider');
  assert.equal(h.button.disabled, false);
  const chosen = h.view();
  createGate.resolve(created('late-create', { workspaceCwd: '/must-not-become-active' }));
  assert.equal(await pending, null);
  assert.deepEqual(h.view(), chosen);
  assert.deepEqual(h.calls.errors, []);
});

test('an authorization held across logout and login cannot create a session under the new account generation', async () => {
  const h = harness(); const authorization = h.delayAuthorization();
  const pending = h.startNewThread(); await flush();
  assert.equal(h.button.disabled, true);
  h.account(false); h.account(true);
  assert.equal(h.button.disabled, false);
  const chosen = h.view();
  authorization.resolve({ requestID: 'old-account-authorization' });
  assert.equal(await pending, null);
  assert.equal(h.calls.create.length, 0);
  assert.deepEqual(h.view(), chosen);
});

test('authentication_required invalidates a held start even when the private generation has not changed yet', async () => {
  const h = harness(); const authorization = h.delayAuthorization();
  const pending = h.startNewThread(); await flush();
  h.account(true, { bump: false, event: 'authentication_required' });
  assert.equal(h.button.disabled, false);
  authorization.resolve({ requestID: 'stale-authorization' });
  assert.equal(await pending, null);
  assert.equal(h.calls.create.length, 0);
});

test('stale authorization and create failures leave the selected timeline and toast stack untouched', async () => {
  for (const phase of ['authorization', 'create']) {
    const h = harness();
    const gate = phase === 'authorization' ? h.delayAuthorization() : h.delayCreate();
    const pending = h.startNewThread(); await flush();
    await h.openThread('chosen-chat', 'Chosen title', '/project/chosen');
    const chosen = h.view();
    gate.reject(Object.assign(new Error('old request failed'), { userMessage: 'Stale failure must stay silent' }));
    assert.equal(await pending, null);
    assert.deepEqual(h.view(), chosen);
    assert.deepEqual(h.calls.errors, []);
    assert.equal(descendants(h.body).filter(node => /git-toast|divider/.test(node.className)).length, 0);
  }
});

test('an older create finally cannot enable the button while a newer create still owns it', async () => {
  const h = harness(); const firstGate = h.delayCreate();
  const first = h.startNewThread(); await flush();
  const secondGate = h.delayCreate();
  const second = h.startNewThread(); await flush();
  assert.equal(h.calls.create.length, 2);
  firstGate.resolve(created('older-created-session'));
  assert.equal(await first, null);
  assert.equal(h.button.disabled, true);
  assert.equal(h.state.sessionId, 'original-session');
  secondGate.resolve(created('newer-created-session'));
  assert.equal((await second).sessionId, 'newer-created-session');
  assert.equal(h.button.disabled, false);
  assert.deepEqual(h.calls.connections.map(connection => connection.sid), ['newer-created-session']);
});

test('a periodic same-account status refresh does not cancel a held create or release its button', async () => {
  const h = harness(); const gate = h.delayCreate();
  const pending = h.startNewThread(); await flush();
  h.refreshStatus();
  // Public event details are not an account identity authority.
  h.refreshStatus({ authenticated: false, accountGeneration: 'forged-new-account' });
  assert.equal(h.button.disabled, true);
  gate.resolve(created('still-owned-session', { connectionId: 'provider/exact-binding', modelId: 'model/exact-binding' }));
  const result = await pending;
  assert.equal(result.sessionId, 'still-owned-session');
  assert.equal(h.state.modelConnectionID, 'provider/exact-binding');
  assert.equal(h.state.model, 'model/exact-binding');
  assert.equal(h.button.disabled, false);
});

test('invalid daemon session IDs never replace state or attach an event stream', async () => {
  for (const sessionId of [undefined, null, '', 42, 'bad id', 'bad\nline', 'bad\0id']) {
    const h = harness(); const gate = h.delayCreate();
    const before = snapshot(h.state);
    const pending = h.startNewThread(); await flush();
    gate.resolve({ sessionId, modelId: 'must-not-select', connectionId: 'must-not-select' });
    assert.equal(await pending, null);
    assert.deepEqual(snapshot(h.state), before);
    assert.equal(h.calls.connections.length, 0);
    assert.equal(h.calls.emitted.filter(event => event.name === 'session_created').length, 0);
    assert.equal(h.button.disabled, false);
  }
});

test('cwd changes during authorization cannot silently retarget create to another project', async () => {
  const h = harness(); const authorization = h.delayAuthorization();
  const pending = h.startNewThread(); await flush();
  h.setActiveCwd('/project/other');
  const chosen = h.view();
  authorization.resolve({ requestID: 'project-original-intent' });
  assert.equal(await pending, null);
  assert.equal(h.calls.create.length, 0);
  assert.deepEqual(h.view(), chosen);
});

test('create uses its captured cwd and cannot canonicalize or replace a later project choice', async () => {
  const h = harness(); const gate = h.delayCreate();
  const pending = h.startNewThread(); await flush();
  assert.equal(h.calls.create[0].cwd, '/project/original');
  assert.strictEqual(h.calls.create[0].authorization, h.calls.authorization[0]);
  h.setActiveCwd('/project/other');
  const chosen = h.view();
  gate.resolve(created('old-project-session', { workspaceCwd: '/canonical/original' }));
  assert.equal(await pending, null);
  assert.deepEqual(h.view(), chosen);
});

test('reselecting the already active chat cancels an earlier start and immediately releases its old button', async () => {
  const h = harness(); const gate = h.delayCreate();
  const pending = h.startNewThread(); await flush();
  await h.openThread('original-session', 'Original title', '/project/original');
  assert.equal(h.calls.resume.length, 0);
  assert.equal(h.button.disabled, false);
  gate.resolve(created('superseded-session'));
  assert.equal(await pending, null);
  assert.equal(h.state.sessionId, 'original-session');
  assert.equal(h.calls.connections.length, 0);
});

test('a new create supersedes a held resume without losing the new connection binding', async () => {
  const h = harness(); const resumeGate = h.delayResume();
  const oldResume = h.openThread('older-chat', 'Older title', '/project/original'); await flush();
  const fresh = await h.startNewThread();
  assert.equal(fresh.sessionId, 'created-session');
  const chosen = h.view();
  resumeGate.resolve(resumed('older-live-session'));
  await oldResume;
  assert.deepEqual(h.view(), chosen);
  assert.equal(h.state.modelConnectionID, 'created-provider');
});

test('a current resume failure shows a toast but a resume rejected after authentication_required stays silent', async () => {
  const current = harness(); const currentGate = current.delayResume();
  const currentResume = current.openThread('chosen-chat', 'Chosen title', '/project/original'); await flush();
  currentGate.reject(new Error('Current resume failed'));
  await currentResume;
  const notices = descendants(current.body).filter(node => /git-toast/.test(node.className));
  assert.equal(notices.length, 1);
  assert.match(notices[0].textContent, /Current resume failed/);

  const stale = harness(); const staleGate = stale.delayResume();
  const oldResume = stale.openThread('chosen-chat', 'Chosen title', '/project/original'); await flush();
  stale.account(true, { bump: false, event: 'authentication_required' });
  const chosen = stale.view();
  staleGate.reject(new Error('Old resume must stay silent'));
  await oldResume;
  assert.deepEqual(stale.view(), chosen);
  assert.equal(descendants(stale.body).filter(node => /git-toast|divider/.test(node.className)).length, 0);
  assert.deepEqual(stale.calls.errors, []);
});

test('a session changed independently during authorization cannot receive the older create intent', async () => {
  const h = harness(); const gate = h.delayAuthorization();
  const pending = h.startNewThread(); await flush();
  h.state.sessionId = 'independently-selected-session';
  const chosen = h.view();
  gate.resolve({ requestID: 'original-session-authorization' });
  assert.equal(await pending, null);
  assert.equal(h.calls.create.length, 0);
  assert.deepEqual(h.view(), chosen);
});

test('a changed private generation fences a held create even without a public account event', async () => {
  const h = harness(); const gate = h.delayCreate();
  const pending = h.startNewThread(); await flush();
  h.generationOnly();
  const chosen = h.view();
  gate.resolve(created('old-account-session'));
  assert.equal(await pending, null);
  assert.deepEqual(h.view(), chosen);
});
