// workcenter.js — Desktop parity for server-owned background work and
// source-backed research. The daemon is authoritative; this module only
// renders snapshots, opens citations, and asks the native shell to notify.
import { daemon, bus, state } from './core.js?v=raven-execution-20261006a';
import { showRightPane } from './rightpanel.js';
import { initTaskTodo } from './task_todo.js?v=task-todo-20261004b';

const $ = (s) => document.querySelector(s);
const ACTIVE = new Set(['queued', 'guiding', 'running', 'cancelling']);

let tasks = [];
let sources = [];
let hydratedSession = '';
const knownTaskStatus = new Map();
const pendingTerminalNotifications = new Map();
let pendingNotificationSession = '';
let agentTools = [];
let agentStage = '';

const BROWSER_TOOLS = new Set([
  'meadow_browser_dom',
  'meadow_browser_visual_check',
  'meadow_browser_interaction_check',
  'web_search',
  'web_fetch',
  'web_crawl',
]);

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text != null) node.textContent = text;
  return node;
}

function statusLabel(status) {
  const labels = {
    queued: '排隊中',
    guiding: '引導中',
    running: '執行中',
    cancelling: '取消中',
    completed: '已完成',
    cancelled: '已取消',
    error: '失敗',
    incomplete: '未完成',
    max_iterations: '達到迭代上限',
    loop_detected: '偵測到循環',
    interrupted: '程式中斷',
  };
  return labels[status] || status || '未知';
}

function isLiveActiveTask(task) {
  return Boolean(task?.sessionId) && ACTIVE.has(task?.status);
}

function taskStatusLabel(task) {
  // A terminal run status alone does not verify the requested outcome.
  if (task?.status === 'completed') return '已結束';
  if (!task?.sessionId && ACTIVE.has(task?.status)) {
    return '等待重新開啟原對話';
  }
  return statusLabel(task?.status);
}

function phaseLabel(phase) {
  const labels = {
    connecting: '正在連接服務',
    model_loading: '正在載入模型',
    queued: '等待排程',
    cache_lookup: '正在查找 Prefix cache',
    prefill: '正在理解上下文',
    first_token: '準備第一個輸出',
    decoding: '正在產生動作',
    agent_requirement_audit: '正在核對需求',
    agent_verifying: '正在獨立驗證',
    agent_validation_failed: '驗收尚未通過',
    agent_recovery: '正在自我修正',
    agent_validation_passed: '本次驗證通過',
  };
  return labels[phase] || phase || '';
}

function toolName(tool) {
  return String(tool?.name || tool?.title || tool?.kind || '工具');
}

function toolDetail(tool) {
  const input = tool?.input || tool?.rawInput || {};
  for (const key of ['file_path', 'source_path', 'html_path', 'url', 'query']) {
    if (typeof input[key] === 'string' && input[key]) return input[key];
  }
  return '';
}

function renderAgentActivity() {
  const root = $('#rpAgentActivity');
  if (!root) return;
  root.replaceChildren();
  if (agentStage) {
    const stage = el('div', 'work-task');
    stage.dataset.status = 'running';
    stage.append(el('span', 'work-dot'));
    const copy = el('div', 'work-copy');
    copy.append(el('strong', '', phaseLabel(agentStage)));
    copy.append(el('span', 'work-meta', '目前 Kernel 階段'));
    stage.append(copy);
    root.append(stage);
  }
  const recent = agentTools.slice(-12).reverse();
  for (const tool of recent) {
    const row = el('div', 'work-task');
    const status = String(tool.status || 'recorded');
    row.dataset.status = status === 'failed' ? 'error' : status;
    row.append(el('span', 'work-dot'));
    const copy = el('div', 'work-copy');
    copy.append(el('strong', '', toolName(tool)));
    copy.append(
      el(
        'span',
        'work-meta',
        `${status === 'recorded' ? '歷史紀錄' : statusLabel(status)}${
          toolDetail(tool) ? ` · ${toolDetail(tool)}` : ''
        }`,
      ),
    );
    row.append(copy);
    root.append(row);
  }
  if (!root.childElementCount)
    root.append(el('div', 'rp-emptyrow', '尚無工具進度'));
}

function renderBrowser() {
  const root = $('#rpBrowser');
  if (!root) return;
  root.replaceChildren();
  const browser = agentTools.filter((tool) =>
    BROWSER_TOOLS.has(toolName(tool)),
  );
  for (const tool of browser.slice(-8).reverse()) {
    const row = el('div', 'work-task');
    row.dataset.status =
      tool.status === 'failed' ? 'error' : tool.status || 'recorded';
    row.append(el('span', 'work-dot'));
    const copy = el('div', 'work-copy');
    copy.append(el('strong', '', toolName(tool)));
    copy.append(
      el('span', 'work-meta', toolDetail(tool) || '已留下可觀測紀錄'),
    );
    row.append(copy);
    root.append(row);
  }
  if (!root.childElementCount)
    root.append(el('div', 'rp-emptyrow', '尚無瀏覽器驗證'));
}

function upsertAgentTool(tool) {
  if (!tool) return;
  const id = String(tool.toolCallId || tool.id || '');
  const normalized = {
    ...tool,
    id,
    name: toolName(tool),
    input: tool.input || tool.rawInput || {},
  };
  const index = id ? agentTools.findIndex((item) => item.id === id) : -1;
  if (index >= 0) agentTools[index] = { ...agentTools[index], ...normalized };
  else agentTools.push(normalized);
  renderAgentActivity();
  renderBrowser();
}

function fmtTime(value) {
  if (!value) return '';
  const d = new Date(value);
  return Number.isNaN(d.getTime())
    ? ''
    : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function toast(message, ok = true) {
  let stack = $('#workToasts');
  if (!stack) {
    stack = el('div', 'work-toasts');
    stack.id = 'workToasts';
    document.body.appendChild(stack);
  }
  const item = el('div', `work-toast ${ok ? 'ok' : 'bad'}`, message);
  stack.appendChild(item);
  setTimeout(() => item.remove(), 5000);
}

function renderTasks() {
  const root = $('#rpTasks');
  const badge = $('#progressBadge');
  const button = $('#btnProgress');
  if (!root) return;
  root.textContent = '';
  if (!tasks.length) {
    root.appendChild(el('div', 'rp-emptyrow', '目前沒有背景工作'));
  } else {
    for (const task of [...tasks].reverse()) {
      const row = el('div', 'work-task');
      row.dataset.taskId = task.id || '';
      row.dataset.status =
        !task.sessionId && ACTIVE.has(task.status)
          ? 'awaiting_resume'
          : task.status || '';
      const dot = el('span', 'work-dot');
      const copy = el('div', 'work-copy');
      copy.appendChild(el('strong', '', task.title || '背景工作'));
      const time = fmtTime(task.finishedAt || task.startedAt || task.createdAt);
      copy.appendChild(
        el(
          'span',
          'work-meta',
          `${taskStatusLabel(task)}${time ? ` · ${time}` : ''}`,
        ),
      );
      row.append(dot, copy);
      if (
        task.kind === 'queued_prompt' &&
        task.sessionId &&
        task.status === 'queued'
      ) {
        const guide = el('button', 'work-guide', '引導');
        guide.type = 'button';
        guide.addEventListener('click', async () => {
          guide.disabled = true;
          try {
            const receipt = await daemon.queueGuide(task.sessionId, task.id);
            if (
              !receipt ||
              receipt.id !== task.id ||
              receipt.mode !== 'steer_current' ||
              receipt.disposition !== 'accepted'
            ) {
              throw new Error('引導回執不完整');
            }
          } catch (e) {
            toast(`引導失敗：${e.message}`, false);
            guide.disabled = false;
          }
        });
        row.appendChild(guide);
      }
      if (
        task.kind === 'queued_prompt' &&
        task.sessionId &&
        ACTIVE.has(task.status) &&
        task.status !== 'cancelling'
      ) {
        const cancel = el('button', 'work-cancel', '取消');
        cancel.type = 'button';
        cancel.addEventListener('click', async () => {
          cancel.disabled = true;
          try {
            await daemon.queueCancel(task.sessionId, task.id);
          } catch (e) {
            toast(`取消失敗：${e.message}`, false);
            cancel.disabled = false;
          }
        });
        row.appendChild(cancel);
      }
      root.appendChild(row);
    }
  }
  const active = tasks.filter(isLiveActiveTask).length;
  if (badge) {
    badge.hidden = active === 0;
    badge.textContent = active > 9 ? '9+' : String(active);
    badge.title = active ? `${active} 個進行中的背景工作` : '';
  }
  if (button) {
    button.classList.toggle('has-work', active > 0);
    button.setAttribute(
      'aria-label',
      active ? `進度，${active} 個進行中的背景工作` : '進度',
    );
  }
}

function renderSources() {
  const root = $('#rpSources');
  if (!root) return;
  root.textContent = '';
  if (!sources.length) {
    root.appendChild(el('div', 'rp-emptyrow', '尚無研究來源'));
    return;
  }
  for (const source of [...sources].reverse()) {
    const details = el('details', 'research-source');
    const summary = el('summary');
    summary.appendChild(el('strong', '', source.title || '研究來源'));
    summary.appendChild(el('span', 'source-host', hostOf(source.url)));
    details.appendChild(summary);
    const body = el('div', 'source-detail');
    const url = el('button', 'source-url', source.url);
    url.type = 'button';
    url.title = '在瀏覽器開啟來源';
    url.addEventListener('click', async () => {
      try {
        await daemon.openURL(source.url);
      } catch (e) {
        toast(`無法開啟來源：${e.message}`, false);
      }
    });
    body.appendChild(url);
    body.appendChild(
      el(
        'div',
        'source-meta',
        `${source.tool || 'research'}${source.callId ? ` · ${source.callId}` : ''}${
          source.observedAt ? ` · ${fmtTime(source.observedAt)}` : ''
        }`,
      ),
    );
    details.appendChild(body);
    root.appendChild(details);
  }
}

function hostOf(raw) {
  try {
    return new URL(raw).host;
  } catch {
    return raw || '';
  }
}

function openProgress() {
  showRightPane('rpPageProg');
}

function notifyTask(task) {
  // Pending chat messages are visible in the composer and Progress panel.
  // Completing one (including guidance) must not create a second floating
  // queue UI or native success notification. Real failures still notify.
  if (task.kind === 'queued_prompt' && task.status === 'completed') return;
  // Keep native notifications private by default: task titles may contain
  // customer names, document contents, or other lock-screen-sensitive text.
  const ok = task.status === 'completed';
  const toastMessage = `${task.title || '背景工作'} · ${statusLabel(task.status)}`;
  toast(toastMessage, ok);
  const privateMessage = ok
    ? '背景工作已完成，開啟 Meadow 查看結果。'
    : `背景工作已停止（${statusLabel(task.status)}），開啟 Meadow 查看詳情。`;
  void daemon
    .notify(
      ok ? 'Meadow 已完成背景工作' : 'Meadow 背景工作已停止',
      privateMessage,
    )
    .catch(() => {});
}

function notifyTransitions(next) {
  for (const task of next) {
    const previous = knownTaskStatus.get(task.id);
    knownTaskStatus.set(task.id, task.status);
    if (!previous || ACTIVE.has(task.status)) continue;
    if (!ACTIVE.has(previous)) continue;
    // A user-initiated cancellation already has immediate UI feedback; a
    // system notification for it is noisy and can arrive before the useful
    // completion notification of the next queued task.
    if (task.status === 'cancelled') continue;
    if (!hydratedSession) {
      // Preserve a real transition received over SSE while the initial task
      // snapshot is still in flight. It will be notified immediately after
      // hydration instead of being mistaken for old persisted history.
      pendingTerminalNotifications.set(task.id, task);
      pendingNotificationSession = state.sessionId || '';
      continue;
    }
    notifyTask(task);
  }
}

async function refreshTasks() {
  const sid = state.sessionId;
  if (!sid) return;
  try {
    const response = await daemon.taskList(sid);
    if (state.sessionId !== sid) return;
    if (response?.sessionId && response.sessionId !== sid) return;
    const next =
      response && Array.isArray(response.items)
        ? response.items.map((task) => ({ ...task, sessionId: sid }))
        : [];
    if (hydratedSession === sid) notifyTransitions(next);
    else {
      knownTaskStatus.clear();
      for (const task of next) knownTaskStatus.set(task.id, task.status);
      hydratedSession = sid;
      if (pendingNotificationSession === sid) {
        for (const task of pendingTerminalNotifications.values()) {
          notifyTask(task);
        }
      }
      pendingTerminalNotifications.clear();
      pendingNotificationSession = '';
    }
    tasks = next;
    renderTasks();
  } catch (e) {
    console.error('background task refresh failed:', e);
  }
}

async function refreshSources() {
  const sid = state.sessionId;
  if (!sid) return;
  try {
    const sourceResponse = await daemon.sourceList(sid);
    if (state.sessionId !== sid) return;
    sources =
      sourceResponse && Array.isArray(sourceResponse.items)
        ? sourceResponse.items
        : [];
    renderSources();
  } catch (e) {
    console.error('research source refresh failed:', e);
  }
}

export function initWorkCenter() {
  initTaskTodo();
  const button = $('#btnProgress');
  if (button) {
    button.hidden = false;
    button.disabled = false;
    button.addEventListener('click', openProgress);
  }
  bus.on('background_tasks', (data) => {
    if (!data || data.sessionId !== state.sessionId) return;
    const next = Array.isArray(data.items)
      ? data.items.map((task) => ({ ...task, sessionId: data.sessionId }))
      : [];
    notifyTransitions(next);
    tasks = next;
    renderTasks();
  });
  bus.on('research_sources', (data) => {
    if (!data || data.sessionId !== state.sessionId) return;
    sources = Array.isArray(data.items) ? data.items : [];
    renderSources();
  });
  // The product shell is intentionally mounted before authentication and
  // session creation so account/settings remain usable when login is closed.
  // Hydrate the session-owned drawers as soon as that deferred session exists;
  // the old boot order happened to call these refreshes only after creation.
  bus.on('session_created', () => {
    void refreshTasks();
    void refreshSources();
  });
  bus.on('work_center_error', (data) => {
    if (data?.sessionId && data.sessionId !== state.sessionId) return;
    toast(`背景工作無法安全儲存：${data?.message || '未知錯誤'}`, false);
  });
  bus.on('inference_progress', (data) => {
    if (!data || (data.sessionId && data.sessionId !== state.sessionId)) return;
    agentStage = String(data.phase || '');
    renderAgentActivity();
  });
  bus.on('update:tool_call', (tool) => {
    if (!tool || tool.sessionId !== state.sessionId) return;
    upsertAgentTool({ ...tool, status: tool.status || 'running' });
  });
  bus.on('update:tool_call_update', (tool) => {
    if (!tool || tool.sessionId !== state.sessionId) return;
    upsertAgentTool(tool);
  });
  bus.on('history:tools', (tools) => {
    agentTools = Array.isArray(tools) ? tools.map((tool) => ({ ...tool })) : [];
    agentStage = '';
    renderAgentActivity();
    renderBrowser();
  });
  bus.on('thread:switched', () => {
    // The right drawer describes the selected conversation, not every task in
    // the workspace. Clear synchronously so the previous session cannot remain
    // visible while the new session's snapshot is in flight.
    tasks = [];
    knownTaskStatus.clear();
    pendingTerminalNotifications.clear();
    pendingNotificationSession = '';
    hydratedSession = '';
    sources = [];
    agentTools = [];
    agentStage = '';
    renderTasks();
    renderSources();
    renderAgentActivity();
    renderBrowser();
    void refreshTasks();
    void refreshSources();
  });
  renderTasks();
  renderSources();
  renderAgentActivity();
  renderBrowser();
  void refreshTasks();
  void refreshSources();
  setInterval(() => void refreshTasks(), 2000);
}
