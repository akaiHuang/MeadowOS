import assert from 'node:assert/strict';
import test from 'node:test';
import { runRecapPresentation, createRunRecapCard } from './run_recap_presentation.js';

const recap = (extra = {}) => ({ status: 'completed', stopReason: 'completed',
  toolCalls: 2, successfulTools: 2, failedTools: 0, ...extra });
const check = (extra = {}) => ({ tool: 'run_shell_command', callId: 'call-1',
  status: 'passed', summary: '2 tests passed', ...extra });

test('legacy checks keep the observed verdict without inventing current file evidence', () => {
  const report = runRecapPresentation(recap({ checks: [check()] }));
  assert.equal(report.title, '執行摘要 · 執行已結束');
  assert.match(report.checks[0].label, /^通過/);
  assert.match(report.checks[0].label, /舊紀錄未提供版本依據/);
  assert.doesNotMatch(report.checks[0].details.join('\n'), /所列檔案未變更/);
  assert.equal(runRecapPresentation({}), null);
  assert.equal(runRecapPresentation(null), null);
});

test('stale, failed, unknown and human acceptance are separate factual states', () => {
  const report = runRecapPresentation(recap({ checks: [
    check({ status: 'stale', observedStatus: 'passed', evidence: { freshness: 'stale', reason: 'file changed' } }),
    check({ status: 'failed', evidence: { freshness: 'current' } }),
    check({ status: 'unverified', observedStatus: 'passed', evidence: { freshness: 'unknown' } }),
    check({ status: 'needs_human_review' }),
    check({ status: 'failed', evidence: { freshness: 'stale' } }),
  ], invalidatedChecks: 2 }));
  assert.equal(report.checks[0].label, '舊結果已過期（原結果通過）');
  assert.equal(report.checks[0].stale, true);
  assert.match(report.checks[1].label, /^未通過/);
  assert.match(report.checks[2].label, /未驗證 · 版本依據不明（原結果通過）/);
  assert.equal(report.checks[3].label, '待人工驗收');
  assert.equal(report.checks[4].label, '舊結果已過期（原結果未通過）');
  assert.ok(report.lines.includes('驗證版本失效 2 次'));
});

test('current declared bytes and assertion values do not attest present Browser state', () => {
  const report = runRecapPresentation(recap({ checks: [check({ evidence: {
    freshness: 'current', inputs: [{ path: '/workspace/counter.html', sha256: 'a'.repeat(64) }],
    criteria: ['Add twice yields two'], assertions: [{ action: 'assert_text', selector: '#count',
      status: 'passed', expected: '2', actual: '2' }],
    document: { pageId: 'p-1', documentId: 'd-1', freshness: 'unknown', url: 'https://example.com' },
  } })] }));
  const result = report.checks[0];
  assert.match(result.label, /所列檔案未變更/);
  assert.ok(result.details.includes('預期 · 2'));
  assert.ok(result.details.includes('實際 · 2'));
  assert.ok(result.details.some((line) => /申報驗收條件/.test(line)));
  assert.ok(result.details.some((line) => /目前文件狀態未確認/.test(line)));
  assert.ok(result.details.includes('Document · d-1'));
  assert.ok(result.details.some((line) => /不代表測試涵蓋完整需求/.test(line)));
});

test('zero-tool reserved handoff renders incomplete work and receipt separately', () => {
  const report = runRecapPresentation({ toolCalls: 0, stopReason: 'incomplete',
    handoff: { reason: 'work_time_reserve', attempted: true, received: true,
      truncated: false, toolCallRejected: false } });
  assert.equal(report.title, '執行摘要 · 工作尚未完成');
  assert.ok(report.lines.includes('工具 0 次 · 成功 0 · 失敗 0'));
  assert.ok(report.lines.some((line) => /已收到模型收尾回覆/.test(line)));
  assert.ok(report.lines.includes('收到收尾回覆不代表任務或驗收完成。'));
});

test('user Stop never invents a handoff attempt or receipt', () => {
  const report = runRecapPresentation({ status: 'cancelled', stopReason: 'cancelled', toolCalls: 0 });
  assert.equal(report.title, '執行摘要 · 已取消');
  assert.doesNotMatch(report.lines.join('\n'), /收尾/);
  const stoppedAdmission = runRecapPresentation(recap({ stopReason: 'cancelled',
    handoff: { attempted: false, received: false, reason: 'work_time_reserve' } }));
  assert.equal(stoppedAdmission.title, '執行摘要 · 已取消');
  assert.ok(stoppedAdmission.lines.some((line) => /未發出收尾請求/.test(line)));
  assert.doesNotMatch(stoppedAdmission.lines.join('\n'), /已收到模型收尾回覆/);
});

test('failed, truncated and rejected-tool handoffs retain their practical limits', () => {
  const report = runRecapPresentation(recap({ stopReason: 'incomplete',
    handoff: { reason: 'work_token_reserve', attempted: true, received: false,
      truncated: true, toolCallRejected: true } }));
  assert.ok(report.lines.some((line) => /已發出收尾請求，未收到回覆/.test(line)));
  assert.ok(report.lines.some((line) => /被截斷/.test(line)));
  assert.ok(report.lines.some((line) => /工具請求已拒絕，未執行/.test(line)));
});

test('evidence details are bounded and omitted or missing assertion values stay visible', () => {
  const report = runRecapPresentation(recap({ checks: Array.from({ length: 30 }, () => check({ evidence: {
    freshness: 'unknown', declaredInputCount: 100,
    inputs: Array.from({ length: 10 }, (_, index) => ({ path: `file-${index}`, sha256: 'f'.repeat(64) })),
    criteria: Array.from({ length: 80 }, () => 'criterion'),
    assertions: Array.from({ length: 20 }, () => ({ action: 'assert_text', actual: 'x'.repeat(2000), valuesOmitted: true })),
  } })) }));
  assert.equal(report.checks.length, 12);
  assert.equal(report.omittedChecks, 18);
  const details = report.checks[0].details;
  assert.equal(details.filter((line) => line.startsWith('申報驗收條件')).length, 8);
  assert.equal(details.filter((line) => line.startsWith('檔案 ·')).length, 8);
  assert.equal(details.filter((line) => line.startsWith('實際斷言')).length, 8);
  assert.ok(details.includes('預期 · 未記錄'));
  assert.ok(details.some((line) => /部分斷言值已省略/.test(line)));
  assert.ok(details.every((line) => line.length < 700));
});

class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.className = ''; this.textContent = ''; }
  append(...children) { this.children.push(...children); }
  set innerHTML(_) { throw new Error('Evidence must never be parsed as HTML'); }
  get allText() { return this.textContent + this.children.map((child) => child.allText).join('\n'); }
}

test('actual card rendering keeps untrusted markup inert and evidence collapsed', () => {
  const previous = globalThis.document;
  globalThis.document = { createElement: (tag) => new Element(tag) };
  try {
    const markup = '<img src=x onerror=alert(1)>';
    const report = runRecapPresentation(recap({ checks: [check({ summary: markup, evidence: {
      freshness: 'stale', criteria: [markup],
      assertions: [{ action: 'assert_text', expected: markup, actual: '<script>1</script>' }],
      document: { url: 'javascript:alert(1)', freshness: 'unknown' },
    } })] }));
    const card = createRunRecapCard(report);
    assert.match(card.className, /run-recap/);
    assert.ok(card.allText.includes(markup));
    assert.ok(card.allText.includes('javascript:alert(1)'));
    const details = card.children.find((child) => child.tag === 'details');
    assert.ok(details);
    assert.equal(details.open, undefined);
    assert.equal(details.children[0].tag, 'summary');
    assert.equal(card.children.some((child) => ['img', 'script', 'a'].includes(child.tag)), false);
  } finally {
    globalThis.document = previous;
  }
});
