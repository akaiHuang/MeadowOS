export const DESKTOP_MEMBERSHIP_SCHEMA = 'meadow.desktop-membership.v2';

const ACCESS_KINDS = new Set(['free_account', 'lifetime']);
const DECISIONS = new Set(['authorized', 'denied']);
const SCOPES = new Set(['new_conversation', 'prompt']);

function exact(value, required, optional = [], label = 'membership object') {
  if (!value || typeof value !== 'object' || Array.isArray(value))
    throw new Error(`${label} must be an object`);
  const allowed = new Set([...required, ...optional]);
  if (Object.keys(value).some((key) => !allowed.has(key)))
    throw new Error(`${label} contains an unknown field`);
  if (required.some((key) => !Object.hasOwn(value, key)))
    throw new Error(`${label} is missing a required field`);
  return value;
}

function text(value, label, min = 1, max = 2048) {
  if (typeof value !== 'string') throw new Error(`${label} must be a string`);
  const result = value.trim();
  if (
    result.length < min ||
    result.length > max ||
    /[\u0000-\u001f\u007f]/.test(result)
  )
    throw new Error(`${label} is invalid`);
  return result;
}

function integer(value, label, minimum, maximum) {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum)
    throw new Error(`${label} is invalid`);
  return value;
}

function date(value, label) {
  const result = text(value, label, 10, 64);
  if (!Number.isFinite(Date.parse(result)))
    throw new Error(`${label} is invalid`);
  return result;
}

function httpsURL(value, label) {
  let parsed;
  try {
    parsed = new URL(text(value, label, 8, 2048));
  } catch {
    throw new Error(`${label} is invalid`);
  }
  if (
    parsed.protocol !== 'https:' ||
    !parsed.hostname ||
    parsed.username ||
    parsed.password ||
    parsed.hash
  )
    throw new Error(`${label} must be HTTPS`);
  return parsed.href;
}

function schema(value) {
  if (value !== DESKTOP_MEMBERSHIP_SCHEMA)
    throw new Error('unsupported membership schema');
  return value;
}

function parseAccess(raw) {
  const value = exact(
    raw,
    [
      'kind',
      'active',
      'trial_total_days',
      'remaining_days',
      'usage_fraction',
      'started_at',
      'expires_at',
    ],
    [],
    'access',
  );
  if (!ACCESS_KINDS.has(value.kind)) throw new Error('access kind is invalid');
  if (typeof value.active !== 'boolean')
    throw new Error('access active is invalid');
  if (
      value.trial_total_days !== null ||
      value.remaining_days !== null ||
      value.usage_fraction !== null ||
      value.started_at !== null ||
      value.expires_at !== null
  ) throw new Error('prompt-based access cannot contain trial limits');
  return Object.freeze({ kind: value.kind, active: value.active });
}

function parsePromptUsage(raw, access) {
  const value = exact(raw, ['unit', 'limit', 'accepted_prompt_count', 'remaining', 'unlimited', 'can_submit'], [], 'prompt usage');
  if (value.unit !== 'accepted_user_prompt' || typeof value.unlimited !== 'boolean' || typeof value.can_submit !== 'boolean')
    throw new Error('prompt usage policy is invalid');
  const used = integer(value.accepted_prompt_count, 'accepted prompt count', 0, Number.MAX_SAFE_INTEGER);
  if (access && (value.unlimited !== (access.kind === 'lifetime') || access.active !== value.can_submit))
    throw new Error('prompt usage access mismatch');
  if (value.unlimited) {
    if (value.limit !== null || value.remaining !== null || !value.can_submit)
      throw new Error('unlimited prompt usage cannot contain limits');
  } else if (value.limit !== 100 || value.remaining !== Math.max(0, 100 - used) || value.can_submit !== (used < 100)) {
    throw new Error('free prompt usage is inconsistent');
  }
  return Object.freeze({
    unit: value.unit, limit: value.limit, acceptedPromptCount: used,
    remaining: value.remaining, unlimited: value.unlimited, canSubmit: value.can_submit,
  });
}

export function parseMembershipUsage(raw) {
  const value = exact(raw, [
    'schema',
    'policy_version',
    'usage',
    'access',
    'legacy_prompt_pool',
    'devices',
    'referral',
    'successful_prompt_count',
    'observed_at',
  ]);
  schema(value.schema);
  if (value.policy_version !== 2) throw new Error('unsupported prompt usage policy');
  const access = parseAccess(value.access);
  const promptUsage = parsePromptUsage(value.usage, access);
  const legacy = exact(
    value.legacy_prompt_pool,
    ['remaining', 'total', 'new_conversation_allowed'],
    [],
    'legacy_prompt_pool',
  );
  const devices = exact(value.devices, ['active', 'maximum'], [], 'devices');
  const referral = exact(
    value.referral,
    ['url', 'earned_days'],
    [],
    'referral',
  );
  const total = integer(legacy.total, 'legacy prompt total', 5, 5);
  const remaining = integer(
    legacy.remaining,
    'legacy prompt remaining',
    0,
    total,
  );
  const maximum = integer(devices.maximum, 'maximum devices', 1, 64);
  const active = integer(devices.active, 'active devices', 0, maximum);
  const earnedDays = integer(
    referral.earned_days,
    'referral earned days',
    0,
    36500,
  );
  if (typeof legacy.new_conversation_allowed !== 'boolean')
    throw new Error('new conversation allowance is invalid');
  if (earnedDays % 7 !== 0)
    throw new Error('referral earned days must use seven-day grants');
  const referralURL =
    referral.url === null ? null : httpsURL(referral.url, 'referral url');
  return Object.freeze({
    schema: value.schema,
    policyVersion: value.policy_version,
    access,
    usage: promptUsage,
    legacyPromptPool: Object.freeze({
      remaining,
      total,
      newConversationAllowed: legacy.new_conversation_allowed,
    }),
    devices: Object.freeze({ active, maximum }),
    referral: Object.freeze({ url: referralURL, earnedDays }),
    successfulPromptCount: integer(
      value.successful_prompt_count,
      'successful prompt count',
      0,
      Number.MAX_SAFE_INTEGER,
    ),
    observedAt: date(value.observed_at, 'observed_at'),
  });
}

export function buildNewConversationAuthorization(conversationID, requestID) {
  return Object.freeze({
    schema: DESKTOP_MEMBERSHIP_SCHEMA,
    request_id: text(requestID, 'request_id', 16, 160),
    conversation_id: text(conversationID, 'conversation_id', 16, 160),
  });
}

export function buildPromptAuthorization(sessionID, requestID) {
  return Object.freeze({
    schema: DESKTOP_MEMBERSHIP_SCHEMA,
    request_id: text(requestID, 'request_id', 16, 160),
    conversation_id: text(sessionID, 'conversation_id', 1, 256),
  });
}

export function parseMembershipAuthorization(
  raw,
  expectedScope,
  expectedRequestID,
  expectedConversationID,
) {
  if (!SCOPES.has(expectedScope))
    throw new Error('authorization scope is invalid');
  const value = exact(
    raw,
    ['schema', 'policy_version', 'usage', 'scope', 'request_id', 'conversation_id', 'decision', 'reason'],
    ['authorization_id', 'expires_at'],
    'authorization',
  );
  schema(value.schema);
  if (value.policy_version !== 2) throw new Error('unsupported authorization policy');
  const promptUsage = parsePromptUsage(value.usage);
  if (
    value.scope !== expectedScope ||
    value.request_id !== expectedRequestID ||
    value.conversation_id !== expectedConversationID
  )
    throw new Error('authorization receipt binding mismatch');
  if (!DECISIONS.has(value.decision))
    throw new Error('authorization decision is invalid');
  const reason = text(value.reason, 'authorization reason', 1, 120);
  if (value.decision === 'denied') {
    if (value.authorization_id !== undefined || value.expires_at !== undefined)
      throw new Error('denied authorization cannot contain a grant');
    return Object.freeze({
      schema: value.schema,
      scope: value.scope,
      requestID: value.request_id,
      conversationID: value.conversation_id,
      decision: value.decision,
      reason,
      policyVersion: value.policy_version,
      usage: promptUsage,
    });
  }
  return Object.freeze({
    schema: value.schema,
    scope: value.scope,
    requestID: value.request_id,
    conversationID: value.conversation_id,
    decision: value.decision,
    reason,
    policyVersion: value.policy_version,
    usage: promptUsage,
    authorizationID: text(value.authorization_id, 'authorization_id', 16, 256),
    expiresAt: date(value.expires_at, 'authorization expires_at'),
  });
}

export function buildPromptConsumption(authorization) {
  if (
    authorization?.scope !== 'prompt' ||
    authorization?.decision !== 'authorized'
  )
    throw new Error('authorized prompt grant required');
  return Object.freeze({
    schema: DESKTOP_MEMBERSHIP_SCHEMA,
    request_id: authorization.requestID,
    authorization_id: authorization.authorizationID,
  });
}

export function buildReferralRequest(requestID) {
  return Object.freeze({
    schema: DESKTOP_MEMBERSHIP_SCHEMA,
    request_id: text(requestID, 'request_id', 16, 160),
  });
}

export function parseReferralShare(raw, expectedRequestID) {
  const value = exact(
    raw,
    ['schema', 'request_id', 'url', 'qr_payload'],
    [],
    'referral share',
  );
  schema(value.schema);
  if (value.request_id !== expectedRequestID)
    throw new Error('referral share binding mismatch');
  const url = httpsURL(value.url, 'referral url');
  if (value.qr_payload !== value.url)
    throw new Error('referral QR binding mismatch');
  return Object.freeze({ url, qrPayload: value.qr_payload });
}

export function parsePromptConsumption(raw, authorization) {
  const value = exact(
    raw,
    ['schema', 'request_id', 'authorization_id', 'status', 'usage'],
    [],
    'consumption',
  );
  schema(value.schema);
  if (
    value.request_id !== authorization.requestID ||
    value.authorization_id !== authorization.authorizationID
  )
    throw new Error('consumption receipt binding mismatch');
  if (value.status !== 'consumed' && value.status !== 'already_consumed' && value.status !== 'sync_pending')
    throw new Error('consumption status is invalid');
  return Object.freeze({
    status: value.status,
    usage: parseMembershipUsage(value.usage),
  });
}
