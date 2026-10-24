import test from 'node:test';
import assert from 'node:assert/strict';
import {
  DESKTOP_MEMBERSHIP_SCHEMA,
  buildNewConversationAuthorization,
  buildPromptAuthorization,
  buildPromptConsumption,
  parseMembershipAuthorization,
  parseMembershipUsage,
  parsePromptConsumption,
} from './membership_contract.js';

const usage = (kind = 'free_account', used = 3) => ({
  schema: DESKTOP_MEMBERSHIP_SCHEMA,
  policy_version: 2,
  usage: { unit: 'accepted_user_prompt', limit: kind === 'lifetime' ? null : 100,
    accepted_prompt_count: used, remaining: kind === 'lifetime' ? null : Math.max(0, 100 - used),
    unlimited: kind === 'lifetime', can_submit: kind === 'lifetime' || used < 100 },
  access: { kind, active: kind === 'lifetime' || used < 100, trial_total_days: null,
    remaining_days: null, usage_fraction: null, started_at: null, expires_at: null },
  legacy_prompt_pool: { remaining: 5, total: 5, new_conversation_allowed: kind === 'lifetime' || used < 100 },
  devices: { active: 1, maximum: 3 },
  referral: { url: null, earned_days: 7 },
  successful_prompt_count: 2,
  observed_at: '2026-08-29T00:00:00Z',
});

test('usage accepts authoritative free100 and lifetime projections, counting submissions instead of successes', () => {
  assert.equal(parseMembershipUsage(usage()).usage.remaining, 97);
  assert.equal(parseMembershipUsage(usage()).usage.acceptedPromptCount, 3);
  assert.equal(parseMembershipUsage(usage()).successfulPromptCount, 2);
  assert.equal(parseMembershipUsage(usage('lifetime')).usage.unlimited, true);
  assert.equal(parseMembershipUsage(usage('free_account', 100)).usage.canSubmit, false);
  assert.equal(parseMembershipUsage(usage('free_account', 103)).usage.remaining, 0);
});

for (const [name, mutate] of [
  ['old schema', v => { v.schema = 'meadow.desktop-membership.v1'; }],
  ['missing policy', v => { delete v.policy_version; }],
  ['missing counter', v => { delete v.usage; }],
  ['noncanonical allowance', v => { v.usage.limit = 101; }],
  ['inconsistent remaining', v => { v.usage.remaining = 100; }],
  ['fractional count', v => { v.usage.accepted_prompt_count = 1.5; }],
  ['forged unlimited', v => { v.usage.unlimited = true; }],
  ['forged submit', v => { v.usage.can_submit = false; }],
  ['unknown field', v => { v.usage.successCount = 3; }],
  ['legacy trial', v => { v.access.kind = 'trial'; }],
]) {
  test(`usage rejects ${name} without inventing a fresh allowance`, () => {
    const value = usage(); mutate(value);
    assert.throws(() => parseMembershipUsage(value));
  });
}

test('usage rejects non-canonical limits and insecure referral URLs', () => {
  assert.throws(() =>
    parseMembershipUsage({
      ...usage(),
      legacy_prompt_pool: { remaining: 6, total: 6 },
    }),
  );
  assert.throws(() =>
    parseMembershipUsage({
      ...usage(),
      referral: { url: 'http://example.test/referral', earned_days: 7 },
    }),
  );
});

test('authorization and consume receipts are request-bound', () => {
  const conversation = 'conversation-1111111111111111';
  const start = buildNewConversationAuthorization(
    conversation,
    '11111111-1111-4111-8111-111111111111',
  );
  assert.throws(() =>
    parseMembershipAuthorization(
      {
        schema: DESKTOP_MEMBERSHIP_SCHEMA, policy_version: 2, usage: usage().usage,
        scope: 'prompt',
        request_id: start.request_id,
        conversation_id: conversation,
        decision: 'denied',
        reason: 'trial_expired',
      },
      'new_conversation',
      start.request_id,
      conversation,
    ),
  );
  const request = buildPromptAuthorization(
    'session-1',
    '22222222-2222-4222-8222-222222222222',
  );
  const grant = parseMembershipAuthorization(
    {
      schema: DESKTOP_MEMBERSHIP_SCHEMA, policy_version: 2, usage: usage('free_account', 100).usage,
      scope: 'prompt',
      request_id: request.request_id,
      conversation_id: 'session-1',
      decision: 'authorized',
      reason: 'free_account',
      authorization_id: 'authorization-000000000002',
      expires_at: '2026-08-29T00:01:00Z',
    },
    'prompt',
    request.request_id,
    'session-1',
  );
  assert.equal(grant.usage.canSubmit, false, 'the 100th accepted grant remains deliverable');
  const consume = buildPromptConsumption(grant);
  assert.equal(
    parsePromptConsumption(
      {
        schema: DESKTOP_MEMBERSHIP_SCHEMA,
        request_id: consume.request_id,
        authorization_id: consume.authorization_id,
        status: 'already_consumed',
        usage: usage(),
      },
      grant,
    ).status,
    'already_consumed',
  );
  assert.throws(() =>
    parsePromptConsumption(
      {
        schema: DESKTOP_MEMBERSHIP_SCHEMA,
        request_id: consume.request_id,
        authorization_id: 'authorization-tampered',
        status: 'consumed',
        usage: usage(),
      },
      grant,
    ),
  );
});

test('background sync receipts retain request binding and authoritative quota', () => {
  const authorization = { requestID: 'request-pending', authorizationID: 'authorization-pending' };
  const receipt = { schema: DESKTOP_MEMBERSHIP_SCHEMA, request_id: authorization.requestID,
    authorization_id: authorization.authorizationID, status: 'sync_pending', usage: usage('free_account', 12) };
  const parsed = parsePromptConsumption(receipt, authorization);
  assert.equal(parsed.status, 'sync_pending');
  assert.equal(parsed.usage.usage.acceptedPromptCount, 12);
  assert.equal(parsed.usage.usage.remaining, 88);
  for (const mutation of [
    { request_id: 'another-request' },
    { authorization_id: 'another-authorization' },
    { status: 'pending' },
    { usage: { ...usage(), usage: { ...usage().usage, remaining: 100 } } },
  ]) assert.throws(() => parsePromptConsumption({ ...receipt, ...mutation }, authorization));
});
