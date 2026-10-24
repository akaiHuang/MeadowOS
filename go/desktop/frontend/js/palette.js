// palette.js — Cmd+K command palette: centered glassy overlay with a fuzzy
// filter, ↑↓ + Enter keyboard navigation, Esc / backdrop click to close.
// DOM is created lazily and appended to <body> (index.html untouched).
// Wire-up: main.js imports { initPalette } and calls it once at boot.
import { daemon, state } from './core.js?v=raven-execution-20261006a';
import {
  loadProjects,
  startNewThread,
  addProject,
  activateProject,
  openThread,
} from './sidebar.js';

const lastSeg = (p) => (p || '').split('/').filter(Boolean).pop() || p || '';

let overlay = null; // #palOverlay (backdrop)
let inputEl = null;
let listEl = null;
let actions = []; // rebuilt on every open — the project list is dynamic
let chatActions = [];
let filtered = [];
let sel = 0;
let booted = false;
let searchTimer = null;
let searchSeq = 0;

// Drive an existing header button; .click() on a disabled button no-ops,
// so actions stay safe while their panel (e.g. #btnDiff, M6) is not wired.
function clickBtn(selector) {
  const b = document.querySelector(selector);
  if (b) b.click();
}

//「開關側欄」: body class only; style.css tail collapses the grid column.
function toggleSidebar() {
  const collapsed = !document.body.classList.contains('side-collapsed');
  if (typeof window.__setMeadowSidebarCollapsed === 'function')
    window.__setMeadowSidebarCollapsed(collapsed);
  else document.body.classList.toggle('side-collapsed', collapsed);
}

function buildActions() {
  const acts = [
    {
      label: '新對話',
      hint: '在目前專案開新對話',
      run: () => startNewThread(),
    },
    { label: '新增專案', hint: '挑選資料夾', run: () => addProject() },
  ];
  for (const p of loadProjects()) {
    const on = p.cwd === state.cwd;
    acts.push({
      label: '切換專案：' + lastSeg(p.cwd),
      hint: on ? '目前專案' : p.cwd,
      run: () => activateProject(p.cwd),
    });
  }
  // terminal.js has no 'terminal:toggle' bus subscriber (verified at M6) —
  // the header buttons' own click handlers are the canonical toggles.
  acts.push({
    label: '開關終端機',
    hint: '⌘J',
    run: () => clickBtn('#btnTerminal'),
  });
  acts.push({
    label: '切換 Diff 面板',
    hint: '差異檢視',
    run: () => clickBtn('#btnDiff'),
  });
  acts.push({
    label: '開關側欄',
    hint: '顯示／隱藏左側專案欄',
    run: toggleSidebar,
  });
  return acts;
}

// Subsequence fuzzy match (works per-char, so CJK is fine); consecutive hits
// score higher, -1 = no match, empty query matches everything.
function fuzzy(q, text) {
  q = String(q).toLowerCase();
  const t = String(text).toLowerCase();
  if (!q) return 0;
  let qi = 0;
  let streak = 0;
  let score = 0;
  for (let i = 0; i < t.length && qi < q.length; i++) {
    if (t[i] === q[qi]) {
      qi++;
      streak++;
      score += streak;
    } else {
      streak = 0;
    }
  }
  return qi === q.length ? score : -1;
}

function paintSel() {
  const kids = listEl.children;
  for (let i = 0; i < kids.length; i++)
    kids[i].classList.toggle('sel', i === sel);
}

function ensureVisible() {
  const el = listEl.children[sel];
  if (el && el.scrollIntoView) el.scrollIntoView({ block: 'nearest' });
}

function render(scheduleSearch = true) {
  const q = inputEl.value.trim();
  if (scheduleSearch) scheduleChatSearch(q);
  const commandMatches = actions
    .map((a) => ({ a, s: fuzzy(q, a.label + ' ' + (a.hint || '')) }))
    .filter((x) => x.s >= 0)
    .sort((x, y) => y.s - x.s)
    .map((x) => x.a);
  filtered = [...commandMatches, ...chatActions];
  if (sel >= filtered.length) sel = Math.max(0, filtered.length - 1);
  listEl.innerHTML = '';
  if (!filtered.length) {
    const d = document.createElement('div');
    d.className = 'pal-empty';
    d.textContent = q ? '沒有符合的聊天或指令' : '沒有可用的指令';
    listEl.appendChild(d);
    return;
  }
  filtered.forEach((a, i) => {
    const it = document.createElement('div');
    it.className = 'pal-it' + (i === sel ? ' sel' : '');
    const lb = document.createElement('span');
    lb.className = 'lb';
    lb.textContent = a.label;
    it.appendChild(lb);
    if (a.hint) {
      const h = document.createElement('span');
      h.className = 'hint';
      h.textContent = a.hint;
      it.appendChild(h);
    }
    it.addEventListener('mouseenter', () => {
      sel = i;
      paintSel();
    });
    it.addEventListener('click', () => exec(a));
    listEl.appendChild(it);
  });
  ensureVisible();
}

function searchableProjects() {
  const projects = [...loadProjects()];
  if (state.cwd && !projects.some((project) => project.cwd === state.cwd)) {
    projects.unshift({ cwd: state.cwd });
  }
  return projects.filter(
    (project, index, all) =>
      project.cwd &&
      all.findIndex((item) => item.cwd === project.cwd) === index,
  );
}

function scheduleChatSearch(query) {
  if (searchTimer) clearTimeout(searchTimer);
  const token = ++searchSeq;
  chatActions = [];
  if (!query) return;
  searchTimer = setTimeout(async () => {
    const projects = searchableProjects();
    const batches = await Promise.allSettled(
      projects.map(async (project) => ({
        project,
        response: await daemon.searchSessions(query, project.cwd, 8),
      })),
    );
    if (token !== searchSeq || !isOpen() || inputEl.value.trim() !== query)
      return;
    const seen = new Set();
    chatActions = [];
    for (const batch of batches) {
      if (batch.status !== 'fulfilled') continue;
      const { project, response } = batch.value;
      const results = Array.isArray(response && response.results)
        ? response.results
        : [];
      for (const result of results) {
        const key = `${project.cwd}:${result.sessionId}`;
        if (!result.sessionId || seen.has(key)) continue;
        seen.add(key);
        const role = result.role === 'assistant' ? 'AI' : '你';
        chatActions.push({
          label: result.title || '未命名對話',
          hint: `${lastSeg(project.cwd)} · ${role}：${result.snippet || ''}`,
          run: () =>
            openThread(
              result.sessionId,
              result.title || '未命名對話',
              project.cwd,
            ),
        });
        if (chatActions.length >= 24) break;
      }
      if (chatActions.length >= 24) break;
    }
    sel = 0;
    render(false);
  }, 160);
}

// Execute-then-close contract: the palette is gone before the action runs.
function exec(a) {
  closePalette();
  try {
    const r = a.run();
    if (r && typeof r.catch === 'function')
      r.catch((e) => console.error('palette action failed:', e));
  } catch (e) {
    console.error('palette action failed:', e);
  }
}

function isOpen() {
  return !!overlay && overlay.classList.contains('open');
}

function openPalette() {
  if (!overlay) buildDom();
  actions = buildActions();
  chatActions = [];
  sel = 0;
  inputEl.value = '';
  overlay.classList.add('open');
  render();
  inputEl.focus();
}

function closePalette() {
  searchSeq++;
  if (searchTimer) clearTimeout(searchTimer);
  searchTimer = null;
  chatActions = [];
  if (overlay) overlay.classList.remove('open');
}

function buildDom() {
  overlay = document.createElement('div');
  overlay.id = 'palOverlay';
  const card = document.createElement('div');
  card.className = 'pal-card';
  inputEl = document.createElement('input');
  inputEl.type = 'text';
  inputEl.placeholder = '搜尋聊天內容或輸入指令…（↑↓ 選擇、Enter 執行）';
  listEl = document.createElement('div');
  listEl.className = 'pal-list';
  card.append(inputEl, listEl);
  overlay.appendChild(card);
  document.body.appendChild(overlay);

  // backdrop click (mousedown so a drag out of the card doesn't mis-close)
  overlay.addEventListener('mousedown', (e) => {
    if (e.target === overlay) closePalette();
  });
  inputEl.addEventListener('input', () => {
    sel = 0;
    render();
  });
  inputEl.addEventListener('keydown', (e) => {
    e.stopPropagation(); // keep composer / global Esc handlers out
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (filtered.length) {
        sel = (sel + 1) % filtered.length;
        paintSel();
        ensureVisible();
      }
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (filtered.length) {
        sel = (sel - 1 + filtered.length) % filtered.length;
        paintSel();
        ensureVisible();
      }
    } else if (e.key === 'Enter' && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      if (filtered[sel]) exec(filtered[sel]);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      closePalette();
    }
  });
}

export function initPalette() {
  if (booted) return;
  booted = true;
  window.addEventListener('keydown', (e) => {
    // Esc fallback (focus may sit on the list, not the input)
    if (e.key === 'Escape' && isOpen()) {
      closePalette();
      return;
    }
    if (!e.key || e.key.toLowerCase() !== 'k' || e.altKey || e.shiftKey) return;
    if (!e.metaKey && !e.ctrlKey) return;
    // Ctrl+K inside a terminal pane is shell kill-line — let it through
    // (⌘K still opens the palette there).
    const inTerm =
      e.target instanceof Element && e.target.closest('#termPanes');
    if (e.ctrlKey && !e.metaKey && inTerm) return;
    e.preventDefault();
    if (isOpen()) closePalette();
    else openPalette();
  });
}
