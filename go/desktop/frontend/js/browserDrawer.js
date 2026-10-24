// In-panel browser for the Desktop right drawer. The page itself is the
// browser (native WKWebView overlay, iframe fallback in tests). DOM / console /
// eval stay available for the agent but are not the primary UI.
import { bus, state, assertClientAuthenticated } from './core.js?v=raven-execution-20261006a';
import { showRightPane, toggleRightPane } from './rightpanel.js';

const $ = (selector) => document.querySelector(selector);
const MAX_EVIDENCE_CHARS = 12000;
let bindActiveOwner = null;
let browserAccountRevision = 0;

// Prompt admission can await this after choosing its session, before tools use
// the native pane. Event-driven layout updates alone are not an admission gate.
export async function bindEmbeddedBrowserOwner(expectedSessionID = state.sessionId) {
  const sessionID = String(expectedSessionID || '');
  const workspaceRoot = String(state.cwd || '');
  if (sessionID !== String(state.sessionId || '')) {
    throw new Error('Browser owner does not match the active conversation');
  }
  await assertClientAuthenticated();
  if (sessionID !== String(state.sessionId || '') || workspaceRoot !== String(state.cwd || '')) {
    throw new Error('Browser owner changed before navigation admission');
  }
  if (bindActiveOwner) return bindActiveOwner(sessionID, workspaceRoot);
  let capability;
  try { capability = await request('GET', '/native/browser-embed'); }
  catch { /* Legacy iframe-only hosts have no native endpoint. */ }
  if (sessionID !== String(state.sessionId || '') || workspaceRoot !== String(state.cwd || '')) {
    throw new Error('Browser owner changed before navigation admission');
  }
  if (capability?.agent_control === true) {
    throw new Error('Native browser drawer is not initialized');
  }
  return null;
}

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
}

async function request(method, path, body) {
  await assertClientAuthenticated();
  const accountRevision = browserAccountRevision;
  const response = await fetch(path, {
    method,
    headers:
      body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let value = null;
  try {
    value = await response.json();
  } catch {
    /* screenshot is binary */
  }
  if (accountRevision !== browserAccountRevision) {
    const error = new Error('登入狀態已變更，請重新操作。');
    error.status = 401;
    throw error;
  }
  if (!response.ok) {
    const detail =
      value?.error?.message || value?.error || `HTTP ${response.status}`;
    const error = new Error(String(detail));
    error.status = response.status;
    throw error;
  }
  return value || {};
}

function toolURL(update) {
  const input = (update && (update.rawInput || update.input)) || {};
  const raw = String(input.url || '').trim();
  if (/^https?:\/\//i.test(raw)) return raw;
  return '';
}

function conversationID() {
  const raw = String(state.sessionId || 'desktop-browser').trim();
  const safe = raw.replace(/[^A-Za-z0-9_-]/g, '_').slice(0, 128);
  return safe || 'desktop-browser';
}

function evidenceText(value) {
  const raw =
    typeof value === 'string' ? value : JSON.stringify(value, null, 2);
  if (raw.length <= MAX_EVIDENCE_CHARS) return raw;
  return `${raw.slice(0, MAX_EVIDENCE_CHARS)}\n…（證據已截斷）`;
}

function normalURL(value) {
  const raw = String(value || '').trim();
  if (!raw) return '';
  if (/^about:/i.test(raw)) return raw;
  return /^[a-z][a-z0-9+.-]*:\/\//i.test(raw) ? raw : `https://${raw}`;
}

export function initBrowserDrawer() {
  const panel = $('#rightPanel');
  const tabs = $('#rpTabs');
  const wrap = $('#rpWrap');
  if (!panel || !tabs || !wrap || $('#rpTabBrowser')) return;

  const tab = el('button', 'rp-tab', '瀏覽器');
  tab.id = 'rpTabBrowser';
  tab.type = 'button';
  tab.setAttribute('role', 'tab');
  tab.setAttribute('aria-selected', 'false');
  tab.setAttribute('aria-controls', 'rpPageBrowser');
  const previewTab = $('#rpTabPreview');
  tabs.insertBefore(tab, previewTab || null);

  const page = el('section', 'rp-page bd-page');
  page.id = 'rpPageBrowser';
  page.setAttribute('role', 'tabpanel');
  page.setAttribute('aria-labelledby', 'rpTabBrowser');

  const chrome = el('div', 'bd-chrome');
  const backButton = el('button', 'bd-icon', '‹');
  const forwardButton = el('button', 'bd-icon', '›');
  const reloadButton = el('button', 'bd-icon', '↻');
  backButton.type = 'button';
  forwardButton.type = 'button';
  reloadButton.type = 'button';
  backButton.title = '上一頁';
  forwardButton.title = '下一頁';
  reloadButton.title = '重新整理';
  const address = document.createElement('input');
  address.className = 'bd-address';
  address.type = 'url';
  address.placeholder = '搜尋或輸入網址';
  address.setAttribute('aria-label', '網頁網址');
  const goButton = el('button', 'bd-primary', '前往');
  goButton.type = 'button';
  chrome.append(backButton, forwardButton, reloadButton, address, goButton);

  const viewport = el('div', 'bd-viewport');
  viewport.id = 'bdViewport';
  const frame = document.createElement('iframe');
  frame.className = 'bd-frame';
  frame.title = '內嵌瀏覽器';
  frame.setAttribute(
    'sandbox',
    'allow-scripts allow-same-origin allow-forms allow-popups allow-modals allow-downloads',
  );
  frame.referrerPolicy = 'no-referrer-when-downgrade';
  const empty = el('div', 'bd-empty', '輸入網址後，頁面會直接顯示在這裡。');
  viewport.append(frame, empty);

  const debug = document.createElement('details');
  debug.className = 'bd-debug';
  debug.innerHTML = '<summary>AI 除錯資料</summary>';
  const debugBody = el('div', 'bd-debug-body');
  const addButton = el('button', 'bd-secondary', '把目前頁面證據加入對話');
  addButton.type = 'button';
  addButton.disabled = true;
  const domOutput = el('pre', 'bd-output', '尚無 DOM');
  const consoleOutput = el('pre', 'bd-output', '尚無 Console');
  const script = document.createElement('textarea');
  script.className = 'bd-script';
  script.spellcheck = false;
  script.placeholder = 'document.title';
  script.setAttribute('aria-label', '頁面 JavaScript');
  const runButton = el('button', 'bd-secondary', '對目前頁面執行');
  runButton.type = 'button';
  const evalOutput = el('pre', 'bd-output', '尚未執行');
  debugBody.append(
    addButton,
    el('div', 'bd-debug-label', 'DOM'),
    domOutput,
    el('div', 'bd-debug-label', 'Console'),
    consoleOutput,
    el('div', 'bd-debug-label', 'JavaScript'),
    script,
    runButton,
    evalOutput,
  );
  debug.appendChild(debugBody);
  page.append(chrome, viewport, debug);
  wrap.appendChild(page);

  let embedded = false;
  let agentControl = false;
  let nativeStatusLoaded = false;
  let pendingAgentBrowse = null;
  let pendingNativeNavigation = null;
  let embedRequests = Promise.resolve();
  let ownerRevision = 0;
  let latestSession = null;
  let latestDOM = null;
  let latestConsole = null;

  function panelOpen() {
    return panel.classList.contains('open') && page.classList.contains('on');
  }

  function showEmpty(message) {
    empty.hidden = false;
    empty.textContent = message || '輸入網址後，頁面會直接顯示在這裡。';
    if (!embedded) frame.removeAttribute('src');
  }

  function currentOwner() {
    return {
      session_id: String(state.sessionId || ''),
      workspace_root: String(state.cwd || ''),
    };
  }

  function matchesOwner(owner) {
    return owner.session_id === String(state.sessionId || '') &&
      owner.workspace_root === String(state.cwd || '');
  }

  function postEmbed(body, owner = currentOwner()) {
    if (ownerRevision >= Number.MAX_SAFE_INTEGER) {
      return Promise.reject(new Error('Native browser ownership revision is exhausted'));
    }
    const revision = ++ownerRevision;
    // Keep ownership/layout requests ordered across a rapid session switch.
    const next = embedRequests.catch(() => {}).then(async () => {
      if (!matchesOwner(owner)) return null;
      const receipt = await request('POST', '/native/browser-embed', {
        ...body, ...owner, owner_revision: revision,
      });
      if (agentControl && (receipt?.ok !== true ||
          receipt.owner_revision !== revision ||
          receipt.owner_session_id !== owner.session_id ||
          receipt.requested_workspace_root !== owner.workspace_root)) {
        throw new Error('Native browser ownership acknowledgment does not match the request');
      }
      return receipt;
    });
    embedRequests = next;
    return next;
  }

  function syncFrame(owner) {
    if (!embedded) return Promise.resolve(null);
    const box = viewport.getBoundingClientRect();
    const visible =
      panelOpen() &&
      !wrap.classList.contains('rp-dragging') &&
      box.width > 8 &&
      box.height > 8;
    return postEmbed({
      visible,
      x: box.left,
      y: box.top,
      width: box.width,
      height: box.height,
    }, owner);
  }

  function activate(automatic = false) {
    showRightPane(page.id, { automatic });
    requestAnimationFrame(() => void syncFrame().catch(() => {}));
  }

  async function refreshEvidence(owner = currentOwner()) {
    if (!matchesOwner(owner)) return;
    const sessionID = conversationID();
    const sessionResult = await request(
      'GET',
      `/browser/session?session_id=${encodeURIComponent(sessionID)}`,
    );
    if (!matchesOwner(owner)) return;
    latestSession = sessionResult.session || null;
    if (latestSession?.url) address.value = latestSession.url;
    const [dom, logs] = await Promise.all([
      request('POST', '/browser/dom', { session_id: sessionID, max_depth: 8 }),
      request(
        'GET',
        `/browser/console?session_id=${encodeURIComponent(sessionID)}&limit=100`,
      ),
    ]);
    if (!matchesOwner(owner)) return;
    latestDOM = dom;
    latestConsole = logs;
    domOutput.textContent = evidenceText(dom.tree || dom);
    consoleOutput.textContent = evidenceText(logs.logs || logs);
    addButton.disabled = !latestSession?.url;
  }

  async function open(raw) {
    const owner = currentOwner();
    const url = normalURL(raw ?? address.value);
    if (!url) {
      address.focus();
      return;
    }
    goButton.disabled = true;
    empty.hidden = true;
    try {
      await assertClientAuthenticated();
      await nativeStatusReady;
      if (!matchesOwner(owner)) return;
      if (embedded) await syncFrame(owner);
      if (!matchesOwner(owner)) return;
      const result = await request('POST', '/browser/open', {
        session_id: conversationID(),
        url,
      });
      if (!matchesOwner(owner)) return;
      latestSession = result.session || null;
      if (!embedded) frame.src = url;
      address.value = latestSession?.url || url;
      await refreshEvidence(owner).catch(() => {});
      if (matchesOwner(owner)) void syncFrame(owner).catch(() => {});
    } catch (error) {
      if (!matchesOwner(owner)) return;
      const signedIn = await assertClientAuthenticated().then(() => true, () => false);
      if (!signedIn || error?.status === 401 || error?.status === 403) {
        showEmpty('請先登入 Meadow，再使用瀏覽器。');
        return;
      }
      if (!embedded) {
        frame.src = url;
        empty.hidden = true;
        address.value = url;
        latestSession = { url, title: '', ready: true };
        addButton.disabled = false;
      } else {
        showEmpty(`無法開啟：${error.message || error}`);
      }
    } finally {
      goButton.disabled = false;
    }
  }

  async function nav(action) {
    const owner = currentOwner();
    try {
      await assertClientAuthenticated();
      if (embedded) {
        await postEmbed({
          visible: panelOpen(),
          action,
        }, owner);
        if (!matchesOwner(owner)) return;
        await syncFrame(owner);
      } else if (action === 'reload' && frame.src) {
        frame.src = frame.src;
      } else if (action === 'back') {
        frame.contentWindow?.history.back();
      } else if (action === 'forward') {
        frame.contentWindow?.history.forward();
      }
      if (matchesOwner(owner)) await refreshEvidence(owner).catch(() => {});
    } catch {
      /* navigation chrome should stay usable even if evidence is empty */
    }
  }

  tab.addEventListener('click', () => toggleRightPane(page.id));
  document.addEventListener('rightpanel:layout', () =>
    requestAnimationFrame(() => void syncFrame().catch(() => {})),
  );
  $('#diffClose')?.addEventListener('click', () => {
    requestAnimationFrame(() => void syncFrame().catch(() => {}));
  });
  goButton.addEventListener('click', () => void open());
  address.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      void open();
    }
  });
  backButton.addEventListener('click', () => void nav('back'));
  forwardButton.addEventListener('click', () => void nav('forward'));
  reloadButton.addEventListener('click', () => void nav('reload'));
  runButton.addEventListener('click', () => {
    const code = script.value.trim();
    if (!code) return;
    runButton.disabled = true;
    evalOutput.textContent = '執行中…';
    request('POST', '/browser/eval', { session_id: conversationID(), code })
      .then((value) => {
        evalOutput.textContent = evidenceText(value.value ?? value);
        void refreshEvidence().catch(() => {});
      })
      .catch((error) => {
        evalOutput.textContent = `失敗：${error.message || error}`;
      })
      .finally(() => {
        runButton.disabled = false;
      });
  });
  addButton.addEventListener('click', () => {
    const input = $('#input');
    if (!input || !latestSession) return;
    const payload = [
      '[Meadow Browser 第一手證據]',
      `URL: ${latestSession.url || address.value || ''}`,
      `標題: ${latestSession.title || ''}`,
      `DOM:\n${evidenceText(latestDOM?.tree || latestDOM || '')}`,
      `Console:\n${evidenceText(latestConsole?.logs || latestConsole || '')}`,
    ].join('\n');
    input.value = input.value ? `${input.value}\n\n${payload}` : payload;
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.focus();
  });

  const frameObserver = new ResizeObserver(() => void syncFrame().catch(() => {}));
  frameObserver.observe(viewport);
  window.addEventListener('resize', () => void syncFrame().catch(() => {}));

  function applyNativeNavigation(detail) {
    if (!agentControl || !detail ||
        String(detail.session_id || '') !== String(state.sessionId || '') ||
        !state.sessionId || typeof detail.url !== 'string') return;
    // This is a native navigation receipt, not a URL to open or evaluate.
    address.value = detail.url;
    empty.hidden = true;
    activate(true);
    void syncFrame().catch(() => {});
  }

  window.addEventListener('meadow:embedded-browser-navigation', (event) => {
    const detail = event?.detail;
    if (!detail || !state.sessionId ||
        String(detail.session_id || '') !== String(state.sessionId)) return;
    if (!nativeStatusLoaded) {
      pendingNativeNavigation = { session_id: detail.session_id, url: detail.url };
      return;
    }
    applyNativeNavigation(detail);
  });

  const loadNativeStatus = () => request('GET', '/native/browser-embed')
    .then((value) => {
      embedded = value?.embedded === true || value?.supported === true;
      agentControl = value?.agent_control === true;
      if (Number.isSafeInteger(value?.owner_revision) && value.owner_revision >= 0) {
        ownerRevision = Math.max(ownerRevision, value.owner_revision);
      }
      if (embedded) {
        frame.hidden = true;
        viewport.classList.add('native');
        empty.textContent = '輸入網址後，頁面會直接顯示在這裡。';
      }
    })
    .catch(() => {
      embedded = false;
      agentControl = false;
    })
    .then(async () => {
      nativeStatusLoaded = true;
      await syncFrame().catch(() => {});
      const navigation = pendingNativeNavigation;
      const browse = pendingAgentBrowse;
      pendingNativeNavigation = null;
      pendingAgentBrowse = null;
      if (agentControl) applyNativeNavigation(navigation);
      else if (browse?.sessionId === String(state.sessionId || '')) {
        activate(true);
        void open(browse.url);
      }
    });

  let nativeStatusReady = loadNativeStatus();
  window.addEventListener('commercial:change', (event) => {
    const account = event.detail || {};
    if (account.capabilitiesReady === true &&
        (account.authenticationRequired === false || account.authenticated === true)) {
      if (!embedded) nativeStatusReady = loadNativeStatus();
      return;
    }
    browserAccountRevision += 1;
    pendingAgentBrowse = null;
    pendingNativeNavigation = null;
    latestSession = null;
    latestDOM = null;
    latestConsole = null;
    frame.removeAttribute('src');
    addButton.disabled = true;
    showEmpty('請先登入 Meadow，再使用瀏覽器。');
  });

  bindActiveOwner = async (
    expectedSessionID = String(state.sessionId || ''),
    expectedWorkspaceRoot = String(state.cwd || ''),
  ) => {
    await assertClientAuthenticated();
    const owner = { session_id: String(expectedSessionID), workspace_root: expectedWorkspaceRoot };
    await nativeStatusReady;
    if (!matchesOwner(owner)) {
      throw new Error('Browser owner changed before navigation admission');
    }
    if (agentControl && !embedded) {
      throw new Error('Native browser pane is unavailable for ownership binding');
    }
    const result = await syncFrame(owner);
    if (!matchesOwner(owner)) {
      throw new Error('Browser owner changed during navigation admission');
    }
    return result;
  };
  const ownerChanged = () => {
    pendingAgentBrowse = null;
    pendingNativeNavigation = null;
    void bindActiveOwner().catch(() => {});
  };
  bus.on('session_created', ownerChanged);
  bus.on('thread:switched', ownerChanged);
  bus.on('workspace:changed', ownerChanged);

  function onAgentBrowse(update) {
    if (!update) return;
    if (update.sessionId && update.sessionId !== state.sessionId) return;
    const url = toolURL(update);
    if (!url) return;
    if (!nativeStatusLoaded) {
      pendingAgentBrowse = { sessionId: String(state.sessionId || ''), url };
      return;
    }
    if (agentControl) return;
    activate(true);
    void open(url);
  }

  bus.on('update:tool_call', onAgentBrowse);
  bus.on('update:tool_call_draft', onAgentBrowse);
  bus.on('update:tool_call_update', onAgentBrowse);
}
