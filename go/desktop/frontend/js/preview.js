/* native-background-ui */
// preview.js — right-panel file preview (用戶 12：對話內超連結 → 側欄預覽).
// Adds a third tab to #rightPanel (alongside diffpanel.js 的「輸出內容／進度」)
// driven entirely by injected DOM — index.html 不在本區塊，故 tab 鈕 #rpTabPreview
// 與分頁 #rpPagePreview 都由這裡注入 #rpTabs / #rpWrap。
//
// 流程：bus 'preview:open' {path}（timeline 超連結 / editcard 檔名）→ 開 panel、
// 切到預覽 tab → POST /workspace/read {cwd,relativePath} → 依副檔名渲染：
//   .md   → core.js md()（XSS 安全）文章樣式，可切「原始」看原始碼
//   .html → iframe sandbox srcdoc（「Meadow 瀏覽器預覽」），可切「原始」看 HTML
//   其他文字 → 等寬純文字（不上色）
//   binary → 「無法預覽二進位檔案」
// 預覽頂列：檔名 tab 標籤 + 原始/預覽切換（md/html 才出現）+ 關閉（共用 #diffClose
// 行為由 panel 控制，這裡額外提供分頁標題列）。
//
// rightpanel.js owns visibility/selection for all drawer pages (maximum two).
// This module only owns preview content, not other tabs' active classes.
//
// INTEGRATION REQUIRED (main.js): import { initPreview } from './preview.js'
// 並在其它 init* 之後呼叫 initPreview()（需在 initDiffPanel() 之後，確保
// #rightPanel skeleton 已存在；兩者皆只讀現成 DOM，順序不嚴格但建議如此）。
import { md, bus, state, daemon } from './core.js?v=raven-execution-20261006a';
import {
  setRightPanelOpen,
  showRightPane,
  hideRightPane,
  toggleRightPane,
} from './rightpanel.js';

const $ = (s) => document.querySelector(s);

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
}

// same-origin POST mirroring core.js request(): throws Error whose .message
// carries the daemon's {error} text.
async function post(path, body, options = {}) {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
    signal: options.signal,
    cache: options.cache,
  });
  let data = null;
  try {
    data = await res.json();
  } catch {
    /* empty body */
  }
  if (!res.ok) {
    const err = new Error(
      (data && (data.error || data.message)) || `HTTP ${res.status}`,
    );
    err.status = res.status;
    throw err;
  }
  return data || {};
}

// lucide file-text (ISC) — 與 diffpanel 檔案列同款，超連結 icon 視覺一致
const FILE_ICON_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z"/><path d="M14 2v5a1 1 0 0 0 1 1h5"/><path d="M10 9H8"/><path d="M16 13H8"/><path d="M16 17H8"/></svg>';

const ext = (p) => {
  const m = /\.([a-z0-9]+)$/i.exec(p || '');
  return m ? m[1].toLowerCase() : '';
};

function acceptsActiveScope(value) {
  const incomingSession = String((value && value.sessionId) || '');
  const activeSession = String(state.sessionId || '');
  if (incomingSession && activeSession && incomingSession !== activeSession)
    return false;
  const incomingRun = String((value && value.runId) || '');
  const activeRun = String(state.runId || '');
  return !incomingRun || !activeRun || incomingRun === activeRun;
}

function scopedToolKey(value) {
  const callID = String((value && (value.toolCallId || value.callId)) || '');
  if (!callID) return '';
  return [
    String((value && value.sessionId) || state.sessionId || 'legacy-session'),
    String((value && value.runId) || 'legacy-run'),
    callID,
  ].join('\u0000');
}

const BROWSER_PREVIEW_SOURCE = 'meadow-browser-preview-v1';

// Opaque sandbox origins expose the Storage properties but throw before page
// code can call them. Keep the artifact bytes untouched on disk and rewrite
// only inline preview scripts to the isolated in-memory stores installed by
// the bridge below. This covers the common localStorage/sessionStorage forms
// without granting the generated page access to Meadow Desktop's origin.
function rewriteOpaqueStorageAccess(contents) {
  const rewrite = (source) =>
    source
      .replace(/localStorage/g, '__meadowLocalStorage')
      .replace(/sessionStorage/g, '__meadowSessionStorage');
  // Attribute handlers, dynamically-evaluated strings and module scripts can
  // access Storage outside a classic inline <script> body. This transformation
  // is preview-only; the persisted artifact remains byte-for-byte unchanged.
  return rewrite(String(contents || ''));
}

// Run generated pages in an opaque-origin sandbox. The bridge is inserted
// before artifact scripts so runtime errors, rejected promises, and console
// warnings are observable without granting the page Meadow Desktop's origin.
// A small in-memory Storage shim keeps standalone games that use localStorage
// functional even though opaque sandbox origins cannot access persistent DOM
// storage.
function instrumentBrowserPreview(contents, nonce, includeDetails) {
  const bridge = `<script>(function () {
    'use strict';
    const source = ${JSON.stringify(BROWSER_PREVIEW_SOURCE)};
    const nonce = ${JSON.stringify(nonce)};
    const includeDetails = ${JSON.stringify(includeDetails)};
    const send = (kind, message) => {
      try {
        parent.postMessage({ source, nonce, kind, message: String(message || '') }, '*');
      } catch (_) {}
    };
    const memory = new Map();
    const storage = {
      get length() { return memory.size; },
      key(index) { return Array.from(memory.keys())[index] || null; },
      getItem(key) { key = String(key); return memory.has(key) ? memory.get(key) : null; },
      setItem(key, value) { memory.set(String(key), String(value)); },
      removeItem(key) { memory.delete(String(key)); },
      clear() { memory.clear(); }
    };
    window.__meadowLocalStorage = storage;
    window.__meadowSessionStorage = storage;
    const printable = (value) => {
      if (value instanceof Error)
        return includeDetails ? value.stack || value.message || String(value) : value.message || '執行錯誤';
      if (typeof value === 'string') return value;
      try { return JSON.stringify(value); } catch (_) { return String(value); }
    };
    for (const level of ['warn', 'error']) {
      const original = console[level] && console[level].bind(console);
      console[level] = (...args) => {
        send(level, args.map(printable).join(' '));
        if (original) original(...args);
      };
    }
    addEventListener('error', (event) => {
      if (event.target && event.target !== window) {
        const resource = event.target.getAttribute('data-meadow-resource') || event.target.getAttribute('src') || event.target.getAttribute('href') || event.target.tagName;
        send('error', '預覽資源載入失敗' + (includeDetails ? ': ' + String(resource).slice(0, 160) : ''));
        return;
      }
      const filename = String(event.filename || '');
      const where = includeDetails && filename ? ' at ' + (filename.startsWith('data:') ? '[preview resource]' : filename.slice(0, 160)) + ':' + event.lineno + ':' + event.colno : '';
      send('error', (event.message || 'Uncaught page error') + where);
    }, true);
    addEventListener('unhandledrejection', (event) => send('error', 'Unhandled promise: ' + printable(event.reason)));
    addEventListener('DOMContentLoaded', () => send('ready', document.title || 'HTML ready'), { once: true });
  })();<\/script>`;
  const instrumented = rewriteOpaqueStorageAccess(contents);
  const head = /<head(?:\s[^>]*)?>/i.exec(instrumented);
  if (!head) return bridge + instrumented;
  const at = head.index + head[0].length;
  return instrumented.slice(0, at) + bridge + instrumented.slice(at);
}

// The authenticated artifact read supplies bounded, root-confined companion
// bytes. Data URLs preserve classic script ordering/defer/async and stylesheet
// attributes without adding a base URL, resource token or same-origin access.
// Encode source instead of inserting raw </script> or </style> text into HTML.
function materializePreviewResources(contents, resources) {
  if (!resources.length) return contents;
  const document = new DOMParser().parseFromString(contents, 'text/html');
  const encoded = new Map();
  for (const resource of resources) {
    if (!['script', 'style'].includes(resource.kind) || typeof resource.url !== 'string' || typeof resource.contents !== 'string') continue;
    const source = resource.kind === 'script'
      ? rewriteOpaqueStorageAccess(resource.contents)
      : resource.contents;
    const bytes = new TextEncoder().encode(source);
    const binary = Array.from(bytes, (byte) => String.fromCharCode(byte)).join('');
    const mime = resource.kind === 'script' ? 'text/javascript' : 'text/css';
    // Match the preview-only Storage transformation already applied to HTML,
    // including filenames that happen to contain localStorage/sessionStorage.
    encoded.set(resource.kind + '\u0000' + rewriteOpaqueStorageAccess(resource.url), `data:${mime};charset=utf-8;base64,${btoa(binary)}`);
  }
  for (const script of document.querySelectorAll('script[src]')) {
    const value = encoded.get('script\u0000' + script.getAttribute('src'));
    if (value) {
      script.setAttribute('data-meadow-resource', script.getAttribute('src'));
      script.setAttribute('src', value);
    }
  }
  for (const link of document.querySelectorAll('link[href]')) {
    if (!link.relList.contains('stylesheet')) continue;
    const value = encoded.get('style\u0000' + link.getAttribute('href'));
    if (value) {
      link.setAttribute('data-meadow-resource', link.getAttribute('href'));
      link.setAttribute('href', value);
    }
  }
  const doctype = document.doctype ? new XMLSerializer().serializeToString(document.doctype) : '';
  return doctype + document.documentElement.outerHTML;
}

// When a turn produced both implementation sources and a user-facing artifact,
// the Preview tab should open the artifact (for example HTML) instead of the
// last-written TypeScript source.  Keep this deterministic and local: history
// ordering remains the tie-breaker within the same file class.
function preferredPreviewPath(paths) {
  const ranked = (paths || []).map((path, index) => {
    const kind = ext(path);
    let rank = 0;
    if (kind === 'html' || kind === 'htm') rank = 4;
    else if (kind === 'md' || kind === 'markdown' || kind === 'svg') rank = 3;
    else if (kind === 'txt' || kind === 'json' || kind === 'csv') rank = 2;
    else rank = 1;
    return { path, index, rank };
  });
  ranked.sort((a, b) => a.rank - b.rank || a.index - b.index);
  return ranked.length ? ranked[ranked.length - 1].path : '';
}

// file_path may arrive absolute (tool schema) or relative; preview reads via
// /workspace/read which resolves against state.cwd, so strip the workspace
// prefix the same way diffpanel.toRel does. Absolute paths outside the
// workspace return '' → caller surfaces a友善錯誤.
function toRel(p) {
  if (!p || typeof p !== 'string') return '';
  if (p === '/workspace/session' || p.startsWith('/workspace/session/'))
    return '/session' + p.slice('/workspace/session'.length);
  if (p === '/session' || p.startsWith('/session/')) return p;
  if (p === '/workspace' || p.startsWith('/workspace/'))
    return p.slice('/workspace/'.length);
  if (!p.startsWith('/')) return p.replace(/^\.\//, '');
  const cwd = (state.cwd || '').replace(/\/+$/, '');
  if (cwd && p.startsWith(cwd + '/')) return p.slice(cwd.length + 1);
  return '';
}

export function initPreview() {
  const panel = $('#rightPanel');
  const wrap = $('#rpWrap');
  const tabs = $('#rpTabs');
  if (!panel || !wrap || !tabs) return; // skeleton missing — nothing to own
  if (document.getElementById('rpTabPreview')) return; // idempotent guard

  // --- inject the preview tab button + page (index.html not in scope) ---
  const tabPrev = el('button', 'rp-tab', '預覽');
  tabPrev.id = 'rpTabPreview';
  tabPrev.type = 'button';
  tabPrev.setAttribute('role', 'tab');
  tabPrev.setAttribute('aria-selected', 'false');
  tabPrev.setAttribute('aria-controls', 'rpPagePreview');
  tabPrev.hidden = false; // product-level destination, with an honest empty state
  tabs.appendChild(tabPrev);

  const page = el('div', 'rp-page');
  page.id = 'rpPagePreview';
  page.setAttribute('role', 'tabpanel');
  page.setAttribute('aria-labelledby', 'rpTabPreview');

  // 預覽頂列：檔名 + 原始/預覽切換（依檔型顯隱）
  const head = el('div', 'pv-head');
  const fic = el('span', 'pv-fic');
  fic.innerHTML = FILE_ICON_SVG;
  const nameEl = el('span', 'pv-name', '尚未選取檔案');
  nameEl.dataset.filePath = '';
  const modeWrap = el('div', 'pv-modes');
  const btnView = el('button', 'pv-mode on', '預覽');
  const btnRaw = el('button', 'pv-mode', '原始');
  btnView.type = 'button';
  btnRaw.type = 'button';
  modeWrap.append(btnView, btnRaw);
  modeWrap.hidden = true; // 純文字／binary 沒有兩種視圖
  const btnReload = el('button', 'pv-mode pv-reload');
  btnReload.type = 'button';
  btnReload.disabled = true;
  btnReload.title = '清除預覽暫存並重新載入';
  btnReload.setAttribute('aria-label', btnReload.title);
  const reloadIcon = el('span', '', '↻'); // Same reload symbol as the browser drawer.
  reloadIcon.setAttribute('aria-hidden', 'true');
  btnReload.appendChild(reloadIcon);
  const btnEditor = el('button', 'pv-editor', '在編輯器開啟');
  btnEditor.type = 'button';
  btnEditor.disabled = true;
  btnEditor.title = '在預設編輯器開啟目前工作區檔案';
  const btnDownload = el('button', 'pv-editor pv-download', '下載');
  btnDownload.type = 'button';
  btnDownload.disabled = true;
  btnDownload.title = '下載完整檔案';
  head.append(fic, nameEl, modeWrap, btnReload, btnDownload, btnEditor);

  const browserStatus = el('div', 'pv-browser-status');
  browserStatus.hidden = true;
  const bodyEl = el('div', 'pv-body');
  page.append(head, browserStatus, bodyEl);
  wrap.appendChild(page);

  // --- panel + tab coordination (shared .open / .rp-tab.on / .rp-page.on) ---
  function setOpen(o) {
    setRightPanelOpen(o);
  }

  // Reveal a destination through the shared drawer controller.
  function activate(automatic = false) {
    tabPrev.hidden = false;
    showRightPane(page.id, { automatic });
  }

  // Selecting a second tab preserves the other pane; selecting it again hides it.
  tabPrev.addEventListener('click', () => {
    if (!toggleRightPane(page.id)) return;
    if (!nameEl.dataset.filePath && seededPath) {
      open(seededPath);
      return;
    }
  });

  // --- rendering ---
  let curExt = ''; // its extension
  let curContents = ''; // text contents (empty for binary)
  let curPreviewResources = [];
  let curPreviewErrors = [];
  let curBinary = false;
  let curTruncated = false;
  let mode = 'view'; // 'view' | 'raw'
  let fetchSeq = 0; // drop stale /workspace/read responses
  let readController = null;
  let seededPath = ''; // newest persisted session output available to preview
  let previewFrame = null;
  let previewNonce = '';
  let browserWarnings = 0;
  let browserErrors = 0;
  const pendingHTMLWrites = new Map();

  // Only this preview owns these snapshots and iframe storage. Never clear the
  // application origin's cookies, Storage, or browser-wide caches on reload.
  function resetPreview() {
    fetchSeq++;
    readController?.abort();
    readController = null;
    curExt = '';
    curContents = '';
    curPreviewResources = [];
    curPreviewErrors = [];
    curBinary = false;
    curTruncated = false;
    previewFrame = null;
    previewNonce = '';
    browserStatus.hidden = true;
    modeWrap.hidden = true;
    btnView.disabled = true;
    btnRaw.disabled = true;
    btnReload.disabled = true;
    btnDownload.disabled = true;
    btnEditor.disabled = true;
    page.removeAttribute('aria-busy');
    bodyEl.textContent = '';
  }

  function isCurrentRead(sequence, scope) {
    return sequence === fetchSeq && scope.cwd === state.cwd && scope.sessionId === state.sessionId;
  }

  function setBrowserStatus(kind, text, detail) {
    browserStatus.hidden = false;
    browserStatus.className = 'pv-browser-status ' + kind;
    browserStatus.textContent = text;
    browserStatus.title = detail || text;
  }

  window.addEventListener('message', (event) => {
    const data = event.data;
    if (
      !previewFrame ||
      event.source !== previewFrame.contentWindow ||
      !data ||
      data.source !== BROWSER_PREVIEW_SOURCE ||
      data.nonce !== previewNonce
    )
      return;
    const message = String(data.message || '');
    if (data.kind === 'error') {
      browserErrors += 1;
      setBrowserStatus(
        'bad',
        `Meadow Browser · ${browserErrors} 個執行錯誤`,
        message,
      );
    } else if (data.kind === 'warn') {
      browserWarnings += 1;
      if (!browserErrors)
        setBrowserStatus(
          'warn',
          `Meadow Browser · ${browserWarnings} 個警告`,
          message,
        );
    } else if (data.kind === 'ready' && !browserErrors && !browserWarnings) {
      setBrowserStatus('ok', 'Meadow Browser · 可互動', message);
    }
  });

  function note(msg, bad) {
    bodyEl.textContent = '';
    bodyEl.appendChild(el('div', 'pv-note' + (bad ? ' bad' : ''), msg));
  }

  // 原始/預覽切換只對「有兩種視圖」的型別有意義（md 渲染↔原始碼、html 預覽↔HTML）
  function hasDualView() {
    return !curBinary && (curExt === 'md' || curExt === 'html');
  }

  function setMode(m) {
    mode = m;
    btnView.classList.toggle('on', m === 'view');
    btnRaw.classList.toggle('on', m === 'raw');
    paint();
  }
  btnView.addEventListener('click', () => setMode('view'));
  btnRaw.addEventListener('click', () => setMode('raw'));

  // 純文字等寬顯示（語法不上色，純文字；textContent 即 XSS 安全）
  function paintPlain(text) {
    bodyEl.textContent = '';
    const pre = el('pre', 'pv-code');
    pre.textContent = text;
    bodyEl.appendChild(pre);
  }

  function paint() {
    modeWrap.hidden = !hasDualView();
    previewFrame = null;
    previewNonce = '';
    browserStatus.hidden = true;
    if (curBinary) {
      note(curExt === 'pptx' ? 'PowerPoint 檔案已產生，請按「下載」開啟編輯；同份簡報的 HTML 可直接預覽。' : '無法預覽二進位檔案，可下載完整檔案。');
      return;
    }
    if (curExt === 'md') {
      if (mode === 'raw') {
        paintPlain(curContents);
      } else {
        bodyEl.textContent = '';
        const art = el('div', 'pv-md');
        art.innerHTML = md(curContents); // core.js md：先 esc 再轉換，XSS 安全
        bodyEl.appendChild(art);
      }
    } else if (curExt === 'html') {
      if (mode === 'raw') {
        paintPlain(curContents);
      } else if (curPreviewErrors.length) {
        note(curPreviewErrors.join('\n'), true);
      } else {
        bodyEl.textContent = '';
        // Executable Meadow Browser preview. Scripts run in an opaque origin:
        // no same-origin access, navigation, popups, forms, or downloads are
        // granted back into the Wails application shell.
        const frame = document.createElement('iframe');
        frame.className = 'pv-frame';
        frame.setAttribute('sandbox', 'allow-scripts');
        frame.setAttribute('referrerpolicy', 'no-referrer');
        previewFrame = frame;
        previewNonce = `${Date.now()}-${Math.random().toString(36).slice(2)}`;
        browserWarnings = 0;
        browserErrors = 0;
        setBrowserStatus('loading', 'Meadow Browser · 載入中');
        frame.srcdoc = materializePreviewResources(instrumentBrowserPreview(
          curContents,
          previewNonce,
          document.documentElement.dataset.frontendMode !== 'production',
        ), curPreviewResources);
        bodyEl.appendChild(frame);
      }
    } else {
      // 其他文字檔：等寬純文字
      paintPlain(curContents);
    }
    if (curTruncated) {
      const t = el('div', 'pv-trunc', '（檔案過大，僅顯示前段內容）');
      bodyEl.appendChild(t);
    }
  }

  async function open(rawPath, automatic = false, reload = false) {
    const nextMode = reload ? mode : 'view';
    resetPreview();
    const my = fetchSeq;
    btnDownload.textContent = '下載';
    const rel = toRel(rawPath);
    if (!automatic) setOpen(true);
    activate(automatic);
    if (!rel) {
      // 工作區外（如 /tmp 暫存）——/workspace/read 會拒，先說清楚
      nameEl.textContent = rawPath || '';
      nameEl.title = rawPath || '';
      nameEl.dataset.filePath = rawPath || '';
      nameEl.dataset.cwd = '';
      nameEl.dataset.sessionId = '';
      tabPrev.textContent = '預覽';
      modeWrap.hidden = true;
      btnEditor.disabled = true;
      btnDownload.disabled = true;
      note('此檔案不在目前工作區內，無法預覽。', true);
      return;
    }
    curExt = ext(rel);
    nameEl.textContent = rel;
    nameEl.title = rel;
    nameEl.dataset.filePath = rel;
    const activeScope = { cwd: state.cwd, sessionId: state.sessionId };
    nameEl.dataset.cwd = activeScope.cwd || '';
    nameEl.dataset.sessionId = activeScope.sessionId || '';
    btnEditor.disabled = false;
    btnDownload.disabled = true;
    // tab 標籤＝basename（短），完整路徑放 title
    tabPrev.textContent = rel.split('/').filter(Boolean).pop() || '預覽';
    tabPrev.title = rel;
    note(reload ? '重新載入中…' : '載入中…');
    page.setAttribute('aria-busy', 'true');
    readController = new AbortController();
    let d;
    try {
      d = await post(curExt === 'html' ? '/workspace/artifact/read' : '/workspace/read', {
        cwd: activeScope.cwd,
        sessionId: activeScope.sessionId,
        relativePath: rel,
      }, { signal: readController.signal, cache: 'no-store' });
    } catch (e) {
      if (!isCurrentRead(my, activeScope)) return;
      note('讀取檔案失敗：' + ((e && e.message) || e), true);
      return;
    } finally {
      if (isCurrentRead(my, activeScope)) {
        readController = null;
        page.removeAttribute('aria-busy');
        btnReload.disabled = false; // A failed read remains retryable.
      }
    }
    if (!isCurrentRead(my, activeScope)) return;
    btnDownload.disabled = false;
    curBinary = d.binary === true;
    curContents = typeof d.contents === 'string' ? d.contents : '';
    curPreviewResources = Array.isArray(d.previewResources) ? d.previewResources : [];
    curPreviewErrors = Array.isArray(d.previewErrors) ? d.previewErrors.filter((message) => typeof message === 'string') : [];
    curTruncated = d.truncated === true;
    // 切檔預設回預覽；重新載入保留使用者目前的視圖。
    mode = nextMode;
    btnView.disabled = false;
    btnRaw.disabled = false;
    btnView.classList.toggle('on', mode === 'view');
    btnRaw.classList.toggle('on', mode === 'raw');
    paint();
  }

  btnReload.addEventListener('click', () => {
    const rel = nameEl.dataset.filePath;
    if (!rel || nameEl.dataset.cwd !== state.cwd || nameEl.dataset.sessionId !== state.sessionId) return;
    void open(rel, false, true);
  });

  btnDownload.addEventListener('click', async () => {
    const rel = nameEl.dataset.filePath;
    const scope = { cwd: nameEl.dataset.cwd, sessionId: nameEl.dataset.sessionId };
    if (!rel || scope.cwd !== state.cwd || scope.sessionId !== state.sessionId) return;
    const sequence = fetchSeq;
    const isCurrentDownload = () => isCurrentRead(sequence, scope) && rel === nameEl.dataset.filePath;
    btnDownload.disabled = true;
    const previous = btnDownload.textContent;
    btnDownload.textContent = '下載中…';
    try {
      if (location.hostname === 'wails.localhost' || location.protocol === 'wails:') {
        await post('/native/save-artifact', { ...scope, relativePath: rel });
        return;
      }
      const response = await fetch('/workspace/artifact/download', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...scope, relativePath: rel }),
      });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const blob = await response.blob();
      if (!isCurrentDownload()) return;
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement('a');
      anchor.href = url;
      anchor.download = rel.split('/').filter(Boolean).pop() || 'artifact';
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
    } catch (error) {
      if (isCurrentDownload())
        note('下載失敗：' + (error?.message || error), true);
    } finally {
      if (isCurrentDownload()) {
        btnDownload.textContent = previous;
        btnDownload.disabled = false;
      }
    }
  });

  btnEditor.addEventListener('click', async () => {
    const rel = nameEl.dataset.filePath;
    if (!rel || !state.cwd) return;
    btnEditor.disabled = true;
    const previous = btnEditor.textContent;
    btnEditor.textContent = '開啟中…';
    try {
      const absolute = `${String(state.cwd).replace(/\/+$/, '')}/${rel}`;
      await daemon.openEditor(absolute, '');
      btnEditor.textContent = '已開啟';
      setTimeout(() => {
        btnEditor.textContent = previous;
        btnEditor.disabled = false;
      }, 900);
    } catch (e) {
      btnEditor.textContent = '開啟失敗';
      btnEditor.title = (e && e.message) || '無法開啟編輯器';
      setTimeout(() => {
        btnEditor.textContent = previous;
        btnEditor.disabled = false;
      }, 1600);
    }
  });

  bus.on('preview:open', (d) => {
    if (d && d.path) open(d.path, d.automatic === true);
  });
  bus.on('preview:show', () => {
    if (!nameEl.dataset.filePath && seededPath) {
      open(seededPath);
      return;
    }
    setOpen(true);
    activate();
  });
  bus.on('update:tool_call', (update) => {
    if (!update || !acceptsActiveScope(update)) return;
    const kind = update.kind || update.title;
    if (
      kind === 'meadow_browser_visual_check' ||
      kind === 'meadow_browser_interaction_check'
    ) {
      const path = update.rawInput && update.rawInput.file_path;
      const rel = toRel(path);
      if (rel && ext(rel) === 'html') open(path, true);
      return;
    }
    if (
      kind !== 'write_file' &&
      kind !== 'save_conversation_text' &&
      kind !== 'edit'
    )
      return;
    const path = update.rawInput && update.rawInput.file_path;
    const rel = toRel(path);
    if (!rel || ext(rel) !== 'html') return;
    const key = scopedToolKey(update);
    if (key) pendingHTMLWrites.set(key, path);
  });
  bus.on('update:tool_call_update', (update) => {
    if (!update || !acceptsActiveScope(update)) return;
    const key = scopedToolKey(update);
    const status = String(update.status || '').toLowerCase();
    if (status === 'failed' || status === 'error' || status === 'cancelled') {
      if (key) pendingHTMLWrites.delete(key);
      return;
    }
    if (
      !['completed', 'complete', 'succeeded', 'success', 'ok'].includes(status)
    )
      return;
    let path = '';
    for (const candidate of Array.isArray(update.changedPaths)
      ? update.changedPaths
      : []) {
      if (toRel(candidate)) {
        path = candidate;
        break;
      }
    }
    if (!path && update.rawInput) path = update.rawInput.file_path || '';
    if (!path && key) path = pendingHTMLWrites.get(key) || '';
    if (key) pendingHTMLWrites.delete(key);
    const rel = toRel(path);
    if (!rel || ext(rel) !== 'html') return;
    open(path, true);
  });
  bus.on('outputs:seed', (paths) => {
    const candidates = (paths || []).filter(
      (path) => typeof path === 'string' && toRel(path),
    );
    seededPath = preferredPreviewPath(candidates);
    if (!seededPath) return;
    resetPreview();
    const rel = toRel(seededPath);
    nameEl.textContent = rel;
    nameEl.title = rel;
    nameEl.dataset.filePath = '';
    btnEditor.disabled = true;
    note('已有 session 輸出。開啟「預覽」即可載入最新檔案。');
  });

  // thread/project switch: previewed file belongs to the old workspace
  bus.on('thread:switched', () => {
    resetPreview();
    pendingHTMLWrites.clear();
    seededPath = '';
    hideRightPane(page.id);
    nameEl.textContent = '尚未選取檔案';
    nameEl.title = '';
    nameEl.dataset.filePath = '';
    nameEl.dataset.cwd = '';
    nameEl.dataset.sessionId = '';
    btnDownload.textContent = '下載';
    btnDownload.disabled = true;
    btnEditor.disabled = true;
    modeWrap.hidden = true;
    note(
      '尚無可預覽內容。完成網頁、Markdown 或文字檔後，可從輸出檔案開啟預覽。',
    );
  });

  // 初始：預覽是可見的產品功能，沒有 artifact 時明確說明用途。
  note('尚無可預覽內容。完成網頁、Markdown 或文字檔後，可從輸出檔案開啟預覽。');
}
