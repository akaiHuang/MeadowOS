import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import * as authContract from './commercial_contract.js';
import * as membershipContract from './membership_contract.js';
import { createPromptGrantWarmup } from './prompt_grant.js';

const coreSource = readFileSync(new URL('./core.js', import.meta.url), 'utf8')
  .replace("import { runLimitOptions } from './run_limits.js';", '')
  .replace("import { VM_LAB_BUILD } from './vm_lab_build.js';", 'const VM_LAB_BUILD = false;')
  .replace(/^export /gm, '');
const commercialSource = readFileSync(new URL('./commercial.js', import.meta.url), 'utf8')
  .replace(/^import[\s\S]*?from '[^']+';\n/gm, '')
  .replace(/^export /gm, '');

function response(value, status = 200) {
  return { ok: status < 400, status, json: async () => value };
}

function coreHarness(capability, operationStatus = 200) {
  const calls = [];
  const streams = [];
  const context = {
    console, setTimeout, clearTimeout,
    document: { documentElement: { dataset: { frontendMode: 'development' } } },
    fetch: async (path, options) => {
      calls.push({ path, ...options });
      if (path === '/agent/client/capabilities') {
        if (capability instanceof Error) throw capability;
        return response(capability);
      }
      return response({ ok: true }, operationStatus);
    },
    EventSource: class {
      constructor(path) { this.path = path; streams.push(this); }
      addEventListener(type, handler) {
        this.handlers ||= new Map();
        this.handlers.set(type, handler);
      }
      close() { this.closed = true; }
    },
  };
  vm.runInNewContext(coreSource + '\nglobalThis.api = { daemon, setClientAuthentication, assertClientAuthenticated, connectEvents, state, bus };', context);
  return { ...context.api, calls, streams };
}

test('public Desktop requires account before workspace and model operations', async () => {
  const h = coreHarness({ protected: false, authenticationRequired: true });
  await assert.rejects(h.daemon.workspaceBootstrap(), { code: 'LOGIN_REQUIRED' });
  await assert.rejects(h.daemon.createSession('/workspace'), { code: 'LOGIN_REQUIRED' });
  await assert.rejects(h.daemon.loadModel('byok/model'), { code: 'LOGIN_REQUIRED' });
  assert.deepEqual(h.calls.map(call => call.path), ['/agent/client/capabilities']);
  await h.daemon.authStatus();
  await h.daemon.authStart({ schema: 'meadow.product-auth.v1' });
  assert.equal(h.calls.at(-1).path, '/auth/start');
});

test('verified General account can use public model operations and logout closes stream', async () => {
  const h = coreHarness({ protected: false, authenticationRequired: true });
  h.setClientAuthentication({ authenticated: true, state: 'authorized_general' });
  await h.daemon.loadModel('byok/model');
  assert.equal(h.calls.at(-1).path, '/agent/model');
  h.state.sessionId = 'one';
  h.connectEvents('one');
  assert.equal(h.streams.length, 1);
  h.setClientAuthentication(null);
  assert.equal(h.streams[0].closed, true);
  await assert.rejects(h.daemon.createSession('/workspace'), { code: 'LOGIN_REQUIRED' });
  h.connectEvents('one');
  assert.equal(h.streams.length, 1);
  h.setClientAuthentication({ authenticated: true, state: 'authorized_general' });
  assert.equal(h.streams.length, 2);
  assert.equal(h.streams[1].path, '/session/one/events');
});

test('direct verified account replacement closes the prior stream and ignores its buffered events', async () => {
  const h = coreHarness({ protected: false, authenticationRequired: true });
  await h.daemon.clientCapabilities();
  h.setClientAuthentication({ authenticated: true, state: 'authorized_general', subject: 'first' });
  h.state.sessionId = 'one';
  h.connectEvents('one');
  const oldStream = h.streams[0];
  const events = [];
  h.bus.on('run_state', event => events.push(event));
  h.setClientAuthentication({ authenticated: true, state: 'authorized_general', subject: 'second' });
  assert.equal(oldStream.closed, true);
  assert.equal(h.streams.length, 1, 'new account must not reconnect the old account session');
  oldStream.handlers.get('run_state')({ data: JSON.stringify({
    type: 'run_state', data: { status: 'running', runId: 'old-private-run' },
  }) });
  assert.equal(events.length, 0);
  assert.equal(h.state.runId, '');
});

test('logout then a different verified account keeps Stop available but never reconnects the prior session', async () => {
  const h = coreHarness({ protected: false, authenticationRequired: true });
  await h.daemon.clientCapabilities();
  const first = accountStatus();
  first.subject = 'same-visible-subject';
  h.setClientAuthentication(authContract.parseStatus(first));
  h.state.sessionId = 'account-one-session';
  h.state.runId = 'old-run';
  h.state.busy = true;
  h.state.hasMessages = true;
  h.connectEvents(h.state.sessionId);
  const oldStream = h.streams[0];
  h.setClientAuthentication(null);
  assert.equal(oldStream.closed, true);
  assert.equal(h.state.sessionId, 'account-one-session');
  await h.daemon.cancel(h.state.sessionId);
  assert.equal(h.calls.at(-1).path, '/session/account-one-session/cancel');

  // The parsed lease is the account proof. Never infer continuity from subject.
  const second = accountStatus();
  second.subject = first.subject;
  second.lease.lease_id = 'account-two-lease';
  h.setClientAuthentication(authContract.parseStatus(second));
  assert.equal(h.streams.length, 1);
  assert.equal(h.state.sessionId, null);
  assert.equal(h.state.runId, '');
  assert.equal(h.state.busy, false);
  assert.equal(h.state.hasMessages, false);
  const events = [];
  h.bus.on('run_state', event => events.push(event));
  oldStream.handlers.get('run_state')({ data: JSON.stringify({
    type: 'run_state', data: { status: 'running', runId: 'old-private-run' },
  }) });
  assert.equal(events.length, 0);
  await h.daemon.createSession('/workspace');
  assert.equal(h.calls.at(-1).path, '/session');
});

for (const [name, capability] of [
  ['missing authentication declaration', { protected: false }],
  ['invalid response', {}],
  ['network failure', new Error('offline')],
]) {
  test(`capability ${name} fails closed even in development`, async () => {
    const h = coreHarness(capability);
    await assert.rejects(h.daemon.createSession('/workspace'));
    assert.equal(h.calls.some(call => call.path === '/session'), false);
    await h.daemon.authStatus();
    assert.equal(h.calls.at(-1).path, '/auth/status');
  });
}

test('only explicit test capability may opt out of account authentication', async () => {
  const h = coreHarness({ protected: false, authenticationRequired: false });
  await h.daemon.createSession('/fixture');
  assert.equal(h.calls.at(-1).path, '/session');
});

test('daemon auth rejection revokes cached admission', async () => {
  const h = coreHarness({ protected: false, authenticationRequired: true }, 401);
  h.setClientAuthentication({ authenticated: true, state: 'authorized_general' });
  await assert.rejects(h.daemon.createSession('/workspace'), { status: 401 });
  const dispatched = h.calls.length;
  await assert.rejects(h.daemon.createSession('/workspace'), { code: 'LOGIN_REQUIRED' });
  assert.equal(h.calls.length, dispatched);
});

test('signed-out users retain bounded Stop while new requests stay locked', async () => {
  const h = coreHarness({ protected: false, authenticationRequired: true });
  await h.daemon.cancel('running-one');
  assert.equal(h.calls.at(-1).path, '/session/running-one/cancel');
  await assert.rejects(h.daemon.createSession('/workspace'), { code: 'LOGIN_REQUIRED' });
  assert.equal(h.calls.some(call => call.path === '/session'), false);
});

function deferred() {
  let resolve;
  const promise = new Promise(yes => { resolve = yes; });
  return { promise, resolve };
}

function transportHarness({ fetchImpl, capability, mode = 'production',
  href = 'https://desktop.example/', } = {}) {
  const calls = [];
  const context = {
    console, setTimeout, clearTimeout, URL, Request, Response, ReadableStream,
    AbortController, TextEncoder, TextDecoder,
    document: { documentElement: { dataset: { frontendMode: mode } } },
    location: new URL(href),
    fetch: async (input, init) => {
      calls.push({ input, init });
      if (input === '/agent/client/capabilities') {
        return Response.json(await (capability || {
          protected: false, authenticationRequired: true,
        }));
      }
      return fetchImpl ? fetchImpl(input, init) : Response.json({ ok: true });
    },
  };
  context.window = context;
  vm.runInNewContext(coreSource + `\nglobalThis.api = {
    daemon, setClientAuthentication, installAccountRequestGuard,
    hasClientAccess, clientAccountGeneration, bus,
  };`, context);
  context.api.installAccountRequestGuard();
  return {
    ...context.api, calls, context,
    fetch: (...args) => context.fetch(...args),
    async login(subject = 'account-one') {
      await context.api.daemon.clientCapabilities();
      context.api.setClientAuthentication({
        authenticated: true, state: 'authorized_general', subject,
      });
    },
  };
}

test('global transport rejects raw workspace, provider, model and external requests before login', async () => {
  const h = transportHarness();
  assert.equal(h.hasClientAccess(), false);
  for (const path of ['/workspace/bootstrap', '/native/model-providers',
    '/model/status', '/session/chat/term', 'https://other.example/auth/status']) {
    await assert.rejects(h.fetch(path), { code: 'LOGIN_REQUIRED' });
  }
  assert.deepEqual(h.calls.map(call => call.input), ['/agent/client/capabilities']);
  h.installAccountRequestGuard();
  await h.login();
  assert.equal(h.hasClientAccess(), true);
  await h.fetch('/workspace/bootstrap');
  assert.equal(h.calls.length, 2, 'installing twice must not nest or redispatch');
});

test('only exact same-origin recovery, static GET/HEAD and bounded Stop routes bypass admission', async () => {
  const h = transportHarness();
  for (const [path, method] of [
    ['/auth/status', 'HEAD'], ['/auth/start', 'POST'], ['/auth/poll', 'POST'],
    ['/auth/refresh', 'POST'], ['/auth/logout', 'POST'],
    ['/native/open-url', 'POST'], ['/agent/browser/cancel', 'POST'],
    ['/session/running/cancel', 'POST'], ['/session/running/end', 'POST'],
    ['/health?probe=1', 'GET'], ['/css/login_ascii.css', 'GET'],
    ['/js/commercial.js?v=current', 'HEAD'],
  ]) await h.fetch(path, { method });
  assert.equal(h.calls.some(call => call.input === '/agent/client/capabilities'), false);
  for (const [path, method] of [
    ['/auth/status', 'POST'], ['/auth/unknown', 'GET'],
    ['/js/core.js', 'POST'], ['/native/open-url/extra', 'POST'],
    ['/session/../cancel', 'POST'], ['/js/../workspace/bootstrap', 'GET'],
  ]) await assert.rejects(h.fetch(path, { method }), { code: 'LOGIN_REQUIRED' });
});

test('guard preserves original Request/init, unconsumed body, headers and signal overrides', async () => {
  const h = transportHarness();
  await h.login();
  const inherited = new AbortController();
  const replacement = new AbortController();
  const input = new Request('https://desktop.example/workspace/read', {
    method: 'POST', body: 'original payload', headers: { 'X-Test': 'yes' },
    signal: inherited.signal,
  });
  const init = { signal: replacement.signal, credentials: 'same-origin' };
  await h.fetch(input, init);
  assert.equal(input.bodyUsed, false, 'classification must never clone or consume Request');
  assert.equal(h.calls.at(-1).input, input);
  assert.equal(h.calls.at(-1).init, init);
  assert.equal(await input.text(), 'original payload');

  inherited.abort(new Error('inherited abort'));
  const cancelled = new Request('https://desktop.example/workspace/read', {
    signal: inherited.signal,
  });
  await assert.rejects(h.fetch(cancelled), inherited.signal.reason);
  await h.fetch(cancelled, { signal: null });
  assert.equal(h.calls.at(-1).init.signal, null, 'null must clear an inherited signal');
});

test('effective Request method controls account exemptions without rewriting transport arguments', async () => {
  const h = transportHarness();
  const publicRequest = new Request('https://desktop.example/auth/status');
  await h.fetch(publicRequest);
  await assert.rejects(h.fetch(publicRequest, { method: 'POST' }), { code: 'LOGIN_REQUIRED' });
  await assert.rejects(h.fetch(publicRequest, { method: null }), { code: 'LOGIN_REQUIRED' });
  const statusPost = new Request('https://desktop.example/auth/status', { method: 'POST' });
  await h.fetch(statusPost, { method: 'HEAD' });
  assert.equal(h.calls.at(-1).input, statusPost);
});

test('abort before or during capability lookup never dispatches a protected operation', async () => {
  const pending = deferred();
  const h = transportHarness({ capability: pending.promise });
  const before = new AbortController();
  before.abort(new Error('cancelled before admission'));
  await assert.rejects(h.fetch('/workspace/bootstrap', { signal: before.signal }), before.signal.reason);
  assert.equal(h.calls.length, 0);
  const during = new AbortController();
  const operation = h.fetch('/workspace/bootstrap', { signal: during.signal });
  during.abort(new Error('cancelled during admission'));
  await assert.rejects(operation, during.signal.reason);
  pending.resolve({ protected: false, authenticationRequired: true });
  await h.daemon.clientCapabilities();
  assert.deepEqual(h.calls.map(call => call.input), ['/agent/client/capabilities']);
});

test('protected fetch resolved after logout is discarded even after a new login', async () => {
  const pending = deferred();
  const h = transportHarness({ fetchImpl: () => pending.promise });
  await h.login();
  const generation = h.clientAccountGeneration();
  const operation = h.fetch('/workspace/bootstrap');
  h.setClientAuthentication(null);
  await h.login('account-two');
  assert.ok(h.clientAccountGeneration() > generation);
  pending.resolve(Response.json({ privateWorkspace: true }));
  await assert.rejects(operation, { code: 'LOGIN_REQUIRED' });
  assert.equal(h.hasClientAccess(), true);
});

test('response body and cloned response cannot deliver data after logout', async () => {
  const pendingBody = deferred();
  const h = transportHarness({ fetchImpl: () => ({ ok: true, status: 200,
    json: () => pendingBody.promise,
  }) });
  await h.login();
  const result = await h.fetch('/workspace/bootstrap');
  const consuming = result.json();
  h.setClientAuthentication(null);
  pendingBody.resolve({ privateWorkspace: true });
  await assert.rejects(consuming, { code: 'LOGIN_REQUIRED' });

  const h2 = transportHarness();
  await h2.login();
  const response = await h2.fetch('/workspace/bootstrap');
  const clone = response.clone();
  h2.setClientAuthentication(null);
  await assert.rejects(clone.text(), { code: 'LOGIN_REQUIRED' });
  await assert.rejects(response.blob(), { code: 'LOGIN_REQUIRED' });
});

test('stream reader checks the account again when a pending chunk arrives', async () => {
  let producer;
  const h = transportHarness({ fetchImpl: () => new Response(new ReadableStream({
    start(controller) { producer = controller; },
  })) });
  await h.login();
  const response = await h.fetch('/workspace/file');
  const reader = response.body.getReader();
  const pendingRead = reader.read();
  h.setClientAuthentication(null);
  producer.enqueue(new TextEncoder().encode('private bytes'));
  await assert.rejects(pendingRead, { code: 'LOGIN_REQUIRED' });
  await reader.cancel();
});

test('unsupported pipeTo never queues old-account chunks behind a blocked destination', { timeout: 1000 }, async () => {
  const firstWrite = deferred();
  const releaseWrite = deferred();
  const writes = [];
  const encoder = new TextEncoder();
  const h = transportHarness({ fetchImpl: () => new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(encoder.encode('private-one'));
      controller.enqueue(encoder.encode('private-two'));
      controller.close();
    },
  })) });
  await h.login();
  const response = await h.fetch('/workspace/file');
  const source = response.body;
  const destination = new WritableStream({
    write(chunk) {
      writes.push(new TextDecoder().decode(chunk));
      firstWrite.resolve();
      return releaseWrite.promise;
    },
  });
  const existingWriter = destination.getWriter();
  const priorWrite = existingWriter.write(encoder.encode('already-in-progress'));
  existingWriter.releaseLock();
  await firstWrite.promise;
  const piped = source.pipeTo(destination);
  assert.equal(typeof piped.then, 'function', 'pipeTo retains its promise return contract');
  await assert.rejects(piped, { code: 'ACCOUNT_STREAM_PIPE_UNSUPPORTED' });
  assert.equal(source.locked, false);
  assert.equal(destination.locked, false);
  assert.equal(response.bodyUsed, false);
  h.setClientAuthentication(null);
  await h.login('account-two');
  releaseWrite.resolve();
  await priorWrite;
  assert.deepEqual(writes, ['already-in-progress']);
  await source.cancel();
});

test('unsupported pipeThrough rejects synchronously without locking or delivering data', async () => {
  const transformed = [];
  const h = transportHarness();
  await h.login();
  const response = await h.fetch('/workspace/file');
  const source = response.body;
  const transform = new TransformStream({
    transform(chunk, controller) {
      transformed.push(chunk);
      controller.enqueue(chunk);
    },
  });
  assert.throws(() => source.pipeThrough(transform), { code: 'ACCOUNT_STREAM_PIPE_UNSUPPORTED' });
  assert.equal(source.locked, false);
  assert.equal(transform.writable.locked, false);
  assert.equal(transform.readable.locked, false);
  assert.equal(response.bodyUsed, false);
  h.setClientAuthentication(null);
  await h.login('account-two');
  assert.deepEqual(transformed, []);
  await source.cancel();
});

test('tee branches discard both buffered and late chunks after account replacement', { timeout: 1000 }, async () => {
  for (const buffered of [false, true]) {
    let producer;
    const h = transportHarness({ fetchImpl: () => new Response(new ReadableStream({
      start(controller) { producer = controller; },
    })) });
    await h.login();
    const response = await h.fetch('/workspace/file');
    const readers = response.body.tee().map(branch => branch.getReader());
    if (buffered) producer.enqueue(new TextEncoder().encode('buffered-private-chunk'));
    const pending = buffered ? [] : readers.map(reader => reader.read());
    h.setClientAuthentication(null);
    await h.login('account-two');
    if (!buffered) producer.enqueue(new TextEncoder().encode('late-private-chunk'));
    const results = buffered ? readers.map(reader => reader.read()) : pending;
    await Promise.all(results.map(result => assert.rejects(result, { code: 'LOGIN_REQUIRED' })));
    await Promise.all(readers.map(reader => reader.cancel()));
  }
});

test('values and async iterators discard pending chunks after logout and release their locks', { timeout: 1000 }, async () => {
  for (const method of ['values', Symbol.asyncIterator]) {
    let producer;
    const h = transportHarness({ fetchImpl: () => new Response(new ReadableStream({
      start(controller) { producer = controller; },
    })) });
    await h.login();
    const response = await h.fetch('/workspace/file');
    const source = response.body;
    const iterator = source[method]();
    const pending = iterator.next();
    h.setClientAuthentication(null);
    await h.login('account-two');
    producer.enqueue(new TextEncoder().encode('late-private-chunk'));
    await assert.rejects(pending, { code: 'LOGIN_REQUIRED' });
    await iterator.return();
    assert.equal(source.locked, false);
  }
});

test('valueOf never exposes an unguarded response, body or reader', async () => {
  const h = transportHarness();
  await h.login();
  const response = await h.fetch('/workspace/file');
  assert.equal(response.valueOf(), response);
  const source = response.body;
  assert.equal(source.valueOf(), source);
  const reader = source.getReader();
  assert.equal(reader.valueOf(), reader);
  h.setClientAuthentication(null);
  await h.login('account-two');
  await assert.rejects(response.valueOf().json(), { code: 'LOGIN_REQUIRED' });
  await assert.rejects(reader.valueOf().read(), { code: 'LOGIN_REQUIRED' });
  assert.throws(() => source.valueOf().getReader(), { code: 'LOGIN_REQUIRED' });
  await reader.cancel();
});

test('raw 401 and account denial revoke admission, while ordinary paid denial does not', async () => {
  for (const [status, body] of [[401, {}], [503, { error: { code: 'auth_unavailable' } }],
    [403, { code: 'lease_rejected' }]]) {
    const h = transportHarness({ fetchImpl: () => Response.json(body, { status }) });
    await h.login();
    const changes = [];
    h.bus.on('authentication_required', event => changes.push(event));
    await assert.rejects(h.fetch('/workspace/bootstrap'), { code: 'LOGIN_REQUIRED' });
    assert.equal(h.hasClientAccess(), false);
    assert.equal(changes.length, 1);
  }
  const h = transportHarness({ fetchImpl: () => Response.json({ code: 'upgrade_required' }, { status: 403 }) });
  await h.login();
  assert.equal((await h.fetch('/paid-feature')).status, 403);
  assert.equal(h.hasClientAccess(), true);
});

test('late old-account 401 cannot revoke newer authenticated account', async () => {
  const pending = deferred();
  const h = transportHarness({ fetchImpl: () => pending.promise });
  await h.login();
  const operation = h.fetch('/workspace/bootstrap');
  h.setClientAuthentication(null);
  await h.login('account-two');
  const changes = [];
  h.bus.on('authentication_required', event => changes.push(event));
  pending.resolve(Response.json({ code: 'login_required' }, { status: 401 }));
  await assert.rejects(operation, { code: 'LOGIN_REQUIRED' });
  assert.equal(h.hasClientAccess(), true);
  assert.equal(changes.length, 0);
});

test('unchanged verified account polling retains generation but principal changes fence old work', async () => {
  const h = transportHarness();
  await h.login();
  const before = h.clientAccountGeneration();
  await h.login();
  assert.equal(h.clientAccountGeneration(), before);
  await h.login('account-two');
  assert.ok(h.clientAccountGeneration() > before);
});

test('parsed account lease, device and revocation changes advance the admission fence', async () => {
  const h = transportHarness();
  await h.daemon.clientCapabilities();
  const raw = accountStatus();
  h.setClientAuthentication(authContract.parseStatus(raw));
  let before = h.clientAccountGeneration();
  h.setClientAuthentication(authContract.parseStatus(raw));
  assert.equal(h.clientAccountGeneration(), before);
  for (const update of [{ lease_id: 'new-lease' }, { device_id: 'new-device' },
    { revocation_epoch: 1 }]) {
    Object.assign(raw.lease, update);
    h.setClientAuthentication(authContract.parseStatus(raw));
    assert.ok(h.clientAccountGeneration() > before);
    before = h.clientAccountGeneration();
  }
});

test('external provider rejection does not revoke the Meadow account', async () => {
  const h = transportHarness({ mode: 'development', href: 'http://localhost/',
    fetchImpl: () => Response.json({ code: 'invalid_api_key' }, { status: 401 }),
  });
  await h.login();
  assert.equal((await h.fetch('https://provider.example/v1/models')).status, 401);
  assert.equal(h.hasClientAccess(), true);
});

test('production and native clients reject an authentication opt-out even if DOM mode changes', async () => {
  for (const config of [{ mode: 'production' },
    { mode: 'development', href: 'wails://localhost/' }]) {
    const h = transportHarness({ ...config,
      capability: { protected: false, authenticationRequired: false },
    });
    h.context.document.documentElement.dataset.frontendMode = 'development';
    await assert.rejects(h.fetch('/workspace/bootstrap'), { code: 'CLIENT_CAPABILITIES_UNAVAILABLE' });
    assert.equal(h.hasClientAccess(), false);
  }
});

function accountStatus(plan = 'general') {
  return {
    schema: authContract.PRODUCT_AUTH_SCHEMA,
    state: plan === 'guest' ? 'login_required' : `authorized_${plan}`,
    plan, authenticated: plan !== 'guest',
    capabilities: authContract.capabilitiesForPlan(plan),
    ...(plan === 'guest' ? {} : { lease: {
      lease_id: 'lease', device_id: 'device',
      effective_expires_at: '2099-01-01T00:00:00Z', revocation_epoch: 0,
    } }),
  };
}

function commercialHarness({ capability, status = accountStatus() }) {
  let currentStatus = status;
  let admittedStatus = null;
  let admissionGeneration = 0;
  let timerID = 0;
  const admissions = [];
  const usageCalls = [];
  const timers = new Map();
  const nodes = new Map();
  const context = {
    ...authContract, ...membershipContract, console, AbortController,
    createPromptGrantWarmup,
    setTimeout: (callback, delay) => {
      const id = ++timerID;
      timers.set(id, { callback, delay });
      return id;
    },
    clearTimeout: id => timers.delete(id),
    daemon: {
      clientCapabilities: async () => {
        if (capability instanceof Error) throw capability;
        return capability;
      },
      authStatus: async () => {
        if (currentStatus instanceof Error) throw currentStatus;
        return currentStatus;
      },
      authAdmissionStatus: async () => {
        if (currentStatus instanceof Error) throw currentStatus;
        return currentStatus;
      },
      preparePromptGrant: async () => ({
        schema: 'meadow.desktop-prompt-grant.v1', ready: false, available: 0,
      }),
      membershipUsage: async () => {
        usageCalls.push('/commercial/usage');
        assert.equal(currentStatus.authenticated, true);
        return {
          schema: membershipContract.DESKTOP_MEMBERSHIP_SCHEMA, policy_version: 2,
          usage: { unit: 'accepted_user_prompt', limit: 100, accepted_prompt_count: 31,
            remaining: 69, unlimited: false, can_submit: true },
          access: { kind: 'free_account', active: true, trial_total_days: null,
            remaining_days: null, usage_fraction: null, started_at: null, expires_at: null },
          legacy_prompt_pool: { remaining: 5, total: 5, new_conversation_allowed: true },
          devices: { active: 1, maximum: 3 }, referral: { url: null, earned_days: 0 },
          successful_prompt_count: 0, observed_at: '2026-10-11T00:00:00Z',
        };
      },
    },
    state: {}, bus: { on() {} },
    clientAccountGeneration: () => admissionGeneration,
    setClientAuthentication: status => {
      const authenticated = status?.authenticated === true;
      const identity = value => JSON.stringify([
        value?.subject || '', value?.entitlement?.leaseID || '',
        value?.entitlement?.deviceID || '', value?.entitlement?.revocationEpoch ?? '',
      ]);
      if (!authenticated || !admittedStatus?.authenticated ||
          identity(status) !== identity(admittedStatus)) admissionGeneration += 1;
      admittedStatus = status;
      admissions.push(status);
    },
    document: {
      querySelector: selector => nodes.get(selector) || (selector.startsWith('link[') ? {} : null),
      querySelectorAll: () => [],
    },
    window: { dispatchEvent() {} },
    CustomEvent: class { constructor(type, options) { this.type = type; this.detail = options.detail; } },
  };
  vm.runInNewContext(commercialSource + '\nglobalThis.account = { requireAuthenticatedSession, canUseCapability, canUseModel, publicSnapshot, renderAccountUsage, refreshMembershipUsage };', context);
  return { ...context.account, admissions, usageCalls, nodes, setStatus: value => { currentStatus = value; } };
}

test('mandatory account is independent of paid capability gates', async () => {
  const h = commercialHarness({ capability: { protected: false, authenticationRequired: true } });
  assert.equal(h.canUseCapability('browser.interact'), false);
  assert.equal(await h.requireAuthenticatedSession({ prompt: false }), true);
  assert.equal(h.canUseCapability('browser.interact'), true);
  assert.equal(h.canUseCapability('agent.auto'), true);
  assert.equal(h.canUseModel({ modelId: 'any-provider/model' }), true);
  assert.equal(h.publicSnapshot().authenticated, true);
  h.setStatus(new Error('auth unavailable'));
  assert.equal(await h.requireAuthenticatedSession({ prompt: false }), false);
  assert.equal(h.canUseCapability('browser.interact'), false);
  assert.equal(h.publicSnapshot().authenticated, false);
  assert.equal(h.admissions.at(-1), null);
});

test('missing account and capability lookup failure never authorize public Desktop', async () => {
  for (const options of [
    { capability: { protected: false, authenticationRequired: true }, status: accountStatus('guest') },
    { capability: new Error('capability offline') },
  ]) {
    const h = commercialHarness(options);
    assert.equal(await h.requireAuthenticatedSession({ prompt: false }), false);
    assert.equal(h.publicSnapshot().authenticated, false);
    assert.equal(h.canUseModel({ modelId: 'local/model' }), false);
  }
});

test('public General account shows authoritative free100 usage and separate provider billing', async () => {
  const h = commercialHarness({ capability: { protected: false, authenticationRequired: true } });
  const usage = { innerHTML: '' };
  h.nodes.set('#commercialUsageDetails', usage);
  await h.requireAuthenticatedSession({ prompt: false });
  await h.refreshMembershipUsage();
  h.renderAccountUsage();
  assert.ok(h.usageCalls.length > 0, 'public account usage must use the v2 server projection');
  assert.match(usage.innerHTML, /剩餘 69 \/ 100 次/);
  assert.match(usage.innerHTML, /31 次/);
  assert.match(usage.innerHTML, /模型 API 費用由供應商計收/);
  assert.equal(h.canUseModel({ modelId: 'byok/model' }), true);
  assert.doesNotMatch(usage.innerHTML, /試用到期|操作已鎖定|data-state="offline"/);
  h.setStatus(new Error('account temporarily unavailable'));
  await h.requireAuthenticatedSession({ prompt: false });
  h.renderAccountUsage();
  assert.match(usage.innerHTML, /data-state="reauth"/);
  assert.equal(h.canUseModel({ modelId: 'byok/model' }), false);
  assert.doesNotMatch(usage.innerHTML, /Pro|69 \/ 100|31 次/);
});
