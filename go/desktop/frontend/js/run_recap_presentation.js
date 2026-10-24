// Factual end-of-run receipts. These labels never evaluate the model's answer
// or infer that the requested task is complete from successful tool transport.
const RUN_LABELS = Object.freeze({
  completed: '執行已結束', partial: '部分完成', failed: '未完成',
  cancelled: '已取消', needs_input: '等待回答',
});
const CHECK_LABELS = Object.freeze({
  passed: '通過', failed: '未通過', unverified: '未驗證',
  stale: '舊結果已過期', needs_human_review: '待人工驗收',
  superseded: '模型已申報更正（不代表通過）',
});
const HANDOFF_REASONS = Object.freeze({
  work_time_reserve: '工作時間預留', work_token_reserve: '工作 token 預留',
  advisor_work_reserve: '工作資源預留', model_turn_limit: '執行步數上限',
});
const BOUNDED_STOPS = new Set([
  'time_budget', 'token_budget', 'max_iterations', 'token_limit', 'incomplete',
]);
const text = (value, limit = 300) => {
  if (typeof value !== 'string') return '';
  let result = '', length = 0;
  for (const character of value) {
    if (length++ === limit) return result + '…';
    result += character;
  }
  return result;
};
const count = (value) => Number.isSafeInteger(value) && value >= 0 ? value : 0;
const list = (value) => Array.isArray(value) ? value : [];

// Only traverse bounded fragments of structured assertion values. An omitted
// or truncated value remains visible as such; it is never filled in by the UI.
function assertionValue(value, depth = 0) {
  if (value === undefined) return '未記錄';
  if (typeof value === 'string') return text(value, 512);
  if (value == null || typeof value === 'boolean' || typeof value === 'number') {
    return JSON.stringify(value);
  }
  if (depth >= 3) return '…';
  if (Array.isArray(value)) {
    const items = value.slice(0, 8).map((item) => assertionValue(item, depth + 1));
    if (value.length > 8) items.push('…');
    return text(`[${items.join(', ')}]`, 512);
  }
  if (typeof value === 'object') {
    const items = Object.entries(value).slice(0, 8)
      .map(([key, item]) => `${text(key, 80)}: ${assertionValue(item, depth + 1)}`);
    if (Object.keys(value).length > 8) items.push('…');
    return text(`{${items.join(', ')}}`, 512);
  }
  return '未記錄';
}

function checkPresentation(check) {
  if (!check || typeof check !== 'object') return null;
  const evidence = check.evidence && typeof check.evidence === 'object' ? check.evidence : null;
  const observed = CHECK_LABELS[check.observedStatus] ||
    (['passed', 'failed'].includes(check.status) ? CHECK_LABELS[check.status] : '');
  let label = CHECK_LABELS[check.status] || '狀態未記錄';
  if (check.status !== 'superseded') {
    if (check.status === 'stale' || evidence?.freshness === 'stale') {
      label = `舊結果已過期${observed ? `（原結果${observed}）` : ''}`;
    } else if (evidence && evidence.freshness !== 'current') {
      label += ` · 版本依據不明${observed ? `（原結果${observed}）` : ''}`;
    } else if (check.status === 'passed' && !evidence) {
      label += '（舊紀錄未提供版本依據）';
    } else if (evidence?.freshness === 'current') {
      label += ' · 所列檔案未變更';
    }
  }
  const details = [];
  if (text(check.callId, 160)) details.push(`呼叫 · ${text(check.callId, 160)}`);
  if (evidence) {
    details.push('版本依據只涵蓋所列檔案；不代表測試涵蓋完整需求或瀏覽器目前狀態。');
    if (text(evidence.reason)) details.push(`版本說明 · ${text(evidence.reason)}`);
    if (text(evidence.scope, 600)) details.push(`檢查範圍 · ${text(evidence.scope, 600)}`);
    for (const criterion of list(evidence.criteria).slice(0, 8)) {
      if (text(criterion)) details.push(`申報驗收條件 · ${text(criterion)}`);
    }
    const inputs = list(evidence.inputs);
    for (const input of inputs.slice(0, 8)) {
      if (input && text(input.path)) {
        details.push(`檔案 · ${text(input.path)} · SHA256 ${text(input.sha256, 64) || '未記錄'}`);
      }
    }
    if (inputs.length > 8) details.push(`另 ${inputs.length - 8} 個檔案未在摘要展開`);
    if (count(evidence.declaredInputCount) > inputs.length) {
      details.push(`申報 ${count(evidence.declaredInputCount)} 個檔案；只收到 ${inputs.length} 個版本紀錄`);
    }
    const assertions = list(evidence.assertions);
    for (const assertion of assertions.slice(0, 8)) {
      if (!assertion || typeof assertion !== 'object') continue;
      const location = [text(assertion.scenarioId, 80), text(assertion.action, 80),
        text(assertion.path || assertion.selector, 160)].filter(Boolean).join(' · ');
      details.push(`實際斷言 · ${location || '未記錄位置'} · ${CHECK_LABELS[assertion.status] || text(assertion.status, 80) || '狀態未記錄'}`);
      for (const [field, caption] of [['expected', '預期'], ['actual', '實際']]) {
        details.push(`${caption} · ${Object.hasOwn(assertion, field) ? assertionValue(assertion[field]) : '未記錄'}`);
      }
      if (assertion.valuesOmitted === true) details.push('部分斷言值已省略；摘要不足以重建完整結果。');
    }
    if (assertions.length > 8) details.push(`另 ${assertions.length - 8} 個斷言未在摘要展開`);
    const document = evidence.document;
    if (document && typeof document === 'object') {
      details.push('Browser 文件 · 執行時的識別紀錄；目前文件狀態未確認');
      for (const [field, caption] of [['pageId', 'Page'], ['documentId', 'Document'], ['backend', 'Backend'], ['url', 'URL']]) {
        if (text(document[field])) details.push(`${caption} · ${text(document[field])}`);
      }
    }
    if (text(evidence.reportPath)) details.push(`報告 · ${text(evidence.reportPath)}`);
  }
  return { tool: text(check.tool, 120) || '未記錄工具', label,
    summary: text(check.summary), details, stale: check.status === 'stale' || evidence?.freshness === 'stale' };
}

export function runRecapPresentation(recap) {
  if (!recap || typeof recap !== 'object') return null;
  const checks = list(recap.checks).slice(0, 12).map(checkPresentation).filter(Boolean);
  const handoff = recap.handoff && typeof recap.handoff === 'object' ? recap.handoff : null;
  if (!count(recap.toolCalls) && !recap.status && !recap.stopReason && !handoff && !checks.length) return null;
  let status = RUN_LABELS[recap.status] || '狀態未記錄';
  if (recap.stopReason === 'cancelled') status = '已取消';
  else if (BOUNDED_STOPS.has(recap.stopReason) || handoff) status = '工作尚未完成';
  const lines = [`工具 ${count(recap.toolCalls)} 次 · 成功 ${count(recap.successfulTools)} · 失敗 ${count(recap.failedTools)}`];
  if (list(recap.changedPaths).length) lines.push(`變更 ${recap.changedPaths.length} 個檔案`);
  if (count(recap.invalidatedChecks)) lines.push(`驗證版本失效 ${count(recap.invalidatedChecks)} 次`);
  if (handoff) {
    const reason = HANDOFF_REASONS[handoff.reason] || text(handoff.reason, 120) || '原因未記錄';
    const delivery = handoff.received === true ? '已收到模型收尾回覆' :
      handoff.attempted === true ? '已發出收尾請求，未收到回覆' : '未發出收尾請求';
    lines.push(`預留收尾 · ${delivery} · ${reason}`);
    if (handoff.truncated === true) lines.push('收尾回覆被截斷；交付內容可能不完整');
    if (handoff.toolCallRejected === true) lines.push('收尾階段的工具請求已拒絕，未執行');
    lines.push('收到收尾回覆不代表任務或驗收完成。');
  }
  if (count(recap.recoveredFailures)) lines.push(`根據錯誤更換策略並恢復 ${count(recap.recoveredFailures)} 個失敗`);
  const outstanding = list(recap.outstanding).slice(0, 3).filter(Boolean)
    .map((issue) => `尚未解決 · ${text(issue.tool, 120) || '未記錄工具'}${text(issue.summary) ? ` · ${text(issue.summary)}` : ''}`);
  return { title: `執行摘要 · ${status}`, lines, checks, outstanding,
    omittedChecks: Math.max(0, list(recap.checks).length - 12) };
}

const node = (tag, cls, value) => {
  const element = document.createElement(tag);
  if (cls) element.className = cls;
  if (value != null) element.textContent = value;
  return element;
};

export function createRunRecapCard(report, corrections = null) {
  if (!report) return null;
  const card = node('div', 'editcard run-recap');
  const head = node('div', 'ec-head');
  head.append(node('span', 'ec-title', report.title));
  card.append(head);
  const addLine = (value, parent = card) => {
    const row = node('div', 'ec-file run-recap-line');
    row.append(node('span', 'fl-tx', value));
    parent.append(row);
  };
  for (const line of report.lines) addLine(line);
  if (corrections) card.append(corrections);
  for (const check of report.checks) {
    addLine(`驗證 ${check.tool} · ${check.label}${check.summary ? ` · ${check.summary}` : ''}`);
    if (check.details.length) {
      const details = node('details', 'tool-commentary run-recap-evidence');
      details.append(node('summary', '', '查看版本與驗收依據'));
      for (const line of check.details) addLine(line, details);
      card.append(details);
    }
  }
  if (report.omittedChecks) addLine(`另 ${report.omittedChecks} 項檢查未在摘要展開`);
  for (const issue of report.outstanding) addLine(issue);
  return card;
}
