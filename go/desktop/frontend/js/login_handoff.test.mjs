import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import * as authContract from './commercial_contract.js';
import { createPromptGrantWarmup } from './prompt_grant.js';

// Exercise the real login flow and closed auth schemas. Only the DOM, clock,
// and daemon transport are fakes; no Firebase account or browser is contacted.
const source = readFileSync(new URL('./commercial.js', import.meta.url), 'utf8');
const commercialSource = source
  .replace(/^import[\s\S]*?from '[^']+';\n/gm, '')
  .replace(/^export /gm, '');

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function startResponse(name = 'first', extra = {}) {
  return {
    schema: authContract.PRODUCT_AUTH_SCHEMA,
    state: 'login_pending',
    transaction_id: `transaction-${name}`,
    user_code: `CODE-${name}`,
    verification_uri: 'https://account.example.test/device',
    verification_uri_complete: `https://account.example.test/device?attempt=${name}`,
    expires_in: 60,
    interval: 1,
    ...extra,
  };
}

function pendingResponse(extra = {}) {
  return {
    schema: authContract.PRODUCT_AUTH_SCHEMA,
    state: 'login_pending',
    error_code: 'authorization_pending',
    interval: 1,
    ...extra,
  };
}

function terminalResponse(state) {
  return {
    schema: authContract.PRODUCT_AUTH_SCHEMA,
    state,
    error_code: state === 'login_denied' ? 'access_denied' : 'expired_token',
  };
}

function loginFailure(status, code) {
  return Object.assign(new Error('RAW_SERVER_ERROR https://private.invalid/?token=SECRET <script>'), {
    status, code,
    data: { error: { message: 'RAW_SERVER_BODY', verification_uri: 'https://private.invalid/' } },
  });
}

function authorizedResponse() {
  return {
    schema: authContract.PRODUCT_AUTH_SCHEMA,
    state: 'authorized_general',
    status: {
      schema: authContract.PRODUCT_AUTH_SCHEMA,
      state: 'authorized_general',
      plan: 'general',
      authenticated: true,
      capabilities: authContract.capabilitiesForPlan('general'),
      lease: {
        lease_id: 'lease', device_id: 'device',
        effective_expires_at: '2099-01-01T00:00:00Z', revocation_epoch: 0,
      },
    },
  };
}

function element(hidden = false) {
  const label = { textContent: '登入' };
  const listeners = new Map();
  return {
    textContent: '', hidden, disabled: false, dataset: {}, style: {},
    isConnected: true, classList: { add() {} },
    querySelector: selector => selector === '.commercial-provider-label' ? label : null,
    querySelectorAll: () => [],
    addEventListener(type, listener) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(listener);
    },
    removeEventListener(type, listener) { listeners.get(type)?.delete(listener); },
    dispatch(type, details = {}) {
      const event = {
        currentTarget: this, target: this, defaultPrevented: false,
        preventDefault() { this.defaultPrevented = true; },
        stopPropagation() {}, stopImmediatePropagation() {},
        ...details,
      };
      return [...(listeners.get(type) || [])].map(listener => listener(event));
    },
    setAttribute() {}, append() {}, focus() {},
    remove() { this.isConnected = false; },
    contains() { return false; },
  };
}

function loginHarness({
  authStart = async () => startResponse(),
  authPoll = async () => pendingResponse(),
  openURL = async () => {},
  authenticationRequired = false,
  authenticated = false,
  authStatus = async () => authorizedResponse().status,
  entitlementStatus = async () => authorizedResponse().status,
  cancel = async () => ({}),
  sessionState = {},
} = {}) {
  let now = 0;
  let timerID = 0;
  let admittedStatus = null;
  let admissionGeneration = 0;
  const createdNodes = [];
  const timers = new Map();
  const calls = { starts: [], startOptions: [], polls: [], urls: [], fallbackURLs: [], admissions: [], statuses: 0, entitlements: 0, logouts: [], cancels: [] };
  const busListeners = new Map();
  const state = { ...sessionState };
  const nodes = new Map([
    ['#commercialLoginStart', element()],
    ['#commercialUserCode', element(true)],
    ['#commercialDeviceCode', element(true)],
    ['#commercialLoginOpen', element(true)],
    ['#commercialLoginDeviceManagement', { ...element(true), disabled: true }],
    ['#commercialLoginStatus', element()],
    ['#commercialLoginTheme', element()],
    ['#commercialLoginClose', element()],
    ['#commercialBackToEmail', element()],
    ['#commercialLoginEmailFlow', element()],
    ['#commercialLoginSerialFlow', element(true)],
    ['#commercialRedeemInput', element()],
  ]);
  class Clock extends Date { static now() { return now; } }
  const context = {
    ...authContract, console, Date: Clock, AbortController,
    createPromptGrantWarmup,
    fixture: { authenticationRequired, status: authenticated ? authorizedResponse().status : null },
    setTimeout: (callback, delay) => {
      const id = ++timerID;
      timers.set(id, { callback, at: now + delay });
      return id;
    },
    clearTimeout: id => timers.delete(id),
    daemon: {
      clientCapabilities: async () => ({ protected: false, authenticationRequired }),
      authStatus: () => { calls.statuses += 1; return authStatus(); },
      authAdmissionStatus: () => { calls.statuses += 1; return authStatus(); },
      entitlementStatus: () => { calls.entitlements += 1; return entitlementStatus(); },
      preparePromptGrant: async () => ({
        schema: 'meadow.desktop-prompt-grant.v1', ready: false, available: 0,
      }),
      authLogout: async request => { calls.logouts.push(request); return { schema: authContract.PRODUCT_AUTH_SCHEMA, ok: true }; },
      cancel: session => { calls.cancels.push(session); return cancel(session); },
      authStart: (request, options) => {
        calls.starts.push(request);
        calls.startOptions.push(options);
        return authStart(request, options);
      },
      authPoll: request => { calls.polls.push(request); return authPoll(request); },
      openURL: url => { calls.urls.push(url); return openURL(url); },
    },
    state, bus: { on(type, listener) {
      if (!busListeners.has(type)) busListeners.set(type, new Set());
      busListeners.get(type).add(listener);
    } },
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
      calls.admissions.push(status);
    },
    document: {
      querySelector: selector => nodes.get(selector) ||
        createdNodes.find(node => node.isConnected && selector === `#${node.id}`) ||
        (selector.startsWith('link[') ? {} : null),
      querySelectorAll: () => [],
      createElement: () => {
        const node = element();
        createdNodes.push(node);
        return node;
      },
      body: { append() {} },
    },
    window: {
      dispatchEvent() {},
      open: url => { calls.fallbackURLs.push(url); return {}; },
      addEventListener() {}, removeEventListener() {},
      matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
    },
    CustomEvent: class {},
    HTMLElement: class {},
    requestAnimationFrame: callback => callback(),
    createAsciiScene: () => ({ setPaused() {}, setTheme() {}, setActive() {}, destroy() {} }),
    getTheme: () => 'dark', setTheme() {},
  };
  // Explicit fixture-only opt-out keeps optional-dialog cancellation coverage
  // separate from the mandatory guest gate used by the shipping application.
  vm.runInNewContext(commercialSource + `
    commercial.authenticationRequired = fixture.authenticationRequired;
    commercial.capabilitiesReady = true;
    commercial.protectedMode = false;
    commercial.status = fixture.status ? parseStatus(fixture.status) : null;
    globalThis.login = { beginLogin, closeLoginGate, publicSnapshot, requestAccountLogin, showLoginGate, refreshStatus, requireAuthenticatedSession, logoutAccount };
  `, context);
  async function flush() {
    for (let i = 0; i < 12; i += 1) await Promise.resolve();
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
    ...context.login, calls, nodes, createdNodes, timers, advance, flush, state,
    emitBus: (type, detail) => { for (const listener of busListeners.get(type) || []) listener(detail); },
    code: nodes.get('#commercialUserCode'),
    block: nodes.get('#commercialDeviceCode'),
    reopen: nodes.get('#commercialLoginOpen'),
    message: nodes.get('#commercialLoginStatus'),
    button: nodes.get('#commercialLoginStart'),
    management: nodes.get('#commercialLoginDeviceManagement'),
  };
}

function assertCleared(h) {
  assert.equal(h.code.textContent, '', 'discard the previous verification code');
  assert.equal(h.code.hidden, true);
  assert.equal(h.block.hidden, true);
  assert.equal(h.reopen.hidden, true);
  assert.equal(h.reopen.disabled, true);
  assert.equal(h.reopen.dataset.url, undefined, 'discard the previous external URL');
}

function assertCurrent(h, name) {
  assert.equal(h.code.textContent, `CODE-${name}`);
  assert.equal(h.code.hidden, false);
  assert.equal(h.block.hidden, false);
  assert.equal(h.reopen.hidden, false);
  assert.equal(h.reopen.disabled, false);
  assert.equal(h.reopen.dataset.url, startResponse(name).verification_uri_complete);
  assert.equal(h.management.hidden, true, 'a new login retires device-limit recovery');
  assert.equal(h.management.disabled, true);
}

test('the sole login button starts the unchanged browser authentication handoff once', async () => {
  const started = startResponse('single');
  started.verification_uri_complete = 'https://account.example.test/device?transaction=single&return_to=%2Fdesktop';
  const h = loginHarness({
    authenticationRequired: true,
    authStart: async () => started,
    authPoll: async () => authorizedResponse(),
  });
  const login = h.requestAccountLogin();
  const markup = h.createdNodes.find(node => node.innerHTML?.includes('commercialLoginStart'))?.innerHTML;
  assert.ok(markup, 'inspect the actual dialog generated by requestAccountLogin');
  const buttons = [...markup.matchAll(/<button\b([^>]*)>([\s\S]*?)<\/button>/g)];
  const choices = buttons.filter(([, attributes]) => /\bcommercial-provider\b/.test(attributes));
  assert.equal(choices.length, 1, 'Desktop presents one account entry, leaving provider choices to the website');
  assert.match(choices[0][1], /id="commercialLoginStart"/);
  assert.equal(choices[0][2].replace(/<[^>]*>/g, '').trim(), '登入');
  assert.equal([...markup.matchAll(/id="commercialLoginStart"/g)].length, 1);
  assert.doesNotMatch(markup.replace(/<[^>]*>/g, ''), /Google|Email/i);
  assert.doesNotMatch(markup, /id="commercialLoginGoogle"/);
  assert.doesNotMatch(markup, /id="commercial(?:RedeemInput|Redeem|ShowSerial|BackToEmail)"/);
  assert.equal(h.calls.starts.length, 0);
  const clicks = h.button.dispatch('click');
  assert.equal(clicks.length, 1, 'the sole login control has one click handler');
  await Promise.all(clicks);
  assert.equal(h.calls.starts.length, 1);
  assert.deepEqual(h.calls.starts[0], authContract.buildStartRequest());
  assert.deepEqual(Object.keys(h.calls.starts[0]).sort(), ['mode', 'schema']);
  assert.ok(h.calls.startOptions[0].signal instanceof AbortSignal);
  assert.deepEqual(h.calls.urls, [started.verification_uri_complete]);
  assert.equal(h.reopen.dataset.url, started.verification_uri_complete);
  assert.equal(h.reopen.hidden, false);
  assert.equal(h.reopen.disabled, false);
  assert.equal(h.block.hidden, false);
  assert.equal(h.code.textContent, started.user_code);
  assert.match(markup, /id="commercialUserCode"[^>]*aria-labelledby="commercialDeviceCodeLabel"/);
  assert.doesNotMatch(h.message.textContent, /Google|Email/i);
  assert.equal(h.button.querySelector('.commercial-provider-label').textContent, '登入');
  await h.advance(1000);
  assert.equal(await login, true);
});

test('a mandatory guest login has no dismissal or animation-pause control', () => {
  const h = loginHarness({ authenticationRequired: true });
  h.requestAccountLogin();
  const markup = h.createdNodes.find(node => node.innerHTML?.includes('commercialLoginStart'))?.innerHTML;
  assert.ok(markup);
  assert.doesNotMatch(markup, /id="commercialLogin(?:Close|Cancel|Motion)"/);
  assert.doesNotMatch(markup.replace(/<[^>]*>/g, ''), /暫停動畫|播放動畫/);
  const overlay = h.createdNodes.find(node => node.id === 'commercialLoginGate');
  assert.equal(overlay?.dataset.mandatory, 'true');
});

test('Escape and backdrop input cannot dismiss the mandatory guest gate', async () => {
  const h = loginHarness({ authenticationRequired: true });
  let resolved = false;
  h.requestAccountLogin().then(() => { resolved = true; });
  const overlay = h.createdNodes.find(node => node.id === 'commercialLoginGate');
  assert.ok(overlay);
  overlay.dispatch('keydown', { key: 'Escape' });
  overlay.dispatch('click');
  h.closeLoginGate(false);
  await h.flush();
  assert.equal(overlay.isConnected, true);
  assert.equal(resolved, false);
  assert.deepEqual(h.calls.starts, []);
  assert.deepEqual(h.calls.urls, []);
});

test('recreating a removed mandatory gate settles both the previous and current login waiters', async () => {
  const h = loginHarness({ authenticationRequired: true, authPoll: async () => authorizedResponse() });
  let originalSettled = false;
  const original = h.requestAccountLogin();
  original.then(() => { originalSettled = true; });
  const overlay = h.createdNodes.find(node => node.id === 'commercialLoginGate');
  overlay.remove();
  const restored = h.requestAccountLogin();
  await h.beginLogin();
  await h.advance(1000);
  assert.equal(originalSettled, true, 'gate recovery must not orphan the startup authentication wait');
  assert.equal(typeof await original, 'boolean');
  assert.equal(await restored, true);
  assert.equal(h.calls.admissions.length, 1);
});

for (const existing of [false, true]) {
  test(`a guest serial request retains the login entry with ${existing ? 'an existing' : 'a new'} gate`, () => {
    const h = loginHarness({ authenticationRequired: true });
    if (existing) h.requestAccountLogin();
    h.showLoginGate({ serial: true });
    assert.equal(h.nodes.get('#commercialLoginEmailFlow').hidden, false,
      'an unavailable redemption flow must not hide the only login entry');
    const markup = h.createdNodes.find(node => node.innerHTML?.includes('commercialLoginStart'))?.innerHTML;
    assert.doesNotMatch(markup, /id="commercial(?:RedeemInput|Redeem|ShowSerial|BackToEmail)"/);
    assert.deepEqual(h.calls.starts, []);
  });
}

for (const boundary of ['logout', 'authentication_required']) {
  for (const settlement of ['success', 'failure']) {
    test(`old entitlement ${settlement} after ${boundary} cannot restore or stale the logged-out account`, async () => {
      const status = deferred();
      const h = loginHarness({ authenticationRequired: true, authenticated: true, entitlementStatus: () => status.promise });
      const pending = h.refreshStatus();
      assert.equal(h.calls.entitlements, 1);
      if (boundary === 'logout') {
        await h.logoutAccount();
        assert.equal(h.calls.logouts.length, 1, 'logout must be acknowledged by the actual schema parser');
      } else h.emitBus('authentication_required');
      assert.equal(h.publicSnapshot().authenticated, false);
      const snapshot = h.publicSnapshot();
      const admissions = h.calls.admissions.length;
      if (settlement === 'success') status.resolve(authorizedResponse().status);
      else status.reject(new Error('old entitlement connection failed'));
      await pending;
      assert.deepEqual(h.publicSnapshot(), snapshot);
      assert.equal(h.calls.admissions.length, admissions, 'a retired read must not change private admission');
      assert.equal(h.publicSnapshot().projectionStale, false);
    });
  }
}

for (const settlement of ['success', 'failure']) {
  test(`pending auth status ${settlement} after an authentication-required event cannot re-admit the account`, async () => {
    const status = deferred();
    const h = loginHarness({ authenticationRequired: true, authStatus: () => status.promise });
    const pending = h.requireAuthenticatedSession({ prompt: false });
    await h.flush();
    assert.equal(h.calls.statuses, 1);
    h.emitBus('authentication_required');
    if (settlement === 'success') status.resolve(authorizedResponse().status);
    else status.reject(new Error('old status connection failed'));
    assert.equal(await pending, false);
    assert.equal(h.publicSnapshot().authenticated, false);
    assert.equal(h.calls.admissions.some(status => status?.authenticated), false);
    assert.deepEqual(h.calls.urls, []);
    assertCleared(h);
  });
}

for (const settlement of ['failure', 'malformed success']) {
  test(`a stale auth status ${settlement} cannot clear a newer successful login`, async () => {
    const oldStatus = deferred();
    const h = loginHarness({ authenticationRequired: true, authStatus: () => oldStatus.promise,
      authPoll: async () => authorizedResponse() });
    const pending = h.requireAuthenticatedSession({ prompt: false });
    await h.flush();
    assert.equal(h.calls.statuses, 1);
    h.emitBus('authentication_required');
    const login = h.requestAccountLogin();
    await h.beginLogin();
    await h.advance(1000);
    assert.equal(await login, true);
    assert.equal(h.publicSnapshot().authenticated, true);
    const snapshot = h.publicSnapshot();
    const admissions = h.calls.admissions.length;
    if (settlement === 'failure') oldStatus.reject(new Error('retired status request failed'));
    else oldStatus.resolve({ schema: authContract.PRODUCT_AUTH_SCHEMA, state: 'unknown' });
    assert.equal(await pending, false);
    assert.deepEqual(h.publicSnapshot(), snapshot);
    assert.equal(h.calls.admissions.length, admissions);
  });
}

test('an idle guest gate offers no work-cancellation control', () => {
  const h = loginHarness({ authenticationRequired: true, sessionState: { sessionId: 'old-session', busy: false } });
  h.requestAccountLogin();
  const markup = h.createdNodes.find(node => node.innerHTML?.includes('commercialLoginStart'))?.innerHTML;
  assert.doesNotMatch(markup, /id="commercialLoginStop"/);
  assert.deepEqual(h.calls.cancels, []);
});

for (const activeState of ['busy', 'recovering', 'resuming']) {
  test(`an authentication-required gate can stop the existing ${activeState} session once`, async () => {
    const stopped = deferred();
    const h = loginHarness({ authenticationRequired: true, authenticated: true,
      sessionState: { sessionId: 'existing-session', [activeState]: true }, cancel: () => stopped.promise });
    const stop = element();
    const output = element();
    h.nodes.set('#commercialLoginStop', stop);
    h.nodes.set('#commercialLoginStopStatus', output);
    h.emitBus('authentication_required');
    h.requestAccountLogin();
    const markup = h.createdNodes.find(node => node.innerHTML?.includes('commercialLoginStart'))?.innerHTML;
    assert.match(markup, /id="commercialLoginStop"[^>]*>停止目前工作</);
    const pending = stop.dispatch('click');
    assert.deepEqual(h.calls.cancels, ['existing-session']);
    assert.equal(stop.disabled, true);
    await Promise.all(stop.dispatch('click'));
    assert.equal(h.calls.cancels.length, 1);
    stopped.resolve({});
    await Promise.all(pending);
    assert.equal(output.textContent, '已送出停止要求。');
    assert.doesNotMatch(output.textContent, /已停止|已完成/);
    assert.equal(h.publicSnapshot().authenticated, false);
    assert.deepEqual(h.calls.starts, []);
  });
}

test('the stop control cannot cancel a different session selected after the gate opened', async () => {
  const h = loginHarness({ authenticationRequired: true, sessionState: { sessionId: 'existing-session', busy: true } });
  const stop = element();
  h.nodes.set('#commercialLoginStop', stop);
  h.nodes.set('#commercialLoginStopStatus', element());
  h.requestAccountLogin();
  h.state.sessionId = 'replacement-session';
  await Promise.all(stop.dispatch('click'));
  assert.deepEqual(h.calls.cancels, []);
  assert.equal(stop.disabled, false);
});

test('a failed stop request remains retryable without claiming the work stopped', async () => {
  const h = loginHarness({ authenticationRequired: true, sessionState: { sessionId: 'existing-session', busy: true },
    cancel: async () => { throw new Error('cancel unavailable'); } });
  const stop = element();
  const output = element();
  h.nodes.set('#commercialLoginStop', stop);
  h.nodes.set('#commercialLoginStopStatus', output);
  h.requestAccountLogin();
  await Promise.all(stop.dispatch('click'));
  assert.deepEqual(h.calls.cancels, ['existing-session']);
  assert.equal(stop.disabled, false);
  assert.equal(output.textContent, '無法送出停止要求，請重試。');
  assert.equal(h.publicSnapshot().authenticated, false);
});

for (const [name, reply] of [
  ['denied', () => terminalResponse('login_denied')],
  ['expired', () => terminalResponse('login_expired')],
  ['invalid schema', () => ({ schema: authContract.PRODUCT_AUTH_SCHEMA, state: 'unknown' })],
  ['invalid slow_down', () => pendingResponse({ error_code: 'slow_down' })],
  ['network failure', () => { throw new Error('connection lost'); }],
  ['authorization failure', () => { throw Object.assign(new Error('denied'), { status: 403 }); }],
]) {
  test(`terminal poll ${name} clears the code and external URL`, async () => {
    const h = loginHarness({ authPoll: reply });
    await h.beginLogin();
    assertCurrent(h, 'first');
    await h.advance(1000);
    assertCleared(h);
    assert.equal(h.message.dataset.tone, 'error');
    assert.equal(h.calls.admissions.length, 0);
    const pollCount = h.calls.polls.length;
    await h.advance(60_000);
    assert.equal(h.calls.polls.length, pollCount, 'terminal results must stop polling');
  });
}

const loginFailureCases = [
  [409, 'device_limit', '額度已滿（最多 3 台），請先移除不再使用的裝置：Meadow 網站 → 已連接裝置。'],
  [409, 'authorization_replayed', '本次授權已處理，請重新登入。'],
  [410, 'login_expired', '登入碼已過期，請重新開始。'],
  [429, 'rate_limited', '登入要求過於頻繁，請稍後再試。'],
  [503, 'keychain_unavailable', '無法讀取或儲存這台 Mac 的登入憑證（鑰匙圈），請檢查鑰匙圈存取權限後重試。'],
  [502, 'network_unavailable', '無法連線至帳號服務，請確認網路連線後重試。'],
  [502, 'invalid_server_response', '無法確認帳號服務的回應，請稍後重試。'],
  [503, 'auth_unavailable', '目前無法完成帳號驗證，請稍後重試。'],
  [409, 'unrecognized_server_code', '目前無法完成帳號驗證，請稍後重試。'],
  [409, '__proto__', '目前無法完成帳號驗證，請稍後重試。'],
  [410, undefined, '登入碼已過期，請重新開始。'],
  [429, undefined, '登入要求過於頻繁，請稍後再試。'],
];

for (const stage of ['start', 'poll']) {
  for (const [status, code, message] of loginFailureCases) {
    test(`${stage} ${status} ${code ?? 'without code'} shows safe recovery guidance and retires the handoff`, async () => {
      const fail = async () => { throw loginFailure(status, code); };
      const h = loginHarness({
        authenticationRequired: true,
        ...(stage === 'start' ? { authStart: fail } : { authPoll: fail }),
      });
      await h.beginLogin();
      if (stage === 'poll') {
        assertCurrent(h, 'first');
        await h.advance(1000);
      }
      assert.equal(h.message.textContent, message);
      assert.doesNotMatch(h.message.textContent, /RAW_SERVER|SECRET|https?:|<script>/);
      assert.equal(h.message.dataset.tone, 'error');
      assertCleared(h);
      assert.equal(h.management.hidden, code !== 'device_limit');
      assert.equal(h.management.disabled, code !== 'device_limit');
      assert.equal(h.button.disabled, false, 'recovery requires an explicit user retry');
      assert.equal(h.publicSnapshot().authenticated, false);
      assert.deepEqual(h.calls.admissions, []);
      assert.deepEqual(h.calls.fallbackURLs, []);
      assert.equal(h.calls.urls.length, stage === 'start' ? 0 : 1);
      const polls = h.calls.polls.length;
      await h.advance(60_000);
      assert.equal(h.calls.starts.length, 1, 'an error must not automatically retry login');
      assert.equal(h.calls.polls.length, polls, 'terminal errors must stop polling');
      assert.equal(h.timers.size, 0);
    });
  }
}

const deviceManagementURL = 'https://meadow-license-2026.web.app/account/devices';

for (const stage of ['start', 'poll']) {
  test(`device-limit recovery after ${stage} opens only the fixed website on an explicit click`, async () => {
    const opening = deferred();
    const fail = async () => { throw loginFailure(409, 'device_limit'); };
    const h = loginHarness({
      authenticationRequired: true,
      ...(stage === 'start' ? { authStart: fail } : { authPoll: fail }),
      openURL: url => url === deviceManagementURL ? opening.promise : Promise.resolve(),
    });
    h.requestAccountLogin();
    const markup = h.createdNodes.find(node => node.innerHTML?.includes('commercialLoginStart'))?.innerHTML;
    assert.match(markup, /id="commercialLoginDeviceManagement"[^>]*hidden[^>]*disabled[^>]*>移除裝置</);
    await Promise.all(h.management.dispatch('click'));
    assert.deepEqual(h.calls.urls, [], 'a hidden recovery action must not open any website');
    await h.beginLogin();
    if (stage === 'poll') await h.advance(1000);
    assert.equal(h.management.hidden, false);
    assert.equal(h.management.disabled, false);
    const before = h.calls.urls.length;
    h.management.dataset.url = 'https://untrusted.invalid/?token=SECRET';
    const pending = h.management.dispatch('click');
    assert.equal(pending.length, 1);
    assert.equal(h.management.disabled, true);
    assert.deepEqual(h.calls.urls.slice(before), [deviceManagementURL]);
    await Promise.all(h.management.dispatch('click'));
    assert.equal(h.calls.urls.length, before + 1, 'a pending click cannot be duplicated');
    opening.resolve({});
    await Promise.all(pending);
    assert.equal(h.management.disabled, false);
    assert.deepEqual(h.calls.fallbackURLs, []);
    assert.equal(h.publicSnapshot().authenticated, false);
    assert.deepEqual(h.calls.admissions, []);
    assert.equal(h.calls.starts.length, 1, 'opening management must not retry or authorize login');
  });
}

test('device management failure stays retryable without a window.open fallback or raw error', async () => {
  const h = loginHarness({
    authenticationRequired: true,
    authStart: async () => { throw loginFailure(409, 'device_limit'); },
    openURL: async () => { throw loginFailure(401, 'login_required'); },
  });
  h.requestAccountLogin();
  await h.beginLogin();
  await Promise.all(h.management.dispatch('click'));
  assert.deepEqual(h.calls.urls, [deviceManagementURL]);
  assert.deepEqual(h.calls.fallbackURLs, []);
  assert.equal(h.management.hidden, false);
  assert.equal(h.management.disabled, false);
  assert.match(h.message.textContent, /無法開啟裝置管理頁/);
  assert.doesNotMatch(h.message.textContent, /RAW_SERVER|SECRET|https?:|<script>/);
  assert.equal(h.publicSnapshot().authenticated, false);
});

for (const boundary of ['retry', 'close']) {
  test(`${boundary} hides recovery and fences a late device-management open failure`, async () => {
    const opening = deferred();
    let starts = 0;
    const h = loginHarness({
      authStart: async () => {
        if (++starts === 1) throw loginFailure(409, 'device_limit');
        return startResponse('new');
      },
      openURL: url => url === deviceManagementURL ? opening.promise : Promise.resolve(),
    });
    h.requestAccountLogin();
    await h.beginLogin();
    const pending = h.management.dispatch('click');
    assert.equal(pending.length, 1);
    assert.deepEqual(h.calls.urls, [deviceManagementURL]);
    if (boundary === 'retry') await h.beginLogin();
    else h.closeLoginGate();
    const message = h.message.textContent;
    assert.equal(h.management.hidden, true);
    assert.equal(h.management.disabled, true);
    opening.reject(loginFailure(503, 'auth_unavailable'));
    await Promise.all(pending);
    assert.equal(h.message.textContent, message);
    assert.equal(h.management.hidden, true);
    assert.equal(h.management.disabled, true);
    const opens = h.calls.urls.length;
    await Promise.all(h.management.dispatch('click'));
    assert.equal(h.calls.urls.length, opens);
    assert.deepEqual(h.calls.fallbackURLs, []);
    if (boundary === 'retry') assertCurrent(h, 'new');
    else assertCleared(h);
  });
}

test('revealing or replacing a management button cannot create a recovery authorization', async () => {
  const h = loginHarness({ authenticationRequired: true });
  h.requestAccountLogin();
  h.management.hidden = false;
  h.management.disabled = false;
  await Promise.all(h.management.dispatch('click'));
  assert.deepEqual(h.calls.urls, [], 'DOM visibility does not authorize the recovery action');
});

for (const detached of ['disconnected', 'replaced']) {
  test(`a ${detached} recovery button cannot open device management`, async () => {
    const h = loginHarness({ authStart: async () => { throw loginFailure(409, 'device_limit'); } });
    h.requestAccountLogin();
    await h.beginLogin();
    if (detached === 'disconnected') h.management.isConnected = false;
    else h.nodes.set('#commercialLoginDeviceManagement', element());
    await Promise.all(h.management.dispatch('click'));
    assert.deepEqual(h.calls.urls, []);
  });
}

test('local transaction expiry clears the handoff after pending polls', async () => {
  const h = loginHarness({ authStart: async () => startResponse('first', { expires_in: 1 }) });
  await h.beginLogin();
  await h.advance(1000);
  assertCleared(h);
  assert.match(h.message.textContent, /過期/);
});

test('an explicit retry clears the old code immediately, including when start fails', async () => {
  const retry = deferred();
  let starts = 0;
  const h = loginHarness({ authStart: () => ++starts === 1 ? startResponse() : retry.promise });
  await h.beginLogin();
  const next = h.beginLogin();
  assertCleared(h);
  retry.reject(Object.assign(new Error('invalid transaction'), { status: 400 }));
  await next;
  assertCleared(h);
  assert.equal(h.message.dataset.tone, 'error');
  assert.equal(h.button.disabled, false);
  await h.advance(1000);
  assert.equal(h.calls.polls.length, 0, 'the failed retry must not resume the old poll');
});

for (const [name, settle] of [
  ['denial', job => job.resolve(terminalResponse('login_denied'))],
  ['authorization', job => job.resolve(authorizedResponse())],
  ['network failure', job => job.reject(new Error('old connection lost'))],
  ['device limit', job => job.reject(loginFailure(409, 'device_limit'))],
]) {
  test(`late old poll ${name} cannot change the replacement transaction`, async () => {
    const oldPoll = deferred();
    const nextStart = deferred();
    let starts = 0;
    const h = loginHarness({
      authStart: () => ++starts === 1 ? startResponse('old') : nextStart.promise,
      authPoll: () => oldPoll.promise,
    });
    await h.beginLogin();
    await h.advance(1000);
    assert.equal(h.calls.polls.length, 1);
    const next = h.beginLogin();
    const connectingMessage = h.message.textContent;
    settle(oldPoll);
    await h.flush();
    assertCleared(h);
    assert.equal(h.message.textContent, connectingMessage);
    assert.equal(h.button.disabled, true, 'the replacement start is still pending');
    assert.equal(h.calls.admissions.length, 0);
    nextStart.resolve(startResponse('new'));
    await next;
    assertCurrent(h, 'new');
    assert.equal(h.calls.urls.at(-1), startResponse('new').verification_uri_complete);
  });
}

test('an older authStart arriving last cannot replace or reopen the new login', async () => {
  const oldStart = deferred();
  let starts = 0;
  const h = loginHarness({
    authStart: () => ++starts === 1 ? oldStart.promise : startResponse('new'),
  });
  const old = h.beginLogin();
  await h.beginLogin();
  assertCurrent(h, 'new');
  const message = h.message.textContent;
  oldStart.resolve(startResponse('old'));
  await old;
  assertCurrent(h, 'new');
  assert.equal(h.message.textContent, message);
  assert.deepEqual(h.calls.urls, [startResponse('new').verification_uri_complete]);
  await h.advance(1000);
  assert.deepEqual(h.calls.polls.map(request => request.transaction_id), ['transaction-new']);
});

test('closing during authStart cannot open a URL or restore a verification code', async () => {
  const start = deferred();
  const h = loginHarness({ authStart: () => start.promise });
  const attempt = h.beginLogin();
  h.closeLoginGate();
  assertCleared(h);
  start.resolve(startResponse());
  await attempt;
  assertCleared(h);
  assert.deepEqual(h.calls.urls, []);
  assert.deepEqual(h.calls.fallbackURLs, []);
  await h.advance(60_000);
  assert.deepEqual(h.calls.polls, []);
});

test('closing an active handoff rejects a later successful poll', async () => {
  const poll = deferred();
  const h = loginHarness({ authPoll: () => poll.promise });
  await h.beginLogin();
  await h.advance(1000);
  h.closeLoginGate();
  assertCleared(h);
  poll.resolve(authorizedResponse());
  await h.flush();
  assertCleared(h);
  assert.equal(h.calls.admissions.length, 0);
  assert.equal(h.publicSnapshot().state, 'login_required');
});

test('a current successful poll admits its account and clears the completed handoff', async () => {
  const h = loginHarness({ authPoll: async () => authorizedResponse() });
  await h.beginLogin();
  await h.advance(1000);
  assertCleared(h);
  assert.equal(h.calls.admissions.length, 1);
  assert.equal(h.calls.admissions[0].state, 'authorized_general');
  await h.advance(1000);
  assert.equal(h.calls.polls.length, 1);
});

test('an old browser-open completion cannot replace the new login status or poll', async () => {
  const oldOpen = deferred();
  let starts = 0;
  const h = loginHarness({
    authStart: () => startResponse(++starts === 1 ? 'old' : 'new'),
    openURL: url => url.includes('attempt=old') ? oldOpen.promise : Promise.resolve(),
  });
  const old = h.beginLogin();
  await h.flush();
  await h.beginLogin();
  const message = h.message.textContent;
  oldOpen.resolve();
  await old;
  assertCurrent(h, 'new');
  assert.equal(h.message.textContent, message);
  await h.advance(1000);
  assert.deepEqual(h.calls.polls.map(request => request.transaction_id), ['transaction-new']);
});

test('closing during native browser open prevents a late fallback window', async () => {
  const opening = deferred();
  const h = loginHarness({ openURL: () => opening.promise });
  const attempt = h.beginLogin();
  await h.flush();
  assert.equal(h.calls.urls.length, 1);
  h.closeLoginGate();
  opening.reject(new Error('native browser launch failed'));
  await attempt;
  assertCleared(h);
  assert.deepEqual(h.calls.fallbackURLs, [], 'closing cancels the outstanding browser fallback');
  await h.advance(1000);
  assert.deepEqual(h.calls.polls, []);
});

test('restarting login prevents an old native-open failure from opening its fallback', async () => {
  const oldOpen = deferred();
  let starts = 0;
  const h = loginHarness({
    authStart: () => startResponse(++starts === 1 ? 'old' : 'new'),
    openURL: url => url.includes('attempt=old') ? oldOpen.promise : Promise.resolve(),
  });
  const old = h.beginLogin();
  await h.flush();
  await h.beginLogin();
  const message = h.message.textContent;
  oldOpen.reject(new Error('old native browser launch failed'));
  await old;
  assertCurrent(h, 'new');
  assert.equal(h.message.textContent, message);
  assert.deepEqual(h.calls.fallbackURLs, []);
  await h.advance(1000);
  assert.deepEqual(h.calls.polls.map(request => request.transaction_id), ['transaction-new']);
});

test('the current transaction still supports browser fallback after native-open failure', async () => {
  const h = loginHarness({ openURL: async () => { throw new Error('native launch unavailable'); } });
  await h.beginLogin();
  assertCurrent(h, 'first');
  assert.deepEqual(h.calls.fallbackURLs, [startResponse().verification_uri_complete]);
  assert.equal(h.message.dataset.tone, '');
  await h.advance(1000);
  assert.deepEqual(h.calls.polls.map(request => request.transaction_id), ['transaction-first']);
});

test('manual reopen binding cancels its late fallback when the close button is clicked', async () => {
  const manualOpen = deferred();
  let opens = 0;
  const h = loginHarness({ openURL: () => ++opens === 1 ? Promise.resolve() : manualOpen.promise });
  const login = h.requestAccountLogin();
  await h.beginLogin();
  h.reopen.dispatch('click');
  assert.equal(h.calls.urls.length, 2, 'the actual reopen click binding launches the native browser');
  h.nodes.get('#commercialLoginClose').dispatch('click');
  assert.equal(await login, false);
  manualOpen.reject(new Error('manual native open failed after close'));
  await h.flush();
  assertCleared(h);
  assert.deepEqual(h.calls.fallbackURLs, []);
});

test('manual reopen binding cancels its late fallback after a new login starts', async () => {
  const manualOpen = deferred();
  let starts = 0;
  let opens = 0;
  const h = loginHarness({
    authStart: () => startResponse(++starts === 1 ? 'old' : 'new'),
    openURL: () => ++opens === 2 ? manualOpen.promise : Promise.resolve(),
  });
  const login = h.requestAccountLogin();
  await h.beginLogin();
  h.reopen.dispatch('click');
  await h.beginLogin();
  manualOpen.reject(new Error('manual old browser launch failed'));
  await h.flush();
  assertCurrent(h, 'new');
  assert.deepEqual(h.calls.fallbackURLs, []);
  h.closeLoginGate();
  assert.equal(await login, false);
});

test('terminal expiry prevents an in-flight manual reopen from opening a late fallback', async () => {
  const manualOpen = deferred();
  let opens = 0;
  const h = loginHarness({
    authPoll: async () => terminalResponse('login_expired'),
    openURL: () => ++opens === 1 ? Promise.resolve() : manualOpen.promise,
  });
  const login = h.requestAccountLogin();
  await h.beginLogin();
  h.reopen.dispatch('click');
  await h.advance(1000);
  assertCleared(h);
  manualOpen.reject(new Error('manual native open failed after expiry'));
  await h.flush();
  assert.deepEqual(h.calls.fallbackURLs, []);
  h.closeLoginGate();
  assert.equal(await login, false);
});

test('an old start rejection cannot enable buttons while its replacement is pending', async () => {
  const oldStart = deferred();
  const newStart = deferred();
  let starts = 0;
  const h = loginHarness({ authStart: () => ++starts === 1 ? oldStart.promise : newStart.promise });
  const old = h.beginLogin();
  const next = h.beginLogin();
  const message = h.message.textContent;
  oldStart.reject(new Error('old request disconnected'));
  await old;
  assertCleared(h);
  assert.equal(h.message.textContent, message);
  assert.equal(h.button.disabled, true);
  newStart.resolve(startResponse('new'));
  await next;
  assertCurrent(h, 'new');
  assert.equal(h.button.disabled, false);
});

test('a hanging authStart becomes retryable at 25 seconds without automatic retries', async () => {
  const start = deferred();
  const h = loginHarness({ authStart: () => start.promise });
  let settled = false;
  const attempt = h.beginLogin().then(() => { settled = true; });
  await h.flush();
  const signal = h.calls.startOptions[0]?.signal;
  assert.ok(signal instanceof AbortSignal, 'authStart receives a real AbortSignal');
  assert.equal(signal.aborted, false);
  assert.equal(h.calls.starts.length, 1);
  assert.equal(h.button.disabled, true);
  await h.advance(24_999);
  assert.equal(settled, false);
  assert.equal(signal.aborted, false);
  assert.equal(h.button.disabled, true);
  await h.advance(1);
  assert.equal(settled, true, 'the UI deadline settles even when the transport ignores abort');
  await attempt;
  assert.equal(signal.aborted, true);
  assertCleared(h);
  assert.equal(h.button.disabled, false);
  assert.equal(h.button.querySelector('.commercial-provider-label').textContent, '登入');
  assert.equal(h.message.dataset.tone, 'error');
  assert.match(h.message.textContent, /逾時|重試/);
  assert.equal(h.timers.size, 0);
  await h.advance(60_000);
  assert.equal(h.calls.starts.length, 1, 'a timeout must wait for explicit user retry');
  assert.deepEqual(h.calls.urls, []);
  assert.deepEqual(h.calls.polls, []);
});

for (const outcome of ['resolve', 'reject']) {
  test(`a timed-out start's late ${outcome} cannot change a successful explicit retry`, async () => {
    const oldStart = deferred();
    let starts = 0;
    const h = loginHarness({ authStart: () => ++starts === 1 ? oldStart.promise : startResponse('new') });
    let settled = false;
    const old = h.beginLogin().then(() => { settled = true; });
    await h.flush();
    const signal = h.calls.startOptions[0]?.signal;
    await h.advance(25_000);
    assert.equal(settled, true, 'the old attempt must stop waiting at its deadline');
    assert.equal(signal?.aborted, true);
    assertCleared(h);
    await h.beginLogin();
    assertCurrent(h, 'new');
    const message = h.message.textContent;
    if (outcome === 'resolve') oldStart.resolve(startResponse('old'));
    else oldStart.reject(new Error('late old transport rejection'));
    await old;
    await h.flush();
    assertCurrent(h, 'new');
    assert.equal(h.message.textContent, message);
    assert.deepEqual(h.calls.urls, [startResponse('new').verification_uri_complete]);
    assert.equal(h.calls.starts.length, 2);
  });
}

for (const cancellation of ['close', 'retry', 'serial switch']) {
  for (const outcome of ['resolve', 'reject']) {
    test(`${cancellation} aborts start, clears its deadline, and ignores late ${outcome}`, async () => {
      const oldStart = deferred();
      let starts = 0;
      const h = loginHarness({
        authenticated: cancellation === 'serial switch',
        authStart: () => ++starts === 1 ? oldStart.promise : startResponse('new'),
      });
      const login = h.requestAccountLogin();
      const old = h.beginLogin();
      await h.flush();
      const signal = h.calls.startOptions[0]?.signal;
      assert.ok(signal instanceof AbortSignal);
      if (cancellation === 'close') h.nodes.get('#commercialLoginClose').dispatch('click');
      else if (cancellation === 'serial switch') h.showLoginGate({ serial: true });
      else await h.beginLogin();
      await h.flush();
      assert.equal(signal.aborted, true, 'cancellation must reach the request transport');
      assert.equal([...h.timers.values()].some(timer => timer.at === 25_000), false);
      const message = h.message.textContent;
      if (outcome === 'resolve') oldStart.resolve(startResponse('old'));
      else oldStart.reject(new Error('canceled request rejected late'));
      await old;
      await h.flush();
      assert.equal(h.message.textContent, message);
      if (cancellation === 'retry') {
        assertCurrent(h, 'new');
        assert.deepEqual(h.calls.urls, [startResponse('new').verification_uri_complete]);
      } else {
        assertCleared(h);
        assert.equal(h.timers.size, 0);
        assert.deepEqual(h.calls.urls, []);
        assert.deepEqual(h.calls.fallbackURLs, []);
      }
      if (cancellation === 'serial switch') {
        assert.equal(h.nodes.get('#commercialLoginSerialFlow').hidden, false);
        assert.equal(h.nodes.get('#commercialLoginEmailFlow').hidden, true);
        assert.equal(h.button.disabled, false);
      }
      h.closeLoginGate();
      assert.equal(await login, false);
    });
  }
}

test('successful authStart clears its deadline before waiting for browser launch', async () => {
  const opening = deferred();
  const h = loginHarness({ openURL: () => opening.promise });
  const attempt = h.beginLogin();
  await h.flush();
  const signal = h.calls.startOptions[0]?.signal;
  assert.ok(signal instanceof AbortSignal);
  assertCurrent(h, 'first');
  assert.equal(h.timers.size, 0, 'only authStart is bounded by the connection deadline');
  await h.advance(25_000);
  assert.equal(signal.aborted, false);
  assertCurrent(h, 'first');
  assert.notEqual(h.message.dataset.tone, 'error');
  opening.resolve();
  await attempt;
  assert.equal(h.button.disabled, false);
  assert.equal(h.calls.starts.length, 1);
});

test('an immediate start rejection clears its deadline and allows retry', async () => {
  const h = loginHarness({ authStart: async () => { throw new Error('service unavailable'); } });
  await h.beginLogin();
  assertCleared(h);
  assert.equal(h.timers.size, 0);
  assert.equal(h.button.disabled, false);
  const message = h.message.textContent;
  await h.advance(25_000);
  assert.equal(h.message.textContent, message, 'a cleared deadline must not replace the original failure');
  assert.equal(h.calls.starts.length, 1);
});

test('the maximum valid verification code is displayed in full without mutation', async () => {
  const code = '<A&B>'.repeat(12) + '1234';
  assert.equal(code.length, 64);
  const h = loginHarness({ authStart: async () => startResponse('long', { user_code: code }) });
  await h.beginLogin();
  assert.equal(h.code.textContent, code);
  assert.equal(h.code.hidden, false);
  assert.equal(h.block.hidden, false);
  assert.equal(h.code.style.fontSize, undefined, 'the login code must not shrink its text');
});

test('an oversized verification code is rejected instead of silently truncated', async () => {
  const h = loginHarness({ authStart: async () => startResponse('invalid', { user_code: 'A'.repeat(65) }) });
  await h.beginLogin();
  assertCleared(h);
  assert.equal(h.message.dataset.tone, 'error');
  assert.deepEqual(h.calls.urls, []);
});

test('the login code has a visible label and distinct purchase-serial wording', () => {
  assert.match(source, /id="commercialDeviceCode"[^>]*hidden/);
  assert.match(source, /id="commercialDeviceCodeLabel"[^>]*>本次登入驗證碼</);
  assert.match(source, /id="commercialUserCode"[^>]*aria-labelledby="commercialDeviceCodeLabel"/);
  assert.match(source, /id="commercialRedeemInput"/);
});
