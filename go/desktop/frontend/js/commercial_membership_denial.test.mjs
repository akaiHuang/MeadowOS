import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { webcrypto } from 'node:crypto';
import vm from 'node:vm';
import * as auth from './commercial_contract.js';
import * as membership from './membership_contract.js';
import { createPromptGrantWarmup } from './prompt_grant.js';

// Exercise the actual commercial admission path and contract parsing. Native
// account, quota and login responses are fixtures; no app or credentials open.
const source = readFileSync(new URL('./commercial.js', import.meta.url), 'utf8')
  .replace(/^import[\s\S]*?from '[^']+';\n/gm, '').replace(/^export /gm, '');
const status = {
  schema: auth.PRODUCT_AUTH_SCHEMA, authenticated: true, subject: 'fixture-account',
  state: 'authorized_general', plan: 'general', capabilities: auth.capabilitiesForPlan('general'),
  lease: { lease_id: 'fixture-lease', device_id: 'fixture-device',
    effective_expires_at: '2099-01-01T00:00:00Z', revocation_epoch: 0 },
};
const quota = { unit: 'accepted_user_prompt', limit: 100, accepted_prompt_count: 62,
  remaining: 38, unlimited: false, can_submit: true };
const usage = {
  schema: membership.DESKTOP_MEMBERSHIP_SCHEMA, policy_version: 2, usage: quota,
  access: { kind: 'free_account', active: true, trial_total_days: null, remaining_days: null,
    usage_fraction: null, started_at: null, expires_at: null },
  legacy_prompt_pool: { remaining: 5, total: 5, new_conversation_allowed: true },
  devices: { active: 1, maximum: 3 }, referral: { url: null, earned_days: 0 },
  successful_prompt_count: 0, observed_at: '2026-10-19T00:00:00Z',
};
function denial(code, http = 403) {
  return Object.assign(new Error('fixture native rejection'), { code, status: http });
}
function harness({ failure = denial('permission_denied'), warmupFailure, usageFailure, deniedReason } = {}) {
  const calls = { login: 0, admission: 0, prepare: 0, consumed: 0, delivered: 0, requests: [] };
  let currentFailure = failure, authenticated = true;
  const context = {
    ...auth, ...membership, createPromptGrantWarmup, console, crypto: webcrypto,
    state: {}, clientAccountGeneration: () => 1, hasClientAccess: () => authenticated,
    setClientAuthentication: value => { authenticated = value?.authenticated === true; },
    document: { querySelector: selector => selector.startsWith('link[') ? {} : null,
      querySelectorAll: () => [] },
    window: { dispatchEvent() {}, addEventListener() {}, removeEventListener() {} },
    CustomEvent: class {}, bus: { on() {} },
    daemon: {
      clientCapabilities: async () => ({ protected: false, authenticationRequired: true }),
      authStatus: async () => status,
      membershipUsage: async () => { if (usageFailure) throw usageFailure; return usage; },
      preparePromptGrant: async () => {
        calls.prepare++;
        if (warmupFailure) throw warmupFailure;
        return { schema: 'meadow.desktop-prompt-grant.v1', ready: true, available: 5,
          expires_at: '2099-01-01T00:00:00Z' };
      },
      authorizePrompt: async request => {
        calls.admission++; calls.requests.push(request);
        if (currentFailure) throw currentFailure;
        if (deniedReason) return { ...request, schema: membership.DESKTOP_MEMBERSHIP_SCHEMA,
          policy_version: 2, scope: 'prompt', decision: 'denied', reason: deniedReason, usage: quota };
        return { ...request, schema: membership.DESKTOP_MEMBERSHIP_SCHEMA,
          policy_version: 2, scope: 'prompt', decision: 'authorized', reason: 'free_account',
          authorization_id: 'fixture-authorization', expires_at: '2099-01-01T00:00:00Z', usage: quota };
      },
      consumePrompt: async () => { calls.consumed++; },
    },
    loginObserved: () => { calls.login++; }, fixtureStatus: status, fixtureUsage: usage,
  };
  vm.runInNewContext(source + `
    commercial.protectedMode = false; commercial.authenticationRequired = true;
    commercial.capabilitiesReady = true; commercial.status = parseStatus(fixtureStatus);
    commercial.usage = parseMembershipUsage(fixtureUsage); commercial.usageState = 'ready';
    showLoginGate = async () => { loginObserved(); return false; };
    globalThis.api = { authorizePromptDelivery, refreshMembershipUsage, publicSnapshot,
      accountAccessLabel, accountAccessValue,
      observed: () => ({ usageState: commercial.usageState, message: commercial.usageMessage,
        accepted: commercial.usage?.usage?.acceptedPromptCount,
        remaining: commercial.usage?.usage?.remaining }) };
  `, context);
  return { ...context.api, calls, allow: () => { currentFailure = null; },
    async send(requestID = 'fixture-prompt-request') {
      const permit = await context.api.authorizePromptDelivery('fixture-session', requestID);
      calls.delivered++;
      return permit;
    }, authenticated: () => authenticated };
}

for (const [code, http] of [['permission_denied', 403], ['failed_precondition', 412], ['', 403]]) {
  test(`commercial ${code || 'unclassified'} HTTP ${http} blocks delivery without requesting login`, async () => {
    const h = harness({ failure: denial(code, http) });
    await assert.rejects(h.send(), error => error.code === code && !/登入/.test(error.userMessage));
    assert.equal(h.calls.login, 0);
    assert.equal(h.calls.admission, 1);
    assert.equal(h.calls.delivered, 0);
    assert.equal(h.calls.consumed, 0);
    assert.equal(h.publicSnapshot().authenticated, true);
    assert.equal(h.authenticated(), true);
    assert.equal(h.observed().accepted, 62);
    assert.equal(h.observed().remaining, 38);
    assert.equal(h.accountAccessLabel(), '額度同步受阻');
    assert.equal(h.accountAccessValue(), '—');
  });
}

test('explicit denied grant reauthentication still requests login without delivering', async () => {
  const h = harness({ failure: null, deniedReason: 'reauth_required' });
  await assert.rejects(h.send(), error => /登入/.test(error.userMessage));
  assert.equal(h.calls.login, 1);
  assert.equal(h.calls.delivered, 0);
  assert.equal(h.calls.consumed, 0);
});

test('ordinary denied quota receipt remains blocked without requesting login', async () => {
  const h = harness({ failure: null, deniedReason: 'free_prompt_limit_reached' });
  await assert.rejects(h.send(), error => /100 次/.test(error.userMessage));
  assert.equal(h.calls.login, 0);
  assert.equal(h.calls.delivered, 0);
  assert.equal(h.calls.consumed, 0);
});

test('historical grant synchronization failure does not retry or deliver the waiting prompt', async () => {
  const h = harness({ failure: denial('prompt_grant_pending', 503), warmupFailure: denial('permission_denied') });
  await assert.rejects(h.send(), { code: 'permission_denied' });
  assert.equal(h.calls.login, 0);
  assert.equal(h.calls.admission, 1);
  assert.equal(h.calls.delivered, 0);
  assert.equal(h.publicSnapshot().authenticated, true);
  assert.equal(h.accountAccessLabel(), '額度同步受阻');
});

test('a quota permission error during usage refresh keeps the verified account and old counter', async () => {
  const h = harness({ usageFailure: denial('permission_denied') });
  await assert.rejects(h.refreshMembershipUsage(), { code: 'permission_denied' });
  assert.equal(h.publicSnapshot().authenticated, true);
  assert.equal(h.calls.login, 0);
  assert.equal(h.observed().accepted, 62);
  assert.equal(h.accountAccessLabel(), '額度同步受阻');
});

for (const code of ['login_required', 'lease_expired', 'device_revoked', 'invalid_refresh_token']) {
  test(`explicit native ${code} denial still requests authentication`, async () => {
    const h = harness({ failure: denial(code, 401) });
    await assert.rejects(h.send(), { code });
    assert.equal(h.calls.login, 1);
    assert.equal(h.calls.delivered, 0);
    assert.equal(h.calls.consumed, 0);
    assert.equal(h.accountAccessLabel(), '需要重新登入');
  });
}

test('recovery still requires a fresh native permit for the same prompt identity', async () => {
  const h = harness();
  const requestID = 'same-prompt-request-0001';
  await assert.rejects(h.send(requestID), { code: 'permission_denied' });
  h.allow();
  const permit = await h.send(requestID);
  assert.equal(permit.requestID, requestID);
  assert.deepEqual(h.calls.requests.map(request => request.request_id), [requestID, requestID]);
  assert.equal(h.calls.admission, 2);
  assert.equal(h.calls.delivered, 1);
  assert.equal(h.calls.consumed, 0);
  assert.equal(h.calls.login, 0);
  assert.equal(h.accountAccessLabel(), '剩餘 38 / 100 次 · 已送出 62 次');
});
