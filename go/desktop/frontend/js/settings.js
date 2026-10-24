// settings.js — ⚙ settings modal + header「在編輯器開啟專案」button.
// Modal sections: approval mode (radios → daemon.setApproval +
// localStorage default), model connection setup, editor
// picker + open-project action (/native/editors, /native/open-editor) and
// version info (/capabilities). Modal is built dynamically onto <body>.
import { daemon, bus, state } from './core.js?v=raven-execution-20261006a';
import { requestAccountLogin } from './commercial.js?v=desktop-send-20260904a';
import { showProductOnboarding } from './product_experience.js?v=welcome-benefits-20260928a';
import { getTheme, setTheme } from './theme.js';
import { buildModelConnections } from './model_connections.js?v=model-connections-20261003a';
import { buildRunLimitControl, buildEvoTokenBudgetControl } from './run_limits.js';

const MODE_KEY = 'meadow.approvalMode'; // default for future sessions
const EDITOR_KEY = 'meadow.editor'; // preferred editor app name
const SETTINGS_STYLES_HREF = '/css/settings_experience.css';
const DEVELOPER_DETAILS_KEY = 'meadow.settings.developer-details.v1';

const MODES = [
  { id: 'default', label: '確認每步' },
  { id: 'auto-edit', label: '自動編輯' },
  { id: 'yolo', label: '全自動' },
  { id: 'plan', label: '規劃' },
];

// same-origin helpers mirroring core.js request() for endpoints the
// daemon{} export does not wrap (/native/*, /capabilities).
async function request(method, path, body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' } };
  if (body !== undefined) opts.body = JSON.stringify(body);
  const res = await fetch(path, opts);
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
const post = (path, body) => request('POST', path, body);
const get = (path) => request('GET', path);

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

// First-time sessions can edit the selected workspace without interruption,
// while host command execution still requires confirmation. An explicit yolo
// choice remains sticky. Legacy values such as `plan` are no longer a visible
// choice and must not leave every radio empty.
const savedMode = lsGet(MODE_KEY);
let curMode = ['default', 'auto-edit', 'yolo'].includes(savedMode)
  ? savedMode
  : 'auto-edit';
if (savedMode !== curMode) lsSet(MODE_KEY, curMode);
let overlay = null; // settings modal overlay
let settingsReturnFocus = null;
let activeSettingsTab = 'general';
let latestCommercialUsage = null;
let disposeSettingsPanel = null;

const SETTINGS_TABS = Object.freeze([
  { id: 'general', label: '一般' },
  { id: 'agent', label: 'Agent 與權限' },
  { id: 'model', label: '設定模型' },
  { id: 'privacy', label: '隱私' },
  { id: 'account', label: '帳號與授權' },
  { id: 'advanced', label: '進階' },
]);

function ensureSettingsStylesheet() {
  if (!document.querySelector('link[data-provider-wizard-styles]')) {
    const wizard = document.createElement('link');
    wizard.rel = 'stylesheet';
    wizard.href = '/css/provider-wizard.css?v=provider-steps-20261011a';
    wizard.dataset.providerWizardStyles = 'true';
    document.head.insertBefore(wizard, document.querySelector('[data-mobile-dialog-styles]'));
  }
  if (document.querySelector('link[data-settings-experience-styles]')) return;
  const link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = SETTINGS_STYLES_HREF;
  link.dataset.settingsExperienceStyles = 'true';
  document.head.insertBefore(
    link,
    document.querySelector('[data-mobile-dialog-styles]'),
  );
}

// --- tiny toast (self-contained; same look-family as git toasts) ---
function toast(msg, ok) {
  let box = document.getElementById('qolToasts');
  if (!box) {
    box = document.createElement('div');
    box.id = 'qolToasts';
    document.body.appendChild(box);
  }
  const t = document.createElement('div');
  t.className = 'qol-toast' + (ok ? '' : ' bad');
  t.textContent = msg;
  t.title = msg;
  box.appendChild(t);
  setTimeout(() => t.remove(), ok ? 3200 : 6500);
}

// --- open-in-editor (shared by header button and modal button) ---
async function openInEditor(editorOverride) {
  if (!state.cwd) {
    toast('尚未選擇專案資料夾', false);
    return;
  }
  const editor = editorOverride || lsGet(EDITOR_KEY) || undefined;
  try {
    // JSON.stringify drops undefined → daemon auto-detects the editor
    const r = await post('/native/open-editor', { path: state.cwd, editor });
    toast('已在 ' + ((r && r.editor) || '編輯器') + ' 開啟專案', true);
  } catch (e) {
    toast('開啟失敗：' + ((e && e.message) || e), false);
  }
}

// --- modal ---
function closeModal({ restoreFocus = true } = {}) {
  disposeSettingsPanel?.();
  disposeSettingsPanel = null;
  const wasOpen = Boolean(overlay);
  const returnTarget = settingsReturnFocus;
  if (overlay) {
    overlay.remove();
    overlay = null;
  }
  if (
    wasOpen &&
    restoreFocus &&
    returnTarget instanceof HTMLElement &&
    returnTarget.isConnected
  ) {
    returnTarget.focus();
  }
  if (wasOpen) settingsReturnFocus = null;
}

function syncRadios() {
  if (!overlay) return;
  for (const input of overlay.querySelectorAll('input[name="approvalMode"]'))
    input.checked = input.value === curMode;
}

function secLabel(text) {
  const el = document.createElement('div');
  el.className = 'set-label';
  el.textContent = text;
  return el;
}

// --- usage formatting (mirrors sidebar.js fmtTokens; kept local so settings
// stays import-free beyond core.js) ---
function fmtTokens(n) {
  const v = Number(n) || 0;
  if (v < 1000) return String(v);
  if (v < 1e6)
    return (v / 1e3).toFixed(v < 1e4 ? 1 : 0).replace(/\.0$/, '') + 'k';
  return (v / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
}
// elapsedMs → "Xh Ym" / "Ym Zs" / "Zs" (human-readable cumulative time).
function fmtDuration(ms) {
  const s = Math.round((Number(ms) || 0) / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}
const lastSeg = (p) => (p || '').split('/').filter(Boolean).pop() || p || '';

// --- 用量 section: lifetime token/time totals + per-project table ---
// Pulls GET /usage (unscoped → {projects, totals}); the daemon tallies per
// cwd across all threads and persists to ~/.meadow/usage.json.
function buildUsageSection(body) {
  body.appendChild(secLabel('用量'));
  const box = document.createElement('div');
  box.className = 'set-sec set-usage';
  box.textContent = '載入中…';
  body.appendChild(box);

  get('/usage')
    .then((u) => {
      if (!overlay) return;
      const totals = (u && u.totals) || {};
      const projects = ((u && u.projects) || [])
        .filter((p) => p && (p.totalTokens || p.turns))
        .sort(
          (a, b) => (Number(b.totalTokens) || 0) - (Number(a.totalTokens) || 0),
        );
      box.textContent = '';

      // Totals: big numbers row + token breakdown + turns + time.
      const kv = document.createElement('div');
      kv.className = 'set-kv';
      const tot = document.createElement('div');
      tot.innerHTML = '總 token：<b></b>';
      tot.querySelector('b').textContent = fmtTokens(totals.totalTokens);
      const split = document.createElement('div');
      split.className = 'usage-split';
      split.textContent = `輸入 ${fmtTokens(totals.promptTokens)} · 輸出 ${fmtTokens(
        totals.completionTokens,
      )}`;
      const turns = document.createElement('div');
      turns.innerHTML = '對話輪數：<b></b>';
      turns.querySelector('b').textContent = String(Number(totals.turns) || 0);
      const time = document.createElement('div');
      time.innerHTML = '累計時間：<b></b>';
      time.querySelector('b').textContent = fmtDuration(totals.elapsedMs);
      kv.append(tot, split, turns, time);
      box.appendChild(kv);

      if (!projects.length) {
        const empty = document.createElement('div');
        empty.className = 'usage-empty';
        empty.textContent = '尚無使用紀錄';
        box.appendChild(empty);
        return;
      }

      // Per-project table: 專案 / token / 輪數.
      const tbl = document.createElement('table');
      tbl.className = 'usage-tbl';
      const thead = document.createElement('thead');
      thead.innerHTML = '<tr><th>專案</th><th>token</th><th>輪數</th></tr>';
      tbl.appendChild(thead);
      const tbody = document.createElement('tbody');
      for (const p of projects) {
        const tr = document.createElement('tr');
        const tdName = document.createElement('td');
        tdName.className = 'u-name';
        tdName.textContent = lastSeg(p.cwd);
        tdName.title = p.cwd;
        const tdTok = document.createElement('td');
        tdTok.className = 'u-num';
        tdTok.textContent = fmtTokens(p.totalTokens);
        const tdTurns = document.createElement('td');
        tdTurns.className = 'u-num';
        tdTurns.textContent = String(Number(p.turns) || 0);
        tr.append(tdName, tdTok, tdTurns);
        tbody.appendChild(tr);
      }
      tbl.appendChild(tbody);
      box.appendChild(tbl);
    })
    .catch(() => {
      if (overlay) box.textContent = '無法取得用量統計（daemon 離線？）';
    });
}

const UPDATE_ERROR_TEXT = {
  busy: '另一個更新操作仍在進行中',
  feed_unavailable: '目前無法取得更新資訊',
  verification_failed: '更新驗證失敗',
  staged_update_not_found: '找不到已驗證的更新，請重新下載',
  install_not_ready: '更新尚未準備完成',
};

function safeUpdateError(error) {
  if (error && UPDATE_ERROR_TEXT[error.code])
    return UPDATE_ERROR_TEXT[error.code];
  if (
    error &&
    (error.status === 502 || error.status === 503 || error.status === 504)
  )
    return '更新服務目前無法使用';
  return '更新操作失敗';
}

function initialUpdateUnavailableMessage(error) {
  if (error?.status === 404) return '此版本未提供自動更新';
  if (error?.status === 401) return '登入後可檢查更新';
  return '';
}

function buildUpdateSection(body) {
  body.appendChild(secLabel('產品更新'));
  const box = document.createElement('div');
  box.className = 'set-sec set-update';
  const status = document.createElement('div');
  status.id = 'updateState';
  status.className = 'update-state';
  status.textContent = '正在取得更新狀態…';
  const detail = document.createElement('div');
  detail.id = 'updateDetail';
  detail.className = 'update-detail';
  const note = document.createElement('div');
  note.className = 'update-note';
  note.textContent = '安裝只會在目前任務完成並提交穩定檢查點後進行。';
  const actions = document.createElement('div');
  actions.className = 'update-actions';
  const checkButton = document.createElement('button');
  checkButton.id = 'updateCheck';
  checkButton.className = 'btn';
  checkButton.textContent = '檢查更新';
  const stageButton = document.createElement('button');
  stageButton.id = 'updateStage';
  stageButton.className = 'btn';
  stageButton.textContent = '下載並驗證';
  const installButton = document.createElement('button');
  installButton.id = 'updateInstall';
  installButton.className = 'btn primary';
  installButton.textContent = '安裝更新';
  actions.append(checkButton, stageButton, installButton);
  box.append(status, detail, note, actions);
  body.appendChild(box);

  let current = null;
  let pending = false;
  const versionText = (item) => {
    if (!item || !item.version) return '';
    const build = Number(item.buildNumber);
    return build > 0 ? `${item.version}（build ${build}）` : item.version;
  };
  const render = (next) => {
    current = next || { state: 'idle', busy: false };
    const check = current.check || null;
    const stage = current.stage || null;
    const install = current.install || null;
    const isBusy = pending || !!current.busy;
    detail.textContent = versionText(stage || check);
    switch (current.state) {
      case 'checking':
        status.textContent = '正在檢查更新';
        break;
      case 'available':
        status.textContent = '有可用更新';
        break;
      case 'staging':
        status.textContent = '正在下載並驗證更新';
        break;
      case 'staged':
        status.textContent = '已下載並驗證，等待安裝';
        break;
      case 'installing':
        status.textContent = '正在安裝更新';
        break;
      case 'handoff':
        status.textContent = '安裝已交接，請重新啟動 Meadow';
        break;
      case 'completed':
        status.textContent = '更新完成';
        break;
      case 'failed':
        status.textContent = UPDATE_ERROR_TEXT[current.errorCode] || '更新失敗';
        break;
      default:
        status.textContent = '尚未檢查更新';
    }
    checkButton.disabled = isBusy;
    stageButton.hidden = current.state !== 'available';
    stageButton.disabled = isBusy;
    installButton.hidden = current.state !== 'staged';
    installButton.disabled = isBusy || !!state.busy;
    if (install && install.handoffReference && current.state !== 'completed')
      detail.textContent = '更新已交接系統安裝器，請依提示完成。';
  };
  const perform = async (operation) => {
    pending = true;
    render(current);
    let succeeded = false;
    try {
      await operation();
      succeeded = true;
    } catch (error) {
      status.textContent = safeUpdateError(error);
      detail.textContent = '';
    } finally {
      pending = false;
      if (succeeded && current) {
        render(current);
      } else {
        checkButton.disabled = false;
        stageButton.disabled = false;
        installButton.disabled = !!state.busy;
      }
    }
  };
  checkButton.addEventListener('click', () =>
    perform(async () => {
      const check = await daemon.updateCheck();
      render({
        state: check && check.stagedId ? 'staged' : 'available',
        busy: false,
        check,
      });
    }),
  );
  stageButton.addEventListener('click', () =>
    perform(async () => {
      render({ ...(current || {}), state: 'staging', busy: true });
      const stage = await daemon.updateStage();
      render({
        state: 'staged',
        busy: false,
        check: current && current.check,
        stage,
      });
    }),
  );
  installButton.addEventListener('click', () =>
    perform(async () => {
      if (state.busy) throw Object.assign(new Error('busy'), { code: 'busy' });
      const stagedId = current && current.stage && current.stage.stagedId;
      if (!stagedId)
        throw Object.assign(new Error('missing stage'), {
          code: 'staged_update_not_found',
        });
      render({ ...current, state: 'installing', busy: true });
      const install = await daemon.updateInstall(stagedId);
      render({
        ...current,
        // A successful HTTP 202 means the signed update was handed to the
        // system installer. `accepted:false` is the canonical current
        // response and must not be presented as a failure.
        state: 'handoff',
        busy: false,
        install,
      });
    }),
  );
  daemon
    .updateStatus()
    .then(render)
    .catch((error) => {
      status.textContent =
        initialUpdateUnavailableMessage(error) || safeUpdateError(error);
      detail.textContent = '';
      checkButton.hidden = true;
      checkButton.disabled = true;
      stageButton.hidden = true;
      stageButton.disabled = true;
      installButton.hidden = true;
      installButton.disabled = true;
    });
  bus.on('run_state', () => current && render(current));
}

function panelIntro(body, kicker, title, copy) {
  const eyebrow = document.createElement('p');
  eyebrow.className = 'settings-kicker';
  eyebrow.textContent = kicker;
  const heading = document.createElement('h2');
  heading.id = `settingsPanelTitle-${activeSettingsTab}`;
  heading.textContent = title;
  const intro = document.createElement('p');
  intro.className = 'settings-intro';
  intro.textContent = copy;
  body.append(eyebrow, heading, intro);
}

function settingRow(body, title, description, control, { tone = '' } = {}) {
  const row = document.createElement('section');
  row.className = 'settings-group';
  const copy = document.createElement('header');
  copy.className = 'settings-group-copy';
  const strong = document.createElement('strong');
  strong.textContent = title;
  const detail = document.createElement('span');
  detail.textContent = description;
  if (tone) detail.dataset.tone = tone;
  copy.append(strong, detail);
  row.append(copy);
  if (control) row.append(control);
  body.appendChild(row);
  return { row, detail };
}

function settingButton(
  label,
  { id = '', className = '', disabled = false } = {},
) {
  const button = document.createElement('button');
  button.type = 'button';
  button.textContent = label;
  button.disabled = disabled;
  if (id) button.id = id;
  if (className) button.className = className;
  return button;
}

function fixedBadge(label, stateName = '') {
  const badge = document.createElement('span');
  badge.className = stateName ? 'settings-state-badge' : 'settings-fixed-badge';
  badge.textContent = label;
  if (stateName) badge.dataset.state = stateName;
  return badge;
}

function settingSwitch({ checked, label, disabled = false, onChange = null }) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'settings-switch';
  button.setAttribute('role', 'switch');
  button.setAttribute('aria-label', label);
  button.setAttribute('aria-checked', String(Boolean(checked)));
  button.disabled = disabled;
  if (typeof onChange === 'function') {
    button.addEventListener('click', () => {
      const next = button.getAttribute('aria-checked') !== 'true';
      button.setAttribute('aria-checked', String(next));
      onChange(next);
    });
  }
  return button;
}

function settingControl(...nodes) {
  const control = document.createElement('div');
  control.className = 'settings-group-control';
  control.append(...nodes.filter(Boolean));
  return control;
}

function currentWorkspaceLabel() {
  return lastSeg(state.cwd) || '尚未選擇工作區';
}

function buildEditorControl() {
  const select = document.createElement('select');
  select.setAttribute('aria-label', '外部編輯器');
  select.disabled = true;
  const loading = document.createElement('option');
  loading.textContent = '偵測中…';
  select.appendChild(loading);
  const openButton = settingButton('開啟專案', { className: 'primary' });
  openButton.disabled = true;
  openButton.addEventListener('click', () =>
    openInEditor(select.disabled ? '' : select.value),
  );
  get('/native/editors')
    .then((result) => {
      if (!overlay || !select.isConnected) return;
      const editors = Array.isArray(result && result.editors)
        ? result.editors.filter((name) => typeof name === 'string' && name)
        : [];
      select.textContent = '';
      if (!editors.length) {
        const none = document.createElement('option');
        none.textContent = '未偵測到編輯器';
        select.appendChild(none);
        openButton.disabled = true;
        return;
      }
      const stored = lsGet(EDITOR_KEY);
      for (const name of editors) {
        const option = document.createElement('option');
        option.value = name;
        option.textContent = name;
        select.appendChild(option);
      }
      select.value = editors.includes(stored) ? stored : editors[0];
      select.disabled = false;
      openButton.disabled = false;
      select.addEventListener('change', () => lsSet(EDITOR_KEY, select.value));
    })
    .catch(() => {
      if (!overlay || !select.isConnected) return;
      select.textContent = '';
      const failed = document.createElement('option');
      failed.textContent = '偵測失敗';
      select.appendChild(failed);
      openButton.disabled = true;
    });
  return settingControl(select, openButton);
}

async function replayOnboarding() {
  const returnFocus = settingsReturnFocus;
  closeModal({ restoreFocus: false });
  const result = await showProductOnboarding({ force: true, returnFocus });
  if (result === 'login') await requestAccountLogin();
}

function buildGeneralPanel(body) {
  panelIntro(
    body,
    'GENERAL',
    '一般',
    '只保留會改變日常體驗的設定；內部協定與診斷資訊集中在「進階」。',
  );
  const themeSelect = document.createElement('select');
  themeSelect.id = 'desktopThemeSelect';
  themeSelect.setAttribute('aria-label', '介面主題');
  for (const [value, label] of [['dark', '暗色 · 黑灰綠'], ['light', '亮色 · 米白與黃綠']]) {
    const option = document.createElement('option');
    option.value = value;
    option.textContent = label;
    themeSelect.appendChild(option);
  }
  themeSelect.value = getTheme();
  themeSelect.addEventListener('change', () => setTheme(themeSelect.value));
  settingRow(body, '介面主題', '套用到整個 App、四頁 ASCII 介紹與終端機；會記住你的選擇。', themeSelect);
  settingRow(
    body,
    '語言',
    '介面與模型預設回覆固定使用繁體中文（台灣）。',
    fixedBadge('繁體中文（台灣）'),
  );
  settingRow(
    body,
    '工作區',
    '新對話使用目前側邊欄選定的專案。',
    fixedBadge(currentWorkspaceLabel()),
  );
  settingRow(
    body,
    '外部編輯器',
    '從 Meadow 開啟目前專案時使用。',
    buildEditorControl(),
  );
  settingRow(
    body,
    '右側即時顯示程式',
    '寫入草稿與最終結果只顯示目前 session；尚未提供開關偏好。',
    settingControl(
      settingSwitch({
        checked: true,
        label: '右側即時顯示程式，固定啟用',
        disabled: true,
      }),
      fixedBadge('session 範圍'),
    ),
  );
  settingRow(
    body,
    '只使用右側 Meadow Browser',
    '網頁與成果預覽預設在右側開啟。登入與購買會開啟瀏覽器網站。',
    settingControl(
      settingSwitch({
        checked: true,
        label: '只使用右側 Meadow Browser，預設啟用',
        disabled: true,
      }),
      fixedBadge('預設開啟'),
    ),
    { tone: 'warn' },
  );
  const tourButton = settingButton('重新播放特色介紹');
  tourButton.addEventListener('click', replayOnboarding);
  settingRow(
    body,
    '首次啟動介紹',
    '重新查看本機 Agent、檔案串流、驗證與資料邊界。',
    tourButton,
  );
  buildUpdateSection(body);
}

const MODE_DESCRIPTIONS = Object.freeze({
  default: '每一步需要確認',
  'auto-edit': '可自動修改目前工作區；本機命令仍需確認',
  yolo: '全自動；高風險操作仍會確認',
  plan: '只規劃，不直接修改檔案',
});

function buildAgentPanel(body) {
  panelIntro(
    body,
    'AGENT & PERMISSIONS',
    'Agent 與權限',
    '用人能理解的層級管理每次任務能做什麼；破壞性與外部影響操作仍有獨立安全邊界。',
  );
  const modeHeading = document.createElement('div');
  modeHeading.className = 'settings-group-copy';
  const title = document.createElement('strong');
  title.textContent = '預設執行模式';
  const description = document.createElement('span');
  description.textContent = '套用至未來任務；目前對話會同步更新。';
  modeHeading.append(title, description);
  body.appendChild(modeHeading);
  const modeGrid = document.createElement('div');
  modeGrid.className = 'settings-mode-grid';
  for (const mode of MODES.filter((entry) => entry.id !== 'plan')) {
    const option = document.createElement('label');
    option.className = 'settings-mode-option';
    const input = document.createElement('input');
    input.type = 'radio';
    input.name = 'approvalMode';
    input.value = mode.id;
    input.checked = mode.id === curMode;
    const copy = document.createElement('span');
    copy.textContent = mode.label;
    const small = document.createElement('small');
    small.textContent = MODE_DESCRIPTIONS[mode.id] || mode.id;
    const text = document.createElement('span');
    text.append(copy, small);
    option.append(input, text);
    input.addEventListener('change', async () => {
      const previous = curMode;
      if (!state.sessionId) {
        curMode = mode.id;
        lsSet(MODE_KEY, mode.id);
        return;
      }
      try {
        await daemon.setApproval(state.sessionId, mode.id);
        curMode = mode.id;
        lsSet(MODE_KEY, mode.id);
      } catch (error) {
        curMode = previous;
        syncRadios();
        toast(
          '切換核准模式失敗：' + ((error && error.message) || error),
          false,
        );
      }
    });
    modeGrid.appendChild(option);
  }
  body.appendChild(modeGrid);
  settingRow(
    body,
    '修改工作區檔案',
    '只限目前選定的專案範圍；離開工作區需重新授權。',
    fixedBadge('依執行模式'),
  );
  settingRow(
    body,
    '執行本機命令',
    '刪除、覆寫與外部傳送仍由 Core 的高風險邊界確認。',
    fixedBadge('安全邊界啟用'),
  );
  settingRow(
    body,
    '網站存取',
    'Web 搜尋與頁面擷取會在工作紀錄明確標示連網。',
    fixedBadge('執行時標示'),
  );
}

function modelStatusLabel(status) {
  const value = String((status && status.status) || '').toLowerCase();
  if (value === 'ready') return ['已載入', 'ready'];
  if (['loading', 'activating', 'starting'].includes(value))
    return ['正在載入', ''];
  if (['unloaded', 'idle', 'stopped', ''].includes(value))
    return ['未載入', ''];
  if (['failed', 'error'].includes(value)) return ['載入失敗', 'error'];
  return [value || '未知', ''];
}

function buildModelPanel(body) {
  panelIntro(
    body,
    'MODEL CONNECTIONS',
    '設定模型',
    '選擇供應商並連接 API，完成後即可在對話中選模型。',
  );
  return buildModelConnections(body);
}

function buildPrivacyPanel(body) {
  panelIntro(
    body,
    'PRIVACY',
    '隱私',
    '把留在本機與需要連網的行為拆開說清楚；尚未有安全操作端點的項目不會顯示假成功。',
  );
  settingRow(
    body,
    '本機資料',
    '專案、對話、附件、模型輸出與歷史紀錄留在這台 Mac。',
    settingButton('管理功能尚未接通', { disabled: true }),
  );
  settingRow(
    body,
    '連網資料',
    'Email、授權驗證、更新，以及你主動使用的 Web 搜尋。',
    fixedBadge('需要時才連線'),
  );
  settingRow(
    body,
    '錯誤診斷分享',
    '目前沒有自動上傳診斷資料的產品端點。',
    settingControl(
      settingSwitch({
        checked: false,
        label: '錯誤診斷分享，未啟用',
        disabled: true,
      }),
      fixedBadge('未啟用'),
    ),
  );
  settingRow(
    body,
    '清除本機紀錄',
    '安全的分類清除與復原流程尚未接通，避免誤刪專案與對話。',
    settingButton('尚未提供', { disabled: true }),
    { tone: 'warn' },
  );
  const note = document.createElement('p');
  note.className = 'settings-inline-note';
  note.textContent =
    '登出只解除帳號與這台 Mac 的授權，不會刪除本機對話、專案或產出。';
  body.appendChild(note);
}

function invokeCommercial(action) {
  const api = window.MeadowCommercial;
  const method = {
    login: 'openLogin',
    account: 'openAccount',
    purchase: 'openPurchase',
    serial: 'openSerial',
    logout: 'openLogout',
  }[action];
  if (api && method && typeof api[method] === 'function') {
    closeModal({ restoreFocus: false });
    void api[method]();
    return true;
  }
  if (
    api &&
    ['purchase', 'serial'].includes(action) &&
    typeof api.showUpgrade === 'function'
  ) {
    closeModal({ restoreFocus: false });
    api.showUpgrade('manual');
    return true;
  }
  toast('這項帳號操作尚未接入 Desktop', false);
  return false;
}

function accountSummary() {
  const snapshot = window.MeadowCommercial?.snapshot?.() || null;
  const authenticated =
    snapshot?.state === 'authorized_general' ||
    snapshot?.state === 'authorized_pro';
  // The usage event can arrive before Settings is initialised. Read the
  // authoritative cached projection as a fallback so opening Settings later
  // never regresses a Pro account to the raw `pro` transport value.
  const access =
    latestCommercialUsage?.access ||
    window.MeadowCommercial?.membershipSnapshot?.()?.access;
  const entitlementPro = snapshot?.plan === 'pro';
  const contradictory =
    snapshot?.projectionConflict === true ||
    (authenticated &&
      ((entitlementPro && access?.kind === 'free_account') ||
        (!entitlementPro && access?.kind === 'lifetime')));
  let label = authenticated ? snapshot.plan : '需要登入';
  if (contradictory) label = '授權狀態需重新整理';
  else if (access?.kind === 'lifetime') label = 'Pro · 永久授權 · ∞';
  else if (access?.kind === 'free_account') {
    const remaining = (latestCommercialUsage || window.MeadowCommercial?.membershipSnapshot?.())?.usage?.remaining;
    label = Number.isSafeInteger(remaining)
      ? `免費帳號 · 剩餘 ${remaining} / 100 次`
      : '免費帳號 · 正在讀取用量';
  }
  return {
    snapshot,
    authenticated,
    contradictory,
    isPro: entitlementPro && !contradictory,
    label,
  };
}

function buildAccountPanel(body) {
  panelIntro(
    body,
    'ACCOUNT & LICENSE',
    '帳號與授權',
    '查看免費次數、Pro 授權與已連接裝置。',
  );
  const summary = accountSummary();
  const primary = summary.authenticated
    ? settingButton(summary.contradictory ? '檢查授權' : '管理帳號')
    : settingButton('登入 Meadow', { className: 'primary' });
  primary.addEventListener('click', () =>
    invokeCommercial(summary.authenticated ? 'account' : 'login'),
  );
  settingRow(body, '目前方案', summary.label, primary);
  const isPro = summary.isPro;
  const purchase = settingButton(isPro ? '查看授權' : '查看 Pro');
  purchase.disabled = summary.contradictory;
  purchase.addEventListener('click', () =>
    invokeCommercial(isPro ? 'account' : 'purchase'),
  );
  settingRow(
    body,
    'Meadow Code Pro',
    isPro
      ? '永久授權已由 agentd 投影確認。'
      : '免費可送出 100 次；一次支付 NT$100 解鎖 Pro，無限使用 Desktop。模型 API 費用由供應商另計。',
    purchase,
  );
  const serial = settingButton('輸入序號');
  serial.disabled =
    summary.contradictory ||
    (typeof window.MeadowCommercial?.openSerial !== 'function' &&
      typeof window.MeadowCommercial?.showUpgrade !== 'function');
  serial.addEventListener('click', () => invokeCommercial('serial'));
  settingRow(
    body,
    '購買序號',
    serial.disabled
      ? '序號入口尚未接入目前 Desktop。'
      : '驗證購買序號並由 agentd 刷新授權狀態。',
    serial,
    { tone: serial.disabled ? 'warn' : '' },
  );
  const logout = settingButton('登出這台 Mac', { className: 'danger' });
  logout.disabled =
    !summary.authenticated ||
    typeof window.MeadowCommercial?.openLogout !== 'function';
  logout.addEventListener('click', () => invokeCommercial('logout'));
  settingRow(
    body,
    '登出',
    !summary.authenticated
      ? '目前尚未登入。'
      : logout.disabled
        ? '登出確認入口尚未接入；不會直接解除授權。'
        : '保留本機對話與檔案，只解除這台 Mac 的帳號。',
    logout,
    { tone: summary.authenticated && logout.disabled ? 'warn' : '' },
  );
}

function buildAdvancedPanel(body) {
  panelIntro(
    body,
    'ADVANCED',
    '進階',
    '執行上限、診斷與開發資訊集中在這裡。',
  );
  const runLimits = buildRunLimitControl();
  const { detail: runLimitDescription } = settingRow(
    body,
    '每次任務的時間上限',
    '僅套用到新送出與新排入的任務。時間到會取消，已存檔案會保留。Token 額度在輸入框的 Effort 選單調整：強度越高，額度越多；關閉 Thinking 也保留所選額度。',
    runLimits,
  );
  runLimitDescription.id = 'settingsRunLimitDescription';
  const runLimitAccounting = document.createElement('p');
  runLimitAccounting.id = 'settingsRunLimitAccounting';
  runLimitAccounting.className = 'settings-diagnostic-output';
  runLimitAccounting.textContent = '需要立即停止時，請按輸入框旁的紅色停止鍵。';
  runLimits.setAttribute('aria-describedby', `${runLimitDescription.id} ${runLimitAccounting.id}`);
  body.appendChild(runLimitAccounting);
  const evoTokens = buildEvoTokenBudgetControl();
  const { detail: evoTokenDescription } = settingRow(
    body,
    'Evo 每次任務的 Token 上限',
    '僅適用於 Evo。沿用 Effort，或另外設定整個流程累計的輸入與輸出額度；重讀上下文也會計入。只套用到新送出與新排入的任務，不延長執行中的任務。',
    evoTokens,
  );
  evoTokenDescription.id = 'settingsEvoTokenDescription';
  evoTokens.setAttribute('aria-describedby', evoTokenDescription.id);
  const health = settingButton('執行健康檢查');
  const healthOutput = document.createElement('p');
  healthOutput.id = 'settingsHealthOutput';
  healthOutput.className = 'settings-diagnostic-output';
  health.addEventListener('click', async () => {
    health.disabled = true;
    healthOutput.textContent =
      '正在檢查 Desktop、模型 runtime 與右側 Browser 模組。';
    try {
      const [modelResult, protocolResult] = await Promise.allSettled([
        daemon.modelStatus(),
        get('/capabilities'),
      ]);
      const model =
        modelResult.status === 'fulfilled'
          ? modelStatusLabel(modelResult.value)[0]
          : '無法連線';
      const protocol =
        protocolResult.status === 'fulfilled'
          ? protocolResult.value?.protocolVersions?.current || '未回報'
          : '無法連線';
      const browser = document.getElementById('rpPagePreview')
        ? '右側介面模組存在（尚未驗證執行能力）'
        : '右側介面模組未建立';
      healthOutput.textContent = `Desktop 協定：${protocol} · 35B：${model} · Meadow Browser：${browser}`;
    } finally {
      health.disabled = false;
    }
  });
  settingRow(
    body,
    '服務診斷',
    '只讀取 Core、Desktop、模型後端與 Browser 狀態。',
    health,
  );
  body.appendChild(healthOutput);
  const developerDetails = lsGet(DEVELOPER_DETAILS_KEY) === 'on';
  const developerSwitch = settingSwitch({
    checked: developerDetails,
    label: '開發者詳細資訊',
    onChange: (enabled) => {
      lsSet(DEVELOPER_DETAILS_KEY, enabled ? 'on' : 'off');
      const details = body.querySelector('#settingsDeveloperDetails');
      const diagnostics = body.querySelector('#settingsDeveloperDiagnostics');
      if (details) details.hidden = !enabled;
      if (diagnostics) diagnostics.hidden = !enabled;
    },
  });
  settingRow(
    body,
    '開發者詳細資訊',
    '顯示本機 token 用量、協定版本與目前工作階段識別。',
    developerSwitch,
  );
  const details = document.createElement('div');
  details.id = 'settingsDeveloperDetails';
  details.hidden = !developerDetails;
  details.className = 'settings-about-grid';
  const session = document.createElement('div');
  session.innerHTML = '<span>目前工作階段</span><strong></strong>';
  session.querySelector('strong').textContent = state.sessionId || '尚未建立';
  const workspace = document.createElement('div');
  workspace.innerHTML = '<span>工作區</span><strong></strong>';
  workspace.querySelector('strong').textContent = state.cwd || '尚未選擇';
  const shell = document.createElement('div');
  shell.innerHTML = '<span>Desktop</span><strong>v0.1 (0.1)</strong>';
  const protocol = document.createElement('div');
  protocol.innerHTML = '<span>應用協定</span><strong>載入中…</strong>';
  details.append(session, workspace, shell, protocol);
  body.appendChild(details);
  const developerDiagnostics = document.createElement('div');
  developerDiagnostics.id = 'settingsDeveloperDiagnostics';
  developerDiagnostics.hidden = !developerDetails;
  buildUsageSection(developerDiagnostics);
  body.appendChild(developerDiagnostics);
  get('/capabilities')
    .then((capabilities) => {
      if (!overlay || !protocol.isConnected) return;
      protocol.querySelector('strong').textContent =
        `v${capabilities?.protocolVersions?.current || '未知'}`;
    })
    .catch(() => {
      if (protocol.isConnected)
        protocol.querySelector('strong').textContent = '無法取得';
    });
  const replay = settingButton('重新播放特色介紹');
  replay.addEventListener('click', replayOnboarding);
  settingRow(
    body,
    '產品導覽',
    '立即重新開啟四頁特色介紹，不會改變帳號或授權。',
    replay,
  );
}

const PANEL_BUILDERS = Object.freeze({
  general: buildGeneralPanel,
  agent: buildAgentPanel,
  model: buildModelPanel,
  privacy: buildPrivacyPanel,
  account: buildAccountPanel,
  advanced: buildAdvancedPanel,
});

function openModal(tab = 'general') {
  closeModal({ restoreFocus: false });
  ensureSettingsStylesheet();
  activeSettingsTab = PANEL_BUILDERS[tab] ? tab : 'general';
  const active = document.activeElement;
  const accountRow = document.getElementById('commercialAccountRow');
  const settingsButton = document.getElementById('btnSettings');
  const visibleFallback = [accountRow, settingsButton].find(
    (node) => node instanceof HTMLElement && node.getClientRects().length > 0,
  );
  settingsReturnFocus =
    active instanceof HTMLElement &&
    active.isConnected &&
    active !== document.body
      ? active
      : visibleFallback || settingsButton;

  overlay = document.createElement('div');
  overlay.className = 'qol-overlay settings-experience-overlay';
  const modal = document.createElement('div');
  modal.className = 'set-modal settings-experience-modal';
  modal.setAttribute('role', 'dialog');
  modal.setAttribute('aria-modal', 'true');
  modal.setAttribute('aria-labelledby', 'settingsModalTitle');

  const head = document.createElement('div');
  head.className = 'qol-head';
  const title = document.createElement('span');
  title.className = 'qol-title';
  title.id = 'settingsModalTitle';
  title.textContent = '設定';
  const close = document.createElement('button');
  close.className = 'qol-x';
  close.type = 'button';
  close.textContent = '×';
  close.title = '關閉';
  close.setAttribute('aria-label', '關閉設定');
  close.addEventListener('click', () => closeModal());
  head.append(title, close);

  const layout = document.createElement('div');
  layout.className = 'settings-experience-layout';
  const nav = document.createElement('nav');
  nav.className = 'settings-experience-nav';
  nav.setAttribute('aria-label', '設定分類');
  const content = document.createElement('div');
  content.id = 'settingsExperienceContent';
  content.className = 'settings-experience-content';
  content.tabIndex = -1;
  layout.append(nav, content);

  const renderTab = (tab) => {
    const next = PANEL_BUILDERS[tab] ? tab : 'general';
    disposeSettingsPanel?.();
    disposeSettingsPanel = null;
    activeSettingsTab = next;
    content.textContent = '';
    const panel = document.createElement('div');
    panel.className = 'settings-panel';
    panel.dataset.settingsPanel = next;
    content.appendChild(panel);
    disposeSettingsPanel = PANEL_BUILDERS[next](panel) || null;
    for (const button of nav.querySelectorAll('[data-settings-tab]')) {
      const selected = button.dataset.settingsTab === next;
      button.classList.toggle('active', selected);
      button.setAttribute('aria-current', selected ? 'page' : 'false');
    }
    content.scrollTop = 0;
  };

  for (const tab of SETTINGS_TABS) {
    const button = document.createElement('button');
    button.type = 'button';
    button.dataset.settingsTab = tab.id;
    button.textContent = tab.label;
    button.setAttribute('aria-controls', 'settingsExperienceContent');
    button.addEventListener('click', () => renderTab(tab.id));
    nav.appendChild(button);
  }
  nav.addEventListener('keydown', (event) => {
    if (
      !['ArrowDown', 'ArrowUp', 'ArrowRight', 'ArrowLeft'].includes(event.key)
    )
      return;
    const buttons = [...nav.querySelectorAll('[data-settings-tab]')];
    const index = buttons.indexOf(document.activeElement);
    if (index < 0) return;
    event.preventDefault();
    const forward = event.key === 'ArrowDown' || event.key === 'ArrowRight';
    const next =
      buttons[(index + (forward ? 1 : -1) + buttons.length) % buttons.length];
    next.focus();
    renderTab(next.dataset.settingsTab);
  });

  modal.append(head, layout);
  overlay.appendChild(modal);
  overlay.addEventListener('click', (event) => {
    if (event.target === overlay) closeModal();
  });
  overlay.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      closeModal();
      return;
    }
    if (event.key !== 'Tab') return;
    const focusable = [
      ...modal.querySelectorAll(
        'button:not([disabled]), input:not([disabled]), select:not([disabled]), [href], [tabindex]:not([tabindex="-1"])',
      ),
    ].filter((node) => node.getClientRects().length > 0);
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  });
  document.body.appendChild(overlay);
  renderTab(activeSettingsTab);
  close.focus();
}

export function initSettings() {
  bus.on('settings:open-model', () => openModal('model'));
  const btn = document.getElementById('btnSettings');
  if (btn)
    btn.addEventListener('click', () => (overlay ? closeModal() : openModal()));

  const edBtn = document.getElementById('btnEditor');
  if (edBtn) edBtn.addEventListener('click', () => openInEditor());

  // stay in sync with switches made elsewhere (composer mode pill)
  bus.on('approval_mode_changed', (d) => {
    const m = d && (d.mode || d.approvalMode);
    if (typeof m === 'string' && m) {
      curMode = m;
      syncRadios();
    }
  });

  // commercial.js validates the membership response before emitting this
  // event. Settings consumes only that sanitized projection and never calls
  // the membership endpoint or reconstructs an unvalidated account object.
  window.addEventListener('commercial:usage', (event) => {
    const usage = event?.detail;
    latestCommercialUsage = usage?.access ? usage : null;
    const accountPanel = overlay?.querySelector(
      '[data-settings-panel="account"]',
    );
    if (accountPanel) {
      accountPanel.textContent = '';
      buildAccountPanel(accountPanel);
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') closeModal();
  });
}
