/* global Terminal, FitAddon */
// terminal.js — M5-3 terminal drawer: xterm.js tabs over the daemon PTY
// endpoints (POST /session/{sid}/term, SSE stream, write/resize, DELETE).
// Vendored UMD globals: window.Terminal (xterm 5.5) and window.FitAddon
// (addon-fit 0.10, ctor at FitAddon.FitAddon). Static drawer DOM lives in
// index.html (#termDrawer); panes/tabs are built here. Closing the drawer
// or quitting the app never kills a pty — only the tab ✕ issues DELETE.
import { state, bus, hasClientAccess, setClientAuthentication } from './core.js?v=raven-execution-20261006a';
import { getTheme } from './theme.js';

const $ = (sel) => document.querySelector(sel);

const MIN_H = 180; // drag clamp lower bound (px)
const MAX_VH = 0.7; // drag clamp upper bound (fraction of window height)
const FIT_DEBOUNCE = 120; // ms between ResizeObserver bursts and fit()

// --- same-origin JSON helper (term endpoints are not in core.daemon) ---
async function req(method, path, body) {
  const revision = terminalAccountRevision;
  if (!terminalAccessCurrent(revision)) throw terminalAccessError();
  const opts = { method, headers: { 'Content-Type': 'application/json' } };
  if (body !== undefined) opts.body = JSON.stringify(body);
  const res = await fetch(path, opts);
  let data = null;
  try {
    data = await res.json();
  } catch {
    /* empty or non-JSON body */
  }
  if (!terminalAccessCurrent(revision)) throw terminalAccessError();
  if (res.status === 401) {
    setClientAuthentication(null);
    bus.emit('authentication_required', { reason: 'expired' });
  }
  if (!terminalAccessCurrent(revision)) throw terminalAccessError();
  if (!res.ok) {
    throw new Error(
      (data && (data.error || data.message)) || `HTTP ${res.status}`,
    );
  }
  return data;
}

// --- base64 <-> bytes (UTF-8 safe: atob/btoa only speak latin1) ---
function b64ToBytes(b64) {
  return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
}
function strToB64(s) {
  const bytes = new TextEncoder().encode(s);
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}

// Meadow dark-green palette (mirrors the style.css tokens).
const XTERM_THEME = {
  background: '#050d0a', // --bg-deep
  foreground: '#f0f4f1',
  cursor: '#4ade80', // --primary
  cursorAccent: '#06130d',
  selectionBackground: 'rgba(74,222,128,.28)',
  black: '#0b1612',
  red: '#f87171',
  green: '#4ade80',
  yellow: '#fbbf24',
  blue: '#7dd3fc',
  magenta: '#f9a8d4',
  cyan: '#5eead4',
  white: '#e7ece9',
  brightBlack: '#46544d',
  brightRed: '#fca5a5',
  brightGreen: '#86efac',
  brightYellow: '#fde68a',
  brightBlue: '#bae6fd',
  brightMagenta: '#fbcfe8',
  brightCyan: '#99f6e4',
  brightWhite: '#ffffff',
};

const tabs = []; // live tab records, oldest first
const LIGHT_XTERM_THEME = {
  background: '#eee9de', foreground: '#242622', cursor: '#526612', cursorAccent: '#faf7f0',
  selectionBackground: '#dbe59a', black: '#242622', red: '#ad2940', green: '#526612',
  yellow: '#87600d', blue: '#245c94', magenta: '#b52878', cyan: '#226b6b', white: '#65675f',
  brightBlack: '#6e7066', brightRed: '#b83149', brightGreen: '#596f0e', brightYellow: '#916217',
  brightBlue: '#3268a0', brightMagenta: '#c3297c', brightCyan: '#247375', brightWhite: '#242622',
};
const terminalTheme = () => getTheme() === 'light' ? LIGHT_XTERM_THEME : XTERM_THEME;
window.addEventListener('meadow:themechange', () => {
  for (const tab of tabs) tab.term.options.theme = terminalTheme();
});
let seq = 0; // tab name counter（終端 1, 終端 2, …）
let drawerEl, tabsEl, panesEl, newBtnEl, btnEl;
let hintEl = null;
let terminalAccountRevision = 0;

function terminalAccessError() {
  const error = new Error('請先登入 Meadow，再使用終端機。');
  error.code = 'LOGIN_REQUIRED';
  error.status = 401;
  return error;
}

function terminalAccessCurrent(revision) {
  return hasClientAccess() && revision === terminalAccountRevision;
}

function tabAccessCurrent(tab) {
  return !tab.detached && terminalAccessCurrent(tab.accountRevision);
}

// Drop only the local view and readers. The server keeps ownership of its PTY;
// account loss must never become a request to kill a running user process.
function detachTab(tab) {
  tab.detached = true;
  tab.pending = '';
  if (tab.es) {
    tab.es.close();
    tab.es.onopen = null;
    tab.es.onmessage = null;
    tab.es = null;
  }
  tab.inputSubscription?.dispose();
  tab.inputSubscription = null;
  tab.ro?.disconnect();
  clearTimeout(tab.fitTimer);
  tab.term.dispose();
  tab.paneEl.remove();
  tab.tabEl.remove();
  const index = tabs.indexOf(tab);
  if (index >= 0) tabs.splice(index, 1);
}

function revokeTerminalAccess() {
  if (hasClientAccess()) return;
  terminalAccountRevision++;
  for (const tab of [...tabs]) detachTab(tab);
  if (drawerEl) drawerEl.hidden = true;
  btnEl?.classList.remove('on');
  removeHint();
}

bus.on('authentication_required', revokeTerminalAccess);
window.addEventListener('commercial:change', revokeTerminalAccess);

function flashHint(msg) {
  removeHint();
  hintEl = document.createElement('div');
  hintEl.className = 'term-hint';
  hintEl.textContent = msg;
  panesEl.appendChild(hintEl);
}
function removeHint() {
  if (hintEl) {
    hintEl.remove();
    hintEl = null;
  }
}

function activeTab() {
  return tabs.find((t) => t.paneEl.classList.contains('active')) || null;
}

function setActive(tab) {
  if (tab && !tabAccessCurrent(tab)) return;
  for (const t of tabs) {
    t.paneEl.classList.toggle('active', t === tab);
    t.tabEl.classList.toggle('active', t === tab);
  }
  if (tab) {
    doFit(tab);
    tab.term.focus();
  }
}

// Fit the active xterm to its pane; push the new grid to the pty when it
// actually changed. No-ops while the drawer is hidden (zero-size pane).
function doFit(tab) {
  if (!tabAccessCurrent(tab) || drawerEl.hidden || !tab.paneEl.classList.contains('active')) return;
  try {
    tab.fit.fit();
  } catch {
    return;
  }
  const { cols, rows } = tab.term;
  if (!cols || !rows || (cols === tab.lastCols && rows === tab.lastRows))
    return;
  tab.lastCols = cols;
  tab.lastRows = rows;
  if (tab.termId && !tab.exited) {
    req('POST', `/session/${tab.sid}/term/${tab.termId}/resize`, {
      cols,
      rows,
    }).catch(() => {});
  }
}

function scheduleFit(tab) {
  if (!tabAccessCurrent(tab)) return;
  clearTimeout(tab.fitTimer);
  tab.fitTimer = setTimeout(() => doFit(tab), FIT_DEBOUNCE);
}

// Serialized stdin writer: bytes accumulate while a POST is in flight so
// rapid keystrokes can never reorder on the wire.
function sendInput(tab, s) {
  if (!tabAccessCurrent(tab) || tab.exited || !tab.termId) return;
  tab.pending += s;
  if (tab.writing) return;
  tab.writing = true;
  (async () => {
    while (tabAccessCurrent(tab) && tab.pending && !tab.exited) {
      const chunk = tab.pending;
      tab.pending = '';
      try {
        await req('POST', `/session/${tab.sid}/term/${tab.termId}/write`, {
          dataB64: strToB64(chunk),
        });
      } catch {
        break; // pty gone (410/404) or daemon down — drop the keystrokes
      }
    }
    tab.writing = false;
  })();
}

function markExited(tab, code) {
  if (!tabAccessCurrent(tab) || tab.exited) return;
  tab.exited = true;
  tab.tabEl.classList.add('exited');
  tab.labelEl.textContent = `${tab.name}（已結束）`;
  tab.tabEl.title = `行程已結束（exit ${code}）`;
  if (tab.es) {
    tab.es.close();
    tab.es = null;
  }
  tab.term.write(`\r\n\x1b[2m[行程已結束 · exit ${code}]\x1b[0m\r\n`);
}

function connectStream(tab) {
  if (!tabAccessCurrent(tab) || !tab.termId || tab.exited) return;
  const es = new EventSource(`/session/${tab.sid}/term/${tab.termId}/stream`);
  tab.es?.close();
  tab.es = es;
  const current = () => tabAccessCurrent(tab) && tab.es === es;
  let opened = false;
  es.onopen = () => {
    if (!current()) return;
    // A reconnect replays the whole backlog (the stream has no Last-Event-ID),
    // so wipe the screen first and let the replay repaint it.
    if (opened) tab.term.reset();
    opened = true;
  };
  es.onmessage = (e) => {
    if (!current()) return;
    let frame;
    try {
      frame = JSON.parse(e.data);
    } catch {
      return;
    }
    if (frame.type === 'data' && frame.data) {
      tab.term.write(b64ToBytes(frame.data));
    } else if (frame.type === 'exit') {
      markExited(tab, frame.code ?? 0);
    }
  };
}

async function createTab() {
  if (!hasClientAccess()) return;
  const accountRevision = terminalAccountRevision;
  removeHint();
  const sid = state.sessionId;
  if (!sid) {
    flashHint('尚未建立 session，無法開啟終端機');
    return;
  }
  seq += 1;
  const name = `終端 ${seq}`;

  const paneEl = document.createElement('div');
  paneEl.className = 'term-pane';
  panesEl.appendChild(paneEl);

  const tabEl = document.createElement('div');
  tabEl.className = 'term-tab';
  const labelEl = document.createElement('span');
  labelEl.className = 'term-tab-label';
  labelEl.textContent = name;
  const xEl = document.createElement('button');
  xEl.className = 'term-x';
  xEl.title = '關閉終端機';
  xEl.textContent = '✕';
  tabEl.append(labelEl, xEl);
  tabsEl.insertBefore(tabEl, newBtnEl);

  const term = new Terminal({
    cursorBlink: true,
    scrollback: 5000,
    fontSize: 12,
    fontFamily: 'ui-monospace, "SF Mono", Menlo, monospace',
    theme: terminalTheme(),
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);

  const tab = {
    accountRevision,
    detached: false,
    inputSubscription: null,
    sid, // pinned at creation: the pty registry key is sid+termId
    termId: null,
    name,
    term,
    fit,
    es: null,
    paneEl,
    tabEl,
    labelEl,
    exited: false,
    pending: '',
    writing: false,
    lastCols: 0,
    lastRows: 0,
    fitTimer: 0,
    ro: null,
  };
  tabs.push(tab);

  tabEl.addEventListener('click', (e) => {
    if (e.target !== xEl) setActive(tab);
  });
  xEl.addEventListener('click', () => closeTab(tab));

  term.open(paneEl);
  // Cmd+J stays an app-level shortcut even while the terminal has focus
  // (plain Ctrl+J is LF and belongs to the shell, so meta only).
  term.attachCustomKeyEventHandler(
    (ev) => tabAccessCurrent(tab) && !(ev.metaKey && ev.key && ev.key.toLowerCase() === 'j'),
  );
  setActive(tab);

  try {
    fit.fit();
  } catch {
    /* zero-size race: server defaults apply */
  }
  const cols = term.cols || 80;
  const rows = term.rows || 24;
  tab.lastCols = cols;
  tab.lastRows = rows;

  try {
    const r = await req('POST', `/session/${sid}/term`, {
      cols,
      rows,
      cwd: state.cwd || '',
    });
    if (!tabAccessCurrent(tab)) return;
    tab.termId = r.termId;
  } catch (e) {
    if (!tabAccessCurrent(tab)) return;
    tab.exited = true;
    tab.tabEl.classList.add('exited');
    tab.labelEl.textContent = `${name}（建立失敗）`;
    term.write(`\x1b[31m無法建立終端機：${e.message}\x1b[0m\r\n`);
    return;
  }

  connectStream(tab);
  tab.inputSubscription = term.onData((d) => sendInput(tab, d));
  tab.ro = new ResizeObserver(() => scheduleFit(tab));
  tab.ro.observe(paneEl);
}

// ✕ — the only path that kills the pty (DELETE). Everything else keeps it.
function closeTab(tab) {
  if (!tabAccessCurrent(tab)) return;
  const idx = tabs.indexOf(tab);
  if (idx < 0) return;
  if (tab.termId) {
    req('DELETE', `/session/${tab.sid}/term/${tab.termId}`).catch(() => {});
  }
  const wasActive = tab.paneEl.classList.contains('active');
  detachTab(tab);
  if (tabs.length === 0) {
    toggleDrawer(false);
    return;
  }
  if (wasActive) setActive(tabs[Math.min(idx, tabs.length - 1)]);
}

// Hiding the drawer never touches the ptys: SSE stays attached so output
// keeps accumulating and re-opening shows the live screen.
function toggleDrawer(force) {
  if (!drawerEl) return;
  const show = force !== undefined ? force : drawerEl.hidden;
  if (show && !hasClientAccess()) return;
  if (show === !drawerEl.hidden) return;
  drawerEl.hidden = !show;
  if (btnEl) btnEl.classList.toggle('on', show);
  if (show) {
    if (tabs.length === 0) createTab();
    else setActive(activeTab() || tabs[0]);
  }
}

// Top-edge drag: resize between MIN_H and 70vh; the pane ResizeObserver
// re-fits (debounced) while dragging, pointerup does a final exact fit.
function initDrag() {
  const bar = $('#termDragbar');
  let drag = null;
  bar.addEventListener('pointerdown', (e) => {
    if (!hasClientAccess()) return;
    drag = { y: e.clientY, h: drawerEl.offsetHeight };
    bar.setPointerCapture(e.pointerId);
    bar.classList.add('dragging');
    e.preventDefault();
  });
  bar.addEventListener('pointermove', (e) => {
    if (!hasClientAccess()) { drag = null; return; }
    if (!drag) return;
    const max = Math.round(window.innerHeight * MAX_VH);
    const h = Math.min(Math.max(drag.h + (drag.y - e.clientY), MIN_H), max);
    drawerEl.style.height = h + 'px';
  });
  const end = (e) => {
    if (!drag) return;
    drag = null;
    bar.classList.remove('dragging');
    try {
      bar.releasePointerCapture(e.pointerId);
    } catch {
      /* capture already released */
    }
    const t = activeTab();
    if (t) doFit(t);
  };
  bar.addEventListener('pointerup', end);
  bar.addEventListener('pointercancel', end);
}

export function initTerminal() {
  drawerEl = $('#termDrawer');
  tabsEl = $('#termTabs');
  panesEl = $('#termPanes');
  newBtnEl = $('#termNew');
  btnEl = $('#btnTerminal');
  if (!drawerEl || !tabsEl || !panesEl || !newBtnEl) return;

  if (typeof window.Terminal !== 'function' || !window.FitAddon) {
    if (btnEl) {
      btnEl.disabled = true;
      btnEl.title = '終端機元件載入失敗';
    }
    console.error('terminal.js: vendor xterm globals missing');
    return;
  }

  if (btnEl) {
    btnEl.disabled = false;
    btnEl.addEventListener('click', () => toggleDrawer());
  }
  newBtnEl.addEventListener('click', () => createTab());

  // Cmd+J（mac）/ Ctrl+J toggles the drawer; Ctrl+J inside a terminal is
  // shell LF and must pass through.
  window.addEventListener('keydown', (e) => {
    if (!e.key || e.key.toLowerCase() !== 'j' || e.altKey || e.shiftKey) return;
    if (!e.metaKey && !e.ctrlKey) return;
    const inTerm =
      e.target instanceof Element && e.target.closest('#termPanes');
    if (e.ctrlKey && !e.metaKey && inTerm) return;
    e.preventDefault();
    toggleDrawer();
  });

  initDrag();
}
