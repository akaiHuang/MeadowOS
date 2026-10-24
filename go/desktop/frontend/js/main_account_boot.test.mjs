import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { webcrypto } from 'node:crypto';
import vm from 'node:vm';
import * as authContract from './commercial_contract.js';
import * as membershipContract from './membership_contract.js';
import { createPromptGrantWarmup } from './prompt_grant.js';

// Run the real boot, account parser, private admission, and guarded transport
// together. Only browser facilities, network replies, and operational module
// initializers are fakes. Each source has its own lexical scope, as with ESM.
// No daemon, provider, Firebase account, or installed App is contacted.
const source = name => readFileSync(new URL(name, import.meta.url), 'utf8')
  .replace(/^import\s+['"][^'"]+['"];\n/gm, '')
  .replace(/^import[\s\S]*?from ['"][^'"]+['"];\n/gm, '')
  .replace(/^export /gm, '');
const sources = Object.fromEntries(
  ['core', 'commercial', 'product_experience', 'main'].map(name => [name, source(`./${name}.js`)]),
);
const operationalInitializers = [
  'initSidebar', 'initTimeline', 'initComposer', 'initHarnessControls',
  'initTerminal', 'initGitbar', 'initRightPanel', 'initDiffPanel',
  'initWorkCenter', 'initContextMenu', 'initPreview', 'initBrowserDrawer',
  'initExpertLens', 'initPalette', 'initOutputTabs', 'initSettings',
  'initPortfolioDemo', 'initMobileShell', 'initForegroundState',
];
const ONBOARDING_KEY = 'meadow.product-experience.onboarding.v2';

function status(plan = 'guest') {
  return {
    schema: authContract.PRODUCT_AUTH_SCHEMA,
    state: plan === 'guest' ? 'login_required' : `authorized_${plan}`,
    plan, authenticated: plan !== 'guest',
    capabilities: authContract.capabilitiesForPlan(plan),
    ...(plan === 'guest' ? {} : { lease: {
      lease_id: 'boot-lease', device_id: 'boot-device',
      effective_expires_at: '2099-01-01T00:00:00Z', revocation_epoch: 0,
    } }),
  };
}

function membershipUsage() {
  return {
    schema: membershipContract.DESKTOP_MEMBERSHIP_SCHEMA, policy_version: 2,
    usage: { unit: 'accepted_user_prompt', limit: 100, accepted_prompt_count: 23,
      remaining: 77, unlimited: false, can_submit: true },
    access: { kind: 'free_account', active: true, trial_total_days: null,
      remaining_days: null, usage_fraction: null, started_at: null, expires_at: null },
    legacy_prompt_pool: { remaining: 5, total: 5, new_conversation_allowed: true },
    devices: { active: 1, maximum: 3 }, referral: { url: null, earned_days: 0 },
    successful_prompt_count: 0, observed_at: '2026-10-11T00:00:00Z',
  };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function element() {
  const listeners = new Map();
  const label = { textContent: '登入' };
  return {
    isConnected: true, hidden: false, disabled: false, textContent: '',
    dataset: {}, style: {}, classList: { add() {}, toggle() {} },
    querySelector: selector => selector === '.commercial-provider-label' ? label : null,
    querySelectorAll: () => [],
    addEventListener(type, listener) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(listener);
    },
    removeEventListener: (type, listener) => listeners.get(type)?.delete(listener),
    setAttribute() {}, removeAttribute() {}, append() {}, focus() {},
    remove() { this.isConnected = false; },
    contains: () => false,
  };
}

function bootHarness({
  account = status(), dismissedOnboarding = true,
  capability = { protected: false, authenticationRequired: true },
  prepareReply = () => Response.json({
    schema: 'meadow.desktop-prompt-grant.v1', ready: false, available: 0,
  }),
} = {}) {
  let currentAccount = account;
  let modelReply = () => Response.json({ status: 'ready', model: 'fixture/model' });
  let now = Date.parse('2026-10-16T00:00:00Z');
  let nextTimer = 0;
  const calls = [];
  const issuedGrants = new Map();
  const effects = [];
  const errors = [];
  const modelEvents = [];
  const streams = [];
  const intervals = new Map();
  const timers = new Map();
  const events = new Map();
  const created = [];
  const storage = new Map([
    ['meadow.cwd', '/prior-project'],
    ...(dismissedOnboarding ? [[ONBOARDING_KEY, 'seen']] : []),
  ]);
  const nodes = new Map([
    'commercialLoginStart', 'commercialUserCode', 'commercialDeviceCode',
    'commercialLoginOpen', 'commercialLoginStatus', 'commercialLoginTheme',
    'commercialLoginEmailFlow',
  ].map(id => [`#${id}`, element()]));
  class Clock extends Date { static now() { return now; } }
  const context = {
    ...authContract, ...membershipContract,
    createPromptGrantWarmup,
    Date: Clock, URL, URLSearchParams, Request, Response, ReadableStream,
    AbortController, TextEncoder, TextDecoder, crypto: webcrypto,
    console: { log() {}, error: (...args) => errors.push(args), warn() {} },
    location: new URL('https://desktop.example.test/'),
    localStorage: {
      getItem: key => storage.get(key) ?? null,
      setItem: (key, value) => storage.set(key, String(value)),
      removeItem: key => storage.delete(key),
    },
    document: {
      documentElement: { dataset: { frontendMode: 'production' } },
      querySelector: selector => nodes.get(selector) ||
        created.find(node => node.isConnected && selector === `#${node.id}`) ||
        (selector.startsWith('link[') ? {} : null),
      querySelectorAll: () => [],
      createElement: () => { const node = element(); created.push(node); return node; },
      body: { append() {} },
    },
    addEventListener(type, listener) {
      if (!events.has(type)) events.set(type, new Set());
      events.get(type).add(listener);
    },
    removeEventListener: (type, listener) => events.get(type)?.delete(listener),
    dispatchEvent(event) {
      for (const listener of [...(events.get(event.type) || [])]) listener(event);
      return true;
    },
    CustomEvent: class { constructor(type, options = {}) { this.type = type; this.detail = options.detail; } },
    HTMLElement: class {},
    EventSource: class {
      constructor(path) { this.path = path; streams.push(this); }
      addEventListener() {}
      close() { this.closed = true; }
    },
    setInterval(callback, delay) {
      const id = ++nextTimer;
      intervals.set(id, { callback, delay });
      return id;
    },
    clearInterval: id => intervals.delete(id),
    setTimeout(callback, delay) {
      const id = ++nextTimer;
      timers.set(id, { callback, delay, at: now + delay });
      return id;
    },
    clearTimeout: id => timers.delete(id),
    requestAnimationFrame: callback => callback(),
    matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
    createAsciiScene: () => ({ setPaused() {}, setTheme() {}, setActive() {}, destroy() {} }),
    getTheme: () => 'dark', setTheme() {},
    initAccountAccessGuard: options => { effects.push('accountGuard'); context.accessGuard = options; },
    loadProjects: () => { effects.push('loadProjects'); return [{ cwd: '/prior-project' }]; },
    saveProjects: () => effects.push('saveProjects'),
    getStoredActive: () => { effects.push('getStoredActive'); return '/prior-project'; },
    setActiveCwd: cwd => { effects.push('setActiveCwd'); context.state.cwd = cwd; },
    canonicalizeActive: cwd => { effects.push('canonicalizeActive'); context.state.cwd = cwd; },
    seedPortfolioDemoProjects: () => effects.push('seedPortfolioDemoProjects'),
    fetch: async (path, options) => {
      calls.push({ path, options });
      if (path === '/agent/client/capabilities') {
        if (capability instanceof Error) throw capability;
        return Response.json(capability);
      }
      if (path === '/auth/status' || path === '/auth/admission-status' || path === '/entitlement/status') {
        if (currentAccount instanceof Error) throw currentAccount;
        return Response.json(currentAccount);
      }
      if (path === '/model/status') return modelReply();
      if (path === '/workspace/bootstrap') return Response.json({ automation: false });
      if (path === '/commercial/usage') {
        assert.equal(currentAccount.authenticated, true);
        return Response.json(membershipUsage());
      }
      if (path === '/commercial/prepare-prompt-grant') {
        assert.equal(currentAccount.authenticated, true);
        assert.deepEqual(JSON.parse(options.body), { schema: 'meadow.desktop-prompt-grant.v1' });
        return prepareReply();
      }
      if (path === '/commercial/authorize-new-conversation') {
        assert.equal(currentAccount.authenticated, true);
        const request = JSON.parse(options.body);
        assert.equal(request.schema, membershipContract.DESKTOP_MEMBERSHIP_SCHEMA);
        assert.equal(typeof request.request_id, 'string');
        assert.equal(typeof request.conversation_id, 'string');
        const grant = { ...request, policy_version: 2, usage: membershipUsage().usage,
          scope: 'new_conversation', decision: 'authorized', reason: 'free_account',
          authorization_id: `boot-grant-${issuedGrants.size + 1}-fixture`,
          expires_at: '2099-01-01T00:00:00Z' };
        issuedGrants.set(grant.authorization_id, grant);
        return Response.json(grant);
      }
      if (path === '/session') {
        const request = JSON.parse(options.body);
        const binding = request.membership_authorization;
        const grant = issuedGrants.get(binding?.authorization_id);
        assert.ok(grant, 'session creation requires a real request-bound fixture grant');
        assert.deepEqual(binding, { schema: grant.schema, request_id: grant.request_id,
          authorization_id: grant.authorization_id, conversation_id: grant.conversation_id });
        assert.equal(options.headers['X-Meadow-Membership-Authorization-ID'], grant.authorization_id);
        return Response.json({
          sessionId: 'boot-session', workspaceCwd: '/prior-project', modelId: 'fixture/model',
        });
      }
      throw new Error(`Unexpected fake transport request: ${path}`);
    },
  };
  context.window = context;
  for (const name of operationalInitializers) context[name] = () => effects.push(name);
  vm.createContext(context);
  vm.runInContext(`globalThis.core = (() => { ${sources.core}\nreturn {
    daemon, ensureSession, bus, state, assertClientAuthenticated, hasClientAccess,
    installAccountRequestGuard, setClientAuthentication, clientAccountGeneration,
  }; })();`, context, { filename: 'core.js' });
  Object.assign(context, context.core);
  vm.runInContext(`globalThis.account = (() => { ${sources.commercial}\nreturn {
    commercialSessionReady, requireAuthenticatedSession, requestAccountLogin,
    authorizeNewConversation,
  }; })();`, context, { filename: 'commercial.js' });
  Object.assign(context, context.account);
  vm.runInContext(`globalThis.product = (() => { ${sources.product_experience}\nreturn {
    showProductOnboarding,
  }; })();`, context, { filename: 'product_experience.js' });
  context.showProductOnboarding = (...args) => {
    effects.push('showProductOnboarding');
    return context.product.showProductOnboarding(...args);
  };
  context.bus.on('model_status', value => modelEvents.push(value));
  vm.runInContext(`(() => { ${sources.main}\n})();`, context, { filename: 'main.js' });

  async function flush() {
    // Drain Response.json plus nested boot/account/session continuations without
    // advancing any fake polling clock or using a real transport.
    for (let index = 0; index < 60; index += 1) await Promise.resolve();
    await new Promise(resolve => setImmediate(resolve));
  }
  async function advance(milliseconds) {
    now += milliseconds;
    for (const [id, timer] of [...timers]) {
      if (timer.at <= now) {
        timers.delete(id);
        timer.callback();
      }
    }
    await flush();
  }
  return {
    context, calls, effects, errors, modelEvents, streams, intervals, timers, storage, flush, advance,
    async authenticate(value = status('general')) {
      currentAccount = value;
      const admitted = await context.account.requireAuthenticatedSession({ prompt: false });
      await flush();
      return admitted;
    },
    async logout() {
      currentAccount = status();
      await context.MeadowCommercial.refresh();
      await flush();
    },
    forge(detail = { authenticated: true, capabilitiesReady: true, state: 'authorized_pro', authenticationRequired: false }) {
      context.dispatchEvent(new context.CustomEvent('commercial:change', { detail }));
    },
    setModelReply: reply => { modelReply = reply; },
    async poll() {
      for (const { callback } of intervals.values()) callback();
      await flush();
    },
  };
}

function assertGuest(h) {
  assert.equal(h.context.hasClientAccess(), false);
  assert.equal(h.context.accessGuard.isAllowed(), false);
  assert.deepEqual(h.effects, ['accountGuard']);
  assert.equal(h.intervals.size, 0);
  assert.equal(h.streams.length, 0);
  assert.equal(h.context.state.cwd, null);
  assert.equal(h.context.state.sessionId, null);
  assert.equal(h.modelEvents.length, 0);
  assert.ok(h.context.document.querySelector('#commercialLoginGate'));
  assert.ok(h.calls.every(({ path }) => ['/agent/client/capabilities', '/auth/status'].includes(path)));
  assert.deepEqual(h.errors, []);
}

for (const dismissedOnboarding of [false, true]) {
  test(`guest boot initializes no operational work with onboarding dismissed=${dismissedOnboarding}`, async () => {
    const h = bootHarness({ dismissedOnboarding });
    await h.flush();
    assertGuest(h);
    h.forge();
    await h.flush();
    assertGuest(h);
    assert.equal(await h.context.MeadowDesktop.startAuthorizedWorkspace(), false);
    await assert.rejects(h.context.MeadowDesktop.ensureWorkspaceSelected(), { code: 'LOGIN_REQUIRED' });
    await h.flush();
    assertGuest(h);
  });
}

for (const [name, account, capability] of [
  ['an auth response rejected by the closed parser', { ...status('general'), unexpected: true }],
  ['an unavailable auth service', new Error('offline')],
  ['missing mandatory capability declaration', status('general'), { protected: false }],
]) {
  test(`boot remains inert after ${name}`, async () => {
    const h = bootHarness({ account, ...(capability ? { capability } : {}) });
    await h.flush();
    assertGuest(h);
    h.forge();
    await h.flush();
    assertGuest(h);
  });
}

for (const authenticatedAtBoot of [false, true]) {
  test(`parsed General admission initializes operational modules once (already logged in=${authenticatedAtBoot})`, async () => {
    const h = bootHarness({ account: status(authenticatedAtBoot ? 'general' : 'guest') });
    await h.flush();
    if (!authenticatedAtBoot) {
      assertGuest(h);
      assert.equal(await h.authenticate(), true);
    }
    assert.equal(h.context.hasClientAccess(), true);
    for (let index = 0; index < 5; index += 1) h.forge();
    await h.flush();
    for (const name of [...operationalInitializers, 'seedPortfolioDemoProjects', 'showProductOnboarding']) {
      assert.equal(h.effects.filter(effect => effect === name).length, 1, name);
    }
    assert.equal(h.intervals.size, 1);
    assert.equal([...h.intervals.values()][0].delay, 2000);
    assert.equal(h.calls.filter(({ path }) => path === '/model/status').length, 1);
    assert.equal(h.calls.filter(({ path }) => path === '/workspace/bootstrap').length, 1);
    const authorizationCalls = h.calls.filter(({ path }) => path === '/commercial/authorize-new-conversation');
    assert.equal(authorizationCalls.length, 1, 'public General boot must obtain v2 conversation admission');
    assert.ok(h.calls.some(({ path }) => path === '/commercial/usage'));
    assert.equal(h.calls.filter(({ path }) => path === '/session').length, 1);
    assert.ok(h.calls.indexOf(authorizationCalls[0]) < h.calls.findIndex(({ path }) => path === '/session'));
    assert.equal(h.context.state.sessionId, 'boot-session');
    assert.equal(h.streams.length, 1);
    assert.equal(h.storage.get(ONBOARDING_KEY), 'seen');
    assert.equal(h.context.document.querySelector('#commercialLoginGate'), null);
    assert.deepEqual(h.errors, []);
  });
}

test('scheduled model polling remains silent after parsed logout and forged unlock events', async () => {
  const h = bootHarness({ account: status('general') });
  await h.flush();
  await h.poll();
  assert.equal(h.calls.filter(({ path }) => path === '/model/status').length, 2);
  await h.logout();
  const callsAfterLogout = h.calls.length;
  const eventsAfterLogout = h.modelEvents.length;
  const effectsAfterLogout = h.effects.length;
  h.forge();
  await h.poll();
  await h.poll();
  assert.equal(h.context.hasClientAccess(), false);
  assert.equal(h.calls.length, callsAfterLogout);
  assert.equal(h.modelEvents.length, eventsAfterLogout);
  assert.equal(h.effects.length, effectsAfterLogout);
  assert.equal(h.streams[0].closed, true);
  assert.deepEqual(h.errors, []);
});

test('failed automatic grant warmup recovers through actual main reactivation without a repeated warmup or login loop', async () => {
  const pendingPrepare = deferred();
  const h = bootHarness({ account: status('general'), prepareReply: () => pendingPrepare.promise });
  const count = path => h.calls.filter(call => call.path === path).length;
  await h.flush();
  assert.equal(h.context.hasClientAccess(), true);
  assert.equal(h.context.state.sessionId, 'boot-session');
  assert.equal(count('/commercial/prepare-prompt-grant'), 1);
  const initialStatusReads = count('/auth/status');

  pendingPrepare.resolve(Response.json({ error: { code: 'auth_unavailable' } }, { status: 503 }));
  await h.flush();
  assert.equal(h.context.hasClientAccess(), false, 'a transient auth denial must lock work before recovery');
  assert.equal(h.context.accessGuard.isAllowed(), false);
  assert.equal(count('/auth/admission-status'), 0);
  await h.advance(249);
  assert.equal(count('/auth/admission-status'), 0);
  await h.advance(1);

  assert.equal(count('/auth/admission-status'), 1, 'recovery reads fresh local admission exactly once');
  assert.equal(count('/auth/status'), initialStatusReads + 1,
    'the real main commercial:change listener reactivates and rechecks the workspace account');
  assert.equal(h.context.hasClientAccess(), true);
  assert.equal(h.context.accessGuard.isAllowed(), true);
  assert.equal(h.context.MeadowCommercial.snapshot().authenticated, true);
  assert.equal(h.context.state.sessionId, 'boot-session');
  assert.equal(count('/commercial/prepare-prompt-grant'), 1,
    'main reactivation must not repeat the failed background POST for the same signed account');

  for (const delay of [1000, 3000, 10000, 30000]) await h.advance(delay);
  assert.equal(h.context.hasClientAccess(), true);
  assert.equal(count('/auth/admission-status'), 1, 'a recovered account must not restart the recovery sequence');
  assert.equal(count('/commercial/prepare-prompt-grant'), 1);
  assert.equal(count('/auth/start'), 0, 'background recovery must not initiate browser sign-in');
  assert.equal(count('/auth/refresh'), 0, 'the frontend must not rotate or extend native authority');
  assert.equal(h.context.document.querySelector('#commercialLoginGate'), null);
  for (const name of operationalInitializers)
    assert.equal(h.effects.filter(effect => effect === name).length, 1, name);
  assert.deepEqual(h.errors, []);
});

for (const outcome of ['response', 'rejection']) {
  test(`an in-flight model poll ${outcome} cannot publish model state after logout`, async () => {
    const h = bootHarness({ account: status('general') });
    await h.flush();
    const pending = deferred();
    h.setModelReply(() => pending.promise);
    await h.poll();
    assert.equal(h.calls.filter(({ path }) => path === '/model/status').length, 2);
    await h.logout();
    h.context.state.model = '';
    const eventsAfterLogout = h.modelEvents.length;
    if (outcome === 'response') pending.resolve(Response.json({ status: 'ready', model: 'stale/private-model' }));
    else pending.reject(new Error('late connection failure'));
    await h.flush();
    assert.equal(h.context.state.model, '');
    assert.equal(h.modelEvents.length, eventsAfterLogout, 'a settled old poll must not emit a signed-out model status');
    assert.deepEqual(h.errors, []);
  });

  test(`an old model poll ${outcome} cannot publish into a later login`, async () => {
    const h = bootHarness({ account: status('general') });
    await h.flush();
    const pending = deferred();
    h.setModelReply(() => pending.promise);
    await h.poll();
    await h.logout();
    assert.equal(await h.authenticate(), true);
    const eventsAfterLogin = h.modelEvents.length;
    h.context.state.model = '';
    if (outcome === 'response') pending.resolve(Response.json({ status: 'ready', model: 'previous/private-model' }));
    else pending.reject(new Error('previous account connection failure'));
    await h.flush();
    assert.equal(h.context.state.model, '');
    assert.equal(h.modelEvents.length, eventsAfterLogin, 'a previous admission must not emit status into the new login');
    assert.deepEqual(h.errors, []);
  });
}
