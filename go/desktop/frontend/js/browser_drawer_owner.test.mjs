import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';

const source = await readFile(new URL('./browserDrawer.js', import.meta.url), 'utf8');
let moduleSequence = 0;

class Surface {
  constructor() { this.listeners = new Map(); }
  addEventListener(name, listener) {
    if (!this.listeners.has(name)) this.listeners.set(name, []);
    this.listeners.get(name).push(listener);
  }
  dispatchEvent(event) {
    for (const listener of this.listeners.get(event.type) || []) listener(event);
    return true;
  }
}

class Node extends Surface {
  constructor(tag) {
    super();
    this.tagName = tag;
    this.children = [];
    this.attributes = new Map();
    this.className = '';
    this.value = '';
    this.hidden = false;
    this.classList = {
      contains: name => this.className.split(/\s+/).includes(name),
      add: name => { if (!this.classList.contains(name)) this.className += ` ${name}`; },
      remove: name => { this.className = this.className.split(/\s+/).filter(v => v !== name).join(' '); },
    };
  }
  append(...nodes) { this.children.push(...nodes); }
  appendChild(node) { this.append(node); return node; }
  insertBefore(node, before) {
    const index = this.children.indexOf(before);
    if (index < 0) this.append(node);
    else this.children.splice(index, 0, node);
  }
  setAttribute(name, value) { this.attributes.set(name, value); }
  removeAttribute(name) { this.attributes.delete(name); if (name === 'src') this.src = ''; }
  getBoundingClientRect() { return { left: 100, top: 80, width: 620, height: 480 }; }
  focus() {}
}

function descendants(root) {
  return [root, ...root.children.flatMap(descendants)];
}

async function harness(t, { initialize = true, authenticated = true } = {}) {
  const globals = ['document', 'window', 'fetch', 'ResizeObserver', 'requestAnimationFrame'];
  const previous = Object.fromEntries(globals.map(name => [name, globalThis[name]]));
  const document = new Surface();
  document.root = new Node('root');
  document.createElement = tag => new Node(tag);
  document.querySelector = selector => descendants(document.root).find(node =>
    selector.startsWith('#') ? node.id === selector.slice(1) : node.classList?.contains(selector.slice(1)),
  ) || null;
  for (const id of ['rightPanel', 'rpTabs', 'rpWrap', 'diffClose', 'input']) {
    const node = new Node('div');
    node.id = id;
    document.root.append(node);
  }
  const window = new Surface();
  const bus = new Surface();
  bus.on = (name, handler) => bus.addEventListener(name, handler);
  bus.emit = (name, value) => {
    for (const handler of bus.listeners.get(name) || []) handler(value);
  };
  const state = { sessionId: 'chat-a', cwd: '/workspace/a' };
  const requests = [];
  const activations = [];
  let nextPostGate = null;
  let nextPostResponse = null;
  let resolveStatus;
  const status = new Promise(resolve => { resolveStatus = resolve; });
  const fetch = async (path, options) => {
    const body = options.body === undefined ? undefined : JSON.parse(options.body);
    requests.push({ path, method: options.method, body });
    if (path === '/native/browser-embed' && options.method === 'POST' && nextPostGate) {
      const gate = nextPostGate;
      nextPostGate = null;
      await gate;
    }
    const responseOverride = path === '/native/browser-embed' && options.method === 'POST'
      ? nextPostResponse : null;
    if (responseOverride) nextPostResponse = null;
    const value = responseOverride ? responseOverride(body)
      : path === '/native/browser-embed' && options.method === 'POST'
        ? { ok: true, owner_revision: body.owner_revision, owner_session_id: body.session_id,
          owner_workspace_root: `/canonical${body.workspace_root}`, requested_workspace_root: body.workspace_root }
      : path === '/native/browser-embed' && options.method === 'GET'
      ? await status
      : path === '/browser/open'
        ? { session: { url: body.url, ready: true } }
        : path.startsWith('/browser/session') ? { session: null } : {};
    const responseStatus = value?.__httpStatus || 200;
    return { ok: responseStatus < 400, status: responseStatus, json: async () => value };
  };
  Object.assign(globalThis, {
    document, window, fetch,
    ResizeObserver: class { observe() {} },
    requestAnimationFrame: callback => queueMicrotask(callback),
  });
  const key = `__browserDrawerTest${++moduleSequence}`;
  globalThis[key] = {
    bus, state,
    showRightPane: (id, options) => {
      activations.push({ id, options });
      document.querySelector('#rightPanel').classList.add('open');
      document.querySelector(`#${id}`).classList.add('on');
    },
    toggleRightPane: () => {},
    assertClientAuthenticated: async () => {
      if (!authenticated) {
        const error = new Error('Please sign in');
        error.status = 401;
        throw error;
      }
    },
  };
  t.after(() => {
    for (const name of globals) {
      if (previous[name] === undefined) delete globalThis[name];
      else globalThis[name] = previous[name];
    }
    delete globalThis[key];
  });
  const adapted = source
    .replace(/^import \{ bus, state, assertClientAuthenticated \} from .*;$/m, `const { bus, state, assertClientAuthenticated } = globalThis.${key};`)
    .replace(/^import \{ showRightPane, toggleRightPane \} from .*;$/m,
      `const { showRightPane, toggleRightPane } = globalThis.${key};`);
  const module = await import(`data:text/javascript;base64,${Buffer.from(adapted).toString('base64')}`);
  if (initialize) module.initBrowserDrawer();
  const flush = async () => { for (let n = 0; n < 40; n++) await Promise.resolve(); };
  return {
    module, state, bus, window, document, requests, activations, flush,
    setAuthenticated: value => {
      authenticated = value;
      window.dispatchEvent({ type: 'commercial:change', detail: {
        capabilitiesReady: true, authenticationRequired: true, authenticated: value,
      } });
    },
    status: async value => { resolveStatus(value); await flush(); },
    navigation: detail => window.dispatchEvent({ type: 'meadow:embedded-browser-navigation', detail }),
    holdNextPost: () => {
      let release;
      nextPostGate = new Promise(resolve => { release = resolve; });
      return release;
    },
    respondNextPost: callback => { nextPostResponse = callback; },
    opens: () => requests.filter(value => value.path === '/browser/open'),
    posts: () => requests.filter(value => value.path === '/native/browser-embed' && value.method === 'POST'),
  };
}

test('native owned navigation opens the pane and updates address without mirroring or reload', async t => {
  const h = await harness(t);
  for (const event of ['update:tool_call_draft', 'update:tool_call', 'update:tool_call_update']) {
    h.bus.emit(event, { sessionId: 'chat-a', rawInput: { url: 'https://tool.example/' } });
  }
  h.navigation({ session_id: 'chat-a', url: 'https://actual.example/redirected' });
  await h.status({ embedded: true, agent_control: true });
  assert.equal(h.opens().length, 0);
  assert.equal(h.document.querySelector('.bd-address').value, 'https://actual.example/redirected');
  assert.equal(h.document.querySelector('.bd-frame').src, undefined);
  assert.ok(h.activations.some(value => value.id === 'rpPageBrowser' && value.options.automatic));
  assert.ok(h.posts().some(value => value.body.visible));
  assert.ok(h.posts().every(value => !value.body.action && !value.body.url));
});

test('signed-out Browser never navigates through the iframe fallback', async t => {
  const h = await harness(t, { authenticated: false });
  await h.flush();
  const address = h.document.querySelector('.bd-address');
  address.value = 'https://must-not-open.example/';
  h.document.querySelector('.bd-primary').dispatchEvent({ type: 'click' });
  await h.flush();
  assert.equal(h.requests.length, 0);
  assert.ok(!h.document.querySelector('.bd-frame').src);
  assert.match(h.document.querySelector('.bd-empty').textContent, /登入 Meadow/);
  await assert.rejects(h.module.bindEmbeddedBrowserOwner('chat-a'), { status: 401 });
});

test('account login retries Browser capability discovery and logout blocks navigation', async t => {
  const h = await harness(t, { authenticated: false });
  await h.flush();
  h.setAuthenticated(true);
  await h.status({ embedded: true, agent_control: true });
  assert.ok(h.requests.some(call => call.path === '/native/browser-embed'));
  h.setAuthenticated(false);
  await h.flush();
  const count = h.requests.length;
  h.document.querySelector('.bd-address').value = 'https://must-not-open.example/';
  h.document.querySelector('.bd-primary').dispatchEvent({ type: 'click' });
  await h.flush();
  assert.equal(h.requests.length, count);
  assert.equal(h.opens().length, 0);
});

test('native navigation ignores another session and tool URLs do not change the owned page', async t => {
  const h = await harness(t);
  await h.status({ embedded: true, agent_control: true });
  h.navigation({ session_id: 'chat-b', url: 'https://other.example/' });
  h.navigation({ url: 'https://unowned.example/' });
  h.bus.emit('update:tool_call', { sessionId: 'chat-a', rawInput: { url: 'https://tool.example/' } });
  await h.flush();
  assert.equal(h.activations.length, 0);
  assert.equal(h.document.querySelector('.bd-address').value, '');
  assert.equal(h.opens().length, 0);
});

test('every native layout and chrome action carries the current session and workspace', async t => {
  const h = await harness(t);
  await h.status({ supported: true, agent_control: true });
  await h.module.bindEmbeddedBrowserOwner('chat-a');
  const first = h.posts().length;
  assert.ok(first > 0);
  assert.ok(h.posts().every(value => value.body.session_id === 'chat-a' && value.body.workspace_root === '/workspace/a'));
  h.state.sessionId = 'chat-b';
  h.state.cwd = '/workspace/b';
  h.bus.emit('thread:switched');
  await h.flush();
  h.state.cwd = '/workspace/b-new';
  h.bus.emit('workspace:changed');
  await h.flush();
  h.bus.emit('session_created');
  h.window.dispatchEvent({ type: 'resize' });
  h.document.querySelector('#rpPageBrowser').children[0].children[2].dispatchEvent({ type: 'click' });
  await h.flush();
  assert.ok(h.posts().slice(first).every(value => value.body.session_id === 'chat-b'));
  assert.ok(h.posts().some(value => value.body.workspace_root === '/workspace/b'));
  assert.ok(h.posts().some(value => value.body.workspace_root === '/workspace/b-new' && value.body.action === 'reload'));
  const revisions = h.posts().map(value => value.body.owner_revision);
  assert.ok(revisions.every((revision, index) => Number.isSafeInteger(revision) && revision > (revisions[index - 1] || 0)));
});

test('queued legacy tool observations retain one iframe fallback navigation after status loads', async t => {
  const h = await harness(t);
  for (const event of ['update:tool_call_draft', 'update:tool_call', 'update:tool_call_update']) {
    h.bus.emit(event, { sessionId: 'chat-a', rawInput: { url: 'https://legacy.example/' } });
  }
  assert.equal(h.opens().length, 0);
  await h.status({ embedded: false, agent_control: false });
  await h.flush();
  assert.equal(h.opens().length, 1);
  assert.equal(h.document.querySelector('.bd-frame').src, 'https://legacy.example/');
  assert.equal(h.posts().length, 0);
});

test('manual native open remains available and follows completed ownership binding', async t => {
  const h = await harness(t);
  await h.status({ embedded: true, agent_control: true });
  h.document.querySelector('.bd-address').value = 'https://manual.example/';
  h.document.querySelector('.bd-primary').dispatchEvent({ type: 'click' });
  await h.flush();
  assert.equal(h.opens().length, 1);
  const openIndex = h.requests.findIndex(value => value.path === '/browser/open');
  assert.ok(h.requests.slice(0, openIndex).some(value => value.method === 'POST' && value.path === '/native/browser-embed' && value.body.session_id === 'chat-a'));
});

test('admission rejects another owner or an uninitialized native drawer', async t => {
  const h = await harness(t, { initialize: false });
  await assert.rejects(h.module.bindEmbeddedBrowserOwner('chat-b'), /active conversation/);
  const binding = h.module.bindEmbeddedBrowserOwner('chat-a');
  await h.status({ embedded: true, agent_control: true });
  await assert.rejects(binding, /not initialized/);
  assert.equal(h.posts().length, 0);
});

test('admission rechecks the expected session after delayed capability discovery', async t => {
  const h = await harness(t);
  const binding = h.module.bindEmbeddedBrowserOwner('chat-a');
  h.state.sessionId = 'chat-b';
  h.state.cwd = '/workspace/b';
  await h.status({ embedded: true, agent_control: true });
  await assert.rejects(binding, /changed before navigation admission/);
  assert.ok(h.posts().every(value => value.body.session_id === 'chat-b'));
});

test('admission rechecks the expected workspace after delayed capability discovery', async t => {
  const h = await harness(t);
  const binding = h.module.bindEmbeddedBrowserOwner('chat-a');
  h.state.cwd = '/workspace/a-next';
  await h.status({ embedded: true, agent_control: true });
  await assert.rejects(binding, /changed before navigation admission/);
  assert.ok(h.posts().every(value => value.body.workspace_root === '/workspace/a-next'));
});

test('late ownership response rejects admission without granting the old chat again', async t => {
  const h = await harness(t);
  await h.status({ embedded: true, agent_control: true });
  const before = h.posts().length;
  const release = h.holdNextPost();
  const binding = h.module.bindEmbeddedBrowserOwner('chat-a');
  await h.flush();
  h.state.sessionId = 'chat-b';
  h.state.cwd = '/workspace/b';
  h.bus.emit('thread:switched');
  release();
  await assert.rejects(binding, /changed during navigation admission/);
  await h.flush();
  assert.deepEqual(h.posts().slice(before).map(value => value.body.session_id), ['chat-a', 'chat-b']);
});

test('renderer reload continues the retained native revision and accepts canonical workspace ACK', async t => {
  const h = await harness(t);
  await h.status({ embedded: true, agent_control: true, owner_revision: 50,
    owner_session_id: 'earlier-chat', owner_workspace_root: '/canonical/earlier' });
  assert.equal(h.posts()[0].body.owner_revision, 51);
  await h.module.bindEmbeddedBrowserOwner('chat-a');
  assert.equal(h.posts().at(-1).body.owner_revision, 52);
});

for (const [name, receipt] of [
  ['missing acknowledgment', () => ({})],
  ['stale revision', body => ({ ok: true, owner_revision: body.owner_revision - 1,
    owner_session_id: body.session_id, requested_workspace_root: body.workspace_root })],
  ['wrong session', body => ({ ok: true, owner_revision: body.owner_revision,
    owner_session_id: 'other-chat', requested_workspace_root: body.workspace_root })],
  ['wrong requested workspace', body => ({ ok: true, owner_revision: body.owner_revision,
    owner_session_id: body.session_id, requested_workspace_root: '/wrong/workspace' })],
]) {
  test(`ownership admission rejects ${name}`, async t => {
    const h = await harness(t);
    await h.status({ embedded: true, agent_control: true, owner_revision: 50 });
    h.respondNextPost(receipt);
    await assert.rejects(h.module.bindEmbeddedBrowserOwner('chat-a'), /acknowledgment does not match/);
  });
}

test('ownership admission propagates the native stale revision HTTP conflict', async t => {
  const h = await harness(t);
  await h.status({ embedded: true, agent_control: true, owner_revision: 50 });
  h.respondNextPost(() => ({ __httpStatus: 409, error: 'stale owner revision' }));
  await assert.rejects(h.module.bindEmbeddedBrowserOwner('chat-a'), /stale owner revision/);
});

test('agent controlled capability without a native pane cannot admit an unacknowledged owner', async t => {
  const h = await harness(t);
  await h.status({ embedded: false, agent_control: true, owner_revision: 50 });
  await assert.rejects(h.module.bindEmbeddedBrowserOwner('chat-a'), /unavailable for ownership binding/);
  assert.equal(h.posts().length, 0);
});
