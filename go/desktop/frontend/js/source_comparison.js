// A server tool receipt, never a verdict inferred from the assistant's prose.
import { safeSourceURL } from './search_presentation.js?v=search-thinking-20260928a';

const labels = Object.freeze({
  literal_match: '原句找到', normalized_match: '忽略空白／大小寫後找到',
  quote_not_found: '未找到這段引文', no_quote: '未提供引文',
  unobserved: '本輪未讀取來源', unavailable: '來源無法讀取',
  source_changed: '原文已變更', range_unavailable: '引用範圍不存在',
  range_too_large: '超過本次比對容量',
});
const bounded = (value, max) => typeof value === 'string' && Array.from(value).length <= max ? value : null;
const words = (value) => Array.isArray(value) && value.length <= 8 && value.every(word => typeof word === 'string' && word.length <= 128) ? value : null;

export function sourceComparisonPresentation(output) {
  let value = output;
  if (typeof value === 'string') {
    if (value.length > 160 * 1024) return null;
    try { value = JSON.parse(value); } catch { return null; }
  }
  if (!value || value.type !== 'source_comparison' || value.version !== 1 ||
      !Array.isArray(value.checks) || value.checks.length < 1 || value.checks.length > 12) return null;
  const checks = [];
  let totalText = 0;
  for (const row of value.checks) {
    if (!row || !Object.hasOwn(labels, row.status) || !bounded(row.claim, 300) ||
        !bounded(row.source_id, 160) || !Number.isSafeInteger(row.start_line) ||
        !Number.isSafeInteger(row.end_line) || row.start_line < 1 ||
        row.end_line < row.start_line || row.end_line - row.start_line >= 80 ||
        bounded(row.quote, 600) === null || bounded(row.source_text || '', 4000) === null ||
        words(row.matched_keywords) === null || words(row.missing_keywords) === null) return null;
    totalText += Array.from(row.source_text || '').length;
    if (totalText > 24000) return null;
    checks.push({
      claim: row.claim, sourceId: row.source_id, startLine: row.start_line, endLine: row.end_line,
      quote: row.quote, sourceText: row.source_text || '', status: row.status, label: labels[row.status],
      name: bounded(row.source_name || '', 4096) || row.source_id,
      url: safeSourceURL(row.source_url), fetchedAt: bounded(row.fetched_at || '', 100) || '',
      matched: row.matched_keywords, missing: row.missing_keywords,
      warning: bounded(row.warning || '', 500) || '',
    });
  }
  const found = checks.filter(row => ['literal_match', 'normalized_match'].includes(row.status)).length;
  return { checks, summary: `來源比對 · ${checks.length} 項 · 文字找到 ${found} 項`,
    notice: '文字吻合不代表主張正確；請核對否定、條件、日期與適用範圍。未找到也可能是所選範圍不足。',
    coordinateScope: '下列 L 座標是抽取文字行，並非 PDF 原頁碼或 Excel 儲存格。' };
}

const node = (tag, cls, text) => {
  const element = document.createElement(tag);
  if (cls) element.className = cls;
  if (text != null) element.textContent = text;
  return element;
};

export function createSourceComparison(report) {
  if (!report) return null;
  const root = node('details', 'source-comparison');
  root.append(node('summary', '', report.summary));
  root.append(node('p', 'source-comparison-notice', report.notice));
  root.append(node('p', 'source-comparison-scope', report.coordinateScope));
  for (const row of report.checks) {
    const item = node('article', 'source-comparison-item');
    item.append(node('h4', '', row.claim), node('div', 'source-comparison-status', row.label));
    const source = node('div', 'source-comparison-location');
    if (row.url) {
      const link = node('a', '', row.name); link.href = row.url;
      link.target = '_blank'; link.rel = 'noopener noreferrer'; source.append(link);
    } else source.append(node('span', '', row.name));
    source.append(node('span', '', ` · 抽取 L${row.startLine}–L${row.endLine}`));
    item.append(source);
    if (row.fetchedAt) item.append(node('p', 'source-comparison-scope', `取得時間：${row.fetchedAt}`));
    const pair = node('div', 'source-comparison-pair');
    for (const [label, text] of [['模型引用', row.quote || '未提供'], ['實際原文', row.sourceText || '未查核／無可用原文']]) {
      const column = node('div', 'source-comparison-column');
      column.append(node('div', 'source-comparison-caption', label), node('pre', '', text)); pair.append(column);
    }
    item.append(pair);
    if (row.matched.length) item.append(node('p', 'source-comparison-keywords', `找到關鍵字：${row.matched.join('、')}`));
    if (row.missing.length) item.append(node('p', 'source-comparison-keywords missing', `未找到關鍵字：${row.missing.join('、')}`));
    if (row.warning) item.append(node('p', 'source-comparison-notice', row.warning));
    root.append(item);
  }
  return root;
}
