// sidebar.js — left rail: multi-project groups (localStorage-backed), per-cwd
// thread lists, new chat, thread switching via POST /session/resume (opens a
// LIVE session preloaded with the stored chat, so old threads continue),
// inline rename (dblclick; display-only localStorage overrides), archive
// (frontend-only). Talks to the daemon through core.js wrappers. Model status
// polling lives in main.js (single poller; re-broadcasts 'model_status').
//
// Storage contract (shared with main.js boot):
//   meadow.projects          JSON [{cwd}]          — pinned project folders
//   meadow.activeCwd         string                — active project cwd
//   meadow.collapsedProjects JSON [cwd]            — folded groups
//   meadow.titleOverrides    JSON {chatId:title}   — display-only renames
//   meadow.sidebarSort       JSON {proj,thread}    — 排序選單偏好
//   meadow.cwd               string (legacy mirror) — composer folder pill
//                            still writes it before location.reload()
//
// M7 像素對齊：視覺 1:1 復刻 TS desktop Sidebar.tsx（meadow-desktop/apps/web）——
// 新聊天/搜尋列（28px ghost 列＋Kbd 快捷鍵）、專案區頭（10px uppercase 標籤＋
// 排序/加資料夾 icon）、專案列（chevron→favicon→名稱、hover 才浮現動作鈕）、
// thread 列（單行：標題＋右側 10px 相對時間，hover 與封存鈕 crossfade）、
// SidebarFooter 兩列。行為（多專案/兩段確認/改名/resume）不變。
import { daemon, connectEvents, bus, state, md, hasClientAccess, clientAccountGeneration } from './core.js?v=raven-execution-20261006a';
import { createSidebarSessionCache, sidebarListingNotice } from './sidebar_session_cache.js';
import {
  authorizeNewConversation,
  mountCommercialAccount,
} from './commercial.js?v=desktop-send-20260904a';

const $ = (sel) => document.querySelector(sel);
const CWD_KEY = 'meadow.cwd'; // legacy mirror, kept in sync on every switch
const PROJECTS_KEY = 'meadow.projects';
const ACTIVE_KEY = 'meadow.activeCwd';
const COLLAPSED_KEY = 'meadow.collapsedProjects';
const ARCHIVE_KEY = 'meadow.archivedSessions';
const TITLE_KEY = 'meadow.titleOverrides';
const SORT_KEY = 'meadow.sidebarSort';

// --- inline lucide icons（TS 同款：24x24 stroke、外層控大小） ---
const ICONS = {
  search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
  arrowUpDown:
    '<path d="m21 16-4 4-4-4"/><path d="M17 20V4"/><path d="m3 8 4-4 4 4"/><path d="M7 4v16"/>',
  folderPlus:
    '<path d="M12 10v6"/><path d="M9 13h6"/><path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>',
  folder:
    '<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>',
  folderOpen:
    '<path d="m6 14 1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.54 6a2 2 0 0 1-1.95 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2"/>',
  chevronRight: '<path d="m9 18 6-6-6-6"/>',
  plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
  x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  archive:
    '<rect width="20" height="5" x="2" y="3" rx="1"/><path d="M4 8v11a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8"/><path d="M10 12h4"/>',
  settings:
    '<path d="M9.671 4.136a2.34 2.34 0 0 1 4.659 0 2.34 2.34 0 0 0 3.319 1.915 2.34 2.34 0 0 1 2.33 4.033 2.34 2.34 0 0 0 0 3.831 2.34 2.34 0 0 1-2.33 4.033 2.34 2.34 0 0 0-3.319 1.915 2.34 2.34 0 0 1-4.659 0 2.34 2.34 0 0 0-3.32-1.915 2.34 2.34 0 0 1-2.33-4.033 2.34 2.34 0 0 0 0-3.831A2.34 2.34 0 0 1 6.35 6.051a2.34 2.34 0 0 0 3.319-1.915"/><circle cx="12" cy="12" r="3"/>',
};
function svgIcon(name, cls) {
  const host = document.createElement('span');
  host.innerHTML =
    `<svg${cls ? ` class="${cls}"` : ''} viewBox="0 0 24 24" fill="none"` +
    ' stroke="currentColor" stroke-width="2" stroke-linecap="round"' +
    ` stroke-linejoin="round" aria-hidden="true">${ICONS[name] || ''}</svg>`;
  return host.firstChild;
}

let booted = false;
let refreshTimer = null;
let confirmTimer = null;
// Stored-chat id backing the live session. After a resume, state.sessionId is
// the NEW live id while the sidebar row keeps its chatId — track both so the
// active highlight survives and re-clicking the row doesn't resume again.
let activeChatId = null;
let openToken = 0; // guards openThread races (rapid thread clicks)
let accountEpoch = 0;
let observedAccountGeneration = clientAccountGeneration();
let newThreadButtonOwner = null;

function releaseNewThreadButton(owner = newThreadButtonOwner) {
  if (!owner || owner !== newThreadButtonOwner) return;
  newThreadButtonOwner = null;
  if (owner.button) owner.button.disabled = false;
}

function threadSelectionOwner(token, cwd) {
  const sessionId = state.sessionId;
  const epoch = accountEpoch;
  const generation = clientAccountGeneration();
  return () => token === openToken && epoch === accountEpoch &&
    generation === clientAccountGeneration() && hasClientAccess() &&
    sessionId === state.sessionId && cwd === (state.cwd || '');
}

function validSessionId(value) {
  return typeof value === 'string' && value.length > 0 &&
    !/[\s\x00-\x1f\x7f]/.test(value);
}

const lastSeg = (p) => (p || '').split('/').filter(Boolean).pop() || p || '';
const projectName = (p) =>
  String((p && p.name) || '').trim() || lastSeg((p && p.cwd) || '');

// --- localStorage helpers ---
function readJSON(key, fallback) {
  try {
    const v = JSON.parse(localStorage.getItem(key) || 'null');
    return v == null ? fallback : v;
  } catch {
    return fallback;
  }
}
function writeJSON(key, v) {
  try {
    localStorage.setItem(key, JSON.stringify(v));
  } catch {
    /* quota / private mode */
  }
}

// --- local POST helper (resume endpoint is not wrapped in core.daemon) ---
// Mirrors core.js request(): rejects with an Error carrying .status and the
// daemon's {error} text.
async function postJSON(path, body) {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  let data = null;
  try {
    data = await res.json();
  } catch {
    /* empty or non-JSON body */
  }
  if (!res.ok) {
    const err = new Error(
      (data && (data.error || data.message)) || `HTTP ${res.status}`,
    );
    err.status = res.status;
    throw err;
  }
  return data;
}

// --- toast (shares the #gitToasts stack + .git-toast styling with gitbar.js
// so all transient notices pile up in one corner) ---
function toast(msg, ok) {
  let box = document.getElementById('gitToasts');
  if (!box) {
    box = document.createElement('div');
    box.id = 'gitToasts';
    document.body.appendChild(box);
  }
  const t = document.createElement('div');
  t.className = 'git-toast' + (ok ? '' : ' bad');
  t.textContent = msg;
  t.title = msg;
  box.appendChild(t);
  setTimeout(() => t.remove(), ok ? 3200 : 6000);
}

// --- projects store (exported: main.js boot uses it too) ---
export function loadProjects() {
  const arr = readJSON(PROJECTS_KEY, []);
  return Array.isArray(arr)
    ? arr.filter((p) => p && typeof p.cwd === 'string' && p.cwd)
    : [];
}
export function saveProjects(list) {
  writeJSON(PROJECTS_KEY, list);
}
export function getStoredActive() {
  try {
    return localStorage.getItem(ACTIVE_KEY) || '';
  } catch {
    return '';
  }
}
// Make `cwd` the active project: state + storage (incl. legacy mirror) +
// composer folder pill label + 'workspace:changed' broadcast.
export function setActiveCwd(cwd) {
  state.cwd = cwd;
  try {
    localStorage.setItem(ACTIVE_KEY, cwd);
    localStorage.setItem(CWD_KEY, cwd);
  } catch {
    /* private mode */
  }
  const pill = $('#pillFolderName');
  if (pill) pill.textContent = lastSeg(cwd) || '資料夾';
  bus.emit('workspace:changed', { cwd });
}
// The daemon may canonicalize a picked path (workspaceCwd ≠ what we sent).
// Swap the active project entry to the canonical cwd, deduping collisions.
export function canonicalizeActive(newCwd) {
  const projects = loadProjects();
  const idx = projects.findIndex((p) => p.cwd === state.cwd);
  if (projects.some((p) => p.cwd === newCwd)) {
    if (idx >= 0) projects.splice(idx, 1); // resolved into an existing entry
  } else if (idx >= 0) projects[idx] = { ...projects[idx], cwd: newCwd };
  else projects.push({ cwd: newCwd });
  saveProjects(projects);
  setActiveCwd(newCwd);
}

// --- collapsed groups ---
function getCollapsed() {
  const arr = readJSON(COLLAPSED_KEY, []);
  return new Set(Array.isArray(arr) ? arr : []);
}
function setCollapsedSet(set) {
  writeJSON(COLLAPSED_KEY, [...set]);
}
function toggleCollapse(cwd) {
  const col = getCollapsed();
  if (!col.delete(cwd)) col.add(cwd);
  setCollapsedSet(col);
  refreshAll(); // expanding re-fetches that group's threads
}

// --- sort prefs（TS ProjectSortMenu 的 vanilla 對應；localStorage 持久化） ---
function getSortPrefs() {
  const s = readJSON(SORT_KEY, {});
  return {
    proj: s && s.proj === 'name' ? 'name' : 'added',
    thread: s && s.thread === 'title' ? 'title' : 'recent',
  };
}
function setSortPref(part, v) {
  const s = getSortPrefs();
  s[part] = v;
  writeJSON(SORT_KEY, s);
  refreshAll();
}

// --- archived sessions (frontend-only) ---
// TODO(service): there is no delete/archive endpoint yet. "Archive"
// only hides the row locally via localStorage; swap to a real daemon call
// (and migrate this list) once the endpoint exists.
function getArchived() {
  const arr = readJSON(ARCHIVE_KEY, []);
  return new Set(Array.isArray(arr) ? arr : []);
}
function setArchived(set) {
  writeJSON(ARCHIVE_KEY, [...set]);
}

// --- title overrides (inline rename) ---
// Pure frontend overrides: only localStorage changes; the ~/.meadow chat file
// keeps its original title, so the TS CLI / other frontends stay compatible.
function getTitleOverrides() {
  const m = readJSON(TITLE_KEY, {});
  return m && typeof m === 'object' && !Array.isArray(m) ? m : {};
}
function setTitleOverride(chatId, title) {
  const m = getTitleOverrides();
  if (title) m[chatId] = title;
  else delete m[chatId];
  writeJSON(TITLE_KEY, m);
}

// --- relative time ("3 分鐘前" / "昨天") ---
function toMs(m) {
  if (m == null) return 0;
  if (typeof m === 'number') return m < 1e12 ? m * 1000 : m; // epoch sec vs ms
  const t = Date.parse(m);
  return Number.isNaN(t) ? 0 : t;
}
// TS timestampFormat.formatRelativeTime 同款分段（just now / Xm / Xh / Xd ago）
// 中文化：剛剛 / X 分鐘前 / X 小時前 / X 天前。
function relTime(m) {
  const t = toMs(m);
  if (!t) return '';
  const diff = Date.now() - t;
  if (diff < 60e3) return '剛剛';
  const mins = Math.floor(diff / 60e3);
  if (mins < 60) return `${mins} 分鐘前`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours} 小時前`;
  return `${Math.floor(hours / 24)} 天前`;
}

// --- per-project usage (GET /usage?cwd=) ---
// One compact line under each project header: "12.5k tokens · 8 輪". The
// The service tallies tokens/turns per cwd across all
// of that project's threads and persists to ~/.meadow/usage.json, so the
// numbers are lifetime totals, not just the live session.
// fmtTokens: 0–999 verbatim, then k / M with one decimal (TS humanize style).
export function fmtTokens(n) {
  const v = Number(n) || 0;
  if (v < 1000) return String(v);
  if (v < 1e6)
    return (v / 1e3).toFixed(v < 1e4 ? 1 : 0).replace(/\.0$/, '') + 'k';
  return (v / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
}
// One-line summary for a project's usage payload, or '' when nothing yet.
function usageLine(u) {
  if (!u || !(u.totalTokens || u.turns)) return '';
  const turns = Number(u.turns) || 0;
  const approximate = Number(u.estimatedTokens || 0) > 0 ? '~' : '';
  return `${approximate}${fmtTokens(u.totalTokens)} tokens · ${turns} 輪`;
}
const usageCache = new Map();
function applyUsageLine(line, u) {
  if (!line) return;
  const text = usageLine(u);
  const span = line.querySelector('.pu-text');
  if (span) span.textContent = text;
  line.classList.toggle('empty', !text);
  const bar = line.querySelector('.pu-bar > i');
  if (bar && u && u.totalTokens) {
    const pct = Math.max(
      0,
      Math.min(100, (Number(u.completionTokens) / Number(u.totalTokens)) * 100),
    );
    bar.style.width = pct.toFixed(0) + '%';
  }
}
async function fetchUsage(cwd) {
  if (!cwd) return null;
  try {
    const res = await fetch(`/usage?cwd=${encodeURIComponent(cwd)}`);
    if (!res.ok) return null;
    return await res.json();
  } catch {
    return null; // daemon offline / route absent — line stays hidden
  }
}
let usageTimer = null;
// Update the .proj-usage line of every rendered project in place (no DOM
// rebuild, so it never disturbs scroll / hover / rename). Concurrent-safe:
// each row is matched back by its data-cwd after the fetch resolves.
async function refreshUsageLines() {
  const rows = document.querySelectorAll('#threadList .proj[data-cwd]');
  await Promise.all(
    [...rows].map(async (row) => {
      const cwd = row.dataset.cwd;
      const line = row.querySelector('.proj-usage');
      if (!line) return;
      const u = await fetchUsage(cwd);
      // A transient daemon miss must not blank a previously rendered value.
      // refreshAll replaces project DOM for background-run polling, so cache
      // also prevents the visible disappear/reappear flash between fetches.
      if (u) usageCache.set(cwd, u);
      applyUsageLine(line, u || usageCache.get(cwd));
    }),
  );
}

// --- small DOM helpers ---
function setHeaderTitle(t) {
  const el = $('#threadTitle');
  if (el) el.textContent = t;
}
function markActive() {
  document.querySelectorAll('#threadList .thread').forEach((el) => {
    el.classList.toggle(
      'active',
      el.dataset.sid === state.sessionId ||
        (!!activeChatId && el.dataset.sid === activeChatId),
    );
  });
}
// Inline notice in the timeline (reuses .divider pill styling).
function note(text) {
  const tl = $('#timeline');
  if (!tl) {
    console.warn('[sidebar]', text);
    return;
  }
  const div = document.createElement('div');
  div.className = 'divider';
  div.innerHTML =
    '<div class="ln"></div><div class="pill"></div><div class="ln"></div>';
  div.querySelector('.pill').textContent = text;
  tl.appendChild(div);
}
// Clear the timeline, then announce — timeline.js may subscribe
// 'thread:switched' to reset its own state; direct clear covers the case
// where it does not (parallel M5 task).
function resetTimeline(sid, live = {}) {
  const tl = $('#timeline');
  if (tl) tl.innerHTML = '';
  bus.emit('thread:switched', { sessionId: sid, ...live });
}

// --- two-step confirm (shared by archive 🗑 and project ✕) ---
// First click arms (red), second within 3s fires; leaving the row disarms.
function resetConfirm(btn) {
  if (!btn) return;
  btn.classList.remove('confirm');
  btn.title = btn.dataset.title || '';
  clearTimeout(confirmTimer);
  confirmTimer = null;
}
function twoStep(btn, armedTitle, fn) {
  if (btn.classList.contains('confirm')) {
    resetConfirm(btn);
    fn();
    return;
  }
  document.querySelectorAll('#threadList .confirm').forEach((b) => {
    b.classList.remove('confirm');
    b.title = b.dataset.title || '';
  });
  btn.classList.add('confirm');
  btn.title = armedTitle;
  clearTimeout(confirmTimer);
  confirmTimer = setTimeout(() => resetConfirm(btn), 3000);
}

// --- project actions (exported: palette.js reuses both) ---
export async function addProject() {
  let r = null;
  try {
    r = await daemon.pickFolder();
  } catch {
    return; // daemon offline
  }
  if (!r || !r.path || r.cancelled) return;
  const cwd = r.path;
  const projects = loadProjects();
  if (!projects.some((p) => p.cwd === cwd)) {
    projects.push({ cwd });
    saveProjects(projects);
  }
  const col = getCollapsed(); // surface the group right away
  if (col.delete(cwd)) setCollapsedSet(col);
  if (cwd === state.cwd) {
    refreshAll();
    return;
  }
  setActiveCwd(cwd);
  await startNewThread(); // fresh session rooted in the new project
}

export async function activateProject(cwd) {
  if (cwd === state.cwd) return;
  setActiveCwd(cwd);
  await startNewThread(); // createSession → connectEvents → 'thread:switched'
}

// Remove = forget the folder locally; daemon sessions are untouched.
function removeProject(cwd) {
  const projects = loadProjects().filter((p) => p.cwd !== cwd);
  saveProjects(projects);
  const col = getCollapsed();
  if (col.delete(cwd)) setCollapsedSet(col);
  if (cwd !== state.cwd) {
    refreshAll();
    return;
  }
  if (projects.length) {
    setActiveCwd(projects[0].cwd);
    startNewThread();
  } else {
    // last project gone: keep the live session, clear stored active so a
    // reload lands on the first-run picker instead of resurrecting it
    try {
      localStorage.removeItem(ACTIVE_KEY);
      localStorage.removeItem(CWD_KEY);
    } catch {
      /* private mode */
    }
    refreshAll();
  }
}

function setProjectPinned(cwd) {
  const projects = loadProjects();
  const project = projects.find((p) => p.cwd === cwd);
  if (!project) return;
  project.pinned = !project.pinned;
  saveProjects(projects);
  refreshAll();
}

function editProject(cwd) {
  document.querySelector('.project-edit-overlay')?.remove();
  const projects = loadProjects();
  const project = projects.find((p) => p.cwd === cwd);
  if (!project) return;
  const overlay = document.createElement('div');
  overlay.className = 'project-edit-overlay';
  const dialog = document.createElement('form');
  dialog.className = 'project-edit-dialog';
  dialog.innerHTML =
    '<h3>編輯專案</h3>' +
    '<label>顯示名稱</label><input name="name" autocomplete="off">' +
    '<label>來源資料夾</label><input name="cwd" readonly>' +
    '<div class="project-edit-actions"><button type="button" data-cancel>取消</button><button class="primary" type="submit">儲存</button></div>';
  const name = dialog.elements.namedItem('name');
  const path = dialog.elements.namedItem('cwd');
  name.value = projectName(project);
  path.value = cwd;
  const close = () => overlay.remove();
  dialog.querySelector('[data-cancel]').addEventListener('click', close);
  dialog.addEventListener('submit', (event) => {
    event.preventDefault();
    const value = name.value.trim();
    project.name = value && value !== lastSeg(cwd) ? value : '';
    saveProjects(projects);
    close();
    refreshAll();
  });
  overlay.addEventListener('pointerdown', (event) => {
    if (event.target === overlay) close();
  });
  overlay.appendChild(dialog);
  document.body.appendChild(overlay);
  requestAnimationFrame(() => {
    name.focus();
    name.select();
  });
}

async function archiveProjectThreads(cwd) {
  try {
    const result = await daemon.listSessions(cwd);
    const archived = getArchived();
    for (const session of (result && result.sessions) || []) {
      if (session && session.sessionId) archived.add(session.sessionId);
    }
    setArchived(archived);
    if (cwd === state.cwd) await startNewThread();
    else refreshAll();
    toast('已封存此專案的對話', true);
  } catch (error) {
    toast(`封存失敗：${(error && error.message) || error}`, false);
  }
}

// --- rendering ---
function scheduleRefresh(delay = 800) {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => refreshAll({ retryFailed: false }), delay);
}

const sessionLists = createSidebarSessionCache(
  (cwd, signal) => daemon.listSessions(cwd, signal),
  (cwd) => {
    const list = $('#threadList');
    const project = loadProjects().find((p) => p.cwd === cwd);
    if (!list || !project) return;
    const row = [...list.children].find((node) => node.dataset.cwd === cwd);
    // Do not destroy an in-progress display-name edit on a background refresh.
    if (!row || row.querySelector('.t-rename')) return;
    const snapshot = sessionLists.snapshot(cwd);
    row.replaceWith(renderProject(project, snapshot.rows, getCollapsed().has(cwd),
      getArchived(), getTitleOverrides(), sidebarListingNotice(snapshot)));
  },
);

function refreshAll({ retryFailed = true } = {}) {
  const list = $('#threadList');
  if (!list) return;
  if (list.querySelector('.t-rename')) return;
  let projects = loadProjects();
  sessionLists.sync(projects.map((p) => p.cwd));
  if (!projects.length) {
    list.innerHTML = '';
    const empty = document.createElement('div');
    empty.className = 'side-empty no-proj';
    empty.textContent = '尚無專案'; // TS: "No projects yet"
    list.appendChild(empty);
    return;
  }
  const sort = getSortPrefs();
  // Pinned projects are always first; the selected secondary sort remains
  // stable inside the pinned and unpinned groups.
  projects = projects
    .map((project, index) => ({ project, index }))
    .sort((a, b) =>
      Boolean(a.project.pinned) === Boolean(b.project.pinned)
        ? a.index - b.index
        : a.project.pinned
          ? -1
          : 1,
    )
    .map(({ project }) => project);
  if (sort.proj === 'name')
    projects = [...projects].sort((a, b) =>
      Boolean(a.pinned) === Boolean(b.pinned)
        ? projectName(a).localeCompare(projectName(b), 'zh-Hant')
        : a.pinned
          ? -1
          : 1,
    );
  const collapsed = getCollapsed();
  const archived = getArchived();
  const overrides = getTitleOverrides();
  const frag = document.createDocumentFragment();
  projects.forEach((p) => {
    const snapshot = sessionLists.snapshot(p.cwd);
    frag.appendChild(
      renderProject(p, snapshot.rows, collapsed.has(p.cwd), archived, overrides,
        sidebarListingNotice(snapshot)),
    );
  });
  list.innerHTML = '';
  list.appendChild(frag);
  // Shells and cached successful rows are already visible. Each project now
  // settles independently; repeated polls share its one outstanding request.
  for (const project of projects) {
    if (!collapsed.has(project.cwd)) sessionLists.refresh(project.cwd, { retryFailed });
  }
  refreshUsageLines(); // fill the per-project usage lines for the new rows
}

// 專案列＝TS SidebarProjectItem header：h-28 rounded-lg px-8、
// chevron-right(展開時轉 90°)→folder icon→名稱(12px medium fg/90)，
// hover 才浮現右側動作鈕（＋ 此專案新聊天、✕ 兩段確認移除）。
function renderProject(p, sessions, isCollapsed, archived, overrides, listingNotice = '') {
  const root = document.createElement('div');
  root.className =
    'proj' +
    (p.cwd === state.cwd ? ' active' : '') +
    (isCollapsed ? ' collapsed' : '');
  root.dataset.cwd = p.cwd;
  root.dataset.projectCwd = p.cwd;
  root.dataset.projectPinned = String(Boolean(p.pinned));
  root.classList.toggle('pinned', Boolean(p.pinned));

  const head = document.createElement('div');
  head.className = 'proj-head';
  head.dataset.projectCwd = p.cwd;
  head.dataset.projectPinned = String(Boolean(p.pinned));
  head.title = p.cwd + (p.cwd === state.cwd ? '' : '（點擊切換專案）');

  const arrow = svgIcon('chevronRight', 'p-arrow');
  arrow.addEventListener('click', (e) => {
    e.stopPropagation();
    toggleCollapse(p.cwd);
  });

  const folder = svgIcon('folder', 'p-folder');

  const name = document.createElement('span');
  name.className = 'p-name';
  name.textContent = projectName(p);

  // hover 動作群（TS：absolute right、平時 opacity-0）
  const acts = document.createElement('div');
  acts.className = 'p-acts';

  const plus = document.createElement('button');
  plus.className = 'p-plus';
  plus.title = `在 ${lastSeg(p.cwd)} 新聊天`;
  plus.appendChild(svgIcon('plus'));
  plus.addEventListener('click', (e) => {
    e.stopPropagation();
    if (p.cwd === state.cwd) startNewThread();
    else activateProject(p.cwd); // activate 內含 startNewThread
  });

  const del = document.createElement('button');
  del.className = 'p-del';
  del.appendChild(svgIcon('x'));
  del.title = del.dataset.title = '移除專案';
  del.addEventListener('click', (e) => {
    e.stopPropagation();
    twoStep(del, '再點一次確認移除', () => removeProject(p.cwd));
  });
  acts.append(plus, del);

  head.addEventListener('click', () => {
    if (p.cwd === state.cwd)
      toggleCollapse(p.cwd); // active → fold/unfold
    else activateProject(p.cwd);
  });
  head.addEventListener('mouseleave', () => resetConfirm(del));
  head.append(arrow, folder, name, acts);
  root.appendChild(head);

  // Usage line（plan-usage 風格的淡色小字＋迷你進度條）：starts empty, then
  // refreshUsageLines() fills it from GET /usage?cwd=. Only shown when the
  // project has any recorded tokens/turns.
  const usageEl = document.createElement('div');
  usageEl.className = 'proj-usage empty';
  const puText = document.createElement('span');
  puText.className = 'pu-text';
  const puBar = document.createElement('span');
  puBar.className = 'pu-bar';
  puBar.appendChild(document.createElement('i'));
  usageEl.append(puText, puBar);
  applyUsageLine(usageEl, usageCache.get(p.cwd));
  root.appendChild(usageEl);

  const body = document.createElement('div');
  body.className = 'proj-threads';
  if (!isCollapsed) {
    if (listingNotice) {
      const notice = document.createElement('div');
      notice.className = 'side-empty';
      notice.textContent = listingNotice;
      body.appendChild(notice);
    }
    if (sessions === undefined) {
      // Unknown/unavailable is not an empty successful list.
    } else {
      const byTitle = getSortPrefs().thread === 'title';
      const rows = (sessions || [])
        .filter((s) => s && s.sessionId && !archived.has(s.sessionId))
        .sort((a, b) =>
          byTitle
            ? String(a.title || '').localeCompare(
                String(b.title || ''),
                'zh-Hant',
              )
            : toMs(b.modified) - toMs(a.modified),
        );
      if (!rows.length && !listingNotice) {
        const empty = document.createElement('div');
        empty.className = 'side-empty';
        empty.textContent = '尚無對話'; // TS sidebar.no-threads-yet
        body.appendChild(empty);
      } else {
        for (const s of rows)
          body.appendChild(renderThreadRow(s, p.cwd, overrides));
      }
    }
  }
  root.appendChild(body);
  return root;
}

// thread 列＝TS SidebarThreadRow：h-28 單行 rounded-lg px-8、標題 12px 截斷
// 靠左、右側 10px 相對時間（hover 時與封存鈕 crossfade，TS opacity 互換）；
// 選中＝綠色 1px 圓角外框（focus ring 同款），非背景填充。
function renderThreadRow(s, cwd, overrides) {
  const rawTitle = String(s.title || '未命名對話');
  const shown = String((overrides && overrides[s.sessionId]) || rawTitle);
  const title = shown.slice(0, 80); // CSS ellipsis finishes the job

  const row = document.createElement('div');
  row.className =
    'thread' +
    (s.sessionId === state.sessionId || s.sessionId === activeChatId
      ? ' active'
      : '');
  row.dataset.sid = s.sessionId;
  row.dataset.liveSid = s.liveSessionId || '';
  row.classList.toggle('running', !!s.running);

  const main = document.createElement('div');
  main.className = 't-main';
  const ti = document.createElement('div');
  ti.className = 't-title';
  ti.textContent = title;
  ti.title = shown;
  main.appendChild(ti);

  // dblclick → inline rename（Enter 確認 / Esc 取消）
  main.addEventListener('dblclick', (e) => {
    e.stopPropagation();
    beginRename(row, ti, s.sessionId, rawTitle);
  });

  // 右側群（TS ml-auto cluster）：時間與封存鈕同位 crossfade
  const side = document.createElement('div');
  side.className = 't-side';
  const tm = document.createElement('div');
  tm.className = 't-time';
  if (s.running) {
    const spin = document.createElement('span');
    spin.className = 't-running';
    spin.setAttribute('aria-label', '背景執行中');
    spin.title = '背景執行中';
    tm.appendChild(spin);
  } else {
    tm.textContent = relTime(s.modified);
  }

  const arch = document.createElement('button');
  arch.className = 't-arch';
  arch.appendChild(svgIcon('archive'));
  arch.title = arch.dataset.title = '封存對話';
  arch.addEventListener('click', (e) => {
    e.stopPropagation(); // don't open the thread
    twoStep(arch, '再點一次確認封存', () => archiveThread(s.sessionId));
  });
  side.append(tm, arch);

  row.addEventListener('mouseleave', () => resetConfirm(arch));
  row.addEventListener('click', () => openThread(s.sessionId, title, cwd));
  row.append(main, side);
  return row;
}

// --- inline rename (dblclick a thread row) ---
// Display-only override persisted in localStorage "meadow.titleOverrides";
// the chat file on disk is never modified (TS interop preserved).
function beginRename(row, ti, chatId, rawTitle) {
  if (row.querySelector('.t-rename')) return;
  const input = document.createElement('input');
  input.type = 'text';
  input.className = 't-rename';
  input.value = getTitleOverrides()[chatId] || rawTitle;
  input.maxLength = 120;
  ti.style.display = 'none';
  ti.after(input);
  input.focus();
  input.select();
  let done = false;
  const finish = (commit) => {
    if (done) return;
    done = true;
    const v = input.value.trim();
    if (commit && v) {
      // typing the original title back removes the override
      setTitleOverride(chatId, v === rawTitle ? '' : v);
      ti.textContent = v.slice(0, 80);
      ti.title = v;
      if (chatId === state.sessionId || chatId === activeChatId)
        setHeaderTitle(v);
    }
    input.remove();
    ti.style.display = '';
  };
  // keep the row's open/rename handlers out of the editing session
  input.addEventListener('click', (e) => e.stopPropagation());
  input.addEventListener('dblclick', (e) => e.stopPropagation());
  input.addEventListener('keydown', (e) => {
    e.stopPropagation();
    if (e.key === 'Enter' && !e.isComposing && e.keyCode !== 229) finish(true);
    else if (e.key === 'Escape') finish(false);
  });
  input.addEventListener('blur', () => finish(false));
}

function archiveThread(sid) {
  const set = getArchived();
  set.add(sid);
  setArchived(set);
  // Archiving the active thread leaves nowhere to stand — start a fresh one.
  if (sid === state.sessionId || sid === activeChatId) startNewThread();
  else refreshAll();
}

// --- new chat (always rooted in the active project) ---
// Exported: palette.js drives this via the「新對話」action.
export async function startNewThread() {
  const btn = $('#newChatBtn');
  if (btn) btn.disabled = true;
  const token = ++openToken; // supersede any in-flight openThread/create
  const cwd = state.cwd || '';
  const isCurrent = threadSelectionOwner(token, cwd);
  const buttonOwner = { button: btn };
  newThreadButtonOwner = buttonOwner;
  try {
    const authorization = await authorizeNewConversation();
    if (!isCurrent()) return null;
    const s = await daemon.createSession(cwd, authorization);
    if (!isCurrent()) return null;
    if (!validSessionId(s?.sessionId))
      throw new Error('daemon 回應缺少有效 sessionId');
    state.sessionId = s.sessionId;
    state.runId = '';
    state.model = String((s && s.modelId) || '').trim();
    state.modelConnectionID = String(s?.connectionId || '').trim();
    activeChatId = s.sessionId; // fresh chats persist under their session id
    if (s.workspaceCwd && s.workspaceCwd !== state.cwd)
      canonicalizeActive(s.workspaceCwd);
    resetTimeline(s.sessionId);
    connectEvents(s.sessionId);
    setHeaderTitle('新對話');
    markActive();
    bus.emit('session_created', s); // same signal main.js sends at boot
    return s;
  } catch (e) {
    if (!isCurrent()) return null;
    console.error('createSession failed:', e);
    note(e?.userMessage || '無法建立新對話，請確認會員授權與連線狀態');
    return null;
  } finally {
    releaseNewThreadButton(buttonOwner);
  }
}

// --- switch to an existing thread (live resume) ---
// POST /session/resume {cwd, chatId} → {sessionId, chatId, messages}: the
// daemon opens a NEW live session preloaded with the stored chat (history +
// turn marks rebuilt), so the conversation simply continues. The returned
// messages repaint the timeline; 404 = the chat file no longer exists.

function renderHistory(messages) {
  const tl = $('#timeline');
  if (!tl) return;
  const frag = document.createDocumentFragment();
  for (const m of messages) {
    if (!m) continue;
    if (m.role === 'user') {
      const row = document.createElement('div');
      row.className = 'urow';
      const bub = document.createElement('div');
      bub.className = 'ubub';
      bub.textContent = m.content || '';
      row.appendChild(bub);
      frag.appendChild(row);
    } else {
      const row = document.createElement('div');
      row.className = 'arow';
      if (m.thought) {
        const det = document.createElement('details');
        det.className = 'think';
        const sum = document.createElement('summary');
        sum.textContent = '思考過程';
        const tx = document.createElement('div');
        tx.className = 'tx';
        tx.textContent = m.thought;
        det.append(sum, tx);
        row.appendChild(det);
      }
      const amd = document.createElement('div');
      amd.className = 'amd';
      amd.innerHTML = md(m.content || ''); // md() escapes first — XSS-safe
      row.appendChild(amd);
      frag.appendChild(row);
    }
  }
  tl.appendChild(frag);
  const sc = $('#scroll');
  if (sc) sc.scrollTop = sc.scrollHeight;
}

function resumeContract(r) {
  const missing = [];
  if (!Array.isArray(r && r.messages)) missing.push('messages[]');
  if (!Array.isArray(r && r.files)) missing.push('files[]');
  if (!Array.isArray(r && r.tools)) missing.push('tools[]');
  if (!Number.isInteger(r && r.replayAfterEventId))
    missing.push('replayAfterEventId');
  return missing;
}

function renderResumeContractError(missing) {
  if (!missing.length) return;
  const tl = $('#timeline');
  if (!tl) return;
  const card = document.createElement('div');
  card.className = 'resume-contract-error';
  card.setAttribute('role', 'alert');
  card.textContent = `舊 session durable output 契約不完整：缺少 ${missing.join(', ')}`;
  tl.appendChild(card);
}

export async function openThread(sid, title, cwd) {
  if (!sid) return;
  const tok = ++openToken;
  releaseNewThreadButton();
  if (sid === state.sessionId || sid === activeChatId) return;
  // Opening a thread from another group also makes that project active
  // (the resume lookup is cwd-scoped).
  if (cwd && cwd !== state.cwd) setActiveCwd(cwd);
  const resumeCwd = state.cwd || '';
  const isCurrent = threadSelectionOwner(tok, resumeCwd);
  let r = null;
  try {
    r = await postJSON('/session/resume', {
      cwd: resumeCwd,
      chatId: sid,
      // Sidebar navigation is inspection, not execution. A genuinely live
      // background run is still reattached server-side, but a persisted
      // interrupted/error turn must not restart merely because it was opened.
      continueInterrupted: false,
    });
  } catch (e) {
    if (!isCurrent()) return; // superseded navigation or account
    toast(
      e.status === 404
        ? '此對話檔案已不存在'
        : '無法恢復對話：' + ((e && e.message) || e),
      false,
    );
    if (e.status === 404) refreshAll(); // stale row — re-list the group
    return;
  }
  if (!isCurrent()) return; // user switched again mid-flight
  if (!validSessionId(r?.sessionId)) {
    toast('無法恢復對話：daemon 回應缺少 sessionId', false);
    return;
  }
  state.sessionId = r.sessionId;
  state.runId = String(r.runId || '');
  activeChatId = r.chatId || sid;
  // Resume restores this chat's binding, independently of the previous chat
  // selection or shared Core resident. Publish it before resetting listeners.
  const modelId = String(r.modelId || '').trim();
  state.modelConnectionID = String(r.connectionId || '').trim();
  if (modelId) {
    state.model = modelId;
    bus.emit('model_switched', { sessionId: r.sessionId, modelId });
  }
  resetTimeline(r.sessionId, {
    chatId: activeChatId,
    running: !!r.running,
    attached: !!r.attached,
  }); // clears #timeline + restores the selected chat's authoritative run state
  setHeaderTitle(title || '對話');
  markActive();
  // Repaint history. Preferred: hand the whole array to the timeline owner;
  // timeline.js (M5 current) has no 'history:load' subscriber yet, so when
  // nothing rendered we paint the simple version here (classes match
  // timeline.js: .urow/.ubub, .arow/.amd, details.think). Live SSE events
  // for new turns keep appending below as usual.
  const missingResumeFields = resumeContract(r);
  const messages = Array.isArray(r && r.messages) ? r.messages : [];
  bus.emit('history:load', messages);
  // 右欄輸出清單只吃 live 工具事件；resume 用 history 端點掃出的檔案清單
  // 重建（必須在 resetTimeline 的 'thread:switched' 清空之後 emit）。
  bus.emit('outputs:seed', Array.isArray(r && r.files) ? r.files : []);
  // Rehydrate the right-panel Progress and Expert Lens from persisted,
  // observable tool facts. This keeps a reopened task inspectable instead of
  // showing an empty panel until the next live SSE event arrives.
  bus.emit('history:tools', Array.isArray(r && r.tools) ? r.tools : []);
  const tl = $('#timeline');
  if (tl && !tl.childElementCount) renderHistory(messages);
  renderResumeContractError(missingResumeFields);
  if (missingResumeFields.length) {
    toast(
      `無法完整恢復舊 session：daemon 缺少 ${missingResumeFields.join(', ')}`,
      false,
    );
  }
  // Paint persisted history before opening SSE. Otherwise an immediate
  // replayed progress/answer event makes the timeline non-empty and the
  // history fallback skips the user bubble, which looks like a severed task.
  connectEvents(
    r.sessionId,
    Number.isInteger(r.replayAfterEventId) ? r.replayAfterEventId : 0,
  );
  if (r.running) {
    bus.emit('run_state', {
      sessionId: r.sessionId,
      chatId: activeChatId,
      status: 'running',
      restored: true,
    });
  }
}

// --- 排序選單（TS ProjectSortMenu → 既有 .pop 樣式的 vanilla popover） ---
let sortPop = null;
let sortAway = null; // document 上的 outside-click / Esc 監聽（關閉時一併拆除）
let sortEsc = null;
function closeSortMenu() {
  if (sortPop) {
    sortPop.remove();
    sortPop = null;
  }
  if (sortAway) {
    document.removeEventListener('mousedown', sortAway, true);
    sortAway = null;
  }
  if (sortEsc) {
    document.removeEventListener('keydown', sortEsc, true);
    sortEsc = null;
  }
}
function paintSortMenu() {
  if (!sortPop) return;
  const cur = getSortPrefs();
  sortPop.querySelectorAll('.it').forEach((it) => {
    const on = cur[it.dataset.part] === it.dataset.val;
    it.classList.toggle('on', on);
    it.querySelector('.chk').textContent = on ? '✓' : '';
  });
}
function toggleSortMenu(anchor) {
  if (sortPop) {
    closeSortMenu();
    return;
  }
  const pop = document.createElement('div');
  pop.className = 'pop sort-pop open';
  const groups = [
    {
      hd: '專案排序',
      part: 'proj',
      opts: [
        ['added', '加入順序'],
        ['name', '名稱'],
      ],
    },
    {
      hd: '對話排序',
      part: 'thread',
      opts: [
        ['recent', '最近活動'],
        ['title', '標題'],
      ],
    },
  ];
  groups.forEach((g, gi) => {
    if (gi) {
      const sep = document.createElement('div');
      sep.className = 'sep';
      pop.appendChild(sep);
    }
    const hd = document.createElement('div');
    hd.className = 'hd';
    hd.textContent = g.hd;
    pop.appendChild(hd);
    for (const [val, lb] of g.opts) {
      const it = document.createElement('div');
      it.className = 'it';
      it.dataset.part = g.part;
      it.dataset.val = val;
      it.innerHTML =
        '<span class="lb"></span><span class="fill"></span><span class="chk"></span>';
      it.querySelector('.lb').textContent = lb;
      it.addEventListener('click', () => {
        setSortPref(g.part, val); // radio click 不關選單（TS MenuRadioItem 同款）
        paintSortMenu();
      });
      pop.appendChild(it);
    }
  });
  document.body.appendChild(pop);
  const r = anchor.getBoundingClientRect();
  pop.style.top = `${Math.round(r.bottom + 4)}px`; // align=end side=bottom
  pop.style.left = `${Math.max(8, Math.round(r.right - pop.offsetWidth))}px`;
  sortPop = pop;
  paintSortMenu();
  sortAway = (e) => {
    if (
      sortPop &&
      !sortPop.contains(e.target) &&
      e.target !== anchor &&
      !anchor.contains(e.target)
    )
      closeSortMenu();
  };
  sortEsc = (e) => {
    if (e.key === 'Escape') closeSortMenu();
  };
  document.addEventListener('mousedown', sortAway, true);
  document.addEventListener('keydown', sortEsc, true);
}

// --- 區塊組裝（TS Sidebar 對位：搜尋列 / 專案區頭 / Footer 兩列） ---
function buildSearchRow() {
  const host = $('#sideSearch');
  if (!host) return;
  const btn = document.createElement('button');
  btn.className = 'side-row dim';
  btn.id = 'sideSearchBtn';
  btn.title = '搜尋（⌘K）';
  btn.appendChild(svgIcon('search'));
  const lb = document.createElement('span');
  lb.className = 'lb';
  lb.textContent = '搜尋'; // TS sidebar.search-placeholder
  const kbd = document.createElement('kbd');
  kbd.className = 'side-kbd';
  kbd.textContent = '⌘K';
  btn.append(lb, kbd);
  // palette.js 只掛了全域 ⌘K 監聽（無按鈕、無 export）——
  // 用合成 keydown 走同一條路，不另開入口。
  btn.addEventListener('click', () => {
    window.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'k', metaKey: true }),
    );
  });
  host.replaceChildren(btn);
}

function buildProjHead() {
  const host = $('#projHead');
  if (!host) return;
  const row = document.createElement('div');
  row.className = 'ph-row';
  const label = document.createElement('span');
  label.className = 'ph-label';
  label.textContent = '專案'; // TS sidebar.projects
  const acts = document.createElement('div');
  acts.className = 'ph-acts';
  const sortBtn = document.createElement('button');
  sortBtn.className = 'ph-icon';
  sortBtn.id = 'projSortBtn';
  sortBtn.title = '側欄排序選項';
  sortBtn.appendChild(svgIcon('arrowUpDown'));
  sortBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    toggleSortMenu(sortBtn);
  });
  const addBtn = document.createElement('button');
  addBtn.className = 'ph-icon';
  addBtn.id = 'projAddBtn';
  addBtn.title = '新增專案'; // TS sidebar.add-project
  addBtn.appendChild(svgIcon('folderPlus'));
  addBtn.addEventListener('click', () => {
    addProject();
  });
  acts.append(sortBtn, addBtn);
  row.append(label, acts);
  host.replaceChildren(row);
}

function buildFooter() {
  const host = $('#sideFoot');
  if (!host) return;
  // Account owns the absolute bottom row, matching the Desktop reference.
  // Settings remains available inside its popover instead of competing with
  // the account trigger as a second footer row.
  host.replaceChildren();
  mountCommercialAccount(host);
}

// Drag the sidebar's right edge to resize (persisted); double-click resets.
function buildResizer() {
  const side = $('#sidebar');
  if (!side || document.getElementById('sideResizer')) return;
  const KEY = 'meadow.sideWidth';
  const apply = (px) => {
    const w = Math.min(520, Math.max(220, px));
    document.documentElement.style.setProperty('--side-w', w + 'px');
    return w;
  };
  try {
    const saved = parseInt(localStorage.getItem(KEY) || '', 10);
    if (saved) apply(saved);
  } catch {
    /* private mode */
  }
  const grip = document.createElement('div');
  grip.id = 'sideResizer';
  grip.title = '拖拉調整側欄寬度（雙擊還原）';
  side.appendChild(grip);
  grip.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    grip.setPointerCapture(e.pointerId);
    const move = (ev) => {
      const w = apply(ev.clientX);
      try {
        localStorage.setItem(KEY, String(w));
      } catch {
        /* ignore */
      }
    };
    const up = () => {
      grip.removeEventListener('pointermove', move);
      grip.removeEventListener('pointerup', up);
    };
    grip.addEventListener('pointermove', move);
    grip.addEventListener('pointerup', up);
  });
  grip.addEventListener('dblclick', () => {
    document.documentElement.style.removeProperty('--side-w');
    try {
      localStorage.removeItem(KEY);
    } catch {
      /* ignore */
    }
  });
}

// --- entry ---
export function initSidebar() {
  if (booted) return;
  booted = true;
  observedAccountGeneration = clientAccountGeneration();

  const invalidateAccountSelection = (forced = false) => {
    const generation = clientAccountGeneration();
    if (!forced && hasClientAccess() && generation === observedAccountGeneration)
      return;
    observedAccountGeneration = generation;
    accountEpoch++;
    openToken++;
    releaseNewThreadButton();
  };
  window.addEventListener('commercial:change', () => invalidateAccountSelection());
  bus.on('authentication_required', () => invalidateAccountSelection(true));

  const newBtn = $('#newChatBtn');
  if (newBtn)
    newBtn.addEventListener('click', () => {
      startNewThread();
    });

  buildResizer();
  buildSearchRow();
  buildProjHead();
  buildFooter();

  bus.on('project:pin', (data) => data && setProjectPinned(data.cwd));
  bus.on('project:edit', (data) => data && editProject(data.cwd));
  bus.on('project:archive', (data) => data && archiveProjectThreads(data.cwd));
  bus.on('project:remove', (data) => data && removeProject(data.cwd));

  // ⇧⌘N＝新聊天（TS sidebar Kbd 同款快捷鍵）
  window.addEventListener('keydown', (e) => {
    if (!e.metaKey || !e.shiftKey || e.altKey || e.ctrlKey || e.repeat) return;
    if ((e.key || '').toLowerCase() !== 'n') return;
    e.preventDefault();
    startNewThread();
  });

  // Keep titles / order fresh: once per turn (user msg), per usage report,
  // on session creation, and a slow tick so relative times don't go stale.
  bus.on('update:user_message_chunk', () => scheduleRefresh(1500));
  bus.on('usage', () => scheduleRefresh(2000));
  bus.on('run_state', () => scheduleRefresh(100));
  bus.on('session_created', () => scheduleRefresh(300));
  bus.on('workspace:changed', () => scheduleRefresh(150));

  // Per-project usage lines update in place (no row rebuild): on each turn
  // boundary (live SSE 'usage' frame), when a prompt finishes ('turn:done'),
  // and on a 5s tick so a long task's running total stays current.
  bus.on('usage', refreshUsageLines);
  bus.on('turn:done', refreshUsageLines);
  clearInterval(usageTimer);
  usageTimer = setInterval(refreshUsageLines, 5000);

  refreshAll();
  // A foreground SSE is intentionally detached when another chat is opened.
  // Poll the tiny session list so background spinners still resolve promptly.
  setInterval(() => scheduleRefresh(0), 2500);
}
