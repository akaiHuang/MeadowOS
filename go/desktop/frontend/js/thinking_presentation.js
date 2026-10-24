// Reasoning is display-only evidence from the model's separate thought channel.
// Do not infer it from answer text, translate it, or repair token boundaries.
const RECEIPT_STATES = new Set(['thinking', 'complete', 'stopped', 'incomplete']);

export function normalizeThinkingReceipt(value = {}) {
  const text = typeof value.text === 'string' ? value.text : '';
  return {
    ...(typeof value.enabled === 'boolean' ? { enabled: value.enabled } : {}),
    observed: !!text.trim() || value.observed === true,
    text,
    state: value.state === 'streaming' ? 'thinking'
      : RECEIPT_STATES.has(value.state) ? value.state : 'complete',
  };
}

export function thinkingReceiptFromMessage(message) {
  if (message?.thinking && typeof message.thinking === 'object') {
    return normalizeThinkingReceipt(message.thinking);
  }
  if (message?.thought && typeof message.thought === 'object') {
    return normalizeThinkingReceipt(message.thought);
  }
  // Historical payloads can contain a real saved thought string. Absence is
  // not evidence of either thinking or a disabled request.
  if (typeof message?.thought === 'string' && message.thought.length) {
    return normalizeThinkingReceipt({ text: message.thought });
  }
  return null;
}

export function appendThinkingDelta(receipt, delta) {
  const current = normalizeThinkingReceipt(receipt);
  if (typeof delta !== 'string' || !delta.length) return current;
  return normalizeThinkingReceipt({
    ...current,
    text: current.text + delta,
    state: 'thinking',
  });
}

export function thinkingStateForStop(stopReason) {
  if (!stopReason || ['end_turn', 'completed'].includes(stopReason)) {
    return 'complete';
  }
  return ['cancelled', 'canceled', 'stopped'].includes(stopReason)
    ? 'stopped'
    : 'incomplete';
}

export function thinkingReceiptText(value) {
  const receipt = normalizeThinkingReceipt(value);
  if (receipt.observed) {
    if (!receipt.text.trim()) return '已思考（這筆舊紀錄未保存思考內容）';
    const phase = {
      thinking: '生成中',
      stopped: '已停止',
      incomplete: '未完成',
      complete: '已思考',
    }[receipt.state];
    const disabled = receipt.enabled === false ? '（思考開關已關閉）' : '';
    return `思考過程 · ${phase}${disabled}`;
  }
  return receipt.enabled === true ? '已要求思考，未收到推理輸出' : '';
}

export function createThinkingView(receipt = {}) {
  const d = document.createElement('details');
  d.className = 'think thinking-receipt';
  d.setAttribute('aria-label', '模型思考過程');
  const sum = document.createElement('summary');
  const hint = document.createElement('p');
  hint.className = 'thinking-hint';
  hint.textContent = '模型原始思考內容，不等於最終答案。';
  const tx = document.createElement('pre');
  tx.className = 'tx thinking-text';
  d.append(sum, hint, tx);
  // Native details starts collapsed. Reuse this node on every token and
  // finalization so the user's open/closed choice always survives.
  const view = { d, sum, tx, hint, receipt: null };
  updateThinkingView(view, receipt);
  return view;
}

export function updateThinkingView(view, value) {
  const receipt = normalizeThinkingReceipt(value);
  view.receipt = receipt;
  view.sum.textContent = thinkingReceiptText(receipt);
  // Raw untrusted reasoning must never be parsed as HTML or Markdown.
  view.tx.textContent = receipt.text;
  const hasText = !!receipt.text.trim();
  view.tx.hidden = !hasText;
  view.hint.hidden = !hasText;
  view.d.hidden = !view.sum.textContent;
  view.d.dataset.observed = String(receipt.observed);
  view.d.dataset.state = receipt.state;
  view.d.classList.toggle('live', hasText && receipt.state === 'thinking');
  return view;
}

export function appendThinkingText(view, delta) {
  return updateThinkingView(view, appendThinkingDelta(view.receipt, delta));
}

export function finishThinkingView(view, stopReason) {
  return updateThinkingView(view, {
    ...view.receipt,
    state: thinkingStateForStop(stopReason),
  });
}
