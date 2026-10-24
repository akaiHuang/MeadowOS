import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('./core.js', import.meta.url), 'utf8')
  .replace("import { runLimitOptions } from './run_limits.js';", '')
  .replace("import { VM_LAB_BUILD } from './vm_lab_build.js';", 'const VM_LAB_BUILD = false;')
  .replace(/^export /gm, '');

test('harness settings encode identity, persist only mode, and observe registered SSE', async () => {
  const calls = [];
  const subscriptions = new Map();
  const events = [];
  const context = {
    console, setTimeout, clearTimeout,
    document: { documentElement: { dataset: {} } },
    fetch: async (path, options) => {
      if (path === '/agent/client/capabilities') return { ok: true, status: 200,
        json: async () => ({ protected: false, authenticationRequired: false }) };
      calls.push({ path, ...options });
      return { ok: true, status: 200, json: async () => ({ sessionId: 'selected' }) };
    },
    EventSource: class {
      constructor() {}
      addEventListener(type, handler) { subscriptions.set(type, handler); }
      close() {}
    },
  };
  vm.runInNewContext(source +
    '\nglobalThis.api = daemon; globalThis.stateRef = state; globalThis.busRef = bus; globalThis.connect = connectEvents;', context);
  await context.api.harnessControls('chat/one');
  await context.api.setHarnessControls('chat/one', { mode: 'raven-code' });
  assert.equal(calls[0].method, 'GET');
  assert.equal(calls[0].path, '/session/chat%2Fone/harness-controls');
  assert.equal(calls[1].method, 'POST');
  assert.equal(calls[1].path, '/session/chat%2Fone/harness-controls');
  assert.deepEqual(JSON.parse(calls[1].body), { mode: 'raven-code' });
  assert.ok(calls.every((call) => !call.path.endsWith('/prompt')));
  context.stateRef.sessionId = 'selected';
  context.busRef.on('harness_controls_changed', (data) => events.push(data));
  context.connect('selected');
  assert.ok(subscriptions.has('harness_controls_changed'));
  subscriptions.get('harness_controls_changed')({
    type: 'harness_controls_changed',
    data: JSON.stringify({ type: 'harness_controls_changed',
      data: { sessionId: 'selected', selectedMode: 'raven-code' } }),
    lastEventId: '11',
  });
  assert.equal(events.length, 1);
  assert.equal(events[0].selectedMode, 'raven-code');
});
