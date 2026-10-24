// Foreground, model-owned plans. Only the committed daemon snapshot is rendered;
// streamed tool arguments, prose, and run completion never mutate this list.
import { daemon, bus, state } from './core.js?v=raven-execution-20261006a';

const ACTIVE_STATUSES = new Set(['pending', 'in_progress']);
const STATUS_LABELS = {
  pending: '待辦',
  in_progress: '進行中',
  completed: '模型標記完成',
  cancelled: '已取消',
};

let initialized = false;
let selectedSession = '';
let generation = 0;
let requestSequence = 0;
let controller = null;
let refreshTimer = null;
let snapshot = null;
let loading = false;
let refreshError = '';
let liveRun = null;
let runEventRevision = 0;
let cardExpanded = true;
const expandedItems = new Set();

function text(value) {
  return typeof value === 'string' ? value : '';
}

function strings(value) {
  return Array.isArray(value) ? value.filter((item) => typeof item === 'string') : [];
}

function node(tag, className, value) {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (value != null) element.textContent = value;
  return element;
}

function fact(parent, label, value) {
  if (!value) return;
  const line = node('div', 'task-todo-fact');
  line.append(node('span', 'task-todo-fact-label', label));
  line.append(node('span', '', value));
  parent.append(line);
}

function receiptSucceeded(receipt) {
  // Committed evidence uses the executor ToolStatus vocabulary. The streamed
  // tool UI's "completed" status is not a successful execution receipt.
  return receipt?.executed === true && receipt.status === 'ok';
}

function receiptLabel(receipt) {
  if (!receipt) return '找不到對應工具紀錄';
  if (receipt.executed !== true) return '未執行／僅有紀錄';
  if (!receiptSucceeded(receipt)) return '已執行；檢查未通過／結果未確認';
  if (receipt.scope === 'executed-checks-passed') return '歷史工具檢查通過，來源版本未重驗';
  return '已執行；未作需求驗證';
}

function localArtifactPath(value) {
  let path = text(value);
  if (!path || /[\u0000-\u001f\\]/.test(path)) return '';
  if (/^file:/i.test(path)) {
    try {
      const url = new URL(path);
      if (url.protocol !== 'file:' || (url.hostname && url.hostname !== 'localhost') || url.search || url.hash) return '';
      path = decodeURIComponent(url.pathname);
    } catch {
      return '';
    }
  } else if (/^[a-z][a-z0-9+.-]*:/i.test(path)) return '';
  if (/[%?#\u0000-\u001f\\]/.test(path) || path.startsWith('//') || path.split('/').some((part) => part === '..' || part === '.')) return '';
  if (!path.startsWith('/')) return path;
  if (path.startsWith('/workspace/')) return path;
  const cwd = text(state.cwd).replace(/\/$/, '');
  return cwd && path.startsWith(`${cwd}/`) ? path : '';
}

function renderEvidence(parent, item, evidence) {
  const ids = strings(item.evidence_ids);
  const title = node('div', 'task-todo-evidence-title', '工具證據');
  parent.append(title);
  if (!ids.length) {
    parent.append(node('p', 'task-todo-note', '模型未附工具證據。'));
    return;
  }
  for (const id of ids) {
    const receipt = evidence.get(id);
    const block = node('div', 'task-todo-receipt');
    block.dataset.evidenceId = id;
    block.dataset.scope = receipt ? text(receipt.scope) : 'unresolved';
    const heading = node('div', 'task-todo-receipt-head');
    heading.append(node('code', '', id));
    heading.append(node('span', 'task-todo-receipt-status', receiptLabel(receipt)));
    block.append(heading);
    if (receipt) {
      fact(block, '工具', text(receipt.toolName));
      fact(block, '紀錄狀態', text(receipt.status));
      fact(block, '來源', text(receipt.sourcePath));
      fact(block, '來源 SHA-256', text(receipt.sourceSha256));
      fact(block, '對應需求', strings(receipt.requirementIds).join('、'));
      if (receipt.scope === 'executed-checks-passed' && receiptSucceeded(receipt)) {
        block.append(node('p', 'task-todo-note', '通過範圍僅限此工具的檢查，不代表整項需求已驗證。'));
      }
    }
    parent.append(block);
  }
}

function renderItem(item, index, itemsById, evidence) {
  const status = text(item.status);
  const row = node('li', 'task-todo-item');
  row.dataset.taskId = text(item.id);
  row.dataset.status = status;
  row.append(node('span', 'task-todo-order', String(index + 1)));
  const body = node('div', 'task-todo-copy');
  const heading = node('div', 'task-todo-item-head');
  heading.append(node('strong', 'task-todo-content', text(item.content)));
  heading.append(node('span', 'task-todo-status', STATUS_LABELS[status] || '未知狀態'));
  body.append(heading);
  if (text(item.success_criteria)) {
    fact(body, '驗收條件', text(item.success_criteria));
  }
  const dependencies = strings(item.depends_on);
  const unresolved = dependencies.filter((id) => {
    const dependency = itemsById.get(id);
    return !dependency || dependency.status !== 'completed';
  });
  if (ACTIVE_STATUSES.has(status) && unresolved.length) {
    body.append(node('p', 'task-todo-dependency', `等待前置項目：${unresolved.join('、')}`));
  }
  const details = node('details', 'task-todo-details');
  const itemId = text(item.id);
  const itemGeneration = generation;
  details.open = expandedItems.has(itemId);
  details.addEventListener('toggle', () => {
    if (itemGeneration !== generation || !details.isConnected) return;
    if (details.open) expandedItems.add(itemId);
    else expandedItems.delete(itemId);
  });
  details.append(node('summary', '', '項目與證據'));
  const detailBody = node('div', 'task-todo-detail-body');
  fact(detailBody, '項目 ID', text(item.id));
  fact(detailBody, '前置項目', dependencies.join('、'));
  fact(detailBody, '對應需求', strings(item.requirement_ids).join('、'));
  const artifacts = strings(item.artifacts);
  if (artifacts.length) {
    const paths = node('div', 'task-todo-artifacts');
    paths.append(node('span', 'task-todo-fact-label', '產出／來源'));
    for (const artifact of artifacts) {
      const localPath = localArtifactPath(artifact);
      if (!localPath) {
        paths.append(node('code', '', artifact));
        continue;
      }
      const open = node('button', 'task-todo-artifact-open', artifact);
      open.type = 'button';
      open.title = '開啟工作區檔案';
      open.addEventListener('click', () => {
        if (itemGeneration !== generation || selectedSession !== String(state.sessionId || '')) return;
        // The existing authenticated preview route owns workspace confinement,
        // permission checks and existence; this checklist grants no new access.
        bus.emit('preview:open', { path: localPath });
      });
      paths.append(open);
    }
    detailBody.append(paths);
  }
  renderEvidence(detailBody, item, evidence);
  details.append(detailBody);
  body.append(details);
  row.append(body);
  return row;
}

// Keep the reference-style card close to the input, and reserve the full
// criteria/evidence view for Progress. Both come from one committed snapshot.
function renderCard() {
  const root = document.querySelector('#taskTodo');
  if (!root) return;
  const focused = root.contains(document.activeElement)
    ? document.activeElement.dataset.todoFocus : '';
  const scroller = root.querySelector('.task-todo-card-body');
  const scrollTop = scroller?.scrollTop || 0;
  root.replaceChildren();
  root.hidden = !selectedSession || !snapshot || (snapshot.plan.revision === 0 && !snapshot.plan.items.length && !text(snapshot.restoreWarning));
  root.setAttribute('aria-busy', String(loading));
  if (root.hidden) return;
  const { plan } = snapshot;
  const items = plan.items;
  const completed = items.filter((item) => item.status === 'completed').length;
  const cancelled = items.filter((item) => item.status === 'cancelled').length;
  const pending = items.filter((item) => ACTIVE_STATUSES.has(item.status)).length;
  const head = node('div', 'task-todo-card-head');
  const toggle = node('button', 'task-todo-card-toggle');
  toggle.type = 'button';
  toggle.dataset.todoFocus = 'toggle';
  toggle.setAttribute('aria-expanded', String(cardExpanded));
  toggle.setAttribute('aria-controls', 'taskTodoItems');
  toggle.title = cardExpanded ? '收合任務清單' : '展開任務清單';
  const titles = node('span', 'task-todo-card-titles');
  titles.append(node('strong', 'task-todo-card-name', text(plan.goal) || '本輪任務'));
  const counts = node('span', 'task-todo-card-counts', `已完成 ${completed} / ${items.length} 項${cancelled ? ` · ${cancelled} 項取消` : ''}`);
  counts.title = '完成數量依模型儲存的標記；工具證據可在進度中查看。';
  titles.append(counts);
  toggle.append(titles);
  const chevron = node('span', 'task-todo-card-chevron', cardExpanded ? '⌃' : '⌄');
  chevron.setAttribute('aria-hidden', 'true');
  toggle.append(chevron);
  toggle.addEventListener('click', () => {
    cardExpanded = !cardExpanded;
    renderCard();
  });
  head.append(toggle);
  root.append(head);
  const body = node('div', 'task-todo-card-body');
  body.id = 'taskTodoItems';
  body.hidden = !cardExpanded;
  if (text(snapshot.restoreWarning)) body.append(node('p', 'task-todo-card-note task-todo-card-error', text(snapshot.restoreWarning)));
  if (refreshError) body.append(node('p', 'task-todo-card-note task-todo-card-error', '更新失敗，顯示上次已儲存清單。'));
  const run = liveRun || snapshot.run || {};
  if (!run.running && pending && (text(run.stopReason) || text(run.runId))) {
    body.append(node('p', 'task-todo-card-note', `本輪已停止，${pending} 項待接續。`));
  }
  const list = node('ol', 'task-todo-card-list');
  for (const [index, item] of items.entries()) {
    const row = node('li', 'task-todo-card-item');
    row.dataset.taskId = text(item.id);
    row.dataset.status = text(item.status);
    const icon = node('span', 'task-todo-card-status', item.status === 'completed' ? '✓' : item.status === 'cancelled' ? '−' : '');
    icon.setAttribute('role', 'img');
    icon.setAttribute('aria-label', STATUS_LABELS[item.status] || '未知狀態');
    icon.title = STATUS_LABELS[item.status] || '未知狀態';
    row.append(icon, node('span', 'task-todo-card-order', `${index + 1}.`), node('span', 'task-todo-card-content', text(item.content)));
    list.append(row);
  }
  body.append(list);
  if (!items.length) body.append(node('p', 'task-todo-card-note', plan.revision > 0 ? '模型已清空任務清單。' : '目前沒有可載入的已儲存清單。'));
  const inspect = node('button', 'task-todo-card-inspect', '查看驗收與證據');
  inspect.type = 'button';
  inspect.dataset.todoFocus = 'inspect';
  inspect.addEventListener('click', () => {
    document.querySelector('#btnProgress')?.click();
    document.querySelector('#rpTaskTodoTitle')?.scrollIntoView({ block: 'nearest' });
  });
  body.append(inspect);
  root.append(body);
  body.scrollTop = scrollTop;
  if (focused) root.querySelector(`[data-todo-focus="${focused}"]`)?.focus({ preventScroll: true });
}

function renderProgress() {
  const root = document.querySelector('#rpTaskTodo');
  if (!root) return;
  const focusedTask = root.contains(document.activeElement)
    ? document.activeElement.closest('[data-task-id]')?.dataset.taskId : '';
  const focusedTag = document.activeElement?.tagName;
  root.replaceChildren();
  root.setAttribute('aria-busy', String(loading));
  if (!selectedSession) {
    root.append(node('p', 'rp-emptyrow', '開啟對話後顯示模型的任務清單。'));
    return;
  }
  if (loading && !snapshot) {
    root.append(node('p', 'rp-emptyrow', '正在載入任務清單…'));
    return;
  }
  if (refreshError) {
    const error = node('div', 'task-todo-error');
    error.append(node('span', '', snapshot ? '清單更新失敗；以下為上次已儲存內容。' : '任務清單載入失敗。'));
    const retry = node('button', 'task-todo-retry', '重試');
    retry.type = 'button';
    retry.addEventListener('click', () => void refresh());
    error.append(retry);
    root.append(error);
  }
  if (!snapshot) return;
  if (text(snapshot.restoreWarning)) root.append(node('p', 'task-todo-error', text(snapshot.restoreWarning)));
  const plan = snapshot.plan;
  const items = plan.items;
  const active = items.filter((item) => ACTIVE_STATUSES.has(item.status));
  const completed = items.filter((item) => item.status === 'completed').length;
  const cancelled = items.filter((item) => item.status === 'cancelled').length;
  const heading = node('div', 'task-todo-heading');
  if (text(plan.goal)) heading.append(node('p', 'task-todo-goal', text(plan.goal)));
  const counts = items.length
    ? `${active.length} 項未完成 · ${completed} 項模型標記完成${cancelled ? ` · ${cancelled} 項取消` : ''}`
    : '';
  if (counts) heading.append(node('p', 'task-todo-counts', counts));
  if (plan.revision > 0) heading.append(node('span', 'task-todo-revision', `已儲存版本 ${plan.revision}`));
  root.append(heading);
  if (!items.length) {
    root.append(node('p', 'rp-emptyrow', plan.revision > 0 ? '模型已清空任務清單。' : '模型尚未建立任務清單；可依任務需要自行使用。'));
    return;
  }
  const run = liveRun || snapshot.run || {};
  if (!run.running && active.length && (text(run.stopReason) || text(run.runId))) {
    root.append(node('p', 'task-todo-stopped', `本輪已停止；${active.length} 項仍未完成。清單保留供後續接續。`));
  }
  const current = items.filter((item) => item.status === 'in_progress');
  if (current.length) root.append(node('p', 'task-todo-current', `目前進行：${current.map((item) => text(item.content)).join('；')}`));
  const itemsById = new Map(items.map((item) => [text(item.id), item]));
  const evidence = new Map((snapshot.evidence || []).map((receipt) => [text(receipt.callId), receipt]));
  const list = node('ol', 'task-todo-list');
  items.forEach((item, index) => list.append(renderItem(item, index, itemsById, evidence)));
  root.append(list);
  root.append(node('p', 'task-todo-note task-todo-footer', '清單由模型管理；完成標記與工具驗證分別顯示。'));
  if (focusedTask && focusedTag === 'SUMMARY') {
    const row = Array.from(root.querySelectorAll('[data-task-id]')).find((item) => item.dataset.taskId === focusedTask);
    row?.querySelector('summary')?.focus({ preventScroll: true });
  }
}

function render() {
  renderCard();
  renderProgress();
}

async function refresh() {
  const sessionId = String(state.sessionId || '');
  if (!sessionId || sessionId !== selectedSession) return;
  const currentGeneration = generation;
  const currentRunRevision = runEventRevision;
  const sequence = ++requestSequence;
  controller?.abort();
  const requestController = new AbortController();
  controller = requestController;
  loading = true;
  refreshError = '';
  render();
  try {
    const response = await daemon.taskPlan(sessionId, requestController.signal);
    if (generation !== currentGeneration || sequence !== requestSequence || state.sessionId !== sessionId) return;
    if (response?.ok !== true || response.sessionId !== sessionId || !response.plan || !Array.isArray(response.plan.items)) {
      throw new Error('invalid committed task plan snapshot');
    }
    snapshot = response;
    // An earlier GET must not undo a terminal event received while it was in
    // flight. The next refresh can reconcile the authoritative run snapshot.
    if (currentRunRevision === runEventRevision) liveRun = null;
  } catch (error) {
    if (generation !== currentGeneration || sequence !== requestSequence || error?.name === 'AbortError') return;
    refreshError = 'failed';
    console.error('task plan refresh failed:', error);
  } finally {
    if (generation === currentGeneration && sequence === requestSequence) {
      controller = null;
      loading = false;
      render();
    }
  }
}

function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => {
    refreshTimer = null;
    void refresh();
  }, 40);
}

function selectSession() {
  generation += 1;
  requestSequence += 1;
  controller?.abort();
  controller = null;
  clearTimeout(refreshTimer);
  refreshTimer = null;
  selectedSession = String(state.sessionId || '');
  snapshot = null;
  liveRun = null;
  runEventRevision = 0;
  loading = false;
  refreshError = '';
  expandedItems.clear();
  cardExpanded = true;
  render();
  void refresh();
}

function isSelected(data) {
  return data && data.sessionId === selectedSession && selectedSession === String(state.sessionId || '');
}

export function initTaskTodo() {
  if (initialized) return;
  initialized = true;
  bus.on('thread:switched', selectSession);
  bus.on('session_created', selectSession);
  bus.on('task_plan_update', (data) => {
    if (isSelected(data)) scheduleRefresh();
  });
  bus.on('update:tool_call_update', (tool) => {
    if (isSelected(tool) && ['completed', 'success'].includes(tool.status)) scheduleRefresh();
  });
  bus.on('run_state', (data) => {
    if (!isSelected(data)) return;
    runEventRevision += 1;
    liveRun = {
      running: ['running', 'cancelling'].includes(data.status),
      stopReason: text(data.stopReason) || (data.status === 'running' ? '' : text(data.status)),
      runId: text(data.runId),
    };
    render();
    scheduleRefresh();
  });
  bus.on('sse_open', (data) => {
    if (isSelected(data)) scheduleRefresh();
  });
  bus.on('turn_completed', (data) => {
    if (isSelected(data)) scheduleRefresh();
  });
  bus.on('turn:done', scheduleRefresh);
  bus.on('history:tools', scheduleRefresh);
  document.querySelector('#btnProgress')?.addEventListener('click', scheduleRefresh);
  selectSession();
}
