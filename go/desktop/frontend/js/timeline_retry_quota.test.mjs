import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { webcrypto } from 'node:crypto';
import vm from 'node:vm';

const source = readFileSync(new URL('./timeline.js', import.meta.url), 'utf8');
const retrySource = source.slice(source.indexOf('async function retryTool('), source.indexOf('\nfunction onToolUpdate('));
function harness({ busy = false, authorize, deliver, consume } = {}) {
  const calls = { auth: [], prompt: [], steer: [], consume: [] };
  const state = { sessionId: 's1', busy, runBudgetControl: { sessionId: 's1' } };
  const context = { state, crypto: webcrypto, console: { error() {} }, setTimeout() {}, bus: { emit() {} },
    authorizePromptDelivery: async (...args) => { calls.auth.push(args); return authorize ? authorize(...args) : { requestID: args[1] }; },
    consumePromptDelivery: async grant => { calls.consume.push(grant); if (consume) return consume(grant); },
    daemon: {
      prompt: async (...args) => { calls.prompt.push(args); return deliver ? deliver() : {}; },
      steer: async (...args) => { calls.steer.push(args); return deliver ? deliver() : {}; },
    },
    el: () => ({ addEventListener(type, fn) { this.click = fn; } }),
  };
  vm.runInNewContext(retrySource + '\nglobalThis.api = { retryTool, addRetryBtn };', context);
  return { ...context.api, calls, state };
}

for (const busy of [false, true]) {
  test(`${busy ? 'busy steer' : 'idle prompt'} tool retry needs a fresh user-send permit`, async () => {
    const h = harness({ busy });
    assert.equal(await h.retryTool('test', 'stable-user-retry'), true);
    assert.deepEqual(h.calls.auth, [['s1', 'stable-user-retry']]);
    const delivery = busy ? h.calls.steer[0] : h.calls.prompt[0];
    assert.equal(delivery[busy ? 3 : 4].requestID, 'stable-user-retry');
    assert.equal(h.calls.consume.length, 1);
    assert.equal(h.state.busy, busy);
  });
  test(`denied quota starts no ${busy ? 'steer' : 'prompt'}`, async () => {
    const h = harness({ busy, authorize: () => { throw new Error('quota exhausted'); } });
    assert.equal(await h.retryTool('test', 'stable-user-retry'), false);
    assert.deepEqual(h.calls.prompt, []);
    assert.deepEqual(h.calls.steer, []);
    assert.deepEqual(h.calls.consume, []);
    assert.equal(h.state.busy, busy);
  });
}

test('same retry button preserves an uncertain send identity and starts a new identity after acceptance', async () => {
  let first = true;
  const h = harness({ deliver: () => { if (first) { first = false; throw new Error('receipt lost'); } return {}; } });
  const tool = { name: 'test', row: { appendChild() {} } };
  h.addRetryBtn(tool);
  await tool.retryBtn.click();
  await tool.retryBtn.click();
  await tool.retryBtn.click();
  assert.equal(h.calls.auth[0][1], h.calls.auth[1][1]);
  assert.notEqual(h.calls.auth[1][1], h.calls.auth[2][1]);
});

test('accepted retry stays accepted when only its success audit receipt fails', async () => {
  const h = harness({ consume: () => { throw new Error('audit offline'); } });
  assert.equal(await h.retryTool('test', 'stable-user-retry'), true);
  assert.equal(h.calls.prompt.length, 1);
});

test('a session change while authorization is pending cannot deliver into the new session', async () => {
  let release;
  const authorization = new Promise(resolve => { release = resolve; });
  const h = harness({ authorize: () => authorization });
  const attempt = h.retryTool('test', 'stable-user-retry');
  h.state.sessionId = 's2';
  release({ requestID: 'stable-user-retry' });
  assert.equal(await attempt, false);
  assert.deepEqual(h.calls.prompt, []);
  assert.deepEqual(h.calls.consume, []);
});
