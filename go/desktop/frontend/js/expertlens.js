// expertlens.js — optional, fact-only Agent observability for human guidance.
//
// This panel deliberately does not expose or synthesize chain-of-thought. It
// projects the same public SSE facts already used by the timeline: user goal,
// runtime phase, tool execution, validator output, and guidance disposition.
// The feature is additive, defaults on, and can be paused persistently without
// changing the existing conversation or Agent pipeline.
import { bus, state } from './core.js?v=raven-execution-20261006a';
import { showRightPane, hideRightPane, toggleRightPane } from './rightpanel.js';
import { artifactSearchPresentation } from './artifact_search_presentation.js';

const $ = (selector) => document.querySelector(selector);
const ENABLED_KEY = 'meadow.expertLens.enabled';

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text != null) node.textContent = text;
  return node;
}

function enabledByDefault() {
  try {
    return localStorage.getItem(ENABLED_KEY) !== 'false';
  } catch {
    return true;
  }
}

function persistEnabled(enabled) {
  try {
    localStorage.setItem(ENABLED_KEY, enabled ? 'true' : 'false');
  } catch {
    // Private storage modes can reject writes; the in-memory toggle still works.
  }
}

function textContent(update) {
  const content = update && update.content;
  if (typeof content === 'string') return content;
  if (content && typeof content.text === 'string') return content.text;
  return '';
}

function stringify(value) {
  if (value == null) return '';
  if (typeof value === 'string') return value;
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function compact(value, limit = 420) {
  const oneLine = stringify(value).replace(/\s+/g, ' ').trim();
  if (!oneLine) return '';
  return oneLine.length > limit ? oneLine.slice(0, limit - 1) + '…' : oneLine;
}

function toolOutput(name, output) {
  return name === 'artifact_search'
    ? artifactSearchPresentation(output)?.text || stringify(output)
    : stringify(output);
}

function failedToolStatus(status) {
  return ['failed', 'error', 'rejected', 'blocked'].includes(
    String(status || '').toLowerCase(),
  );
}

const PHASES = {
  connecting: ['連接 Core', '等待本機推理服務就緒'],
  model_loading: ['載入模型', '模型進入統一記憶體後才會開始任務'],
  queued: ['等待排程', '等待目前推理工作釋放執行槽'],
  cache_lookup: ['重用上下文', '正在檢查 Prefix cache'],
  prefill: ['理解上下文', '模型正在讀取本輪輸入與工作紀錄'],
  first_token: ['準備行動', '上下文處理完成，等待第一個輸出'],
  decoding: ['產生回覆', '模型正在串流文字或組裝工具參數'],
  agent_requirement_audit: ['核對需求', '逐項比對原始需求與目前證據'],
  agent_checkpoint: ['保存檢查點', '保存目前可回復的最佳版本'],
  agent_verifying: ['獨立驗證', '執行編譯、瀏覽器或其他驗收工具'],
  agent_validation_failed: ['驗收失敗', '具體錯誤已保留，下一輪應改變策略'],
  agent_recovery: ['自我修正', '根據最近的真實錯誤重新規劃'],
  agent_rollback: ['回復安全版本', '目前修改未改善結果，回復檢查點'],
  agent_validation_passed: ['本次驗證通過', '正在核對其餘需求與最後輸出'],
};

function phaseInfo(progress, running) {
  const phase = String((progress && progress.phase) || '');
  if (PHASES[phase])
    return { phase, label: PHASES[phase][0], detail: PHASES[phase][1] };
  if (running)
    return {
      phase: 'running',
      label: '執行中',
      detail: '等待下一個可觀測事件',
    };
  return {
    phase: 'idle',
    label: '待命',
    detail: '送出任務後會顯示可驗證的執行階段',
  };
}

function nextAction(tool, status, output, phase) {
  if (status !== 'failed') {
    if (phase === 'agent_validation_passed')
      return '整理通過證據並產生最後結果';
    if (tool) return `等待 ${tool} 結果或下一個 Kernel 階段`;
    return '等待模型提出下一個可執行動作';
  }
  const lower = `${tool} ${output}`.toLowerCase();
  if (lower.includes('interaction'))
    return '比對「使用者原始要求、驗收期待、實際 DOM」；若要求彼此矛盾，先請使用者決定';
  if (lower.includes('typescript') || lower.includes('build'))
    return '依編譯器錯誤修正 TypeScript，重新編譯後再驗證';
  if (lower.includes('visual') || lower.includes('browser'))
    return '依瀏覽器錯誤修正可見介面，再重跑同一份驗收';
  if (lower.includes('edit'))
    return '確認檔案路徑與替換範圍，產生一個不同且可驗證的修改';
  if (lower.includes('web_'))
    return '改用可存取的來源或更精準查詢，避免重複失敗網址';
  return '保留錯誤證據，改用不同策略；不要原樣重複失敗動作';
}

function latestUserGoal(messages) {
  if (!Array.isArray(messages)) return '';
  for (let index = messages.length - 1; index >= 0; index--) {
    const message = messages[index] || {};
    if (message.role !== 'user') continue;
    const content = message.content;
    if (typeof content === 'string' && content.trim()) return content.trim();
    if (Array.isArray(content)) {
      const joined = content
        .map((part) => (part && typeof part.text === 'string' ? part.text : ''))
        .join('')
        .trim();
      if (joined) return joined;
    }
  }
  return '';
}

export function initExpertLens() {
  const panel = $('#rightPanel');
  const wrap = $('#rpWrap');
  const tabs = $('#rpTabs');
  if (!panel || !wrap || !tabs || $('#rpTabExpert')) return;

  const tab = el('button', 'rp-tab', '專家透視');
  tab.id = 'rpTabExpert';
  tab.type = 'button';
  tab.setAttribute('role', 'tab');
  tab.setAttribute('aria-selected', 'false');
  tab.setAttribute('aria-controls', 'rpPageExpert');
  tabs.appendChild(tab);

  const page = el('div', 'rp-page expert-lens');
  page.id = 'rpPageExpert';
  page.setAttribute('role', 'tabpanel');
  page.setAttribute('aria-labelledby', 'rpTabExpert');
  page.innerHTML = `
    <div class="xl-head">
      <div>
        <div class="xl-title">專家透視 <span>Beta</span></div>
        <div class="xl-sub">只顯示執行事實，不顯示或猜測模型私有思考鏈。</div>
      </div>
      <button id="xlToggle" class="xl-toggle" type="button" role="switch"></button>
    </div>
    <div id="xlDisabled" class="xl-disabled" hidden>專家透視已暫停。任務仍照原本流程執行，不會改變模型或 Kernel 行為。</div>
    <div id="xlBody">
      <section class="xl-card xl-goal"><div class="xl-label">目前目標</div><div id="xlGoal" class="xl-value">尚未收到任務</div></section>
      <div class="xl-grid">
        <section class="xl-card"><div class="xl-label">目前階段</div><div id="xlPhase" class="xl-value">待命</div><div id="xlPhaseDetail" class="xl-detail">送出任務後開始追蹤</div></section>
        <section class="xl-card"><div class="xl-label">本輪狀態</div><div id="xlRun" class="xl-value">尚未執行</div><div id="xlRunDetail" class="xl-detail">0 個工具動作</div></section>
      </div>
      <section class="xl-card"><div class="xl-label">本輪執行步驟</div><ol id="xlActivity" class="xl-activity"><li class="empty">尚無執行步驟</li></ol></section>
      <section class="xl-card"><div class="xl-label">最新可驗證證據</div><div id="xlEvidence" class="xl-value">尚無工具結果</div><div id="xlEvidenceDetail" class="xl-detail">編譯、瀏覽器與檔案結果會顯示在這裡</div></section>
      <section id="xlBlockerCard" class="xl-card xl-blocker" hidden><div class="xl-label">目前卡點</div><div id="xlBlocker" class="xl-value"></div><div class="xl-label xl-next-label">建議關注的下一步</div><div id="xlNext" class="xl-detail"></div></section>
      <section class="xl-card"><div class="xl-label">專家引導</div><div id="xlGuidance" class="xl-value">沒有待套用引導</div><div class="xl-detail">引導會在下一個安全邊界交給模型，不會中斷正在寫入的工具。</div><button id="xlGuide" class="xl-guide" type="button">輸入專家引導</button></section>
    </div>`;
  wrap.appendChild(page);

  const ui = {
    toggle: $('#xlToggle'),
    disabled: $('#xlDisabled'),
    body: $('#xlBody'),
    goal: $('#xlGoal'),
    phase: $('#xlPhase'),
    phaseDetail: $('#xlPhaseDetail'),
    run: $('#xlRun'),
    runDetail: $('#xlRunDetail'),
    activity: $('#xlActivity'),
    evidence: $('#xlEvidence'),
    evidenceDetail: $('#xlEvidenceDetail'),
    blockerCard: $('#xlBlockerCard'),
    blocker: $('#xlBlocker'),
    next: $('#xlNext'),
    guidance: $('#xlGuidance'),
    guide: $('#xlGuide'),
  };
  let enabled = enabledByDefault();
  let goal = '';
  let progress = null;
  let running = false;
  let toolCount = 0;
  let currentTool = '';
  let lastTool = '';
  let lastToolStatus = '';
  let lastToolOutput = '';
  let pendingGuidance = 0;
  let lastRecordedPhase = '';
  let lastPhaseActivityKey = '';
  let currentToolActivityKey = '';
  let activitySequence = 0;
  let observedRunId = '';
  const activities = [];
  const toolAttempts = new Map();
  const openButton = $('#btnExpertLens');

  function acceptsEvent(update) {
    if (!update || (update.sessionId && update.sessionId !== state.sessionId))
      return false;
    const runId = String(update.runId || '');
    const currentRunId = observedRunId || String(state.runId || '');
    if (runId && currentRunId && runId !== currentRunId) return false;
    if (runId && !observedRunId) observedRunId = runId;
    return true;
  }

  function resetRunFacts() {
    progress = null;
    toolCount = 0;
    currentTool = '';
    lastTool = '';
    lastToolStatus = '';
    lastToolOutput = '';
    lastRecordedPhase = '';
    lastPhaseActivityKey = '';
    currentToolActivityKey = '';
    activities.length = 0;
    toolAttempts.clear();
  }

  function observeToolAttempt(update, terminal = false) {
    const key = toolActivityKey(update) ||
      (terminal && currentToolActivityKey) || `tool:local-${++activitySequence}`;
    const previous = toolAttempts.get(key);
    const alreadyCompleted = previous?.completed === true;
    if (!previous) {
      toolAttempts.set(key, { completed: terminal });
      toolCount++;
    } else if (terminal) previous.completed = true;
    return { key, alreadyCompleted };
  }

  function addActivity(label, detail = '', status = '', options = {}) {
    const item = {
      label: compact(label, 120),
      detail: compact(detail, 220),
      status,
      key: String(options.key || ''),
      kind: String(options.kind || ''),
    };
    const existing = item.key
      ? activities.find((activity) => activity.key === item.key)
      : null;
    if (existing) {
      existing.label = item.label;
      existing.detail = item.detail;
      existing.status = item.status;
      existing.kind = item.kind || existing.kind;
      return;
    }
    const previous = activities[activities.length - 1];
    if (
      previous &&
      previous.label === item.label &&
      previous.detail === item.detail &&
      previous.status === item.status
    )
      return;
    activities.push(item);
    if (activities.length > 12) activities.shift();
  }

  function completeActivity(key) {
    if (!key) return;
    const activity = activities.find((item) => item.key === key);
    if (activity && !activity.status) activity.status = 'passed';
  }

  function toolActivityKey(update) {
    const id = String(
      (update && (update.toolCallId || update.callId || update.id)) || '',
    );
    const sessionId = String(update?.sessionId || state.sessionId || '');
    const runId = String(update?.runId || observedRunId || state.runId || '');
    return id ? `tool:${sessionId}\u0000${runId}\u0000${id}` : '';
  }

  function paintActivities() {
    ui.activity.replaceChildren();
    if (!activities.length) {
      ui.activity.appendChild(el('li', 'empty', '尚無執行事件'));
      return;
    }
    for (const activity of activities) {
      const status =
        activity.status ||
        (!running && activity.kind === 'phase' ? 'passed' : '');
      const item = el('li', status);
      item.appendChild(el('span', 'xl-activity-dot'));
      const copy = el('div');
      copy.appendChild(el('strong', '', activity.label));
      if (activity.detail) copy.appendChild(el('small', '', activity.detail));
      item.appendChild(copy);
      ui.activity.appendChild(item);
    }
  }

  function applyEnabled() {
    ui.toggle.textContent = enabled ? '開啟' : '關閉';
    ui.toggle.setAttribute('aria-checked', enabled ? 'true' : 'false');
    ui.toggle.classList.toggle('on', enabled);
    ui.disabled.hidden = enabled;
    ui.body.hidden = !enabled;
  }

  function paint() {
    applyEnabled();
    if (!enabled) return;
    const info = phaseInfo(progress, running);
    ui.goal.textContent = compact(goal, 520) || '尚未收到任務';
    ui.goal.title = goal;
    ui.phase.textContent = info.label;
    ui.phaseDetail.textContent = info.detail;
    ui.run.textContent = running ? 'Agent 執行中' : '目前沒有執行中的任務';
    ui.runDetail.textContent = `${toolCount} 個工具動作${currentTool ? ` · 正在執行 ${currentTool}` : ''}`;
    paintActivities();
    if (lastTool) {
      const evidenceState =
        lastToolStatus === 'failed'
          ? '未通過'
          : lastToolStatus === 'recorded'
            ? '歷史紀錄'
            : '已完成';
      ui.evidence.textContent = `${evidenceState} · ${lastTool}`;
      ui.evidenceDetail.textContent =
        compact(lastToolOutput, 520) || '工具未回傳文字摘要';
      ui.evidenceDetail.title = lastToolOutput;
    } else {
      ui.evidence.textContent = '尚無工具結果';
      ui.evidenceDetail.textContent = '編譯、瀏覽器與檔案結果會顯示在這裡';
      ui.evidenceDetail.title = '';
    }
    const blocked = failedToolStatus(lastToolStatus);
    ui.blockerCard.hidden = !blocked;
    if (blocked) {
      ui.blocker.textContent =
        compact(lastToolOutput, 520) || `${lastTool} 未通過`;
      ui.blocker.title = lastToolOutput;
      ui.next.textContent = nextAction(
        lastTool,
        lastToolStatus,
        lastToolOutput,
        info.phase,
      );
    }
    ui.guidance.textContent =
      pendingGuidance > 0
        ? `${pendingGuidance} 則引導等待下一個安全邊界`
        : '沒有待套用引導';
  }

  function activate() {
    showRightPane(page.id);
    paint();
  }

  function deactivate() {
    hideRightPane(page.id);
  }

  tab.addEventListener('click', () => {
    toggleRightPane(page.id);
    paint();
  });
  document.addEventListener('rightpanel:layout', () => {
    openButton?.classList.toggle(
      'on',
      page.classList.contains('on') && panel.classList.contains('open'),
    );
  });
  openButton?.addEventListener('click', () => {
    if (page.classList.contains('on') && panel.classList.contains('open')) {
      deactivate();
      return;
    }
    activate();
  });
  $('#diffClose')?.addEventListener('click', () =>
    openButton?.classList.remove('on'),
  );
  ui.toggle.addEventListener('click', () => {
    enabled = !enabled;
    persistEnabled(enabled);
    paint();
  });
  ui.guide.addEventListener('click', () => {
    const mode = $('#runningInputMode');
    const modeName = $('#runningInputModeName');
    if (mode && !mode.hidden && modeName?.textContent?.includes('排入'))
      mode.click();
    const input = $('#input');
    if (input) {
      input.focus();
      input.placeholder = state.busy
        ? '輸入專家引導；會在下一個安全邊界套用…'
        : '輸入訊息…';
    }
  });

  bus.on('update:user_message_chunk', (update) => {
    if (!acceptsEvent(update)) return;
    const text = textContent(update).trim();
    if (text) {
      goal = text;
      // Modern admission resets on run identity, before this echo arrives.
      // A Guide echo belongs to the existing run even if delivered late.
      if (!running && !update._steer && !update.runId) resetRunFacts();
    }
    paint();
  });
  bus.on('history:load', (messages) => {
    goal = latestUserGoal(messages);
    paint();
  });
  bus.on('history:tools', (tools) => {
    const observed = Array.isArray(tools) ? tools : [];
    resetRunFacts();
    observedRunId ||= String(state.runId || '');
    // The display is bounded, but the snapshot's attempt count is not its
    // visible row count. Retain identities for replayed completion receipts.
    for (const tool of observed) observeToolAttempt(tool, true);
    for (const tool of observed.slice(-12)) {
      const name = String(tool?.name || '工具');
      const detail = compact(toolOutput(name, tool?.output || tool?.input), 220);
      const status = failedToolStatus(tool?.status) ? 'failed' : 'passed';
      addActivity(
        `${name} · ${status === 'failed' ? '未通過' : '歷史紀錄'}`,
        detail,
        status,
        { key: toolActivityKey(tool), kind: 'tool' },
      );
    }
    const latest = observed[observed.length - 1];
    if (latest) {
      lastTool = String(latest.name || '工具');
      lastToolStatus = String(latest.status || 'recorded');
      lastToolOutput = toolOutput(lastTool, latest.output || latest.input);
    }
    paint();
  });
  bus.on('inference_progress', (value) => {
    if (!acceptsEvent(value)) return;
    progress = value;
    const info = phaseInfo(progress, running);
    if (info.phase !== 'idle' && info.phase !== lastRecordedPhase) {
      completeActivity(lastPhaseActivityKey);
      lastPhaseActivityKey = `phase:${++activitySequence}`;
      addActivity(
        info.label,
        info.detail,
        info.phase.includes('failed') ? 'failed' : '',
        { key: lastPhaseActivityKey, kind: 'phase' },
      );
      lastRecordedPhase = info.phase;
    }
    paint();
  });
  bus.on('update:tool_call', (update) => {
    if (!acceptsEvent(update)) return;
    const attempt = observeToolAttempt(update);
    // Reconnection can deliver a delayed start after its terminal receipt.
    if (attempt.alreadyCompleted) return;
    completeActivity(lastPhaseActivityKey);
    currentTool = String((update && (update.title || update.kind)) || '工具');
    currentToolActivityKey = attempt.key;
    addActivity(`執行 ${currentTool}`, '等待真實工具結果', '', {
      key: currentToolActivityKey,
      kind: 'tool',
    });
    paint();
  });
  bus.on('update:tool_call_update', (update) => {
    if (!acceptsEvent(update)) return;
    // Results survive replay/transport gaps even when their start does not.
    const attempt = observeToolAttempt(update, true);
    lastTool = String(update.title || currentTool || '工具');
    lastToolStatus = String(update.status || '');
    lastToolOutput = toolOutput(lastTool, update.rawOutput);
    const failed = failedToolStatus(lastToolStatus);
    const activityKey = attempt.key;
    addActivity(
      failed ? `${lastTool} 未通過` : `${lastTool} 已完成`,
      lastToolOutput,
      failed ? 'failed' : 'passed',
      { key: activityKey, kind: 'tool' },
    );
    currentToolActivityKey = '';
    currentTool = '';
    paint();
  });
  bus.on('guidance', (event) => {
    if (!acceptsEvent(event)) return;
    const disposition = String(event.disposition || '');
    if (disposition === 'accepted' || disposition === 'queued')
      pendingGuidance++;
    if (
      (disposition === 'applied' || disposition === 'cancelled') &&
      pendingGuidance > 0
    )
      pendingGuidance--;
    if (disposition)
      addActivity(
        disposition === 'applied' ? '專家引導已套用' : '專家引導狀態更新',
        disposition,
        disposition === 'cancelled' ? 'failed' : 'passed',
      );
    paint();
  });
  bus.on('run_state', (value) => {
    if (!value || (value.sessionId && value.sessionId !== state.sessionId))
      return;
    const runId = String(value.runId || '');
    if (runId && state.runId && runId !== state.runId) return;
    const nextRunning = value.status === 'running' || value.status === 'cancelling';
    if (nextRunning && runId && runId !== observedRunId) {
      resetRunFacts();
      observedRunId = runId;
    } else if (!acceptsEvent(value)) return;
    running = nextRunning;
    paint();
  });
  bus.on('turn:done', (value = {}) => {
    if (!acceptsEvent(value)) return;
    running = false;
    currentTool = '';
    paint();
  });
  bus.on('thread:switched', (value = {}) => {
    goal = '';
    resetRunFacts();
    observedRunId = String(value.runId || state.runId || '');
    running = !!value.running;
    pendingGuidance = 0;
    deactivate();
    paint();
  });

  applyEnabled();
  paint();
}
