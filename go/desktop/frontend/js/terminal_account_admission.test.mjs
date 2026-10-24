import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const terminalSource = readFileSync(new URL('./terminal.js', import.meta.url), 'utf8')
  .replace(/^import[\s\S]*?from '[^']+';\n/gm, '')
  .replace(/^export /gm, '');

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

class Surface {
  listeners = new Map();
  addEventListener(name, callback) {
    const callbacks = this.listeners.get(name) || [];
    callbacks.push(callback);
    this.listeners.set(name, callbacks);
  }
  dispatchEvent(event) {
    for (const callback of this.listeners.get(event.type) || []) callback(event);
  }
}

class Node extends Surface {
  constructor(tag = 'div') {
    super();
    this.tagName = tag;
    this.children = [];
    this.className = '';
    this.style = {};
    this.hidden = false;
    this.classList = {
      contains: name => this.className.split(/\s+/).includes(name),
      add: name => {
        if (!this.classList.contains(name)) this.className += ` ${name}`;
      },
      remove: name => {
        this.className = this.className.split(/\s+/).filter(value => value !== name).join(' ');
      },
      toggle: (name, force) => {
        const on = force === undefined ? !this.classList.contains(name) : force;
        this.classList[on ? 'add' : 'remove'](name);
        return on;
      },
    };
  }
  append(...nodes) {
    for (const node of nodes) {
      node.parentNode = this;
      this.children.push(node);
    }
  }
  appendChild(node) { this.append(node); return node; }
  insertBefore(node, before) {
    const index = this.children.indexOf(before);
    if (index < 0) return this.appendChild(node);
    node.parentNode = this;
    this.children.splice(index, 0, node);
    return node;
  }
  remove() {
    if (this.parentNode) {
      this.parentNode.children = this.parentNode.children.filter(node => node !== this);
      this.parentNode = null;
    }
  }
  closest() { return null; }
}

// Evaluate the real module in a fresh VM. Only imported account state, browser
// surfaces, xterm, and transport are mocked; no app, PTY, or network is started.
function harness({ authenticated = true } = {}) {
  let access = authenticated;
  let nextRequest = null;
  let nextStatus = 200;
  let nextTimer = 0;
  const timers = new Map();
  const calls = [];
  const admissions = [];
  const streams = [];
  const terminals = [];
  const observers = [];
  const nodes = new Map();
  for (const id of ['termDrawer', 'termTabs', 'termPanes', 'termNew', 'btnTerminal', 'termDragbar']) {
    nodes.set(`#${id}`, new Node());
  }
  nodes.get('#termDrawer').hidden = true;
  nodes.get('#termTabs').append(nodes.get('#termNew'));
  const window = new Surface();
  window.innerHeight = 1000;
  const bus = {
    listeners: new Map(),
    on(name, callback) {
      const callbacks = this.listeners.get(name) || [];
      callbacks.push(callback);
      this.listeners.set(name, callbacks);
    },
    emit(name, value) {
      for (const callback of this.listeners.get(name) || []) callback(value);
    },
  };
  class Terminal {
    constructor(options) {
      Object.assign(this, { options, cols: 80, rows: 24, inputReaders: [], writes: [], resets: 0 });
      terminals.push(this);
    }
    loadAddon() {}
    open() {}
    attachCustomKeyEventHandler() {}
    focus() {}
    write(value) { this.writes.push(value); }
    reset() { this.resets++; }
    onData(callback) {
      const reader = { callback, disposed: false, dispose() { this.disposed = true; } };
      this.inputReaders.push(reader);
      return reader;
    }
    dispose() { this.disposed = true; }
  }
  const FitAddon = { FitAddon: class { fit() {} } };
  Object.assign(window, { Terminal, FitAddon });
  const context = {
    console, window, bus, Terminal, FitAddon, Element: Node, TextEncoder,
    atob: value => Buffer.from(value, 'base64').toString('latin1'),
    btoa: value => Buffer.from(value, 'latin1').toString('base64'),
    state: { sessionId: 'chat-one', cwd: '/workspace/one' },
    getTheme: () => 'dark',
    hasClientAccess: () => access,
    setClientAuthentication(status) {
      admissions.push(status);
      access = status?.authenticated === true;
    },
    document: {
      querySelector: selector => nodes.get(selector) || null,
      createElement: tag => new Node(tag),
    },
    setTimeout(callback, delay) {
      const id = ++nextTimer;
      timers.set(id, { callback, delay });
      return id;
    },
    clearTimeout: id => timers.delete(id),
    EventSource: class {
      constructor(path) { this.path = path; streams.push(this); }
      close() { this.closed = true; }
    },
    ResizeObserver: class {
      constructor(callback) { this.callback = callback; observers.push(this); }
      observe() {}
      disconnect() { this.disconnected = true; }
    },
    fetch: async (path, options) => {
      calls.push({ path, ...options });
      const gate = nextRequest;
      nextRequest = null;
      const status = nextStatus;
      nextStatus = 200;
      if (gate) await gate;
      return { ok: status < 400, status, json: async () => ({ termId: `pty-${calls.length}` }) };
    },
  };
  vm.runInNewContext(terminalSource + `
    globalThis.api = { initTerminal, createTab, sendInput, connectStream, toggleDrawer, tabs };
  `, context, { filename: 'terminal.js' });
  context.api.initTerminal();
  return {
    ...context.api, calls, admissions, streams, terminals, observers, nodes, bus, window,
    respondNext(status) { nextStatus = status; },
    setAccess(value, { notify = true, detail = {} } = {}) {
      access = value;
      if (notify) window.dispatchEvent({ type: 'commercial:change', detail });
    },
    holdNextRequest() {
      const gate = deferred();
      nextRequest = gate.promise;
      return gate.resolve;
    },
    async flush() { for (let n = 0; n < 30; n++) await Promise.resolve(); },
  };
}

function dataFrame(text) {
  return { data: JSON.stringify({ type: 'data', data: Buffer.from(text).toString('base64') }) };
}

test('guest access prevents terminal creation even when the account event claims authorization', async () => {
  const h = harness({ authenticated: false });
  h.setAccess(false, { detail: { authenticated: true, authenticationRequired: false } });
  await h.createTab();
  h.toggleDrawer(true);
  await h.flush();
  assert.equal(h.calls.length, 0);
  assert.equal(h.streams.length, 0);
  assert.equal(h.terminals.length, 0);
  assert.equal(h.tabs.length, 0);
  assert.equal(h.nodes.get('#termDrawer').hidden, true);
});

test('input and stream entry points reject stale tabs immediately when access is false', async () => {
  const h = harness();
  await h.createTab();
  const tab = h.tabs[0];
  const callCount = h.calls.length;
  const streamCount = h.streams.length;
  h.setAccess(false, { notify: false });
  h.sendInput(tab, 'must not reach the shell\n');
  h.connectStream(tab);
  await h.flush();
  assert.equal(h.calls.length, callCount);
  assert.equal(h.streams.length, streamCount);
});

for (const event of ['commercial:change', 'authentication_required']) {
  test(`${event} revocation releases local terminal resources without deleting a PTY`, async () => {
    const h = harness();
    h.toggleDrawer(true);
    await h.flush();
    const tab = h.tabs[0];
    const stream = h.streams[0];
    const terminal = tab.term;
    const input = terminal.inputReaders[0];
    assert.ok(input, 'an admitted terminal must own an input reader');
    h.setAccess(false, { notify: false });
    if (event === 'commercial:change') {
      h.window.dispatchEvent({ type: event, detail: { authenticated: true } });
    } else {
      h.bus.emit(event, { reason: 'expired' });
    }
    await h.flush();
    assert.equal(stream.closed, true);
    assert.equal(input.disposed, true);
    assert.equal(terminal.disposed, true);
    assert.ok(h.observers.every(observer => observer.disconnected));
    assert.equal(h.tabs.length, 0);
    assert.equal(tab.paneEl.parentNode, null);
    assert.equal(tab.tabEl.parentNode, null);
    assert.equal(h.nodes.get('#termDrawer').hidden, true);
    assert.equal(h.calls.some(call => call.method === 'DELETE'), false);
  });
}

test('late stream and input callbacks stay inert after logout and a fresh authenticated reopen', async () => {
  const h = harness();
  await h.createTab();
  const oldTab = h.tabs[0];
  const oldStream = h.streams[0];
  const oldOpen = oldStream.onopen;
  const oldMessage = oldStream.onmessage;
  const oldInput = oldTab.term.inputReaders[0].callback;
  oldOpen();
  h.setAccess(false);
  const writesAtLogout = oldTab.term.writes.length;
  const resetsAtLogout = oldTab.term.resets;
  oldOpen();
  oldMessage(dataFrame('private output after logout'));
  oldMessage({ data: JSON.stringify({ type: 'exit', code: 0 }) });
  oldInput('private input after logout\n');
  await h.flush();
  h.setAccess(true);
  assert.equal(h.tabs.length, 0, 'authentication alone must not revive an old terminal');
  h.toggleDrawer(true);
  await h.flush();
  const freshTab = h.tabs[0];
  assert.ok(freshTab && freshTab !== oldTab);
  assert.equal(h.streams.length, 2);
  const writesBeforeOldCallback = h.calls.filter(call => call.path.endsWith('/write')).length;
  oldOpen();
  oldMessage(dataFrame('private output from the previous account'));
  oldMessage({ data: JSON.stringify({ type: 'exit', code: 9 }) });
  oldInput('private input from the previous account\n');
  await h.flush();
  assert.equal(oldTab.term.writes.length, writesAtLogout);
  assert.equal(oldTab.term.resets, resetsAtLogout);
  assert.equal(h.calls.filter(call => call.path.endsWith('/write')).length, writesBeforeOldCallback);
  const freshStream = h.streams[1];
  freshStream.onmessage(dataFrame('fresh output'));
  freshTab.term.inputReaders[0].callback('fresh input\n');
  await h.flush();
  assert.equal(freshTab.term.writes.length, 1);
  assert.equal(Buffer.from(freshTab.term.writes[0]).toString(), 'fresh output');
  const write = h.calls.findLast(call => call.path.endsWith('/write'));
  assert.ok(write);
  assert.equal(Buffer.from(JSON.parse(write.body).dataB64, 'base64').toString(), 'fresh input\n');
  assert.equal(h.calls.some(call => call.method === 'DELETE'), false);
});

test('a create response from an earlier account generation cannot attach after logout and login', async () => {
  const h = harness();
  const release = h.holdNextRequest();
  const creating = h.createTab();
  await h.flush();
  assert.equal(h.calls.length, 1);
  h.setAccess(false);
  h.setAccess(true);
  release();
  await creating;
  await h.flush();
  assert.equal(h.tabs.length, 0);
  assert.equal(h.streams.length, 0);
  assert.equal(h.terminals[0].inputReaders.length, 0);
  assert.equal(h.terminals[0].disposed, true);
  assert.equal(h.calls.some(call => call.method === 'DELETE'), false);
  await h.createTab();
  assert.equal(h.tabs.length, 1);
  assert.equal(h.streams.length, 1);
  assert.equal(h.terminals.at(-1).inputReaders.length, 1);
});

test('an in-flight write cannot drain queued bytes from an earlier account after reauthentication', async () => {
  const h = harness();
  await h.createTab();
  const tab = h.tabs[0];
  const input = tab.term.inputReaders[0].callback;
  const release = h.holdNextRequest();
  input('already dispatched\n');
  await h.flush();
  input('queued before logout\n');
  const writesBeforeLogout = h.calls.filter(call => call.path.endsWith('/write')).length;
  assert.equal(writesBeforeLogout, 1);
  h.setAccess(false);
  h.setAccess(true);
  input('late old reader after login\n');
  release();
  await h.flush();
  assert.equal(h.calls.filter(call => call.path.endsWith('/write')).length, writesBeforeLogout);
  assert.equal(h.calls.some(call => call.method === 'DELETE'), false);
});

test('a current terminal 401 revokes admission and releases local readers without deleting the PTY', async () => {
  const h = harness();
  await h.createTab();
  const tab = h.tabs[0];
  const stream = h.streams[0];
  const reader = tab.term.inputReaders[0];
  h.respondNext(401);
  reader.callback('denied write\n');
  await h.flush();
  assert.deepEqual(h.admissions, [null]);
  assert.equal(h.tabs.length, 0);
  assert.equal(stream.closed, true);
  assert.equal(reader.disposed, true);
  assert.equal(tab.term.disposed, true);
  const dispatched = h.calls.length;
  await h.createTab();
  assert.equal(h.calls.length, dispatched);
  assert.equal(h.calls.some(call => call.method === 'DELETE'), false);
});

test('a late 401 from the previous account cannot revoke a newly authenticated terminal', async () => {
  const h = harness();
  await h.createTab();
  const oldTab = h.tabs[0];
  const release = h.holdNextRequest();
  h.respondNext(401);
  oldTab.term.inputReaders[0].callback('old account write\n');
  await h.flush();
  h.setAccess(false);
  h.setAccess(true);
  await h.createTab();
  const freshTab = h.tabs[0];
  const freshStream = h.streams.at(-1);
  release();
  await h.flush();
  assert.equal(h.admissions.length, 0, 'a stale response must not change current account admission');
  assert.equal(h.tabs.length, 1);
  assert.equal(h.tabs[0], freshTab);
  assert.notEqual(freshStream.closed, true);
  freshTab.term.inputReaders[0].callback('new account write\n');
  await h.flush();
  const write = h.calls.findLast(call => call.path.endsWith('/write'));
  assert.equal(Buffer.from(JSON.parse(write.body).dataB64, 'base64').toString(), 'new account write\n');
  assert.equal(h.calls.some(call => call.method === 'DELETE'), false);
});
