/* native-background-ui */
// diffpanel.js — right panel:「輸出內容／進度」dual-tab (M7 像素對齊).
// Owns #rightPanel (skeleton in index.html): slides open via the .open class
// (e2e asserts it), header #btnDiff toggles the whole panel.
//   輸出內容 tab = TS WorkspacePanel/FileListPanel：session 輸出檔案列表
//     （自訂 update:tool_call 收集，同 outputtabs.js 規則）＋點檔案→
//     POST /vcs/diff 著色檢視（+/−/@@，e2e 斷言 .dl.add）。
//   進度 tab = TS ProgressSidebar 四區塊：輸出內容（同列表）／子代理／
//     瀏覽器／來源（Go 引擎無此概念，誠實空狀態佔位）。
// 「↶ 回退此輪」(conversation rollback only — files stay on disk) 保留在頂列。
// Inputs: bus 'diff:open' {path} (timeline edit cards) → 開 panel 並切到
// 輸出內容 tab 顯示該檔 diff；bus 'update:tool_call' → 收集輸出檔。
//
// INTEGRATION REQUIRED (main.js): import { initDiffPanel } from
// './diffpanel.js' and call initDiffPanel() with the other init* calls.
import { daemon, bus, state } from './core.js?v=raven-execution-20261006a';
import {
  setRightPanelOpen,
  showRightPane,
  toggleRightPane,
  resetRightPanes,
  hasUserRightPaneSelection,
} from './rightpanel.js';

const $ = (s) => document.querySelector(s);

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
}

// same-origin POST mirroring core.js request(): throws Error whose
// .message carries the daemon's {error} text (git stderr for vcs ops)
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
    const err = new Error(
      (data && (data.error || data.message)) || `HTTP ${res.status}`,
    );
    err.status = res.status;
    throw err;
  }
  return data || {};
}

// ---------- toast (own stack; gitbar.js does not export its toast) ----------
function toast(msg, ok) {
  let box = document.getElementById('dpToasts');
  if (!box) {
    box = el('div');
    box.id = 'dpToasts';
    document.body.appendChild(box);
  }
  const t = el('div', 'dp-toast' + (ok ? '' : ' bad'), msg);
  t.title = msg;
  box.appendChild(t);
  setTimeout(() => t.remove(), ok ? 3200 : 6500);
}

// ---------- unified diff colouring (e2e asserts #diffBody .dl.add) ----------
const META_PREFIXES = [
  'diff ',
  'index ',
  'new file',
  'deleted file',
  'old mode',
  'new mode',
  'similarity',
  'rename ',
  'copy ',
  'Binary files',
  '\\ No newline',
];

function lineClass(line) {
  if (line.startsWith('+++') || line.startsWith('---')) return 'meta';
  if (line.startsWith('@@')) return 'hunk';
  if (line.startsWith('+')) return 'add';
  if (line.startsWith('-')) return 'del';
  for (const p of META_PREFIXES) if (line.startsWith(p)) return 'meta';
  return 'ctx';
}

function renderDiff(body, text) {
  body.textContent = '';
  const frag = document.createDocumentFragment();
  for (const line of text.replace(/\n$/, '').split('\n'))
    frag.appendChild(el('div', 'dl ' + lineClass(line), line || ' '));
  body.appendChild(frag);
}

function note(body, msg, bad) {
  body.textContent = '';
  body.appendChild(el('div', 'dp-note' + (bad ? ' bad' : ''), msg));
}

// ---------- session-output collection（同 outputtabs.js 的收集規則） ----------
const WRITE_KINDS = new Set(['write_file', 'save_conversation_text', 'edit']);

const baseName = (p) => p.split('/').filter(Boolean).pop() || p;

// file_path arrives absolute (tool schema asks for it) or relative (local
// models emit them anyway). /vcs/diff resolves against state.cwd, so absolute
// paths outside the workspace (e.g. /tmp scratch) are skipped.
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

// lucide file-text (ISC) — TS FileListPanel／ProgressSidebar 每列同款圖示
const FILE_ICON_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z"/><path d="M14 2v5a1 1 0 0 0 1 1h5"/><path d="M10 9H8"/><path d="M16 13H8"/><path d="M16 17H8"/></svg>';

// ---------- panel state ----------
let lastPath = ''; // most recent diffed path; #btnDiff re-opens it
let fetchSeq = 0; // drop stale /vcs/diff responses
let files = []; // committed [{rel, base}] insertion order, most recently touched last
const diffStats = new Map(); // rel -> {add, del}，從真 diff 數行（誠實資料）
const drafts = new Map(); // session/run/draft -> transient argument bytes
const pendingWrites = new Map(); // session/run/toolCallId -> candidate commit
let activeDraftKey = '';

function acceptsActiveSession(value) {
  const incoming = String((value && value.sessionId) || '');
  const active = String(state.sessionId || '');
  if (incoming && active && incoming !== active) return false;
  const incomingRun = String((value && value.runId) || '');
  const activeRun = String(state.runId || '');
  return !incomingRun || !activeRun || incomingRun === activeRun;
}

function eventSession(value) {
  return String(
    (value && value.sessionId) || state.sessionId || 'legacy-session',
  );
}

function eventRun(value) {
  return String((value && value.runId) || 'legacy-run');
}

function scopedToolKey(value) {
  const callID = String((value && (value.toolCallId || value.callId)) || '');
  if (!callID) return '';
  return [eventSession(value), eventRun(value), callID].join('\u0000');
}

function scopedDraftKey(value) {
  const identity = String(
    (value && value.draftId) ||
      `${(value && value.title) || 'tool_call'}:${Number.isInteger(value && value.index) ? value.index : -1}`,
  );
  return [eventSession(value), eventRun(value), identity].join('\u0000');
}

function completedTool(value) {
  return ['completed', 'complete', 'succeeded', 'success', 'ok'].includes(
    String((value && value.status) || '').toLowerCase(),
  );
}

export function initDiffPanel() {
  const panel = $('#rightPanel');

  // 右欄寬度拖拉（左緣拖把；persisted，雙擊還原預設 clamp）
  (function buildRpResizer() {
    if (!panel || document.getElementById('rpResizer')) return;
    const KEY = 'meadow.rpWidth';
    const apply = (px) => {
      const max = Math.round(window.innerWidth * 0.7);
      const w = Math.min(max, Math.max(320, px));
      document.documentElement.style.setProperty('--rp-w', w + 'px');
      return w;
    };
    try {
      const saved = parseInt(localStorage.getItem(KEY) || '', 10);
      if (saved) apply(saved);
    } catch {
      /* private mode */
    }
    const grip = document.createElement('div');
    grip.id = 'rpResizer';
    grip.title = '拖拉調整右欄寬度（雙擊還原）';
    panel.appendChild(grip);
    grip.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      grip.setPointerCapture(e.pointerId);
      panel.classList.add('resizing'); // 拖拉中停用 width transition
      const move = (ev) => {
        const w = apply(window.innerWidth - ev.clientX);
        try {
          localStorage.setItem(KEY, String(w));
        } catch {
          /* ignore */
        }
      };
      const up = () => {
        panel.classList.remove('resizing');
        grip.removeEventListener('pointermove', move);
        grip.removeEventListener('pointerup', up);
      };
      grip.addEventListener('pointermove', move);
      grip.addEventListener('pointerup', up);
    });
    grip.addEventListener('dblclick', () => {
      document.documentElement.style.removeProperty('--rp-w');
      try {
        localStorage.removeItem(KEY);
      } catch {
        /* ignore */
      }
    });
  })();
  const pathEl = $('#diffPath');
  const body = $('#diffBody');
  const btnDiff = $('#btnDiff');
  const tabOut = $('#rpTabOut');
  const tabProg = $('#rpTabProg');
  const outEmpty = $('#rpOutEmpty');
  const outMain = $('#rpOutMain');
  const fileList = $('#rpFileList');
  const filesCount = $('#rpFilesCount');
  const progFiles = $('#rpProgFiles');
  if (!panel || !pathEl || !body) return; // skeleton missing — nothing to own

  function setOpen(o) {
    setRightPanelOpen(o);
  }
  const isOpen = () => panel.classList.contains('open');

  // --- tabs：輸出內容 / 進度 ---
  function setTab(which, automatic = true) {
    showRightPane(which === 'out' ? 'rpPageOut' : 'rpPageProg', {
      open: false,
      automatic,
    });
  }

  // --- file rows（輸出內容＝完整相對路徑；進度＝basename，TS 同款） ---
  function statBadge(rel, sizeCls) {
    const s = diffStats.get(rel);
    if (!s) return null;
    const wrap = el('span', 'rp-stat' + (sizeCls ? ' ' + sizeCls : ''));
    wrap.appendChild(el('span', 'add', '+' + s.add));
    wrap.appendChild(document.createTextNode(' '));
    wrap.appendChild(el('span', 'del', '-' + s.del));
    return wrap;
  }

  function fileRow(f, cls, label) {
    const b = el('button', cls);
    b.type = 'button';
    b.title = f.rel;
    const ic = el('span', 'rp-fic');
    ic.innerHTML = FILE_ICON_SVG;
    b.appendChild(ic);
    b.appendChild(el('span', 'rp-fp', label));
    const st = statBadge(f.rel, cls === 'rp-prow' ? 'lg' : '');
    if (st) b.appendChild(st);
    b.addEventListener('click', () => {
      // Session artifacts are durable deliverables outside the git root.
      // They have no meaningful git diff, so open their actual contents.
      if (f.rel === '/session' || f.rel.startsWith('/session/')) {
        bus.emit('preview:open', { path: f.rel });
        return;
      }
      openWith(f.rel);
    });
    return b;
  }

  function renderLists() {
    const has = files.length > 0 || activeDraftKey !== '';
    if (outEmpty) outEmpty.hidden = has;
    if (outMain) outMain.hidden = !has;
    if (filesCount) filesCount.textContent = `${files.length} 個 session 輸出`;
    const newestFirst = [...files].reverse(); // TS collect 由新到舊
    if (fileList) {
      fileList.textContent = '';
      for (const f of newestFirst) {
        const row = fileRow(f, 'rp-file', f.rel);
        if (f.rel === lastPath) row.classList.add('sel');
        fileList.appendChild(row);
      }
    }
    if (progFiles) {
      progFiles.textContent = '';
      if (!has) {
        progFiles.appendChild(el('div', 'rp-emptyrow', '尚無輸出'));
      } else {
        // TS ProgressSidebar 輸出區最多 8 列；點列→輸出內容 tab 看 diff
        for (const f of newestFirst.slice(0, 8))
          progFiles.appendChild(fileRow(f, 'rp-prow', f.base));
      }
    }
  }

  // Streamed tool arguments are an editor preview, not durable output.  A
  // malformed/incomplete tool call can end before update:tool_call arrives,
  // so there is otherwise no commit/failure event to retire the draft.  Clear
  // every transient buffer when the active turn reaches a terminal event and
  // restore the most recent committed file (if any).
  function clearTransientDrafts(value) {
    if (value && !acceptsActiveSession(value)) return;
    if (!drafts.size && !pendingWrites.size && !activeDraftKey) return;
    drafts.clear();
    pendingWrites.clear();
    activeDraftKey = '';
    const fallback =
      (lastPath && files.some((file) => file.rel === lastPath) && lastPath) ||
      (files.length ? files[files.length - 1].rel : '');
    renderLists();
    if (fallback) {
      openWith(fallback, true);
      return;
    }
    pathEl.textContent = '尚未選取檔案';
    pathEl.title = '';
    note(body, '選取一個輸出檔案來預覽。');
  }

  // Decode one JSON string even while its closing quote has not arrived yet.
  // This is preview-only: malformed/incomplete bytes are never executed or
  // written to disk. The provider's final JSON decoder remains authoritative.
  function partialJSONString(raw, key) {
    const marker = new RegExp(`"${key}"\\s*:\\s*"`, 'm');
    const match = marker.exec(raw);
    if (!match) return '';
    let out = '';
    let i = match.index + match[0].length;
    for (; i < raw.length; i++) {
      const ch = raw[i];
      if (ch === '"') break;
      if (ch !== '\\') {
        out += ch;
        continue;
      }
      if (++i >= raw.length) break;
      const escaped = raw[i];
      const simple = {
        '"': '"',
        '\\': '\\',
        '/': '/',
        b: '\b',
        f: '\f',
        n: '\n',
        r: '\r',
        t: '\t',
      };
      if (Object.prototype.hasOwnProperty.call(simple, escaped)) {
        out += simple[escaped];
      } else if (escaped === 'u') {
        const hex = raw.slice(i + 1, i + 5);
        if (!/^[0-9a-fA-F]{4}$/.test(hex)) break;
        out += String.fromCharCode(parseInt(hex, 16));
        i += 4;
      } else {
        out += escaped;
      }
    }
    return out;
  }

  // Some structured tool streams use tagged parameters rather than JSON.
  // Keep this preview-only parser deliberately small: accumulated bytes are
  // displayed, never executed or persisted, and the final native tool result
  // remains the only commit boundary.
  function partialTaggedParameter(raw, key, trim = false) {
    const markers = [
      `<parameter=${key}>`,
      `<parameter name="${key}">`,
      `<parameter name='${key}'>`,
      `<parameter name="${key}" value="`,
      `<parameter name='${key}' value='`,
    ];
    for (const marker of markers) {
      const start = raw.indexOf(marker);
      if (start < 0) continue;
      const valueStart = start + marker.length;
      const close = raw.indexOf('</parameter>', valueStart);
      const value = raw.slice(valueStart, close < 0 ? raw.length : close);
      return trim ? value.trim() : value.replace(/^\r?\n/, '');
    }
    return '';
  }

  // Gemma 4 occasionally places file_path as a trailing attribute after its
  // content parameter instead of producing a second parameter element. This
  // remains preview-only; the provider separately requires a complete HTML
  // document and a normalized /workspace/*.html path before execution.
  function partialLooseAttribute(raw, key) {
    const marker = new RegExp(
      `(?:^|[\\s>])${key}\\s*=\\s*["']([^"']*)["']`,
      'm',
    );
    const match = marker.exec(raw);
    return match ? match[1].trim() : '';
  }

  function partialLooseNamedParameter(raw, key) {
    const marker = new RegExp(
      `</?parameter\\s+name\\s*=\\s*["']${key}["']\\s*>\\s*([^<]*)`,
      'm',
    );
    const match = marker.exec(raw);
    return match ? match[1].trim() : '';
  }

  function renderSourceDraft(key, path, content, bytes) {
    activeDraftKey = key;
    setTab('out');
    renderLists();
    pathEl.textContent = `草稿 · ${path || '等待檔名…'} · ${bytes} bytes`;
    pathEl.title = '模型仍在產生內容；尚未寫入磁碟';
    body.textContent = '';
    const source = el('pre', 'dp-draft-source');
    source.textContent = content || '正在接收檔案內容…';
    body.appendChild(source);
    body.scrollTop = body.scrollHeight;
  }

  function onToolDraft(u) {
    if (!u || !acceptsActiveSession(u) || !u.argumentsDelta) return;
    const name = u.title || '';
    // Conversation saves carry a source reference, not a streamed source draft.
    if (name !== 'write_file' && name !== 'edit') return;
    const key = scopedDraftKey(u);
    const previous = drafts.get(key);
    const raw = ((previous && previous.raw) || '') + u.argumentsDelta;
    drafts.set(key, {
      raw,
      sessionId: eventSession(u),
      runId: eventRun(u),
      name,
    });
    const contentKey = name === 'write_file' ? 'content' : 'new_string';
    const path = toRel(
      partialJSONString(raw, 'file_path') ||
        partialTaggedParameter(raw, 'file_path', true) ||
        partialLooseAttribute(raw, 'file_path') ||
        partialLooseNamedParameter(raw, 'file_path'),
    );
    const content =
      partialJSONString(raw, contentKey) ||
      partialTaggedParameter(raw, contentKey);
    renderSourceDraft(key, path, content, Number(u.bytes) || raw.length);
  }

  function remember(rel) {
    const i = files.findIndex((f) => f.rel === rel);
    if (i >= 0) files.splice(i, 1); // re-touched → move to the recent end
    files.push({ rel, base: baseName(rel) });
  }

  function onToolCall(u) {
    if (!u || !acceptsActiveSession(u)) return;
    if (!WRITE_KINDS.has(u.kind || u.title)) return;
    const rel = toRel(u.rawInput && u.rawInput.file_path);
    const name = u.kind || u.title;
    const sessionId = eventSession(u);
    const runId = eventRun(u);
    for (const [key, draft] of drafts) {
      if (
        (u.draftId && key === scopedDraftKey(u)) ||
        (!u.draftId &&
          draft.sessionId === sessionId &&
          draft.runId === runId &&
          draft.name === name)
      ) {
        drafts.delete(key);
        if (activeDraftKey === key) activeDraftKey = '';
      }
    }
    const key = scopedToolKey(u);
    if (key) pendingWrites.set(key, { rel, draftId: u.draftId || '' });
    if (!rel) return;
    renderLists();
    setTab('out');
    pathEl.textContent = rel;
    pathEl.title = rel;
    note(
      body,
      name === 'save_conversation_text'
        ? '已選取對話原文，正在原子儲存…'
        : '草稿已完成，正在原子儲存與驗證…',
    );
  }

  function onToolUpdate(u) {
    if (!u || !acceptsActiveSession(u)) return;
    const key = scopedToolKey(u);
    const pending = key ? pendingWrites.get(key) : null;
    const observedPaths = Array.isArray(u.changedPaths) ? u.changedPaths : [];
    let rel = '';
    for (const path of observedPaths) {
      rel = toRel(path);
      if (rel) break;
    }
    if (!rel && WRITE_KINDS.has(u.kind || u.title))
      rel = toRel(u.rawInput && u.rawInput.file_path);
    if (!rel && pending) rel = pending.rel;
    const draftKey = u.draftId ? scopedDraftKey(u) : '';
    if (draftKey) {
      drafts.delete(draftKey);
      if (activeDraftKey === draftKey) activeDraftKey = '';
    }
    if (completedTool(u)) {
      if (key) pendingWrites.delete(key);
      if (!rel) return;
      activeDraftKey = '';
      remember(rel);
      renderLists();
      if (rel === '/session' || rel.startsWith('/session/')) {
        bus.emit('preview:open', { path: rel, automatic: true });
      } else {
        openWith(rel, true);
      }
    } else if (u.status === 'failed') {
      if (key) pendingWrites.delete(key);
      if (!rel) return;
      activeDraftKey = `failed:${key || rel}`;
      setTab('out');
      renderLists();
      pathEl.textContent = rel;
      pathEl.title = rel;
      note(
        body,
        '儲存失敗：' + (u.rawOutput || '檔案未變更；草稿未寫入磁碟'),
        true,
      );
    }
  }

  async function openWith(path, automatic = false) {
    if (!path) return;
    // diff:open 可能先於收集（如歷史 editcard）——它確實是 session 輸出，補進列表
    if (!files.some((f) => f.rel === path)) remember(path);
    lastPath = path;
    if (!automatic) setOpen(true);
    setTab('out', automatic);
    renderLists(); // 同步 .sel 選取標記
    pathEl.textContent = path;
    pathEl.title = path;
    note(body, '載入差異中…');
    const my = ++fetchSeq;
    let d;
    try {
      d = await post('/vcs/diff', { cwd: state.cwd, path });
    } catch (e) {
      if (my !== fetchSeq) return;
      note(body, '取得差異失敗：' + ((e && e.message) || e), true);
      return;
    }
    if (my !== fetchSeq) return;
    if (d.binary === true) {
      note(body, '二進位檔案');
      return;
    }
    const diff = typeof d.diff === 'string' ? d.diff : '';
    if (!diff.trim()) {
      note(body, '沒有差異（與 HEAD 相同）');
      return;
    }
    renderDiff(body, diff);
    // +N −N badge（TS 檔案列右側）：從剛抓到的 diff 數行
    let add = 0;
    let del = 0;
    for (const line of diff.split('\n')) {
      if (line.startsWith('+') && !line.startsWith('+++')) add++;
      else if (line.startsWith('-') && !line.startsWith('---')) del++;
    }
    diffStats.set(path, { add, del });
    renderLists();
  }

  if (tabOut)
    tabOut.addEventListener('click', () => toggleRightPane('rpPageOut'));
  if (tabProg)
    tabProg.addEventListener('click', () => toggleRightPane('rpPageProg'));

  $('#diffClose').addEventListener('click', () => setOpen(false));

  // Rollback rewinds the conversation only; the daemon does not touch the
  // working tree — the toast copy is honest about that.
  $('#diffRollback')?.addEventListener('click', async (e) => {
    const b = e.currentTarget;
    if (!state.sessionId) {
      toast('回退失敗：沒有作用中的對話', false);
      return;
    }
    b.disabled = true;
    try {
      await daemon.rollback(state.sessionId, 1);
      toast('已回退 1 輪（檔案還原請用 git）', true);
    } catch (err) {
      // 404 = unknown session/turn, 409 = turn in progress; daemon text shown
      toast('回退失敗：' + ((err && err.message) || err), false);
    } finally {
      b.disabled = false;
    }
  });

  bus.on('diff:open', (d) => {
    if (d && d.path) openWith(d.path);
  });

  bus.on('update:tool_call', onToolCall);
  bus.on('update:tool_call_draft', onToolDraft);
  bus.on('update:tool_call_update', onToolUpdate);

  // resume 重建：後端掃 stored chat 工具呼叫回傳的檔案清單（舊→新），
  // 餵進同一條 remember 流水線；workspace 外路徑由 toRel 自然濾掉。
  bus.on('outputs:seed', (paths) => {
    let any = false;
    for (const p of paths || []) {
      const rel = toRel(p);
      if (!rel) continue;
      remember(rel);
      any = true;
    }
    if (any) renderLists();
  });

  // Both names are kept because the Desktop SSE bridge emits stream_error
  // immediately, then turn:done after it closes the active run.  Either event
  // must be sufficient on its own; duplicate delivery is intentionally a no-op.
  bus.on('stream_error', clearTransientDrafts);
  bus.on('turn_completed', clearTransientDrafts);
  bus.on('turn:done', clearTransientDrafts);

  if (btnDiff) {
    btnDiff.hidden = false; // belt & braces with the index.html edit
    btnDiff.disabled = false;
    btnDiff.title = '右側預覽'; // TS ChatHeader 的 preview toggle tooltip
    btnDiff.addEventListener('click', () => {
      if (isOpen()) {
        setOpen(false);
        return;
      }
      setOpen(true);
      // Header icon means preview, not "restore whichever right-panel tab was
      // active last". preview.js owns the empty/file preview page.
      if (!hasUserRightPaneSelection()) bus.emit('preview:show');
    });
  }

  // thread/project switch: the remembered paths belong to the old workspace
  bus.on('thread:switched', () => {
    lastPath = '';
    fetchSeq++;
    files = [];
    diffStats.clear();
    drafts.clear();
    pendingWrites.clear();
    activeDraftKey = '';
    setOpen(false);
    resetRightPanes();
    pathEl.textContent = '尚未選取檔案'; // TS path 列 idle 文案
    pathEl.title = '';
    note(body, '選取一個輸出檔案來預覽。'); // TS preview idle 文案
    renderLists();
  });

  // 初始：空狀態＋idle 文案
  note(body, '選取一個輸出檔案來預覽。');
  renderLists();
}
