import test from 'node:test';
import assert from 'node:assert/strict';
import {
  appendThinkingDelta,
  normalizeThinkingReceipt,
  thinkingReceiptFromMessage,
  thinkingReceiptText,
  thinkingStateForStop,
} from './thinking_presentation.js';

test('reasoning preserves token fragments, whitespace, language and markup exactly', () => {
  const chunks = ['Plan', 'ning', '.', 'Next', '\n\n', '服务器', '<script>alert(1)</script>'];
  let receipt = normalizeThinkingReceipt();
  for (const chunk of chunks) receipt = appendThinkingDelta(receipt, chunk);
  assert.equal(receipt.text, chunks.join(''));
  assert.equal(receipt.state, 'thinking');
  assert.equal(receipt.observed, true);
});

test('a requested mode never invents received reasoning', () => {
  const receipt = normalizeThinkingReceipt({ enabled: true });
  assert.equal(receipt.observed, false);
  assert.equal(thinkingReceiptText(receipt), '已要求思考，未收到推理輸出');
  assert.equal(thinkingReceiptText({}), '');
  assert.equal(thinkingReceiptText({ enabled: false }), '');
  assert.equal(thinkingReceiptFromMessage({ content: '<think>not a channel</think>' }), null);
});

test('saved receipts keep partial raw bytes and explicit stop state', () => {
  const thinking = { text: '未完\n 成', enabled: true, observed: true, state: 'stopped' };
  assert.deepEqual(thinkingReceiptFromMessage({ thinking }), thinking);
  assert.deepEqual(thinkingReceiptFromMessage({ thought: thinking }), thinking);
  assert.match(thinkingReceiptText(thinking), /已停止/);
  assert.deepEqual(thinkingReceiptFromMessage({ thought: 'legacy\nraw' }), {
    text: 'legacy\nraw', observed: true, state: 'complete',
  });
  assert.match(thinkingReceiptText({ observed: true }), /未保存思考內容/);
  assert.equal(thinkingReceiptFromMessage({ thought: {
    text: 'live partial', observed: true, state: 'streaming', runId: 'run', sequence: 2,
  } }).state, 'thinking');
});

test('stop reasons distinguish cancellation, incomplete and normal completion', () => {
  for (const reason of ['cancelled', 'canceled', 'stopped']) {
    assert.equal(thinkingStateForStop(reason), 'stopped');
  }
  for (const reason of ['error', 'token_limit', 'time_budget', 'incomplete']) {
    assert.equal(thinkingStateForStop(reason), 'incomplete');
  }
  for (const reason of ['end_turn', 'completed', undefined]) {
    assert.equal(thinkingStateForStop(reason), 'complete');
  }
});

test('received reasoning remains visible even if the request disabled thinking', () => {
  assert.equal(thinkingReceiptText({ enabled: false, text: 'raw', state: 'complete' }),
    '思考過程 · 已思考（思考開關已關閉）');
  assert.equal(thinkingReceiptText({ text: 'raw', state: 'complete' }), '思考過程 · 已思考');
});
