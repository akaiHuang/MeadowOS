import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const REQUEST_ID_HEADER = 'X-Meadow-Prompt-Request-ID';
const LOCAL_CANCELLED = {
  stopReason: 'cancelled',
  cancelled: true,
  local: true,
  dispatched: true,
};
const coreSource = readFileSync(new URL('./core.js', import.meta.url), 'utf8');

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}

function response(data = { stopReason: 'end_turn' }, status = 200) {
  return { ok: status >= 200 && status < 300, status, json: async () => data };
}

// No browser, native bridge, network, real crypto, or wall-clock waits are used.
// A fresh VM gives each case independent private delivery/fingerprint maps.
function loadCore({ digest, promptFetch, cancelFetch, browserBind, budgetOptions, admission, queueFetch, steerFetch } = {}) {
  const calls = [];
  const events = [];
  const hashCalls = [];
  const uuids = [];
  const ownerCalls = [];
  const budgetCalls = [];
  const timers = new Map();
  const fingerprints = new Map();
  let nextTimer = 0;
  let now = 1000;
  const context = {
    AbortController,
    TextEncoder,
    console,
    document: { documentElement: { dataset: {} } },
    runLimitOptions(effort) {
      budgetCalls.push(effort);
      return budgetOptions
        ? budgetOptions(effort)
        : { runTimeoutMs: 600000, runTokenLimit: 500000 };
    },
    testBrowserModule: {
      async bindEmbeddedBrowserOwner(sid) {
        ownerCalls.push(sid);
        if (browserBind) await browserBind(sid);
      },
    },
    crypto: {
      randomUUID() {
        const uuid = `00000000-0000-4000-8000-${String(uuids.length + 1).padStart(12, '0')}`;
        uuids.push(uuid);
        events.push('uuid');
        return uuid;
      },
      subtle: {
        digest(algorithm, payload) {
          assert.equal(algorithm, 'SHA-256');
          events.push('digest');
          hashCalls.push(new TextDecoder().decode(payload));
          if (digest) return digest(payload);
          const key = Buffer.from(payload).toString('hex');
          if (!fingerprints.has(key)) {
            fingerprints.set(key, Uint8Array.of(fingerprints.size + 1).buffer);
          }
          return Promise.resolve(fingerprints.get(key));
        },
      },
    },
    Date: class extends Date {
      constructor(...args) {
        super(...(args.length ? args : [now]));
      }
      static now() { return now; }
    },
    setTimeout(callback, delay) {
      const id = ++nextTimer;
      timers.set(id, { callback, delay });
      return id;
    },
    clearTimeout(id) { timers.delete(id); },
    fetch(path, options) {
      if (path === '/agent/client/capabilities') return Promise.resolve(response({
        protected: false, authenticationRequired: false,
      }));
      const call = { path, ...options };
      calls.push(call);
      assert.equal(options.method, 'POST');
      if (/^\/session\/[^/]+\/prompt$/.test(path)) {
        return promptFetch ? promptFetch(call) : Promise.resolve(response());
      }
      if (/^\/session\/[^/]+\/cancel$/.test(path)) {
        return cancelFetch
          ? cancelFetch(call)
          : Promise.resolve(response({ ok: true, cancelling: true }));
      }
      if (/^\/session\/[^/]+\/queue$/.test(path))
        return queueFetch ? queueFetch(call) : Promise.resolve(response({ item: { id: 'queued' }, items: [] }));
      if (/^\/session\/[^/]+\/steer$/.test(path))
        return steerFetch ? steerFetch(call) : Promise.resolve(response({ disposition: 'accepted' }));
      throw new Error(`unexpected test request: ${path}`);
    },
  };
  const importLine = "import { runLimitOptions } from './run_limits.js';";
  assert.ok(coreSource.includes(importLine));
  const browserImport = "import('./browserDrawer.js?v=desktop-browser-drawer-20260904c')";
  assert.equal(coreSource.split(browserImport).length - 1, 1,
    'expected exactly one known Browser owner module import');
  // Mock only the known module expression. Both production awaits and the
  // asynchronous Browser ownership gate remain in the evaluated source.
  const source = coreSource
    .replace(importLine, '')
    .replace("import { VM_LAB_BUILD } from './vm_lab_build.js';", 'const VM_LAB_BUILD = false;')
    .replace(browserImport, 'Promise.resolve(testBrowserModule)')
    .replace(/^export /gm, '');
  vm.runInNewContext(`${source}\nglobalThis.testDaemon = daemon; globalThis.testState = state; globalThis.testBus = bus; globalThis.registerAdmission = registerPromptAdmissionCheck;`, context, {
    filename: 'core.js',
  });
  if (admission) context.registerAdmission(admission);
  return {
    daemon: context.testDaemon,
    calls,
    events,
    hashCalls,
    uuids,
    ownerCalls,
    budgetCalls,
    state: context.testState,
    bus: context.testBus,
    timers,
    prompts: () => calls.filter((call) => call.path.endsWith('/prompt')),
    cancels: () => calls.filter((call) => call.path.endsWith('/cancel')),
    queues: () => calls.filter((call) => call.path.endsWith('/queue')),
    runTimer() {
      assert.ok(timers.size, 'expected a pending retry timer');
      const [id, { callback, delay }] = timers.entries().next().value;
      timers.delete(id);
      now += delay;
      callback();
    },
  };
}

test('a late mode change rejects an Evo prompt allowance before transport', async () => {
  const browser = deferred(); let mode = 'evo';
  const h = loadCore({ browserBind: () => browser.promise, admission: async () => ({ effectiveMode: mode }) });
  const sending = h.daemon.prompt('one', 'timer', [], { runTimeoutMs: 3600000, runTokenLimit: 3000000 });
  mode = 'codex-style'; browser.resolve();
  await assert.rejects(sending, /Evo 額度僅能用於 Evo/);
  assert.equal(h.prompts().length, 0);
});

test('Evo queue and prompt budget require confirmed mode at final admission', async () => {
  let mode = 'codex-style';
  const h = loadCore({ admission: async () => ({ effectiveMode: mode }) });
  await assert.rejects(h.daemon.queueAdd('one', 'timer', [], { runTokenLimit: 2000000 }), /Evo 額度僅能用於 Evo/);
  assert.equal(h.queues().length, 0);
  mode = 'evo';
  await h.daemon.queueAdd('one', 'timer', [], { runTokenLimit: 2000000 });
  assert.equal(JSON.parse(h.queues()[0].body).options.runTokenLimit, 2000000);
  await h.daemon.prompt('one', 'timer', [], { runTokenLimit: 3000000 });
  assert.equal(JSON.parse(h.prompts()[0].body).options.runTokenLimit, 3000000);
});

test('a server grant keeps one operation identity through prompt, queue and manual guidance delivery', async () => {
  const grant = { schema: 'meadow.desktop-membership.v2', requestID: 'submission-000000000000001',
    authorizationID: 'authorization-0000000001', conversationID: 'bound-session' };
  const h = loadCore();
  await h.daemon.prompt('bound-session', 'first', [], {}, grant);
  await h.daemon.queueAdd('bound-session', 'next', [], {}, grant);
  await h.daemon.steer('bound-session', 'manual retry', [], grant);
  const work = h.calls.filter((call) => /\/(prompt|queue|steer)$/.test(call.path));
  assert.equal(work.length, 3);
  for (const call of work) {
    assert.equal(call.headers[REQUEST_ID_HEADER], grant.requestID);
    assert.equal(call.headers['X-Meadow-Membership-Request-ID'], grant.requestID);
    assert.equal(JSON.parse(call.body).membership_authorization.authorization_id, grant.authorizationID);
  }
  assert.equal(h.uuids.length, 0, 'grant identity must survive instead of minting a new delivery identity');
});

async function until(predicate, message) {
  for (let turn = 0; turn < 40; turn += 1) {
    if (predicate()) return;
    await Promise.resolve();
  }
  assert.ok(predicate(), message);
}

async function resultOf(promise) {
  let settled = false;
  let value;
  let failure;
  promise.then(
    (result) => { value = result; settled = true; },
    (error) => { failure = error; settled = true; },
  );
  await until(() => settled, 'delivery should settle without real time passing');
  if (failure) throw failure;
  // Normalize the VM object's prototype for node:assert/strict comparison.
  return JSON.parse(JSON.stringify(value));
}

async function assertPending(promise) {
  let settled = false;
  promise.then(() => { settled = true; }, () => { settled = true; });
  for (let turn = 0; turn < 40; turn += 1) await Promise.resolve();
  assert.equal(settled, false, 'unacknowledged dispatched delivery stays owned');
}

function requestID(call) {
  return call.headers[REQUEST_ID_HEADER];
}

test('Stop while hashing has an identity and never sends the prompt', async () => {
  const hash = deferred();
  const h = loadCore({ digest: () => hash.promise });
  const pending = h.daemon.prompt('hashing', 'hello');

  await until(() => h.hashCalls.length === 1, 'hashing phase should start');
  assert.deepEqual(h.events, ['uuid', 'digest']);
  assert.equal(h.prompts().length, 0);
  await h.daemon.cancel('hashing');
  assert.equal(h.cancels().length, 1);
  assert.equal(h.cancels()[0].path, '/session/hashing/cancel');
  assert.deepEqual(JSON.parse(h.cancels()[0].body), {
    requestId: `prompt-${h.uuids[0]}`,
  });

  assert.deepEqual(await resultOf(pending), {
    ...LOCAL_CANCELLED,
    dispatched: false,
  });
  hash.resolve(Uint8Array.of(1).buffer);
  for (let turn = 0; turn < 10; turn += 1) await Promise.resolve();
  assert.equal(h.prompts().length, 0);
  assert.equal(h.timers.size, 0);
});

test('Stop aborts an active HTTP prompt and suppresses transport retry', async () => {
  const http = deferred();
  const h = loadCore({
    promptFetch: ({ signal }) => {
      signal.addEventListener('abort', () => {
        const error = new Error('aborted by test signal');
        error.name = 'AbortError';
        http.reject(error);
      }, { once: true });
      return http.promise;
    },
  });
  const pending = h.daemon.prompt('active', 'hello');
  await until(() => h.prompts().length === 1, 'prompt should start');
  const call = h.prompts()[0];
  assert.equal(call.signal.aborted, false);

  await h.daemon.cancel('active');
  assert.equal(call.signal.aborted, true);
  assert.deepEqual(JSON.parse(h.cancels()[0].body), {
    requestId: requestID(call),
  });
  assert.deepEqual(await resultOf(pending), LOCAL_CANCELLED);
  assert.equal(h.prompts().length, 1);
  assert.equal(h.timers.size, 0);
});

test('Stop during retry backoff prevents the next attempt', async () => {
  const h = loadCore({
    promptFetch: () => Promise.reject(new TypeError('connection lost')),
  });
  const pending = h.daemon.prompt('backoff', 'hello');
  await until(() => h.timers.size === 1, 'retry should await its timer');
  const firstID = requestID(h.prompts()[0]);

  await h.daemon.cancel('backoff');
  // Implementations may either interrupt or finish the bounded retry wait.
  if (h.timers.size) h.runTimer();
  assert.deepEqual(await resultOf(pending), LOCAL_CANCELLED);
  assert.deepEqual(JSON.parse(h.cancels()[0].body), { requestId: firstID });
  assert.equal(h.prompts().length, 1);
  assert.equal(h.timers.size, 0);
});

test('ordinary transport retry preserves the request ID and exact body', async () => {
  let attempts = 0;
  const h = loadCore({
    promptFetch: () => ++attempts === 1
      ? Promise.reject(new TypeError('connection lost'))
      : Promise.resolve(response()),
  });
  const pending = h.daemon.prompt('retry', 'hello', [], { thinking: true });
  await until(() => h.timers.size === 1, 'retry should await its timer');
  assert.equal(h.prompts().length, 1);
  h.runTimer();

  assert.deepEqual(await resultOf(pending), { stopReason: 'end_turn' });
  const [first, second] = h.prompts();
  assert.equal(h.prompts().length, 2);
  assert.equal(requestID(first), `prompt-${h.uuids[0]}`);
  assert.equal(requestID(second), requestID(first));
  assert.equal(second.body, first.body);
  assert.equal(h.cancels().length, 0);
});

test('same-body resubmit recovers the identity after exhausted transport retries', async () => {
  let attempts = 0;
  const h = loadCore({
    promptFetch: () => ++attempts <= 2
      ? Promise.reject(new TypeError('connection lost'))
      : Promise.resolve(response()),
  });
  const pending = h.daemon.prompt('recover', 'same body');
  const rejected = assert.rejects(pending, /connection lost/);
  await until(() => h.timers.size === 1, 'retry should await its timer');
  h.runTimer();
  await rejected;
  assert.equal(h.prompts().length, 2);
  assert.equal(requestID(h.prompts()[1]), requestID(h.prompts()[0]));

  assert.deepEqual(
    await resultOf(h.daemon.prompt('recover', 'same body')),
    { stopReason: 'end_turn' },
  );
  assert.equal(h.prompts().length, 3);
  assert.equal(requestID(h.prompts()[2]), requestID(h.prompts()[0]));
  assert.equal(h.prompts()[2].body, h.prompts()[0].body);
});

test('a late HTTP success after Stop stays cancelled and a new same-body prompt works', async () => {
  const http = deferred();
  let attempts = 0;
  const h = loadCore({
    // Deliberately ignore abort to model a receipt racing with Stop.
    promptFetch: () => ++attempts === 1
      ? http.promise
      : Promise.resolve(response()),
  });
  const pending = h.daemon.prompt('again', 'same body');
  await until(() => h.prompts().length === 1, 'prompt should start');
  await h.daemon.cancel('again');
  http.resolve(response());
  assert.deepEqual(await resultOf(pending), LOCAL_CANCELLED);

  assert.deepEqual(
    await resultOf(h.daemon.prompt('again', 'same body')),
    { stopReason: 'end_turn' },
  );
  assert.equal(h.prompts().length, 2);
  assert.notEqual(requestID(h.prompts()[1]), requestID(h.prompts()[0]));
  assert.equal(h.prompts()[1].signal.aborted, false);
});

test('Stop while parsing a response does not accept the late completion', async () => {
  const json = deferred();
  let parsing = false;
  const h = loadCore({
    promptFetch: () => Promise.resolve({
      ok: true,
      status: 200,
      json: () => { parsing = true; return json.promise; },
    }),
  });
  const pending = h.daemon.prompt('parsing', 'hello');
  await until(() => parsing, 'response parsing should start');
  await h.daemon.cancel('parsing');
  json.resolve({ stopReason: 'end_turn' });

  assert.deepEqual(await resultOf(pending), LOCAL_CANCELLED);
  assert.equal(h.prompts().length, 1);
  assert.equal(h.timers.size, 0);
});

test('old cancelled delivery cleanup preserves a newer delivery in the same session', async () => {
  const first = deferred();
  const second = deferred();
  let attempts = 0;
  const h = loadCore({
    promptFetch: () => ++attempts === 1 ? first.promise : second.promise,
  });
  const oldPending = h.daemon.prompt('same-session', 'same body');
  await until(() => h.prompts().length === 1, 'old prompt should start');
  await h.daemon.cancel('same-session');
  const newPending = h.daemon.prompt('same-session', 'same body');
  await until(() => h.prompts().length === 2, 'new prompt should start');
  const newest = h.prompts()[1];
  assert.notEqual(requestID(newest), requestID(h.prompts()[0]));
  assert.equal(newest.signal.aborted, false);

  first.resolve(response());
  assert.deepEqual(await resultOf(oldPending), LOCAL_CANCELLED);
  await h.daemon.cancel('same-session');
  assert.equal(newest.signal.aborted, true);
  assert.deepEqual(JSON.parse(h.cancels().at(-1).body), {
    requestId: requestID(newest),
  });
  second.resolve(response());
  assert.deepEqual(await resultOf(newPending), LOCAL_CANCELLED);
});

test('Stop only aborts the requested session', async () => {
  const first = deferred();
  const second = deferred();
  const h = loadCore({
    promptFetch: ({ path }) => path.includes('/one/')
      ? first.promise
      : second.promise,
  });
  const one = h.daemon.prompt('one', 'same body');
  const two = h.daemon.prompt('two', 'same body');
  await until(() => h.prompts().length === 2, 'both sessions should start');
  await h.daemon.cancel('one');

  const firstCall = h.prompts().find(({ path }) => path === '/session/one/prompt');
  const secondCall = h.prompts().find(({ path }) => path === '/session/two/prompt');
  assert.equal(firstCall.signal.aborted, true);
  assert.equal(secondCall.signal.aborted, false);
  assert.notEqual(requestID(firstCall), requestID(secondCall));
  first.resolve(response());
  second.resolve(response());
  assert.deepEqual(await resultOf(one), LOCAL_CANCELLED);
  assert.deepEqual(await resultOf(two), { stopReason: 'end_turn' });
  assert.equal(h.cancels().length, 1);
});

test('failed Stop keeps dispatched delivery pending and a repeated Stop uses its identity', async () => {
  let cancelAttempts = 0;
  const h = loadCore({
    promptFetch: () => Promise.reject(new TypeError('connection lost')),
    cancelFetch: () => ++cancelAttempts === 1
      ? Promise.reject(new TypeError('cancel unavailable'))
      : Promise.resolve(response({ ok: true, cancelling: true })),
  });
  const pending = h.daemon.prompt('offline', 'hello');
  await until(() => h.timers.size === 1, 'retry should await its timer');
  await assert.rejects(h.daemon.cancel('offline'), /cancel unavailable/);
  if (h.timers.size) h.runTimer();

  await assertPending(pending);
  assert.equal(h.prompts().length, 1);
  assert.equal(h.prompts()[0].signal.aborted, false);
  await h.daemon.cancel('offline');

  assert.deepEqual(await resultOf(pending), LOCAL_CANCELLED);
  assert.equal(h.prompts()[0].signal.aborted, true);
  assert.equal(h.cancels().length, 2);
  for (const cancel of h.cancels()) {
    assert.deepEqual(JSON.parse(cancel.body), {
      requestId: requestID(h.prompts()[0]),
    });
  }
  assert.equal(h.prompts().length, 1);
  assert.equal(h.timers.size, 0);
});

test('HTTP 200 without a positive cancellation acknowledgement keeps ownership', async () => {
  const http = deferred();
  let cancelAttempts = 0;
  const h = loadCore({
    promptFetch: () => http.promise,
    cancelFetch: () => Promise.resolve(response({
      ok: true,
      cancelling: ++cancelAttempts > 1,
    })),
  });
  const pending = h.daemon.prompt('unacknowledged', 'hello');
  await until(() => h.prompts().length === 1, 'prompt should start');
  await assert.rejects(h.daemon.cancel('unacknowledged'), {
    code: 'PROMPT_CANCEL_NOT_ACKNOWLEDGED',
  });
  await assertPending(pending);
  assert.equal(h.prompts()[0].signal.aborted, false);

  await h.daemon.cancel('unacknowledged');
  assert.deepEqual(await resultOf(pending), LOCAL_CANCELLED);
  assert.equal(h.prompts()[0].signal.aborted, true);
  assert.equal(h.cancels()[1].body, h.cancels()[0].body);
  http.resolve(response());
});

test('a dispatched prompt awaits Stop acknowledgement even if its HTTP request fails', async () => {
  const http = deferred();
  const cancel = deferred();
  const h = loadCore({
    promptFetch: () => http.promise,
    cancelFetch: () => cancel.promise,
  });
  const pending = h.daemon.prompt('awaiting-ack', 'hello');
  await until(() => h.prompts().length === 1, 'prompt should start');
  const stopping = h.daemon.cancel('awaiting-ack');
  await assertPending(pending);
  assert.equal(h.prompts()[0].signal.aborted, false);
  http.reject(new TypeError('connection lost'));
  await assertPending(pending);
  assert.equal(h.prompts().length, 1);
  assert.equal(h.timers.size, 0);

  cancel.resolve(response({ ok: true, cancelling: true }));
  await stopping;
  assert.deepEqual(await resultOf(pending), LOCAL_CANCELLED);
  assert.equal(h.prompts()[0].signal.aborted, true);
});

test('an actual terminal receipt can finish a prompt while Stop remains unacknowledged', async () => {
  const http = deferred();
  const cancel = deferred();
  const h = loadCore({
    promptFetch: () => http.promise,
    cancelFetch: () => cancel.promise,
  });
  const pending = h.daemon.prompt('terminal-race', 'hello');
  await until(() => h.prompts().length === 1, 'prompt should start');
  const stopping = h.daemon.cancel('terminal-race');
  const rejected = assert.rejects(stopping, /cancel unavailable/);
  http.resolve(response({ stopReason: 'end_turn' }));

  assert.deepEqual(await resultOf(pending), { stopReason: 'end_turn' });
  assert.equal(h.prompts()[0].signal.aborted, false);
  cancel.reject(new TypeError('cancel unavailable'));
  await rejected;
  assert.equal(h.prompts().length, 1);
});

test('overlapping direct prompts in one session reject without replacing the owner', async () => {
  const hash = deferred();
  const h = loadCore({ digest: () => hash.promise });
  const pending = h.daemon.prompt('overlap', 'first');
  await assert.rejects(h.daemon.prompt('overlap', 'second'), {
    code: 'PROMPT_DELIVERY_ACTIVE',
  });
  assert.equal(h.hashCalls.length, 1);
  await h.daemon.cancel('overlap');
  assert.deepEqual(JSON.parse(h.cancels()[0].body), {
    requestId: `prompt-${h.uuids[0]}`,
  });
  assert.deepEqual(await resultOf(pending), {
    ...LOCAL_CANCELLED,
    dispatched: false,
  });
  hash.resolve(Uint8Array.of(1).buffer);
  assert.equal(h.prompts().length, 0);
});

test('Stop during resubmit hashing targets the uncertain prior delivery and waits for acknowledgement', async () => {
  const secondHash = deferred();
  const cancel = deferred();
  let hashes = 0;
  const h = loadCore({
    digest: () => ++hashes === 1
      ? Promise.resolve(Uint8Array.of(1).buffer)
      : secondHash.promise,
    promptFetch: () => Promise.reject(new TypeError('connection lost')),
    cancelFetch: () => cancel.promise,
  });
  const failed = h.daemon.prompt('uncertain', 'same body');
  const rejected = assert.rejects(failed, /connection lost/);
  await until(() => h.timers.size === 1, 'retry should await its timer');
  h.runTimer();
  await rejected;
  const priorID = requestID(h.prompts()[0]);
  const resubmit = h.daemon.prompt('uncertain', 'same body');
  await until(() => h.hashCalls.length === 2, 'resubmit hashing phase should start');
  assert.equal(h.hashCalls.length, 2);
  const stopping = h.daemon.cancel('uncertain');
  assert.deepEqual(JSON.parse(h.cancels()[0].body), { requestId: priorID });
  await assertPending(resubmit);
  assert.equal(h.prompts().length, 2);

  cancel.resolve(response({ ok: true, cancelling: true }));
  await stopping;
  assert.deepEqual(await resultOf(resubmit), LOCAL_CANCELLED);
  secondHash.resolve(Uint8Array.of(1).buffer);
  for (let turn = 0; turn < 10; turn += 1) await Promise.resolve();
  assert.equal(h.prompts().length, 2);
});

test('run budget uses confirmed SID effort and snapshots before Browser ownership', async () => {
  const owner = deferred();
  const h = loadCore({
    admission: async () => ({ effectiveMode: 'evo' }),
    browserBind: () => owner.promise,
    budgetOptions: (effort) => ({
      runTimeoutMs: 600000,
      runTokenLimit: effort === 'high' ? 2000000 : 500000,
    }),
  });
  h.state.runBudgetControl = { sessionId: 'confirmed', effort: 'high' };
  const pending = h.daemon.prompt('confirmed', 'first');
  assert.deepEqual(h.budgetCalls, ['high']);
  await until(() => h.ownerCalls.length === 1, 'Browser ownership gate should start');
  assert.equal(h.prompts().length, 0, 'prompt must await Browser ownership');
  h.state.runBudgetControl = { sessionId: 'confirmed', effort: 'minimal' };
  owner.resolve();
  assert.deepEqual(await resultOf(pending), { stopReason: 'end_turn' });
  assert.equal(JSON.parse(h.prompts()[0].body).options.runTokenLimit, 2000000,
    'mid-admission preference changes must not replace the submitted budget');

  h.state.runBudgetControl = { sessionId: 'confirmed', effort: 'high' };
  assert.deepEqual(await resultOf(h.daemon.prompt('different', 'second')),
    { stopReason: 'end_turn' });
  assert.deepEqual(h.budgetCalls, ['high', undefined]);
  assert.equal(JSON.parse(h.prompts()[1].body).options.runTokenLimit, 500000,
    'another SID must not inherit the confirmed session effort');
});

test('Stop owns an identity during held Browser admission and prevents later dispatch', async () => {
  const owner = deferred();
  const h = loadCore({ browserBind: () => owner.promise });
  const pending = h.daemon.prompt('admission', 'hello');
  assert.equal(h.uuids.length, 1, 'delivery identity must exist before Browser admission awaits');
  await until(() => h.ownerCalls.length === 1, 'Browser ownership gate should start');
  assert.equal(h.hashCalls.length, 0, 'hashing must wait for Browser admission');
  assert.equal(h.prompts().length, 0);

  await h.daemon.cancel('admission');
  assert.deepEqual(JSON.parse(h.cancels()[0].body), {
    requestId: `prompt-${h.uuids[0]}`,
  });
  assert.deepEqual(await resultOf(pending), {
    ...LOCAL_CANCELLED,
    dispatched: false,
  });
  owner.resolve();
  for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  assert.equal(h.hashCalls.length, 0, 'released admission must respect the completed Stop');
  assert.equal(h.prompts().length, 0, 'released admission must never dispatch the cancelled prompt');
  assert.equal(h.timers.size, 0);
});

test('direct idle retry prompt waits preference persistence and Stop prevents late dispatch', async () => {
  const selection = deferred();
  let admissions = 0;
  const h = loadCore({ admission: async () => { admissions++; await selection.promise; } });
  const pending = h.daemon.prompt('admission', 'retry tool');
  await until(() => admissions === 1, 'preference gate must start after Browser/hash');
  assert.equal(h.prompts().length, 0);
  await h.daemon.cancel('admission');
  assert.deepEqual(await resultOf(pending), { ...LOCAL_CANCELLED, dispatched: false });
  selection.resolve();
  for (let i = 0; i < 20; i++) await Promise.resolve();
  assert.equal(h.prompts().length, 0);
});

test('queue admission waits preference save, keeps frozen allowance and respects Stop', async () => {
  const selection = deferred();
  let admissions = 0;
  const h = loadCore({ admission: async () => { admissions++; await selection.promise; } });
  const pending = h.daemon.queueAdd('queued', 'next', [], { runTokenLimit: 750000, runTimeoutMs: 20000 });
  await until(() => admissions === 1, 'queue must reach preference gate');
  assert.equal(h.queues().length, 0);
  h.state.runBudgetControl = { sessionId: 'queued', effort: 'minimal' };
  selection.resolve();
  await pending;
  assert.equal(h.queues().length, 1);
  assert.equal(JSON.parse(h.queues()[0].body).options.runTokenLimit, 750000);
  assert.equal(JSON.parse(h.queues()[0].body).options.runTimeoutMs, 20000);

  const stopped = deferred();
  const stoppedHarness = loadCore({ admission: () => stopped.promise });
  const stoppedQueue = stoppedHarness.daemon.queueAdd('stopped', 'next');
  await stoppedHarness.daemon.cancel('stopped');
  stopped.resolve();
  await assert.rejects(stoppedQueue, /已停止/);
  assert.equal(stoppedHarness.queues().length, 0);
});

test('failed selection admission refuses direct prompt and queue without transport retries', async () => {
  const h = loadCore({ admission: async () => { throw new Error('selection not confirmed'); } });
  await assert.rejects(h.daemon.prompt('held', 'hello'), /selection not confirmed/);
  await assert.rejects(h.daemon.queueAdd('held', 'next'), /selection not confirmed/);
  assert.equal(h.prompts().length, 0);
  assert.equal(h.queues().length, 0);
  assert.equal(h.timers.size, 0);
});

test('A-to-B-to-A thread switch invalidates held direct admission despite identical final SID', async () => {
  const selection = deferred();
  let admissions = 0;
  const h = loadCore({ admission: async () => { admissions++; await selection.promise; } });
  h.state.sessionId = 'same';
  const prompt = h.daemon.prompt('same', 'idle retry');
  const queue = h.daemon.queueAdd('same', 'queued');
  await until(() => admissions === 2, 'both admissions must be pending');
  h.state.sessionId = 'other';
  h.bus.emit('thread:switched');
  h.state.sessionId = 'same';
  h.bus.emit('thread:switched');
  selection.resolve();
  await assert.rejects(prompt, /對話已切換/);
  await assert.rejects(queue, /對話已切換/);
  assert.equal(h.prompts().length, 0);
  assert.equal(h.queues().length, 0);
});
