import { daemon, state, bus, setClientAuthentication, hasClientAccess, clientAccountGeneration } from './core.js?v=raven-execution-20261006a';
import { createAsciiScene } from './ascii_onboarding.js';
import { createPromptGrantWarmup } from './prompt_grant.js';
import { getTheme, setTheme } from './theme.js';
import {
  CAPABILITY_AGENT_AUTO,
  CAPABILITY_BROWSER,
  CAPABILITY_MODEL_12B,
  CAPABILITY_DEVICES_MAX,
  buildDeviceRevokeRequest,
  buildPollRequest,
  buildRedeemRequest,
  buildSchemaRequest,
  buildStartRequest,
  parseLogout,
  parseDeviceList,
  parseDeviceRevoke,
  parsePoll,
  parseRedeem,
  parseStart,
  parseStatus,
} from './commercial_contract.js';
import {
  DESKTOP_MEMBERSHIP_SCHEMA,
  buildNewConversationAuthorization,
  buildPromptAuthorization,
  buildPromptConsumption,
  parseMembershipAuthorization,
  parseMembershipUsage,
  parsePromptConsumption,
  buildReferralRequest,
  parseReferralShare,
} from './membership_contract.js';

const PURCHASE_URL = 'https://meadow-desktop-pilot-20261009.web.app/products/meadow-desktop-pro-once-v1';
const DEVICE_MANAGEMENT_URL = 'https://meadow-license-2026.web.app/account/devices';
const PROACTIVE_INTERVAL_MS = 14 * 24 * 60 * 60 * 1000;
const CONTEXTUAL_INTERVAL_MS = 7 * 24 * 60 * 60 * 1000;
const STORAGE_KEY = 'meadow.commercial.prompt-decisions.v3'; // gitleaks:allow -- Fixed localStorage preference namespace, not a credential.
const LOGIN_START_TIMEOUT_MS = 25_000;
const MODEL_TIERS = Object.freeze({
  // The deployed v1 model.12b flag is the base-model seat. Project it only
  // onto the sole canonical model row; historical and unknown rows stay
  // inaccessible even when their old signed capability bit is true.
  'Meadow Coder 35B': CAPABILITY_MODEL_12B,
});
const $ = (selector) => document.querySelector(selector);

const commercial = {
  status: null,
  requestedMode: 'general',
  projectionStale: false,
  signedOut: false,
  logoutPending: false,
  loginPromise: null,
  loginResolve: null,
  loginClose: null,
  loginStart: null,
  deviceManagementGeneration: null,
  pendingSerial: '',
  pollGeneration: 0,
  usageGeneration: 0,
  protectedMode: true,
  authenticationRequired: true,
  capabilitiesReady: false,
  usage: null,
  promptQuota: null,
  usageState: 'idle',
  usageMessage: '',
};

function injectStylesheet() {
  const stylesheets = [
    ['/commercial.css', 'base'],
    ['/css/commercial_experience.css', 'experience'],
    ['/css/login_ascii.css', 'login-ascii'],
  ];
  for (const [href, layer] of stylesheets) {
    if (document.querySelector(`link[data-commercial-styles="${layer}"]`))
      continue;
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = href;
    link.dataset.commercialStyles = layer;
    document.head.insertBefore(
      link,
      document.querySelector('[data-mobile-dialog-styles]'),
    );
  }
}

async function resolveProtectedClient() {
  const clientGeneration = clientAccountGeneration();
  try {
    const capability = await daemon.clientCapabilities();
    commercial.protectedMode = capability.protected;
    commercial.authenticationRequired = capability.authenticationRequired;
    commercial.vmLab = capability.vmLab === true && capability.authenticationRequired === false;
    commercial.capabilitiesReady = true;
  } catch (error) {
    if (clientGeneration !== clientAccountGeneration()) throw error;
    commercial.capabilitiesReady = false;
    commercial.authenticationRequired = true;
    throw error;
  }
  if (!commercial.protectedMode) commercial.requestedMode = 'general';
  return commercial.protectedMode;
}

function accountReady() {
  return commercial.capabilitiesReady &&
    (commercial.authenticationRequired === false ||
      (commercial.status?.authenticated === true && !commercial.signedOut && !commercial.logoutPending &&
       !commercial.projectionStale &&
       ['authorized_general', 'authorized_pro'].includes(commercial.status.state)));
}

function protectedClient() {
  return commercial.protectedMode;
}

function membershipRequired() {
  return protectedClient() || commercial.authenticationRequired !== false;
}

function loadDecisions() {
  try {
    const parsed = JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}');
    return {
      firstLoginShown: parsed.firstLoginShown === true,
      proactiveDecisionAt: Number(parsed.proactiveDecisionAt) || 0,
      contextualDecisionAt:
        parsed.contextualDecisionAt &&
        typeof parsed.contextualDecisionAt === 'object'
          ? parsed.contextualDecisionAt
          : {},
    };
  } catch {
    return {
      firstLoginShown: false,
      proactiveDecisionAt: 0,
      contextualDecisionAt: {},
    };
  }
}

function saveDecisions(value) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(value));
  } catch {
    // Prompt cadence is advisory. Agentd remains authoritative.
  }
}

function escapeHTML(value) {
  return String(value || '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

function hasCapability(capability) {
  return commercial.status?.capabilities?.[capability] === true;
}

function proAvailable() {
  if (!accountReady()) return false;
  if (!protectedClient()) return true;
  if (accountProjection().contradictory) return false;
  return (
    commercial.status?.state === 'authorized_pro' &&
    commercial.status?.plan === 'pro' &&
    hasCapability(CAPABILITY_AGENT_AUTO) &&
    hasCapability(CAPABILITY_BROWSER)
  );
}

function activeMode() {
  return commercial.requestedMode === 'pro' && proAvailable()
    ? 'pro'
    : 'general';
}

function accountProjection() {
  const authenticated = commercial.status?.authenticated === true;
  const plan = authenticated ? commercial.status?.plan || '' : '';
  const accessKind = commercial.usage?.access?.kind || '';
  const contradictory =
    authenticated &&
    accessKind !== '' &&
    ((plan === 'pro' && accessKind !== 'lifetime') ||
      (plan !== 'pro' && accessKind === 'lifetime'));
  return Object.freeze({
    authenticated,
    plan,
    accessKind,
    contradictory,
    isPro:
      authenticated &&
      !contradictory &&
      plan === 'pro' &&
      (accessKind === '' || accessKind === 'lifetime'),
    isTrial:
      authenticated &&
      !contradictory &&
      plan !== 'pro' &&
      accessKind === 'free_account',
  });
}

function publicSnapshot() {
  const projection = accountProjection();
  return Object.freeze({
    state: commercial.status?.state || 'login_required',
    plan: commercial.status?.plan || 'guest',
    mode: activeMode(),
    protected: protectedClient(),
    authenticationRequired: commercial.authenticationRequired,
    authenticated: accountReady(),
    capabilitiesReady: commercial.capabilitiesReady,
    proAllowed: proAvailable(),
    capabilities: Object.freeze({ ...(commercial.status?.capabilities || {}) }),
    projectionStale: commercial.projectionStale,
    projectionConflict: projection.contradictory,
    membershipKind: projection.accessKind,
    reconciledPro: projection.isPro,
    email: commercial.status?.email || '',
  });
}

function membershipSnapshot() {
  return commercial.usage;
}

function emitChange() {
  // An authenticated account page may have opened the legacy serial form.
  // Revocation replaces that form with the mandatory login entry.
  const gate = $('#commercialLoginGate');
  if (commercial.authenticationRequired !== false && !accountReady() &&
      gate?.dataset.mandatory === 'false') {
    closeLoginGate(false, { force: true });
    void showLoginGate();
  }
  renderStatusPill();
  renderModeSwitch();
  renderAccountRow();
  window.dispatchEvent(
    new CustomEvent('commercial:change', { detail: publicSnapshot() }),
  );
}

const promptGrantWarmup = createPromptGrantWarmup({
  identity: () => accountReady() && membershipRequired()
    ? `${commercial.pollGeneration}:${commercial.status?.subject}:${commercial.status?.accessExpiresAt}` : '',
  prepare: () => daemon.preparePromptGrant(),
});
let automaticWarmupIdentity = '';

// A failed verification locks work immediately; it is not proof of logout.
// Recheck only the local admission endpoint, with a bounded retry sequence and
// one five-second check per cooldown while the unavailable UI remains visible.
// Never retry a prompt, extend a lease, or restore a cached account projection.
const ACCOUNT_RECOVERY_DELAYS = [250, 1000, 3000, 10000, 30000];
const ACCOUNT_RECOVERY_COOLDOWN_MS = 30_000;
let accountRecovery = null;
let accountRecoveryCooldown = null;
// Exhausting one bounded retry sequence is not proof that the account expired.
// Foreground/online events and a visible cooldown may recheck only an
// availability failure. None of them can recover a definite auth denial.
let accountRecoveryEligible = false;

function cancelAccountRecovery() {
  if (accountRecoveryCooldown !== null) clearTimeout(accountRecoveryCooldown);
  accountRecoveryCooldown = null;
  const pending = accountRecovery;
  accountRecovery = null;
  if (!pending) return;
  clearTimeout(pending.timer);
  pending.cancelCheck?.();
}

function transientAccountFailure(error) {
  const status = transportStatus(error);
  const code = error?.code || error?.data?.error?.code;
  if (status === 401 || status === 403 ||
      ['login_required', 'lease_expired', 'lease_rejected', 'authorization_revoked',
        'device_revoked', 'activation_revoked', 'invalid_refresh_token',
        'refresh_reuse_detected', 'device_mismatch', 'anti_rollback_rejected',
        'refresh_recovery_unavailable'].includes(code)) return false;
  return status === 0 || status >= 500;
}

function accountRecoveryVisible() {
  return typeof document === 'undefined' || document.visibilityState !== 'hidden';
}

function scheduleAccountRecoveryCooldown() {
  if (!accountRecoveryEligible || accountRecoveryCooldown !== null || !accountRecoveryVisible()) return;
  accountRecoveryCooldown = setTimeout(() => {
    accountRecoveryCooldown = null;
    startAccountRecovery({ singleAttempt: true });
  }, ACCOUNT_RECOVERY_COOLDOWN_MS);
}

function startAccountRecovery({ singleAttempt = false } = {}) {
  if (!accountRecoveryEligible || accountRecovery || commercial.signedOut || commercial.logoutPending || accountReady() ||
      commercial.authenticationRequired === false || !accountRecoveryVisible() || typeof setTimeout !== 'function') return;
  if (accountRecoveryCooldown !== null) clearTimeout(accountRecoveryCooldown);
  accountRecoveryCooldown = null;
  const pending = { generation: commercial.pollGeneration,
    clientGeneration: clientAccountGeneration(), attempt: 0, timer: null };
  accountRecovery = pending;
  const current = () => accountRecovery === pending &&
    pending.generation === commercial.pollGeneration && !commercial.signedOut &&
    pending.clientGeneration === clientAccountGeneration();
  const schedule = () => {
    if (!current()) return;
    if (singleAttempt || pending.attempt >= ACCOUNT_RECOVERY_DELAYS.length) {
      accountRecovery = null;
      scheduleAccountRecoveryCooldown();
      return;
    }
    pending.timer = setTimeout(check, ACCOUNT_RECOVERY_DELAYS[pending.attempt++]);
  };
  const check = async () => {
    if (!current()) return;
    const controller = new AbortController();
    let timeout;
    try {
      const interrupted = new Promise((_, reject) => {
        pending.cancelCheck = () => {
          controller.abort();
          reject(new Error('account verification interrupted'));
        };
        timeout = setTimeout(pending.cancelCheck, 5000);
      });
      const observation = await Promise.race([
        (async () => ({
          capability: commercial.capabilitiesReady ? null : await daemon.clientCapabilities(),
          raw: await daemon.authAdmissionStatus({ signal: controller.signal }),
        }))(), interrupted,
      ]);
      if (!current()) return;
      const status = parseStatus(observation.raw);
      // Native decides authority. Also reject a deadline that passed while
      // the response was in transit; no grace or expiry is invented here.
      if (status.authenticated &&
          !(Date.parse(status.entitlement?.effectiveExpiresAt) > Date.now()))
        throw Object.assign(new Error('account deadline elapsed'), { status: 401 });
      if (observation.capability) {
        commercial.protectedMode = observation.capability.protected;
        commercial.authenticationRequired = observation.capability.authenticationRequired;
        commercial.capabilitiesReady = true;
      }
      accountRecovery = null;
      // Background grant maintenance has its own native owner. Re-warming
      // here could turn a failing warmup into an endless recovery loop.
      applyStatus(status, { warmPromptGrant: false });
      if (accountReady() && commercial.loginPromise) closeLoginGate(true);
    } catch (error) {
      if (!current()) return;
      if (!transientAccountFailure(error)) {
        accountRecovery = null;
        accountRecoveryEligible = false;
        return;
      }
      schedule();
    } finally {
      clearTimeout(timeout);
      pending.cancelCheck = null;
    }
  };
  if (singleAttempt) void check();
  else schedule();
}

function resumeAvailableAccountRecovery() {
  if (!accountRecoveryVisible()) {
    cancelAccountRecovery();
    return;
  }
  startAccountRecovery();
}

function accountReadFailed(error) {
  const retryable = transientAccountFailure(error);
  applyStatus(null);
  accountRecoveryEligible = retryable;
  commercial.projectionStale = retryable;
  emitChange();
  if (retryable) startAccountRecovery();
}

function applyStatus(status, { warmPromptGrant = true } = {}) {
  cancelAccountRecovery();
  accountRecoveryEligible = false;
  const oldIdentity = commercial.status?.subject || commercial.status?.entitlement?.leaseID || '';
  const nextIdentity = status?.subject || status?.entitlement?.leaseID || '';
  if (!status?.authenticated || oldIdentity !== nextIdentity) {
    promptGrantWarmup.invalidate();
    commercial.usageGeneration += 1;
    commercial.usage = null;
    commercial.promptQuota = null;
    commercial.usageState = status?.authenticated ? 'idle' : 'reauth';
    commercial.usageMessage = '';
  }
  setClientAuthentication(status);
  commercial.status = status;
  if (status?.authenticated === true) commercial.signedOut = false;
  commercial.projectionStale = false;
  if (!proAvailable()) commercial.requestedMode = 'general';
  emitChange();
  const warmupIdentity = status?.authenticated ? JSON.stringify([
    status.subject, status.entitlement?.leaseID, status.entitlement?.deviceID,
    status.entitlement?.revocationEpoch,
  ]) : '';
  // Repeated status projections (including automatic shell reactivation)
  // must not restart a failing warmup. Native maintenance and an explicit
  // prompt keep their own reservation paths; this is only an initial UI hint.
  if (warmupIdentity && warmupIdentity !== automaticWarmupIdentity) {
    automaticWarmupIdentity = warmupIdentity;
    if (warmPromptGrant && accountReady()) void promptGrantWarmup.warm().catch(() => {});
  }
  return status;
}

async function refreshStatus({ preserveProjection = true } = {}) {
  if (commercial.logoutPending) return commercial.status;
  const previous = commercial.status;
  const generation = commercial.pollGeneration;
  const clientGeneration = clientAccountGeneration();
  try {
    const status = parseStatus(await daemon.entitlementStatus());
    if (generation !== commercial.pollGeneration || clientGeneration !== clientAccountGeneration()) return commercial.status;
    return applyStatus(status);
  } catch (error) {
    if (generation !== commercial.pollGeneration || clientGeneration !== clientAccountGeneration()) return commercial.status;
    const status = transportStatus(error);
    cancelAccountRecovery();
    accountRecoveryEligible = transientAccountFailure(error);
    setClientAuthentication(null);
    if (preserveProjection && previous && status !== 401 && status !== 403) {
      commercial.status = previous;
      commercial.projectionStale = true;
      emitChange();
      if (accountRecoveryEligible) startAccountRecovery();
      return previous;
    }
    commercial.status = null;
    commercial.requestedMode = 'general';
    commercial.projectionStale = false;
    emitChange();
    if (accountRecoveryEligible) startAccountRecovery();
    throw error;
  }
}

function transportStatus(error) {
  const direct = Number(
    error?.status || error?.statusCode || error?.response?.status,
  );
  if (Number.isInteger(direct) && direct >= 400 && direct <= 599) return direct;
  const match = String(error?.message || '').match(/\b(4\d\d|5\d\d)\b/);
  return match ? Number(match[1]) : 0;
}

function newRequestID() {
  if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID();
  const bytes = new Uint8Array(16);
  globalThis.crypto.getRandomValues(bytes);
  bytes[6] = (bytes[6] & 15) | 64;
  bytes[8] = (bytes[8] & 63) | 128;
  const hex = [...bytes]
    .map((value) => value.toString(16).padStart(2, '0'))
    .join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

const MEMBERSHIP_ACCOUNT_DENIAL_CODES = new Set([
  'login_required', 'lease_expired', 'lease_rejected', 'authorization_revoked',
  'device_revoked', 'activation_revoked', 'invalid_refresh_token',
  'refresh_reuse_detected', 'refresh_recovery_unavailable', 'invalid_device_proof',
  'device_mismatch', 'anti_rollback_rejected', 'unknown_lease_key',
]);

function membershipRequiresLogin(error) {
  const status = transportStatus(error);
  const code = error?.code || error?.data?.error?.code;
  return (status === 401 || status === 403) &&
    MEMBERSHIP_ACCOUNT_DENIAL_CODES.has(code);
}

function membershipFailure(error) {
  const status = transportStatus(error);
  // A commercial ownership/ledger refusal does not revoke the independently
  // verified account. Keep this submission blocked without starting another
  // device login, which cannot repair a historical quota receipt.
  commercial.usageState = membershipRequiresLogin(error)
    ? 'reauth'
    : status >= 400 && status < 500 ? 'sync_error' : 'offline';
  commercial.usageMessage =
    commercial.usageState === 'reauth'
      ? '登入已過期，請重新驗證。'
      : commercial.usageState === 'sync_error'
        ? '額度同步暫時無法完成，請重新整理後再試。'
        : '授權服務目前離線，操作保持鎖定。';
  renderAccountRow();
  window.dispatchEvent(
    new CustomEvent('commercial:usage', {
      detail: { state: commercial.usageState },
    }),
  );
}

async function refreshMembershipUsage() {
  if (!membershipRequired() || !accountReady()) return null;
  const generation = ++commercial.usageGeneration;
  commercial.usageState = 'loading';
  commercial.usageMessage = '正在同步使用情況。';
  renderAccountRow();
  try {
    const usage = parseMembershipUsage(await daemon.membershipUsage());
    if (generation !== commercial.usageGeneration || commercial.signedOut)
      return null;
    commercial.usage = usage;
    commercial.promptQuota = usage.usage;
    const projection = accountProjection();
    commercial.usageState = projection.contradictory ? 'conflict' : 'ready';
    commercial.usageMessage = projection.contradictory
      ? '帳號方案與使用資格不一致；請重新整理或重新登入。'
      : '';
    renderAccountRow();
    window.dispatchEvent(
      new CustomEvent('commercial:usage', { detail: commercial.usage }),
    );
    return commercial.usage;
  } catch (error) {
    if (generation !== commercial.usageGeneration || commercial.signedOut)
      return null;
    membershipFailure(error);
    throw error;
  }
}

function deniedMessage(reason, scope) {
  if (reason === 'blocked' || reason === 'free_prompt_limit_reached' || reason === 'free_prompt_allowance_exhausted')
    return '免費 100 次訊息額度已用完。可一次付 NT$100 升級 Pro，解除 Meadow 訊息次數限制。';
  if (reason === 'trial_expired' && scope === 'new_conversation')
    return '免費訊息額度已用完，可升級 Pro 後繼續。';
  if (reason === 'legacy_prompt_pool_exhausted')
    return '免費訊息額度已用完，可升級 Pro 後繼續。';
  if (reason === 'reauth_required') return '登入已過期，請重新驗證。';
  return scope === 'new_conversation'
    ? '會員授權未允許建立新對話。'
    : '會員用量未允許送出這次訊息。';
}

async function authorize(scope, request, call, allowLoginRetry = true) {
  const authenticated = await requireAuthenticatedSession();
  if (!authenticated) {
    const error = new Error('請先登入 Meadow，再開始使用。');
    error.status = 401;
    error.userMessage = error.message;
    throw error;
  }
  const admissionGeneration = clientAccountGeneration();
  if (!membershipRequired()) return null;
  const generation = commercial.pollGeneration;
  let explicitReauthentication = false;
  try {
    let response;
    try { response = await call(request); }
    catch (error) {
      if (error?.code !== 'prompt_grant_pending') throw error;
      await promptGrantWarmup.warm({ refresh: true });
      if (generation !== commercial.pollGeneration || !accountReady() ||
          !hasClientAccess() || admissionGeneration !== clientAccountGeneration()) throw error;
      // Retry only the same idempotent admission, never the model delivery.
      response = await call(request);
    }
    const grant = parseMembershipAuthorization(
      response,
      scope,
      request.request_id,
      request.conversation_id,
    );
    if (generation !== commercial.pollGeneration || !accountReady() ||
        !hasClientAccess() || admissionGeneration !== clientAccountGeneration())
      throw Object.assign(new Error('登入狀態已變更，請重新送出。'), { userMessage: '登入狀態已變更，請重新送出。' });
    if (scope === 'prompt' && grant.decision === 'authorized') promptGrantWarmup.debit();
    // The grant already contains the authoritative account counter. Project
    // it locally without another usage + device-list network round trip.
    commercial.usageGeneration += 1;
    commercial.promptQuota = grant.usage;
    if (commercial.usage) {
      commercial.usage = Object.freeze({
        ...commercial.usage, usage: grant.usage,
        access: Object.freeze({ kind: grant.usage.unlimited ? 'lifetime' : 'free_account', active: grant.usage.canSubmit }),
      });
    }
    commercial.usageState = 'ready';
    commercial.usageMessage = '';
    emitChange();
    if (grant.decision !== 'authorized') {
      explicitReauthentication = grant.reason === 'reauth_required';
      commercial.usageState =
        explicitReauthentication ? 'reauth' : 'ready';
      commercial.usageMessage = deniedMessage(grant.reason, scope);
      renderAccountRow();
      const error = new Error(grant.reason);
      error.userMessage = commercial.usageMessage;
      throw error;
    }
    return Object.freeze({
      schema: DESKTOP_MEMBERSHIP_SCHEMA,
      requestID: grant.requestID,
      authorizationID: grant.authorizationID,
      conversationID: grant.conversationID,
      expiresAt: grant.expiresAt,
      reason: grant.reason,
      accountGeneration: generation,
    });
  } catch (error) {
    if (generation !== commercial.pollGeneration || !accountReady() ||
        !hasClientAccess() || admissionGeneration !== clientAccountGeneration()) throw error;
    const status = transportStatus(error);
    if (!error.userMessage) {
      membershipFailure(error);
      error.userMessage = commercial.usageMessage;
    }
    // Reauthentication is an interaction boundary, not a dead-end toast. The
    // actual browser/PKCE transaction remains owned by agentd. After it
    // succeeds, make sure a workspace exists and retry this same bound request
    // once. Trial expiry and other commercial denials never enter this branch.
    if (allowLoginRetry && (explicitReauthentication || membershipRequiresLogin(error))) {
      const authenticated = await showLoginGate();
      if (authenticated) {
        await window.MeadowDesktop?.ensureWorkspaceSelected?.();
        return authorize(scope, request, call, false);
      }
    }
    throw error;
  }
}

export async function authorizeNewConversation() {
  const conversationID = newRequestID();
  const request = buildNewConversationAuthorization(
    conversationID,
    newRequestID(),
  );
  return authorize('new_conversation', request, (body) =>
    daemon.authorizeNewConversation(body),
  );
}

export async function authorizePromptDelivery(sessionID, requestID = newRequestID()) {
  const request = buildPromptAuthorization(sessionID, requestID);
  return authorize('prompt', request, (body) => daemon.authorizePrompt(body));
}

export async function consumePromptDelivery(authorization) {
  if (!authorization) return null;
  const generation = commercial.pollGeneration;
  const quotaGeneration = commercial.usageGeneration;
  if (authorization.accountGeneration !== undefined && authorization.accountGeneration !== generation)
    throw new Error('登入狀態已變更，無法同步舊訊息回執。');
  try {
    const grant = {
      scope: 'prompt',
      decision: 'authorized',
      requestID: authorization.requestID,
      authorizationID: authorization.authorizationID,
      conversationID: authorization.conversationID,
    };
    const receipt = parsePromptConsumption(
      await daemon.consumePrompt(buildPromptConsumption(grant)),
      authorization,
    );
    if (generation !== commercial.pollGeneration || !accountReady()) return null;
    if (quotaGeneration !== commercial.usageGeneration) return receipt;
    commercial.usageGeneration += 1;
    commercial.usage = receipt.usage;
    commercial.promptQuota = receipt.usage.usage;
    commercial.usageState = 'ready';
    commercial.usageMessage = receipt.status === 'sync_pending' ? '使用紀錄正在背景同步。' : '';
    void promptGrantWarmup.warm({ refresh: true }).catch(() => {});
    emitChange();
    return receipt;
  } catch (error) {
    if (generation !== commercial.pollGeneration || !accountReady()) throw error;
    // Admission was already charged and persisted by agentd. A receipt sync
    // failure must not lock working local inference or restore the sent draft.
    // Agentd retries its durable accepted bindings; keep the granted counter.
    commercial.usageMessage = '使用紀錄等待背景同步。';
    renderAccountRow();
    throw error;
  }
}

function stableLoginError(kind) {
  const messages = {
    denied: '登入已被拒絕，請重新開始。',
    expired: '登入碼已過期，請重新開始。',
    timeout: '建立登入連線已等待 25 秒，已停止等待。請按登入重試。',
    device_limit: '額度已滿（最多 3 台），請先移除不再使用的裝置：Meadow 網站 → 已連接裝置。',
    authorization_replayed: '本次授權已處理，請重新登入。',
    rate_limited: '登入要求過於頻繁，請稍後再試。',
    keychain_unavailable: '無法讀取或儲存這台 Mac 的登入憑證（鑰匙圈），請檢查鑰匙圈存取權限後重試。',
    network_unavailable: '無法連線至帳號服務，請確認網路連線後重試。',
    invalid_server_response: '無法確認帳號服務的回應，請稍後重試。',
    unavailable: '目前無法完成帳號驗證，請稍後重試。',
    invalid: '目前無法確認帳號狀態，請重新登入。',
  };
  return messages[kind] || messages.unavailable;
}

function loginErrorKind(error) {
  // Only known codes select local copy. Never render server messages, URLs or
  // credential-bearing response data in the login card.
  switch (error?.code) {
    case 'device_limit':
    case 'authorization_replayed':
    case 'rate_limited':
    case 'keychain_unavailable':
    case 'network_unavailable':
    case 'invalid_server_response':
      return error.code;
    case 'login_expired':
      return 'expired';
    default: {
      const status = transportStatus(error);
      if (status === 401 || status === 403) return 'denied';
      if (status === 410) return 'expired';
      if (status === 429) return 'rate_limited';
      if (status === 400 || status === 422) return 'invalid';
      return 'unavailable';
    }
  }
}

function focusable(root) {
  return [
    ...root.querySelectorAll(
      'button:not([disabled]), input:not([disabled]), summary, a[href], [tabindex]:not([tabindex="-1"])',
    ),
  ].filter(
    (node) =>
      !node.hidden &&
      !node.closest('[hidden], [aria-hidden="true"], [inert]') &&
      node.getClientRects().length > 0,
  );
}

function trapDialog(overlay, allowEscape = false) {
  const handler = (event) => {
    if (event.key === 'Escape') {
      if (!allowEscape) event.preventDefault();
      return;
    }
    if (event.key !== 'Tab') return;
    const items = focusable(overlay);
    if (!items.length) return;
    // WebKit may skip buttons during native Tab traversal and leave the
    // WebView before reaching our boundary. Own each dialog Tab step instead.
    const current = items.indexOf(document.activeElement);
    const next = event.shiftKey
      ? (current <= 0 ? items.length - 1 : current - 1)
      : (current < 0 || current === items.length - 1 ? 0 : current + 1);
    event.preventDefault();
    items[next].focus();
  };
  overlay.addEventListener('keydown', handler);
  return () => overlay.removeEventListener('keydown', handler);
}

function createOverlay(label, mandatory = false) {
  const returnFocus = document.activeElement;
  const overlay = document.createElement('div');
  overlay.className = 'commercial-overlay';
  overlay.dataset.mandatory = String(mandatory);
  const dialog = document.createElement('section');
  dialog.className = 'commercial-dialog';
  dialog.setAttribute('role', 'dialog');
  dialog.setAttribute('aria-modal', 'true');
  dialog.setAttribute('aria-label', label);
  overlay.append(dialog);
  document.body.append(overlay);
  const release = trapDialog(overlay, !mandatory);
  return {
    overlay,
    dialog,
    close: () => {
      release();
      overlay.remove();
      if (returnFocus instanceof HTMLElement && returnFocus.isConnected)
        returnFocus.focus();
    },
  };
}

function setLoginMessage(message, tone = '') {
  const node = $('#commercialLoginStatus');
  if (!node) return;
  node.textContent = message;
  node.dataset.tone = tone;
}

function setLoginButtonsBusy(busy) {
  const button = $('#commercialLoginStart');
  if (button) {
    button.disabled = busy;
    button.querySelector('.commercial-provider-label').textContent =
      busy ? '正在連線…' : '登入';
  }
}

function cancelLoginStart() {
  commercial.loginStart?.cancel();
}

async function boundedLoginStart() {
  const controller = new AbortController();
  let rejectInterrupted;
  let settled = false;
  let timer;
  const interrupted = new Promise((_, reject) => { rejectInterrupted = reject; });
  const attempt = {
    cancel(timeout = false) {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      const error = new Error(timeout ? 'Login start timed out' : 'Login start interrupted');
      error.code = timeout ? 'LOGIN_START_TIMEOUT' : 'LOGIN_START_CANCELLED';
      // Settle the local UI even when a native transport ignores abort. An
      // aborted fetch does not prove that the server cancelled its operation.
      rejectInterrupted(error);
      controller.abort();
    },
  };
  commercial.loginStart = attempt;
  timer = setTimeout(() => attempt.cancel(true), LOGIN_START_TIMEOUT_MS);
  try {
    return await Promise.race([
      daemon.authStart(buildStartRequest(), { signal: controller.signal }),
      interrupted,
    ]);
  } finally {
    settled = true;
    clearTimeout(timer);
    if (commercial.loginStart === attempt) commercial.loginStart = null;
  }
}

function clearLoginHandoff() {
  commercial.deviceManagementGeneration = null;
  const management = $('#commercialLoginDeviceManagement');
  if (management) {
    management.hidden = true;
    management.disabled = true;
  }
  const block = $('#commercialDeviceCode');
  if (block) block.hidden = true;
  const code = $('#commercialUserCode');
  if (code) { code.textContent = ''; code.hidden = true; }
  const reopen = $('#commercialLoginOpen');
  if (reopen) {
    reopen.hidden = true;
    reopen.disabled = true;
    delete reopen.dataset.url;
  }
}

function finishLoginAttempt(generation, kind) {
  if (generation !== commercial.pollGeneration) return;
  clearLoginHandoff();
  setLoginMessage(stableLoginError(kind), 'error');
  if (kind === 'device_limit') {
    commercial.deviceManagementGeneration = generation;
    const management = $('#commercialLoginDeviceManagement');
    if (management) {
      management.hidden = false;
      management.disabled = false;
    }
  }
}

async function openLoginDeviceManagement(event) {
  const button = event.currentTarget;
  const generation = commercial.pollGeneration;
  const isCurrent = () => commercial.deviceManagementGeneration === generation &&
    commercial.pollGeneration === generation && button.isConnected &&
    $('#commercialLoginDeviceManagement') === button && !button.hidden;
  if (!isCurrent() || button.disabled) return;
  button.disabled = true;
  try {
    // The native account gate admits this exact recovery URL. Do not use the
    // generic external opener or fall back around a native refusal.
    await daemon.openURL(DEVICE_MANAGEMENT_URL);
  } catch {
    if (isCurrent()) setLoginMessage('無法開啟裝置管理頁，請稍後重試，或到 Meadow 網站的「已連接裝置」查看。', 'error');
  } finally {
    if (isCurrent()) button.disabled = false;
  }
}

function wait(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

async function pollLogin(started, generation) {
  let interval = started.interval;
  const expiresAt = Date.now() + started.expiresIn * 1000;
  while (generation === commercial.pollGeneration && Date.now() < expiresAt) {
    await wait(interval * 1000);
    if (generation !== commercial.pollGeneration) return;
    let result;
    try {
      const response = await daemon.authPoll(
        buildPollRequest(started.transactionID),
      );
      // Closing the login card invalidates this transaction even when the
      // response was already in flight. Never let a late authorization mutate
      // account state or start a workspace behind the user's dismissal.
      if (generation !== commercial.pollGeneration) return;
      result = parsePoll(response);
    } catch (error) {
      if (generation !== commercial.pollGeneration) return;
      finishLoginAttempt(generation, loginErrorKind(error));
      return;
    }
    if (result.state === 'login_pending') {
      if (result.errorCode === 'slow_down' && result.interval <= interval) {
        finishLoginAttempt(generation, 'invalid');
        return;
      }
      interval = result.interval;
      setLoginMessage(
        result.errorCode === 'slow_down'
          ? '驗證仍在等待中，已依服務要求降低查詢頻率。'
          : '等待你在瀏覽器完成驗證。',
      );
      continue;
    }
    if (result.state === 'login_denied') {
      finishLoginAttempt(generation, 'denied');
      return;
    }
    if (result.state === 'login_expired') {
      finishLoginAttempt(generation, 'expired');
      return;
    }
    if (
      result.state === 'authorized_general' ||
      result.state === 'authorized_pro'
    ) {
      clearLoginHandoff();
      applyStatus(result.status);
      void refreshMembershipUsage().catch(() => {});
      if (commercial.pendingSerial) {
        const serial = commercial.pendingSerial;
        commercial.pendingSerial = '';
        setLoginFlow('serial');
        const input = $('#commercialRedeemInput');
        if (input) input.value = serial;
        const success = await redeemSerial(input, $('#commercialRedeemStatus'));
        if (!success || generation !== commercial.pollGeneration) return;
      }
      closeLoginGate(true);
      return;
    }
  }
  finishLoginAttempt(generation, 'expired');
}

async function openExternalURL(url, isCurrent = () => true) {
  const target = String(url || '').trim();
  const permitted = () => isCurrent() && (accountReady() || $('#commercialLoginOpen')?.dataset.url === target);
  if (!target || !permitted()) return false;
  try {
    await daemon.openURL(target);
    return true;
  } catch {
    if (!permitted()) return false;
    try {
      return Boolean(window.open(target, '_blank', 'noopener,noreferrer'));
    } catch {
      return false;
    }
  }
}

function closeLoginGate(result = false, { force = false } = {}) {
  if (!result && !force && commercial.authenticationRequired !== false && !accountReady()) return false;
  commercial.pollGeneration += 1;
  cancelLoginStart();
  clearLoginHandoff();
  setLoginButtonsBusy(false);
  const close = commercial.loginClose;
  const resolve = commercial.loginResolve;
  commercial.loginClose = null;
  commercial.loginResolve = null;
  commercial.loginPromise = null;
  commercial.pendingSerial = '';
  close?.();
  resolve?.(result);
  return true;
}

async function beginLogin() {
  if (commercial.logoutPending) return;
  automaticWarmupIdentity = '';
  accountRecoveryEligible = false;
  cancelAccountRecovery();
  const generation = ++commercial.pollGeneration;
  cancelLoginStart();
  clearLoginHandoff();
  setLoginButtonsBusy(true);
  const code = $('#commercialUserCode');
  const block = $('#commercialDeviceCode');
  const reopen = $('#commercialLoginOpen');
  if (code) code.hidden = true;
  if (reopen) reopen.hidden = true;
  setLoginMessage('正在建立安全登入連線。');
  try {
    const response = await boundedLoginStart();
    if (generation !== commercial.pollGeneration) return;
    const started = parseStart(response);
    if (block) block.hidden = false;
    if (code) {
      code.textContent = started.userCode;
      code.hidden = false;
    }
    if (reopen) {
      reopen.hidden = false;
      reopen.disabled = false;
      reopen.dataset.url = started.verificationURIComplete;
    }
    const opened = await openExternalURL(started.verificationURIComplete, () => generation === commercial.pollGeneration && reopen?.dataset.url === started.verificationURIComplete);
    if (generation !== commercial.pollGeneration) return;
    setLoginMessage(
      opened
        ? '請在瀏覽器完成登入；完成後這裡會自動更新。'
        : '瀏覽器沒有開啟，請按「在瀏覽器開啟」。',
      opened ? '' : 'error',
    );
    void pollLogin(started, generation);
  } catch (error) {
    if (generation !== commercial.pollGeneration) return;
    if (error?.code === 'LOGIN_START_TIMEOUT') {
      finishLoginAttempt(generation, 'timeout');
      return;
    }
    finishLoginAttempt(generation, loginErrorKind(error));
  } finally {
    if (generation === commercial.pollGeneration) setLoginButtonsBusy(false);
  }
}

export function requestAccountLogin() {
  return showLoginGate();
}

function setLoginFlow(flow) {
  if (flow === 'serial' && !accountReady()) flow = 'email';
  if (commercial.loginStart || $('#commercialLoginOpen')?.dataset.url) {
    commercial.pollGeneration += 1;
    cancelLoginStart();
    clearLoginHandoff();
    setLoginButtonsBusy(false);
  }
  const serial = flow === 'serial';
  const emailFlow = $('#commercialLoginEmailFlow');
  const serialFlow = $('#commercialLoginSerialFlow');
  if (emailFlow) emailFlow.hidden = serial;
  if (serialFlow) serialFlow.hidden = !serial;
  setLoginMessage('');
  if (serial) $('#commercialRedeemInput')?.focus();
  else $('#commercialLoginStart')?.focus();
  const redeem = $('#commercialRedeem');
  if (redeem) {
    redeem.textContent = commercial.status?.authenticated
      ? '驗證並啟用這台裝置'
      : '先登入，再驗證並啟用';
  }
}

function showLoginGate(options = {}) {
  if ($('#commercialLoginGate')) {
    if (options.serial) setLoginFlow('serial');
    return commercial.loginPromise;
  }
  // A removed dialog is not a completed login. Retire its pending transport
  // before creating a replacement so late callbacks cannot revive old codes.
  if (commercial.loginPromise) closeLoginGate(false, { force: true });
  const mandatory = commercial.authenticationRequired !== false && !accountReady();
  const serialAvailable = accountReady();
  const stoppingSession = state.sessionId && (state.busy || state.recovering || state.resuming) ? state.sessionId : '';
  commercial.loginPromise = new Promise((resolve) => {
    commercial.loginResolve = resolve;
  });
  const { overlay, dialog, close } = createOverlay('登入 Meadow', mandatory);
  overlay.id = 'commercialLoginGate';
  commercial.loginClose = close;
  dialog.classList.add('commercial-login-card', 'commercial-auth-experience');
  dialog.innerHTML = `
    ${mandatory ? '' : '<button id="commercialLoginClose" class="commercial-close" type="button" aria-label="關閉登入視窗" title="關閉"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" aria-hidden="true"><path d="m6 6 12 12M18 6 6 18"/></svg></button>'}
    <section class="commercial-auth-brand" aria-label="Meadow 帳號特色">
      <div class="commercial-auth-art"><canvas id="commercialLoginAscii" role="img" aria-label="水滴落入盆栽，發芽長成樹的 ASCII 動畫"></canvas></div>
      <div class="commercial-auth-caption">
      <img class="commercial-auth-mark" src="/assets/meadow-wave-mark.svg" alt="Meadow" />
      <p class="commercial-kicker">MEADOW ACCOUNT</p>
      <h2>登入 Meadow，<br>選擇你的模型。</h2>
      <p>登入後可連接自己的模型 API 或本地模型；Meadow Core 可另外安裝。</p>
      <ul class="commercial-feature-list"><li>免費建立帳號</li><li>內建 Browser 與 Agent 工作環境</li><li>模型連線由你選擇</li></ul>
      </div>
    </section>
    <section class="commercial-auth-form">
      <div id="commercialLoginEmailFlow">
        <h2>歡迎使用 Meadow</h2>
        <p class="commercial-form-note">使用前請先登入或建立 Meadow 帳號。</p>
        <button id="commercialLoginStart" class="commercial-primary commercial-wide commercial-provider" type="button"><span class="commercial-provider-label">登入</span></button>
        <section id="commercialDeviceCode" class="commercial-device-code" aria-labelledby="commercialDeviceCodeLabel" hidden>
          <span id="commercialDeviceCodeLabel" class="commercial-device-code-label">本次登入驗證碼</span>
          <output id="commercialUserCode" class="commercial-code" aria-labelledby="commercialDeviceCodeLabel" hidden></output>
          <p class="commercial-device-code-help">請確認瀏覽器顯示相同代碼；這是暫時的裝置登入代碼，並非購買序號。</p>
          <button id="commercialLoginOpen" class="commercial-secondary commercial-wide" type="button" hidden disabled>在瀏覽器開啟</button>
        </section>
      </div>
      ${serialAvailable ? `<div id="commercialLoginSerialFlow" class="commercial-auth-serial" hidden>
        <p class="commercial-kicker">ACTIVATE LICENSE</p><h2>啟用這台裝置</h2>
        <label class="commercial-field" for="commercialRedeemInput"><span>Meadow Code 序號</span><input id="commercialRedeemInput" autocomplete="off" spellcheck="false" placeholder="MEADOW-XXXX-XXXX-XXXX"></label>
        <button id="commercialRedeem" class="commercial-primary commercial-wide" type="button">驗證並啟用這台裝置</button>
        <p class="commercial-form-note">啟用授權會驗證序號與裝置。模型請求中的訊息、附件及所需檔案內容會傳送至你選擇的模型服務。</p>
        <button id="commercialBackToEmail" class="commercial-serial-link" type="button">返回帳號</button>
      </div>` : ''}
      <p id="commercialLoginStatus" class="commercial-live" role="status" aria-live="polite"></p>
      <button id="commercialLoginDeviceManagement" class="commercial-secondary commercial-wide" type="button" hidden disabled>移除裝置</button>
      <p id="commercialRedeemStatus" class="commercial-live" role="status" aria-live="polite"></p>
      ${mandatory && stoppingSession ? '<p id="commercialLoginStopStatus" class="commercial-live" role="status" aria-live="polite"></p>' : ''}
    </section>
    <footer class="commercial-auth-utilities">
      <button id="commercialLoginTheme" type="button" aria-label="切換介面主題"></button>
      <button id="commercialLoginAccount" type="button">帳號狀態</button>
      ${mandatory && stoppingSession ? '<button id="commercialLoginStop" type="button">停止目前工作</button>' : ''}
    </footer>`;
  const art = createAsciiScene($('#commercialLoginAscii'), { scene: 'login', theme: getTheme() });
  const motion = window.matchMedia('(prefers-reduced-motion: reduce)');
  const syncMotion = () => {
    art.setPaused(motion.matches);
  };
  const syncTheme = () => {
    art.setTheme(getTheme());
    $('#commercialLoginTheme').textContent = getTheme() === 'light' ? '切換暗色' : '切換亮色';
  };
  const onMotion = syncMotion;
  window.addEventListener('meadow:themechange', syncTheme);
  motion.addEventListener('change', onMotion);
  $('#commercialLoginTheme').addEventListener('click', () => setTheme(getTheme() === 'light' ? 'dark' : 'light'));
  $('#commercialLoginAccount')?.addEventListener('click', showAccount);
  $('#commercialLoginStop')?.addEventListener('click', async (event) => {
    const button = event.currentTarget;
    const output = $('#commercialLoginStopStatus');
    if (!stoppingSession || state.sessionId !== stoppingSession ||
        !(state.busy || state.recovering || state.resuming) || button.disabled) return;
    button.disabled = true;
    output.textContent = '正在送出停止要求。';
    try {
      await daemon.cancel(stoppingSession);
      if (overlay.isConnected) output.textContent = '已送出停止要求。';
    } catch {
      if (overlay.isConnected) {
        output.textContent = '無法送出停止要求，請重試。';
        button.disabled = false;
      }
    }
  });
  commercial.loginClose = () => {
    window.removeEventListener('meadow:themechange', syncTheme);
    motion.removeEventListener('change', onMotion);
    art.destroy();
    close();
  };
  syncMotion();
  syncTheme();
  art.setActive(true);
  $('#commercialLoginStart')?.addEventListener('click', () => beginLogin());
  $('#commercialLoginDeviceManagement')?.addEventListener('click', openLoginDeviceManagement);
  $('#commercialLoginOpen')?.addEventListener('click', (event) => {
    const generation = commercial.pollGeneration;
    const target = event.currentTarget.dataset.url;
    void openExternalURL(target, () => generation === commercial.pollGeneration && $('#commercialLoginOpen')?.dataset.url === target);
  });
  $('#commercialBackToEmail')?.addEventListener('click', () => {
    closeLoginGate(true);
    showAccount();
  });
  $('#commercialRedeem')?.addEventListener('click', async () => {
    if (!accountReady()) {
      closeLoginGate(false, { force: true });
      void showLoginGate();
      return;
    }
    const success = await redeemSerial(
      $('#commercialRedeemInput'),
      $('#commercialRedeemStatus'),
    );
    if (success) closeLoginGate(true);
  });
  const dismiss = () => closeLoginGate(false);
  $('#commercialLoginClose')?.addEventListener('click', dismiss);
  $('#commercialLoginCancel')?.addEventListener('click', dismiss);
  overlay.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !mandatory) dismiss();
    event.stopPropagation();
  });
  setLoginFlow(options.serial ? 'serial' : 'email');
  // Opening may happen during a parent overlay's close/focus restoration.
  // Wait for that synchronous transition before placing focus in this modal.
  requestAnimationFrame(() => {
    if (!overlay.isConnected || overlay.contains(document.activeElement)) return;
    const target = options.serial ? '#commercialRedeemInput' : '#commercialLoginStart';
    dialog.querySelector(target)?.focus();
  });
  return commercial.loginPromise;
}

function interruptedWorkflow() {
  if (state?.busy || state?.recovering || state?.resuming) return true;
  return [
    ...document.querySelectorAll(
      '#rightPanel.open #diffBody .dl, .permission-card, .user-input-card',
    ),
  ].some((node) => !node.hidden && node.getClientRects().length > 0);
}

function promptKey(capability) {
  return String(capability || CAPABILITY_AGENT_AUTO)
    .replace(/[^a-zA-Z0-9._:-]/g, '')
    .slice(0, 160);
}

function promptAllowed(kind, capability) {
  if (interruptedWorkflow()) return false;
  const decisions = loadDecisions();
  const now = Date.now();
  if (kind === 'proactive')
    return now - decisions.proactiveDecisionAt >= PROACTIVE_INTERVAL_MS;
  return (
    now -
      (Number(decisions.contextualDecisionAt[promptKey(capability)]) || 0) >=
    CONTEXTUAL_INTERVAL_MS
  );
}

function recordDecision(kind, capability) {
  const decisions = loadDecisions();
  if (kind === 'proactive') decisions.proactiveDecisionAt = Date.now();
  else decisions.contextualDecisionAt[promptKey(capability)] = Date.now();
  saveDecisions(decisions);
}

async function redeemSerial(input, output) {
  const serial = String(input.value || '').trim();
  if (!serial) {
    output.textContent = '請輸入授權碼。';
    return false;
  }
  input.disabled = true;
  output.textContent = '正在驗證授權碼。';
  try {
    const request = buildRedeemRequest(serial);
    const redeemed = parseRedeem(await daemon.licenseRedeem(request));
    if (redeemed.receipt.requestID !== request.request_id)
      throw new Error('redeem receipt mismatch');
    applyStatus(redeemed.availability);
    commercial.usage = null;
    commercial.usageState = 'loading';
    void refreshMembershipUsage().catch(() => {});
    output.textContent = proAvailable()
      ? 'Pro 已可使用。模型與工具不會自動切換。'
      : '授權已受理，目前仍為 General。';
    return proAvailable();
  } catch {
    output.textContent = '授權碼尚未驗證成功，既有帳號權限維持不變。';
    return false;
  } finally {
    input.disabled = false;
  }
}

function trialPurchaseLabel() {
  const quota = commercial.promptQuota || commercial.usage?.usage;
  if (!quota || commercial.usageState !== 'ready') return '尚未取得帳號訊息額度';
  return quota.unlimited ? 'Pro · 訊息次數不限' : `剩餘 ${quota.remaining} / ${quota.limit} 次 · 已送出 ${quota.acceptedPromptCount} 次`;
}

function renderPurchaseProjection(root) {
  if (!root) return;
  const label = trialPurchaseLabel();
  for (const node of root.querySelectorAll('[data-trial-projection]')) {
    node.textContent = label;
  }
}

function showStorefrontSheet({ onClose } = {}) {
  if (!accountReady()) { void showLoginGate(); return false; }
  if ($('#commercialStorefront')) return true;
  const {
    overlay,
    dialog,
    close: closeOverlay,
  } = createOverlay('Meadow 帳號與序號商店');
  overlay.id = 'commercialStorefront';
  overlay.classList.add('commercial-sheet-overlay');
  dialog.classList.add('commercial-storefront-sheet');
  const close = () => {
    closeOverlay();
    onClose?.();
  };
  dialog.innerHTML = `
    <header class="commercial-sheet-head"><img src="/assets/meadow-wave-mark.svg" alt=""><span>MEADOW ACCOUNT · LICENSE STORE</span><button class="commercial-close" id="commercialStorefrontClose" type="button" aria-label="關閉序號商店"></button></header>
    <div class="commercial-storefront-grid">
      <section class="commercial-storefront-copy"><p class="commercial-kicker">YOUR MODELS. YOUR WORKSPACE.</p><h2>Pro 一次買斷，訊息次數不限。</h2><p>免費帳號提供 100 次訊息送出。一次付 NT$100 升級 Pro，解除 Meadow 訊息次數限制。模型 API 費用另計；付款前請確認商店條款。</p><div class="commercial-store-proof"><span>自帶模型 API</span><span>Core 可另行安裝</span><span>使用前需登入</span></div><blockquote>「人的工作先留在人身邊；需要連網的地方，清楚標示。」</blockquote></section>
      <section class="commercial-checkout-card"><p class="commercial-kicker">MEADOW CODE PLANS</p><h3>Meadow Desktop Pro</h3><strong>NT$100 · 一次買斷</strong><p>Meadow 訊息次數不限，最多連接 3 台裝置。不包含模型 API 費用、Meadow API 點數或 SI 模組。</p><button id="commercialCheckout" class="commercial-primary commercial-wide" type="button">前往購買 Pro · NT$100</button><p id="commercialCheckoutStatus" class="commercial-live" role="status" aria-live="polite"></p><small>完成購買或啟用後，回到 App 重新整理授權狀態。是否立即生效以帳號服務回傳的簽章投影為準。</small><button id="commercialStorefrontSerial" class="commercial-serial-link" type="button">已經有序號？回 App 啟用</button></section>
    </div>`;
  $('#commercialStorefrontClose')?.addEventListener('click', close);
  $('#commercialCheckout')?.addEventListener('click', async () => {
    const output = $('#commercialCheckoutStatus');
    output.textContent = '正在開啟 Pro 商品頁；付款前可確認價格與條款。';
    const opened = await openExternalURL(PURCHASE_URL);
    if (!opened) output.textContent = '目前無法開啟安全商店，請稍後再試。';
  });
  $('#commercialStorefrontSerial')?.addEventListener('click', () => {
    closeOverlay();
    void showLoginGate({ serial: true });
  });
  overlay.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') close();
  });
  $('#commercialCheckout')?.focus();
  return true;
}

function showUpgrade(kind = 'contextual', capability = CAPABILITY_AGENT_AUTO) {
  if (!accountReady()) { void showLoginGate(); return false; }
  if (kind !== 'manual' && !promptAllowed(kind, capability)) return false;
  if ($('#commercialUpgrade')) return true;
  const { overlay, dialog, close } = createOverlay('Meadow 方案與模型連線');
  overlay.id = 'commercialUpgrade';
  overlay.classList.add('commercial-sheet-overlay');
  dialog.classList.add('commercial-purchase-sheet');
  const decide = () => recordDecision(kind, capability);
  const dismiss = () => {
    decide();
    close();
  };
  dialog.innerHTML = `
    <header class="commercial-sheet-head"><img src="/assets/meadow-wave-mark.svg" alt=""><span>MEADOW CODE PRO</span><button id="commercialUpgradeClose" class="commercial-close" type="button" aria-label="關閉購買頁"></button></header>
    <div class="commercial-purchase-scroll">
      <section class="commercial-purchase-hero"><div><p class="commercial-kicker">YOUR DESKTOP. YOUR MODEL.</p><h2>在自己的工作區，連接適合的模型。</h2><p>Meadow Desktop 內建 Browser 與 Harness。登入後可連接外部 API、既有本地 API，或另外安裝 Meadow Core。</p><div class="commercial-proof-strip"><span>自帶模型 API</span><span>本地或遠端 Core</span><span>模型連線由你選擇</span></div><button id="commercialBuy" class="commercial-purchase-cta" type="button">Pro 一次買斷 NT$100 <b>→</b></button></div><img src="/assets/onboarding-local-agent-hero.png" alt="Meadow Code 本機工作情境"></section>
      <section class="commercial-outcome-flow"><p class="commercial-kicker">90 秒看懂 MEADOW</p><h3>不是回答你怎麼做，而是把工作做完。</h3><div><article><small>01 / 說出目標</small><b>「做一個可玩的貪食蛇」</b></article><i>→</i><article><small>02 / 寫進檔案</small><b>右側即時看到程式</b></article><i>→</i><article><small>03 / 驗證修正</small><b>實際可玩才完成</b></article></div></section>
      <section class="commercial-license-grid"><article><p class="commercial-kicker">目前方案</p><h3>免費帳號</h3><strong data-trial-projection>${escapeHTML(trialPurchaseLabel())}</strong><ul><li>共 100 次訊息送出額度</li><li>可連接自己的 API 或本地模型</li><li>模型 API 費用由供應商計收</li></ul><button id="commercialContinue" type="button">返回工作區</button></article><article class="commercial-pro-plan"><p class="commercial-kicker">正式方案</p><h3>Meadow Desktop Pro</h3><strong>NT$100 · 一次買斷</strong><ul><li>Meadow 訊息次數不限</li><li>最多連接 3 台裝置</li><li>模型 API 費用、API 點數與 SI 模組另計</li></ul><button id="commercialPlanBuy" type="button">購買 Pro</button></article></section>
      <section class="commercial-scenario-section"><p class="commercial-kicker">REAL WORK, THREE WAYS</p><h3>不是功能清單，是三種每天會發生的工作。</h3><div><article><small>BUILD</small><b>從一句話建立可用網頁</b><p>直接寫檔、顯示差異、在右側完成互動驗收。</p></article><article><small>FIX</small><b>讀取既有專案並修正</b><p>保留上下文、做最小修改，驗證通過才宣稱完成。</p></article><article><small>RESEARCH</small><b>整理資料與可追溯來源</b><p>把來源、方法與交付物放在同一個本機工作區。</p></article></div></section>
      <section class="commercial-truth-table"><div><p class="commercial-kicker">資料邊界</p><h3>什麼留在本機，什麼需要連網。</h3></div><dl><div><dt>儲存在裝置</dt><dd>專案檔案與本機對話歷史</dd></div><div><dt>模型請求</dt><dd>訊息、附件與任務需要的檔案內容會傳送至所選模型服務；外部 API 依供應商政策處理。</dd></div><div><dt>選用 Core</dt><dd>Meadow Core 可另行安裝，推論在該主機執行；連接遠端 Core 時，請求會傳送到該主機。</dd></div><div><dt>需要連網</dt><dd>帳號登入、授權驗證、軟體更新，以及使用中的外部模型與網路工具。</dd></div></dl></section>
      <section class="commercial-buying-details"><div><p class="commercial-kicker">BEFORE YOU BUY</p><h3>售價之外，這些條件必須一次講清楚。</h3></div><dl><div><dt>相容性</dt><dd>下載版本支援的作業系統；另裝 Core 時需確認 GPU、記憶體與磁碟需求。</dd></div><div><dt>授權</dt><dd>可用裝置數、換機、解除綁定與授權復原方式。</dd></div><div><dt>更新與退款</dt><dd>更新期限、重大版本政策與退款條件由商店列明。</dd></div><div><dt>交付</dt><dd>付款、交付與生效方式由安全商店列明；App 只採用帳號服務回傳的簽章授權投影。</dd></div></dl></section>
      <section class="commercial-purchase-faq"><p class="commercial-kicker">FAQ</p><h3>購買前最常問的四件事。</h3><details open><summary>Meadow 會把我的程式碼上傳嗎？</summary><p>專案檔案與對話歷史儲存在裝置；模型請求會把訊息、附件與任務需要的檔案內容傳送至你選擇的服務。使用本機 Core 時，推論在本機執行；遠端 Core 與外部 API 則會收到請求。</p></details><details><summary>連接自己的 API 需要付費會員嗎？</summary><p>登入免費帳號即可連接自己的 API 或本地模型，並有 100 次訊息送出額度。用完後可一次付 NT$100 升級 Pro，解除 Meadow 訊息次數限制；外部模型費用由供應商計收。</p></details><details><summary>更換裝置怎麼辦？</summary><p>帳號與授權頁提供已綁定裝置，遠端裝置須經確認才能撤銷。</p></details><details><summary>還需要另外付模型 Token 嗎？</summary><p>使用自有 API 的費用由模型供應商計收；自架 Core 的設備與運算成本由你負擔。Meadow 方案包含的用量及價格以正式條款為準。</p></details><button id="commercialBuyBottom" class="commercial-purchase-cta" type="button">Pro 一次買斷 NT$100 · 查看條款 <b>→</b></button></section>
      <section class="commercial-inline-redeem"><div><p class="commercial-kicker">ALREADY PURCHASED?</p><h3>已有序號，直接啟用這台裝置。</h3></div><div><input id="commercialRedeemInput" autocomplete="off" spellcheck="false" placeholder="MEADOW-XXXX-XXXX-XXXX"><button id="commercialRedeem" type="button">驗證並啟用</button></div><p id="commercialRedeemStatus" class="commercial-live" role="status" aria-live="polite"></p></section>
    </div>`;
  const openStorefront = () => {
    decide();
    close();
    showStorefrontSheet();
  };
  $('#commercialUpgradeClose')?.addEventListener('click', dismiss);
  $('#commercialContinue')?.addEventListener('click', () => {
    commercial.requestedMode = 'general';
    emitChange();
    dismiss();
  });
  for (const id of [
    '#commercialBuy',
    '#commercialPlanBuy',
    '#commercialBuyBottom',
  ]) {
    $(id)?.addEventListener('click', openStorefront);
  }
  $('#commercialRedeem')?.addEventListener('click', async () => {
    const success = await redeemSerial(
      $('#commercialRedeemInput'),
      $('#commercialRedeemStatus'),
    );
    decide();
    if (success) close();
  });
  overlay.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') dismiss();
  });
  renderPurchaseProjection(dialog);
  if (!commercial.usage && commercial.status?.authenticated) {
    void refreshMembershipUsage()
      .then(() => renderPurchaseProjection(dialog))
      .catch(() => renderPurchaseProjection(dialog));
  }
  $('#commercialBuy')?.focus();
  return true;
}

function ensureModeSwitch() {
  document
    .querySelectorAll('[data-commercial-mode-switch="footer"]')
    .forEach((root) => root.remove());
  const pill = $('#licensePill');
  if (pill) pill.hidden = true;
}

function renderModeSwitch() {
  for (const root of document.querySelectorAll(
    '[data-commercial-mode-switch]',
  )) {
    for (const button of root.querySelectorAll('[data-commercial-mode]')) {
      const active = button.dataset.commercialMode === activeMode();
      button.setAttribute('aria-pressed', String(active));
      button.classList.toggle('active', active);
    }
  }
}

function statusPresentation() {
  if (commercial.vmLab) return ['VM 測試版', '免 Meadow 登入；使用自己的 API'];
  if (commercial.signedOut) return ['需要登入', '已登出；本機資料保留'];
  if (!commercial.status) return ['需要登入', '尚未登入'];
  // The standard Desktop does not capability-gate local features, so
  // proAvailable() intentionally returns true there. Account chrome must use
  // the canonical auth projection instead; otherwise a signed-out standard
  // build misleadingly labels the guest account as "Pro".
  if (commercial.status.authenticated !== true) return ['需要登入', '尚未登入'];
  const projection = accountProjection();
  if (projection.contradictory)
    return ['需要重新整理', '帳號方案與使用資格不一致'];
  if (projection.isPro) {
    if (commercial.status.reason === 'offline_grace')
      return [
        'Pro',
        `離線寬限至 ${commercial.status.entitlement?.effectiveExpiresAt || '未知時間'}`,
      ];
    return [
      'Pro',
      commercial.projectionStale ? '連線逾時，保留 agentd 投影' : 'Pro 已啟用',
    ];
  }
  if (commercial.status.reason === 'offline_grace')
    return [
      'General',
      `離線寬限至 ${commercial.status.entitlement?.effectiveExpiresAt || '未知時間'}`,
    ];
  if (commercial.status.state === 'authorized_general')
    return protectedClient()
      ? ['General', 'General 已授權']
      : ['免費帳號', '已登入；可連接自己的 API 或本地模型'];
  return ['需要登入', '尚未登入'];
}

function renderStatusPill() {
  const pill = $('#licensePill');
  if (!pill) return;
  pill.hidden = true;
  pill.textContent = '';
  pill.removeAttribute('role');
  pill.removeAttribute('aria-label');
  pill.removeAttribute('tabindex');
  pill.onclick = null;
  pill.onkeydown = null;
}

function accountIdentity() {
  if (commercial.vmLab) return {name: 'Meadow VM Lab', detail: '免登入測試版', initial: 'VM'};
  const name = commercial.usage?.account?.displayName || 'Meadow 帳號';
  const detail = commercial.usage?.account?.email || statusPresentation()[0];
  const initial = [...name.trim()][0]?.toUpperCase() || 'M';
  return { name, detail, initial };
}

function accountAccessLabel() {
  if (commercial.vmLab) return '免 Meadow 登入 · 自備 API';
  if (commercial.usageState === 'loading') return '同步使用情況…';
  if (commercial.usageState === 'offline') return '離線 · 操作已鎖定';
  if (commercial.usageState === 'sync_error') return '額度同步受阻';
  if (commercial.usageState === 'reauth') return '需要重新登入';
  if (accountProjection().contradictory) return '授權狀態不一致 · 請重新整理';
  const quota = commercial.promptQuota || commercial.usage?.usage;
  if (quota?.unlimited) return 'Pro · 一次買斷 · 訊息次數不限';
  if (quota) return `剩餘 ${quota.remaining} / ${quota.limit} 次 · 已送出 ${quota.acceptedPromptCount} 次`;
  return accountReady() ? '尚未取得訊息額度' : statusPresentation()[0];
}

function accountAccessValue() {
  if (commercial.usageState !== 'ready') return '—';
  if (accountProjection().contradictory) return '！';
  const quota = commercial.promptQuota || commercial.usage?.usage;
  return quota?.unlimited ? '∞' : quota ? `${quota.remaining}/${quota.limit}` : '—';
}

function renderAccountRow() {
  const row = $('#commercialAccountRow');
  if (!row) return;
  row.hidden = false;
  const identity = accountIdentity();
  row.querySelector('.commercial-account-avatar').textContent =
    identity.initial;
  row.querySelector('.commercial-account-name').textContent = identity.name;
  row.querySelector('.commercial-account-detail').textContent =
    accountAccessLabel();
  row.setAttribute(
    'aria-label',
    `${identity.name}，${accountAccessLabel()}，開啟帳號選單`,
  );
  row.dataset.state = commercial.usageState;
}

let accountPopoverCleanup = null;

function closeAccountPopover() {
  accountPopoverCleanup?.();
  accountPopoverCleanup = null;
  $('#commercialAccountPopover')?.remove();
}
function settingsOpen() {
  $('#btnSettings')?.click();
}

async function logoutAccount(output, close) {
  accountRecoveryEligible = false;
  cancelAccountRecovery();
  commercial.pollGeneration += 1;
  commercial.logoutPending = true;
  commercial.projectionStale = true;
  setClientAuthentication(null);
  emitChange();
  if (output) output.textContent = '正在登出。';
  try {
    parseLogout(await daemon.authLogout(buildSchemaRequest()));
    commercial.pollGeneration += 1;
    commercial.usageGeneration += 1;
    commercial.signedOut = true;
    commercial.logoutPending = false;
    commercial.status = null;
    setClientAuthentication(null);
    commercial.usage = null;
    commercial.promptQuota = null;
    commercial.usageState = 'reauth';
    commercial.usageMessage = '已登出；本機對話仍保留。';
    commercial.requestedMode = 'general';
    commercial.projectionStale = false;
    emitChange();
    close?.();
  } catch (error) {
    commercial.logoutPending = false;
    accountReadFailed(error);
    if (output) output.textContent = '登出未被 agentd 確認。';
  }
}

function showLogoutConfirmation() {
  if ($('#commercialLogoutConfirm')) return true;
  const { overlay, dialog, close } = createOverlay('登出 Meadow');
  overlay.id = 'commercialLogoutConfirm';
  dialog.classList.add('commercial-logout-card');
  dialog.innerHTML = `
    <button id="commercialLogoutClose" class="commercial-close" type="button" aria-label="關閉登出確認"></button>
    <img src="/assets/meadow-wave-mark.svg" alt="">
    <p class="commercial-kicker">SIGN OUT</p>
    <h2>要登出 Meadow 嗎？</h2>
    <p>儲存在這台裝置的對話、專案與產出會保留。登出後需重新登入才能繼續使用。</p>
    <p id="commercialLogoutStatus" class="commercial-live" role="status" aria-live="polite"></p>
    <div class="commercial-actions"><button id="commercialLogoutCancel" class="commercial-secondary" type="button">取消</button><button id="commercialLogoutConfirmButton" class="commercial-danger" type="button">登出這台裝置</button></div>`;
  const cancel = () => {
    close();
    $('#commercialAccountRow')?.focus();
  };
  $('#commercialLogoutClose')?.addEventListener('click', cancel);
  $('#commercialLogoutCancel')?.addEventListener('click', cancel);
  let pending = false;
  $('#commercialLogoutConfirmButton')?.addEventListener('click', async () => {
    if (pending) return;
    pending = true;
    const confirm = $('#commercialLogoutConfirmButton');
    const cancelButton = $('#commercialLogoutCancel');
    if (confirm) confirm.disabled = true;
    if (cancelButton) cancelButton.disabled = true;
    await logoutAccount($('#commercialLogoutStatus'), close);
    if (overlay.isConnected) {
      pending = false;
      if (confirm) confirm.disabled = false;
      if (cancelButton) cancelButton.disabled = false;
    }
  });
  overlay.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') cancel();
  });
  $('#commercialLogoutCancel')?.focus();
  return true;
}

async function openReferral(output) {
  try {
    const request = buildReferralRequest(newRequestID());
    const share = parseReferralShare(
      await daemon.referralShare(request),
      request.request_id,
    );
    window.open(share.url, '_blank', 'noopener,noreferrer');
  } catch {
    if (output) output.textContent = commercial.usageMessage;
  }
}

function showAccountPopover(anchor) {
  if (!accountReady()) { void showLoginGate(); return; }
  if ($('#commercialAccountPopover')) {
    closeAccountPopover();
    return;
  }
  const identity = accountIdentity();
  const popover = document.createElement('section');
  popover.id = 'commercialAccountPopover';
  popover.className = 'commercial-account-popover pop open';
  popover.setAttribute('role', 'menu');
  popover.setAttribute('aria-label', 'Meadow 帳號選單');
  const authenticated = commercial.status?.authenticated === true;
  const accessLabel = accountAccessLabel();
  const accessValue = accountAccessValue();
  const accessState =
    identity.detail === accessLabel
      ? ''
      : `<div class="commercial-pop-state">${escapeHTML(accessLabel)}</div>`;
  const actions = commercial.vmLab
    ? `<button type="button" role="menuitem" data-account-action="settings"><span><i aria-hidden="true">⚙</i>設定</span><small>⌘,</small></button>`
    : authenticated
    ? `<button type="button" role="menuitem" data-account-action="usage"><span><i aria-hidden="true">◔</i><span class="commercial-pop-action-copy"><strong>使用情況</strong><small>${escapeHTML(accessLabel)}</small></span></span><b>${escapeHTML(accessValue)}</b></button>
       ${commercial.usage?.usage && !commercial.usage.usage.unlimited && commercial.usageState === 'ready' ? `<div class="commercial-pop-meter" role="progressbar" aria-label="剩餘訊息次數" aria-valuemin="0" aria-valuemax="${commercial.usage.usage.limit}" aria-valuenow="${commercial.usage.usage.remaining}"><i style="width:${commercial.usage.usage.remaining / commercial.usage.usage.limit * 100}%"></i></div>` : ''}
       ${commercial.usage?.usage?.unlimited ? '' : '<button type="button" role="menuitem" data-account-action="upgrade"><span>Pro 一次買斷 NT$100</span></button>'}
       <button type="button" role="menuitem" data-account-action="settings"><span><i aria-hidden="true">⚙</i>設定</span><small>⌘,</small></button>
       <div class="commercial-pop-divider"></div>
       <button type="button" role="menuitem" data-account-action="logout"><span><i aria-hidden="true">↪</i>登出</span></button>`
    : `<button type="button" role="menuitem" data-account-action="login"><span><i aria-hidden="true">↗</i>登入 Meadow</span></button>
       <div class="commercial-pop-divider"></div>
       <button type="button" role="menuitem" data-account-action="settings"><span><i aria-hidden="true">⚙</i>設定</span><small>⌘,</small></button>`;
  popover.innerHTML = `
    <div class="commercial-pop-profile"><span class="commercial-account-avatar">${escapeHTML(identity.initial)}</span><div><strong>${escapeHTML(identity.name)}</strong><small>${escapeHTML(identity.detail)}</small></div></div>
    ${accessState}
    ${actions}
    <p class="commercial-pop-live" role="status" aria-live="polite"></p>`;
  document.body.append(popover);
  const rect = anchor.getBoundingClientRect();
  popover.style.width = `${Math.max(220, rect.width)}px`;
  popover.style.left = `${Math.max(8, Math.min(rect.left, window.innerWidth - popover.offsetWidth - 8))}px`;
  popover.style.bottom = `${Math.max(8, window.innerHeight - rect.top + 6)}px`;
  const closeOutside = (event) => {
    if (!popover.contains(event.target) && event.target !== anchor) {
      closeAccountPopover();
    }
  };
  const outsideTimer = setTimeout(() => {
    document.addEventListener('pointerdown', closeOutside, true);
  }, 0);
  accountPopoverCleanup = () => {
    clearTimeout(outsideTimer);
    document.removeEventListener('pointerdown', closeOutside, true);
  };
  popover.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      closeAccountPopover();
      anchor.focus();
      return;
    }
    const items = [...popover.querySelectorAll('[role="menuitem"]')].filter(
      (item) => !item.disabled,
    );
    if (!items.length) return;
    const current = Math.max(0, items.indexOf(document.activeElement));
    let next = -1;
    if (event.key === 'ArrowDown') next = (current + 1) % items.length;
    if (event.key === 'ArrowUp')
      next = (current - 1 + items.length) % items.length;
    if (event.key === 'Home') next = 0;
    if (event.key === 'End') next = items.length - 1;
    if (next >= 0) {
      event.preventDefault();
      items[next].focus();
    }
  });
  popover.addEventListener('click', (event) => {
    const action = event.target.closest('[data-account-action]')?.dataset
      .accountAction;
    const output = popover.querySelector('.commercial-pop-live');
    if (action === 'login') {
      closeAccountPopover();
      void showLoginGate();
    }
    if (action === 'usage') {
      closeAccountPopover();
      showAccount();
    }
    if (action === 'upgrade') {
      closeAccountPopover();
      showStorefrontSheet();
    }
    if (action === 'settings') {
      closeAccountPopover();
      settingsOpen();
    }
    if (action === 'logout') {
      closeAccountPopover();
      showLogoutConfirmation();
    }
  });
  popover.querySelector('button')?.focus();
  if (!commercial.usage && commercial.status?.authenticated)
    void refreshMembershipUsage().catch(() => {});
}

export function mountCommercialAccount(host) {
  if (!host || $('#commercialAccountRow')) return;
  const row = document.createElement('button');
  row.id = 'commercialAccountRow';
  row.type = 'button';
  row.className = 'commercial-account-row';
  row.setAttribute('aria-haspopup', 'menu');
  row.innerHTML =
    '<span class="commercial-account-avatar">M</span><span class="commercial-account-copy"><strong class="commercial-account-name">Meadow 帳號</strong><small class="commercial-account-detail">載入中</small></span><span class="commercial-account-chevron" aria-hidden="true">···</span>';
  row.addEventListener('click', () => showAccountPopover(row));
  host.append(row);
  renderAccountRow();
  const generation = commercial.pollGeneration;
  const clientGeneration = clientAccountGeneration();
  void resolveProtectedClient()
    .then(async () => {
      if (generation !== commercial.pollGeneration || clientGeneration !== clientAccountGeneration()) return;
      try {
        const status = parseStatus(await daemon.authStatus());
        if (generation !== commercial.pollGeneration || clientGeneration !== clientAccountGeneration()) return;
        applyStatus(status);
        if (commercial.status?.authenticated)
          void refreshMembershipUsage().catch(() => {});
      } catch (error) {
        if (generation !== commercial.pollGeneration || clientGeneration !== clientAccountGeneration()) return;
        accountReadFailed(error);
      }
    })
    .catch(error => {
      if (generation === commercial.pollGeneration && clientGeneration === clientAccountGeneration() &&
          !commercial.capabilitiesReady && !commercial.logoutPending) accountReadFailed(error);
      else renderAccountRow();
    });
}

function formatAccountDate(value) {
  try {
    return new Intl.DateTimeFormat('zh-TW', { dateStyle: 'medium' }).format(
      new Date(value),
    );
  } catch {
    return '無法顯示';
  }
}

function renderAccountUsage() {
  const container = $('#commercialUsageDetails');
  if (!container) return;
  if (commercial.usageState === 'loading') {
    container.innerHTML =
      '<div class="commercial-usage-placeholder">正在取得帳號訊息額度…</div>';
    return;
  }
  if (!commercial.usage || commercial.usageState !== 'ready') {
    container.innerHTML = `<div class="commercial-usage-placeholder" data-state="${escapeHTML(commercial.usageState)}">${escapeHTML(commercial.usageMessage || '尚未取得使用情況。')}</div>`;
    return;
  }
  if (accountProjection().contradictory) {
    container.innerHTML =
      '<div class="commercial-usage-placeholder" data-state="conflict">帳號方案與使用資格不一致；操作已鎖定。請先重新整理授權，若仍存在請登出後重新登入。</div>';
    return;
  }
  const usage = commercial.usage;
  const quota = usage.usage;
  const percent = quota.unlimited ? 100 : quota.remaining / quota.limit * 100;
  container.innerHTML = `
    <div class="commercial-usage-head"><div><span>${quota.unlimited ? 'MEADOW PRO' : 'MEADOW FREE'}</span><strong>${escapeHTML(quota.unlimited ? 'Pro · 一次買斷 · 訊息次數不限' : `剩餘 ${quota.remaining} / ${quota.limit} 次`)}</strong></div><b>${quota.unlimited ? '∞' : `${quota.remaining}/${quota.limit}`}</b></div>
    ${quota.unlimited ? '' : `<div class="commercial-usage-bar" role="progressbar" aria-label="剩餘訊息次數" aria-valuemin="0" aria-valuemax="${quota.limit}" aria-valuenow="${quota.remaining}"><i style="width:${percent}%"></i></div>`}
    <dl class="commercial-usage-grid"><div><dt>免費額度已送出</dt><dd>${quota.acceptedPromptCount} 次</dd></div><div><dt>裝置</dt><dd>${usage.devices.active} / ${usage.devices.maximum} 台</dd></div></dl>
    <p class="commercial-form-note">每次由帳號服務接受的送出計 1 次；工具呼叫與模型內部處理不另計。模型 API 費用由供應商計收。</p>
    ${quota.unlimited ? '' : '<button id="commercialUsageUpgrade" class="commercial-primary commercial-wide" type="button">Pro 一次買斷 NT$100 · 訊息次數不限</button>'}`;
  $('#commercialUsageUpgrade')?.addEventListener('click', () => showStorefrontSheet());
}

function showAccount() {
  if ($('#commercialAccount')) return;
  const { overlay, dialog, close } = createOverlay('帳號與授權');
  overlay.id = 'commercialAccount';
  dialog.classList.add('commercial-license-card');
  if (!accountReady()) {
    dialog.innerHTML = `
      <p class="commercial-kicker">MEADOW ACCOUNT</p><h2>帳號狀態</h2>
      <p>登入後才能使用對話、專案、Browser 與模型連線設定。</p>
      <p id="commercialAccountStatus" class="commercial-live" role="status" aria-live="polite">${commercial.projectionStale ? '目前無法確認登入狀態，請重新檢查或重新登入。' : '尚未登入 Meadow。'}</p>
      <div class="commercial-actions"><button id="commercialAccountRefresh" type="button">重新檢查</button><button id="commercialAccountClose" type="button">返回登入</button></div>`;
    $('#commercialAccountRefresh')?.addEventListener('click', async (event) => {
      event.currentTarget.disabled = true;
      const output = $('#commercialAccountStatus');
      output.textContent = '正在檢查登入狀態。';
      const authenticated = await requireAuthenticatedSession({ prompt: false });
      if (!overlay.isConnected) return;
      if (authenticated) close();
      else {
        output.textContent = commercial.projectionStale ? '目前無法確認登入狀態，請稍後重試或返回登入。' : '尚未登入 Meadow，請返回登入完成驗證。';
        $('#commercialAccountRefresh').disabled = false;
        $('#commercialAccountRefresh').focus();
      }
    });
    $('#commercialAccountClose')?.addEventListener('click', close);
    overlay.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') close();
      event.stopPropagation();
    });
    $('#commercialAccountClose')?.focus();
    return;
  }
  const onAccountChange = () => {
    if (!overlay.isConnected) { window.removeEventListener('commercial:change', onAccountChange); return; }
    if (accountReady()) return;
    close();
    window.removeEventListener('commercial:change', onAccountChange);
    showAccount();
  };
  window.addEventListener('commercial:change', onAccountChange);
  const closeAccount = () => {
    window.removeEventListener('commercial:change', onAccountChange);
    close();
  };
  const projection = accountProjection();
  const pro = projection.isPro;
  const entitlement = commercial.status?.entitlement;
  const leaseTail = entitlement?.leaseID
    ? entitlement.leaseID.slice(-4).toUpperCase()
    : '';
  dialog.innerHTML = `
    <button id="commercialAccountCloseIcon" class="commercial-close" type="button" aria-label="關閉帳號與授權"></button>
    <img class="commercial-license-mark" src="/assets/meadow-wave-mark.svg" alt="">
    <p class="commercial-kicker">${pro ? 'MEADOW CODE PRO' : 'MEADOW ACCOUNT'}</p>
    <h2>${escapeHTML(pro ? 'Meadow Desktop Pro' : accountIdentity().name)}</h2>
    <p>${escapeHTML(pro ? `${accountIdentity().name} · ${accountIdentity().detail}` : accountIdentity().detail)}</p>
    ${pro ? `<dl class="commercial-license-summary"><div><dt>這台裝置</dt><dd>${entitlement?.deviceID ? '已啟用' : '等待授權投影'}</dd></div><div><dt>授權識別</dt><dd>${leaseTail ? `••••-${escapeHTML(leaseTail)}` : '由帳號服務管理'}</dd></div><div><dt>本機授權投影有效至</dt><dd>${escapeHTML(entitlement?.effectiveExpiresAt ? formatAccountDate(entitlement.effectiveExpiresAt) : '等待 agentd 投影')}</dd></div></dl>` : ''}
    <section id="commercialUsageDetails" aria-label="使用情況"></section>
    <section aria-labelledby="commercialDevicesTitle"><h3 id="commercialDevicesTitle">已綁定裝置</h3><div id="commercialDevices" aria-live="polite">正在讀取裝置。</div></section>
    <p id="commercialAccountStatus" class="commercial-live" role="status" aria-live="polite">${projection.contradictory ? '帳號方案與使用資格不一致；僅允許重新整理或登出。' : ''}</p>
    <div class="commercial-actions"><button id="commercialAccountSerial" type="button">輸入購買序號</button><button id="commercialAccountReferral" type="button" ${projection.contradictory ? 'disabled aria-disabled="true"' : ''}>邀請好友</button><button id="commercialAccountRefresh" type="button">重新整理授權</button><button id="commercialAccountClose" type="button">關閉</button></div>`;
  renderAccountUsage();
  if (!commercial.usage)
    void refreshMembershipUsage()
      .then(renderAccountUsage)
      .catch(renderAccountUsage);
  void renderAccountDevices();
  $('#commercialAccountRefresh')?.addEventListener('click', async () => {
    const output = $('#commercialAccountStatus');
    output.textContent = '正在重新整理。';
    try {
      await refreshStatus();
      await refreshMembershipUsage();
      renderAccountUsage();
      await renderAccountDevices();
      output.textContent = '帳號狀態已更新。';
    } catch {
      output.textContent = '目前無法更新帳號狀態，請稍後重試。';
    }
  });
  $('#commercialAccountReferral')?.addEventListener(
    'click',
    () => void openReferral($('#commercialAccountStatus')),
  );
  $('#commercialAccountSerial')?.addEventListener('click', () => {
    closeAccount();
    void showLoginGate({ serial: true });
  });
  $('#commercialAccountCloseIcon')?.addEventListener('click', closeAccount);
  $('#commercialAccountClose')?.addEventListener('click', closeAccount);
  overlay.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') closeAccount();
    event.stopPropagation();
  });
  $('#commercialAccountClose')?.focus();
}

function deviceManagementAvailable() {
  const maximum = commercial.status?.capabilities?.[CAPABILITY_DEVICES_MAX];
  return (
    !accountProjection().contradictory &&
    commercial.status?.authenticated === true &&
    Number.isSafeInteger(maximum) &&
    maximum > 0
  );
}

async function renderAccountDevices() {
  const container = $('#commercialDevices');
  if (!container) return;
  if (!deviceManagementAvailable()) {
    container.textContent =
      '目前無法確認裝置授權，請重新整理帳號狀態。';
    return;
  }
  try {
    const result = parseDeviceList(
      await daemon.deviceList(buildSchemaRequest()),
    );
    if (
      result.maximumDevices !==
        commercial.status.capabilities[CAPABILITY_DEVICES_MAX] ||
      result.currentDeviceID !== commercial.status.entitlement?.deviceID
    ) {
      throw new Error('device list and signed lease mismatch');
    }
    container.innerHTML = `<p>${result.activeDevices} / ${result.maximumDevices} 台使用中</p><ul class="commercial-device-list">${result.devices
      .map(
        (device) => `
      <li><div><strong>${escapeHTML(device.name)}</strong><span>${escapeHTML(device.platform)} · ${escapeHTML(device.status)}${device.current ? ' · 目前裝置' : ''}</span></div>
      <button type="button" data-device-revoke="${escapeHTML(device.id)}" ${device.current || device.status !== 'active' ? 'disabled aria-disabled="true"' : ''}>${device.current ? '請使用登出' : device.status === 'active' ? '撤銷' : '已撤銷'}</button></li>`,
      )
      .join('')}</ul>`;
    for (const button of container.querySelectorAll(
      '[data-device-revoke]:not([disabled])',
    )) {
      button.addEventListener('click', async () => {
        const deviceID = button.dataset.deviceRevoke;
        if (
          !window.confirm(
            '撤銷此遠端裝置？其現有 session、refresh token 與 signed lease 將立即失效。',
          )
        )
          return;
        button.disabled = true;
        const reason = 'Desktop user confirmed remote device revocation';
        try {
          const request = buildDeviceRevokeRequest(deviceID, reason);
          const revoked = parseDeviceRevoke(await daemon.deviceRevoke(request));
          if (
            revoked.receipt.requestID !== request.request_id ||
            revoked.receipt.deviceID !== deviceID
          ) {
            throw new Error('device revoke receipt mismatch');
          }
          applyStatus(revoked.availability);
          void refreshMembershipUsage().catch(() => {});
          await renderAccountDevices();
        } catch {
          button.disabled = false;
          const output = $('#commercialAccountStatus');
          if (output)
            output.textContent = '裝置撤銷未被 agentd 確認，授權狀態未變更。';
        }
      });
    }
  } catch {
    container.textContent =
      '無法取得經 agentd 驗證的裝置清單；所有撤銷操作保持停用。';
  }
}

async function selectMode(mode) {
  if (mode !== 'pro') {
    commercial.requestedMode = 'general';
    emitChange();
    return true;
  }
  if (!proAvailable()) {
    showUpgrade('contextual', CAPABILITY_AGENT_AUTO);
    return false;
  }
  commercial.requestedMode = 'pro';
  emitChange();
  return true;
}

function modelTier(entry) {
  const id = String(entry?.modelId || entry?.modelID || entry?.id || '').trim();
  return MODEL_TIERS[id] || '';
}

function canUseModel(entry) {
  if (!accountReady()) return false;
  if (!protectedClient()) return true;
  const tier = modelTier(entry);
  if (!tier) return false;
  return hasCapability(tier);
}

function requestModelAccess(entry) {
  if (!accountReady()) { void showLoginGate(); return false; }
  if (canUseModel(entry)) return true;
  const tier = modelTier(entry);
  if (tier && tier !== CAPABILITY_MODEL_12B) showUpgrade('contextual', tier);
  return false;
}

function canUseCapability(capability) {
  if (!accountReady()) return false;
  if (!protectedClient()) return true;
  if (String(capability || '').startsWith('model.'))
    return hasCapability(capability);
  return activeMode() === 'pro' && hasCapability(capability);
}

export async function requireAuthenticatedSession({ prompt = true } = {}) {
  if (commercial.logoutPending) return false;
  injectStylesheet();
  ensureModeSwitch();
  const generation = commercial.pollGeneration;
  const clientGeneration = clientAccountGeneration();
  try {
    await resolveProtectedClient();
    if (generation !== commercial.pollGeneration || clientGeneration !== clientAccountGeneration()) return false;
    if (commercial.authenticationRequired === false) return true;
    const status = parseStatus(await daemon.authStatus());
    if (generation !== commercial.pollGeneration || clientGeneration !== clientAccountGeneration()) return false;
    applyStatus(status);
    if (accountReady()) {
      if (commercial.loginPromise) closeLoginGate(true);
      if (!commercial.usage && !commercial.promptQuota && commercial.usageState === 'idle')
        void refreshMembershipUsage().catch(() => {});
      return true;
    }
  } catch (error) {
    if (generation !== commercial.pollGeneration || clientGeneration !== clientAccountGeneration()) return false;
    accountReadFailed(error);
  }
  return prompt ? showLoginGate() : false;
}

export function commercialSessionReady() {
  if (!protectedClient() || !commercial.status || interruptedWorkflow()) return;
  if (
    commercial.status.state !== 'authorized_general' &&
    commercial.status.state !== 'authorized_pro'
  )
    return;
  const decisions = loadDecisions();
  if (!decisions.firstLoginShown) {
    decisions.firstLoginShown = true;
    saveDecisions(decisions);
  }
}

window.MeadowCommercial = Object.freeze({
  snapshot: publicSnapshot,
  membershipSnapshot,
  selectMode,
  canUseModel,
  requestModelAccess,
  canUseCapability,
  modelTier,
  showUpgrade,
  openLogin: requestAccountLogin,
  openSerial: () => showLoginGate({ serial: true }),
  openPurchase: () => showUpgrade('manual', CAPABILITY_AGENT_AUTO),
  openStorefront: () => showStorefrontSheet(),
  openAccount: showAccount,
  openLogout: showLogoutConfirmation,
  refreshUsage: refreshMembershipUsage,
  refresh: async () => {
    await refreshStatus();
    return publicSnapshot();
  },
});

bus.on('authentication_required', (event) => {
  commercial.pollGeneration += 1;
  cancelLoginStart();
  clearLoginHandoff();
  setLoginButtonsBusy(false);
  applyStatus(null);
  if (event?.retryable === true) {
    accountRecoveryEligible = true;
    commercial.projectionStale = true;
    emitChange();
    startAccountRecovery();
  }
});

if (typeof window !== 'undefined') {
  window.addEventListener?.('focus', resumeAvailableAccountRecovery);
  window.addEventListener?.('online', resumeAvailableAccountRecovery);
}
if (typeof document !== 'undefined') {
  document.addEventListener?.('visibilitychange', resumeAvailableAccountRecovery);
}

injectStylesheet();
ensureModeSwitch();
emitChange();
