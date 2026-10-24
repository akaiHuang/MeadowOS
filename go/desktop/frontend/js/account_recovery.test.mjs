import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { webcrypto } from 'node:crypto';
import * as auth from './commercial_contract.js';
import * as membership from './membership_contract.js';
import { createPromptGrantWarmup } from './prompt_grant.js';

// Execute the actual shared transport and account module together. Fake time
// and native responses isolate the UI path; no app, Keychain or model is used.
const coreSource = readFileSync(new URL('./core.js', import.meta.url), 'utf8')
  .replace(/^import[^\n]+\n/gm, '').replace(/^export /gm, '');
const commercialSource = readFileSync(new URL('./commercial.js', import.meta.url), 'utf8')
  .replace(/^import[\s\S]*?from '[^']+';\n/gm, '').replace(/^export /gm, '');

function account({ guest = false, subject = 'one', expiry = '2099-01-01T00:00:00Z', reason } = {}) {
  return {
    schema: auth.PRODUCT_AUTH_SCHEMA, state: guest ? 'login_required' : 'authorized_general',
    plan: guest ? 'guest' : 'general', authenticated: !guest,
    capabilities: auth.capabilitiesForPlan(guest ? 'guest' : 'general'),
    ...(reason ? { reason } : {}),
    ...(guest ? {} : { subject, access_expires_at: expiry, lease: {
      lease_id: `lease-${subject}`, device_id: 'device-one',
      effective_expires_at: expiry, revocation_epoch: 0,
    } }),
  };
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function harness({ admission = async () => account(), nativeStatus, prepare, logout,
  work = () => Response.json({ error: { code: 'auth_unavailable' } }, { status: 503 }) } = {}) {
  let now = Date.parse('2026-10-16T12:22:20Z'), timerID = 0;
  const timers = new Map(), calls = [], events = [], streams = [];
  const listeners = new Map();
  const listen = (type, callback) => {
    if (!listeners.has(type)) listeners.set(type, new Set());
    listeners.get(type).add(callback);
  };
  class Clock extends Date { static now() { return now; } }
  const context = {
    ...auth, ...membership, createPromptGrantWarmup, console,
    Date: Clock, URL, Request, Response, ReadableStream, AbortController,
    TextEncoder, TextDecoder, crypto: webcrypto, runLimitOptions: () => ({}),
    setTimeout(callback, delay) { const id = ++timerID; timers.set(id, { callback, at: now + delay }); return id; },
    clearTimeout: id => timers.delete(id),
    document: {
      documentElement: { dataset: { frontendMode: 'production' } },
      visibilityState: 'visible', addEventListener: listen,
      querySelector: selector => selector.startsWith('link[') ? {} : null,
      querySelectorAll: () => [],
    },
    location: new URL('https://desktop.example/'),
    CustomEvent: class { constructor(type, options = {}) { this.type = type; this.detail = options.detail; } },
    dispatchEvent: event => events.push(event),
    addEventListener: listen,
    removeEventListener: (type, callback) => listeners.get(type)?.delete(callback),
    EventSource: class {
      constructor(path) { this.path = path; streams.push(this); }
      addEventListener() {}
      close() { this.closed = true; }
    },
    fetch: async (input, init = {}) => {
      const path = String(input);
      calls.push({ path, method: init.method || 'GET', signal: init.signal });
      if (path === '/agent/client/capabilities') return Response.json({ protected: false, authenticationRequired: true });
      if (path === '/auth/admission-status') {
        const value = await admission(init);
        return value instanceof Response ? value : Response.json(value);
      }
      if (path === '/auth/status') return nativeStatus ? nativeStatus() : Response.json(account());
      if (path === '/auth/logout') return logout ? logout() : Response.json({ schema: auth.PRODUCT_AUTH_SCHEMA, ok: true });
      if (path === '/commercial/prepare-prompt-grant') return prepare ? prepare() : Response.json({ schema: 'meadow.desktop-prompt-grant.v1', ready: true, available: 5, expires_at: '2099-01-01T00:00:00Z' });
      if (path === '/auth/start') return Response.json({ error: { code: 'auth_unavailable' } }, { status: 503 });
      if (path === '/commercial/usage') throw new Error('fixture has no usage transport');
      return work(path, init);
    },
  };
  context.window = context;
  vm.createContext(context);
  vm.runInContext(`(() => { ${coreSource}\nglobalThis.core = { daemon, state, bus,
    setClientAuthentication, hasClientAccess, clientAccountGeneration, installAccountRequestGuard, connectEvents }; })();`, context);
  Object.assign(context, context.core);
  context.core.installAccountRequestGuard();
  vm.runInContext(`(() => { ${commercialSource}\nglobalThis.account = {
    apply: raw => applyStatus(parseStatus(raw)), requireAuthenticatedSession,
    async login(raw) { await resolveProtectedClient(); applyStatus(parseStatus(raw)); },
    snapshot: publicSnapshot, beginLogin, logout: () => logoutAccount(),
    failRead: accountReadFailed,
    armGate() { commercial.loginPromise = Promise.resolve(false);
      commercial.loginClose = () => { globalThis.gateClosed = true; }; },
    recovering: () => accountRecovery !== null,
  }; })();`, context);
  async function flush() { for (let i = 0; i < 36; i++) await Promise.resolve(); }
  async function advance(milliseconds) {
    now += milliseconds;
    for (const [id, timer] of [...timers]) if (timer.at <= now) { timers.delete(id); timer.callback(); }
    await flush();
  }
  return {
    ...context.account, core: context.core, calls, events, streams, timers, context,
    advance, flush, reads: () => calls.filter(x => x.path === '/auth/admission-status'),
    async foreground(type = 'focus', visibility = 'visible') {
      context.document.visibilityState = visibility;
      for (const listener of listeners.get(type) || []) listener({ type });
      await flush();
    },
    async login(raw = account()) { await context.account.login(raw); await flush(); },
    async deny() { await assert.rejects(context.core.daemon.workspaceBootstrap(), { code: 'LOGIN_REQUIRED' }); await flush(); },
  };
}

test('a temporary native denial locks work, then fresh admission restores the account and closes the mandatory gate', async () => {
  const h = harness();
  await h.login();
  h.core.state.sessionId = 'same-account-session';
  h.core.connectEvents('same-account-session');
  await h.deny();
  h.armGate();
  assert.equal(h.core.hasClientAccess(), false);
  assert.equal(h.snapshot().authenticated, false);
  assert.equal(h.streams[0].closed, true);
  const before = h.calls.length;
  await assert.rejects(h.core.daemon.workspaceBootstrap(), { code: 'LOGIN_REQUIRED' });
  assert.equal(h.calls.length, before, 'work must remain locally locked during recovery');
  await h.advance(249);
  assert.equal(h.reads().length, 0);
  await h.advance(1);
  assert.equal(h.reads().length, 1);
  assert.equal(h.core.hasClientAccess(), true);
  assert.equal(h.snapshot().authenticated, true);
  assert.equal(h.context.gateClosed, true);
  assert.equal(h.streams.length, 2, 'only the same verified account may reconnect its existing session');
  assert.equal(h.calls.filter(x => x.path === '/auth/status').length, 0);
  assert.equal(h.calls.filter(x => x.path === '/auth/refresh').length, 0);
});

test('network/account-read failures recover through the local public admission endpoint even before capability boot completed', async () => {
  const h = harness();
  h.failRead(new TypeError('request failed'));
  assert.equal(h.snapshot().authenticated, false);
  await h.advance(250);
  assert.equal(h.reads().length, 1);
  assert.equal(h.snapshot().authenticated, true);
  assert.equal(h.calls.filter(x => x.path === '/agent/client/capabilities').length, 1);
});

test('exhausted availability retries use one admission check per visible cooldown and never call refresh or inference', async () => {
  const h = harness({ admission: async () => { throw new TypeError('offline'); } });
  await h.login();
  await h.deny();
  for (const delay of [250, 1000, 3000, 10000, 30000]) await h.advance(delay);
  assert.equal(h.reads().length, 5);
  assert.equal(h.recovering(), false);
  await h.advance(29_999);
  assert.equal(h.reads().length, 5);
  await h.advance(1);
  assert.equal(h.reads().length, 6);
  await h.advance(29_999);
  assert.equal(h.reads().length, 6);
  await h.advance(1);
  assert.equal(h.reads().length, 7);
  assert.equal(h.snapshot().authenticated, false);
  assert.equal(h.calls.some(x => ['/auth/status', '/auth/refresh', '/session'].includes(x.path)), false);
});

test('a gate left visible recovers after the bounded sequence without another user action', async () => {
  let available = false;
  const h = harness({ admission: async () => available ? account() :
    Response.json({ error: { code: 'auth_unavailable' } }, { status: 503 }) });
  await h.login();
  await h.deny();
  h.armGate();
  for (const delay of [250, 1000, 3000, 10000, 30000]) await h.advance(delay);
  available = true;
  await h.advance(30_000);
  assert.equal(h.reads().length, 6);
  assert.equal(h.snapshot().authenticated, true);
  assert.equal(h.context.gateClosed, true);
  await h.advance(60_000);
  assert.equal(h.reads().length, 6, 'fresh authority ends cooldown maintenance');
  assert.equal(h.calls.some(call => ['/auth/start', '/auth/refresh', '/session'].includes(call.path)), false);
});

test('hidden visibility pauses the cooldown and an in-flight cooldown cannot overlap foreground recovery', async () => {
  let available = false;
  const held = deferred();
  const h = harness({ admission: async () => available ? held.promise :
    Response.json({ error: { code: 'auth_unavailable' } }, { status: 503 }) });
  await h.login();
  await h.deny();
  for (const delay of [250, 1000, 3000, 10000, 30000]) await h.advance(delay);
  await h.foreground('visibilitychange', 'hidden');
  await h.advance(60_000);
  assert.equal(h.reads().length, 5);
  await h.foreground('visibilitychange', 'visible');
  for (const delay of [250, 1000, 3000, 10000, 30000]) await h.advance(delay);
  available = true;
  await h.advance(30_000);
  assert.equal(h.reads().length, 11);
  await h.foreground('focus');
  await h.foreground('online');
  await h.advance(250);
  assert.equal(h.reads().length, 11);
  await h.advance(4750);
  assert.equal(h.reads()[10].signal.aborted, true, 'cooldown attempts retain the five-second deadline');
  held.resolve(account());
  await h.flush();
  assert.equal(h.snapshot().authenticated, false, 'a late deadline response cannot unlock work');
});

test('foreground after exhausted availability retries rechecks native authority and closes the gate', async () => {
  let available = false;
  const h = harness({ admission: async () => available ? account() :
    Response.json({ error: { code: 'auth_unavailable' } }, { status: 503 }) });
  await h.login();
  await h.deny();
  h.armGate();
  for (const delay of [250, 1000, 3000, 10000, 30000]) await h.advance(delay);
  assert.equal(h.reads().length, 5);
  assert.equal(h.recovering(), false);
  assert.equal(h.snapshot().authenticated, false);
  available = true;
  await h.foreground();
  await h.advance(250);
  assert.equal(h.reads().length, 6);
  assert.equal(h.snapshot().authenticated, true);
  assert.equal(h.context.gateClosed, true);
  assert.equal(h.calls.some(call => ['/auth/start', '/auth/refresh', '/session'].includes(call.path)), false);
});

test('visibility and online triggers stay bounded and never overlap an existing recovery', async () => {
  let available = false;
  const held = deferred();
  const h = harness({ admission: async () => available ? held.promise :
    Response.json({ error: { code: 'auth_unavailable' } }, { status: 503 }) });
  await h.login();
  await h.deny();
  for (const delay of [250, 1000, 3000, 10000, 30000]) await h.advance(delay);
  available = true;
  await h.foreground('visibilitychange', 'hidden');
  await h.advance(250);
  assert.equal(h.reads().length, 5);
  await h.foreground('visibilitychange', 'visible');
  await h.foreground('online');
  await h.foreground('focus');
  await h.advance(250);
  assert.equal(h.reads().length, 6);
  await h.foreground('focus');
  await h.foreground('online');
  await h.advance(250);
  assert.equal(h.reads().length, 6, 'foreground events must not duplicate the in-flight native check');
  held.resolve(account());
  await h.flush();
  assert.equal(h.snapshot().authenticated, true);
});

for (const boundary of ['revoked', 'logout', 'new-login']) {
  test(`foreground cannot resume exhausted availability recovery after ${boundary}`, async () => {
    let available = false;
    const h = harness({ admission: async () => available ? account() :
      Response.json({ error: { code: 'auth_unavailable' } }, { status: 503 }) });
    await h.login();
    await h.deny();
    for (const delay of [250, 1000, 3000, 10000, 30000]) await h.advance(delay);
    assert.equal(h.reads().length, 5);
    if (boundary === 'revoked') h.core.bus.emit('authentication_required', { reason: 'expired', retryable: false });
    if (boundary === 'logout') await h.logout();
    if (boundary === 'new-login') await h.beginLogin();
    available = true;
    await h.foreground();
    await h.foreground('online');
    await h.advance(60_000);
    assert.equal(h.reads().length, 5);
    assert.equal(h.snapshot().authenticated, false);
  });
}

for (const code of ['login_required', 'device_revoked', 'activation_revoked', 'lease_expired']) {
  test(`explicit ${code} denial remains locked without automatic re-admission`, async () => {
    const h = harness({ work: () => Response.json({ error: { code } }, { status: 401 }) });
    await h.login();
    await h.deny();
    await h.foreground();
    await h.foreground('online');
    await h.advance(60_000);
    assert.equal(h.snapshot().authenticated, false);
    assert.equal(h.reads().length, 0);
  });
}

for (const result of ['guest', 'revoked', 'expired', 'malformed']) {
  test(`recovery ${result} response never opens work or reuses the old projection`, async () => {
    const value = result === 'malformed' ? { authenticated: true } :
      result === 'expired' ? account({ expiry: '2026-10-16T12:22:19Z' }) : account({ guest: true, reason: result });
    const h = harness({ admission: async () => value });
    await h.login();
    await h.deny();
    await h.advance(250);
    assert.equal(h.snapshot().authenticated, false);
    assert.equal(h.core.hasClientAccess(), false);
    if (result !== 'malformed') {
      await h.advance(60_000);
      assert.equal(h.reads().length, 1, 'authoritative denial stops retries');
    }
  });
}

for (const boundary of ['logout', 'revoked', 'new-login']) {
  test(`an admission response held across ${boundary} cannot restore or replace the newer account`, async () => {
    const held = deferred();
    const h = harness({ admission: () => held.promise });
    await h.login();
    await h.deny();
    await h.advance(250);
    assert.equal(h.reads().length, 1);
    if (boundary === 'logout') await h.logout();
    if (boundary === 'revoked') h.core.bus.emit('authentication_required', { reason: 'expired', retryable: false });
    if (boundary === 'new-login') h.apply(account({ subject: 'new-account' }));
    held.resolve(account());
    await h.flush();
    await h.advance(60_000);
    assert.equal(h.reads().length, 1);
    assert.equal(h.snapshot().authenticated, boundary === 'new-login', JSON.stringify({ snapshot: h.snapshot(), calls: h.calls.map(x => x.path) }));
    assert.equal(h.recovering(), false);
  });
}

test('a hung local admission is timed out and a late response cannot authenticate after the bounded attempt', async () => {
  const held = deferred();
  let reads = 0;
  const h = harness({ admission: () => ++reads === 1 ? held.promise : account({ guest: true }) });
  await h.login();
  await h.deny();
  await h.advance(250);
  await h.advance(5000);
  assert.equal(h.reads()[0].signal.aborted, true);
  held.resolve(account());
  await h.flush();
  assert.equal(h.snapshot().authenticated, false);
  await h.advance(1000);
  assert.equal(h.reads().length, 2);
  assert.equal(h.snapshot().authenticated, false);
});

test('an old startup status failure cannot cancel a newer authoritative account recovery', async () => {
  const held = deferred();
  const h = harness({ nativeStatus: () => held.promise });
  const boot = h.requireAuthenticatedSession({ prompt: false });
  await h.flush();
  h.core.bus.emit('authentication_required', { retryable: true, reason: 'verification_unavailable' });
  await h.advance(250);
  assert.equal(h.snapshot().authenticated, true);
  held.reject(new Error('old startup request failed'));
  assert.equal(await boot, false);
  assert.equal(h.snapshot().authenticated, true);
});

test('a failing grant warmup does not restart itself after an admission-only recovery', async () => {
  const h = harness({ prepare: () => Response.json({ error: { code: 'auth_unavailable' } }, { status: 503 }) });
  await h.login();
  assert.equal(h.snapshot().authenticated, false);
  await h.advance(250);
  assert.equal(h.snapshot().authenticated, true);
  await h.advance(60_000);
  assert.equal(h.reads().length, 1);
  assert.equal(h.calls.filter(x => x.path === '/commercial/prepare-prompt-grant').length, 1);
  assert.equal(h.snapshot().authenticated, true);
});

for (const late of ['guest', 'failure']) {
  test(`an old public status ${late} cannot overwrite a newer recovery without a poll-generation change`, async () => {
    const held = deferred();
    const h = harness({ nativeStatus: () => held.promise });
    const pending = h.requireAuthenticatedSession({ prompt: false });
    await h.flush();
    h.failRead(new TypeError('temporary account observation failed'));
    await h.advance(250);
    assert.equal(h.snapshot().authenticated, true);
    if (late === 'guest') held.resolve(Response.json(account({ guest: true })));
    else held.reject(new Error('old request failed'));
    assert.equal(await pending, false);
    assert.equal(h.snapshot().authenticated, true);
    await h.advance(60_000);
    assert.equal(h.reads().length, 1);
  });
}

test('logout intent fences public reads and blocks recovery before native logout confirmation', async () => {
  const heldStatus = deferred(), heldLogout = deferred();
  const h = harness({ nativeStatus: () => heldStatus.promise, logout: () => heldLogout.promise });
  await h.login();
  const old = h.requireAuthenticatedSession({ prompt: false });
  await h.flush();
  const signingOut = h.logout();
  await h.flush();
  assert.equal(h.core.hasClientAccess(), false);
  heldStatus.resolve(Response.json(account()));
  assert.equal(await old, false);
  h.failRead(new TypeError('another in-flight observation failed'));
  await h.advance(60_000);
  assert.equal(h.reads().length, 0);
  heldLogout.resolve(Response.json({ schema: auth.PRODUCT_AUTH_SCHEMA, ok: true }));
  await signingOut;
  assert.equal(h.snapshot().authenticated, false);
});
