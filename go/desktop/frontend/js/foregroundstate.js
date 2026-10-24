// foregroundstate.js - one Claude-style foreground status for the Desktop.
// Model residency, inference telemetry, the agent loop, permissions and tools
// remain separate facts, but only the highest-priority current fact is shown
// in the primary shell status. Diagnostic panels may still retain full history.
import { bus, state } from './core.js?v=raven-execution-20261006a';

const TERMINAL_TOOL_STATUSES = new Set([
  'completed',
  'failed',
  'cancelled',
  'blocked',
  'denied',
]);

const AGENT_PHASES = {
  agent_requirement_audit: ['正在核對任務需求', 'agent'],
  agent_checkpoint: ['正在保存檢查點', 'agent'],
  agent_verifying: ['正在驗證結果', 'agent'],
  agent_validation_failed: ['驗證失敗，準備更換策略', 'warning'],
  agent_validation_passed: ['驗證已通過', 'agent'],
  agent_recovery: ['正在根據錯誤修正', 'agent'],
  agent_rollback: ['正在回復安全版本', 'warning'],
};

const INFERENCE_PHASES = {
  connecting: ['正在連接 Meadow Core', 'model'],
  model_loading: ['正在載入模型', 'model'],
  queued: ['等待推理排程', 'model'],
  waiting: ['正在等待模型回覆', 'model'],
  streaming: ['正在接收模型 API 資料', 'model'],
  cache_lookup: ['正在查找 Prefix cache', 'model'],
  prefill: ['正在理解輸入', 'model'],
  first_token: ['即將開始輸出', 'model'],
  decoding: ['正在產生回覆或動作', 'model'],
  document_ingest: ['正在建立文件索引', 'agent'],
  document_ready: ['文件索引已就緒', 'agent'],
};

let initialized = false;
let resident = { status: 'starting', model: '', detail: '' };
let runStatus = 'idle';
let progress = null;
let currentTool = null;
let waitingFor = '';
let streamError = '';

const shortName = (value) =>
  String(value || '')
    .split('/')
    .filter(Boolean)
    .pop() || String(value || '');

function sameModel(a, b) {
  const left = shortName(a).trim().toLowerCase();
  const right = shortName(b).trim().toLowerCase();
  return !!left && !!right && left === right;
}

function activeRun() {
  return runStatus === 'running' || runStatus === 'cancelling';
}

function presentation() {
  const selectedModel = String(state.model || '').trim();
  const residentModel = String(resident.model || '').trim();
  const mismatch =
    !!selectedModel &&
    !!residentModel &&
    !sameModel(selectedModel, residentModel);
  const base = { selectedModel, residentModel, mismatch, active: true };

  if (streamError) {
    return { ...base, kind: 'error', tone: 'bad', label: streamError };
  }
  if (runStatus === 'cancelling') {
    return { ...base, kind: 'cancelling', tone: 'warn', label: '正在停止任務' };
  }
  if (waitingFor) {
    return {
      ...base,
      kind: 'waiting_user',
      tone: 'warn',
      label: waitingFor,
      detail: '等待你的回覆',
    };
  }
  if (currentTool) {
    return {
      ...base,
      kind: 'tool',
      tone: 'ok',
      label: `正在執行 ${currentTool.name}`,
      detail: currentTool.detail || '',
    };
  }
  const phase = String(progress?.phase || '');
  if (AGENT_PHASES[phase]) {
    const [label, kind] = AGENT_PHASES[phase];
    return {
      ...base,
      kind,
      phase,
      tone: kind === 'warning' ? 'warn' : 'ok',
      label,
    };
  }
  if (INFERENCE_PHASES[phase] && phase !== 'model_ready') {
    const [label, kind] = INFERENCE_PHASES[phase];
    return { ...base, kind, phase, tone: 'ok', label };
  }
  if (activeRun()) {
    return {
      ...base,
      kind: 'agent',
      tone: 'ok',
      label: '等待模型下一步',
      detail: '模型、工具與驗證共用同一個 Agent loop',
    };
  }
  if (resident.status === 'error') {
    return {
      ...base,
      kind: 'error',
      tone: 'bad',
      label: resident.detail || '模型載入失敗',
    };
  }
  if (resident.status !== 'ready') {
    const detail = String(resident.detail || '');
    const loading =
      resident.status === 'loading' || /載入|恢復|啟動/.test(detail);
    if (!loading && selectedModel) {
      return {
        ...base,
        active: false,
        kind: 'cold',
        tone: 'ok',
        label: '',
        detail: '送出訊息時會自動載入已選模型',
      };
    }
    if (loading) {
      return {
        ...base,
        active: false,
        kind: 'model_loading',
        tone: 'ok',
        label: '',
        detail: '模型準備進度會顯示在本次對話狀態中',
      };
    }
    return {
      ...base,
      kind: 'model',
      tone: 'warn',
      label: '沒有載入模型，請先選擇模型',
    };
  }
  if (mismatch) {
    return {
      ...base,
      kind: 'model_mismatch',
      tone: 'warn',
      label: `已選 ${shortName(selectedModel)} · 常駐 ${shortName(residentModel)}`,
      detail: '送出前會切換到此對話選擇的模型',
    };
  }
  return {
    ...base,
    active: false,
    kind: 'ready',
    tone: 'ok',
    label: '模型載入完成',
  };
}

function paint() {
  const value = presentation();
  const foot = document.querySelector('#modelFoot');
  const dot = document.querySelector('#modelDot');
  const text = document.querySelector('#modelText');
  // Timeline owns the active turn narrative (loading, TTFT, model output,
  // tools and validation). Repeating those phases below the composer made one
  // run look like two independent jobs. Keep this shell-level surface only for
  // actionable cross-turn failures or a selected/resident model mismatch.
  const shellVisible =
    value.kind === 'error' || value.kind === 'model_mismatch';
  if (foot) foot.hidden = !shellVisible;
  if (!shellVisible) {
    bus.emit('foreground_state', value);
    return;
  }
  if (dot)
    dot.className =
      value.tone === 'bad' ? 'bad' : value.tone === 'ok' ? 'ok' : '';
  if (text) {
    text.textContent = value.label;
    text.title = [
      value.detail,
      value.selectedModel ? `已選模型：${value.selectedModel}` : '',
      value.residentModel ? `常駐模型：${value.residentModel}` : '',
    ]
      .filter(Boolean)
      .join('\n');
  }
  bus.emit('foreground_state', value);
}

function toolName(value) {
  return String(value?.title || value?.kind || value?.name || '工具');
}

export function initForegroundState() {
  if (initialized) return;
  initialized = true;
  bus.on('model_status', (value) => {
    resident = value || { status: 'starting', model: '', detail: '' };
    paint();
  });
  bus.on('run_state', (value) => {
    if (!value || (value.sessionId && value.sessionId !== state.sessionId))
      return;
    runStatus = String(value.status || 'idle');
    if (!activeRun()) {
      currentTool = null;
      progress = null;
      waitingFor = '';
    }
    paint();
  });
  bus.on('inference_progress', (value) => {
    if (!value || (value.sessionId && value.sessionId !== state.sessionId))
      return;
    progress = value;
    paint();
  });
  bus.on('update:tool_call', (value) => {
    currentTool = {
      id: value?.toolCallId,
      name: toolName(value),
      detail: String(value?.detail || ''),
    };
    progress = null;
    paint();
  });
  bus.on('update:tool_call_update', (value) => {
    if (!value) return;
    const status = String(value.status || '').toLowerCase();
    if (TERMINAL_TOOL_STATUSES.has(status)) {
      if (
        !currentTool ||
        !value.toolCallId ||
        currentTool.id === value.toolCallId
      )
        currentTool = null;
    } else {
      currentTool = {
        id: value.toolCallId,
        name: toolName(value),
        detail: String(value.detail || ''),
      };
    }
    paint();
  });
  bus.on('permission_request', () => {
    waitingFor = '等待操作權限';
    paint();
  });
  bus.on('user_input_request', () => {
    waitingFor = '等待你的選擇';
    paint();
  });
  bus.on('location_request', (value) => {
    if (value?.sessionId && value.sessionId !== state.sessionId) return;
    waitingFor = '等待搜尋位置';
    paint();
  });
  const resume = () => {
    waitingFor = '';
    paint();
  };
  bus.on('permission_resolved', resume);
  bus.on('user_input_resolved', resume);
  bus.on('location_resolved', (value) => {
    if (!value?.sessionId || value.sessionId === state.sessionId) resume();
  });
  bus.on('stream_error', (value) => {
    streamError = String(value?.message || value?.error || '本輪執行失敗');
    paint();
  });
  const resetRun = () => {
    runStatus = 'idle';
    progress = null;
    currentTool = null;
    waitingFor = '';
    streamError = '';
    paint();
  };
  bus.on('turn:done', resetRun);
  bus.on('thread:switched', resetRun);
  bus.on('session_created', paint);
  bus.on('model_switched', paint);
  paint();
}
