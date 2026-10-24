// Shared frontend transport, state, structured events, and safe rendering.
import { runLimitOptions } from './run_limits.js';
import { VM_LAB_BUILD } from './vm_lab_build.js';

const JSON_HEADERS = { 'Content-Type': 'application/json' };

async function request(method, path, body, headers = {}, encodedBody, signal) {
  const protectedRequest = !accountPublicPath(path, method);
  const generation = clientAccountGeneration();
  throwIfRequestAborted(signal);
  if (protectedRequest && !hasClientAccess())
    await waitForAccountAdmission(signal);
  throwIfRequestAborted(signal);
  if (protectedRequest) assertAccountGeneration(generation);
  const data = await requestUnchecked(method, path, body, headers, encodedBody, signal);
  if (protectedRequest) assertAccountGeneration(generation);
  return data;
}

async function requestUnchecked(method, path, body, headers = {}, encodedBody, signal) {
  const generation = clientAccountGeneration();
  const opts = { method, headers: { ...JSON_HEADERS, ...headers } };
  if (signal) opts.signal = signal;
  if (body !== undefined)
    opts.body = encodedBody === undefined ? JSON.stringify(body) : encodedBody;
  const res = await fetch(path, opts);
  let data = null;
  try {
    data = await res.json();
  } catch (error) {
    if (signal?.aborted) throw error;
    /* empty or non-JSON body */
  }
  if (!res.ok) {
    const serverError =
      data && typeof data.error === 'object' && data.error ? data.error : null;
    const production =
      document.documentElement.dataset.frontendMode === 'production';
    const attachmentError = res.status === 422 &&
      data?.code === 'ATTACHMENT_UNREADABLE';
    const err = new Error(
      production
        ? attachmentError
          ? '附件格式或內容無法讀取；請確認檔案可正常開啟且未加密，再重新上傳。'
          : `操作失敗 (HTTP ${res.status})`
        : (serverError && serverError.message) ||
          (data && (data.error || data.message)) ||
          `HTTP ${res.status}`,
    );
    err.status = res.status;
    if (!accountPublicPath(path, method) && accountDenial(res.status, data))
      revokeClientAuthentication(generation, res.status, data);
    if (!production) err.data = data;
    if (serverError && serverError.code) err.code = serverError.code;
    if (
      data &&
      typeof data.code === 'string' &&
      /^[A-Za-z0-9_-]{1,80}$/.test(data.code)
    )
      err.code = data.code;
    throw err;
  }
  return data;
}
const post = (path, body) => request('POST', path, body);
const get = (path) => request('GET', path);
const del = (path) => request('DELETE', path);

const MEMBERSHIP_HEADERS = Object.freeze({
  schema: 'X-Meadow-Membership-Schema',
  request: 'X-Meadow-Membership-Request-ID',
  authorization: 'X-Meadow-Membership-Authorization-ID',
  conversation: 'X-Meadow-Membership-Conversation-ID',
});

const PROMPT_REQUEST_ID_HEADER = 'X-Meadow-Prompt-Request-ID';
const pendingPromptRequests = new Map();
const unresolvedPromptSessions = new Map();
const activePromptDeliveries = new Map();
let promptAdmissionCheck = null;
const promptAdmissionStopEpochs = new Map();

// Optional UI preference persistence barrier. It cannot send a prompt itself,
// change tool permissions, or alter the caller's frozen task allowance.
export function registerPromptAdmissionCheck(check) {
  if (typeof check !== 'function') throw new Error('admission check required');
  promptAdmissionCheck = check;
}

async function confirmPromptAdmission(sessionID, options) {
  const controls = promptAdmissionCheck ? await promptAdmissionCheck(sessionID) : null;
  // Explicit Evo budgets exceed the ordinary Max allowance. A mode save may
  // finish while account/Browser admission awaits; never dispatch that frozen
  // allowance to a newly selected ordinary mode or rewrite a retry's body.
  if (options?.runTokenLimit > 1000000 && controls?.effectiveMode !== 'evo')
    throw new Error('執行方式已變更，Evo 額度僅能用於 Evo；請切回 Evo 後重試');
}
const MAX_PENDING_PROMPT_REQUESTS = 32;
const TRANSIENT_PROMPT_STATUS = new Set([502, 503, 504]);
const PROMPT_FINGERPRINT_FULL_LIMIT = 1 << 20;
const PROMPT_FINGERPRINT_SAMPLE_WIDTH = 4096;
const PROMPT_FINGERPRINT_SAMPLE_COUNT = 32;

function newPromptRequestID() {
  if (globalThis.crypto?.randomUUID)
    return `prompt-${globalThis.crypto.randomUUID()}`;
  const bytes = new Uint8Array(16);
  if (!globalThis.crypto?.getRandomValues)
    throw new Error('secure prompt request identity is unavailable');
  globalThis.crypto.getRandomValues(bytes);
  return `prompt-${Array.from(bytes, (byte) =>
    byte.toString(16).padStart(2, '0'),
  ).join('')}`;
}

async function promptDeliveryFingerprint(sessionID, encodedBody) {
  if (!globalThis.crypto?.subtle || !globalThis.TextEncoder) return '';
  try {
    let fingerprintBody = encodedBody;
    if (encodedBody.length > PROMPT_FINGERPRINT_FULL_LIMIT) {
      const samples = [];
      const lastOffset = Math.max(
        0,
        encodedBody.length - PROMPT_FINGERPRINT_SAMPLE_WIDTH,
      );
      for (let index = 0; index < PROMPT_FINGERPRINT_SAMPLE_COUNT; index += 1) {
        const offset = Math.floor(
          (lastOffset * index) / (PROMPT_FINGERPRINT_SAMPLE_COUNT - 1),
        );
        samples.push(
          encodedBody.slice(offset, offset + PROMPT_FINGERPRINT_SAMPLE_WIDTH),
        );
      }
      // This key only reconnects a UI retry with its prior opaque request ID;
      // Core still verifies the complete raw-body SHA-256 before replaying.
      fingerprintBody = `${encodedBody.length}\0${samples.join('\0')}`;
    }
    const payload = new TextEncoder().encode(
      `${sessionID}\0${fingerprintBody}`,
    );
    const digest = new Uint8Array(
      await globalThis.crypto.subtle.digest('SHA-256', payload),
    );
    return Array.from(digest, (byte) =>
      byte.toString(16).padStart(2, '0'),
    ).join('');
  } catch {
    // Old WebKit builds can expose SubtleCrypto on a custom scheme but reject
    // digest operations. The immediate transport retry remains idempotent via
    // its local request ID; only cross-submit recovery is unavailable.
    return '';
  }
}

function rememberPromptRequest(sessionID, fingerprint, requestID) {
  // Keep only IDs as a pre-hash recovery hint. An earlier transport failure
  // can leave its daemon run active; Stop on a resubmit must target that ID.
  unresolvedPromptSessions.delete(sessionID);
  unresolvedPromptSessions.set(sessionID, { fingerprint, requestID });
  while (unresolvedPromptSessions.size > MAX_PENDING_PROMPT_REQUESTS) {
    unresolvedPromptSessions.delete(
      unresolvedPromptSessions.keys().next().value,
    );
  }
  if (fingerprint) pendingPromptRequests.set(fingerprint, requestID);
  while (pendingPromptRequests.size > MAX_PENDING_PROMPT_REQUESTS) {
    pendingPromptRequests.delete(pendingPromptRequests.keys().next().value);
  }
}

function forgetPromptRequest(fingerprint, requestID) {
  if (fingerprint && pendingPromptRequests.get(fingerprint) === requestID) {
    pendingPromptRequests.delete(fingerprint);
  }
  for (const [sessionID, pending] of unresolvedPromptSessions) {
    if (pending.requestID === requestID)
      unresolvedPromptSessions.delete(sessionID);
  }
}

function localPromptCancellation(dispatched) {
  return { stopReason: 'cancelled', cancelled: true, local: true, dispatched };
}

function waitPromptRetry(signal) {
  return new Promise((resolve) => {
    let timer;
    const finish = () => {
      clearTimeout(timer);
      signal.removeEventListener('abort', finish);
      resolve();
    };
    if (signal.aborted) return finish();
    signal.addEventListener('abort', finish, { once: true });
    timer = setTimeout(finish, 120);
  });
}

async function deliverPrompt(path, sessionID, body, headers, prepare) {
  const selectionRevision = sessionSelectionRevision;
  if (activePromptDeliveries.has(sessionID)) {
    const error = new Error(
      'a prompt delivery is already active for this session',
    );
    error.code = 'PROMPT_DELIVERY_ACTIVE';
    throw error;
  }
  let resolveCancellation;
  const cancellation = new Promise((resolve) => {
    resolveCancellation = resolve;
  });
  const freshRequestID = headers?.[MEMBERSHIP_HEADERS.request] || newPromptRequestID();
  const unresolved = unresolvedPromptSessions.get(sessionID);
  // Own a cancellable identity before Browser admission and hashing. A Stop
  // can reach Core before the prompt POST, so it sends this
  // same identity for Core to retain as a cancellation tombstone.
  const delivery = {
    requestID: unresolved?.requestID || freshRequestID,
    fingerprint: unresolved?.fingerprint || '',
    cancelRequested: false,
    dispatched: !!unresolved,
    cancelRequest: null,
    controller: new AbortController(),
    cancellation,
    resolveCancellation,
  };
  activePromptDeliveries.set(sessionID, delivery);
  try {
    const completion = (async () => {
      // Stop during Browser admission must not be followed by a late prompt POST.
      if (prepare) await prepare();
      if (delivery.cancelRequested) return cancellation;
      // Encode once: retries retain the exact body and opaque request ID.
      const encodedBody = JSON.stringify(body);
      const fingerprint = await promptDeliveryFingerprint(
        sessionID,
        encodedBody,
      );
      if (delivery.cancelRequested) return cancellation;
      delivery.fingerprint = fingerprint;
      // Preference saves may begin while Browser ownership or hashing awaits.
      // Confirm immediately before dispatch, within the existing Stop owner.
      await confirmPromptAdmission(sessionID, body.options);
      if (delivery.cancelRequested) return cancellation;
      if (selectionRevision !== sessionSelectionRevision)
        throw new Error('對話已切換，訊息尚未送出');
      delivery.requestID =
        (fingerprint && pendingPromptRequests.get(fingerprint)) ||
        freshRequestID;
      rememberPromptRequest(sessionID, fingerprint, delivery.requestID);
      const requestHeaders = {
        ...headers,
        [PROMPT_REQUEST_ID_HEADER]: delivery.requestID,
      };

      // Only transport failures retry. Stop invalidates this delivery, including
      // a retry timer or late hash/fetch result, without affecting a newer send.
      for (let attempt = 0; attempt < 2; attempt += 1) {
        if (delivery.cancelRequested) return cancellation;
        try {
          delivery.dispatched = true;
          const result = await request(
            'POST',
            path,
            body,
            requestHeaders,
            encodedBody,
            delivery.controller.signal,
          );
          if (
            !result ||
            typeof result !== 'object' ||
            typeof result.stopReason !== 'string'
          ) {
            throw new Error('invalid prompt response');
          }
          forgetPromptRequest(fingerprint, delivery.requestID);
          return result;
        } catch (error) {
          if (delivery.cancelRequested) return cancellation;
          if (
            Number.isInteger(error?.status) &&
            !TRANSIENT_PROMPT_STATUS.has(error.status)
          ) {
            forgetPromptRequest(fingerprint, delivery.requestID);
            throw error;
          }
          if (attempt === 1) throw error;
          await waitPromptRetry(delivery.controller.signal);
        }
      }
      throw new Error('prompt delivery failed');
    })();
    return await Promise.race([completion, cancellation]);
  } finally {
    if (activePromptDeliveries.get(sessionID) === delivery) {
      activePromptDeliveries.delete(sessionID);
    }
  }
}

function cancelPromptDelivery(sessionID) {
  promptAdmissionStopEpochs.set(sessionID,
    (promptAdmissionStopEpochs.get(sessionID) || 0) + 1);
  const delivery = activePromptDeliveries.get(sessionID);
  if (!delivery) return post(`/session/${sessionID}/cancel`);
  if (delivery.cancelRequest) return delivery.cancelRequest;
  delivery.cancelRequested = true;
  if (!delivery.dispatched) {
    // A held hash has not sent any prompt bytes. It can finish locally now;
    // the tombstone still protects against an out-of-order late delivery.
    forgetPromptRequest(delivery.fingerprint, delivery.requestID);
    delivery.controller.abort();
    delivery.resolveCancellation(localPromptCancellation(false));
  }
  // A daemon-owned run survives an HTTP disconnect. Until Core acknowledges
  // Stop, keep a dispatched delivery owned and its outcome pending, so a
  // failed cancel remains retryable instead of making the UI falsely idle.
  const cancelRequest = post(`/session/${sessionID}/cancel`, {
    requestId: delivery.requestID,
  })
    .then((receipt) => {
      if (receipt?.cancelling !== true) {
        const error = new Error('the prompt cancellation was not acknowledged');
        error.code = 'PROMPT_CANCEL_NOT_ACKNOWLEDGED';
        throw error;
      }
      forgetPromptRequest(delivery.fingerprint, delivery.requestID);
      delivery.controller.abort();
      delivery.resolveCancellation(
        localPromptCancellation(delivery.dispatched),
      );
      return receipt;
    })
    .finally(() => {
      if (delivery.cancelRequest === cancelRequest)
        delivery.cancelRequest = null;
    });
  delivery.cancelRequest = cancelRequest;
  return cancelRequest;
}

const DOCUMENT_MEDIA_TYPES = Object.freeze({
  xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  csv: 'text/csv',
  docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  pptx: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  txt: 'text/plain',
  md: 'text/markdown',
});

export function documentMediaType(name) {
  const extension = String(name || '').match(/\.([^.\/\\]+)$/)?.[1].toLowerCase();
  return Object.hasOwn(DOCUMENT_MEDIA_TYPES, extension)
    ? DOCUMENT_MEDIA_TYPES[extension]
    : '';
}

function promptBlocks(text, attachments) {
  const prompt = [{ type: 'text', text }];
  for (const attachment of attachments || []) {
    if (typeof attachment === 'string' && attachment) {
      prompt.push({ type: 'image_url', image_url: { url: attachment } });
      continue;
    }
    if (!attachment || !['pdf', 'document'].includes(attachment.kind)) continue;
    const name =
      attachment.name || (attachment.kind === 'pdf' ? '附件.pdf' : '附件.txt');
    const mediaType =
      attachment.kind === 'pdf' ? 'application/pdf' : documentMediaType(name);
    if (!mediaType) continue;
    const file = {
      name,
      media_type: mediaType,
    };
    if (attachment.path) file.file_path = attachment.path;
    else if (attachment.dataURL) file.data_url = attachment.dataURL;
    else continue;
    prompt.push({ type: 'file', file });
  }
  return prompt;
}

function membershipAuthorizationFields(authorization) {
  if (!authorization) return {};
  return {
    membership_authorization: {
      schema: authorization.schema,
      request_id: authorization.requestID,
      authorization_id: authorization.authorizationID,
      conversation_id: authorization.conversationID,
    },
  };
}

function membershipAuthorizationHeaders(authorization) {
  if (!authorization) return {};
  return {
    [MEMBERSHIP_HEADERS.schema]: authorization.schema,
    [MEMBERSHIP_HEADERS.request]: authorization.requestID,
    [MEMBERSHIP_HEADERS.authorization]: authorization.authorizationID,
    [MEMBERSHIP_HEADERS.conversation]: authorization.conversationID,
  };
}

let modelVaultAvailable = null;
let protectedClientCapabilities = null;
let clientCapabilitiesPending = null;
let clientAuthenticated = false;
let accountGeneration = 0;
let clientAccountIdentity = '';
let lastAdmittedClientAccountIdentity = '';
let accountRequestGuardInstalled = false;
// Capture the build context once; later DOM changes cannot opt out of login.
const accountFixtureAllowed =
  document.documentElement.dataset.frontendMode !== 'production' &&
  !(typeof window !== 'undefined' &&
    (['wails:', 'file:'].includes(window.location?.protocol) ||
      window.location?.hostname === 'wails.localhost'));

// These endpoints bootstrap or manage account access. Operational endpoints
// never acquire access merely because a build is unprotected or in development.
const ACCOUNT_PUBLIC_REQUESTS = new Set([
  'GET /health', 'HEAD /health', 'GET /agent/client/capabilities',
  'HEAD /agent/client/capabilities', 'HEAD /auth/status',
  'HEAD /entitlement/status', 'HEAD /license/status',
  'GET /auth/admission-status', 'HEAD /auth/admission-status',
  'GET /auth/status', 'POST /auth/start', 'POST /auth/poll',
  'POST /auth/refresh', 'POST /auth/logout', 'GET /entitlement/status',
  'GET /license/status', 'POST /license/login/start', 'POST /license/login/poll',
  // Native validates this against the exact active, expiring login handoff URL.
  'POST /native/open-url', 'POST /agent/browser/cancel',
]);
function accountPublicPath(path, method) {
  const pathname = String(path).split('?')[0];
  // A user must still be able to stop an admitted run after login expires.
  return ACCOUNT_PUBLIC_REQUESTS.has(`${method} ${pathname}`) ||
    (method === 'POST' && /^\/session\/(?!\.{1,2}\/)[^/]+\/(cancel|end)$/.test(pathname));
}

function loginRequiredError() {
  const error = new Error('請先登入 Meadow，再開始使用。');
  error.status = 401;
  error.code = 'LOGIN_REQUIRED';
  error.userMessage = error.message;
  return error;
}

function assertAccountGeneration(generation) {
  if (generation !== accountGeneration || !hasClientAccess())
    throw loginRequiredError();
}

function throwIfRequestAborted(signal) {
  if (!signal?.aborted) return;
  if (signal.reason !== undefined) throw signal.reason;
  const error = new Error('The operation was aborted.');
  error.name = 'AbortError';
  throw error;
}

function waitForAccountAdmission(signal) {
  throwIfRequestAborted(signal);
  if (!signal) return assertClientAuthenticated();
  return new Promise((resolve, reject) => {
    const aborted = () => {
      try { throwIfRequestAborted(signal); } catch (error) { reject(error); }
    };
    signal.addEventListener('abort', aborted, { once: true });
    assertClientAuthenticated().then(resolve, reject).finally(() => {
      signal.removeEventListener('abort', aborted);
    });
  });
}

const ACCOUNT_DENIAL_CODES = new Set([
  'login_required', 'auth_unavailable', 'invalid_auth_state', 'lease_expired',
  'lease_rejected', 'authorization_revoked', 'device_revoked',
  'activation_revoked', 'invalid_refresh_token', 'refresh_reuse_detected',
  'refresh_recovery_unavailable', 'keychain_unavailable', 'device_mismatch',
  'device_binding_unavailable', 'anti_rollback_rejected', 'unknown_lease_key',
]);
function accountDenial(status, data) {
  return status === 401 || (status >= 400 &&
    ACCOUNT_DENIAL_CODES.has(data?.error?.code || data?.code));
}

function revokeClientAuthentication(generation, status, data) {
  // An old denial cannot sign out an account established by a newer login.
  if (generation !== accountGeneration) return;
  setClientAuthentication(null);
  const code = data?.error?.code || data?.code;
  const retryable = status >= 500 &&
    ['auth_unavailable', 'keychain_unavailable', 'invalid_auth_state',
      'device_binding_unavailable'].includes(code);
  // Availability failures still lock every operation. They do not establish
  // logout or revocation: the account UI may recheck native admission only.
  bus.emit('authentication_required', {
    reason: retryable ? 'verification_unavailable' : 'expired', retryable,
  });
}

function frontendAssetPath(path, method) {
  if (method !== 'GET' && method !== 'HEAD') return false;
  if (['/', '/index.html', '/wails/custom.js', '/style.css',
    '/commercial.css', '/product_experience.css'].includes(path)) return true;
  return /^\/(assets|build|css|fonts|icons|images|js|vendor)\//.test(path);
}

function guardedResponseBody(body, check) {
  if (!body) return body;
  return new Proxy(body, {
    get(target, key, receiver) {
      if (key === 'valueOf') return () => receiver;
      if (key === 'getReader') return (...args) => {
        check();
        const reader = target.getReader(...args);
        return new Proxy(reader, {
          get(owner, property, receiver) {
            if (property === 'valueOf') return () => receiver;
            if (property === 'read') return async (...readArgs) => {
              check();
              const chunk = await owner.read(...readArgs);
              check();
              return chunk;
            };
            const value = Reflect.get(owner, property, owner);
            return typeof value === 'function' ? value.bind(owner) : value;
          },
        });
      };
      if (key === 'tee') return () => {
        check();
        return target.tee().map(branch => guardedResponseBody(branch, check));
      };
      if (key === 'pipeTo' || key === 'pipeThrough') return () => {
        // A native pipe may queue a chunk in its destination while this account
        // is valid, then write it after logout. No frontend consumer needs that
        // interface; use Response consumers or a checked reader instead. Reject
        // before locking either stream, preserving each method's return shape.
        const reject = () => {
          check();
          const error = new TypeError('Protected response bodies require an account-checked reader.');
          error.code = 'ACCOUNT_STREAM_PIPE_UNSUPPORTED';
          throw error;
        };
        if (key === 'pipeThrough') return reject();
        return Promise.resolve().then(reject);
      };
      if ((key === Symbol.asyncIterator || key === 'values') &&
          typeof target[key] === 'function') return (...args) => {
        check();
        const iterator = target[key](...args);
        return {
          [Symbol.asyncIterator]() { return this; },
          async next(...nextArgs) {
            check();
            const chunk = await iterator.next(...nextArgs);
            check();
            return chunk;
          },
          return: (...returnArgs) => iterator.return(...returnArgs),
        };
      };
      const value = Reflect.get(target, key, target);
      return typeof value === 'function' ? value.bind(target) : value;
    },
  });
}

function guardedAccountResponse(response, generation, signal) {
  const check = () => {
    throwIfRequestAborted(signal);
    assertAccountGeneration(generation);
  };
  let body;
  return new Proxy(response, {
    get(target, key, receiver) {
      if (key === 'valueOf') return () => receiver;
      if (key === 'body') {
        check();
        return body ??= guardedResponseBody(target.body, check);
      }
      if (key === 'clone') return () => {
        check();
        return guardedAccountResponse(target.clone(), generation, signal);
      };
      if (['json', 'text', 'blob', 'arrayBuffer', 'formData', 'bytes'].includes(key) &&
          typeof target[key] === 'function') return async (...args) => {
        check();
        const result = await target[key](...args);
        check();
        return result;
      };
      const value = Reflect.get(target, key, target);
      return typeof value === 'function' ? value.bind(target) : value;
    },
  });
}

// Install explicitly before account/workspace initialization. Keep the original
// Request and init intact, including streams, credentials and caller cancellation.
// The existing production fetch guard and native authorization remain in force.
export function installAccountRequestGuard() {
  if (accountRequestGuardInstalled) return;
  if (typeof window === 'undefined' || typeof window.fetch !== 'function') return;
  const originalFetch = window.fetch.bind(window);
  const base = new URL(window.location.href);
  window.fetch = async function accountGuardedFetch(input, init) {
    const isRequest = typeof Request !== 'undefined' && input instanceof Request;
    const raw = isRequest ? input.url : String(input);
    const url = new URL(raw, base);
    const method = String(init?.method !== undefined ? init.method :
      (isRequest ? input.method : 'GET')).toUpperCase();
    const signal = init?.signal !== undefined ? init.signal :
      (isRequest ? input.signal : undefined);
    throwIfRequestAborted(signal);
    const sameOrigin = url.protocol === base.protocol && url.host === base.host;
    const publicRequest = sameOrigin &&
      (accountPublicPath(url.pathname, method) || frontendAssetPath(url.pathname, method));
    if (publicRequest) return originalFetch(input, init);
    const generation = clientAccountGeneration();
    if (!hasClientAccess()) await waitForAccountAdmission(signal);
    throwIfRequestAborted(signal);
    assertAccountGeneration(generation);
    const response = await originalFetch(input, init);
    throwIfRequestAborted(signal);
    assertAccountGeneration(generation);
    let denial = response.status === 401;
    let data;
    if (sameOrigin && !response.ok && typeof response.clone === 'function') {
      try { data = await response.clone().json(); } catch { /* non-JSON error */ }
      throwIfRequestAborted(signal);
      assertAccountGeneration(generation);
      denial = accountDenial(response.status, data);
    }
    if (sameOrigin && denial) {
      revokeClientAuthentication(generation, response.status, data);
      throw loginRequiredError();
    }
    return guardedAccountResponse(response, generation, signal);
  };
  accountRequestGuardInstalled = true;
}

async function detectClientCapabilities() {
  if (protectedClientCapabilities) return protectedClientCapabilities;
  if (clientCapabilitiesPending) return clientCapabilitiesPending;
  clientCapabilitiesPending = (async () => {
    const capability = await requestUnchecked('GET', '/agent/client/capabilities');
    if (!capability || typeof capability.protected !== 'boolean' ||
        typeof capability.authenticationRequired !== 'boolean' ||
        (!accountFixtureAllowed && capability.authenticationRequired !== true &&
          !(typeof VM_LAB_BUILD !== 'undefined' && VM_LAB_BUILD && capability.vmLab === true && capability.protected === false)) ||
        (capability.protected && (capability.authenticationRequired !== true ||
          typeof capability.modelVaultRequired !== 'boolean'))) {
      const error = new Error('無法確認登入要求，請稍後重試。');
      error.code = 'CLIENT_CAPABILITIES_UNAVAILABLE';
      throw error;
    }
    protectedClientCapabilities = Object.freeze({ ...capability });
    return protectedClientCapabilities;
  })();
  try { return await clientCapabilitiesPending; }
  finally { clientCapabilitiesPending = null; }
}

async function detectProtectedClient() {
  return (await detectClientCapabilities()).protected;
}

// Called only after the product-auth contract parser accepts an Agent response.
// The Agent independently validates every operation and remains authoritative.
export function setClientAuthentication(status) {
  const wasAuthenticated = clientAuthenticated;
  clientAuthenticated = status?.authenticated === true &&
    (status.state === 'authorized_general' || status.state === 'authorized_pro');
  const identity = clientAuthenticated ? JSON.stringify([
    status.subject || '', status.entitlement?.leaseID || '',
    status.entitlement?.deviceID || '', status.entitlement?.revocationEpoch ?? '',
  ]) : '';
  const identityChanged = identity !== clientAccountIdentity;
  const samePriorAccount = clientAuthenticated &&
    identity === lastAdmittedClientAccountIdentity;
  if (!clientAuthenticated || !wasAuthenticated || identityChanged)
    accountGeneration += 1;
  clientAccountIdentity = identity;
  // Keep the last verified identity across logout so Stop can still address the
  // old session while signed out. A different login must establish a new session;
  // neither an empty current identity nor a shared subject implies continuity.
  if (clientAuthenticated) lastAdmittedClientAccountIdentity = identity;
  if (!clientAuthenticated || !samePriorAccount) disconnectEvents();
  if (clientAuthenticated && !samePriorAccount) retainedEventProjection = null;
  if (clientAuthenticated && !samePriorAccount && state.sessionId) {
    state.sessionId = null;
    state.runId = '';
    state.busy = false;
    state.hasMessages = false;
    sessionSelectionRevision += 1;
  } else if (!wasAuthenticated && samePriorAccount && state.sessionId) {
    connectEventStream(state.sessionId, undefined, true);
  }
}

export function hasClientAccess() {
  return Boolean(protectedClientCapabilities &&
    (protectedClientCapabilities.authenticationRequired === false || clientAuthenticated));
}

export function clientAccountGeneration() {
  return accountGeneration;
}

export async function assertClientAuthenticated() {
  const capability = await detectClientCapabilities();
  if (capability.authenticationRequired === false || clientAuthenticated) return;
  throw loginRequiredError();
}

async function detectModelVault() {
  if (modelVaultAvailable !== null) return modelVaultAvailable;
  const protectedMode = await detectProtectedClient();
  // Public/local editions ship with the user's configured model paths.  The
  // Agent may still expose catalog-shaped endpoints for compatibility, but
  // those must never turn the Desktop into the old download/onboarding flow.
  if (!protectedMode) {
    modelVaultAvailable = false;
    return modelVaultAvailable;
  }
  // Protected editions with bundled/configured product models use the real
  // Agent-owned /model/list picker. The signed Model Vault is an independent
  // install/update facility and must not replace the picker when disabled.
  if (protectedClientCapabilities?.modelVaultRequired !== true) {
    modelVaultAvailable = false;
    return modelVaultAvailable;
  }
  try {
    const status = await get('/agent/models/catalog/status');
    if (!status || typeof status.installAvailable !== 'boolean')
      throw new Error('invalid model catalog status');
    modelVaultAvailable = true;
  } catch (error) {
    if (!protectedMode && error && error.status === 404)
      modelVaultAvailable = false;
    else throw error;
  }
  return modelVaultAvailable;
}

async function managedModelList() {
  if (!(await detectModelVault())) return get('/model/list');
  const [catalog, installed] = await Promise.all([
    get('/agent/models/catalog'),
    get('/agent/models'),
  ]);
  const local = new Map(
    ((installed && installed.models) || []).map((model) => [
      `${model.modelId}\u0000${model.version}`,
      model,
    ]),
  );
  return ((catalog && catalog.models) || []).map((entry) => {
    const current =
      local.get(`${entry.modelId}\u0000${entry.version}`) || entry;
    return {
      modelId: entry.modelId,
      name: entry.modelId,
      displayName: entry.modelId,
      description: entry.profileId || '',
      version: entry.version,
      profileId: entry.profileId,
      licenseId: entry.licenseId,
      official: true,
      installed: !!current.installed,
      present: !!current.installed,
      runtimeReady: !!current.runtimeReady,
      live: !!current.runtimeReady,
      active: !!current.runtimeReady,
      status: current.runtimeReady
        ? 'ready'
        : current.installed
          ? 'installed'
          : 'available',
    };
  });
}

async function managedModelStatus() {
  // Desktop owns the edition/artifact compatibility check. Agent resident
  // status alone cannot distinguish the public "Meadow Coder 35B" ID backed
  // by the standard artifact from the Huihui artifact used by /badai.
  return get('/model/status');
}

async function managedModelActivate(modelId) {
  const selected =
    typeof modelId === 'string'
      ? modelId.trim()
      : String((modelId && (modelId.modelId || modelId.model)) || '').trim();
  if (!selected) throw new Error('model is required');
  return post('/agent/model', { model: selected });
}

async function managedModelUnload() {
  return del('/agent/model');
}

// --- application API ---
export const daemon = {
  createSession: (cwd, authorization) =>
    request(
      'POST',
      '/session',
      {
        cwd,
        sessionScope: 'thread',
        ...membershipAuthorizationFields(authorization),
      },
      membershipAuthorizationHeaders(authorization),
    ),
  membershipUsage: () => get('/commercial/usage'),
  authorizeNewConversation: (body) =>
    post('/commercial/authorize-new-conversation', body),
  authorizePrompt: (body) => post('/commercial/authorize-prompt', body),
  preparePromptGrant: () => post('/commercial/prepare-prompt-grant', { schema: 'meadow.desktop-prompt-grant.v1' }),
  authAdmissionStatus: (options = {}) => request('GET', '/auth/admission-status',
    undefined, {}, undefined, options.signal),
  consumePrompt: (body) => post('/commercial/consume-prompt', body),
  referralShare: (body) => post('/commercial/referral/share', body),
  referralAccept: (body) => post('/commercial/referral/accept', body),
  referralStatus: () => get('/commercial/referral/status'),
  // Blocks until the turn ends; resolves {stopReason}. Attachments are image
  // data URLs or PDF descriptors. Core renders PDF pages for the vision path;
  // structured payloads stay separate from visible timeline rendering.
  prompt: async (sid, text, attachments, options, authorization) => {
    const confirmedEffort = state.runBudgetControl?.sessionId === sid
      ? state.runBudgetControl.effort : undefined;
    const resolvedOptions = { ...runLimitOptions(confirmedEffort), ...(options || {}) };
    const prompt = promptBlocks(text, attachments);
    resolvedOptions.mode =
      resolvedOptions.mode || (state.researchMode ? 'research' : 'auto');
    const body = {
      prompt,
      options: resolvedOptions,
      ...membershipAuthorizationFields(authorization),
    };
    return deliverPrompt(
      `/session/${sid}/prompt`,
      sid,
      body,
      membershipAuthorizationHeaders(authorization),
      async () => {
        // Establish native Browser ownership before any model page action,
        // inside the cancellable delivery lifecycle.
        const browser = await import('./browserDrawer.js?v=desktop-browser-drawer-20260904c');
        await browser.bindEmbeddedBrowserOwner(sid);
      },
    );
  },
  queueList: (sid) => get(`/session/${sid}/queue`),
  queueAdd: async (sid, text, attachments, options, authorization) => {
    const selectionRevision = sessionSelectionRevision;
    const stopEpoch = promptAdmissionStopEpochs.get(sid) || 0;
    const body = {
      ...membershipAuthorizationFields(authorization),
      prompt: promptBlocks(text, attachments),
      options: {
        ...runLimitOptions(state.runBudgetControl?.sessionId === sid
          ? state.runBudgetControl.effort : undefined),
        ...(options || {}),
        mode: (options && options.mode) ||
          (state.researchMode ? 'research' : 'auto'),
      },
    };
    await confirmPromptAdmission(sid, body.options);
    if (selectionRevision !== sessionSelectionRevision)
      throw new Error('對話已切換，任務尚未排入佇列');
    if ((promptAdmissionStopEpochs.get(sid) || 0) !== stopEpoch)
      throw new Error('已停止，任務未排入佇列');
    return request(
      'POST',
      `/session/${sid}/queue`,
      body,
      { ...membershipAuthorizationHeaders(authorization),
        ...(authorization ? { [PROMPT_REQUEST_ID_HEADER]: authorization.requestID } : {}) },
    );
  },
  queueMove: (sid, queueId, direction) =>
    post(`/session/${sid}/queue/${encodeURIComponent(queueId)}/move`, {
      direction,
    }),
  queueReorder: (sid, queueId, target) =>
    post(`/session/${sid}/queue/${encodeURIComponent(queueId)}/move`, target),
  queueGuide: (sid, queueId) =>
    post(`/session/${sid}/queue/${encodeURIComponent(queueId)}/guide`),
  queueEdit: (sid, queueId) =>
    post(`/session/${sid}/queue/${encodeURIComponent(queueId)}/edit`),
  queueCancel: (sid, queueId) =>
    del(`/session/${sid}/queue/${encodeURIComponent(queueId)}`),
  taskList: (sid) => get(`/session/${sid}/tasks`),
  taskPlan: (sid, signal) =>
    request('GET', `/session/${encodeURIComponent(sid)}/task-plan`, undefined, {}, undefined, signal),
  taskListAll: (cwd) => get(`/tasks?cwd=${encodeURIComponent(cwd || '')}`),
  sourceList: (sid) => get(`/session/${sid}/sources`),
  placeList: (sid) => get(`/session/${encodeURIComponent(sid)}/places`),
  mapConfig: (sid) => get(`/session/${encodeURIComponent(sid)}/map-config`),
  locationRequests: (sid) => get(`/session/${encodeURIComponent(sid)}/location`),
  replyLocation: (sid, requestId, result) =>
    post(`/session/${encodeURIComponent(sid)}/location/${encodeURIComponent(requestId)}`, result),
  contextStatus: (sid) => get(`/session/${sid}/context`),
  compactContext: (sid) => post(`/session/${sid}/compact`),
  steer: (sid, text, attachments, authorization) =>
    request('POST', `/session/${sid}/steer`, {
      prompt: promptBlocks(text, attachments),
      ...membershipAuthorizationFields(authorization),
    }, { ...membershipAuthorizationHeaders(authorization),
      ...(authorization ? { [PROMPT_REQUEST_ID_HEADER]: authorization.requestID } : {}) }),
  cancel: (sid) => cancelPromptDelivery(sid),
  // Wails/WebKit has intermittently presented an empty POST body for this
  // hot-path route. Mirror the small non-secret mode in a fixed header so the
  // Desktop shell can validate and re-encode it before crossing the private
  // Agent socket.
  setApproval: (sid, mode) =>
    request(
      'POST',
      `/session/${sid}/approval`,
      { mode },
      { 'X-Meadow-Approval-Mode': String(mode || '') },
    ),
  vote: (sid, requestId, optionId) =>
    post(`/session/${sid}/permission/${requestId}`, {
      outcome: { outcome: 'selected', optionId },
    }),
  answer: (sid, requestId, answers) =>
    post(`/session/${sid}/user-input/${requestId}`, { answers }),
  rollback: (sid, numTurns) => post(`/session/${sid}/rollback`, { numTurns }),
  // WebKit/Wails can surface an empty POST body on this private hot-path
  // route. Mirror the small non-secret model id in a fixed header so the
  // Desktop shell can validate and re-encode it before crossing the Agent
  // socket, matching the approval-mode bridge below.
  setModel: (sid, modelId) =>
    request(
      'POST',
      `/session/${sid}/model`,
      { modelId },
      { 'X-Meadow-Model-ID': String(modelId || '') },
    ),
  inferenceControls: (sid) => get(`/session/${sid}/inference-controls`),
  setInferenceControls: (sid, controls) =>
    post(`/session/${sid}/inference-controls`, controls),
  evoStatus: (sid) => get(`/session/${encodeURIComponent(sid)}/evo`),
  harnessControls: (sid) =>
    get(`/session/${encodeURIComponent(sid)}/harness-controls`),
  setHarnessControls: (sid, controls) =>
    post(`/session/${encodeURIComponent(sid)}/harness-controls`, controls),
  switchEdition: (profile) => post('/native/edition', { profile }),
  listSessions: (cwd, signal) => request('GET', `/sessions?cwd=${encodeURIComponent(cwd)}`, undefined, {}, undefined, signal),
  searchSessions: (query, cwd, limit = 20) =>
    get(
      `/sessions/search?q=${encodeURIComponent(query || '')}&cwd=${encodeURIComponent(cwd || '')}&limit=${encodeURIComponent(limit)}`,
    ),
  // Stored-chat history (extras.go); cwd scopes the workspace lookup.
  history: (sid, cwd) =>
    get(
      `/sessions/${encodeURIComponent(sid)}/history?cwd=${encodeURIComponent(cwd || '')}`,
    ),
  pickFolder: () => post('/native/pick-folder'),
  workspaceBootstrap: () => get('/workspace/bootstrap'),
  modelStatus: managedModelStatus,
  modelList: managedModelList,
  // Cold-model load progress is followed by polling modelStatus().
  loadModel: managedModelActivate,
  unloadModel: managedModelUnload,
  memStats: () => get('/native/memstats'),
  revealPath: (path, cwd, sessionId) =>
    post('/native/reveal-path', { path, cwd, sessionId }),
  // Open a concrete workspace artifact in the user's configured editor.  The
  // native endpoint owns editor selection and path validation; the UI only
  // passes a path that was just read from the active workspace.
  openEditor: (path, editor) =>
    post('/native/open-editor', { path, editor: editor || '' }),
  // Model download (first-run): catalog with presence/recommend, start a
  // resumable pull, poll its progress, cancel (keeps the .partial for resume).
  modelCatalog: () => get('/native/model-catalog'),
  pullModel: (model) => post('/native/pull-model', { model }),
  pullStatus: () => get('/native/pull-status'),
  pullCancel: () => post('/native/pull-cancel'),
  modelVaultCatalogStatus: () => get('/agent/models/catalog/status'),
  protectedClientMode: detectProtectedClient,
  clientCapabilities: detectClientCapabilities,
  modelVaultCatalog: () => get('/agent/models/catalog'),
  modelVaultList: () => get('/agent/models'),
  modelVaultStatus: (modelId, version) =>
    post('/agent/models/status', { modelId, version }),
  modelVaultInstall: (modelId, version, licenseSubject) =>
    post('/agent/models/install', {
      modelId,
      version,
      licenseAccepted: true,
      licenseSubject,
    }),
  modelVaultRemove: (modelId, version) =>
    post('/agent/models/remove', { modelId, version, confirmed: true }),
  modelVaultImport: () => post('/agent/models/import'),
  activateInstalledModel: (modelId) => post('/agent/model', { model: modelId }),
  authStatus: () => get('/auth/status'),
  authStart: (body, { signal } = {}) =>
    request('POST', '/auth/start', body, {}, undefined, signal),
  authPoll: (body) => post('/auth/poll', body),
  authRefresh: (body) => post('/auth/refresh', body),
  authLogout: (body) => post('/auth/logout', body),
  entitlementStatus: () => get('/entitlement/status'),
  licenseRedeem: (body) => post('/license/redeem', body),
  deviceList: (body) => post('/devices/list', body),
  deviceRevoke: (body) => post('/devices/revoke', body),
  updateCheck: () => post('/agent/update/check'),
  updateStatus: () => get('/agent/update/status'),
  updateStage: () => post('/agent/update/stage'),
  updateInstall: (stagedId) => post('/agent/update/install', { stagedId }),
  openURL: (url) => post('/native/open-url', { url }),
  notify: (title, body) => post('/native/notify', { title, body }),
};

// --- pub/sub ---
const listeners = new Map();
export const bus = {
  on(type, fn) {
    if (!listeners.has(type)) listeners.set(type, new Set());
    listeners.get(type).add(fn);
    return () => bus.off(type, fn);
  },
  off(type, fn) {
    const set = listeners.get(type);
    if (set) set.delete(fn);
  },
  emit(type, data) {
    const set = listeners.get(type);
    if (!set) return;
    for (const fn of [...set]) {
      try {
        fn(data);
      } catch (e) {
        console.error(`bus handler for "${type}" threw`, e);
      }
    }
  },
};

// --- shared mutable state ---
// hasMessages: true once the active thread has any conversation content
// (live user chunk or resumed history); composer.js uses it to lock the
// folder pill — a started chat stays bound to its folder.
export const state = {
  sessionId: null,
  runId: '',
  cwd: null,
  busy: false,
  model: '',
  hasMessages: false,
  researchMode: false,
};

// Session creation can race the Agent's first RuntimeControl ensure during a
// cold Desktop launch.  Keep one shared in-flight promise so boot retries and
// an impatient first send cannot create two conversations, then publish the
// session through the same state/event path used everywhere else.
let sessionCreation = null;
let sessionSelectionRevision = 0;
let eventProjectionRevision = 0;
bus.on('thread:switched', () => {
  sessionSelectionRevision++;
  eventProjectionRevision++;
});
bus.on('history:load', () => { eventProjectionRevision++; });

export async function ensureSession(cwd, authorization) {
  if (state.sessionId) {
    return {
      sessionId: state.sessionId,
      modelId: state.model,
      workspaceCwd: state.cwd,
    };
  }
  if (sessionCreation) return sessionCreation;
  const selectedRevision = sessionSelectionRevision;
  sessionCreation = (async () => {
    const session = await daemon.createSession(cwd, authorization);
    const sessionID = String(session?.sessionId || '').trim();
    if (!sessionID) throw new Error('session response missing sessionId');
    if (state.sessionId || sessionSelectionRevision !== selectedRevision) {
      return {
        sessionId: state.sessionId,
        modelId: state.model,
        workspaceCwd: state.cwd,
      };
    }
    state.sessionId = sessionID;
    state.runId = '';
    state.model = String(session?.modelId || '').trim();
    state.modelConnectionID = String(session?.connectionId || '').trim();
    if (session?.workspaceCwd) state.cwd = session.workspaceCwd;
    connectEvents(sessionID);
    bus.emit('session_created', session);
    return session;
  })();
  try {
    return await sessionCreation;
  } finally {
    sessionCreation = null;
  }
}

// --- SSE ---
// Envelope: {v:1, type, data, id}. Only allowlisted structured events are
// forwarded to renderers.
const EVENT_TYPES = [
  'session_update',
  'permission_request',
  'permission_resolved',
  'user_input_request',
  'user_input_resolved',
  'usage',
  'inference_progress',
  'guidance',
  'stream_error',
  'approval_mode_changed',
  'model_switched',
  'inference_controls_changed',
  'harness_controls_changed',
  'evo_state',
  'model_escalation',
  'queue_changed',
  'background_task',
  'background_tasks',
  'task_plan_update',
  'work_center_error',
  'research_sources',
  'place_results',
  'location_request',
  'location_resolved',
  'run_state',
  'context_compaction',
  'turn_completed',
  'run_recap',
];

const SENSITIVE_EVENT_KEY =
  /^(?:authorization|accessToken|refreshToken|apiKey|secret|socket|socketPath|manifestKey|modelPath|backend|policy|systemPrompt|resumePrompt)$/i;

function sanitizeEventData(value, depth, allowResumePrompt) {
  const level = depth || 0;
  if (level > 10) return null;
  if (value == null || typeof value === 'boolean' || typeof value === 'number')
    return value;
  if (typeof value === 'string') return value;
  if (Array.isArray(value))
    return value
      .slice(0, 1000)
      .map((item) => sanitizeEventData(item, level + 1, allowResumePrompt));
  if (typeof value !== 'object') return null;
  const clean = {};
  for (const [key, item] of Object.entries(value)) {
    if (
      allowResumePrompt &&
      level === 0 &&
      key === 'resumePrompt' &&
      typeof item === 'string'
    ) {
      clean[key] = item.slice(0, 2000);
      continue;
    }
    if (SENSITIVE_EVENT_KEY.test(key)) continue;
    clean[key] = sanitizeEventData(item, level + 1, allowResumePrompt);
  }
  return clean;
}

// A model-escalation resume prompt is a bounded, server-authored continuation
// instruction that the user explicitly accepts by clicking the handoff card.
// Keep the general secret filter intact for every other event and for nested
// prompt-like fields; only this one top-level product contract may pass.
export function sanitizePublicEventData(type, value) {
  return sanitizeEventData(value, 0, type === 'model_escalation');
}

let activeConnection = null;
let retainedEventProjection = null;
// The native replay ring retains 8,000 frames. Keep its whole overlap bounded
// without comparing message text or treating an unseen lower ID as a duplicate.
const EVENT_REPLAY_ID_LIMIT = 8192;

function eventCursor(value) {
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value) || value < 0) return '';
    value = String(value);
  }
  if (typeof value !== 'string' || !/^\d{1,20}$/.test(value)) return '';
  return value.replace(/^0+(?=\d)/, '');
}

function compareEventCursors(a, b) {
  return a.length === b.length ? (a === b ? 0 : a < b ? -1 : 1) : a.length < b.length ? -1 : 1;
}

export function connectEvents(sid, lastEventId) {
  // Public callers repaint/resume history or select a fresh session. They own
  // a NEW projection, including an explicit cursor of zero for a full replay.
  return connectEventStream(sid, lastEventId, false);
}

function connectEventStream(sid, lastEventId, resumeProjection) {
  if (!protectedClientCapabilities ||
      (protectedClientCapabilities.authenticationRequired && !clientAuthenticated)) {
    return () => {};
  }
  disconnectEvents();
  const sessionId = String(sid || '').trim();
  const generation = clientAccountGeneration();
  const retained = retainedEventProjection;
  const cursor = eventCursor(lastEventId);
  const projection = resumeProjection && retained && retained.sessionId === sessionId &&
    retained.accountIdentity === clientAccountIdentity && retained.revision === eventProjectionRevision
    ? retained : {
      sessionId, accountIdentity: clientAccountIdentity, revision: eventProjectionRevision,
      lastId: cursor, snapshotCursor: cursor, seenIds: new Set(),
    };
  const connection = {
    sessionId,
    projection,
    source: null,
    retryTimer: null,
    retryMs: 1000, // hard-reconnect backoff: 1s → 30s cap, reset on open
    closed: false,
    disconnect: null,
  };
  activeConnection = connection;

  function isCurrent() {
    return (
      !connection.closed &&
      generation === clientAccountGeneration() && hasClientAccess() &&
      activeConnection === connection &&
      String(state.sessionId || '') === sessionId
    );
  }

  function open() {
    if (!isCurrent()) return;
    if (connection.retryTimer) {
      clearTimeout(connection.retryTimer);
      connection.retryTimer = null;
    }
    const qs = projection.lastId ? `?lastEventId=${encodeURIComponent(projection.lastId)}` : '';
    const cur = new EventSource(`/session/${sessionId}/events${qs}`);
    connection.source = cur;

    const handle = (e) => {
      // EventSource listeners remain dispatchable after close(), and a hard
      // reconnect timer can fire after the user switches threads. Treat the
      // connection identity and the selected session as the ownership fence:
      // an old stream must never mutate the new thread's timeline or drawers.
      if (!isCurrent() || connection.source !== cur) return;
      let envelope;
      try {
        envelope = JSON.parse(e.data);
      } catch {
        return;
      }
      const type = (envelope && envelope.type) || e.type;
      if (!EVENT_TYPES.includes(type)) return;
      const rawID = eventCursor(e.lastEventId) || eventCursor(envelope?.id);
      const eventID = rawID === '0' ? '' : rawID;
      if (eventID) {
        if (projection.seenIds.has(eventID) || (projection.snapshotCursor &&
            compareEventCursors(eventID, projection.snapshotCursor) <= 0)) return;
        projection.seenIds.add(eventID);
        if (projection.seenIds.size > EVENT_REPLAY_ID_LIMIT)
          projection.seenIds.delete(projection.seenIds.values().next().value);
        if (!projection.lastId || compareEventCursors(eventID, projection.lastId) > 0)
          projection.lastId = eventID;
      }
      const sanitized = sanitizePublicEventData(
        type,
        (envelope && envelope.data) || {},
      );
      // The SSE URL is session-scoped even when an older daemon omits the
      // sessionId field inside data. Preserve that ownership on every public
      // bus event so renderers can apply their own defensive filter too.
      const data =
        sanitized && typeof sanitized === 'object' && !Array.isArray(sanitized)
          ? { ...sanitized, sessionId }
          : { value: sanitized, sessionId };
      if (type === 'run_state' && data.status === 'running' && data.runId) {
        state.runId = String(data.runId);
      } else if (
        type === 'session_update' &&
        !state.runId &&
        data.update &&
        data.update.runId
      ) {
        // Backward-compatible recovery when a replay starts after run_state.
        state.runId = String(data.update.runId);
      }
      bus.emit(type, data);
      if (
        type === 'session_update' &&
        data.update &&
        data.update.sessionUpdate
      ) {
        const updateType = String(data.update.sessionUpdate || '');
        if (/^[a-z][a-z0-9_]{0,63}$/.test(updateType)) {
          bus.emit('update:' + updateType, {
            ...data.update,
            sessionId,
          });
        }
      }
    };

    for (const t of EVENT_TYPES) cur.addEventListener(t, handle);
    cur.onmessage = handle;
    cur.onopen = () => {
      if (!isCurrent() || connection.source !== cur) return;
      connection.retryMs = 1000;
      bus.emit('sse_open', { sessionId });
    };
    cur.onerror = () => {
      if (!isCurrent() || connection.source !== cur) return;
      bus.emit('sse_error', { sessionId });
      // CONNECTING = native retry in flight (carries Last-Event-ID header).
      // CLOSED = hard failure: rebuild after a beat, replaying from lastId.
      // Backoff keeps a permanently-404 session (stale thread) quiet.
      if (cur.readyState === EventSource.CLOSED && !connection.retryTimer) {
        const delay = connection.retryMs;
        connection.retryMs = Math.min(connection.retryMs * 2, 30000);
        connection.retryTimer = setTimeout(() => {
          connection.retryTimer = null;
          open();
        }, delay);
      }
    };
  }

  function disconnect() {
    if (connection.closed) return;
    connection.closed = true;
    if (connection.retryTimer) {
      clearTimeout(connection.retryTimer);
      connection.retryTimer = null;
    }
    if (connection.source) {
      connection.source.close();
      connection.source = null;
    }
    if (activeConnection === connection) {
      retainedEventProjection = projection;
      activeConnection = null;
    }
  }
  connection.disconnect = disconnect;
  open();
  return disconnect;
}

export function disconnectEvents() {
  if (activeConnection) activeConnection.disconnect();
}

// --- markdown (XSS-safe: escape everything first, then transform) ---
export function esc(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

export function md(text) {
  let s = esc(text == null ? '' : String(text));
  // fenced code blocks first so inner markup stays literal
  s = s.replace(
    /```\w*\n?([\s\S]*?)```/g,
    (_, code) => `<pre><code>${code.replace(/\n$/, '')}</code></pre>`,
  );
  s = s.replace(/`([^`\n]+)`/g, '<code>$1</code>');
  s = s.replace(/\*\*([^*\n]+)\*\*/g, '<b>$1</b>');
  s = s.replace(/^### (.*)$/gm, '<h4>$1</h4>');
  s = s.replace(/^## (.*)$/gm, '<h3>$1</h3>');
  s = s.replace(/^# (.*)$/gm, '<h3>$1</h3>');
  // http(s) only; quotes are already escaped so attributes cannot break out
  s = s.replace(
    /\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\)/g,
    '<a href="$2" target="_blank" rel="noopener">$1</a>',
  );
  s = s.replace(/^[-*] (.*)$/gm, '• $1');
  return s;
}
