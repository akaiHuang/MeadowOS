// outputtabs.js — header tabs for files the agent wrote this session.
// Subscribes file mutation tool events, commits file_path only after success,
// renders up to 8 tabs into #outTabs（同 TS ChatHeader outputFiles.slice(0,8)；
// older ones fold into a +N menu）。M7 像素對齊：tabs 移到 header 左群組
// （標題右側、border-l 分隔），視覺樣式見 style.css「M7 header」區塊。
// Click = read-only viewer modal over POST /workspace/read; hover ✕ removes
// a tab; 'thread:switched' clears everything.
import { bus, state } from './core.js?v=raven-execution-20261006a';

const MAX_TABS = 8; // TS ChatHeader 顯示上限同值
const WRITE_KINDS = new Set(['write_file', 'save_conversation_text', 'edit']);

let host = null; // #outTabs
let files = []; // committed [{rel, base}] insertion order, most recently touched last
const pendingWrites = new Map(); // session/run/toolCallId -> candidate path
let menu = null; // +N overflow dropdown (body-mounted)
let overlay = null; // viewer modal overlay (body-mounted)

// same-origin POST mirroring core.js request() (workspace.* is not wrapped
// by the daemon{} export); throws Error carrying the daemon's {error} text.
async function post(path, body) {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  let data = null;
  try {
    data = await res.json();
  } catch {
    /* empty body */
  }
  if (!res.ok) {
    throw new Error(
      (data && (data.error || data.message)) || `HTTP ${res.status}`,
    );
  }
  return data;
}

const baseName = (p) => p.split('/').filter(Boolean).pop() || p;
const opensInPreview = (p) => /\.(?:html?|md|pptx|pdf)$/i.test(String(p || ''));

function openOutput(rel) {
  if (opensInPreview(rel)) {
    bus.emit('preview:open', { path: rel });
    return;
  }
  openViewer(rel);
}

// lucide file-text (ISC) — same icon the TS header renders on each tab
const FILE_ICON_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z"/><path d="M14 2v5a1 1 0 0 0 1 1h5"/><path d="M10 9H8"/><path d="M16 13H8"/><path d="M16 17H8"/></svg>';

// file_path arrives absolute (tool schema asks for it) or relative (local
// models emit them anyway). /workspace/read only accepts workspace-relative
// paths, so absolute paths outside state.cwd (e.g. /tmp scratch) are skipped.
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

function completedTool(value) {
  return ['completed', 'complete', 'succeeded', 'success', 'ok'].includes(
    String((value && value.status) || '').toLowerCase(),
  );
}

function rememberCommitted(rel) {
  if (!rel) return;
  const i = files.findIndex((f) => f.rel === rel);
  if (i >= 0) files.splice(i, 1);
  files.push({ rel, base: baseName(rel) });
}

function onToolCall(u) {
  if (!u || !host || !acceptsActiveScope(u)) return;
  if (!WRITE_KINDS.has(u.kind || u.title)) return;
  const rel = toRel(u.rawInput && u.rawInput.file_path);
  const key = scopedToolKey(u);
  if (key) pendingWrites.set(key, rel);
}

function onToolUpdate(u) {
  if (!u || !host || !acceptsActiveScope(u)) return;
  const key = scopedToolKey(u);
  const fallback = key ? pendingWrites.get(key) : '';
  if (key && (completedTool(u) || u.status === 'failed'))
    pendingWrites.delete(key);
  if (!completedTool(u)) return;
  let rel = '';
  const changed = (Array.isArray(u.changedPaths) ? u.changedPaths : [])
    .map(toRel).filter(Boolean);
  if (changed.length) {
    // Keep the tool's primary deliverable newest without losing sibling exports.
    for (const path of changed.slice().reverse()) rememberCommitted(path);
    render();
    return;
  }
  if (!rel && WRITE_KINDS.has(u.kind || u.title))
    rel = toRel(u.rawInput && u.rawInput.file_path);
  if (!rel) rel = fallback || '';
  if (!rel) return;
  rememberCommitted(rel);
  render();
}

function removeFile(rel) {
  files = files.filter((f) => f.rel !== rel);
  render();
}

// --- tab strip ---
function tabEl(f) {
  const tab = document.createElement('button');
  tab.className = 'out-tab';
  tab.title = f.rel;
  tab.dataset.filePath = f.rel;
  tab.setAttribute('role', 'tab');
  const ic = document.createElement('span');
  ic.className = 'out-ic';
  ic.innerHTML = FILE_ICON_SVG;
  const lb = document.createElement('span');
  lb.className = 'out-lb';
  lb.textContent = f.base;
  const x = document.createElement('span');
  x.className = 'out-x';
  x.textContent = '✕';
  x.title = '移除';
  x.addEventListener('click', (e) => {
    e.stopPropagation();
    removeFile(f.rel);
  });
  tab.append(ic, lb, x);
  tab.addEventListener('click', () => openOutput(f.rel));
  return tab;
}

function render() {
  closeMenu();
  host.textContent = '';
  if (!files.length) return;
  const recent = files.slice(-MAX_TABS); // newest stay visible
  const older = files.slice(0, -MAX_TABS);
  for (const f of recent) host.appendChild(tabEl(f));
  if (older.length) {
    const more = document.createElement('button');
    more.className = 'out-tab out-more';
    more.textContent = `+${older.length}`;
    more.title = '更多輸出檔';
    more.addEventListener('click', (e) => {
      e.stopPropagation();
      if (menu) closeMenu();
      else openMenu(more, older);
    });
    host.appendChild(more);
  }
}

// --- +N overflow menu ---
function closeMenu() {
  if (menu) {
    menu.remove();
    menu = null;
  }
}

function openMenu(anchor, items) {
  menu = document.createElement('div');
  menu.className = 'out-menu';
  for (const f of items.slice().reverse()) {
    // newest first
    const it = document.createElement('div');
    it.className = 'out-mi';
    it.title = f.rel;
    it.dataset.filePath = f.rel;
    const lb = document.createElement('span');
    lb.className = 'out-lb';
    lb.textContent = f.rel;
    const x = document.createElement('span');
    x.className = 'out-x';
    x.textContent = '✕';
    x.title = '移除';
    x.addEventListener('click', (e) => {
      e.stopPropagation();
      removeFile(f.rel); // render() closes the menu
    });
    it.append(lb, x);
    it.addEventListener('click', () => {
      closeMenu();
      openOutput(f.rel);
    });
    menu.appendChild(it);
  }
  document.body.appendChild(menu);
  const r = anchor.getBoundingClientRect();
  menu.style.top = r.bottom + 6 + 'px';
  menu.style.left =
    Math.max(8, Math.min(r.left, window.innerWidth - menu.offsetWidth - 8)) +
    'px';
}

// --- read-only viewer modal ---
function closeViewer() {
  if (overlay) {
    overlay.remove();
    overlay = null;
  }
}

async function openViewer(rel) {
  closeMenu();
  closeViewer();
  overlay = document.createElement('div');
  const my = overlay; // guard against a newer viewer superseding this fetch
  overlay.className = 'qol-overlay';
  const modal = document.createElement('div');
  modal.className = 'ot-modal';

  const head = document.createElement('div');
  head.className = 'qol-head';
  const ti = document.createElement('span');
  ti.className = 'qol-title';
  ti.textContent = rel;
  ti.title = rel;
  const x = document.createElement('button');
  x.className = 'qol-x';
  x.textContent = '✕';
  x.title = '關閉';
  x.addEventListener('click', closeViewer);
  head.append(ti, x);

  const body = document.createElement('pre');
  body.className = 'ot-body';
  body.textContent = '載入中…';
  modal.append(head, body);
  overlay.appendChild(modal);
  overlay.addEventListener('click', (e) => {
    if (e.target === overlay) closeViewer();
  });
  document.body.appendChild(overlay);

  try {
    const r = await post('/workspace/read', {
      cwd: state.cwd,
      sessionId: state.sessionId,
      relativePath: rel,
    });
    if (overlay !== my) return;
    if (r && r.binary) {
      body.textContent = '（二進位檔案，無法預覽）';
    } else {
      body.textContent = (r && r.contents) || '（空檔案）';
      if (r && r.truncated) {
        const note = document.createElement('div');
        note.className = 'qol-note';
        note.textContent = `檔案過大，僅顯示部分內容（共 ${r.sizeBytes} bytes）`;
        modal.appendChild(note);
      }
    }
  } catch (e) {
    if (overlay === my)
      body.textContent = '讀取失敗：' + ((e && e.message) || e);
  }
}

export function initOutputTabs() {
  host = document.getElementById('outTabs');
  if (!host) return;

  bus.on('update:tool_call', onToolCall);
  bus.on('update:tool_call_update', onToolUpdate);
  bus.on('outputs:seed', (paths) => {
    for (const path of paths || []) rememberCommitted(toRel(path));
    render();
  });
  bus.on('thread:switched', () => {
    files = [];
    pendingWrites.clear();
    closeViewer();
    render(); // also closes the menu
  });

  document.addEventListener('click', (e) => {
    if (menu && !menu.contains(e.target) && !host.contains(e.target))
      closeMenu();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      closeMenu();
      closeViewer();
    }
  });
}
