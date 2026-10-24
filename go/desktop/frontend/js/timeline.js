// timeline.js — chat timeline rendering for the Meadow desktop frontend.
// Subscribes to bus events (SSE-fed via core.js) and renders into #timeline:
// user bubbles (steer-tagged when sent mid-run), streamed assistant markdown
// with a typing caret, raw streamed thought receipts (user-controlled
// disclosure), consecutive tool
// calls folded into one work-log card, stream-error bars and an end-of-turn
// divider pill. Stick-to-bottom scrolling with an accessible arrow jump button.
// Unboxed nine-dot status segments follow conversation order. Only the
// current segment animates; previous segments keep their original anchors.
//
// M7 像素對齊（對照 TS MessagesTimeline.tsx）：
// - assistant 無氣泡，訊息下方 .ameta 小字「時間戳 • 耗時」（耗時=turn:done
//   的 elapsedMs，掛該輪最後一則 assistant；串流中只顯時間），複製鈕移入
//   meta 列（hover 浮現，同 TS AssistantCopyButton）。
// - user 氣泡 rounded-2xl/rounded-br-sm；時間與訊息動作放在氣泡下方。
// - 模型 gate：bus('model_status') status!=='ready' → composer 上方
//   #modelGateBar 黃條（文案=TS ChatComposer meadowEmptyState 原字串）。
//
// Turn completion is server-authoritative: core.js forwards the
// `turn_completed` SSE event and initTimeline bridges it to the long-lived
// `turn:done` UI event used by the timeline, Git panel, and usage footer.

import { bus, daemon, state } from './core.js?v=raven-execution-20261006a';
import { authorizePromptDelivery, consumePromptDelivery } from './commercial.js?v=desktop-send-20260904a';
import { initNearby } from './nearby.js?v=nearby-google-link-20260930a';
import { animateNineDots } from './nine_dots.js';
import { renderAnswer } from './answer_presentation.js?v=search-thinking-20260928a';
import { artifactSearchPresentation } from './artifact_search_presentation.js';
import { sourceComparisonPresentation, createSourceComparison } from './source_comparison.js';
import { runRecapPresentation, createRunRecapCard } from './run_recap_presentation.js';
import {
  createSearchGallery,
  updateSearchGallery,
  disposeSearchGallery,
  normalizeResearchSources,
} from './search_presentation.js?v=search-thinking-20260928a';
import {
  createThinkingView,
  appendThinkingText,
  finishThinkingView,
  thinkingReceiptFromMessage,
} from './thinking_presentation.js?v=search-thinking-20260928a';

const REASONS = {
  end_turn: '回覆結束',
  completed: '回覆結束',
  empty: '⚠ 模型沒有產出（重試或換個說法）',
  incomplete: '⚠ 未完成（必要驗證或收尾步驟未通過）',
  cancelled: '已取消',
  aborted: '已中止',
  interrupted: '⚠ 執行已中斷，工作尚未完成',
  unknown_stop: '本輪已停止（未記錄結束原因）',
  loop_detected: '偵測到循環',
  time_budget: '已達本次時間上限，工作尚未完成；等你決定是否續做',
  token_budget: '已達本次 token 上限，工作尚未完成；等你決定是否續做',
  max_iterations: '已達本次執行步數上限，工作尚未完成',
  token_limit: '已達本次模型容量上限，工作尚未完成',
  audit_inconclusive: '⚠ 驗證證據不足，未標示完成',
  error: '發生錯誤',
};

// Preferred rawInput keys for the one-line parameter chip on tool rows.
const CHIP_KEYS = [
  'command',
  'file_path',
  'path',
  'pattern',
  'query',
  'url',
  'prompt',
  'text',
  'description',
  'title',
];

// Tools whose rawInput.file_path counts as a file edit for the per-turn
// "已編輯 N 個檔案" card (names match go/core/tools).
const EDIT_TOOLS = new Set(['write_file', 'save_conversation_text', 'edit']);

let tl = null; // #timeline
let nearby = null;
let scroller = null; // #scroll
let jumpBtn = null; // floating latest-message button

let last = null; // {kind:'user'|'agent'|'thought'|'tools'} — block merging
let openAgent = null; // {mdEl, raw:{text}} — streaming assistant segment
let lastAgentMeta = null; // {tsEl, time} — 該輪最後一則 assistant 的 meta 時間戳
let openThought = null; // current run's raw reasoning receipt
const thoughtViews = new Map(); // session/run -> receipt; one across tool rounds
let thoughtFallbackRun = 0; // legacy unscoped events within this timeline
let curWl = null; // {card, wh, body, n, folded} — open work-log card
const toolRows = new Map(); // session/run/toolCallId -> {row, st, name}
const sourceGalleries = new Map(); // run/call -> session-owned source gallery
const previewRequests = new Set();
let sourceEpoch = 0;
let sourceRevision = 0;
const runningTools = new Map(); // scoped tool key -> name, in-flight this turn
const pendingEditPaths = new Map(); // scoped tool key -> requested/normalized path
const turnEdits = new Set(); // file paths edited this turn (insertion order)
const turnAgentEls = []; // this turn's .amd nodes — re-linkified at turn end
// once turnEdits is complete (edit may follow the text)
let turnOpen = false;
// run_state can precede the first user message. A running run alone is not
// evidence that the next ordinary message is a mid-turn Guide.
let turnHasUser = false;
let turnStart = 0;
let pinned = true; // stick-to-bottom unless the user scrolled up
let raf = 0;
let wait = null; // The only live chronological status segment.
let waitingForUser = false;
let latestInferenceProgress = null;
// The newest visible user message can be resent only after its turn has
// actually stopped without a visible assistant answer.
let pendingUserReply = null; // {row,text,resend,answered,sessionId}

// --- tiny DOM helpers ---
function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
}

function truncate(s, n) {
  s = String(s);
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}

function stringifyRaw(v) {
  if (v == null) return '';
  if (typeof v === 'string') return v;
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

function rawContentText(u) {
  const c = u && u.content;
  if (c == null) return '';
  return String(
    typeof c === 'string'
      ? c
      : Array.isArray(c)
        ? c.map((x) => (x && x.text) || '').join('')
        : c.text || '',
  );
}

function contentText(u) {
  const raw = rawContentText(u);
  // Old sessions may contain the synthetic attachment marker emitted by
  // earlier Core builds. Attachments already render as thumbnails, so hide
  // those legacy-only lines instead of showing duplicate machine prose.
  return String(raw)
    .split(/\r?\n/)
    .filter(
      (line) => !/^\[使用者附了(?:一張|\s*\d+\s*張)圖片\]$/.test(line.trim()),
    )
    .join('\n')
    .trim();
}

function acceptsActiveSession(value) {
  const incoming = String((value && value.sessionId) || '');
  const active = String(state.sessionId || '');
  if (incoming && active && incoming !== active) return false;
  const incomingRun = String((value && value.runId) || '');
  const activeRun = String(state.runId || '');
  return !incomingRun || !activeRun || incomingRun === activeRun;
}

function scopedToolKey(value) {
  if (!value) return '';
  const callID = String(value.toolCallId || value.callId || '');
  if (!callID) return '';
  return [
    String(value.sessionId || state.sessionId || 'legacy-session'),
    String(value.runId || 'legacy-run'),
    callID,
  ].join('\u0000');
}

function completedTool(value) {
  return ['completed', 'complete', 'succeeded', 'success', 'ok'].includes(
    String((value && value.status) || '').toLowerCase(),
  );
}

// These are observed tool receipts, never links extracted from model prose.
// A session switch invalidates both source hydration and in-flight previews.
function sourceOptions(sessionId, epoch) {
  return {
    loadPreview: async (source) => {
      if (sessionId !== state.sessionId || epoch !== sourceEpoch) return null;
      const controller = new AbortController();
      previewRequests.add(controller);
      try {
        const response = await fetch(
          `/session/${encodeURIComponent(sessionId)}/source-preview`,
          {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ url: source.receiptURL || source.url, runId: source.runId || '' }),
            cache: 'no-store',
            signal: controller.signal,
          },
        );
        if (!response.ok) return null;
        const result = await response.json();
        return sessionId === state.sessionId && epoch === sourceEpoch
          ? result.image || null
          : null;
      } finally {
        previewRequests.delete(controller);
      }
    },
  };
}

function placeSourceGallery(gallery, records) {
  const callIds = new Set(records.map((source) => source.callId).filter(Boolean));
  const tool = [...toolRows.values()].find((entry) => callIds.has(entry.callId));
  const worklog = tool?.row.closest('.worklog');
  if (worklog?.isConnected) {
    worklog.after(gallery);
    return;
  }
  const runId = records.find((source) => source.runId)?.runId;
  const historyRows = runId
    ? [...tl.children].filter((row) => row.dataset.historyRun === runId)
    : [];
  const answer = historyRows.at(-1);
  if (answer) {
    answer.insertBefore(gallery, answer.querySelector('.amd'));
    return;
  }
  const times = records.map((source) => Date.parse(source.recordedAt || source.observedAt || '')).filter(Number.isFinite);
  const observedAt = times.length ? Math.min(...times) : NaN;
  const following = Number.isFinite(observedAt)
    ? [...tl.children].find((row) => Date.parse(row.dataset.timestamp || '') >= observedAt)
    : null;
  if (following) tl.insertBefore(gallery, following);
  else tl.appendChild(gallery);
}

function showSourceRecords(records, replace = false) {
  const grouped = new Map();
  for (const source of Array.isArray(records) ? records : []) {
    if (!source || typeof source !== 'object') continue;
    const key = String(source.runId || source.callId || 'history');
    if (!grouped.has(key)) grouped.set(key, []);
    grouped.get(key).push(source);
  }
  if (replace) {
    for (const [key, entry] of sourceGalleries) {
      if (!grouped.has(key)) {
        disposeSearchGallery(entry.gallery);
        entry.gallery.remove();
        sourceGalleries.delete(key);
      }
    }
  }
  for (const [key, incoming] of grouped) {
    const previous = sourceGalleries.get(key);
    const byURL = new Map((replace ? [] : previous?.records || []).map((source) => [source.url, source]));
    for (const source of incoming) byURL.set(source.url, source);
    const rows = [...byURL.values()];
    if (!normalizeResearchSources(rows).length) continue;
    const signature = JSON.stringify(rows);
    if (previous?.signature === signature) continue;
    const options = sourceOptions(state.sessionId, sourceEpoch);
    const gallery = previous
      ? updateSearchGallery(previous.gallery, rows, options)
      : createSearchGallery(rows, options);
    if (!gallery) continue;
    gallery.dataset.sourceGroup = key;
    sourceGalleries.set(key, { gallery, records: rows, signature });
    if (!gallery.isConnected) placeSourceGallery(gallery, rows);
  }
  stick();
}

async function hydrateSources() {
  const sid = state.sessionId;
  if (!sid) return;
  const epoch = sourceEpoch;
  const revision = sourceRevision;
  try {
    const response = await daemon.sourceList(sid);
    if (sid !== state.sessionId || epoch !== sourceEpoch || revision !== sourceRevision) return;
    showSourceRecords(response?.items, true);
  } catch {
    // Optional presentation must not remove an already received receipt.
  }
}

function committedEditPaths(value, fallback) {
  const observed = Array.isArray(value && value.changedPaths)
    ? value.changedPaths.filter((path) => typeof path === 'string' && path)
    : [];
  if (observed.length) return observed;
  const inputPath = value && value.rawInput && value.rawInput.file_path;
  if (typeof inputPath === 'string' && inputPath) return [inputPath];
  return fallback ? [fallback] : [];
}

function messageAttachments(message) {
  const raw = message && message.attachments;
  if (!Array.isArray(raw)) return [];
  return raw
    .map((item, index) => {
      if (typeof item === 'string')
        return { url: item, name: `圖片 ${index + 1}` };
      if (!item || typeof item.url !== 'string' || !item.url.trim())
        return null;
      return { url: item.url, name: item.name || `圖片 ${index + 1}` };
    })
    .filter(Boolean);
}

function showImageLightbox(url, name) {
  let box = document.getElementById('imageLightbox');
  if (!box) {
    box = el('div', 'image-lightbox');
    box.id = 'imageLightbox';
    box.setAttribute('role', 'dialog');
    box.setAttribute('aria-modal', 'true');
    box.innerHTML =
      '<button type="button" class="image-lightbox-close" aria-label="關閉圖片">×</button><img class="image-lightbox-img" alt="" />';
    document.body.appendChild(box);
    const close = () => box.classList.remove('on');
    box.addEventListener('click', (event) => {
      if (
        event.target === box ||
        event.target.classList.contains('image-lightbox-close')
      )
        close();
    });
    box._close = close;
  }
  const img = box.querySelector('img');
  img.src = url;
  img.alt = name || '使用者附加圖片';
  box.classList.add('on');
}

function chipText(raw) {
  if (raw == null) return '';
  if (typeof raw === 'string') return raw;
  for (const k of CHIP_KEYS) {
    const v = raw[k];
    if (typeof v === 'string' && v.trim()) return v;
  }
  const s = stringifyRaw(raw);
  return s === '{}' ? '' : s;
}

function fmtElapsed(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  const m = Math.floor(s / 60);
  return m > 0 ? `${m}m ${s % 60}s` : `${s}s`;
}

function fmtTokens(n) {
  n = Number(n) || 0;
  if (n < 1000) return String(n);
  return (n / 1000).toFixed(n >= 100000 ? 0 : 1).replace(/\.0$/, '') + 'K';
}

function fmtContextTokens(n) {
  const named = new Map([
    [1048576, '1M'],
    [524288, '512K'],
    [262144, '256K'],
    [131072, '128K'],
    [65536, '64K'],
    [32768, '32K'],
    [16384, '16K'],
  ]);
  return named.get(Number(n) || 0) || fmtTokens(n);
}

function fmtSeconds(ms) {
  const seconds = Math.max(0, Number(ms) || 0) / 1000;
  return seconds < 10 ? seconds.toFixed(2) + 's' : fmtElapsed(seconds * 1000);
}

function fmtRate(tokensPerSecond) {
  const rate = Number(tokensPerSecond) || 0;
  if (rate <= 0) return '';
  if (rate >= 1000) return `${fmtTokens(rate)} tok/s`;
  return `${rate.toFixed(rate >= 100 ? 0 : 1)} tok/s`;
}

function prefillTelemetryDetail(progress, context) {
  const p = progress || {};
  const processed = Math.max(0, Number(p.prefillProcessedTokens) || 0);
  const total = Math.max(0, Number(p.prefillTotalTokens) || 0);
  const observed = Math.max(0, Number(p.prefillObservedTokens) || 0);
  const observedMs = Math.max(0, Number(p.prefillObservedElapsedMs) || 0);
  const elapsedMs = Math.max(0, Number(p.elapsedMs) || 0);
  const rate =
    observed > 0 && observedMs > 0 ? observed / (observedMs / 1000) : 0;
  const parts = [];

  if (total > 0) {
    parts.push(
      `已處理 ${fmtTokens(Math.min(processed, total))}/${fmtTokens(total)} tokens`,
    );
    const percent = Math.min(100, Math.max(0, (processed / total) * 100));
    parts.push(`${percent.toFixed(percent >= 10 ? 0 : 1)}%`);
  } else if (processed > 0) {
    parts.push(`已處理 ${fmtTokens(processed)} tokens`);
    parts.push('等待 Core 回報總量');
  } else {
    parts.push('等待 Core 回報 token 進度');
  }

  if (rate > 0) {
    parts.push(`prefill ${fmtRate(rate)}`);
    if (total > processed) {
      parts.push(`ETA ${fmtElapsed(((total - processed) / rate) * 1000)}`);
    }
  } else {
    parts.push('尚無 prefill 速率');
  }
  if (elapsedMs > 0) parts.push(`已耗時 ${fmtSeconds(elapsedMs)}`);
  return parts.join(' · ') + context;
}

function progressPresentation(progress) {
  const p = progress || {};
  const phase = p.phase || '';
  const cached = Number(p.cachedTokens) || 0;
  const remaining =
    Number(p.remainingTokens) ||
    Math.max((Number(p.promptTokens) || 0) - cached, 0);
  const total =
    Number(p.prefillTotalTokens) || Number(p.promptEstimatedTokens) || 0;
  const completion = Number(p.completionTokens) || 0;
  const ttft = Number(p.ttftMs) || 0;
  const cacheType = String(p.cacheHitType || '')
    .trim()
    .toUpperCase();
  const contextWindow = Number(p.contextWindowTokens) || 0;
  const promptTokens = Number(p.promptTokens) || total;
  const context =
    contextWindow > 0 && promptTokens > 0
      ? ` · ${fmtTokens(promptTokens)}/${fmtContextTokens(contextWindow)} 工作區`
      : '';
  switch (phase) {
    case 'connecting':
      return ['( •_•)', '正在連接 Meadow Core', '建立本機推理通道'];
    case 'model_loading':
      return ['(￣ヘ￣)', '正在準備本機推理', '正在載入到統一記憶體'];
    case 'model_ready':
      return ['(｡•̀ᴗ-)✧', '等待模型下一步', '模型與工具共用同一個 Agent loop'];
    case 'queued':
      return ['( •_•)ゞ', '等待推理排程', 'Meadow Core 佇列'];
    case 'waiting':
      return ['( •_•)ゞ', '正在等待模型回覆', ''];
    case 'streaming': {
      const bytes = Math.max(0, Math.floor(Number(p.bytesReceived) || 0));
      return [
        '( •̀ᴗ•́ )',
        '正在接收模型 API 資料',
        bytes > 0 ? `已接收 ${bytes.toLocaleString('en-US')} bytes` : '上游回應資料串流中',
      ];
    }
    case 'cache_lookup':
      return [
        '(◍•ᴗ•◍)',
        '正在查找 Prefix cache',
        cached > 0
          ? `Prefix cache ${cacheType || 'HIT'} · ${fmtTokens(cached)} cached · ${remaining} new${context}`
          : `等待快取結果${context}`,
      ];
    case 'prefill': {
      let detail = prefillTelemetryDetail(p, context);
      if (cached > 0) {
        detail = `Prefix cache ${cacheType || 'HIT'} · ${fmtTokens(cached)} cached · ${fmtTokens(remaining)} new · ${detail}`;
      }
      const source = String(p.readingSource || '').trim();
      const excerpt = String(p.readingExcerpt || '').trim();
      if (total >= 32000 && (source || excerpt)) {
        const excerptText = excerpt ? `「${excerpt}」` : '';
        detail += ` · 閱讀 ${source || '資料'}${excerptText}`;
      }
      return ['(￣ヘ￣)', '正在理解新輸入', detail];
    }
    case 'first_token':
      return [
        '( •̀ᴗ•́ )',
        '即將開始輸出',
        ttft > 0
          ? `TTFT ${fmtSeconds(ttft)}${context}`
          : `模型已完成預處理${context}`,
      ];
    case 'decoding': {
      const decodeMs = Math.max(0, (Number(p.elapsedMs) || 0) - ttft);
      const tps =
        completion > 0 && decodeMs > 0 ? completion / (decodeMs / 1000) : 0;
      const parts = [];
      if (completion > 0) parts.push(`${fmtTokens(completion)} tokens`);
      if (tps > 0) parts.push(`${tps.toFixed(1)} tok/s`);
      if (ttft > 0) {
        parts.push(
          `${p.firstTokenVisible === false ? 'Model TTFT' : 'TTFT'} ${fmtSeconds(ttft)}`,
        );
      }
      return [
        '( •̀ᴗ•́ )',
        '正在產生回覆',
        (parts.length ? parts.join(' · ') : '文字串流中') + context,
      ];
    }
    case 'document_ingest': {
      const done = Number(p.ingestionProcessed) || 0;
      const count = Number(p.ingestionTotal) || 0;
      const source = p.ingestionSourceName || p.readingSource || '輸入文件';
      const lines =
        p.ingestionStartLine > 0
          ? ` · 行 ${p.ingestionStartLine}-${p.ingestionEndLine || p.ingestionStartLine}`
          : '';
      return [
        '(◍•ᴗ•◍)',
        '正在建立文件索引',
        `${source}${count > 0 ? ` · ${done}/${count} chunks` : ''}${lines}`,
      ];
    }
    case 'document_ready': {
      const source = p.ingestionSourceName || p.ingestionSourceId || '文件';
      return [
        '(｡•̀ᴗ-)✧',
        '文件索引已就緒',
        `${p.ingestionReused ? '重用' : '完成'}文件索引 · ${source}`,
      ];
    }
    case 'agent_requirement_audit':
      return ['( •_•)ゞ', '正在核對任務需求', '獨立檢查需求與驗收條件'];
    case 'agent_checkpoint':
      return ['(｡•̀ᴗ-)✧', '正在保存檢查點', '保存目前最佳可驗證版本'];
    case 'agent_verifying':
      return ['( •_•)ゞ', '正在執行獨立驗證', '重新檢查輸出與驗收條件'];
    case 'agent_validation_failed':
      return ['(￣ヘ￣)', '驗收尚未通過', '具體錯誤已交回模型，準備不同策略'];
    case 'agent_validation_passed':
      return ['(｡•̀ᴗ-)✧', '本次驗證已通過', '正在核對其餘需求與最後結果'];
    case 'agent_recovery':
      return ['( •̀ᴗ•́ )', '正在自我修正', '根據具體錯誤重新規劃'];
    case 'agent_rollback':
      return ['( •_•)ゞ', '正在回復安全版本', '回復上一個可驗證檢查點'];
    default:
      return ['(◍•ᴗ•◍)', '執行中…', '等待可觀測進度'];
  }
}

const COMPLETED_PHASE_LABELS = {
  connecting: '服務已連線',
  model_loading: '模型已載入',
  queued: '推理排程完成',
  cache_lookup: 'Prefix cache 已查找',
  prefill: '輸入理解完成',
  first_token: '首字已產生',
  document_ingest: '文件索引建立',
  document_ready: '文件索引就緒',
  agent_requirement_audit: '需求核對完成',
  agent_checkpoint: '檢查點已保存',
  agent_validation_failed: '已取得驗收錯誤',
  agent_recovery: '自我修正完成',
  agent_rollback: '安全版本已回復',
  agent_verifying: '獨立驗證完成',
  // One executed validator passed. The task may still require another build,
  // visual check, interaction check, or requirement audit.
  agent_validation_passed: '一項驗證已通過',
};

// 訊息時間戳＝TS formatTimestamp（Intl 預設 locale、時:分:秒，zh-TW 系統
// 顯示「下午2:03:38」）；formatter 建一次重用（TS 有同款 cache）。
const timeFmt = new Intl.DateTimeFormat(undefined, {
  hour: 'numeric',
  minute: '2-digit',
  second: '2-digit',
});
function fmtTime(t) {
  return timeFmt.format(t == null ? Date.now() : t);
}

function messageTime(message) {
  const raw =
    message && (message.timestamp || message.time || message.createdAt);
  if (raw == null || raw === '') return null;
  const parsed = typeof raw === 'number' ? raw : Date.parse(raw);
  return Number.isFinite(parsed) ? parsed : null;
}

async function copyText(s) {
  try {
    await navigator.clipboard.writeText(s);
    return true;
  } catch {
    // non-secure-context fallback
    const ta = document.createElement('textarea');
    ta.value = s;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand('copy');
    } catch {
      /* unsupported */
    }
    ta.remove();
    return ok;
  }
}

function settlePendingUser(answered) {
  if (!pendingUserReply) return;
  if (answered) pendingUserReply.answered = true;
  pendingUserReply.resend.hidden = !!pendingUserReply.answered;
}

function userMessageRow(message, options = {}) {
  const text = contentText(message);
  const row = el('div', 'urow');
  const bub = el('div', 'ubub');
  if (options.isSteer) bub.appendChild(el('div', 'steer-tag', '⤷ 引導'));
  const attachments = messageAttachments({
    ...message,
    attachments:
      message && message.attachments
        ? message.attachments
        : options.attachments,
  });
  if (attachments.length) {
    const strip = el('div', 'user-attachments');
    for (const attachment of attachments) {
      const button = el('button', 'user-attachment');
      button.type = 'button';
      button.title = '點擊放大圖片';
      button.setAttribute('aria-label', `放大${attachment.name}`);
      const img = document.createElement('img');
      img.src = attachment.url;
      img.alt = attachment.name;
      img.loading = 'lazy';
      button.appendChild(img);
      button.addEventListener('click', () =>
        showImageLightbox(attachment.url, attachment.name),
      );
      strip.appendChild(button);
    }
    bub.appendChild(strip);
  }
  if (text) bub.appendChild(el('div', 'utext', text));

  const meta = el('div', 'umeta');
  const at = messageTime(message);
  // Older resume payloads do not yet carry time. Never invent a historical
  // timestamp; live messages always get the current time.
  if (at != null || !options.restored)
    meta.appendChild(el('span', 'uts', fmtTime(at == null ? undefined : at)));

  const copy = el('button', 'user-action-btn user-copy-btn', '複製');
  copy.type = 'button';
  copy.title = '複製訊息';
  copy.setAttribute('aria-label', '複製訊息');
  copy.addEventListener('click', async () => {
    const ok = await copyText(text);
    copy.textContent = ok ? '已複製' : '複製失敗';
    setTimeout(() => {
      copy.textContent = '複製';
    }, 1200);
  });
  meta.appendChild(copy);

  const resend = el('button', 'user-action-btn user-resend-btn', '↻');
  resend.type = 'button';
  resend.hidden = true;
  resend.title = '重新傳送';
  resend.setAttribute('aria-label', '重新傳送');
  resend.addEventListener('click', () => {
    resend.disabled = true;
    bus.emit('message:resend', {
      text,
      sessionId: options.sessionId || state.sessionId,
    });
  });
  meta.appendChild(resend);
  row.append(bub, meta);
  return { row, resend, text };
}

// --- scrolling ---
function showJump() {
  if (jumpBtn) jumpBtn.classList.add('show');
}
function hideJump() {
  if (jumpBtn) jumpBtn.classList.remove('show');
}

function stick() {
  if (pinned) scroller.scrollTop = scroller.scrollHeight;
  else showJump();
}

function buildJump() {
  const wrap = el('div', 'jump-wrap');
  jumpBtn = el('button');
  jumpBtn.id = 'jumpLatest';
  jumpBtn.type = 'button';
  jumpBtn.setAttribute('aria-label', '回到最新訊息');
  jumpBtn.title = '回到最新訊息';
  const arrow = el('span', 'jump-arrow');
  arrow.setAttribute('aria-hidden', 'true');
  jumpBtn.appendChild(arrow);
  jumpBtn.addEventListener('click', () => {
    document.dispatchEvent(new Event('conversation:latest'));
    pinned = true;
    hideJump();
    scroller.scrollTo({
      top: scroller.scrollHeight,
      behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth',
    });
  });
  wrap.appendChild(jumpBtn);
  scroller.appendChild(wrap); // sticky; stays the last child of #scroll
}

// --- chronological, event-backed activity (approved iPhone nine-dot motion) ---
// A text delta proves output, not task completion. It starts a writing segment
// before the answer; tools and subsequent model steps create their own rows.
// Every archived row releases its animation/timers/listeners immediately.
function buildWait() {
  const row = el('div', 'activityrow waitrow');
  row.setAttribute('role', 'status');
  row.setAttribute('aria-live', 'polite');
  const indicator = el('span', 'wnine');
  indicator.setAttribute('aria-hidden', 'true');
  for (let i = 0; i < 9; i++) indicator.appendChild(el('i'));
  const content = el('div', 'wcontent');
  const primary = el('div', 'wline wprimary');
  const secondary = el('div', 'wline wsecondary');
  const text = el('span', 'wtext', '');
  const detail = el('span', 'wdetail', '');
  const sec = el('span', 'wsec', '已等待 0s');
  sec.setAttribute('aria-hidden', 'true');
  primary.append(text, sec);
  secondary.append(detail);
  content.append(primary, secondary);
  row.append(indicator, content);
  return {
    row,
    indicator,
    content,
    primary,
    secondary,
    text,
    detail,
    sec,
    timer: 0,
    disposeDots: animateNineDots(indicator),
    ticks: 0,
    start: 0,
    mode: '',
    tool: '',
    progress: null,
    phaseHistory: [],
    lastPhase: '',
    lastProgress: null,
  };
}

function renderProgressTrail() {
  if (!wait) return;
  const history = wait.phaseHistory || [];
  // iOS keeps completion history out of the loading surface. Preserve it as
  // inspectable metadata without turning one current action into three rows.
  if (history.length) {
    wait.row.dataset.completedPhases = history.join(' | ');
    wait.row.title = `已完成：${history.join('、')}`;
  } else {
    delete wait.row.dataset.completedPhases;
    wait.row.removeAttribute('title');
  }
}

function resetProgressTrail() {
  if (!wait) return;
  wait.phaseHistory = [];
  wait.lastPhase = '';
  wait.lastProgress = null;
  renderProgressTrail();
}

function recordProgressPhase(progress) {
  if (!wait || !progress) return;
  const phase = String(progress.phase || '');
  if (wait.lastPhase && wait.lastPhase !== phase) {
    const label = COMPLETED_PHASE_LABELS[wait.lastPhase];
    if (label && wait.phaseHistory[wait.phaseHistory.length - 1] !== label)
      wait.phaseHistory.push(label);
  }
  wait.lastPhase = phase;
  wait.lastProgress = progress;
  renderProgressTrail();
}

function setWaitCopy(label, detail) {
  if (wait.text.textContent !== label) wait.text.textContent = label;
  wait.text.title = label;
  if (wait.detail.textContent !== detail) wait.detail.textContent = detail;
  wait.detail.title = detail;
  wait.secondary.hidden = !detail;
}

function paintWait() {
  if (!wait) return;
  // Match iOS: the visible clock begins when the turn is first shown and does
  // not restart when the backend moves from scheduling to prefill or tools.
  const elapsedMs = Math.max(0, Date.now() - (turnStart || wait.start));
  const secs = Math.floor(elapsedMs / 1000);
  if (wait.mode === 'writing') {
    const progress = wait.progress;
    const detail =
      progress && ['first_token', 'decoding'].includes(progress.phase)
        ? progressPresentation(progress)[2]
        : '文字串流中';
    setWaitCopy('正在產生回覆', detail);
  } else if (wait.mode === 'reasoning') {
    setWaitCopy('正在思考', '已收到模型的思考內容');
  } else if (wait.mode === 'next') {
    setWaitCopy('正在接續處理', '工具已回報，等待模型下一步');
  } else if (wait.mode === 'progress' && wait.progress) {
    const phase = String(wait.progress.phase || '');
    if (phase === 'model_ready') {
      setWaitCopy('AI 分析中…', '訊息已送出，正在等待模型開始處理');
    } else {
      const [, label, detail] = progressPresentation(wait.progress);
      setWaitCopy(label, detail);
    }
  } else if (wait.mode === 'draft') {
    const kb = wait.bytes > 0 ? (wait.bytes / 1024).toFixed(1) + ' KB' : '…';
    setWaitCopy('AI 正在準備操作…', `正在組合 ${wait.tool} 的工具參數 · ${kb}`);
  } else if (wait.mode === 'tool') {
    setWaitCopy('正在執行工具', `${wait.tool || '工具'} · 等待執行結果`);
  } else if (wait.mode === 'background') {
    setWaitCopy('背景任務仍在執行', '已重新連線，正在等待下一個模型或工具事件');
  } else if (secs >= 30) {
    // No runtime telemetry means exactly that: never invent a prefill phase,
    // cache state, token count, or ETA merely because the turn is quiet.
    setWaitCopy('仍在等待模型回報', '尚未收到排程、token 或工具進度');
  } else {
    setWaitCopy('AI 分析中…', '訊息已送出，正在等待模型開始處理');
  }
  wait.sec.textContent = `已進行 ${secs}s`;
}

function finishWait(label) {
  if (!wait) return;
  paintWait();
  // This is a frozen position in the whole turn, not an active phase timer.
  wait.sec.textContent = wait.sec.textContent.replace(/^已進行 /, '本輪進度至 ');
  clearInterval(wait.timer);
  wait.disposeDots();
  const labels = {
    writing: '已輸出回覆',
    reasoning: '已輸出思考',
    tool: '工具執行紀錄',
    draft: '工具參數準備',
    next: '接續處理',
    think: '模型準備',
    progress: '模型處理紀錄',
    background: '背景執行紀錄',
  };
  setWaitCopy(label || labels[wait.mode] || '處理紀錄', '');
  wait.row.classList.remove('waitrow');
  wait.row.classList.add('status-history');
  wait.row.removeAttribute('role');
  wait.row.removeAttribute('aria-live');
  wait = null;
}

function showWait(mode, tool, bytes) {
  if (waitingForUser) return;
  // Preparation telemetry updates one row; output/tool boundaries create a
  // new segment. Existing nodes never move past their associated content.
  const group = (m) =>
    ['think', 'progress', 'background', 'next'].includes(m) ? 'prepare' : m;
  if (wait && group(wait.mode) !== group(mode)) {
    finishWait();
    finalizeAgent();
    finalizeThought();
  }
  if (!wait) wait = buildWait();
  const t = tool || '';
  // restart the phase clock when the row (re)appears or switches phase
  if (!wait.row.isConnected || wait.mode !== mode || wait.tool !== t) {
    wait.start = Date.now();
    wait.ticks = 0;
  }
  wait.mode = mode;
  wait.tool = t;
  wait.bytes = bytes || 0;
  paintWait();
  if (!wait.row.isConnected) tl.appendChild(wait.row);
  if (!wait.timer)
    wait.timer = setInterval(() => {
      wait.ticks++;
      paintWait();
    }, 1000);
  stick();
}

function onToolDraft(update) {
  if (!update || !acceptsActiveSession(update) || !turnOpen || waitingForUser)
    return;
  const tool = update.title || 'write_file';
  showWait('draft', tool, Number(update.bytes) || 0);
}

function onInferenceProgress(progress) {
  if (
    !progress ||
    !acceptsActiveSession(progress) ||
    !turnOpen ||
    waitingForUser
  )
    return;
  latestInferenceProgress = progress;
  const phase = progress.phase || '';
  if (phase === 'completed' || phase === 'failed') {
    return;
  }
  if (lastAgentMeta && (phase === 'first_token' || phase === 'decoding')) {
    const [, , detail] = progressPresentation(progress);
    lastAgentMeta.perf = detail === '文字串流中' ? '' : detail;
    lastAgentMeta.tsEl.textContent = lastAgentMeta.perf
      ? `${lastAgentMeta.time} • ${lastAgentMeta.perf}`
      : lastAgentMeta.time;
  }
  // Runtime polling may deliver cache/prefill after visible tokens. It cannot
  // end a content segment. Only an observable agent phase (or a tool/draft/user
  // event through its own handler) establishes the next work boundary.
  if (
    wait &&
    ['writing', 'reasoning'].includes(wait.mode) &&
    !phase.startsWith('agent_')
  ) {
    if (['first_token', 'decoding'].includes(phase)) {
      wait.progress = progress;
      wait.row.dataset.phase = phase;
      recordProgressPhase(progress);
      paintWait();
    }
    return;
  }
  showWait('progress');
  recordProgressPhase(progress);
  wait.progress = progress;
  wait.row.dataset.phase = phase;
  paintWait();
}

function hideWait() {
  if (!wait) return;
  if (wait.timer) {
    clearInterval(wait.timer);
    wait.timer = 0;
  }
  if (wait.row.isConnected) wait.row.remove();
  wait.disposeDots();
  wait = null;
}

// After a permission / user-input card resolves, the run continues — bring
// the row back in whichever phase the engine is actually in.
function resumeWait() {
  waitingForUser = false;
  if (!turnOpen) return;
  const names = [...runningTools.values()];
  if (names.length) showWait('tool', names[names.length - 1]);
  else showWait('next');
}

// --- 對話內檔案超連結（用戶 12） ---
// 偵測 assistant 文字裡的產出檔案路徑 → 綠色檔案連結，點擊 bus.emit
// ('preview:open',{path})。事實來源優先 turnEdits（本輪真的寫過的檔，relPath
// 後的相對路徑）；另外用保守 regex 撈 md 內看起來像「相對路徑且帶副檔名」的
// token（dir/file.ext 或 file.ext），避免把句子裡的縮寫誤判成檔案。
// 安全性：md() 已先 esc 再轉換，這裡只在「渲染後的 DOM 文字節點」上動刀
// （walk text nodes、用 anchor element 包裹），絕不把原始字串塞回 innerHTML，
// 也不會破壞既有 <a>/<code>/<pre> 內容（跳過這些祖先）。
const FILE_PATH_RE =
  /\/?(?:[\w.@~-]+\/)*[\w.@~-]+\.[A-Za-z][\w]{0,7}(?::\d+)?/g;
// 連結化時跳過的祖先（程式碼字面、已是連結）。inline <code> 另案處理：
// 整段就是一個檔案路徑時包成連結（模型慣用 `path` 寫法），<pre> 區塊不動。
const SKIP_LINK_TAGS = new Set(['A', 'CODE', 'PRE']);

// 取本輪已知檔案的相對路徑集合（turnEdits 是絕對或模型給的相對，統一 relPath）
function knownRelEdits() {
  const set = new Set();
  for (const p of turnEdits) {
    const r = relPath(p);
    if (r) set.add(r);
  }
  return set;
}

// 一個 token 只有在本輪真實 turnEdits 命中時才是可預覽產出檔。
// 模型文字裡單獨出現 foo.html 不是檔案存在證據，不能顯示成綠色連結。
// 回傳要 emit 的真實相對路徑或 null。
function fileLinkTarget(token, known) {
  let path = token.replace(/:\d+$/, ''); // 去掉行號尾巴 file.ext:12
  // 絕對路徑：工作區內→轉相對（preview 以 cwd 解析）；工作區外→不連結
  //（點了也讀不到，誠實不給假連結）
  if (path.startsWith('/')) {
    const rel = relPath(path);
    if (rel === path) return null;
    path = rel;
  }
  if (known.has(path)) return path;
  const base = path.split('/').filter(Boolean).pop() || path;
  for (const k of known) {
    if (k === path || k.endsWith('/' + path) || k === base) return k;
  }
  return null;
}

function markTurnUnverified() {
  if (!pendingEditPaths.size && !turnEdits.size) return;
  for (const mdEl of turnAgentEls) {
    if (!mdEl.isConnected) continue;
    const row = mdEl.closest('.arow');
    if (!row || row.querySelector('.unverified-note')) continue;
    row.insertBefore(
      el(
        'div',
        'unverified-note',
        '未驗證草稿 · 此輪未完成，不能視為已寫入或已儲存',
      ),
      mdEl,
    );
  }
}

function makeFileLink(label, path) {
  const a = el('a', 'file-link');
  a.href = '#';
  a.title = `預覽 ${path}`;
  a.dataset.filePath = path;
  const ic = el('span', 'fl-ic');
  ic.innerHTML = FILE_LINK_ICON;
  a.append(ic, el('span', 'fl-tx', label));
  a.addEventListener('click', (e) => {
    e.preventDefault();
    bus.emit('preview:open', { path });
  });
  return a;
}

// lucide file-text 同款（diffpanel/preview 一致），inline 連結的小 icon
const FILE_LINK_ICON =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z"/><path d="M14 2v5a1 1 0 0 0 1 1h5"/><path d="M16 13H8"/><path d="M16 17H8"/></svg>';

function linkifyFiles(root) {
  const known = knownRelEdits();
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode(node) {
      if (!node.nodeValue || !/[\w]\.[A-Za-z]/.test(node.nodeValue))
        return NodeFilter.FILTER_REJECT;
      for (let p = node.parentNode; p && p !== root; p = p.parentNode)
        if (SKIP_LINK_TAGS.has(p.nodeName)) return NodeFilter.FILTER_REJECT;
      return NodeFilter.FILTER_ACCEPT;
    },
  });
  const targets = [];
  for (let n = walker.nextNode(); n; n = walker.nextNode()) targets.push(n);
  for (const node of targets) {
    const text = node.nodeValue;
    FILE_PATH_RE.lastIndex = 0;
    let m;
    let lastIdx = 0;
    let frag = null;
    while ((m = FILE_PATH_RE.exec(text))) {
      const token = m[0];
      const tgt = fileLinkTarget(token, known);
      if (!tgt) continue;
      if (!frag) frag = document.createDocumentFragment();
      if (m.index > lastIdx)
        frag.appendChild(document.createTextNode(text.slice(lastIdx, m.index)));
      frag.appendChild(makeFileLink(token, tgt));
      lastIdx = m.index + token.length;
    }
    if (frag) {
      if (lastIdx < text.length)
        frag.appendChild(document.createTextNode(text.slice(lastIdx)));
      node.parentNode.replaceChild(frag, node);
    }
  }
  // inline <code> 整段是檔案路徑（模型慣用 `path` 寫法）→ 整顆包成連結；
  // <pre> 內的程式碼區塊不動，已含連結的跳過。
  for (const code of root.querySelectorAll('code')) {
    if (code.closest('pre') || code.querySelector('a')) continue;
    const txt = (code.textContent || '').trim();
    FILE_PATH_RE.lastIndex = 0;
    const m = FILE_PATH_RE.exec(txt);
    if (!m || m[0] !== txt) continue; // 整段必須恰好是一個路徑
    const tgt = fileLinkTarget(txt, known);
    if (!tgt) continue;
    const link = makeFileLink(txt, tgt);
    code.textContent = '';
    code.appendChild(link);
  }
}

// --- assistant streaming ---
function renderOpenAgent(streaming) {
  if (!openAgent) return;
  renderAnswer(openAgent.mdEl, openAgent.raw.text);
  if (streaming) openAgent.mdEl.appendChild(el('span', 'caret'));
  // 串流中先不連結化（路徑可能還沒打完、turnEdits 也未必到齊）；定稿才掃一次
  else linkifyFiles(openAgent.mdEl);
}

function scheduleRender() {
  if (raf) return;
  raf = requestAnimationFrame(() => {
    raf = 0;
    renderOpenAgent(true);
    stick();
  });
}

function finalizeAgent() {
  if (!openAgent) return;
  if (raf) {
    cancelAnimationFrame(raf);
    raf = 0;
  }
  renderOpenAgent(false);
  openAgent = null;
}

function newAgentBlock() {
  finalizeAgent();
  finalizeThought();
  const row = el('div', 'arow');
  const raw = { text: '' };
  const mdEl = el('div', 'amd');
  // meta 列＝TS AssistantTimelineRow 訊息下方一行：text-[10px] muted/30 的
  // 「時間戳（• 耗時）」＋ hover 才浮現的複製鈕（group-hover 同款）。
  const meta = el('div', 'ameta');
  const ts = el('span', 'ats', fmtTime());
  const btn = el('button', 'copy-btn', '複製');
  btn.title = '複製原始文字';
  btn.addEventListener('click', async () => {
    const ok = await copyText(raw.text);
    btn.textContent = ok ? '已複製' : '複製失敗';
    setTimeout(() => {
      btn.textContent = '複製';
    }, 1200);
  });
  meta.append(ts, btn);
  row.append(mdEl, meta);
  tl.appendChild(row);
  openAgent = { mdEl, raw };
  turnAgentEls.push(mdEl); // re-linkify at turn end with the full edit set
  lastAgentMeta = { tsEl: ts, time: ts.textContent }; // turn:done 補耗時
  // Decode progress can arrive just before the first visible content chunk.
  // Carry that last real measurement into the newly-created assistant row so
  // TTFT/tok/s are not lost at the wait-row -> answer transition.
  if (
    latestInferenceProgress &&
    ['first_token', 'decoding'].includes(latestInferenceProgress.phase)
  ) {
    const [, , detail] = progressPresentation(latestInferenceProgress);
    lastAgentMeta.perf = detail === '文字串流中' ? '' : detail;
    ts.textContent = lastAgentMeta.perf
      ? `${lastAgentMeta.time} • ${lastAgentMeta.perf}`
      : lastAgentMeta.time;
  }
  last = { kind: 'agent' };
}

function onAgentChunk(u) {
  if (!acceptsActiveSession(u)) return;
  // A streamed delta is not a complete display message. Leading spaces and
  // newline-only chunks carry token-boundary formatting and must survive
  // byte-for-byte; filtering/trimming belongs only to durable messages.
  const text = rawContentText(u);
  if (text.length === 0) return;
  settlePendingUser(true);
  if (turnOpen) showWait('writing');
  if (!openAgent || !last || last.kind !== 'agent') newAgentBlock();
  openAgent.raw.text += text;
  scheduleRender();
  stick();
}

// --- user bubbles (one event = one message; mid-turn = steer) ---
function onUserChunk(u) {
  const text = contentText(u);
  const attachments = messageAttachments(u);
  if (!text && !attachments.length) return;
  if (!turnHasUser && wait?.mode === 'background') hideWait();
  else finishWait();
  waitingForUser = false;
  latestInferenceProgress = null;
  finalizeAgent();
  finalizeThought();
  const isSteer = turnOpen && turnHasUser;
  if (!isSteer) {
    openThought = null;
    thoughtFallbackRun++;
  }
  turnHasUser = true;
  if (!turnOpen) {
    turnOpen = true;
    turnStart = Date.now();
  }
  // A newer user message supersedes the prior resend affordance. The button
  // is revealed only by turn completion/error if no agent chunk answered it.
  if (pendingUserReply) pendingUserReply.resend.hidden = true;
  const built = userMessageRow(u, {
    isSteer,
    sessionId: state.sessionId,
    attachments: u && u.attachments,
  });
  tl.appendChild(built.row);
  pendingUserReply = {
    ...built,
    answered: false,
    sessionId: state.sessionId,
  };
  last = { kind: 'user' };
  // message on screen = the model is working; give CLI-style feedback now
  resetProgressTrail();
  showWait('think');
}

// --- thought blocks ---
// One receipt per run preserves the raw stream across model/tool rounds.
// Opening/closing belongs to the user and survives tokens and finalization.
function thoughtRunKey(value) {
  return [
    String(value?.sessionId || state.sessionId || 'legacy-session'),
    String(value?.runId || state.runId || `turn-${thoughtFallbackRun}`),
  ].join('\u0000');
}

function finalizeThought(stopReason) {
  if (!openThought) return;
  finishThinkingView(openThought, stopReason);
}

function onThoughtChunk(u) {
  if (!acceptsActiveSession(u)) return;
  const text = rawContentText(u);
  if (text.length === 0) return;
  const key = thoughtRunKey(u);
  const sequence = Number.isSafeInteger(u?.thoughtSequence) ? u.thoughtSequence : 0;
  let view = thoughtViews.get(key);
  // Resume supplies a durable cumulative snapshot and replays later SSE.
  // Ignore only confirmed sequence duplicates, never deduplicate by text.
  if (sequence > 0 && sequence <= (view?.sequence || 0)) return;
  if (turnOpen) showWait('reasoning');
  finalizeAgent();
  if (!view) {
    view = createThinkingView();
    thoughtViews.set(key, view);
    tl.appendChild(view.d);
  }
  appendThinkingText(view, text);
  if (sequence > 0) view.sequence = sequence;
  openThought = view;
  last = { kind: 'thought', tx: view.tx };
  stick();
}

// --- tool calls folded into a work-log card ---
function refreshWlHeader(wl) {
  wl.wh.textContent = `${wl.folded ? '▸' : '▾'} 工作紀錄 · ${wl.n}`;
}

function newWorklog() {
  const card = el('div', 'worklog');
  const wh = el('div', 'wh');
  const body = el('div', 'wbody');
  card.append(wh, body);
  tl.appendChild(card);
  const wl = { card, wh, body, n: 0, folded: false };
  wh.addEventListener('click', () => {
    wl.folded = !wl.folded;
    card.classList.toggle('folded', wl.folded);
    refreshWlHeader(wl);
  });
  refreshWlHeader(wl);
  curWl = wl;
  return wl;
}

function appendToolRow(wl, u) {
  const row = el('div', 'tool');
  const st = el('span', 'st run', '●');
  const name = (u && (u.title || u.kind)) || '工具';
  const nm = el('span', 'nm', name);
  row.append(st, nm);
  const chip = chipText(u && u.rawInput);
  if (chip) {
    const c = el('span', 'chip', truncate(chip, 160));
    c.title = truncate(chip, 1000);
    row.appendChild(c);
  }
  wl.body.appendChild(row);
  wl.n++;
  refreshWlHeader(wl);
  const t = { row, st, name, callId: String(u?.toolCallId || u?.callId || ''), rawInput: u && u.rawInput };
  const key = scopedToolKey(u);
  if (key) toolRows.set(key, t);
  return t;
}

function onToolCall(u) {
  if (!u || !acceptsActiveSession(u)) return;
  finalizeAgent();
  finalizeThought();
  const name = (u && (u.title || u.kind)) || '';
  const key = scopedToolKey(u);
  if (EDIT_TOOLS.has(name) && u && u.rawInput) {
    const fp = u.rawInput.file_path;
    if (key && typeof fp === 'string' && fp) pendingEditPaths.set(key, fp);
  }
  const sameGroup = wait && wait.mode === 'tool';
  showWait('tool', name || '工具');
  const wl =
    sameGroup && last && last.kind === 'tools' && curWl ? curWl : newWorklog();
  appendToolRow(wl, u);
  last = { kind: 'tools' };
  // the model is blocked on this tool now — wait row in execution mode
  const label = name || '工具';
  if (key) runningTools.set(key, label);
}

// "重試" on a failed tool row: steer mid-turn, otherwise fire a new prompt.
// The idle path owns the blocking prompt busy lifecycle; turn completion is
// always delivered independently by the daemon over SSE.
async function retryTool(name, requestID) {
  const sid = state.sessionId;
  if (!sid) return false;
  const msg = `上一個 ${name} 失敗了，請修正後重試`;
  const steering = state.busy;
  if (!steering && state.runBudgetControl?.sessionId !== sid) {
    console.error('retry prompt deferred: inference controls are still loading');
    return false;
  }
  if (!steering) {
    state.busy = true;
    bus.emit('busy:changed'); // composer 亮停止鈕（重試也要可中斷）
  }
  let accepted = false;
  try {
    const authorization = await authorizePromptDelivery(sid, requestID);
    if (state.sessionId !== sid || !state.busy) return false;
    const result = steering
      ? await daemon.steer(sid, msg, undefined, authorization)
      : await daemon.prompt(sid, msg, undefined, undefined, authorization);
    accepted = result?.local !== true;
    if (accepted) await consumePromptDelivery(authorization);
  } catch (e) {
    console.error('retry prompt failed:', e);
  } finally {
    if (!steering && state.sessionId === sid) {
      state.busy = false;
      bus.emit('busy:changed');
    }
  }
  return accepted;
}

function addRetryBtn(t) {
  if (t.retryBtn) return;
  const b = el('button', 'retry-btn', '重試');
  b.title = '請模型修正後重試此工具';
  b.addEventListener('click', async () => {
    b.disabled = true;
    t.retryRequestID ||= globalThis.crypto.randomUUID();
    const ok = await retryTool(t.name || '工具', t.retryRequestID);
    if (ok) t.retryRequestID = '';
    b.disabled = false;
    b.textContent = ok ? '已送出' : '重試失敗';
    setTimeout(() => {
      b.textContent = '重試';
    }, 1500);
  });
  t.retryBtn = b;
  t.row.appendChild(b);
}

function onToolUpdate(u) {
  if (!u || !acceptsActiveSession(u)) return;
  const key = scopedToolKey(u);
  let t = key ? toolRows.get(key) : null;
  if (!t) t = appendToolRow(curWl || newWorklog(), u); // replay gap: materialize
  if (u.rawInput) t.rawInput = u.rawInput;
  if (completedTool(u)) {
    t.st.className = 'st ok';
    t.st.title = '工具呼叫成功';
    t.retryBtn?.remove();
    t.retryBtn = null;
    t.outcome?.remove();
    t.outcome = null;
    const name = (u.title || u.kind || (t && t.name) || '').trim();
    if (EDIT_TOOLS.has(name)) {
      for (const path of committedEditPaths(
        u,
        key && pendingEditPaths.get(key),
      ))
        turnEdits.add(path);
    }
  } else if (u.status === 'failed') {
    t.st.className = 'st fail';
    t.st.title = '本次工具嘗試未成功；不代表整個任務已停止';
    setToolOutcome(t, '未成功');
    addRetryBtn(t);
  }
  if (completedTool(u) || u.status === 'failed') {
    if (key) {
      runningTools.delete(key);
      pendingEditPaths.delete(key);
    }
    // still between tool end and the next chunk: flip the wait row back to
    // thinking (or to a still-running sibling tool), never resurrect it
    if (wait && wait.row.isConnected && wait.mode === 'tool') resumeWait();
  }
  if (u.rawOutput != null) {
    if (t.name === 'source_compare' && completedTool(u)) {
      const report = sourceComparisonPresentation(u.rawOutput);
      t.sourceComparison?.remove();
      t.sourceComparison = createSourceComparison(report);
      if (t.sourceComparison) t.row.appendChild(t.sourceComparison);
    }
    const artifact = t.name === 'artifact_search' ? artifactSearchPresentation(u.rawOutput) : null;
    const displayed = artifact ? artifact.text : stringifyRaw(u.rawOutput);
    t.row.title = truncate(displayed, 600);
    // Keep original observations accessible, as inert text. The neutral
    // attempt UI must never hide the reason or imply a failure recovered.
    if (!t.details) {
      t.details = el('details', 'tool-result');
      t.details.appendChild(el('summary', '', '查看工具結果'));
      t.resultText = el('pre', 'tool-result-text');
      t.details.appendChild(t.resultText);
      t.row.appendChild(t.details);
    }
    t.details.querySelector('summary').textContent = artifact ? artifact.summary : '查看工具結果';
    t.resultText.textContent = displayed;
    if (completedTool(u)) presentProcessObservation(t, u.rawOutput);
  }
  if (completedTool(u) && Array.isArray(u.evidenceSources)) {
    sourceRevision++;
    showSourceRecords(u.evidenceSources.map((source) => ({
      ...source,
      callId: u.toolCallId || u.callId,
      runId: u.runId,
      tool: u.title || u.kind || t.name,
    })));
  }
  stick();
}

function setToolOutcome(t, label) {
  if (!t.outcome) {
    t.outcome = el('span', 'tool-outcome');
    t.row.insertBefore(t.outcome, t.retryBtn || t.details || null);
  }
  t.outcome.textContent = label;
}

function presentProcessObservation(t, output) {
  if (t.name !== 'process' || !['wait', 'poll'].includes(t.rawInput?.action))
    return;
  let snapshot = output;
  if (typeof snapshot === 'string') {
    try {
      snapshot = JSON.parse(snapshot);
    } catch {
      return;
    }
  }
  // Only explicit tool fields, never error-keyword or semantic matching.
  if (!snapshot || typeof snapshot.process_id !== 'string') return;
  if (snapshot.status === 'running') {
    t.st.className = 'st observed';
    t.st.title = '已取得程序狀態；程序尚未結束';
    setToolOutcome(t, '程序仍在執行');
  } else if (
    snapshot.status === 'exited' &&
    Number.isInteger(snapshot.exit_code)
  ) {
    if (snapshot.exit_code !== 0) {
      t.st.className = 'st fail';
      t.st.title = '已取得程序狀態；程序以非零退出碼結束';
    }
    setToolOutcome(t, `程序已結束 · exit ${snapshot.exit_code}`);
  }
}

// --- error bar ---
function onStreamError(d) {
  if (!acceptsActiveSession(d)) return;
  finalizeAgent();
  finalizeThought('error');
  openThought = null;
  markTurnUnverified();
  finishWait('串流已中斷');
  turnOpen = false;
  turnHasUser = false;
  waitingForUser = false;
  runningTools.clear();
  pendingEditPaths.clear();
  last = null;
  settlePendingUser(false);
  tl.appendChild(
    el('div', 'errbar', `⚠ 串流錯誤：${(d && d.message) || '連線中斷'}`),
  );
  stick();
}

function onModelEscalation(d) {
  if (!d || (d.sessionId && d.sessionId !== state.sessionId)) return;
  const targetId = String(d.recommendedModel || '').trim();
  if (!targetId) return;
  const target = String(
    d.recommendedLabel || d.label || targetId || '建議模型',
  ).trim();
  const card = el('div', 'escalation-card');
  const body = el('div', 'escalation-copy');
  body.append(
    el('strong', '', `這個任務建議交給 ${target}`),
    el('span', '', '目前模型已保留現有證據，建議切換後繼續處理。'),
  );
  const action = el('button', 'btn primary', `切換到 ${target}`);
  action.addEventListener('click', () => {
    action.disabled = true;
    action.textContent = '正在切換…';
    bus.emit('model:escalate', {
      modelId: targetId,
      resumePrompt: String(d.resumePrompt || '').trim(),
      onError: () => {
        action.disabled = false;
        action.textContent = `切換到 ${target}`;
      },
    });
  });
  card.append(body, action);
  tl.appendChild(card);
  stick();
}

// --- per-turn edited-files card (before the completion pill) ---
function relPath(p) {
  if (p === '/workspace/session' || p.startsWith('/workspace/session/'))
    return '/session' + p.slice('/workspace/session'.length);
  if (p === '/session' || p.startsWith('/session/')) return p;
  if (p === '/workspace' || p.startsWith('/workspace/'))
    return p.slice('/workspace/'.length);
  let cwd = state.cwd || '';
  if (!cwd) return p;
  if (!cwd.endsWith('/')) cwd += '/';
  return p.startsWith(cwd) ? p.slice(cwd.length) : p;
}

function buildEditsCard(paths) {
  const card = el('div', 'editcard');
  const head = el('div', 'ec-head');
  // 標頭/按鈕＝TS AssistantChangedFilesSection：uppercase 小標 +「檢視差異」
  head.appendChild(el('span', 'ec-title', `已編輯 ${paths.length} 個檔案`));
  const open = el('button', 'ec-open', '檢視差異');
  open.addEventListener('click', () =>
    bus.emit('diff:open', { path: relPath(paths[0]) }),
  );
  head.appendChild(open);
  card.appendChild(head);
  // 每個檔名＝對話內超連結（用戶 12）：點檔名 → 側欄預覽（md 渲染／檔案預覽）；
  // 上面的「檢視差異」按鈕仍走 diff:open（e2e 依賴）。兩條路徑互不影響。
  for (const p of paths) {
    const rel = relPath(p);
    const row = el('div', 'ec-file');
    row.dataset.filePath = rel;
    const ic = el('span', 'fl-ic');
    ic.innerHTML = FILE_LINK_ICON;
    row.append(ic, el('span', 'fl-tx', rel));
    row.title = `預覽 ${rel}`;
    row.addEventListener('click', () =>
      bus.emit('preview:open', { path: rel }),
    );
    card.appendChild(row);
  }
  return card;
}

function verificationCorrections(corrections) {
  if (!Array.isArray(corrections)) return null;
  const records = corrections.filter((record) => record?.status === 'superseded' &&
    ['originalCallId', 'replacementCallId', 'correctionCallId', 'tool', 'reason']
      .every((key) => typeof record[key] === 'string' && record[key].trim()) &&
    Array.isArray(record.evidenceCallIds) && record.evidenceCallIds.length > 0 &&
    record.evidenceCallIds.every((id) => typeof id === 'string' && id.trim()));
  if (!records.length) return null;
  const details = el('details', 'tool-commentary verification-corrections');
  details.appendChild(el('summary', '', `更正測試 ${records.length} 項`));
  const body = el('div', 'verification-corrections-body');
  body.appendChild(el('p', '', '模型申報；更正理由未經獨立驗證。原始失敗紀錄仍保留。'));
  for (const record of records) {
    const entry = el('div', 'verification-correction');
    entry.append(
      el('p', '', `原始檢查（失敗） · ${record.tool} · ${record.originalCallId}`),
      el('p', '', `替代檢查 · ${record.replacementCallId}`),
      el('p', '', `更正紀錄 · ${record.correctionCallId}`),
      el('p', '', `更正理由 · ${record.reason}`),
      el('p', '', `依據呼叫 · ${record.evidenceCallIds.join('、')}`),
    );
    body.appendChild(entry);
  }
  details.appendChild(body);
  return details;
}

function onRunRecap(d) {
  if (!d || (d.sessionId && d.sessionId !== state.sessionId)) return;
  const recap = d.recap;
  const report = runRecapPresentation(recap);
  if (!report) return;
  const card = createRunRecapCard(report, verificationCorrections(recap.corrections));
  tl.appendChild(card);
  stick();
}

// --- end of turn: centered divider pill ---
function runDivider(stop, ms = 0) {
  const row = el('div', 'divider');
  const pill = el(
    'span',
    'pill' + (stop !== 'end_turn' && stop !== 'completed' ? ' bad' : ''),
  );
  pill.textContent =
    (REASONS[stop] || stop) + (ms > 0 ? ` · 耗時 ${fmtElapsed(ms)}` : '');
  row.append(el('div', 'ln'), pill, el('div', 'ln'));
  return row;
}

function appendHistoricalOutcomes(parent, outcomes) {
  if (!Array.isArray(outcomes)) return;
  for (const outcome of outcomes) {
    if (!outcome || !['finished', 'interrupted'].includes(outcome.status)) continue;
    const corrections = verificationCorrections(outcome.corrections);
    if (corrections) parent.appendChild(corrections);
    let stop = typeof outcome.stopReason === 'string' ? outcome.stopReason.trim() : '';
    // A terminal record is not necessarily a successful run. Unknown/missing
    // reasons and interrupted records must never inherit the live success default.
    if (outcome.status === 'interrupted' && (!Object.hasOwn(REASONS, stop) || ['end_turn', 'completed'].includes(stop))) {
      stop = 'interrupted';
    } else if (!Object.hasOwn(REASONS, stop)) {
      stop = 'unknown_stop';
    }
    const row = runDivider(stop);
    if (typeof outcome.timestamp === 'string') row.dataset.timestamp = outcome.timestamp;
    parent.appendChild(row);
  }
}

function onTurnDone(d) {
  if (!acceptsActiveSession(d)) return;
  finalizeAgent();
  const thought = thoughtViews.get(thoughtRunKey(d)) || openThought;
  if (thought) finishThinkingView(thought, d?.stopReason);
  openThought = null;
  const normalEnd =
    !d?.stopReason || ['end_turn', 'completed'].includes(d.stopReason);
  finishWait(normalEnd ? undefined : REASONS[d.stopReason] || '本輪已停止');
  waitingForUser = false;
  runningTools.clear();
  pendingEditPaths.clear();
  const stop = (d && d.stopReason) || 'end_turn';
  if (stop !== 'end_turn' && stop !== 'completed') markTurnUnverified();
  let ms = d && typeof d.elapsedMs === 'number' ? d.elapsedMs : 0;
  if (!ms && turnStart) ms = Date.now() - turnStart;
  turnOpen = false;
  turnStart = 0;
  last = null;
  turnHasUser = false;
  // 該輪耗時掛在最後一則 assistant 的 meta（TS formatMessageMeta：
  // 「下午2:03:38 • 17s」，串流中原本只顯時間）。
  if (lastAgentMeta && ms > 0) {
    const perf = lastAgentMeta.perf ? ` • ${lastAgentMeta.perf}` : '';
    lastAgentMeta.tsEl.textContent = `${lastAgentMeta.time} • ${fmtElapsed(ms)}${perf}`;
  }
  lastAgentMeta = null;
  settlePendingUser(false);
  // Re-linkify every assistant block now that turnEdits is complete: a file
  // written AFTER the text that names it only becomes a known target here.
  for (const mdEl of turnAgentEls) if (mdEl.isConnected) linkifyFiles(mdEl);
  turnAgentEls.length = 0;
  const edits = [...turnEdits]; // this turn's card only; next turn restarts
  turnEdits.clear();
  if (!tl.childElementCount) return; // nothing to terminate
  if (edits.length) tl.appendChild(buildEditsCard(edits));
  tl.appendChild(runDivider(stop, ms));
  stick();
}

// --- session switch: wipe the timeline ---
function clear() {
  nearby?.reset();
  sourceEpoch++;
  sourceRevision++;
  for (const request of previewRequests) request.abort();
  previewRequests.clear();
  for (const entry of sourceGalleries.values()) disposeSearchGallery(entry.gallery);
  sourceGalleries.clear();
  if (raf) {
    cancelAnimationFrame(raf);
    raf = 0;
  }
  hideWait(); // stops the elapsed-seconds interval too
  tl.textContent = '';
  toolRows.clear();
  runningTools.clear();
  pendingEditPaths.clear();
  turnEdits.clear();
  turnAgentEls.length = 0;
  openAgent = null;
  lastAgentMeta = null;
  openThought = null;
  thoughtViews.clear();
  thoughtFallbackRun = 0;
  curWl = null;
  last = null;
  turnOpen = false;
  turnStart = 0;
  latestInferenceProgress = null;
  turnHasUser = false;
  waitingForUser = false;
  pendingUserReply = null;
  resetProgressTrail();
  pinned = true;
  hideJump();
}

function loadHistory(messages) {
  clear();
  const list = Array.isArray(messages) ? messages : [];
  const frag = document.createDocumentFragment();
  let newestUser = null;
  let answeredAfterNewestUser = false;
  let historyRun = '';
  for (const message of list) {
    if (!message) continue;
    if (message.role === 'tool' && message.sourceComparison) {
      const report = sourceComparisonPresentation(message.sourceComparison);
      const card = createSourceComparison(report);
      if (card) {
        const row = el('div', 'arow');
        if (message.timestamp) row.dataset.timestamp = message.timestamp;
        row.append(card); frag.append(row);
        appendHistoricalOutcomes(frag, message.runOutcomes);
      }
      continue;
    }
    if (message.role === 'user') {
      historyRun = '';
      const built = userMessageRow(message, {
        restored: true,
        sessionId: state.sessionId,
      });
      if (message.timestamp) built.row.dataset.timestamp = message.timestamp;
      frag.appendChild(built.row);
      appendHistoricalOutcomes(frag, message.runOutcomes);
      newestUser = built;
      answeredAfterNewestUser = false;
      continue;
    }
    // Preserve the durable assistant bytes, including literal code whitespace.
    const text = rawContentText(message);
    const receipt = thinkingReceiptFromMessage(message);
    if (!text && !receipt) continue;
    if (text) answeredAfterNewestUser = true;
    const row = el('div', 'arow');
    if (message.timestamp) row.dataset.timestamp = message.timestamp;
    if (message.thought?.runId) historyRun = message.thought.runId;
    if (historyRun) row.dataset.historyRun = historyRun;
    if (receipt) {
      const view = createThinkingView(receipt);
      const saved = message.thought || message.thinking;
      if (saved && typeof saved === 'object' && saved.runId) {
        thoughtViews.set(thoughtRunKey(saved), view);
        view.sequence = Number.isSafeInteger(saved.sequence) ? saved.sequence : 0;
        if (receipt.state === 'thinking') openThought = view;
      }
      row.appendChild(view.d);
    }
    if (text) {
      const body = el('div', 'amd');
      renderAnswer(body, text);
      // An assistant message can include both prose and tool calls. Preserve
      // that operation note separately from a later final answer, even when
      // their text happens to be identical. Never infer this role from words.
      if (Number.isSafeInteger(message.toolCallCount) && message.toolCallCount > 0) {
        const note = el('details', 'tool-commentary');
        note.append(
          el('summary', '', `工具操作說明 · ${message.toolCallCount} 次呼叫`),
          body,
        );
        row.appendChild(note);
      } else {
        row.appendChild(body);
      }
    }
    frag.appendChild(row);
    appendHistoricalOutcomes(frag, message.runOutcomes);
  }
  tl.appendChild(frag);
  void hydrateSources();
  void nearby?.hydrate();
  if (newestUser) {
    pendingUserReply = {
      ...newestUser,
      answered: answeredAfterNewestUser,
      sessionId: state.sessionId,
    };
    settlePendingUser(false);
  }
  stick();
}

// A selected model is a preference, not a resident process. Cold startup keeps
// the composer usable; the first prompt activates the selected model behind
// the Agent boundary. Do not render a global "restoring model" banner or lock
// the send button while no model is resident.
function onModelStatus() {}

// Public hook for main.js/composer.js: call after daemon.prompt() settles.
export function notifyTurnDone(stopReason, elapsedMs) {
  bus.emit('turn:done', { stopReason, elapsedMs });
}

export function initTimeline() {
  tl = document.getElementById('timeline');
  scroller = document.getElementById('scroll');
  nearby = initNearby({ host: () => tl, place: placeSourceGallery });
  buildJump();

  scroller.addEventListener('scroll', () => {
    const dist =
      scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight;
    pinned = dist < 48;
    if (pinned) hideJump();
    else showJump();
  });

  bus.on('update:user_message_chunk', onUserChunk);
  bus.on('update:agent_message_chunk', onAgentChunk);
  bus.on('update:agent_thought_chunk', onThoughtChunk);
  bus.on('update:tool_call_draft', onToolDraft);
  bus.on('update:tool_call', onToolCall);
  bus.on('update:tool_call_update', onToolUpdate);
  bus.on('research_sources', (data) => {
    if (!data || data.sessionId !== state.sessionId) return;
    sourceRevision++;
    showSourceRecords(data.items, true);
  });
  bus.on('inference_progress', onInferenceProgress);
  bus.on('run_state', (data) => {
    if (!data || (data.sessionId && data.sessionId !== state.sessionId)) return;
    if (data.status === 'running' || data.status === 'cancelling') {
      turnOpen = true;
      if (!turnStart) turnStart = Date.now();
      if (!openAgent && !wait) showWait('background');
    } else if (!openAgent) {
      hideWait();
    }
  });
  bus.on('stream_error', onStreamError);
  bus.on('model_escalation', onModelEscalation);
  bus.on('run_recap', onRunRecap);
  bus.on('turn_completed', (data) => bus.emit('turn:done', data || {}));
  bus.on('turn:done', onTurnDone);
  bus.on('model_status', onModelStatus);
  // Waiting for a user decision is static, never an executing spinner.
  const pauseForUser = (data) => {
    if (!acceptsActiveSession(data)) return;
    finishWait('等待你的確認');
    waitingForUser = true;
  };
  bus.on('permission_request', pauseForUser);
  bus.on('user_input_request', pauseForUser);
  bus.on('location_request', pauseForUser);
  const resumeForUser = (data) => {
    if (acceptsActiveSession(data)) resumeWait();
  };
  bus.on('permission_resolved', resumeForUser);
  bus.on('user_input_resolved', resumeForUser);
  bus.on('location_resolved', resumeForUser);
  bus.on('history:load', loadHistory);
  bus.on('session_created', () => {
    clear();
    void hydrateSources();
    void nearby?.hydrate();
  });
  // sidebar.js emits this on every switch (new chat + existing thread);
  // without the reset, stale openAgent/curWl refs would point at nodes the
  // sidebar's innerHTML='' just detached and new chunks would render nowhere.
  bus.on('thread:switched', clear);
  // 'usage' is composer-owned (#ctxMeter); the timeline ignores it.
}
