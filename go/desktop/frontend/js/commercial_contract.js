export const PRODUCT_AUTH_SCHEMA = 'meadow.product-auth.v1';
export const CAPABILITY_MODEL_12B = 'model.12b';
export const CAPABILITY_MODEL_27B = 'model.27b';
export const CAPABILITY_MODEL_30B = 'model.30b';
export const CAPABILITY_MODEL_35B = 'model.35b';
export const CAPABILITY_VISION = 'vision';
export const CAPABILITY_AGENT_AUTO = 'agent.auto';
export const CAPABILITY_BROWSER = 'browser.interact';
export const CAPABILITY_DELEGATION = 'delegation';
export const CAPABILITY_COMMERCIAL_USE = 'commercial_use';
export const CAPABILITY_DEVICES_MAX = 'devices.max';

const PLANS = new Set(['guest', 'general', 'pro']);
const STATUS_STATES = new Set([
  'guest',
  'login_required',
  'authorized_general',
  'authorized_pro',
]);
const CAPABILITY_KEYS = Object.freeze([
  CAPABILITY_MODEL_12B,
  CAPABILITY_MODEL_27B,
  CAPABILITY_MODEL_30B,
  CAPABILITY_MODEL_35B,
  CAPABILITY_VISION,
  CAPABILITY_AGENT_AUTO,
  CAPABILITY_BROWSER,
  CAPABILITY_DELEGATION,
  CAPABILITY_COMMERCIAL_USE,
  CAPABILITY_DEVICES_MAX,
]);
const POLL_STATES = new Set([
  'login_pending',
  'authorized_general',
  'authorized_pro',
  'login_denied',
  'login_expired',
]);

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function exactKeys(value, required, optional = []) {
  if (!isObject(value)) throw new Error('product auth object required');
  const allowed = new Set([...required, ...optional]);
  if (Object.keys(value).some((key) => !allowed.has(key)))
    throw new Error('product auth response contains an unknown field');
  if (required.some((key) => !Object.hasOwn(value, key)))
    throw new Error('product auth response is missing a required field');
  return value;
}

function stringValue(value, label, min = 1, max = 2048) {
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

function optionalString(value, label, max = 2048) {
  return value === undefined ? '' : stringValue(value, label, 1, max);
}

function schema(value) {
  if (value !== PRODUCT_AUTH_SCHEMA)
    throw new Error('unsupported product auth schema');
  return value;
}

function httpsURL(value, label) {
  const raw = stringValue(value, label, 8, 2048);
  let parsed;
  try {
    parsed = new URL(raw);
  } catch {
    throw new Error(`${label} is invalid`);
  }
  if (
    parsed.protocol !== 'https:' ||
    !parsed.hostname ||
    parsed.username ||
    parsed.password ||
    parsed.hash
  ) {
    throw new Error(`${label} must be a public HTTPS URL`);
  }
  return parsed.href;
}

function dateValue(value, label) {
  const raw = stringValue(value, label, 10, 64);
  if (!Number.isFinite(Date.parse(raw))) throw new Error(`${label} is invalid`);
  return raw;
}

function canonicalCapabilities(plan) {
  const pro = plan === 'pro';
  return {
    [CAPABILITY_MODEL_12B]: plan === 'general' || pro,
    [CAPABILITY_MODEL_27B]: pro,
    [CAPABILITY_MODEL_30B]: pro,
    [CAPABILITY_MODEL_35B]: pro,
    [CAPABILITY_VISION]: pro,
    [CAPABILITY_AGENT_AUTO]: pro,
    [CAPABILITY_BROWSER]: pro,
    [CAPABILITY_DELEGATION]: pro,
    [CAPABILITY_COMMERCIAL_USE]: pro,
    [CAPABILITY_DEVICES_MAX]: plan === 'general' || pro ? 3 : 0,
  };
}

export function capabilitiesForPlan(plan) {
  if (!PLANS.has(plan)) throw new Error('unsupported authorization plan');
  return Object.freeze(canonicalCapabilities(plan));
}

function parseCapabilities(raw, plan) {
  const value = exactKeys(raw, CAPABILITY_KEYS);
  for (const key of CAPABILITY_KEYS) {
    if (key === CAPABILITY_DEVICES_MAX) {
      if (!Number.isSafeInteger(value[key]) || value[key] < 0)
        throw new Error('devices.max is invalid');
    } else if (typeof value[key] !== 'boolean') {
      throw new Error(`${key} must be boolean`);
    }
  }
  const expected = canonicalCapabilities(plan);
  // The native verifier may still expose an unexpired legacy signed lease.
  // Preserve its actual limit until refresh; accept no other policy drift.
  if (plan === 'general' && value[CAPABILITY_DEVICES_MAX] === 1)
    expected[CAPABILITY_DEVICES_MAX] = 1;
  if (CAPABILITY_KEYS.some((key) => value[key] !== expected[key]))
    throw new Error('capabilities do not match plan');
  return Object.freeze({ ...value });
}

function parseEntitlement(raw) {
  if (raw === undefined) return null;
  const value = exactKeys(raw, [
    'lease_id',
    'device_id',
    'effective_expires_at',
    'revocation_epoch',
  ]);
  if (
    !Number.isSafeInteger(value.revocation_epoch) ||
    value.revocation_epoch < 0
  )
    throw new Error('revocation_epoch is invalid');
  return Object.freeze({
    leaseID: stringValue(value.lease_id, 'lease_id', 1, 256),
    deviceID: stringValue(value.device_id, 'device_id', 1, 256),
    effectiveExpiresAt: dateValue(
      value.effective_expires_at,
      'effective_expires_at',
    ),
    revocationEpoch: value.revocation_epoch,
  });
}

export function parseStatus(raw) {
  const value = exactKeys(
    raw,
    ['schema', 'state', 'plan', 'authenticated', 'capabilities'],
    ['subject', 'access_expires_at', 'lease', 'reason'],
  );
  schema(value.schema);
  if (!STATUS_STATES.has(value.state))
    throw new Error('unsupported authorization state');
  if (!PLANS.has(value.plan)) throw new Error('unsupported authorization plan');
  if (typeof value.authenticated !== 'boolean')
    throw new Error('authenticated must be boolean');
  const capabilities = parseCapabilities(value.capabilities, value.plan);
  const entitlement = parseEntitlement(value.lease);
  const guest = value.state === 'guest' || value.state === 'login_required';
  if (
    guest &&
    (value.plan !== 'guest' || value.authenticated || entitlement !== null)
  )
    throw new Error('invalid guest authorization status');
  if (
    value.state === 'authorized_general' &&
    (value.plan !== 'general' || !value.authenticated || entitlement === null)
  ) {
    throw new Error('invalid General authorization status');
  }
  if (
    value.state === 'authorized_pro' &&
    (value.plan !== 'pro' || !value.authenticated || entitlement === null)
  ) {
    throw new Error('invalid Pro authorization status');
  }
  return Object.freeze({
    schema: value.schema,
    state: value.state,
    plan: value.plan,
    authenticated: value.authenticated,
    capabilities: Object.freeze(capabilities),
    subject: optionalString(value.subject, 'subject', 256),
    accessExpiresAt:
      value.access_expires_at === undefined
        ? ''
        : dateValue(value.access_expires_at, 'access_expires_at'),
    entitlement,
    reason: optionalString(value.reason, 'reason', 256),
  });
}

export function buildStartRequest() {
  return Object.freeze({ schema: PRODUCT_AUTH_SCHEMA, mode: 'browser' });
}

export function parseStart(raw) {
  const value = exactKeys(raw, [
    'schema',
    'state',
    'transaction_id',
    'user_code',
    'verification_uri',
    'verification_uri_complete',
    'expires_in',
    'interval',
  ]);
  schema(value.schema);
  if (value.state !== 'login_pending')
    throw new Error('invalid auth start state');
  if (
    !Number.isInteger(value.expires_in) ||
    value.expires_in < 1 ||
    value.expires_in > 1800
  )
    throw new Error('expires_in is invalid');
  if (
    !Number.isInteger(value.interval) ||
    value.interval < 1 ||
    value.interval > 60
  )
    throw new Error('interval is invalid');
  return Object.freeze({
    schema: value.schema,
    state: value.state,
    transactionID: stringValue(value.transaction_id, 'transaction_id', 8, 160),
    userCode: stringValue(value.user_code, 'user_code', 3, 64),
    verificationURI: httpsURL(value.verification_uri, 'verification_uri'),
    verificationURIComplete: httpsURL(
      value.verification_uri_complete,
      'verification_uri_complete',
    ),
    expiresIn: value.expires_in,
    interval: value.interval,
  });
}

export function buildPollRequest(transactionID) {
  return Object.freeze({
    schema: PRODUCT_AUTH_SCHEMA,
    transaction_id: stringValue(transactionID, 'transaction_id', 8, 160),
  });
}

export function parsePoll(raw) {
  const value = exactKeys(
    raw,
    ['schema', 'state'],
    ['interval', 'error_code', 'status'],
  );
  schema(value.schema);
  if (!POLL_STATES.has(value.state))
    throw new Error('unsupported auth poll state');
  if (value.state === 'login_pending') {
    if (
      value.error_code !== 'authorization_pending' &&
      value.error_code !== 'slow_down'
    )
      throw new Error('invalid pending poll error code');
    if (
      !Number.isInteger(value.interval) ||
      value.interval < 1 ||
      value.interval > 60
    )
      throw new Error('invalid pending poll interval');
    if (value.status !== undefined)
      throw new Error('pending poll cannot include status');
    return Object.freeze({
      schema: value.schema,
      state: value.state,
      interval: value.interval,
      errorCode: value.error_code,
      status: null,
    });
  }
  if (value.state === 'login_denied' || value.state === 'login_expired') {
    const expected =
      value.state === 'login_denied' ? 'access_denied' : 'expired_token';
    if (
      value.error_code !== expected ||
      value.interval !== undefined ||
      value.status !== undefined
    )
      throw new Error('invalid terminal poll response');
    return Object.freeze({
      schema: value.schema,
      state: value.state,
      interval: 0,
      errorCode: value.error_code,
      status: null,
    });
  }
  if (
    value.error_code !== undefined ||
    value.interval !== undefined ||
    value.status === undefined
  )
    throw new Error('invalid authorized poll response');
  const status = parseStatus(value.status);
  if (status.state !== value.state)
    throw new Error('poll and status state mismatch');
  return Object.freeze({
    schema: value.schema,
    state: value.state,
    interval: 0,
    errorCode: '',
    status,
  });
}

export function buildSchemaRequest() {
  return Object.freeze({ schema: PRODUCT_AUTH_SCHEMA });
}

function operationID(prefix) {
  if (!globalThis.crypto?.randomUUID)
    throw new Error('secure operation ID unavailable');
  return `${prefix}-${globalThis.crypto.randomUUID()}`;
}

export function buildRedeemRequest(serial) {
  return Object.freeze({
    schema: PRODUCT_AUTH_SCHEMA,
    serial: stringValue(serial, 'serial', 6, 256),
    request_id: operationID('desktop-request'),
    idempotency_key: operationID('desktop-redeem'),
  });
}

export function parseRedeem(raw) {
  const value = exactKeys(raw, ['schema', 'availability', 'receipt']);
  schema(value.schema);
  const receipt = exactKeys(value.receipt, [
    'request_id',
    'receipt_id',
    'status',
  ]);
  if (receipt.status !== 'accepted')
    throw new Error('redeem receipt was not accepted');
  return Object.freeze({
    schema: value.schema,
    availability: parseStatus(value.availability),
    receipt: Object.freeze({
      requestID: stringValue(receipt.request_id, 'request_id', 1, 160),
      receiptID: stringValue(receipt.receipt_id, 'receipt_id', 1, 160),
      status: receipt.status,
    }),
  });
}

export function parseLogout(raw) {
  const value = exactKeys(raw, ['schema', 'ok']);
  schema(value.schema);
  if (value.ok !== true) throw new Error('logout was not confirmed');
  return Object.freeze({ schema: value.schema, ok: true });
}

function nullableDate(value, label) {
  if (value === null) return null;
  return dateValue(value, label);
}

export function parseDeviceList(raw) {
  const value = exactKeys(raw, [
    'schema',
    'current_device_id',
    'active_devices',
    'maximum_devices',
    'devices',
  ]);
  schema(value.schema);
  if (
    !Number.isSafeInteger(value.active_devices) ||
    value.active_devices < 1 ||
    !Number.isSafeInteger(value.maximum_devices) ||
    value.maximum_devices < 1 ||
    value.maximum_devices > 16 ||
    value.active_devices > value.maximum_devices ||
    !Array.isArray(value.devices) ||
    value.devices.length < 1 ||
    value.devices.length > 50
  ) {
    throw new Error('device list bounds are invalid');
  }
  const currentDeviceID = stringValue(
    value.current_device_id,
    'current_device_id',
    1,
    128,
  );
  const seen = new Set();
  let active = 0;
  let current = 0;
  const devices = value.devices.map((rawDevice) => {
    const device = exactKeys(rawDevice, [
      'id',
      'name',
      'platform',
      'status',
      'current',
      'created_at',
      'last_seen_at',
      'revoked_at',
    ]);
    const id = stringValue(device.id, 'device.id', 1, 128);
    if (seen.has(id)) throw new Error('device list contains duplicates');
    seen.add(id);
    if (device.status !== 'active' && device.status !== 'revoked')
      throw new Error('device status is invalid');
    if (typeof device.current !== 'boolean')
      throw new Error('device current flag is invalid');
    if (device.status === 'active') active += 1;
    if (device.current) {
      current += 1;
      if (id !== currentDeviceID || device.status !== 'active')
        throw new Error('current device is invalid');
    }
    return Object.freeze({
      id,
      name: stringValue(device.name, 'device.name', 1, 100),
      platform: stringValue(device.platform, 'device.platform', 1, 40),
      status: device.status,
      current: device.current,
      createdAt: dateValue(device.created_at, 'device.created_at'),
      lastSeenAt: nullableDate(device.last_seen_at, 'device.last_seen_at'),
      revokedAt: nullableDate(device.revoked_at, 'device.revoked_at'),
    });
  });
  if (active !== value.active_devices || current !== 1)
    throw new Error('device counters are invalid');
  return Object.freeze({
    schema: value.schema,
    currentDeviceID,
    activeDevices: value.active_devices,
    maximumDevices: value.maximum_devices,
    devices: Object.freeze(devices),
  });
}

export function buildDeviceRevokeRequest(deviceID, reason) {
  return Object.freeze({
    schema: PRODUCT_AUTH_SCHEMA,
    device_id: stringValue(deviceID, 'device_id', 1, 128),
    request_id: operationID('desktop-device-request'),
    idempotency_key: operationID('desktop-device-revoke'),
    reason: stringValue(reason, 'reason', 3, 500),
    confirmed: true,
  });
}

export function parseDeviceRevoke(raw) {
  const value = exactKeys(raw, ['schema', 'availability', 'receipt']);
  schema(value.schema);
  const receipt = exactKeys(value.receipt, [
    'request_id',
    'receipt_id',
    'audit_id',
    'device_id',
    'status',
    'changed',
  ]);
  if (receipt.status !== 'revoked' || typeof receipt.changed !== 'boolean') {
    throw new Error('device revoke receipt is invalid');
  }
  return Object.freeze({
    schema: value.schema,
    availability: parseStatus(value.availability),
    receipt: Object.freeze({
      requestID: stringValue(receipt.request_id, 'request_id', 1, 160),
      receiptID: stringValue(receipt.receipt_id, 'receipt_id', 1, 160),
      auditID: stringValue(receipt.audit_id, 'audit_id', 1, 160),
      deviceID: stringValue(receipt.device_id, 'device_id', 1, 128),
      status: receipt.status,
      changed: receipt.changed,
    }),
  });
}
