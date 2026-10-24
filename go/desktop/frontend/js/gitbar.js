// gitbar.js — git 工具列 + header 動作列（M7 像素對齊 TS ChatHeader.tsx）。
// 1) 分支膠囊：掛到 composer 下方 #branchSlot（無則退回 #gitArea，該容器已 display:none）；
//    顯示 branch / dirty 點 / ahead-behind，每 5s 與每輪結束後刷新；點開 .pop 彈窗
//    可切分支（過濾＋建立）、commit（add-all）、push、pull（daemon /vcs/*）。
// 2) header 動作列（index.html #headerActions 靜態骨架，本檔接線）：
//    #btnSideToggle 開關側欄（同 palette 的 body.side-collapsed）、#projBadge 專案 basename、
//    #btnNewAction → startNewThread、#btnEditorMenu →「在編輯器開啟」下拉（/native/editors +
//    /native/open-editor，與 settings.js 共用 meadow.editor 偏好；#btnEditor 主鈕仍由 settings.js 接）、
//    #gitQuickGroup → TS GitActionsControl quick action（Commit / Push / Publish repository ＋ ▾ 選單）。
import { bus, state } from './core.js?v=raven-execution-20261006a';
import { startNewThread } from './sidebar.js';

const $ = (s) => document.querySelector(s);

const EDITOR_KEY = 'meadow.editor'; // settings.js 同 key：偏好編輯器

// lucide icons (ISC) — 同 TS 取用的 GitBranch / GitCommit / CloudUpload
const GIT_BRANCH_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 6a9 9 0 0 0-9 9V3"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/></svg>';
const GIT_COMMIT_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="3"/><path d="M3 12h6"/><path d="M15 12h6"/></svg>';
const CLOUD_UP_SVG =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 13v8"/><path d="M4 14.899A7 7 0 1 1 15.71 8h1.79a4.5 4.5 0 0 1 2.5 8.242"/><path d="m8 17 4-4 4 4"/></svg>';

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
    throw new Error(
      (data && (data.error || data.message)) || `HTTP ${res.status}`,
    );
  }
  return data;
}

async function get(path) {
  const res = await fetch(path);
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

const lsGet = (k) => {
  try {
    return localStorage.getItem(k) || '';
  } catch {
    return '';
  }
};
const lsSet = (k, v) => {
  try {
    localStorage.setItem(k, v);
  } catch {
    /* private mode */
  }
};

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}

// 共用彈窗定位：右緣對齊 anchor（TS MenuPopup align="end"）、下方擠不下時翻到上方
function placePop(pop, anchor) {
  pop.style.visibility = 'hidden';
  pop.classList.add('open');
  const r = anchor.getBoundingClientRect();
  const w = pop.offsetWidth;
  const h = pop.offsetHeight;
  const left = Math.max(8, Math.min(r.right - w, window.innerWidth - w - 8));
  let top = r.bottom + 8;
  if (top + h > window.innerHeight - 8) top = r.top - h - 8;
  top = Math.max(8, Math.min(top, window.innerHeight - h - 8));
  pop.style.left = left + 'px';
  pop.style.top = top + 'px';
  pop.style.visibility = '';
}

// ---------- toast ----------
function toast(msg, ok) {
  let box = document.getElementById('gitToasts');
  if (!box) {
    box = el('div');
    box.id = 'gitToasts';
    document.body.appendChild(box);
  }
  const t = el('div', 'git-toast' + (ok ? '' : ' bad'), msg);
  t.title = msg;
  box.appendChild(t);
  setTimeout(() => t.remove(), ok ? 3200 : 6500);
}

// ---------- header 動作列（與 git 狀態無關的部分） ----------
function initHeaderChrome() {
  // 側欄完整收合；恢復入口就是主視窗 header 的同一顆按鈕。
  // DOM 中保留舊 sideRail 只為相容既有 bundle，產品介面永不顯示它。
  const tog = $('#btnSideToggle');
  const railExpand = $('#sideRailExpand');
  const setCollapsed = (collapsed) => {
    document.body.classList.toggle('side-collapsed', collapsed);
    tog?.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    railExpand?.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    $('#sideRail')?.setAttribute('aria-hidden', 'true');
  };
  window.__setMeadowSidebarCollapsed = setCollapsed;
  setCollapsed(document.body.classList.contains('side-collapsed'));
  tog?.setAttribute('aria-controls', 'sidebar');
  tog?.addEventListener('click', () =>
    setCollapsed(!document.body.classList.contains('side-collapsed')),
  );
  railExpand?.addEventListener('click', () => setCollapsed(false));

  // 專案 badge＝TS activeProjectName Badge（目前專案資料夾 basename）
  const badge = $('#projBadge');
  const paintBadge = () => {
    if (!badge) return;
    const base = (state.cwd || '').replace(/\/+$/, '').split('/').pop() || '';
    badge.textContent = base;
    badge.title = state.cwd || '';
    badge.hidden = !base;
  };
  paintBadge();
  bus.on('thread:switched', paintBadge);

  // ＋ 新增動作＝TS ProjectScriptsControl 空清單態；先接「開新對話」
  const na = $('#btnNewAction');
  if (na)
    na.addEventListener('click', () => {
      startNewThread();
    });

  initEditorMenu();
}

// 「開啟 ▾」下拉＝TS OpenInPicker 的編輯器清單（主鈕 #btnEditor 由 settings.js 接）
function initEditorMenu() {
  const grp = $('#openGroup');
  const mBtn = $('#btnEditorMenu');
  if (!mBtn) return;
  const pop = el('div', 'pop ed-pop');
  document.body.appendChild(pop);
  let open = false;
  const close = () => {
    open = false;
    pop.classList.remove('open');
  };
  document.addEventListener('click', (e) => {
    if (!open) return;
    if (pop.contains(e.target) || (grp && grp.contains(e.target))) return;
    close();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && open) close();
  });
  mBtn.addEventListener('click', async () => {
    if (open) {
      close();
      return;
    }
    open = true;
    pop.innerHTML = '';
    pop.appendChild(el('div', 'hd', '在編輯器開啟'));
    pop.appendChild(el('div', 'git-hint', '偵測編輯器中…'));
    placePop(pop, grp || mBtn);
    let editors = [];
    try {
      const r = await get('/native/editors');
      editors = (r && r.editors) || [];
    } catch {
      /* daemon 離線 → 空清單 */
    }
    if (!open) return;
    pop.innerHTML = '';
    pop.appendChild(el('div', 'hd', '在編輯器開啟'));
    if (!editors.length) {
      pop.appendChild(el('div', 'git-hint', '未偵測到編輯器'));
    } else {
      const stored = lsGet(EDITOR_KEY);
      const preferred = editors.includes(stored) ? stored : editors[0];
      for (const name of editors) {
        const it = el('div', 'it' + (name === preferred ? ' on' : ''));
        it.appendChild(el('span', 'lb', name));
        if (name === preferred) {
          it.appendChild(el('span', 'fill'));
          it.appendChild(el('span', 'chk', '✓'));
        }
        it.addEventListener('click', async () => {
          close();
          lsSet(EDITOR_KEY, name); // #btnEditor 主鈕（settings.js）跟著換偏好
          if (!state.cwd) {
            toast('尚未選擇專案資料夾', false);
            return;
          }
          try {
            const r = await post('/native/open-editor', {
              path: state.cwd,
              editor: name,
            });
            toast('已在 ' + ((r && r.editor) || name) + ' 開啟專案', true);
          } catch (e2) {
            toast('開啟失敗：' + ((e2 && e2.message) || e2), false);
          }
        });
        pop.appendChild(it);
      }
    }
    placePop(pop, grp || mBtn);
  });
}

export function initGitbar() {
  initHeaderChrome();

  // 分支膠囊掛載點：composer 下方 #branchSlot（M7）；舊 #gitArea 僅作後援（已隱藏）
  const host = $('#branchSlot') || $('#gitArea');
  if (!host) return;

  // ---------- branch pill（composer 列） ----------
  host.innerHTML = '';
  const btn = el('button', 'git-btn');
  btn.hidden = true;
  const icEl = el('span', 'git-ic');
  icEl.innerHTML = GIT_BRANCH_SVG;
  const branchEl = el('span', 'git-branch', '');
  const dotEl = el('span', 'git-dot');
  dotEl.hidden = true;
  const abEl = el('span', 'git-ab', '');
  abEl.hidden = true;
  btn.append(icEl, branchEl, dotEl, abEl);
  // 非 repo 狀態：可點的「初始化 Git」鈕（git init → 刷新）。沿用 .git-btn 外觀。
  const initBtn = el('button', 'git-btn git-init-btn');
  const initIc = el('span', 'git-ic');
  initIc.innerHTML = GIT_BRANCH_SVG;
  initBtn.append(initIc, el('span', 'git-branch', '初始化 Git'));
  initBtn.title = '在此資料夾建立 Git 儲存庫（git init）';
  initBtn.hidden = true;
  host.append(btn, initBtn);

  async function doInit() {
    if (!state.cwd || initBtn.disabled) return;
    initBtn.disabled = true;
    try {
      const r = await post('/vcs/init', { cwd: state.cwd });
      toast(
        r && r.already ? '此資料夾已是 Git 儲存庫' : '已初始化 Git 儲存庫',
        true,
      );
      refresh();
    } catch (e) {
      toast('初始化失敗：' + ((e && e.message) || e), false);
    }
    initBtn.disabled = false;
  }
  initBtn.addEventListener('click', doInit);

  // 新對話只選資料夾不碰 git：對話開始（有訊息）才顯示整個 git 區
  //（含分支膠囊與初始化鈕）。與 composer 的資料夾鎖共用同三個訊號。
  const reflectGitVisibility = () => {
    host.hidden = !state.hasMessages;
  };
  reflectGitVisibility();
  bus.on('thread:switched', reflectGitVisibility);
  bus.on('update:user_message_chunk', reflectGitVisibility);
  bus.on('history:load', reflectGitVisibility);

  // ---------- header quick action（TS GitActionsControl split group） ----------
  const qGrp = $('#gitQuickGroup');
  const qBtn = $('#btnGitQuick');
  const qIc = $('#gitQuickIc');
  const qLb = $('#gitQuickLb');
  const qMenu = $('#btnGitMenu');

  const pop = el('div', 'pop git-pop');
  document.body.appendChild(pop);

  let cur = null; // last /vcs/status payload
  let open = false;
  let anchorEl = null; // 觸發來源（header 群組或 composer 膠囊）

  // TS resolveQuickAction 簡化版：dirty→Commit、ahead→Push、其餘→Publish repository
  function paintQuick(st) {
    if (!qGrp || !qBtn || !qIc || !qLb) return;
    if (!st || st.isRepo !== true) {
      qGrp.hidden = true;
      return;
    }
    qGrp.hidden = false;
    const mode = st.dirty ? 'commit' : st.ahead > 0 ? 'push' : 'publish';
    qBtn.dataset.act = mode;
    if (mode === 'commit') {
      qIc.innerHTML = GIT_COMMIT_SVG;
      qLb.textContent = 'Commit';
      qBtn.title = `${(st.files || []).length} 個變更未提交`;
    } else if (mode === 'push') {
      qIc.innerHTML = CLOUD_UP_SVG;
      qLb.textContent = 'Push';
      qBtn.title = `推送 ${st.ahead} 個 commit`;
    } else {
      qIc.innerHTML = CLOUD_UP_SVG;
      qLb.textContent = 'Publish repository';
      qBtn.title = '發佈／推送到遠端儲存庫';
    }
  }

  // ---------- status polling ----------
  let inflight = false;
  let seq = 0;

  function paint(st) {
    if (!st || st.isRepo !== true) {
      btn.hidden = true;
      initBtn.hidden = !state.cwd; // no workspace yet → show nothing
      paintQuick(null);
      if (open) closePop();
      return;
    }
    initBtn.hidden = true;
    btn.hidden = false;
    branchEl.textContent = st.branch || 'HEAD';
    dotEl.hidden = !st.dirty;
    const ab =
      (st.ahead > 0 ? '↑' + st.ahead : '') +
      (st.behind > 0 ? '↓' + st.behind : '');
    abEl.textContent = ab;
    abEl.hidden = !ab;
    const n = (st.files || []).length;
    btn.title =
      (st.branch || 'HEAD') +
      (st.dirty ? ` · ${n} 個變更` : ' · 乾淨') +
      (ab ? ` · ${ab}` : '');
    paintQuick(st);
  }

  async function refresh() {
    if (!state.cwd || inflight) return;
    inflight = true;
    const my = ++seq;
    try {
      const st = await post('/vcs/status', { cwd: state.cwd });
      if (my === seq) {
        cur = st;
        paint(st);
      }
    } catch {
      /* daemon hiccup — keep last paint */
    }
    inflight = false;
  }

  refresh();
  setInterval(() => {
    if (!document.hidden) refresh();
  }, 5000);
  bus.on('turn:done', refresh);
  // project/thread switch: re-fetch immediately
  bus.on('thread:switched', () => {
    seq++; // drop any stale in-flight result
    inflight = false;
    refresh();
  });

  // ---------- popover ----------
  function place() {
    placePop(pop, anchorEl || btn);
  }

  function closePop() {
    open = false;
    pushing = false; // 關閉 pop 即放棄進行中的 push 流程
    pop.classList.remove('open', 'busy');
  }

  document.addEventListener('click', (e) => {
    if (!open) return;
    if (
      pop.contains(e.target) ||
      btn.contains(e.target) ||
      (qGrp && qGrp.contains(e.target))
    )
      return;
    closePop();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && open) closePop();
  });

  const busyPop = (b) => pop.classList.toggle('busy', b);

  // one action at a time; success closes the pop and re-polls status
  async function run(fn, okMsg, failPrefix) {
    busyPop(true);
    try {
      const r = await fn();
      closePop();
      toast(typeof okMsg === 'function' ? okMsg(r) : okMsg, true);
      refresh();
    } catch (e) {
      busyPop(false);
      toast(failPrefix + '：' + ((e && e.message) || e), false);
    }
  }

  const doSwitch = (refName, create) =>
    run(
      () => post('/vcs/switch', { cwd: state.cwd, refName, create }),
      (r) =>
        (create ? '已建立並切換到 ' : '已切換到 ') +
        ((r && r.branch) || refName),
      '切換失敗',
    );

  // ---------- push 流程（掃機密 → 警告卡 → remote → push） ----------
  // 整條流程的互動步驟（機密警告卡、輸入 remote URL）都渲染進 popover；
  // 觸發來源可能是 popover 的 Push 鈕或 header quick action，後者先把 pop 開起來。
  let pushing = false;

  // git push 失敗訊息判讀：含認證字樣 → 提示終端機設定 git 認證
  function pushFailToast(m) {
    const s = String(m || '');
    if (
      /authentication failed|could not read username|could not read password|permission denied|invalid username or password|403|terminal prompts disabled|fatal: could not read/i.test(
        s,
      )
    ) {
      toast(
        '推送失敗：git 認證未設定。\n請先在終端機執行 gh auth login，或設定 git credential 後重試。',
        false,
      );
      return;
    }
    if (
      /could not read from remote|repository not found|no configured push destination|does not appear to be a git repository/i.test(
        s,
      )
    ) {
      toast(
        '推送失敗：找不到遠端儲存庫。\n請確認 remote URL 是否正確。',
        false,
      );
      return;
    }
    toast('推送失敗：' + s, false);
  }

  // 實際送出 git push（remote 已就緒）；done 回呼讓 UI 收尾
  async function doPush(done) {
    try {
      await post('/vcs/push', { cwd: state.cwd });
      toast('已推送到遠端', true);
      done(true);
    } catch (e) {
      pushFailToast((e && e.message) || e);
      done(false);
    }
    refresh();
  }

  // remote 不存在時，於 pop 內請使用者輸入 URL → /vcs/set-remote → 續推
  function renderRemotePrompt(done) {
    pop.innerHTML = '';
    pop.appendChild(el('div', 'hd', '設定遠端儲存庫'));
    pop.appendChild(
      el('div', 'git-hint', '尚未設定遠端，請輸入 remote URL 後推送：'),
    );
    const input = el('input');
    input.type = 'text';
    input.placeholder = 'https://github.com/使用者/儲存庫.git';
    pop.appendChild(input);
    const row = el('div', 'git-actions');
    const okBtn = el('button', 'btn primary', '設定並推送');
    const cancelBtn = el('button', 'btn', '取消');
    row.append(okBtn, cancelBtn);
    pop.appendChild(row);
    place();
    input.focus();

    const submit = async () => {
      const url = input.value.trim();
      if (!url) return;
      okBtn.disabled = true;
      cancelBtn.disabled = true;
      try {
        await post('/vcs/set-remote', { cwd: state.cwd, url });
      } catch (e) {
        toast('設定遠端失敗：' + ((e && e.message) || e), false);
        done(false);
        return;
      }
      await doPush((ok) => {
        closePop();
        done(ok);
      });
    };
    okBtn.addEventListener('click', submit);
    cancelBtn.addEventListener('click', () => {
      closePop();
      done(false);
    });
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.isComposing) submit();
    });
  }

  // remote 檢查 → 有就直接推、沒有就先要 URL
  async function pushAfterScan(done) {
    let hasRemote = false;
    try {
      const r = await get('/vcs/remote?cwd=' + encodeURIComponent(state.cwd));
      hasRemote = !!(r && r.hasRemote);
    } catch {
      hasRemote = false; // 查不到當作沒有 → 走輸入 URL
    }
    if (!hasRemote) {
      renderRemotePrompt(done);
      return;
    }
    await doPush((ok) => {
      if (open) closePop();
      done(ok);
    });
  }

  // 機密外洩警告卡：列出疑似洩漏檔案＋kind（紅色），二次確認才放行
  function renderSecretWarning(findings, done) {
    pop.innerHTML = '';
    pop.appendChild(el('div', 'hd git-warn-hd', '⚠ 偵測到疑似機密外洩'));
    pop.appendChild(
      el(
        'div',
        'git-hint',
        '以下變更可能含有金鑰／密碼，推送到遠端後將難以撤回：',
      ),
    );
    const list = el('div', 'git-secret-list');
    for (const f of findings) {
      const it = el('div', 'git-secret-it');
      const loc = f.file + (f.line ? ':' + f.line : '');
      it.appendChild(el('span', 'git-secret-kind', f.kind || '機密'));
      it.appendChild(el('span', 'git-secret-loc', loc));
      if (f.preview) it.appendChild(el('span', 'git-secret-prev', f.preview));
      list.appendChild(it);
    }
    pop.appendChild(list);

    let armed = false; // 第一次點「仍要推送」只武裝、第二次才真推
    const row = el('div', 'git-actions');
    const goBtn = el('button', 'btn git-danger', '仍要推送');
    const cancelBtn = el('button', 'btn', '取消');
    row.append(goBtn, cancelBtn);
    pop.appendChild(row);
    const hint = el('div', 'git-hint git-warn-confirm');
    hint.hidden = true;
    hint.textContent = '再次點擊「仍要推送」以確認略過警告。';
    pop.appendChild(hint);
    place();

    goBtn.addEventListener('click', () => {
      if (!armed) {
        armed = true;
        goBtn.textContent = '確認推送（略過警告）';
        hint.hidden = false;
        place();
        return;
      }
      goBtn.disabled = true;
      cancelBtn.disabled = true;
      pushAfterScan(done);
    });
    cancelBtn.addEventListener('click', () => {
      closePop();
      done(false);
    });
  }

  // push 入口：掃機密 → 有 findings 顯警告卡、否則直接續推
  function startPush(anchor) {
    if (pushing || !state.cwd) return;
    pushing = true;
    const finish = () => {
      pushing = false;
    };
    // 互動步驟需要 pop；若尚未開啟（header quick 觸發），先開起來
    if (!open) {
      open = true;
      anchorEl = anchor || qGrp || btn;
    }
    pop.innerHTML = '';
    pop.appendChild(el('div', 'hd', '推送前安全掃描'));
    pop.appendChild(el('div', 'git-hint', '掃描變更中…'));
    pop.classList.add('open');
    place();

    post('/vcs/scan-secrets', { cwd: state.cwd })
      .then((r) => {
        if (!pushing) return; // 期間被關掉
        const findings = (r && r.findings) || [];
        if (findings.length) {
          renderSecretWarning(findings, finish);
        } else {
          pushAfterScan(finish);
        }
      })
      .catch((e) => {
        // 掃描失敗不應擋住推送，但要讓使用者知道未掃成功
        toast('機密掃描失敗，已略過掃描：' + ((e && e.message) || e), false);
        pushAfterScan(finish);
      });
  }

  function renderPop(commitOpen) {
    pop.innerHTML = '';
    pop.appendChild(el('div', 'hd', '分支'));

    const filter = el('input');
    filter.type = 'text';
    filter.placeholder = '過濾分支或輸入新分支名稱…';
    pop.appendChild(filter);

    const refsBox = el('div', 'git-refs');
    pop.appendChild(refsBox);

    let refsCache = null; // null = loading
    function renderRefs() {
      const name = filter.value.trim();
      const q = name.toLowerCase();
      refsBox.innerHTML = '';
      if (refsCache === null) {
        refsBox.appendChild(el('div', 'git-hint', '載入中…'));
        return;
      }
      const shown = refsCache.filter(
        (rf) => !q || rf.name.toLowerCase().includes(q),
      );
      if (!shown.length && !name)
        refsBox.appendChild(el('div', 'git-hint', '沒有分支'));
      for (const rf of shown) {
        const it = el('div', 'it' + (rf.current ? ' on' : ''));
        it.appendChild(el('span', 'lb', '⎇ ' + rf.name));
        it.appendChild(el('span', 'fill'));
        if (rf.kind === 'remote') it.appendChild(el('span', 'sub', 'remote'));
        if (rf.current) it.appendChild(el('span', 'chk', '✓'));
        else
          it.addEventListener('click', () => {
            // remote ref: switch by short name (git guess-mode tracks it)
            const target =
              rf.kind === 'remote'
                ? rf.name.split('/').slice(1).join('/')
                : rf.name;
            doSwitch(target, false);
          });
        refsBox.appendChild(it);
      }
      if (name && !refsCache.some((rf) => rf.name === name)) {
        const c = el('div', 'it git-create');
        c.appendChild(el('span', 'lb', '＋ 建立新分支「' + name + '」'));
        c.addEventListener('click', () => doSwitch(name, true));
        refsBox.appendChild(c);
      }
    }
    filter.addEventListener('input', renderRefs);
    filter.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.isComposing) {
        const name = filter.value.trim();
        if (name && refsCache && !refsCache.some((rf) => rf.name === name))
          doSwitch(name, true);
      }
    });
    renderRefs();

    pop.appendChild(el('div', 'sep'));

    // --- commit ---
    const n = (cur && cur.files && cur.files.length) || 0;
    const cToggle = el('div', 'it');
    cToggle.appendChild(el('span', 'lb', 'Commit…'));
    cToggle.appendChild(el('span', 'fill'));
    cToggle.appendChild(el('span', 'sub', n ? `${n} 個變更` : '無變更'));
    pop.appendChild(cToggle);

    const cBox = el('div', 'git-commit');
    cBox.hidden = true;
    const ta = el('textarea');
    ta.placeholder = '提交訊息…';
    ta.rows = 3;
    const cRow = el('div', 'git-commit-row');
    const cBtn = el('button', 'btn primary', '全部暫存並提交');
    cBtn.disabled = true;
    cRow.appendChild(cBtn);
    cBox.append(ta, cRow);
    pop.appendChild(cBox);

    const doCommit = () => {
      const message = ta.value.trim();
      if (!message) return;
      run(
        () => post('/vcs/commit', { cwd: state.cwd, message, addAll: true }),
        (r) => '已提交 ' + (((r && r.sha) || '').slice(0, 7) || ''),
        '提交失敗',
      );
    };
    ta.addEventListener('input', () => {
      cBtn.disabled = !ta.value.trim();
    });
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) doCommit();
    });
    cBtn.addEventListener('click', doCommit);
    cToggle.addEventListener('click', () => {
      cBox.hidden = !cBox.hidden;
      if (!cBox.hidden) ta.focus();
      place();
    });

    pop.appendChild(el('div', 'sep'));

    // --- push / pull ---
    const acts = el('div', 'git-actions');
    const pushBtn = el(
      'button',
      'btn',
      'Push' + (cur && cur.ahead > 0 ? ' ↑' + cur.ahead : ''),
    );
    const pullBtn = el(
      'button',
      'btn',
      'Pull' + (cur && cur.behind > 0 ? ' ↓' + cur.behind : ''),
    );
    // Push 走機密掃描流程（startPush 會接管 pop 的內容渲染）；pop 已是開啟態
    pushBtn.addEventListener('click', () => startPush(anchorEl || btn));
    pullBtn.addEventListener('click', () =>
      run(() => post('/vcs/pull', { cwd: state.cwd }), '已拉取', '拉取失敗'),
    );
    acts.append(pushBtn, pullBtn);
    pop.appendChild(acts);

    place();

    // refs load after first paint so the pop opens instantly
    post('/vcs/refs', { cwd: state.cwd, limit: 200 })
      .then((r) => {
        refsCache = (r && r.refs) || [];
        renderRefs();
        place();
      })
      .catch(() => {
        refsCache = [];
        renderRefs();
      });
    if (commitOpen) {
      cBox.hidden = false;
      place();
      ta.focus();
    } else {
      filter.focus();
    }
  }

  function togglePop(anchor, commitOpen) {
    if (open) {
      closePop();
      return;
    }
    open = true;
    anchorEl = anchor;
    renderPop(!!commitOpen);
  }

  btn.addEventListener('click', () => togglePop(btn, false));
  if (qMenu) qMenu.addEventListener('click', () => togglePop(qGrp, false));
  if (qBtn)
    qBtn.addEventListener('click', () => {
      const act = qBtn.dataset.act;
      if (act === 'commit') {
        togglePop(qGrp, true);
        return;
      }
      // push／publish 都先掃機密；startPush 會自行把 pop 開起來渲染後續步驟
      startPush(qGrp || qBtn);
    });
}
