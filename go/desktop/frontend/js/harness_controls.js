// Execution selection is separate from model inference preferences. The server
// owns defaults, admission and queued snapshots; this UI only saves a choice.
import { daemon, bus, state, registerPromptAdmissionCheck } from './core.js?v=raven-execution-20261006a';
import { setEvoRunLimitAvailability, setRunLimitExecutionMode } from './run_limits.js';

const MODES = new Set(['kernel', 'codex-style', 'raven-code', 'evo']);
const text = (value) => typeof value === 'string' ? value.trim() : '';

function checkedControls(value, sessionId) {
  if (!value || value.sessionId !== sessionId ||
      !['', ...MODES].includes(value.selectedMode) ||
      !MODES.has(value.effectiveMode) ||
      !['', ...MODES].includes(value.activeMode) ||
      typeof value.running !== 'boolean' || value.appliesNextRun !== true ||
      !Array.isArray(value.modes)) {
    throw new Error('執行方式回覆不完整，請重新讀取。');
  }
  const modes = value.modes.filter((mode) =>
    MODES.has(mode?.id) && text(mode.label));
  if (new Set(modes.map((mode) => mode.id)).size !== modes.length ||
      !modes.some((mode) => mode.id === value.effectiveMode) ||
      (value.selectedMode && !modes.some((mode) => mode.id === value.selectedMode)) ||
      (value.activeMode && !modes.some((mode) => mode.id === value.activeMode))) {
    throw new Error('執行方式清單與目前設定不一致，請重新讀取。');
  }
  return { ...value, modes };
}

function bounded(promise, milliseconds) {
  let timer;
  return Promise.race([
    promise,
    new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error('操作逾時')), milliseconds);
    }),
  ]).finally(() => clearTimeout(timer));
}

// Transport ordering is independently testable without a browser or inference.
export function createHarnessControlsController({
  api, currentSession, onChange = () => {}, timeoutMs = 5000,
}) {
  let sessionId = '';
  let controls = null;
  let loading = false;
  let error = '';
  let writeError = false;
  let revision = 0;
  let pendingRead = null;
  // A timeout does not cancel a server-side write. Keep its ownership until
  // the actual transport settles so a late POST cannot overwrite a newer save.
  const pendingWrites = new Map();
  const failedWrites = new Map();
  const snapshot = () => ({ sessionId, controls, loading,
    saving: pendingWrites.has(sessionId), error });
  const publish = () => onChange(snapshot());
  const owns = (sid, token) => sid === sessionId &&
    sid === text(currentSession()) && token === revision;
  const ownsWrite = (sid, owner) => sid === sessionId &&
    sid === text(currentSession()) && pendingWrites.get(sid) === owner;

  function selectSession() {
    const sid = text(currentSession());
    if (sid === sessionId && pendingWrites.has(sid)) return refresh();
    revision += 1;
    sessionId = sid;
    controls = null;
    loading = false;
    error = pendingWrites.get(sid)?.error || failedWrites.get(sid) || '';
    writeError = !!error;
    publish();
    return refresh({ preserveError: !!error });
  }

  async function refresh({ preserveError = false } = {}) {
    if (sessionId !== text(currentSession())) return selectSession();
    if (!sessionId) return;
    const pending = pendingWrites.get(sessionId);
    if (pending) {
      pending.refreshRequested = true;
      return;
    }
    // Startup and SSE invalidations can request the same observation together.
    // Share its completion so a send does not discard the read it is awaiting.
    if (pendingRead?.sessionId === sessionId && pendingRead.token === revision) {
      pendingRead.preserveError &&= preserveError;
      return pendingRead.operation;
    }
    const sid = sessionId;
    const token = ++revision;
    const owner = { sessionId: sid, token, preserveError, operation: null };
    // Install ownership before observation/publication. A synchronous observer
    // must be able to join the operation, and a model change starts a new one.
    owner.operation = (async () => {
      await Promise.resolve();
      try {
        const next = checkedControls(
          await bounded(api.harnessControls(sid), timeoutMs), sid,
        );
        if (!owns(sid, token)) return;
        controls = next;
        if (!owner.preserveError) { error = ''; writeError = false; failedWrites.delete(sid); }
      } catch (err) {
        if (!owns(sid, token)) return;
        error = `無法取得執行方式：${err?.message || err}`;
      } finally {
        if (pendingRead === owner) pendingRead = null;
        if (owns(sid, token)) {
          loading = false;
          publish();
        }
      }
    })();
    pendingRead = owner;
    loading = true;
    publish();
    return owner.operation;
  }

  async function save(mode) {
    if (sessionId !== text(currentSession())) return selectSession();
    if (!sessionId || !controls || loading || pendingWrites.has(sessionId) ||
        !(mode === '' || controls.modes.some((item) => item.id === mode))) return;
    const sid = sessionId;
    revision += 1;
    const owner = { settled: false, error: '', refreshRequested: false, timedOut: false };
    pendingWrites.set(sid, owner);
    error = '';
    writeError = false;
    // Keep the last confirmed choice until persistence is acknowledged.
    publish();
    const operation = (async () => {
      try {
        const next = checkedControls(await api.setHarnessControls(sid, { mode }), sid);
        if (next.selectedMode !== mode)
          throw new Error('伺服器未確認所選執行方式，請重新讀取後再送出');
        owner.error = '';
        failedWrites.delete(sid);
        if (!ownsWrite(sid, owner)) return;
        controls = next;
        error = '';
        writeError = false;
      } catch (err) {
        owner.error = `執行方式未確認儲存：${err?.message || err}`;
        failedWrites.set(sid, owner.error);
        if (ownsWrite(sid, owner)) { error = owner.error; writeError = true; }
      } finally {
        owner.settled = true;
        const selected = ownsWrite(sid, owner);
        if (pendingWrites.get(sid) === owner) pendingWrites.delete(sid);
        if (selected) {
          publish();
          // Unordered SSE is only an invalidation. Failed-save alerts remain
          // visible across automatic reconciliation until explicit rereading.
          if (owner.refreshRequested || owner.timedOut)
            await refresh({ preserveError: !!owner.error });
        }
      }
    })();
    owner.operation = operation;
    try {
      await bounded(operation, timeoutMs);
    } catch (err) {
      if (owner.settled) return;
      owner.timedOut = true;
      owner.error = `執行方式儲存結果尚未確認：${err?.message || err}。等待回覆期間不能再次修改。`;
      if (ownsWrite(sid, owner)) {
        error = owner.error;
        writeError = true;
        publish();
      }
    }
  }

  async function ensureReady(sid) {
    const selected = () => sid && sid === text(currentSession()) && sid === sessionId;
    if (sessionId !== text(currentSession())) await selectSession();
    if (!selected()) throw new Error('對話已切換，訊息尚未送出');
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      let pending;
      while ((pending = pendingWrites.get(sid))) {
        if (pending.timedOut) throw new Error(pending.error);
        const remaining = deadline - Date.now();
        if (remaining <= 0) throw new Error('執行方式儲存結果尚未確認，訊息尚未送出');
        try { await bounded(pending.operation, remaining); }
        catch { throw new Error('執行方式儲存結果尚未確認，訊息尚未送出'); }
        if (!selected()) throw new Error('對話已切換，訊息尚未送出');
        if (pending.error) throw new Error(pending.error);
      }
      // A failed read can recover on the next send now that the experimental
      // picker has no visible Retry button. Unsettled writes stay fenced above.
      if (!writeError && (!controls || error || loading)) {
        const remaining = deadline - Date.now();
        if (remaining <= 0) throw new Error('無法取得執行方式：操作逾時');
        try { await bounded(refresh(), remaining); }
        catch { throw new Error('無法取得執行方式：操作逾時'); }
      }
      if (!selected()) throw new Error('對話已切換，訊息尚未送出');
      // A read publishes controls before its promise settles; a selection can
      // start at that boundary. Never return ready with a newly pending write.
      if (pendingWrites.has(sid)) continue;
      // A same-session model change can replace the observation while a send
      // waits. Join that current read before inspecting its confirmed controls.
      if (!writeError && pendingRead?.sessionId === sid && pendingRead.token === revision)
        continue;
      if (error || !controls || ['invalid', 'unavailable'].includes(controls.preferenceStatus))
        throw new Error(error || '執行方式尚未確認，請重新讀取或選擇後再送出');
      return { sessionId: sid, selectedMode: controls.selectedMode,
        effectiveMode: controls.effectiveMode };
    }
  }

  return { snapshot, selectSession, refresh, save, ensureReady };
}

let initialized = false;
let liveController = null;

export async function ensureHarnessControlsReady(sid) {
  if (!liveController) throw new Error('執行方式設定尚未就緒，訊息尚未送出');
  return liveController.ensureReady(sid);
}

export function initHarnessControls() {
  if (initialized) return;
  initialized = true;
  let renderEvo = null;
  const controller = createHarnessControlsController({
    api: daemon,
    currentSession: () => state.sessionId,
    onChange(view) {
      setEvoRunLimitAvailability(!view.error &&
        !!view.controls?.modes.some((mode) => mode.id === 'evo'));
      setRunLimitExecutionMode(!view.error && !view.saving ? view.controls?.effectiveMode : '');
      if (!renderEvo && view.controls?.modes.some((mode) => mode.id === 'evo') && typeof document?.createElement === 'function') renderEvo = mountEvoControls(controller);
      renderEvo?.(view);
    },
  });
  liveController = controller;
  registerPromptAdmissionCheck(ensureHarnessControlsReady);

  const isSelected = (data) => !data?.sessionId || data.sessionId === state.sessionId;
  for (const event of ['thread:switched', 'session_created', 'model_switched'])
    bus.on(event, () => void controller.selectSession());
  for (const event of ['harness_controls_changed', 'run_state', 'turn_completed', 'sse_open'])
    bus.on(event, (data) => {
      if (isSelected(data)) void controller.refresh({ preserveError: true });
    });
  bus.on('harness_controls_opened', () => void controller.refresh());
  void controller.selectSession();
}

function mountEvoControls(controller) {
  const panel = document.createElement('section');
  panel.id = 'evoControls'; panel.hidden = true;
  panel.setAttribute('aria-label', 'Evo 執行方式');
  const row = document.createElement('label'); row.textContent = '執行方式 ';
  const select = document.createElement('select'); select.setAttribute('aria-label', '執行方式'); row.append(select);
  const status = document.createElement('span'); status.setAttribute('role', 'status');
  const open = document.createElement('button'); open.type = 'button'; open.textContent = '開啟結果專案'; open.hidden = true;
  const retry = document.createElement('button'); retry.type = 'button'; retry.textContent = '重試回顧'; retry.hidden = true;
  retry.title = '沿用已完成的執行結果，只重試回顧與經驗儲存';
  panel.append(row, status, open, retry);
  document.getElementById('composerWrap')?.prepend(panel);
  let resultWorkspace = '', currentRun = '';
  function showState(value) {
    if (!value || value.sessionId !== state.sessionId || !value.runId) return;
    if (currentRun && currentRun !== value.runId) resultWorkspace = '';
    currentRun = value.runId;
    const labels = { running: '執行中', completed: '已完成', incomplete: '部分完成', interrupted: '已中斷', failed: '失敗' };
    const phase = { snapshot: '準備專案副本', coordinator: '準備試跑', executor: '試跑', improver: '比較與改善', ended: '' };
    status.textContent = `Evo · ${labels[value.status] || value.status} ${phase[value.phase] || ''}${value.stage ? ` · 第 ${value.stage} 階段` : ''}`;
    if (value.status === 'running' && Array.isArray(value.activeStages) && value.activeStages.length > 1) status.textContent += ` · 同時處理 ${value.activeStages.length} 個階段`;
    if (value.stopReason === 'token_budget') status.textContent = 'Evo · 流程預算已用完，尚未完成';
    status.title = value.error || (value.omittedFiles ? `有 ${value.omittedFiles} 個項目未納入專案副本。` : '在專案副本中執行');
    status.dataset.running = String(value.status === 'running');
    resultWorkspace = value.stopReason === 'token_budget' ? '' : value.resultWorkspace || '';
    open.hidden = !resultWorkspace || value.status === 'running';
    retry.hidden = value.reflectionRecoveryAvailable !== true || value.status === 'running';
    retry.disabled = value.status === 'running';
    if (!retry.hidden) status.textContent = 'Evo · 回顧待恢復，執行結果已保留';
  }
  let read = 0;
  async function refreshEvo() {
    const sid = state.sessionId, token = ++read;
    if (!sid || panel.hidden) return;
    try { const value = await daemon.evoStatus(sid); if (token === read && sid === state.sessionId) showState(value); } catch { /* existing run errors own alerts */ }
  }
  open.addEventListener('click', async () => {
    if (!resultWorkspace) return;
    const { loadProjects, saveProjects, activateProject } = await import('./sidebar.js?v=raven-execution-20261006a');
    const projects = loadProjects();
    if (!projects.some((item) => item.cwd === resultWorkspace)) { projects.push({ cwd: resultWorkspace }); saveProjects(projects); }
    await activateProject(resultWorkspace);
  });
  retry.addEventListener('click', () => {
    if (!retry.hidden && !retry.disabled && currentRun && !state.busy) {
      retry.disabled = true;
      bus.emit('evo:retry-reflection', { sessionId: state.sessionId, runId: currentRun });
    }
  });
  select.addEventListener('change', () => void controller.save(select.value));
  bus.on('evo_state', (value) => { if (value?.sessionId === state.sessionId) { read += 1; showState(value); } });
  for (const event of ['thread:switched','session_created']) bus.on(event, () => { currentRun = ''; resultWorkspace = ''; open.hidden = true; retry.hidden = true; status.textContent = ''; void refreshEvo(); });
  bus.on('turn_completed', (value) => { if (value?.sessionId === state.sessionId) void refreshEvo(); });
  bus.on('evo:reflection-retry-settled', (value) => {
    if (value?.sessionId === state.sessionId && value.runId === currentRun) {
      retry.disabled = false;
      void refreshEvo();
    }
  });

 return (view) => {

      panel.hidden = !view.controls?.modes.some((mode) => mode.id === 'evo');
      if (panel.hidden) return;
      select.replaceChildren(...[{ id: '', label: 'Meadow Evo（預設）' }, ...view.controls.modes].map((mode) => { const option = document.createElement('option'); option.value = mode.id; option.textContent = mode.label; return option; }));
      select.value = view.controls.selectedMode || '';
      select.disabled = view.loading || view.saving;
      if (view.error) { status.textContent = view.error; status.dataset.running = 'false'; }
      else if (!currentRun) status.textContent = 'Evo 在專案副本中試跑，再合併改善。';
      void refreshEvo();
 };
}
