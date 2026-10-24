import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { webcrypto } from 'node:crypto';
import vm from 'node:vm';
import * as auth from './commercial_contract.js';
import * as membership from './membership_contract.js';
import { createPromptGrantWarmup } from './prompt_grant.js';

const source = readFileSync(new URL('./commercial.js', import.meta.url), 'utf8')
  .replace(/^import[\s\S]*?from '[^']+';\n/gm, '').replace(/^export /gm, '');
const quota = (used = 0, pro = false) => ({ unit: 'accepted_user_prompt', limit: pro ? null : 100,
  accepted_prompt_count: used, remaining: pro ? null : Math.max(0, 100 - used), unlimited: pro, can_submit: pro || used < 100 });
function usage(used = 0, pro = false) {
  return { schema: membership.DESKTOP_MEMBERSHIP_SCHEMA, policy_version: 2, usage: quota(used, pro),
    access: { kind: pro ? 'lifetime' : 'free_account', active: pro || used < 100, trial_total_days: null,
      remaining_days: null, usage_fraction: null, started_at: null, expires_at: null },
    legacy_prompt_pool: { remaining: 5, total: 5, new_conversation_allowed: pro || used < 100 },
    devices: { active: 1, maximum: 3 }, referral: { url: null, earned_days: 0 },
    successful_prompt_count: 0, observed_at: '2026-10-11T00:00:00Z' };
}
function status(pro = false) {
  return { schema: auth.PRODUCT_AUTH_SCHEMA, authenticated: true, subject: 'test-principal',
    state: pro ? 'authorized_pro' : 'authorized_general', plan: pro ? 'pro' : 'general',
    capabilities: auth.capabilitiesForPlan(pro ? 'pro' : 'general'),
    lease: { lease_id: 'test-lease', device_id: 'test-device', effective_expires_at: '2099-01-01T00:00:00Z', revocation_epoch: 0 } };
}
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function harness({ used = 0, pro = false, readUsage, authorize, consume, prepare } = {}) {
  const usageNode = { innerHTML: '' };
  const listeners = new Map();
  const calls = { usage: 0, authorize: [], consume: [], urls: [] };
  let clientGeneration = 0, clientAuthenticated = true, clientIdentity = '';
  const grant = request => ({ ...request, schema: membership.DESKTOP_MEMBERSHIP_SCHEMA,
    policy_version: 2, usage: quota(used, pro), scope: 'prompt', decision: 'authorized',
    reason: 'free_account', authorization_id: 'authorization-11111111', expires_at: '2099-01-01T00:00:00Z' });
  const context = { ...auth, ...membership, createPromptGrantWarmup, console, crypto: webcrypto,
    state: {},
    hasClientAccess: () => clientAuthenticated,
    clientAccountGeneration: () => clientGeneration,
    setClientAuthentication(value) {
      const authenticated = value?.authenticated === true;
      const identity = authenticated ? JSON.stringify([value.subject, value.entitlement?.leaseID,
        value.entitlement?.deviceID, value.entitlement?.revocationEpoch]) : '';
      if (!authenticated || !clientAuthenticated || clientIdentity !== identity) clientGeneration++;
      clientAuthenticated = authenticated; clientIdentity = identity;
    },
    daemon: {
      clientCapabilities: async () => ({ protected: false, authenticationRequired: true }),
      authStatus: async () => status(pro),
      preparePromptGrant: async () => prepare ? prepare() : ({ schema: 'meadow.desktop-prompt-grant.v1', ready: true,
        available: 5, expires_at: new Date(Date.now() + 60000).toISOString() }),
      membershipUsage: async () => { calls.usage++; return readUsage ? readUsage() : usage(used, pro); },
      authorizePrompt: async request => { calls.authorize.push(request); return authorize ? authorize(request) : grant(request); },
      consumePrompt: async request => { calls.consume.push(request); return consume ? consume(request) : {
        ...request, status: 'consumed', usage: usage(used, pro) }; },
      openURL: async url => { calls.urls.push(url); },
    },
    bus: { on: (name, listener) => listeners.set(name, listener) },
    document: { querySelector: selector => selector.startsWith('link[') ? {} : selector === '#commercialUsageDetails' ? usageNode : null,
      querySelectorAll: () => [] },
    window: { dispatchEvent() {}, addEventListener() {}, removeEventListener() {} }, CustomEvent: class {},
  };
  const runtime = { ...context, daemonStatus: status(pro) };
  vm.runInNewContext(source + `
    commercial.protectedMode = false; commercial.authenticationRequired = true;
    commercial.capabilitiesReady = true; commercial.status = parseStatus(daemonStatus);
    globalThis.api = { refreshMembershipUsage, authorizePromptDelivery, consumePromptDelivery,
      accountAccessLabel, accountAccessValue, trialPurchaseLabel, renderAccountUsage,
      snapshot: () => ({ usage: commercial.usage, usageState: commercial.usageState }),
      changeIdentity: value => applyStatus(parseStatus({ ...daemonStatus, ...value })),
      expire: () => { commercial.pollGeneration++; applyStatus(null); }, purchaseURL: PURCHASE_URL };
  `, runtime);
  return { ...runtime.api, calls, usageNode, grant, flush: async () => { for (let i = 0; i < 16; i++) await Promise.resolve(); } };
}

test('portable mandatory-account usage comes from the server and displays used and remaining', async () => {
  const h = harness({ used: 31 });
  assert.equal(h.accountAccessValue(), '—');
  assert.doesNotMatch(h.accountAccessLabel(), /100/);
  await h.refreshMembershipUsage();
  assert.equal(h.calls.usage, 1);
  assert.equal(h.accountAccessLabel(), '剩餘 69 / 100 次 · 已送出 31 次');
  assert.equal(h.accountAccessValue(), '69/100');
  h.renderAccountUsage();
  assert.match(h.usageNode.innerHTML, /剩餘 69 \/ 100 次/);
  assert.match(h.usageNode.innerHTML, /31 次/);
  assert.match(h.usageNode.innerHTML, /一次買斷 NT\$100/);
  assert.equal(h.purchaseURL, 'https://meadow-desktop-pilot-20261009.web.app/products/meadow-desktop-pro-once-v1');
});

for (const transition of ['subject', 'lease']) {
  test(`a ${transition} change during warmup cannot retry old admission as the new identity`, async () => {
    const warm = deferred();
    const h = harness({
      prepare: () => warm.promise,
      authorize: () => { throw Object.assign(new Error('waiting for reservation'), { code: 'prompt_grant_pending', status: 503 }); },
    });
    const pending = h.authorizePromptDelivery('session-old', '11111111-1111-4111-8111-111111111111');
    const rejected = assert.rejects(pending);
    await h.flush();
    assert.equal(h.calls.authorize.length, 1);
    h.changeIdentity(transition === 'subject'
      ? { subject: 'new-principal' }
      : { lease: { ...status().lease, lease_id: 'new-lease', revocation_epoch: 1 } });
    warm.resolve({ schema: 'meadow.desktop-prompt-grant.v1', ready: true, available: 5,
      expires_at: new Date(Date.now() + 60000).toISOString() });
    await rejected;
    await h.flush();
    assert.equal(h.calls.authorize.length, 1, 'old admission must not be retried against the new account/lease');
    assert.equal(h.calls.consume.length, 0);
  });
}

for (const missing of ['network', 'schema']) {
  test(`${missing} failure never resets usage to a fabricated 100`, async () => {
    const h = harness({ readUsage: () => { if (missing === 'network') throw new Error('down'); return {}; } });
    await assert.rejects(h.refreshMembershipUsage());
    h.renderAccountUsage();
    assert.equal(h.accountAccessValue(), '—');
    assert.doesNotMatch(h.usageNode.innerHTML, /100|無限|不限/);
    assert.equal(h.snapshot().usage, null);
  });
}

test('portable sends require a request-bound permit even when protected is false; the 100th is deliverable', async () => {
  const h = harness({ used: 100 });
  const requestID = '11111111-1111-4111-8111-111111111111';
  await h.refreshMembershipUsage();
  const permit = await h.authorizePromptDelivery('session-one', requestID);
  assert.equal(h.calls.authorize.length, 1);
  assert.equal(h.calls.authorize[0].request_id, requestID);
  assert.equal(permit.requestID, requestID);
  await h.flush();
  assert.equal(h.snapshot().usage.usage.remaining, 0);
  await h.consumePromptDelivery(permit);
  assert.equal(h.calls.consume.length, 1);
});

test('an uncertain authorization retry uses the supplied identity instead of minting another charge', async () => {
  let first = true;
  const h = harness({ authorize: request => { if (first) { first = false; throw new Error('response lost'); } return h.grant(request); } });
  const requestID = '11111111-1111-4111-8111-111111111111';
  await assert.rejects(h.authorizePromptDelivery('session-one', requestID));
  await h.authorizePromptDelivery('session-one', requestID);
  assert.deepEqual(h.calls.authorize.map(v => v.request_id), [requestID, requestID]);
});

test('a denied 101st submission produces upgrade guidance and no consume', async () => {
  const h = harness({ used: 100, authorize: request => ({ ...request, scope: 'prompt', policy_version: 2,
    usage: quota(100), decision: 'denied', reason: 'free_prompt_limit_reached' }) });
  await assert.rejects(h.authorizePromptDelivery('session-one'), error => /100 次/.test(error.userMessage));
  assert.deepEqual(h.calls.consume, []);
});

test('verified Pro usage is unlimited with API costs separate, while a stale usage response cannot survive logout', async () => {
  const h = harness({ pro: true, used: 15 });
  await h.refreshMembershipUsage();
  assert.equal(h.accountAccessValue(), '∞');
  h.renderAccountUsage();
  assert.match(h.usageNode.innerHTML, /API 費用由供應商計收/);
  const pending = deferred();
  const other = harness({ readUsage: () => pending.promise });
  const read = other.refreshMembershipUsage();
  other.expire(); pending.resolve(usage());
  assert.equal(await read, null);
  assert.equal(other.snapshot().usage, null);
});

test('a late consumption receipt after logout cannot restore usage', async () => {
  const pending = deferred();
  const h = harness({ consume: () => pending.promise });
  const permit = await h.authorizePromptDelivery('session-one');
  const read = h.consumePromptDelivery(permit);
  h.expire();
  pending.resolve({ schema: membership.DESKTOP_MEMBERSHIP_SCHEMA, request_id: permit.requestID,
    authorization_id: permit.authorizationID, status: 'consumed', usage: usage() });
  assert.equal(await read, null);
  assert.equal(h.snapshot().usage, null);
});


test('grant projects account quota locally without an extra usage/device read', async () => {
  const h = harness({ used: 36 });
  await h.refreshMembershipUsage();
  const permit = await h.authorizePromptDelivery('session-one', '11111111-1111-4111-8111-111111111111');
  await h.flush();
  assert.equal(h.calls.usage, 1);
  assert.equal(h.accountAccessLabel(), '剩餘 64 / 100 次 · 已送出 36 次');
  assert.ok(permit.authorizationID);
});

test('receipt failure preserves charged quota and working local account state', async () => {
  const h = harness({ used: 37, consume: () => { throw new Error('receipt network unavailable'); } });
  await h.refreshMembershipUsage();
  const permit = await h.authorizePromptDelivery('session-one', '11111111-1111-4111-8111-111111111111');
  await assert.rejects(h.consumePromptDelivery(permit));
  assert.equal(h.snapshot().usageState, 'ready');
  assert.equal(h.accountAccessLabel(), '剩餘 63 / 100 次 · 已送出 37 次');
  assert.equal(h.calls.authorize.length, 1);
});
