/* native-background-ui */
// composer.js — composer interactions for the Wails desktop frontend.
// Owns: #input send/steer, footer 鈕列（模型置中 modal／核准模式 #pillMode .pop／
// 互動模式 #pillInteract .pop）, #checkoutRow 資料夾 pill, permission + user-input
// cards in #pendingArea, context ring in #ctxMeter.（M7 視覺對齊 TS ChatComposer /
// ProviderModelPicker / ModelPickerContent / ModelListRow）
// 核准模式（approval）與互動模式（interaction）是兩個正交 pill，但都寫同一個
// /approval 端點：applyMode(approval, interact) 是唯一仲裁者，規劃時送 plan 並鎖
// 核准模式 pill 顯示「規劃中」，回一般時補送回記住的核准模式值（見 initPills）。
// NOTE: #input 的 placeholder 由 timeline agent 依 model 狀態廣播覆寫
//（未載入=「請先選擇模型…（點左下的模型選單）」），本檔絕不寫入 placeholder。
import {
  daemon,
  bus,
  state,
  esc,
  ensureSession,
  documentMediaType,
  hasClientAccess,
  clientAccountGeneration,
} from './core.js?v=raven-execution-20261006a';
import { startNewThread } from './sidebar.js';
import { runLimitOptions, snapshotRunLimitPreferences } from './run_limits.js';
import { renderComposerQueue } from './composer_queue.js';
import { configuredPickerRows } from './model_picker_catalog.js';
import { ensureHarnessControlsReady } from './harness_controls.js?v=raven-execution-20261006a';
import {
  authorizeNewConversation,
  authorizePromptDelivery,
  consumePromptDelivery,
} from './commercial.js?v=desktop-send-20260904a';

let reflectArmedHook = null;
let refreshMemHook = null;
let restoreQueueDraftHook = null;

const $ = (s) => document.querySelector(s);
const CWD_KEY = 'meadow.cwd'; // must match main.js
const RESEARCH_MODE_KEY = 'meadow.researchMode';
const shortName = (p) => (p || '').split('/').filter(Boolean).pop() || p || '';

function fmtK(n) {
  n = Number(n) || 0;
  if (n < 1000) return String(n);
  const k = n / 1000;
  return (
    (k >= 100 ? String(Math.round(k)) : k.toFixed(1).replace(/\.0$/, '')) + 'k'
  );
}

function fmtContextCapacity(n) {
  return Number(n) === 131072 ? '128K' : fmtK(n);
}

// ---------- inline icons（lucide ISC 同款 path；Meadow 品牌 icon＝TS Icons.tsx） ----------
const svgIcon = (paths, sw = 2) =>
  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="${sw}" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${paths}</svg>`;
const ICONS = {
  meadow: svgIcon(
    '<path d="M1.5 17 C 4 13, 6 13, 8.5 16.5 C 10.5 19, 12 14, 14 13 C 16 12, 18 14, 20 16 C 21 17, 22 17, 22.5 17"/><path d="M1.5 20.5 L 22.5 20.5" opacity="0.4"/>',
    1.75,
  ),
  lock: svgIcon(
    '<rect width="18" height="11" x="3" y="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>',
  ),
  lockOpen: svgIcon(
    '<rect width="18" height="11" x="3" y="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 9.9-1"/>',
  ),
  pen: svgIcon(
    '<path d="M12 20h9"/><path d="M16.376 3.622a1 1 0 0 1 3.002 3.002L7.368 18.635a2 2 0 0 1-.855.506l-2.872.838a.5.5 0 0 1-.62-.62l.838-2.872a2 2 0 0 1 .506-.854z"/>',
  ),
  todo: svgIcon(
    '<rect x="3" y="5" width="6" height="6" rx="1"/><path d="m3 17 2 2 4-4"/><path d="M13 6h8"/><path d="M13 12h8"/><path d="M13 18h8"/>',
  ),
  bars: svgIcon(
    '<path d="M8 6h13"/><path d="M8 12h13"/><path d="M8 18h13"/><path d="M3 6h.01"/><path d="M3 12h.01"/><path d="M3 18h.01"/>',
  ),
  folder: svgIcon(
    '<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>',
  ),
  search: svgIcon('<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>'),
  star: svgIcon(
    '<path d="M11.525 2.295a.53.53 0 0 1 .95 0l2.31 4.679a2.123 2.123 0 0 0 1.595 1.16l5.166.756a.53.53 0 0 1 .294.904l-3.736 3.638a2.123 2.123 0 0 0-.611 1.878l.882 5.14a.53.53 0 0 1-.771.56l-4.618-2.428a2.122 2.122 0 0 0-1.973 0L6.396 21.01a.53.53 0 0 1-.77-.56l.881-5.139a2.122 2.122 0 0 0-.611-1.879L2.16 9.795a.53.53 0 0 1 .294-.906l5.165-.755a2.122 2.122 0 0 0 1.597-1.16z"/>',
  ),
};

// ---------- 附件（圖片／PDF／文件 → #attachStrip 卡片） ----------
// 圖片仍以 multipart image_url 送給 vision provider；PDF 由 Core 逐頁
// 渲染成 JPEG 後送進同一個 vision encoder，不做文字抽取。
const attachState = []; // image:{kind,url,...} | pdf/document:{kind,path|dataURL,...}
let attachSeq = 0;
let attachmentSessionRevision = 0;
let attachmentAccountRevision = 0;
let composerAccountGeneration = clientAccountGeneration();
const pendingAttachmentReaders = new Set();
const nativeDropOwners = new Map();
let clearAccountDraftsHook = null;
let imageCapabilityCheck = null;
let commitApprovalMode = async () => false;
const MAX_ATTACH = 4;
const MAX_PDF_BYTES = 64 * 1024 * 1024;
const MAX_DOCUMENT_BYTES = 20 * 1024 * 1024;
const MAX_BROWSER_IMAGE_BYTES = 1024 * 1024;
const MAX_BROWSER_IMAGE_DIMENSION = 32768;
const MAX_BROWSER_IMAGE_PIXELS = 25_000_000;
// Keep ordinary screenshots materially legible for OCR and visual comparison.
// We never upscale a source, but large inputs begin normalization at up to 2K
// wide instead of the old thumbnail-sized 512 px path.
const MAX_BROWSER_IMAGE_START_WIDTH = 2048;

const attXIcon = svgIcon('<path d="M18 6 6 18"/><path d="m6 6 12 12"/>');

function attachmentOwnerCurrent(session, revision) {
  return hasClientAccess() && state.sessionId === session &&
    attachmentSessionRevision === revision;
}

function revokeComposerAccess() {
  const generation = clientAccountGeneration();
  if (hasClientAccess() && generation === composerAccountGeneration) return;
  composerAccountGeneration = generation;
  attachmentSessionRevision++;
  attachmentAccountRevision++;
  nativeDropOwners.clear();
  for (const reader of pendingAttachmentReaders) reader.abort();
  pendingAttachmentReaders.clear();
  attachState.length = 0;
  const picker = $('#imageAttachInput');
  if (picker) picker.value = '';
  $('#composer')?.classList.remove('dragover');
  clearAccountDraftsHook?.();
  renderAttachStrip();
}

bus.on('authentication_required', revokeComposerAccess);
window.addEventListener('commercial:change', revokeComposerAccess);

function renderAttachStrip() {
  const strip = $('#attachStrip');
  if (!strip) return;
  if (reflectArmedHook) reflectArmedHook();
  strip.innerHTML = '';
  for (const a of attachState) {
    const card = document.createElement('div');
    card.className = 'att-card';
    card.title = a.name;
    let preview;
    if (a.kind === 'pdf' || a.kind === 'document') {
      preview = document.createElement('span');
      preview.className = 'att-thumb att-pdf';
      preview.textContent =
        a.kind === 'pdf'
          ? 'PDF'
          : (a.name.split('.').pop() || '文件').toUpperCase();
      preview.setAttribute('aria-hidden', 'true');
    } else {
      preview = document.createElement('img');
      preview.className = 'att-thumb';
      preview.src = a.url;
      preview.alt = a.name;
    }
    const nm = document.createElement('span');
    nm.className = 'att-name';
    nm.textContent = a.name;
    const rm = document.createElement('button');
    rm.type = 'button';
    rm.className = 'att-x';
    rm.title = '移除';
    rm.setAttribute('aria-label', '移除附件');
    rm.innerHTML = attXIcon;
    rm.addEventListener('click', () => {
      const i = attachState.findIndex((x) => x.id === a.id);
      if (i >= 0) attachState.splice(i, 1);
      renderAttachStrip();
      restoreQueueDraftHook?.();
    });
    card.appendChild(preview);
    card.appendChild(nm);
    card.appendChild(rm);
    strip.appendChild(card);
  }
}

function dataURLPayloadBytes(url) {
  const comma = String(url || '').indexOf(',');
  if (comma < 0) return 0;
  const payload = String(url).slice(comma + 1);
  return (
    Math.floor((payload.length * 3) / 4) -
    (payload.endsWith('==') ? 2 : payload.endsWith('=') ? 1 : 0)
  );
}

function fileDataURL(file) {
  return new Promise((resolve, reject) => {
    if (!hasClientAccess()) { reject(new Error('請先登入 Meadow')); return; }
    const reader = new FileReader();
    pendingAttachmentReaders.add(reader);
    const finish = (callback, value) => {
      pendingAttachmentReaders.delete(reader);
      callback(value);
    };
    reader.onerror = () => finish(reject, reader.error || new Error('檔案讀取失敗'));
    reader.onabort = () => finish(reject, new Error('附件讀取已中止'));
    reader.onload = () => finish(resolve, String(reader.result || ''));
    try { reader.readAsDataURL(file); }
    catch (error) { finish(reject, error); }
  });
}

function loadImage(url) {
  return new Promise((resolve, reject) => {
    const image = new Image();
    image.onload = () => resolve(image);
    image.onerror = () => reject(new Error('圖片解碼失敗'));
    image.src = url;
  });
}

function browserImageDimensions(width, height) {
  const safeWidth = Math.max(1, Number(width) || 1);
  const safeHeight = Math.max(1, Number(height) || 1);
  const scale = Math.min(
    1,
    MAX_BROWSER_IMAGE_START_WIDTH / safeWidth,
    MAX_BROWSER_IMAGE_DIMENSION / safeWidth,
    MAX_BROWSER_IMAGE_DIMENSION / safeHeight,
    Math.sqrt(MAX_BROWSER_IMAGE_PIXELS / (safeWidth * safeHeight)),
  );
  return {
    width: Math.max(1, Math.floor(safeWidth * scale)),
    height: Math.max(1, Math.floor(safeHeight * scale)),
  };
}

function browserImageGeometryIsSafe(width, height) {
  return (
    width > 0 &&
    height > 0 &&
    width <= MAX_BROWSER_IMAGE_DIMENSION &&
    height <= MAX_BROWSER_IMAGE_DIMENSION &&
    width * height <= MAX_BROWSER_IMAGE_PIXELS
  );
}

// Preserve already-bounded PNG/JPEG bytes. Every other input, including small
// GIF/WebP files, is decoded through canvas and emitted as a bounded JPEG so
// the first request and durable resume path share the same format contract.
async function normalizeBrowserImage(file, isCurrent = hasClientAccess) {
  if (!isCurrent()) return null;
  const original = await fileDataURL(file);
  if (!isCurrent()) return null;
  const image = await loadImage(original);
  if (!isCurrent()) return null;
  const naturalWidth = Number(image.naturalWidth) || 0;
  const naturalHeight = Number(image.naturalHeight) || 0;
  if (naturalWidth <= 0 || naturalHeight <= 0) {
    throw new Error('圖片尺寸無效');
  }
  const inputType = String(file.type || '').toLowerCase();
  const mayPreserveOriginal =
    (inputType === 'image/png' && original.startsWith('data:image/png;')) ||
    (inputType === 'image/jpeg' && original.startsWith('data:image/jpeg;'));
  if (
    mayPreserveOriginal &&
    browserImageGeometryIsSafe(naturalWidth, naturalHeight) &&
    dataURLPayloadBytes(original) <= MAX_BROWSER_IMAGE_BYTES
  ) {
    return {
      url: original,
      width: naturalWidth,
      height: naturalHeight,
    };
  }
  let { width, height } = browserImageDimensions(naturalWidth, naturalHeight);
  for (let attempt = 0; attempt < 12; attempt++) {
    const canvas = document.createElement('canvas');
    canvas.width = width;
    canvas.height = height;
    const ctx = canvas.getContext('2d', { alpha: false });
    if (!ctx) throw new Error('無法建立圖片壓縮畫布');
    ctx.fillStyle = '#ffffff';
    ctx.fillRect(0, 0, width, height);
    ctx.drawImage(image, 0, 0, width, height);
    for (const quality of [0.9, 0.82, 0.72, 0.62, 0.52, 0.42, 0.32]) {
      const candidate = canvas.toDataURL('image/jpeg', quality);
      if (dataURLPayloadBytes(candidate) <= MAX_BROWSER_IMAGE_BYTES) {
        return { url: candidate, width, height };
      }
    }
    if (width === 1 && height === 1) break;
    width = Math.max(1, Math.floor(width * 0.85));
    height = Math.max(1, Math.floor(height * 0.85));
  }
  throw new Error('圖片壓縮後仍超過 1 MB');
}

async function addImageFile(file) {
  if (!hasClientAccess()) return;
  if (!file || !/^image\//.test(file.type)) return;
  if (attachState.length >= MAX_ATTACH) {
    toast(`一次最多附 ${MAX_ATTACH} 個檔案`, false);
    return;
  }
  const selectedModel = state.model;
  const selectedSession = state.sessionId;
  const selectedRevision = attachmentSessionRevision;
  try {
    if (!(await verifyImageCapability(selectedModel))) return;
    const isCurrent = () => attachmentOwnerCurrent(selectedSession, selectedRevision);
    if (!isCurrent()) return;
    const normalized = await normalizeBrowserImage(file, isCurrent);
    if (!normalized || !isCurrent()) return;
    if (state.model !== selectedModel) {
      toast('模型已變更，請重新附加圖片', false);
      return;
    }
    if (attachState.length >= MAX_ATTACH) return;
    attachState.push({
      id: ++attachSeq,
      kind: 'image',
      url: normalized.url,
      name: file.name || '貼上的圖片',
      width: normalized.width,
      height: normalized.height,
    });
    renderAttachStrip();
  } catch (e) {
    if (!attachmentOwnerCurrent(selectedSession, selectedRevision)) return;
    console.error('image attachment failed:', e);
    toast(`圖片無法附加：${e.message || e}`, false);
  }
}

async function addPDFFile(file) {
  if (!hasClientAccess()) return;
  if (
    !file ||
    (file.type !== 'application/pdf' && !/\.pdf$/i.test(file.name || ''))
  )
    return;
  if (attachState.length >= MAX_ATTACH) {
    toast(`一次最多附 ${MAX_ATTACH} 個檔案`, false);
    return;
  }
  if (
    !Number.isFinite(file.size) ||
    file.size <= 0 ||
    file.size > MAX_PDF_BYTES
  ) {
    toast('PDF 必須小於 64 MB', false);
    return;
  }
  const selectedSession = state.sessionId;
  const selectedRevision = attachmentSessionRevision;
  try {
    const pdfBlob =
      file.type === 'application/pdf'
        ? file
        : new Blob([file], { type: 'application/pdf' });
    const dataURL = await fileDataURL(pdfBlob);
    if (!attachmentOwnerCurrent(selectedSession, selectedRevision)) return;
    if (!dataURL.startsWith('data:application/pdf;base64,')) {
      throw new Error('PDF 格式無效');
    }
    if (attachState.length >= MAX_ATTACH) return;
    attachState.push({
      id: ++attachSeq,
      kind: 'pdf',
      dataURL,
      name: file.name || '附件.pdf',
      type: 'application/pdf',
    });
    renderAttachStrip();
  } catch (error) {
    if (!attachmentOwnerCurrent(selectedSession, selectedRevision)) return;
    console.error('PDF attachment failed:', error);
    toast(`PDF 無法附加：${error.message || error}`, false);
  }
}

async function addDocumentFile(file) {
  if (!hasClientAccess()) return;
  const mediaType = documentMediaType(file?.name);
  if (!file || !mediaType) return;
  if (attachState.length >= MAX_ATTACH) {
    toast(`一次最多附 ${MAX_ATTACH} 個檔案`, false);
    return;
  }
  if (
    !Number.isFinite(file.size) ||
    file.size <= 0 ||
    file.size > MAX_DOCUMENT_BYTES
  ) {
    toast('文件必須大於 0 且不超過 20 MB', false);
    return;
  }
  const selectedSession = state.sessionId;
  const selectedRevision = attachmentSessionRevision;
  try {
    const blob =
      file.type === mediaType ? file : new Blob([file], { type: mediaType });
    const dataURL = await fileDataURL(blob);
    if (!attachmentOwnerCurrent(selectedSession, selectedRevision)) return;
    if (!dataURL.startsWith(`data:${mediaType};base64,`)) {
      throw new Error('文件格式無效');
    }
    if (attachState.length >= MAX_ATTACH) return;
    attachState.push({
      id: ++attachSeq,
      kind: 'document',
      dataURL,
      name: file.name,
      type: mediaType,
    });
    renderAttachStrip();
  } catch (error) {
    if (!attachmentOwnerCurrent(selectedSession, selectedRevision)) return;
    console.error('document attachment failed:', error);
    toast(`文件無法附加：${error.message || error}`, false);
  }
}

async function addNativeDroppedImage(image) {
  if (!hasClientAccess()) return;
  const url = String((image && image.url) || '');
  if (!url.startsWith('data:image/')) return;
  const selectedSession = state.sessionId;
  const selectedRevision = attachmentSessionRevision;
  try {
    const response = await fetch(url);
    if (!attachmentOwnerCurrent(selectedSession, selectedRevision)) return;
    const blob = await response.blob();
    if (!attachmentOwnerCurrent(selectedSession, selectedRevision)) return;
    await addImageFile(
      new File([blob], String(image.name || '拖放的圖片'), {
        type: String(image.type || blob.type || 'image/png'),
      }),
    );
  } catch (error) {
    if (!attachmentOwnerCurrent(selectedSession, selectedRevision)) return;
    console.error('native image drop failed:', error);
    toast(`圖片無法附加：${error.message || error}`, false);
  }
}

function addNativeDroppedPDF(pdf) {
  if (!hasClientAccess()) return;
  const path = String((pdf && pdf.path) || '');
  if (!path || attachState.length >= MAX_ATTACH) return;
  attachState.push({
    id: ++attachSeq,
    kind: 'pdf',
    path,
    name: String(pdf.name || '附件.pdf'),
    type: 'application/pdf',
  });
  renderAttachStrip();
}

function addNativeDroppedDocument(document) {
  if (!hasClientAccess()) return;
  const path = String(document?.path || '');
  const name = String(document?.name || '');
  const mediaType = documentMediaType(name);
  if (!path || !mediaType || attachState.length >= MAX_ATTACH) return;
  attachState.push({
    id: ++attachSeq,
    kind: 'document',
    path,
    name,
    type: mediaType,
  });
  renderAttachStrip();
}

// drop / paste 的共用入口：圖片、PDF 和文件都收進附件列。
function ingestAttachmentFiles(files) {
  if (!hasClientAccess()) return Boolean(files?.length);
  let any = false;
  let hasImage = false;
  for (const f of files || []) {
    if (f && /\.xls$/i.test(f.name || '')) {
      toast('XLS 尚不支援，請先轉成 XLSX 或 CSV', false);
      any = true;
    } else if (f && documentMediaType(f.name)) {
      addDocumentFile(f);
      any = true;
    } else if (f && /^image\//.test(f.type)) {
      hasImage = true;
      addImageFile(f);
      any = true;
    } else if (
      f &&
      (f.type === 'application/pdf' || /\.pdf$/i.test(f.name || ''))
    ) {
      addPDFFile(f);
      any = true;
    }
  }
  // Even when the model rejects the image, consume the drop/paste event so a
  // browser does not navigate away from the Desktop composer.
  if (hasImage && !currentModelSupportsImages()) return true;
  return any;
}

// 取走目前附件的 dataURL（並清空附件列）；送出時呼叫
function takeAttachments() {
  const attachments = attachState.map((a) =>
    a.kind === 'pdf' || a.kind === 'document'
      ? {
          kind: a.kind,
          name: a.name,
          type: a.type,
          path: a.path,
          dataURL: a.dataURL,
        }
      : a.url,
  );
  attachState.length = 0;
  renderAttachStrip();
  return attachments;
}

function restoreAttachments(attachments) {
  if (!hasClientAccess()) return;
  for (const attachment of attachments || []) {
    if (typeof attachment === 'string' && attachment) {
      attachState.push({
        id: ++attachSeq,
        kind: 'image',
        url: attachment,
        name: '圖片',
      });
      continue;
    }
    if (!attachment || !['pdf', 'document'].includes(attachment.kind)) continue;
    const name =
      attachment.name || (attachment.kind === 'pdf' ? '附件.pdf' : '附件.txt');
    const type =
      attachment.kind === 'pdf' ? 'application/pdf' : documentMediaType(name);
    if (!type) continue;
    attachState.push({
      id: ++attachSeq,
      kind: attachment.kind,
      name,
      type,
      path: attachment.path,
      dataURL: attachment.dataURL,
    });
  }
  renderAttachStrip();
}

const hasFiles = (dt) => !!dt && Array.from(dt.types || []).includes('Files');

// composer 卡片整體吃拖放 + 貼上；dragover 高亮（.dragover）
function initAttachments() {
  const composer = $('#composer');
  const input = $('#input');
  const picker = $('#imageAttachInput');
  const pickerButton = $('#attachImageBtn');
  if (!composer) return;
  renderAttachStrip();
  bus.on('thread:switched', () => {
    attachmentSessionRevision++;
  });

  const nativeDropID = (payload) => {
    if (typeof payload.drop_id !== 'string') return '';
    const id = payload.drop_id.trim();
    return id.length <= 128 ? id : '';
  };
  let nativeDropBridgeRegistered = false;
  const registerNativeDropBridge = (events = window.wails?.Events) => {
    if (nativeDropBridgeRegistered) return true;
    if (!events || typeof events.On !== 'function') return false;
    events.On('meadow:files-drop-started', (event) => {
      if (!hasClientAccess()) return;
      const id = nativeDropID((event && event.data) || {});
      if (!id || nativeDropOwners.has(id)) return;
      // Native reads may complete after the user switches threads. Keep a
      // bounded owner ledger and never rebind a duplicate start to a new owner.
      while (nativeDropOwners.size >= 32) {
        nativeDropOwners.delete(nativeDropOwners.keys().next().value);
      }
      nativeDropOwners.set(id, {
        sessionId: state.sessionId,
        revision: attachmentSessionRevision,
      });
    });
    events.On('meadow:image-files-dropped', (event) => {
      if (!hasClientAccess()) return;
      const payload = (event && event.data) || {};
      // Completion must belong to a start admitted in this account/session.
      // An untracked legacy event cannot prove which account read its files.
      const id = nativeDropID(payload);
      const owner = nativeDropOwners.get(id);
      nativeDropOwners.delete(id);
      if (!owner || !attachmentOwnerCurrent(owner.sessionId, owner.revision)) return;
      for (const image of payload.images || [])
        void addNativeDroppedImage(image);
      for (const pdf of payload.pdfs || []) addNativeDroppedPDF(pdf);
      for (const document of payload.documents || [])
        addNativeDroppedDocument(document);
      for (const reason of payload.rejected || []) toast(String(reason), false);
    });
    nativeDropBridgeRegistered = true;
    return true;
  };
  if (!registerNativeDropBridge() && location.protocol === 'wails:') {
    void import('/wails/runtime.js')
      .then((runtime) => registerNativeDropBridge(runtime.Events))
      .catch((error) =>
        console.error('native image drop bridge failed:', error),
      );
  }

  if (picker && pickerButton) {
    pickerButton.addEventListener('click', () => {
      if (!hasClientAccess()) return;
      picker.value = '';
      picker.click();
    });
    picker.addEventListener('change', () => {
      ingestAttachmentFiles(picker.files || []);
      picker.value = '';
    });
  }

  let dragDepth = 0; // enter/leave 在子元素間冒泡，用深度計數避免閃爍
  composer.addEventListener('dragenter', (e) => {
    if (!hasFiles(e.dataTransfer)) return;
    e.preventDefault();
    if (!hasClientAccess()) return;
    dragDepth++;
    composer.classList.add('dragover');
  });
  composer.addEventListener('dragover', (e) => {
    if (!hasFiles(e.dataTransfer)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = hasClientAccess() ? 'copy' : 'none';
  });
  composer.addEventListener('dragleave', () => {
    dragDepth = Math.max(0, dragDepth - 1);
    if (dragDepth === 0) composer.classList.remove('dragover');
  });
  composer.addEventListener('drop', (e) => {
    dragDepth = 0;
    composer.classList.remove('dragover');
    const files = e.dataTransfer && e.dataTransfer.files;
    if (files && files.length && ingestAttachmentFiles(files))
      e.preventDefault();
  });

  // 貼上附件（clipboard items 內的 file）。
  if (input) {
    input.addEventListener('paste', (e) => {
      if (!hasClientAccess()) { e.preventDefault(); return; }
      const items = (e.clipboardData && e.clipboardData.items) || [];
      const files = [];
      for (const it of items) {
        if (it.kind === 'file') {
          const f = it.getAsFile();
          if (f) files.push(f);
        }
      }
      if (files.length && ingestAttachmentFiles(files)) e.preventDefault();
    });
  }
}

// ---------- send / steer ----------
function initInput() {
  const input = $('#input');
  const btn = $('#sendBtn');
  const stopBtn = $('#stopBtn');
  const runningSessions = new Set();
  const pendingSends = new Map();
  const stopEpochs = new Map();
  let queueItems = [];
  let queuePaused = false;
  let queueRevision = 0;
  const queueActions = new Set();
  const recoveredDrafts = new Map();
  let restoredSubmission = null;
  const newSubmissionID = () => globalThis.crypto.randomUUID();
  clearAccountDraftsHook = () => {
    recoveredDrafts.clear();
    restoredSubmission = null;
    for (const pending of pendingSends.values()) pending.cancelled = true;
    queueRevision++;
    queueItems = [];
    queueActions.clear();
    renderTaskQueue();
  };
  const deferDraft = (sid, draft) => {
    const drafts = recoveredDrafts.get(sid) || [];
    drafts.push(draft);
    recoveredDrafts.set(sid, drafts);
  };
  const renderTaskQueue = () =>
    renderComposerQueue($('#taskQueue'), queueItems, queueActions, queueAction, queuePaused);
  let lastSendIntent = { signature: '', at: 0 };

  const replaceQueueItems = (items, { reset = false, paused = queuePaused } = {}) => {
    queuePaused = paused === true;
    const incoming = Array.isArray(items) ? items : [];
    if (reset) {
      queueItems = incoming;
      renderTaskQueue();
      return;
    }
    // POST receipts and SSE queue updates travel independently. A slow POST
    // may still contain `pending` after the SSE stream has already promoted
    // the same item to `running`. Never let that older receipt resurrect the
    // editable queue row above a user message that is already executing.
    const previousByID = new Map(queueItems.map((item) => [item.id, item]));
    queueItems = incoming.map((item) => {
      const previous = previousByID.get(item.id);
      if (
        previous &&
        (previous.status === 'running' || previous.status === 'cancelling') &&
        item.status === 'pending'
      ) {
        return {
          ...item,
          status: previous.status,
          startedAt: previous.startedAt || item.startedAt,
        };
      }
      return item;
    });
    renderTaskQueue();
  };

  // A response from an earlier session or before newer SSE cannot resurrect
  // consumed guidance or replace the queue in a different conversation.
  const applyQueueReceipt = (sid, revision, receipt) => {
    if (sid !== state.sessionId || revision !== queueRevision) return;
    if (!Array.isArray(receipt?.items)) throw new Error('排隊回執不完整');
    replaceQueueItems(receipt.items, { paused: receipt.paused });
    syncBusy();
    reflectBusy();
  };

  const restoreQueueDraft = () => {
    if (!hasClientAccess()) return;
    const sid = state.sessionId;
    const drafts = recoveredDrafts.get(sid);
    const draft = drafts?.[0];
    if (!draft || input.value.trim() || attachState.length) return;
    drafts.shift();
    if (!drafts.length) recoveredDrafts.delete(sid);
    input.value = draft.text || '';
    restoreAttachments(draft.attachments || [...(draft.images || []), ...(draft.files || [])]);
    restoredSubmission = draft.submissionRequestID ? {
      requestID: draft.submissionRequestID, sessionID: sid,
      text: input.value, attachmentIDs: attachState.map((item) => item.id).join(','),
      limits: draft.limits ? { ...draft.limits } : null, deliveryKind: draft.deliveryKind,
    } : null;
    autoGrow();
    input.focus();
  };
  restoreQueueDraftHook = restoreQueueDraft;
  const restoreFailedDraft = (sid, text, attachments, accountRevision, submissionRequestID, limits, deliveryKind) => {
    if (!hasClientAccess() || accountRevision !== attachmentAccountRevision) return;
    deferDraft(sid, {
      text, submissionRequestID, limits: limits ? { ...limits } : null, deliveryKind,
      attachments: (attachments || []).slice(),
      images: (attachments || []).filter(
        (attachment) => typeof attachment === 'string',
      ),
      files: (attachments || []).filter(
        (attachment) => attachment && typeof attachment === 'object',
      ),
    });
    if (sid === state.sessionId) restoreQueueDraft();
  };

  async function queueAction(action, id, target) {
    if (!hasClientAccess()) return;
    const accountRevision = attachmentAccountRevision;
    const sid = state.sessionId;
    if (!sid || queueActions.size) return;
    if (
      action === 'edit' &&
      (input.value.trim() || attachState.length || recoveredDrafts.has(sid))
    ) {
      toast('輸入框已有草稿，請先送出或清空再編輯排隊訊息', false);
      return;
    }
    queueActions.add(id);
    renderTaskQueue();
    const revision = queueRevision;
    try {
      let receipt;
      if (action === 'guide') {
        receipt = await daemon.queueGuide(sid, id);
        if (
          receipt?.id !== id ||
          receipt.mode !== 'steer_current' ||
          receipt.disposition !== 'accepted'
        )
          throw new Error('引導回執不完整');
      } else if (action === 'edit') {
        receipt = await daemon.queueEdit(sid, id);
        if (!hasClientAccess() || accountRevision !== attachmentAccountRevision) return;
        if (!receipt?.draft) throw new Error('編輯回執不完整');
        // Keep a returned draft with its own session if the user switched or
        // began typing while the request was in flight; never overwrite it.
        deferDraft(sid, receipt.draft);
        if (state.sessionId === sid) restoreQueueDraft();
      } else if (action === 'cancel') {
        receipt = await daemon.queueCancel(sid, id);
      } else if (action === 'reorder') {
        receipt = await daemon.queueReorder(sid, id, target);
      } else {
        receipt = await daemon.queueMove(sid, id, action === 'up' ? -1 : 1);
      }
      applyQueueReceipt(sid, revision, receipt);
    } catch (e) {
      toast(`排隊操作失敗：${e.message}`, false);
    } finally {
      queueActions.delete(id);
      renderTaskQueue();
    }
  }

  const claimSendIntent = (text, hasImages) => {
    const signature = `${state.sessionId || 'new'}\u0000${text}\u0000${hasImages ? attachState.length : 0}`;
    const now = performance.now();
    if (
      signature === lastSendIntent.signature &&
      now - lastSendIntent.at < 750
    ) {
      return false;
    }
    lastSendIntent = { signature, at: now };
    return true;
  };

  const queueRunning = () =>
    queueItems.some(
      (item) => item.status === 'running' || item.status === 'cancelling',
    );

  const syncBusy = () => {
    state.busy = runningSessions.has(state.sessionId) || queueRunning();
  };

  stopBtn.addEventListener('click', async () => {
    if (!state.sessionId) return;
    const sid = state.sessionId;
    stopEpochs.set(sid, (stopEpochs.get(sid) || 0) + 1);
    const pending = pendingSends.get(sid);
    if (pending) pending.cancelled = true;
    stopBtn.disabled = true;
    try {
      await daemon.cancel(sid);
      // busy flips off when the blocking prompt resolves (stopReason
      // "cancelled"); nothing else to do here.
    } catch (e) {
      console.error('cancel failed:', e);
      toast('停止請求失敗，請再按一次：' + e.message, false);
    } finally {
      stopBtn.disabled = false;
    }
  });

  const reflectBusy = () => {
    (function reflectMemberActivity(document, active) {
      document.documentElement.toggleAttribute(
        'data-member-task-active',
        Boolean(active),
      );
      for (const id of ['btnProgress', 'mobileWorkToggle']) {
        const button = document.getElementById(id);
        if (!button) continue;
        button.dataset.memberIdleTitle ??= button.title || '成果與進度';
        button.title = active
          ? '任務執行中 · 點擊查看進度'
          : button.dataset.memberIdleTitle;
      }
    })(document, state.busy);
    // Keep hard cancellation available even while composing guidance/queued text.
    // Sending still queues; it never implicitly cancels or steers the run.
    const has = !!input.value.trim() || attachState.length > 0;
    const showStop = !!state.busy && !has;
    btn.hidden = showStop;
    stopBtn.hidden = !state.busy;
    btn.title = state.busy ? '排入下一回合' : '送出';
  };

  const autoGrow = () => {
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight, 200) + 'px';
    input.style.overflowY = input.scrollHeight > 200 ? 'auto' : 'hidden';
    reflectArmed();
    reflectBusy();
  };

  // 送出鈕：空輸入＝灰、有文字或附件＝綠（armed）
  const reflectArmed = () => {
    const has = !!input.value.trim() || attachState.length > 0;
    btn.classList.toggle('armed', has);
  };
  reflectArmedHook = () => {
    reflectArmed();
    reflectBusy();
  };
  reflectArmed();

  // 點 composer 卡片任何空白處都聚焦輸入框（用戶回饋：原本只有
  // textarea 本身可點）。互動元素（pill／按鈕／附件卡）不搶。
  const composerCard = $('#composer');
  if (composerCard) {
    composerCard.addEventListener('click', (e) => {
      if (e.target === input) return;
      if (e.target.closest('button, a, kbd, .pill, .att-card, textarea'))
        return;
      input.focus();
    });
    // Give WebKit accessibility activation a native HTML submit path. Some
    // AX versions focus a button but do not deliver its JavaScript click
    // listener; a real submit button still dispatches the form contract.
    composerCard.addEventListener('submit', (e) => {
      e.preventDefault();
      void send();
    });
  }

  async function runPrompt(sid, text, attachments, limits, submissionRequestID = newSubmissionID(), evoResumeReflectionRunId = '') {
    if (!hasClientAccess()) return;
    const accountRevision = attachmentAccountRevision;
    const selectionRevision = attachmentSessionRevision;
    const needsEffortLoad = !limits &&
      (!inferenceControls || inferenceControls.maturity === 'error');
    const preferences = !limits ? snapshotRunLimitPreferences() : null;
    let effort = inferenceControls?.reasoningEffort;
    limits ||= runLimitOptions(effort, preferences);
    limits.mode ||= state.researchMode ? 'research' : 'auto';
    if (evoResumeReflectionRunId) limits.evoResumeReflectionRunId = evoResumeReflectionRunId;
    sweepResolvedCards();
    let accepted = false;
    const pending = { cancelled: false };
    pendingSends.set(sid, pending);
    runningSessions.add(sid);
    if (state.sessionId === sid) {
      state.busy = true;
      reflectBusy();
    }
    try {
      if (needsEffortLoad) {
        await loadInferenceControls();
        if (!pending.cancelled) {
          if (state.sessionId !== sid || !inferenceControls || inferenceControls.maturity === 'error')
            throw new Error('無法讀取任務額度設定，請稍後重試');
          effort = inferenceControls.reasoningEffort;
        }
      }
      const harness = await ensureHarnessControlsReady(sid);
      if (preferences) limits.runTokenLimit = runLimitOptions(effort, preferences, harness?.effectiveMode || '').runTokenLimit;
      if (state.sessionId !== sid || attachmentSessionRevision !== selectionRevision)
        throw new Error('對話已切換，訊息尚未送出');
      const authorization = await authorizePromptDelivery(sid, submissionRequestID);
      // A hard Stop during authorization must not launch a delayed prompt.
      if (pending.cancelled || state.sessionId !== sid ||
          attachmentSessionRevision !== selectionRevision) {
        restoreFailedDraft(sid, text, attachments, accountRevision, submissionRequestID, limits, 'prompt');
      } else {
        const result = await daemon.prompt(sid, text, attachments, limits, authorization);
        accepted = result?.local !== true;
        if (result?.local === true && result.dispatched === false)
          restoreFailedDraft(sid, text, attachments, accountRevision, submissionRequestID, limits, 'prompt');
        if (accepted) void consumePromptDelivery(authorization).catch(() => {});
      }
    } catch (e) {
      console.error('prompt failed:', e);
      if (accepted) {
        toast(
          e?.userMessage || '用量回執同步失敗；後續訊息保持鎖定，請重新連線',
          false,
        );
      }
      // Restore cards so a failed send does not silently eat attachments.
      if (!accepted) {
        restoreFailedDraft(sid, text, attachments, accountRevision, submissionRequestID, limits, 'prompt');
        toast(
          e?.userMessage ||
            `訊息未送出：${e?.message || '無法連線至本機引擎，請檢查模型狀態後重試'}`,
          false,
        );
      }
    }
    if (pendingSends.get(sid) === pending) pendingSends.delete(sid);
    runningSessions.delete(sid);
    if (state.sessionId === sid) {
      syncBusy();
      reflectBusy();
      restoreQueueDraft();
    }
  }

  async function send() {
    if (!hasClientAccess()) return;
    const accountRevision = attachmentAccountRevision;
    const controlsAtIntent = inferenceControls;
    const preferencesAtIntent = snapshotRunLimitPreferences();
    let effortAtIntent = controlsAtIntent?.reasoningEffort;
    let limits = { ...runLimitOptions(effortAtIntent, preferencesAtIntent),
      mode: state.researchMode ? 'research' : 'auto' };
    const sessionAtIntent = state.sessionId;
    const stopEpochAtIntent = stopEpochs.get(sessionAtIntent) || 0;
    const sessionRevision = attachmentSessionRevision;
    const cwdAtIntent = state.cwd;
    const rawDraftAtIntent = input.value;
    const attachmentIDsAtIntent = attachState.map((item) => item.id).join(',');
    const text = input.value.trim();
    const hasAttachments = attachState.length > 0;
    const editionProfile = { '/badai': 'skull', '/goodai': 'standard' }[
      text.toLowerCase()
    ];
    if (editionProfile) {
      input.value = '';
      autoGrow();
      try {
        toast(
          editionProfile === 'skull' ? '正在切換模式…' : '正在恢復標準模式…',
          true,
        );
        const result = await daemon.switchEdition(editionProfile);
        if (!hasClientAccess() || accountRevision !== attachmentAccountRevision) return;
        if (!result || result.restarting !== true) {
          localStorage.removeItem(MODEL_CACHE_KEY);
          modelCache = [];
          await refetchModels();
          if (!hasClientAccess() || accountRevision !== attachmentAccountRevision) return;
          toast(
            editionProfile === 'skull' ? '已切換為 💀 版本' : '已恢復正式版本',
            true,
          );
        }
      } catch (error) {
        if (!hasClientAccess() || accountRevision !== attachmentAccountRevision) return;
        if (!input.value) input.value = text;
        autoGrow();
        toast(error?.message || '模式切換失敗');
      }
      return;
    }
    // 附件可獨立成訊息：無文字也允許送出。
    if (!text && !hasAttachments) return;
    // Wails/WebKit accessibility Return can reach both the textarea and the
    // document fallback during the same key gesture. Claim that gesture before
    // the first await so it creates exactly one prompt instead of a prompt plus
    // an accidental queued duplicate.
    if (!claimSendIntent(text, hasAttachments)) return;
    const recoveredSubmission = restoredSubmission &&
      restoredSubmission.sessionID === sessionAtIntent &&
      restoredSubmission.text === rawDraftAtIntent &&
      restoredSubmission.attachmentIDs === attachmentIDsAtIntent
        ? restoredSubmission : null;
    const submissionRequestID = recoveredSubmission?.requestID || newSubmissionID();
    if (recoveredSubmission?.limits) limits = { ...recoveredSubmission.limits };
    const deliveryKind = recoveredSubmission?.deliveryKind || (state.busy ? 'queue' : 'prompt');

    if (!state.sessionId) {
      try {
        const authorization = await authorizeNewConversation();
        if (
          state.sessionId !== sessionAtIntent ||
          attachmentSessionRevision !== sessionRevision
        )
          return;
        await ensureSession(cwdAtIntent, authorization);
      } catch (error) {
        toast(error?.userMessage || '對話尚未建立，請稍後重試', false);
        return;
      }
    }
    const sid = state.sessionId;
    if (
      attachmentSessionRevision !== sessionRevision ||
      (sessionAtIntent && sid !== sessionAtIntent)
    )
      return;
    // A new or just-switched chat must not silently submit the medium allowance
    // while its saved High/Max preference is still loading. Time stays pinned
    // to the click; already confirmed effort stays pinned through later awaits.
    if (!recoveredSubmission?.limits && (!controlsAtIntent || controlsAtIntent.maturity === 'error')) {
      await loadInferenceControls();
      if (state.sessionId !== sid || attachmentSessionRevision !== sessionRevision)
        return;
      if (!inferenceControls || inferenceControls.maturity === 'error') {
        toast('無法讀取任務額度設定，訊息尚未送出，請稍後重試', false);
        return;
      }
      effortAtIntent = inferenceControls.reasoningEffort;
    }
    try {
      const harness = await ensureHarnessControlsReady(sid);
      if (!recoveredSubmission?.limits) limits.runTokenLimit = runLimitOptions(effortAtIntent, preferencesAtIntent, harness?.effectiveMode || '').runTokenLimit;
    } catch (error) {
      toast(`訊息尚未送出：${error?.message || error}`, false);
      return;
    }
    if (state.sessionId !== sid || attachmentSessionRevision !== sessionRevision ||
        (stopEpochs.get(sid) || 0) !== stopEpochAtIntent) return;
    // The visible permission pill is a session contract, not an optimistic UI
    // label. Serialize it before the first prompt so 全自動 cannot race a fresh
    // server session that is still using its default approval mode.
    if (!state.busy && !(await commitApprovalMode())) {
      toast('無法同步核准模式，訊息尚未送出');
      return;
    }
    if (
      state.sessionId !== sid ||
      attachmentSessionRevision !== sessionRevision ||
      (stopEpochs.get(sid) || 0) !== stopEpochAtIntent ||
      input.value !== rawDraftAtIntent ||
      attachState.map((item) => item.id).join(',') !== attachmentIDsAtIntent
    )
      return;
    if (deliveryKind === 'prompt' && state.busy) {
      toast('前次訊息回執尚未確認，請等待目前任務完成後再重試。', false);
      return;
    }
    input.value = '';
    restoredSubmission = null;
    autoGrow();

    if (deliveryKind === 'queue') {
      const revision = queueRevision;
      const attachments = takeAttachments();
      let accepted = false;
      try {
        const authorization = await authorizePromptDelivery(sid, submissionRequestID);
        if (state.sessionId !== sid || attachmentSessionRevision !== sessionRevision ||
            (stopEpochs.get(sid) || 0) !== stopEpochAtIntent) {
          restoreFailedDraft(sid, text, attachments, accountRevision, submissionRequestID, limits, 'queue');
          return;
        }
        const r = await daemon.queueAdd(
          sid,
          text,
          attachments,
          limits,
          authorization,
        );
        if (
          !r ||
          !r.item ||
          typeof r.item.id !== 'string' ||
          !Array.isArray(r.items)
        ) {
          throw new Error('queue add receipt requires item.id and items[]');
        }
        accepted = true;
        void consumePromptDelivery(authorization).catch(() => {});
        applyQueueReceipt(sid, revision, r);
        restoreQueueDraft();
      } catch (e) {
        if (accepted) {
          toast(
            e?.userMessage || '任務已排入，但用量回執同步失敗；請重新連線',
            false,
          );
          return;
        }
        restoreFailedDraft(sid, text, attachments, accountRevision, submissionRequestID, limits, 'queue');
        toast(e?.userMessage || `排入任務失敗：${e.message}`, false);
      }
      return;
    }

    const attachments = takeAttachments(); // 送出即清空附件列
    await runPrompt(sid, text, attachments, limits, submissionRequestID);
  }

  // sidebar.js emits this on every switch (new chat + existing thread).
  bus.on('queue_changed', (data) => {
    if (!data || data.sessionId !== state.sessionId) return;
    queueRevision++;
    replaceQueueItems(data.items, { paused: data.paused });
    syncBusy();
    reflectBusy();
  });
  bus.on('turn_completed', (data) => {
    if (!data || data.sessionId !== state.sessionId) return;
    if (data.stopReason === 'cancelled' || data.stopReason === 'canceled')
      cancelPermissionCardsForSession(data.sessionId);
    if (data.origin === 'foreground') runningSessions.delete(data.sessionId);
    syncBusy();
    reflectBusy();
  });
  bus.on('model:escalation-ready', (data) => {
    const sid = String((data && data.sessionId) || '').trim();
    const resumePrompt = String((data && data.resumePrompt) || '').trim();
    if (!sid || sid !== state.sessionId || !resumePrompt) return;
    void runPrompt(sid, resumePrompt, []);
  });
  bus.on('evo:retry-reflection', (data) => {
    if (data?.sessionId !== state.sessionId || state.busy || !/^task-[0-9]+$/.test(data?.runId || '')) return;
    void runPrompt(data.sessionId, '重試既有 Evo 任務的回顧與經驗儲存。', [], undefined, newSubmissionID(), data.runId)
      .finally(() => bus.emit('evo:reflection-retry-settled', data));
  });
  bus.on('thread:switched', async (data) => {
    queueRevision++;
    replaceQueueItems([], { reset: true, paused: false });
    restoreQueueDraft();
    if (data && data.sessionId) {
      if (data.running) runningSessions.add(data.sessionId);
      else runningSessions.delete(data.sessionId);
    }
    syncBusy();
    reflectBusy();
    const sid = state.sessionId;
    const revision = queueRevision;
    if (!sid) return;
    try {
      const r = await daemon.queueList(sid);
      if (state.sessionId !== sid || revision !== queueRevision) return;
      replaceQueueItems(r && r.items, { reset: true, paused: r?.paused });
      syncBusy();
      reflectBusy();
    } catch (e) {
      console.error('queue refresh failed:', e);
    }
  });
  bus.on('run_state', (data) => {
    if (!data || !data.sessionId) return;
    if (data.status === 'running' || data.status === 'cancelling')
      runningSessions.add(data.sessionId);
    else runningSessions.delete(data.sessionId);
    if (data.status === 'cancelling')
      cancelPermissionCardsForSession(data.sessionId);
    if (data.sessionId === state.sessionId) {
      syncBusy();
      reflectBusy();
    }
  });
  bus.on('message:resend', async (data) => {
    if (
      !data ||
      !data.text ||
      !data.sessionId ||
      data.sessionId !== state.sessionId
    )
      return;
    if (state.busy) {
      toast('目前任務仍在執行，請先停止或等待完成', false);
      return;
    }
    await runPrompt(data.sessionId, data.text, []);
  });
  // timeline 的重試鈕等外部路徑自行驅動 prompt 生命週期；它們改完
  // state.busy 後 emit 這個事件，停止鈕才會跟著亮／熄（用戶回報：
  // 重試跑著卻無法中斷）。
  bus.on('busy:changed', reflectBusy);

  input.addEventListener('input', () => {
    restoredSubmission = null;
    autoGrow();
    if (!input.value.trim()) restoreQueueDraft();
  });
  input.addEventListener('keydown', (e) => {
    // skip IME composition Enter (zh-TW input methods)
    if (
      e.key === 'Enter' &&
      !e.shiftKey &&
      !e.isComposing &&
      e.keyCode !== 229 &&
      // Mobile Return adds a line; explicit Cmd/Ctrl+Enter and Send use the
      // same existing queue/send path as Desktop.
      (!window.matchMedia('(max-width: 1024px) and (pointer: coarse)').matches ||
        e.metaKey || e.ctrlKey)
    ) {
      e.preventDefault();
      send();
    }
  });
  // macOS WebKit can expose accessibility-generated Return events as IME
  // composition (keyCode 229), so the normal textarea handler correctly
  // ignores them. Keep an explicit, unambiguous keyboard path for Computer
  // Use and keyboard-only users. Meta/Ctrl+Enter is intentional even while an
  // input method is active and never inserts a newline.
  const explicitSendShortcut = (e) => {
    // WebKit may mark an accessibility/IME Return as already handled even when
    // the textarea rejected keyCode 229, and some AX paths expose only keyup to
    // page JavaScript. Accept both phases; claimSendIntent above collapses the
    // bubbling/key-phase duplicates into one submission.
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void send();
    }
  };
  document.addEventListener('keydown', explicitSendShortcut);
  document.addEventListener('keyup', explicitSendShortcut);
  btn.addEventListener('click', send);
  // Wails' native menu/keybinding bridge calls this exact action when macOS
  // accessibility activation does not dispatch a WebKit click or submit
  // event. It deliberately reuses send() so session, approval, queue, usage,
  // and duplicate-intent semantics remain identical to the visible button.
  window.__meadowSubmitComposer = () => {
    void send();
  };
  // Native macOS menu/keybinding callbacks run outside WebKit. Use Wails'
  // supported host-to-page event channel instead of evaluating ad-hoc JS in
  // the webview; the latter is unreliable while a native menu is dismissing.
  let nativeComposerBridgeRegistered = false;
  const registerNativeComposerBridge = (events = window.wails?.Events) => {
    if (nativeComposerBridgeRegistered) return true;
    if (!events || typeof events.On !== 'function') return false;
    events.On('meadow:composer-submit', () => {
      void send();
    });
    nativeComposerBridgeRegistered = true;
    return true;
  };
  if (!registerNativeComposerBridge() && location.protocol === 'wails:') {
    // URL-backed Wails v3 pages expose the event API as an ES module rather
    // than a window.wails global. Keep the dynamic import out of browser-only
    // UI tests, whose fake server intentionally has no Wails runtime route.
    void import('/wails/runtime.js')
      .then((runtime) => registerNativeComposerBridge(runtime.Events))
      .catch((error) => console.error('native composer bridge failed:', error));
  }
  autoGrow();
  reflectBusy();
}

// ---------- popover plumbing (.pop pattern；執行模式選單仍用) ----------
let openedPop = null; // { pop, anchor }

function closePop() {
  if (openedPop) {
    openedPop.pop.classList.remove('open');
    openedPop = null;
  }
}

function openPop(pop, anchor) {
  pop.style.visibility = 'hidden';
  pop.classList.add('open');
  const r = anchor.getBoundingClientRect();
  const left = Math.min(
    Math.max(8, r.left),
    window.innerWidth - pop.offsetWidth - 8,
  );
  let top = r.top - pop.offsetHeight - 8; // prefer above (composer sits at bottom)
  if (top < 8)
    top = Math.min(r.bottom + 8, window.innerHeight - pop.offsetHeight - 8);
  pop.style.left = left + 'px';
  pop.style.top = top + 'px';
  pop.style.visibility = '';
  openedPop = { pop, anchor };
}

document.addEventListener('click', (e) => {
  if (!openedPop) return;
  if (openedPop.pop.contains(e.target) || openedPop.anchor.contains(e.target))
    return;
  closePop();
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') closePop();
});

// POST /session/{id}/model — fall back to raw fetch if core.js lacks it
function setModel(sid, modelId) {
  if (typeof daemon.setModel === 'function')
    return daemon.setModel(sid, modelId);
  return fetch(`/session/${sid}/model`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ modelId }),
  }).then((res) => {
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return res.json().catch(() => null);
  });
}

// ---------- model picker：two-level selection + lazy activation ----------
// The composer pill is an explicit click target. Inside the first panel, the
// model setting row owns hover discovery of the right-side model list. Picking
// a row only updates the conversation binding; the daemon activates (and, when
// needed, switches away from) the resident model at the next prompt boundary.
let modelModalRef = null; // { wrap, list, inference } — built once by initPills
let modelLoading = null; // { modelId, display } while a load is in flight
let modelLoadError = null; // { modelId, msg } from the last failed load
let pendingModelEscalation = null; // { modelId, resumePrompt, sessionId }
let modelPollTimer = null;
let modelConfirmId = null; // cold row armed by first click（第二次點擊才真的載）
// last /model/list payload；localStorage 持久化＝開 modal 即畫上次清單，
// live 燈號靠背景 refetch 補上（用戶回饋：開選單不等 server probe）
const MODEL_CACHE_KEY = 'meadow.modelCache';
const MODEL_CACHE_FIELDS = [
  'modelId',
  'connectionId',
  'provider',
  'name',
  'displayName',
  'description',
  'status',
  'live',
  'active',
  'loading',
  'present',
  'recommended',
  'multimodal',
  'memoryGB',
  'recommendedMemoryGB',
  'recommendedMemoryGiB',
  'sizeGB',
  'contextWindowTokens',
  'roleLabel',
  'roleDescription',
  'productRole',
  'productionReady',
  'harnessStatus',
  'official',
  'installed',
  'runtimeReady',
  'version',
  'profileId',
  'licenseId',
];

function cacheModelForUI(model) {
  if (!model || typeof model !== 'object') return null;
  const cached = {};
  for (const key of MODEL_CACHE_FIELDS) {
    const value = model[key];
    if (
      typeof value === 'string' ||
      typeof value === 'number' ||
      typeof value === 'boolean'
    ) {
      cached[key] = value;
    }
  }
  return cached.modelId || cached.name ? cached : null;
}

function cacheableModelList(models) {
  return Array.isArray(models)
    ? models.map(cacheModelForUI).filter(Boolean)
    : [];
}

let modelCache = [];
try {
  modelCache = cacheableModelList(
    JSON.parse(localStorage.getItem(MODEL_CACHE_KEY) || '[]'),
  );
} catch {
  modelCache = [];
}
let modelFetched = false; // first /model/list round-trip done
let modelFetchFailed = false;
let modelListRefreshing = false; // cached live badges are untrusted while probing
let modelFetchRevision = 0; // newest probe owns cache + loading state
let modelVisible = []; // rows currently painted, in order（⌘1-9 對應）
let inferenceControls = null; // authoritative per-session controls from Core
let inferenceControlsRevision = 0;
let inferenceControlsLoad = null; // share the same session's pending GET
let inferenceControlsSaving = false;
let inferenceControlsRefreshRequested = false;
let inferenceControlsSaveToken = null; // owns a POST across SSE and thread switches
let modelModalAnchor = null;
let modelConnectionSelection = 0;

// 收藏（☆）：localStorage 持久化，置頂排序
const FAV_KEY = 'meadow.modelFavs';
function getFavs() {
  try {
    const a = JSON.parse(localStorage.getItem(FAV_KEY) || '[]');
    return Array.isArray(a) ? a : [];
  } catch {
    return [];
  }
}
function toggleFav(id) {
  // 單選（用戶語義）：星星＝預設模型，一次最多一顆；再點同一顆＝取消。
  // 沿用陣列儲存格式（向後相容舊多選資料，讀取只看第一個）。
  const f = getFavs();
  const next = f.includes(id) ? [] : [id];
  try {
    localStorage.setItem(FAV_KEY, JSON.stringify(next));
  } catch {
    /* private mode */
  }
}

const displayName = (m) => (m && (m.name || shortName(m.modelId))) || '';
const selectedModelName = (modelId) =>
  displayName(modelEntryFor(modelId)) || shortName(modelId);

function modelCatalogContains(modelId) {
  const exact = String(modelId || '').trim();
  const wanted = exact.toLowerCase();
  if (!wanted) return false;
  // Compatibility aliases may still be returned by an older Core or restored
  // from localStorage. They are useful for resolving old sessions, but they
  // are not valid Desktop selections once a release tier has retired. Keep
  // the composer identity aligned with the sole 35B product row we render.
  return modelCache.some((m) => {
    if (!isStandardProductModel(m)) return false;
    if (m.delivery === 'api' || m.connectionId) {
      return (m.connectionId || '') === (state.modelConnectionID || '') &&
        m.modelId === exact;
    }
    if (state.modelConnectionID) return false;
    return [m.modelId, m.name, displayName(m)].some((value) => {
      const candidate = String(value || '')
        .trim()
        .toLowerCase();
      return (
        candidate === wanted ||
        shortName(candidate).toLowerCase() === shortName(wanted).toLowerCase()
      );
    });
  });
}

function modelEntryFor(modelId, models = modelCache) {
  const exact = String(modelId || '').trim();
  const wanted = exact.toLowerCase();
  if (!wanted) return null;
  return (
    models.find((m) => {
      if (m.delivery === 'api' || m.connectionId) {
        return (m.connectionId || '') === (state.modelConnectionID || '') &&
          m.modelId === exact;
      }
      if (state.modelConnectionID) return false;
      return [m.modelId, m.name, displayName(m)].some((value) => {
        const candidate = String(value || '')
          .trim()
          .toLowerCase();
        return (
          candidate === wanted ||
          shortName(candidate).toLowerCase() === shortName(wanted).toLowerCase()
        );
      });
    }) || null
  );
}

function currentModelSupportsImages() {
  const entry = modelEntryFor(state.model);
  if (entry && typeof entry.multimodal === 'boolean') return entry.multimodal;
  // Capability is stability-sensitive. While the Core catalog is unknown or
  // stale, fail closed instead of guessing from a model name.
  return false;
}

// A cold Core intentionally reports no verified vision capability. Opening the
// picker must remain lazy, but actually attaching an image is a user request to
// use the selected model. Recheck the catalog, and connect an inactive API via
// the same managed lifecycle used by text prompts. Never authorize images from
// cached metadata or infer vision support from the model's name.
async function verifyImageCapability(modelId) {
  if (!imageCapabilityCheck || imageCapabilityCheck.modelId !== modelId) {
    const check = { modelId, promise: null };
    check.promise = (async () => {
      let models = await refetchModels({ verifyRuntime: true });
      if (!models) throw new Error('無法確認模型的圖片能力，請稍後重試');
      if (state.model !== modelId) return false;
      let entry = modelEntryFor(modelId, models);
      // Native API rows use active for the configured selection even while
      // Core is stopped. Only live confirms a connected, verified runtime.
      if (entry?.delivery === 'api' && entry.live !== true) {
        await daemon.loadModel(modelId);
        models = await refetchModels({ verifyRuntime: true });
        if (!models) throw new Error('無法確認模型的圖片能力，請稍後重試');
        entry = modelEntryFor(modelId, models);
      }
      if (state.model !== modelId) return false;
      if (entry?.multimodal !== true) {
        rejectImageForCurrentModel();
        return false;
      }
      return true;
    })().finally(() => {
      if (imageCapabilityCheck === check) imageCapabilityCheck = null;
    });
    imageCapabilityCheck = check;
  }
  return imageCapabilityCheck.promise;
}

function rejectImageForCurrentModel() {
  const entry = modelEntryFor(state.model);
  const label = displayName(entry) || state.model || '目前模型';
  toast(`${label} 不支援圖片辨識，請切換到支援視覺的模型`, false);
}

function clearImageAttachmentsForModelSwitch() {
  const kept = attachState.filter((attachment) => attachment.kind !== 'image');
  if (kept.length === attachState.length) return;
  attachState.splice(0, attachState.length, ...kept);
  renderAttachStrip();
  toast('已清除圖片附件：目前模型不支援圖片辨識', false);
}

function reconcileImageCapability() {
  if (!currentModelSupportsImages()) clearImageAttachmentsForModelSwitch();
}

function clearRetiredSessionModel() {
  if (!modelFetched || modelFetchFailed || modelLoading) return;
  if (!state.model || modelCatalogContains(state.model)) return;
  state.model = '';
  const pill = $('#pillModelName');
  if (pill) pill.textContent = '請選擇模型';
}

// shares the #gitToasts stack + .git-toast styling (same pattern as
// sidebar.js / gitbar.js) so transient notices pile up in one corner
function toast(msg, ok) {
  let box = document.getElementById('gitToasts');
  if (!box) {
    box = document.createElement('div');
    box.id = 'gitToasts';
    document.body.appendChild(box);
  }
  const t = document.createElement('div');
  t.className = 'git-toast' + (ok ? '' : ' bad');
  t.textContent = msg;
  t.title = msg;
  box.appendChild(t);
  setTimeout(() => t.remove(), ok ? 3200 : 6000);
}

const modelModalOpen = () => !!(modelModalRef && !modelModalRef.wrap.hidden);

function buildModelModal() {
  const wrap = document.createElement('div');
  wrap.id = 'modelModal';
  wrap.className = 'compact-model-picker';
  wrap.hidden = true;
  wrap.innerHTML =
    `<div class="mm-card" role="dialog" aria-modal="false" aria-label="模型與推理設定">` +
    `<button type="button" class="mm-setting-row" id="modelSubmenuToggle" aria-expanded="false">` +
    `<span class="mm-setting-label">模型</span>` +
    `<span class="mm-setting-value" id="modelSubmenuValue">選擇模型</span>` +
    `<svg class="mm-chevron" viewBox="0 0 24 24" aria-hidden="true"><path d="m9 18 6-6-6-6"/></svg>` +
    `</button>` +
    `<section class="mm-inference" id="modelInference" aria-label="推理強度設定"></section>` +
    `<button type="button" class="mm-setup-model" id="modelSetupButton">設定模型</button>` +
    `</div>` +
    `<aside class="mm-model-submenu" id="modelSubmenu" aria-label="選擇模型" hidden>` +
    `<div class="mm-list" id="modelList" role="listbox" aria-label="模型"></div>` +
    `</aside>`;
  document.body.appendChild(wrap);
  wrap.querySelector('#modelSetupButton').addEventListener('click', () => {
    closeModelModal();
    bus.emit('settings:open-model');
  });

  // The picker is a small anchored popover, not a full-screen backdrop. The
  // document pointer listener below owns outside-click dismissal.
  wrap.addEventListener('mousedown', (e) => {
    if (e.target === wrap) closeModelModal();
  });
  // Header tools are global navigation. If the model picker is open, one
  // click should close it and perform the requested tool action, instead of
  // consuming the first click on the backdrop.
  for (const selector of [
    '#btnTerminal',
    '#btnProgress',
    '#btnExpertLens',
    '#btnDiff',
  ]) {
    const button = document.querySelector(selector);
    button?.addEventListener('pointerdown', closeModelModal, true);
  }
  modelModalRef = {
    wrap,
    list: wrap.querySelector('#modelList'),
    inference: wrap.querySelector('#modelInference'),
    submenu: wrap.querySelector('#modelSubmenu'),
    toggle: wrap.querySelector('#modelSubmenuToggle'),
    current: wrap.querySelector('#modelSubmenuValue'),
  };
  const card = wrap.querySelector('.mm-card');
  let submenuCloseTimer = null;
  const setSubmenuOpen = (open) => {
    if (submenuCloseTimer) {
      clearTimeout(submenuCloseTimer);
      submenuCloseTimer = null;
    }
    modelModalRef.submenu.hidden = !open;
    modelModalRef.toggle.setAttribute('aria-expanded', String(open));
    if (open) positionModelModal();
  };
  const hoverTargetName = (event) =>
    event?.target?.id ||
    event?.target?.closest?.('[id]')?.id ||
    event?.target?.className ||
    'unknown';
  const reportSubmenuState = (phase, event) => {
    if (!new URLSearchParams(window.location.search).has('hoverDebug')) return;
    requestAnimationFrame(() => {
      const rect = modelModalRef.submenu.getBoundingClientRect();
      const style = getComputedStyle(modelModalRef.submenu);
      const entry = {
        type: `model-picker:${phase}`,
        event: event?.type || 'programmatic',
        buttons: event?.buttons ?? 0,
        target: hoverTargetName(event),
        hidden: modelModalRef.submenu.hidden,
        ariaExpanded: modelModalRef.toggle.getAttribute('aria-expanded'),
        display: style.display,
        visibility: style.visibility,
        opacity: style.opacity,
        rect: {
          x: Math.round(rect.x),
          y: Math.round(rect.y),
          width: Math.round(rect.width),
          height: Math.round(rect.height),
        },
      };
      console.log('[model-picker:state]', entry);
      void fetch('/native/hover-debug', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(entry),
      }).catch(() => {});
    });
  };
  const keepSubmenuOpen = (event) => {
    if (modelModalRef.submenu.hidden) {
      console.log('[model-picker:hover] enter', {
        event: event?.type || 'programmatic',
        target: hoverTargetName(event),
      });
    }
    setSubmenuOpen(true);
    reportSubmenuState('open', event);
  };
  const scheduleSubmenuClose = (event) => {
    if (event?.pointerType === 'touch') return;
    if (!submenuCloseTimer) {
      console.log('[model-picker:hover] leave', {
        event: event?.type || 'programmatic',
        target: hoverTargetName(event),
      });
    }
    if (submenuCloseTimer) clearTimeout(submenuCloseTimer);
    submenuCloseTimer = setTimeout(() => {
      submenuCloseTimer = null;
      console.log('[model-picker:hover] close');
      setSubmenuOpen(false);
      reportSubmenuState('close', event);
    }, 140);
  };
  // Hover is a shortcut. A normal click must also open model selection.
  modelModalRef.toggle?.addEventListener('click', () => setSubmenuOpen(true));
  modelModalRef.toggle?.addEventListener('pointerenter', keepSubmenuOpen);
  modelModalRef.toggle?.addEventListener('pointerleave', scheduleSubmenuClose);
  modelModalRef.toggle?.addEventListener('mouseover', (event) => {
    if (modelModalRef.submenu.hidden) keepSubmenuOpen(event);
  });
  // Touch has no hover contract; retain the existing mouse/keyboard paths.
  modelModalRef.toggle?.addEventListener('pointerup', (event) => {
    if (event.pointerType === 'touch') keepSubmenuOpen(event);
  });
  // Do not open merely because a pointer click gives the row focus: that hid
  // native hover regressions by making a broken WKWebView path look click-only.
  // Keep keyboard discovery explicit instead.
  modelModalRef.toggle?.addEventListener('keydown', (event) => {
    if (!['ArrowRight', 'Enter', ' '].includes(event.key)) return;
    event.preventDefault();
    console.log('[model-picker:keyboard] open', { key: event.key });
    setSubmenuOpen(true);
  });
  modelModalRef.submenu?.addEventListener('pointerenter', keepSubmenuOpen);
  modelModalRef.submenu?.addEventListener('pointerleave', scheduleSubmenuClose);
  modelModalRef.submenu?.addEventListener('mouseover', (event) => {
    if (modelModalRef.submenu.hidden) keepSubmenuOpen(event);
  });
  document.addEventListener(
    'pointerdown',
    (event) => {
      if (!modelModalOpen()) return;
      const triggers = [$('#pillModel')];
      if (
        triggers.some((trigger) => trigger?.contains(event.target)) ||
        card?.contains(event.target) ||
        modelModalRef.submenu?.contains(event.target)
      ) {
        return;
      }
      closeModelModal();
    },
    true,
  );
  for (const button of wrap.querySelectorAll('[data-commercial-mode]')) {
    button.addEventListener('click', () => {
      window.MeadowCommercial?.selectMode(button.dataset.commercialMode, {
        contextual: true,
      });
      syncCommercialModelMode();
      paintModelRows();
    });
  }
  document.addEventListener('commercial:change', () => {
    syncCommercialModelMode();
    if (modelModalOpen()) paintModelRows();
    reconcileImageCapability();
  });
  // Catalog/effort responses and window resizing can change the available
  // space after opening. Keep positioning tied to layout, not catalog size.
  let positionFrame = 0;
  const schedulePosition = () => {
    if (!modelModalOpen() || positionFrame) return;
    positionFrame = requestAnimationFrame(() => {
      positionFrame = 0;
      if (modelModalOpen()) positionModelModal();
    });
  };
  const pickerResize = new ResizeObserver(schedulePosition);
  pickerResize.observe(card);
  pickerResize.observe(modelModalRef.list);
  window.addEventListener('resize', schedulePosition);
}

function syncCommercialModelMode() {
  if (!modelModalRef) return;
  const snapshot = window.MeadowCommercial?.snapshot() || {
    mode: 'general',
    proAllowed: false,
  };
  for (const button of modelModalRef.wrap.querySelectorAll(
    '[data-commercial-mode]',
  )) {
    const active = button.dataset.commercialMode === snapshot.mode;
    button.classList.toggle('active', active);
    button.setAttribute('aria-selected', String(active));
    if (button.dataset.commercialMode === 'pro') {
      button.classList.toggle('locked', !snapshot.proAllowed);
      button.setAttribute(
        'aria-label',
        snapshot.proAllowed ? '切換到 Pro 模式' : 'Pro 模式，需要升級',
      );
    }
  }
}

function openModelModal(anchor = $('#pillModel')) {
  if (!modelModalRef) return;
  modelModalAnchor = anchor;
  for (const trigger of [$('#pillModel')])
    trigger?.setAttribute('aria-expanded', String(trigger === anchor));
  closePop();
  modelConfirmId = null;
  // A cached `live:true` row may outlive Core's idle unload. Mark the list as
  // probing before the first paint so a fast click cannot mistake yesterday's
  // resident badge for current state and silently close the picker.
  modelListRefreshing = true;
  modelModalRef.wrap.hidden = false;
  modelModalRef.submenu.hidden = true;
  modelModalRef.toggle?.setAttribute('aria-expanded', 'false');
  positionModelModal();
  paintModelRows(); // instant paint from cache（或「載入中…」）
  paintInferenceControls();
  void loadInferenceControls();
  bus.emit('harness_controls_opened');
  refetchModels(); // fresh /model/list → live 燈號更新
}

function positionModelModal() {
  if (!modelModalRef) return;
  const trigger = modelModalAnchor || document.getElementById('pillModel');
  if (!trigger) return;
  const rect = trigger.getBoundingClientRect();
  const width = Math.min(224, window.innerWidth - 24);
  const left = Math.max(
    12,
    Math.min(rect.left, window.innerWidth - width - 12),
  );
  const submenuWidth = Math.min(360, window.innerWidth - 24);
  const cardHeight = Math.max(
    0,
    modelModalRef.wrap.querySelector('.mm-card')?.offsetHeight || 0,
  );
  const bottom = Math.max(12, Math.min(
    window.innerHeight - rect.top + 8,
    window.innerHeight - cardHeight - 12,
  ));
  const rightSide = left + width + 8;
  const leftSide = left - submenuWidth - 8;
  const fitsRight = rightSide + submenuWidth <= window.innerWidth - 12;
  const fitsLeft = leftSide >= 12;
  // Both panels now share one ordinary fixed wrapper. Submenu coordinates are
  // therefore local offsets, matching the isolated native hover probe instead
  // of two unrelated fixed layers under a display:contents ancestor.
  const stacked = !fitsRight && !fitsLeft;
  const submenuX = fitsRight ? width + 8 : fitsLeft ? -submenuWidth - 8 :
    Math.min(0, window.innerWidth - submenuWidth - 12 - left);
  let submenuY = stacked ? cardHeight + 8 : 0;
  let submenuMaxHeight = window.innerHeight - bottom - submenuY - 12;
  // A narrow window may have more room below the card. Put the list there
  // rather than leaving only a sliver above a centered new-chat composer.
  const belowHeight = bottom - 20;
  const placeBelow = stacked && belowHeight > submenuMaxHeight;
  if (placeBelow) submenuMaxHeight = belowHeight;
  modelModalRef.wrap.style.setProperty('--mm-left', `${left}px`);
  modelModalRef.wrap.style.setProperty('--mm-bottom', `${bottom}px`);
  modelModalRef.wrap.style.setProperty('--mm-width', `${width}px`);
  modelModalRef.wrap.style.setProperty('--mm-submenu-x', `${submenuX}px`);
  modelModalRef.wrap.style.setProperty('--mm-submenu-y', `${submenuY}px`);
  modelModalRef.wrap.style.setProperty(
    '--mm-submenu-max-height', `${Math.max(0, submenuMaxHeight)}px`,
  );
  modelModalRef.wrap.style.setProperty(
    '--mm-submenu-width',
    `${submenuWidth}px`,
  );
  if (placeBelow && !modelModalRef.submenu.hidden) {
    submenuY = -modelModalRef.submenu.offsetHeight - 8;
    modelModalRef.wrap.style.setProperty('--mm-submenu-y', `${submenuY}px`);
  }
}

function closeModelModal() {
  if (!modelModalRef) return;
  for (const trigger of [$('#pillModel')])
    trigger?.setAttribute('aria-expanded', 'false');
  modelModalRef.wrap.hidden = true;
  modelModalRef.submenu.hidden = true;
  modelModalRef.toggle?.setAttribute('aria-expanded', 'false');
}

// repaint with a fresh /model/list if currently open（finish/fail 後亮燈用）
function refreshModelModal() {
  if (modelModalOpen()) {
    refetchModels();
    void loadInferenceControls();
  }
}

const EFFORTS = ['minimal', 'low', 'medium', 'high', 'max'];

function effortProfiles(control) {
  const profiles = control?.effortProfiles;
  if (!Array.isArray(profiles) || profiles.length !== EFFORTS.length) return [];
  return EFFORTS.map((effort) =>
    profiles.find(
      (profile) =>
        profile.effort === effort &&
        typeof profile.label === 'string' &&
        typeof profile.description === 'string' &&
        ['on', 'off'].includes(profile.thinkingMode),
    ),
  ).filter(Boolean);
}

async function loadInferenceControls() {
  // Reopening the picker must not let an older GET invalidate a pending save.
  // Thread/model switches clear the save owner before loading their controls.
  if (inferenceControlsSaving) return;
  const sid = state.sessionId;
  if (inferenceControlsLoad?.sessionId === sid)
    return inferenceControlsLoad.promise;
  const revision = ++inferenceControlsRevision;
  if (!sid) {
    inferenceControls = null;
    paintInferenceControls();
    return;
  }
  const load = { sessionId: sid, promise: null };
  inferenceControlsLoad = load;
  load.promise = (async () => {
  let timeout;
  try {
    const next = await Promise.race([
      daemon.inferenceControls(sid),
      new Promise((_, reject) => {
        timeout = setTimeout(() => reject(new Error('推理設定讀取逾時')), 5000);
      }),
    ]);
    if (revision !== inferenceControlsRevision || sid !== state.sessionId)
      return;
    inferenceControls = next;
  } catch (err) {
    if (revision !== inferenceControlsRevision || sid !== state.sessionId)
      return;
    inferenceControls = {
      supported: false,
      maturity: 'error',
      note: '無法取得推理設定：' + ((err && err.message) || err),
    };
  } finally { clearTimeout(timeout); }
  paintInferenceControls();
  })().finally(() => {
    if (inferenceControlsLoad === load) inferenceControlsLoad = null;
  });
  return load.promise;
}

async function updateInferenceControls(patch) {
  const sid = state.sessionId;
  if (!sid || inferenceControlsSaving) return;
  const previous = inferenceControls;
  const revision = ++inferenceControlsRevision;
  const saveToken = {
    restoreFocus: modelModalRef?.inference?.contains(document.activeElement)
      ? document.activeElement?.matches('input[type="range"]')
        ? 'input[type="range"]'
        : document.activeElement?.id === 'thinkingToggle'
          ? '#thinkingToggle'
          : null
      : null,
  };
  inferenceControlsSaveToken = saveToken;
  inferenceControlsSaving = true;
  // Keep confirmed preferences visible until the server has persisted them.
  // A pending request must not claim that Thinking has already changed.
  paintInferenceControls();
  try {
    const next = await daemon.setInferenceControls(sid, patch);
    if (revision !== inferenceControlsRevision || sid !== state.sessionId)
      return;
    inferenceControls = next;
  } catch (err) {
    if (revision !== inferenceControlsRevision || sid !== state.sessionId)
      return;
    inferenceControls = previous;
    toast('推理設定未更新：' + ((err && err.message) || err), false);
  } finally {
    // HTTP receipts and SSE are independent transports. A delayed older SSE
    // cannot establish ordering; read the current persisted controls instead.
    // Keep controls disabled until notifications received during this save
    // have been reconciled, including notifications arriving during the GET.
    while (
      inferenceControlsSaveToken === saveToken &&
      sid === state.sessionId &&
      inferenceControlsRefreshRequested
    ) {
      inferenceControlsRefreshRequested = false;
      try {
        const latest = await daemon.inferenceControls(sid);
        if (inferenceControlsSaveToken === saveToken && sid === state.sessionId)
          inferenceControls = latest;
      } catch (err) {
        if (inferenceControlsSaveToken === saveToken && sid === state.sessionId)
          inferenceControls = {
            supported: false,
            maturity: 'error',
            note: '無法同步推理設定，請重新開啟設定：' + ((err && err.message) || err),
          };
      }
    }
    if (inferenceControlsSaveToken === saveToken) {
      inferenceControlsSaveToken = null;
      inferenceControlsSaving = false;
      if (sid === state.sessionId) {
        paintInferenceControls();
        if (saveToken.restoreFocus && document.activeElement === document.body)
          modelModalRef?.inference
            ?.querySelector(saveToken.restoreFocus)
            ?.focus({ preventScroll: true });
      }
    }
  }
}

function resetInferenceControls() {
  ++inferenceControlsRevision;
  inferenceControlsLoad = null;
  inferenceControlsSaveToken = null;
  inferenceControlsSaving = false;
  inferenceControlsRefreshRequested = false;
  inferenceControls = null;
  paintInferenceControls();
}

function paintEffortShortcut() {
  // Transport-only retries use this same confirmed, session-owned allowance.
  // Reset/loading/error controls clear ownership instead of leaking another chat.
  state.runBudgetControl = inferenceControls && inferenceControls.maturity !== 'error'
    ? { sessionId: state.sessionId, effort: inferenceControls.reasoningEffort }
    : null;
  const trigger = $('#pillModel');
  if (!trigger) return;
  const control = inferenceControls;
  const profiles = effortProfiles(control);
  const selected = profiles.find(
    (profile) => profile.effort === control?.reasoningEffort,
  );
  const available =
    control?.effortSupported === true && control.maturity !== 'error';
  const level = available ? profiles.indexOf(selected) : -1;
  const fraction = level >= 0 ? level / Math.max(1, profiles.length - 1) : 0;
  trigger.dataset.effort = level >= 0 ? selected.effort : 'unknown';
  trigger.dataset.effortState = !control
    ? 'loading'
    : level >= 0 ? 'ready' : 'unavailable';
  trigger.style.setProperty('--effort-angle', `${-135 + fraction * 270}deg`);
  trigger.style.setProperty('--effort-sweep', `${fraction * 270}deg`);
  const label = !control
    ? '讀取中'
    : (available && selected?.label) ||
      (control.maturity === 'error'
        ? '無法讀取'
        : control.thinkingSupported ? '思考' : '未提供');
  const output = trigger.querySelector('#pillEffortValue');
  if (output) output.textContent = label;
  trigger.dataset.thinking =
    (control?.effortSupported ? control.effectiveProfile?.thinkingMode : null) ||
    control?.thinkingMode ||
    'unknown';
  trigger.setAttribute('aria-busy', String(inferenceControlsSaving));
  const modeLabel = {
    on: '思考開啟',
    off: '思考關閉',
    auto: '模型預設',
  }[control?.thinkingMode] || '';
  trigger.setAttribute(
    'aria-label',
    `模型與推理設定，Effort ${label}，每次任務額度 ${runLimitOptions(control?.reasoningEffort).runTokenLimit / 10000} 萬 tokens${modeLabel ? `，${modeLabel}` : ''}`,
  );
  trigger.title = control?.effortSupported && control.effectiveProfile
    ? `已選 ${label}；下一輪：${control.effectiveProfile.label} · ${modeLabel}；模型與推理設定`
    : control?.thinkingSupported
      ? `下一輪：思考${control.thinkingMode === 'on' ? '開啟' : control.thinkingMode === 'off' ? '關閉' : '自動'}；開啟思考設定`
      : '思考與推理設定';
}

function thinkingToggle(control) {
  const enabled = control.thinkingMode === 'on';
  const wrap = document.createElement('span');
  wrap.className = 'mic-thinking';
  const status = document.createElement('span');
  status.className = 'mic-thinking-state';
  status.textContent =
    control.thinkingMode === 'auto' ? '自動' : enabled ? '開啟' : '關閉';
  const button = document.createElement('button');
  button.id = 'thinkingToggle';
  button.type = 'button';
  button.className = 'mic-lightning' + (enabled ? ' on' : '');
  button.innerHTML =
    '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 14a1 1 0 0 1-.78-1.63l9-11a.5.5 0 0 1 .87.45l-1.69 6.2A1 1 0 0 0 12.36 9H20a1 1 0 0 1 .78 1.63l-9 11a.5.5 0 0 1-.87-.45l1.69-6.2A1 1 0 0 0 11.64 14Z"/></svg>';
  button.title = enabled ? '關閉 Thinking' : '開啟 Thinking';
  button.setAttribute('aria-label', button.title);
  button.setAttribute('aria-pressed', String(enabled));
  button.disabled = inferenceControlsSaving;
  button.addEventListener('click', (event) => {
    event.stopPropagation();
    void updateInferenceControls({ thinkingMode: enabled ? 'off' : 'on' });
  });
  wrap.append(status, button);
  return wrap;
}

function paintInferenceControls() {
  paintEffortShortcut();
  if (!modelModalRef || !modelModalRef.inference) return;
  const panel = modelModalRef.inference;
  const restoreSliderFocus =
    panel.contains(document.activeElement) &&
    document.activeElement?.matches('input[type="range"]');
  panel.classList.remove(
    'disabled',
    'has-effort-profiles',
    'is-default-effort',
  );
  const control = inferenceControls;
  if (!state.sessionId || !control) {
    panel.innerHTML =
      '<div class="mic-head"><span>推理強度</span><span class="mic-badge">讀取中</span></div>' +
      '<div class="mic-note">建立聊天後即可調整。</div>';
    return;
  }

  const thinkingSupported = control.thinkingSupported === true;
  const effortSupported = control.effortSupported === true;
  panel.innerHTML = '';
  const head = document.createElement('div');
  head.className = 'mic-head';
  const title = document.createElement('span');
  title.className = 'mic-head-title';
  head.appendChild(title);
  const note = document.createElement('div');
  note.className = 'mic-note';

  if (!thinkingSupported && !effortSupported) {
    title.textContent =
      control.maturity === 'error' ? '推理設定暫時無法讀取' : '不支援 Think';
    note.textContent =
      control.maturity === 'error'
        ? control.note
        : '此模型使用穩定 Agent 模式，沒有 Effort 設定。';
    panel.append(head, note);
    panel.classList.add('disabled');
    return;
  }

  if (thinkingSupported) head.appendChild(thinkingToggle(control));

  if (effortSupported) {
    const profiles = effortProfiles(control);
    const selectedIndex = profiles.findIndex(
      (profile) => profile.effort === control.reasoningEffort,
    );
    const effectiveIndex = profiles.findIndex(
      (profile) => profile.effort === control.effectiveEffort,
    );
    const defaultProfile =
      control.effectiveEffort === 'auto' &&
      control.effectiveProfile?.effort === 'auto'
        ? control.effectiveProfile
        : null;
    if (
      profiles.length !== EFFORTS.length ||
      selectedIndex < 0 ||
      (effectiveIndex < 0 && !defaultProfile)
    ) {
      title.textContent = '推理設定暫時無法讀取';
      note.textContent = '尚未取得此模型的推理等級，請稍後重開選單。';
      panel.append(head, note);
      panel.classList.add('disabled');
      return;
    }
    panel.classList.add('has-effort-profiles');
    const effortRow = document.createElement('label');
    effortRow.className = 'mic-effort';
    const sliderWrap = document.createElement('div');
    sliderWrap.className = 'mic-slider-wrap';
    const slider = document.createElement('input');
    slider.type = 'range';
    slider.min = '0';
    slider.max = String(profiles.length - 1);
    slider.step = '1';
    slider.disabled = inferenceControlsSaving;
    slider.setAttribute('aria-label', '推理強度');
    slider.setAttribute('aria-describedby', 'effortPurpose effortRunTokenLimit');
    const purpose = document.createElement('div');
    purpose.id = 'effortPurpose';
    purpose.className = 'mic-purpose';
    const runBudget = document.createElement('div');
    runBudget.id = 'effortRunTokenLimit';
    runBudget.className = 'mic-effective';
    runBudget.title = '整個任務的累計處理量，包含重複輸入與快取；不是單次輸出長度或費用。從下一次送出或排入的任務生效。';
    const effectiveProfile = control.effectiveProfile;
    const renderProfile = (index) => {
      panel.classList.remove('is-default-effort');
      const profile = profiles[index];
      slider.value = String(index);
      slider.style.setProperty(
        '--effort-fill',
        `${(index / (profiles.length - 1)) * 100}%`,
      );
      slider.setAttribute(
        'aria-valuetext',
        `${profile.label}：${profile.description}`,
      );
      title.textContent = `推理 · ${profile.label}`;
      purpose.textContent = profile.description;
      runBudget.textContent = `每次任務額度：${runLimitOptions(profile.effort).runTokenLimit / 10000} 萬 tokens`;
      const budgetLimited =
        index === effectiveIndex && effectiveProfile
          ? effectiveProfile.budgetLimited
          : profile.budgetLimited;
      note.textContent = inferenceControlsSaving
        ? '正在儲存設定…'
        : ['invalid', 'unavailable'].includes(control.preferenceStatus)
          ? control.note
          : control.thinkingMode !== 'on' && profile.effort !== 'minimal'
            ? `任務額度依${profile.label}套用。思考目前${control.thinkingMode === 'off' ? '關閉' : '自動'}；開啟後才套用${profile.label}推理。`
            : budgetLimited
              ? '已依模型可用容量調整思考空間。從下一輪生效。'
              : '越往右任務額度越多；開啟思考時也投入更多推理。從下一次送出或排入的任務生效。';
      return profile;
    };
    const commitProfile = (index) => {
      const profile = renderProfile(index);
      // Retain the explicit Thinking preference while changing effort. Rapid
      // is the one non-thinking tier; choosing it deliberately disables Think.
      void updateInferenceControls({
        reasoningEffort: profile.effort,
        thinkingMode:
          profile.thinkingMode === 'off' ? 'off' : control.thinkingMode,
      });
    };
    renderProfile(selectedIndex);
    slider.addEventListener('click', (event) => event.stopPropagation());
    slider.addEventListener('input', () => renderProfile(Number(slider.value)));
    slider.addEventListener('change', (event) => {
      event.stopPropagation();
      commitProfile(Number(slider.value));
    });
    const ticks = document.createElement('div');
    ticks.className = 'mic-ticks';
    ticks.setAttribute('aria-hidden', 'true');
    for (const profile of profiles) {
      const tick = document.createElement('i');
      tick.title = profile.label;
      ticks.appendChild(tick);
    }
    const scale = document.createElement('div');
    scale.className = 'mic-scale';
    scale.setAttribute('aria-hidden', 'true');
    for (const profile of profiles) {
      const label = document.createElement('span');
      label.textContent = profile.label;
      scale.appendChild(label);
    }
    sliderWrap.append(slider, ticks);
    effortRow.append(sliderWrap, scale);
    const effective = document.createElement('div');
    effective.className = 'mic-effective';
    const activeMode = control.effectiveProfile?.thinkingMode;
    const modeLabel =
      activeMode === 'on'
        ? '思考開啟'
        : activeMode === 'off'
          ? '思考關閉'
          : '模型預設';
    effective.textContent = `下一輪：${effectiveProfile?.label || '模型預設'} · ${modeLabel}`;
    panel.append(head, effortRow, purpose, runBudget, effective, note);
    if (restoreSliderFocus) slider.focus({ preventScroll: true });
    return;
  }

  // Models that expose only Thinking keep their actual supported control.
  title.textContent = '思考模式';
  note.textContent = inferenceControlsSaving
    ? '正在儲存設定…'
    : control.note || '從下一輪生效。';
  panel.append(head, note);
}

async function refetchModels({ verifyRuntime = false } = {}) {
  const revision = ++modelFetchRevision;
  const accountRevision = attachmentAccountRevision;
  const accountGeneration = clientAccountGeneration();
  const sessionRevision = attachmentSessionRevision;
  const sessionId = state.sessionId;
  const connectionId = state.modelConnectionID;
  modelListRefreshing = true;
  if (modelModalOpen()) paintModelRows();
  let list = null;
  let timeoutId = null;
  try {
    list =
      (await Promise.race([
        (async () => {
          // Saved native connections are the selection authority. Opening the
          // picker must not wait for a remote model-discovery request; the
          // existing identity-scoped model_status poll owns live badges.
          const response = await fetch('/native/model-providers', {
            headers: { 'X-Meadow-Settings': '1' },
          }).catch(() => null);
          if (response?.status === 401 || response?.status === 403)
            throw new Error('model catalog access denied');
          const connections = response?.ok ? await response.json() : null;
          if (connections?.mode === 'connections') {
            if (!verifyRuntime) return configuredPickerRows([], connections);
            // An explicit image attachment still requires current runtime
            // evidence. Saved capabilities must not authorize image delivery.
            const live = await daemon.modelList();
            if (!Array.isArray(live)) throw new Error('model capability unavailable');
            return configuredPickerRows(live, connections).map((row) => {
              const runtime = row.connectionId === connections.selectedConnectionId
                ? live.find((model) => model.modelId === row.modelId) : null;
              return { ...row, multimodal: runtime?.multimodal === true };
            });
          }
          // Legacy/Core-only compositions do not have a native connection
          // catalog and retain their original discovery contract.
          const models = await daemon.modelList();
          if (!Array.isArray(models)) throw new Error('model catalog unavailable');
          return models;
        })(),
        new Promise((resolve) => {
          timeoutId = setTimeout(() => resolve(null), 12000);
        }),
      ])) || null;
  } catch {
    list = null;
  } finally {
    if (timeoutId) clearTimeout(timeoutId);
  }
  // An older slow request must never overwrite a newer resident-state probe.
  if (revision !== modelFetchRevision) return list;
  if (!hasClientAccess() || accountGeneration !== clientAccountGeneration() ||
      accountRevision !== attachmentAccountRevision ||
      sessionRevision !== attachmentSessionRevision || sessionId !== state.sessionId ||
      connectionId !== state.modelConnectionID) {
    modelListRefreshing = false;
    if (hasClientAccess() && modelModalOpen()) paintModelRows();
    return null;
  }
  modelFetched = true;
  modelFetchFailed = list === null;
  if (list) {
    modelCache = list;
    try {
      localStorage.setItem(
        MODEL_CACHE_KEY,
        JSON.stringify(cacheableModelList(list)),
      );
    } catch {
      /* ignore */
    }
    clearRetiredSessionModel();
    const pill = $('#pillModelName');
    const selectedEntry =
      modelEntryFor(state.model) ||
      list.find((entry) => entry.active === true || entry.live === true) ||
      list.find(isStandardProductModel) ||
      null;
    if (pill && selectedEntry) {
      pill.textContent = shortName(displayName(selectedEntry));
    }
  }
  modelListRefreshing = false;
  if (modelModalOpen()) paintModelRows();
  return list;
}

function paintModelRows() {
  if (!modelModalOpen()) return;
  const { list } = modelModalRef;
  const favs = getFavs();
  const order = { '35B': 1 };
  const rows = modelCache
    .filter(isStandardProductModel)
    .sort(
      (a, b) =>
        (order[productModelTier(a)] ?? 100) -
        (order[productModelTier(b)] ?? 100),
    );
  const selected = rows.find((model) => model === modelEntryFor(state.model));
  const resident = rows.find((model) => model.active);
  if (modelModalRef.current)
    modelModalRef.current.textContent =
      displayName(selected) ||
      selectedModelName(state.model) ||
      displayName(resident) ||
      '選擇模型';
  modelVisible = rows;
  list.innerHTML = '';
  if (!rows.length) {
    const empty = document.createElement('div');
    empty.className = 'mm-empty';
    empty.textContent = modelListRefreshing
      ? '正在讀取模型…'
      : modelFetchFailed && !modelCache.length
        ? '無法取得模型清單'
        : modelCache.length
          ? '找不到模型'
          : '沒有可用模型';
    list.appendChild(empty);
    if (modelFetchFailed && !modelListRefreshing) {
      const retry = document.createElement('button');
      retry.type = 'button';
      retry.className = 'mm-setup-model';
      retry.textContent = '重新讀取';
      retry.addEventListener('click', () => void refetchModels());
      list.appendChild(retry);
    }
  } else {
    rows.forEach((m, idx) => list.appendChild(modelRow(m, idx, favs)));
  }
  const setup = document.createElement('button');
  setup.type = 'button';
  setup.className = 'mm-setup-model';
  setup.textContent = '設定模型';
  setup.addEventListener('click', () => {
    closeModelModal();
    bus.emit('settings:open-model');
  });
  list.appendChild(setup);
}

function productModelTier(model) {
  const identity = [
    model && model.modelId,
    model && model.name,
    displayName(model),
    model && model.profileId,
    model && model.description,
  ]
    .filter(Boolean)
    .join(' ');
  if (/\bMeadow\s+Lightning\b/i.test(identity)) return 'LIGHTNING';
  const match = identity.match(/(?:^|\s)(26B|30B|35B)(?:\s|$)/i);
  if (match) return match[1].toUpperCase();
  if (/ornith/i.test(identity)) return '35B';
  if (/nemotron/i.test(identity)) return '30B';
  if (/gemma[-_ .]?4.*26|gemma.*26/i.test(identity)) return '26B';
  return '';
}

function isStandardProductModel(model) {
  // API availability is declared by the authenticated native catalog, never
  // inferred from a parameter count or a provider's display name.
  return model?.delivery === 'api' || productModelTier(model) === '35B';
}

function productModelBrief(model) {
  if (model?.delivery === 'api')
    return (
      model.description ||
      model.desc ||
      '使用既有模型 API · 經 Meadow Core 連線'
    );
  if (model && model.description) return model.description;
  if (model && model.official === true)
    return model.description || model.profileId || '官方受保護模型';
  switch (productModelTier(model)) {
    case 'LIGHTNING':
      return '搜尋、證據抽取與短摘要';
    case '35B':
      return '快速 Agent 與多步開發';
    default:
      return '';
  }
}

// 一列＝TS ModelListRow：☆ ＋（名稱列＋⌘N）＋狀態行（● 燈號＋文字）
function modelRow(m, idx, favs) {
  const row = document.createElement('div');
  const selected = modelEntryFor(state.model) === m;
  // The public product catalog is 35B-only. General/Trial and Pro share the
  // same local model; Pro is reserved for the advanced feature capabilities.
  const commercialTier =
    m.delivery === 'api' || productModelTier(m) === '35B' ? 'general' : 'pro';
  const commercial = window.MeadowCommercial;
  const commercialSnapshot = commercial?.snapshot() || {
    mode: 'general',
    protected: true,
    proAllowed: false,
  };
  const commercialGateApplies = commercialSnapshot.protected !== false;
  const proPresentationAllowed =
    !commercialGateApplies || commercialSnapshot.proAllowed;
  const commerciallyUsable = commercial?.canUseModel(m) === true;
  row.className =
    'mrow' + (m.active ? ' active' : '') + (selected ? ' selected' : '');
  row.dataset.commercialTier = commercialTier;
  row.classList.toggle('commercial-locked', !commerciallyUsable);
  row.classList.toggle(
    'commercial-mode-locked',
    commercialTier === 'pro' && proPresentationAllowed && !commerciallyUsable,
  );
  row.setAttribute('role', 'option');
  row.setAttribute('aria-selected', String(selected));
  if (commercialTier === 'pro' && !proPresentationAllowed)
    row.setAttribute('aria-disabled', 'true');
  row.tabIndex = 0;

  const main = document.createElement('div');
  main.className = 'mmain';
  const l1 = document.createElement('div');
  l1.className = 'ml1';
  const identity = document.createElement('div');
  identity.className = 'midentity';
  const nm = document.createElement('span');
  nm.className = 'mname';
  nm.textContent = displayName(m) || `Meadow ${productModelTier(m)}`;
  row.setAttribute('aria-label', nm.textContent);
  nm.title = m.modelId || '';
  identity.appendChild(nm);
  l1.appendChild(identity);
  if (idx < 9) {
    const shortcut = document.createElement('span');
    shortcut.className = 'mkbd';
    shortcut.textContent = `⌘${idx + 1}`;
    l1.appendChild(shortcut);
  }

  const l2 = document.createElement('div');
  l2.className = 'ml2';
  if (m.harnessStatus === 'adapting') row.classList.add('experimental');
  if (
    commercialGateApplies &&
    commercialTier === 'pro' &&
    !proPresentationAllowed
  ) {
    l2.classList.add('commercial-plan-note');
    l2.innerHTML =
      '<span class="mdot"></span><span>尚未解鎖 · 點擊查看授權選項</span>';
  } else if (
    commercialGateApplies &&
    commercialTier === 'pro' &&
    proPresentationAllowed &&
    commercialSnapshot.mode !== 'pro'
  ) {
    l2.classList.add('commercial-plan-note');
    l2.innerHTML = '<span class="mdot"></span><span>點擊啟用進階模型</span>';
  } else if (modelLoading && modelLoading.modelId === m.modelId) {
    row.classList.add('loading');
    l2.classList.add('busy');
    l2.innerHTML =
      '<span class="mdot loading"></span><span>正在啟動 Core 並載入模型…</span><span class="mspin"></span>';
  } else if (modelListRefreshing && m.live === true) {
    row.classList.add('checking');
    l2.classList.add('busy');
    l2.innerHTML =
      '<span class="mdot loading"></span><span>正在確認模型狀態…</span><span class="mspin"></span>';
  } else if (m.live === true) {
    l2.classList.add('ok');
    l2.innerHTML =
      '<span class="mdot live"></span><span>已啟動</span>' +
      (m.active ? '<span class="dim">· 使用中</span>' : '');
    // 卸載 ✕（用戶回饋：釋放記憶體；確認後 POST unload）
    const un = document.createElement('button');
    un.type = 'button';
    un.className = 'munload';
    un.title =
      m.delivery === 'api'
        ? '中斷 API 連線（不會停止上游模型服務）'
        : '卸載模型（釋放記憶體）';
    un.textContent = '✕';
    un.addEventListener('click', async (e) => {
      e.stopPropagation();
      if (un.dataset.arm !== '1') {
        un.dataset.arm = '1';
        un.textContent = m.delivery === 'api' ? '確認中斷？' : '確認卸載？';
        setTimeout(() => {
          un.dataset.arm = '';
          un.textContent = '✕';
        }, 2500);
        return;
      }
      try {
        await daemon.unloadModel();
        toast(
          m.delivery === 'api'
            ? '已中斷 API 連線；上游模型服務仍持續運作'
            : '模型已卸載',
          true,
        );
        if (refreshMemHook) refreshMemHook();
        refetchModels();
      } catch (err) {
        toast('卸載失敗：' + ((err && err.message) || err), false);
      }
    });
    l2.appendChild(un);
  } else if (modelLoadError && modelLoadError.modelId === m.modelId) {
    l2.classList.add('bad');
    l2.innerHTML = '<span class="mdot"></span><span class="mmsg"></span>';
    l2.querySelector('.mmsg').textContent = modelLoadError.msg + '（點擊重試）';
  } else if (modelConfirmId === m.modelId) {
    l2.classList.add('arm');
    l2.innerHTML =
      '<span class="mdot"></span><span>再點一次載入試驗模型（會重啟 server）</span>';
  } else if (m.delivery === 'api') {
    l2.innerHTML =
      '<span class="mdot"></span><span>已設定 API · 等待連線</span>';
  } else if (m.installed === true) {
    l2.classList.add('pending');
    l2.innerHTML = '<span class="mdot"></span><span>本機已安裝 · 未載入</span>';
  } else if (m.harnessStatus === 'adapting') {
    l2.classList.add('pending');
    l2.innerHTML =
      '<span class="mdot"></span><span>專屬 pipeline 尚未完成 · 可載入試驗</span>';
  } else {
    l2.innerHTML = '<span class="mdot"></span><span>未載入</span>';
  }

  main.appendChild(l1);
  const d = document.createElement('div');
  d.className = 'mdesc';
  d.textContent = productModelBrief(m);
  main.appendChild(d);
  main.appendChild(l2);
  row.appendChild(main);
  row.addEventListener('click', () => onModelRowClick(m));
  row.addEventListener('keydown', (event) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    void onModelRowClick(m);
  });
  return row;
}

async function onModelRowClick(m) {
  if (modelListRefreshing || !hasClientAccess()) return;
  if (!window.MeadowCommercial?.requestModelAccess(m)) {
    syncCommercialModelMode();
    paintModelRows();
    return;
  }
  if (m.official === true && m.installed !== true) {
    const accountRevision = attachmentAccountRevision;
    const accountGeneration = clientAccountGeneration();
    const sessionRevision = attachmentSessionRevision;
    const sessionId = state.sessionId;
    const current = () => hasClientAccess() && accountGeneration === clientAccountGeneration() &&
      accountRevision === attachmentAccountRevision &&
      sessionRevision === attachmentSessionRevision && sessionId === state.sessionId;
    closeModelModal();
    try {
      const module = await import('./models.js');
      if (!current()) return;
      await module.openModelSetup();
    } catch {
      if (!current()) return;
      toast('無法開啟官方模型安裝介面', false);
    }
    return;
  }
  // Picking a model changes only the conversation binding. RuntimeControl
  // lazily activates that model when the next prompt reaches ChatStream, so
  // browsing 26/30/35 never allocates weights or replaces a resident model.
  closeModelModal();
  const selection = ++modelConnectionSelection;
  const accountRevision = attachmentAccountRevision;
  const accountGeneration = clientAccountGeneration();
  const modelId = m.modelId;
  const connectionId = m.connectionId || '';
  let sessionId = state.sessionId;
  let sessionRevision = attachmentSessionRevision;
  const ownsSelection = () => hasClientAccess() &&
    accountGeneration === clientAccountGeneration() &&
    accountRevision === attachmentAccountRevision &&
    selection === modelConnectionSelection;
  const ownsSession = () => ownsSelection() &&
    sessionId === state.sessionId && sessionRevision === attachmentSessionRevision;
  try {
    if (connectionId) {
      const changedProvider = sessionId && state.modelConnectionID !== connectionId;
      const response = await fetch('/native/model-providers/select', {
        method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Meadow-Settings': '1' },
        body: JSON.stringify({ connectionId, modelId }),
      });
      if (!ownsSession()) return;
      if (!response.ok) throw new Error(response.status === 409 ? '目前任務仍在執行，請完成或停止後再切換供應商。' : '無法套用模型連線，請回到設定模型重試。');
      const receipt = await response.json();
      if (!ownsSession()) return;
      if (receipt.activation !== 'applied') throw new Error('模型設定已儲存，請重新啟動 Meadow Desktop 後使用。');
      if (changedProvider) {
        const created = await startNewThread();
        if (!ownsSelection()) return;
        if (!created) {
          if (!ownsSession()) return;
          throw new Error('已選擇供應商，請建立新對話後使用。');
        }
        // Only this deliberate new conversation can transfer the selection's
        // ownership. Later awaits must never bind whichever chat is then open.
        if (!created.sessionId || state.sessionId !== created.sessionId ||
            attachmentSessionRevision !== sessionRevision + 1) return;
        sessionId = created.sessionId;
        sessionRevision = attachmentSessionRevision;
      }
      if (!ownsSession()) return;
      state.modelConnectionID = connectionId;
      await refetchModels();
      if (!ownsSession()) return;
    }
    if (sessionId) {
      await setModel(sessionId, modelId);
      if (!ownsSession()) return;
    }
    state.model = modelId;
    $('#pillModelName').textContent = shortName(displayName(m));
    modelLoadError = null;
    modelConfirmId = null;
    reconcileImageCapability();
    toast(
      `已選擇 ${displayName(m)}；送出訊息時會自動${m.delivery === 'api' ? '連線' : '載入'}`,
      true,
    );
  } catch (err) {
    if (!ownsSession()) return;
    console.error('model switch failed:', err);
    toast('切換模型失敗：' + ((err && err.message) || err), false);
  }
}

// modal 開啟時的鍵盤層：Esc 關閉、⌘1-9 直接選對應項（capture 搶在其他模組前）
document.addEventListener(
  'keydown',
  (e) => {
    if (!modelModalOpen()) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      closeModelModal();
      return;
    }
    if ((e.metaKey || e.ctrlKey) && e.key >= '1' && e.key <= '9') {
      const m = modelVisible[Number(e.key) - 1];
      if (!m) return;
      e.preventDefault();
      e.stopPropagation();
      void onModelRowClick(m);
    }
  },
  true,
);

function beginModelLoad(modelId, display) {
  modelLoading = { modelId, display };
  modelLoadError = null;
  if (modelModalOpen()) paintModelRows(); // 立即畫 spinner＋「載入中…」
  (async () => {
    try {
      const r = await daemon.loadModel(modelId);
      if (r && r.loading === false) {
        // already the live serving model — no restart happened
        let name = '';
        try {
          name = ((await daemon.modelStatus()) || {}).model || '';
        } catch {
          /* keep '' — finish falls back to modelId */
        }
        finishModelLoad(modelId, display, name);
        return;
      }
    } catch (err) {
      failModelLoad(modelId, (err && err.message) || String(err));
      return;
    }
    pollModelLoad(modelId, display, Date.now());
  })();
}

// Poll /model/status every 2s. The shell flips status away from "ready"
// before the load-model response returns, so a later "ready" means the
// new server is up;「失敗」/error in the status text means it died.
function pollModelLoad(modelId, display, t0) {
  modelPollTimer = setTimeout(async () => {
    if (!modelLoading || modelLoading.modelId !== modelId) return; // superseded
    let s = null;
    try {
      s = await daemon.modelStatus();
    } catch {
      /* daemon hiccup: keep polling */
    }
    if (!modelLoading || modelLoading.modelId !== modelId) return;
    const readyModel = String((s && s.model) || '').trim();
    const expectedModels = [modelId, display]
      .map((value) =>
        String(value || '')
          .trim()
          .toLowerCase(),
      )
      .filter(Boolean);
    const selectedModelIsReady =
      readyModel && expectedModels.includes(readyModel.toLowerCase());
    if (s && s.status === 'ready' && selectedModelIsReady) {
      finishModelLoad(modelId, display, s.model || '');
      return;
    }
    if (s && /失敗|error/i.test(s.status || '')) {
      failModelLoad(modelId, s.detail || s.status);
      return;
    }
    if (Date.now() - t0 > 300000) {
      failModelLoad(modelId, '載入逾時（300 秒）');
      return;
    }
    pollModelLoad(modelId, display, t0);
  }, 2000);
}

async function finishModelLoad(modelId, display, servedName) {
  modelLoading = null;
  clearTimeout(modelPollTimer);
  toast('模型已啟用：' + display, true);
  // Prefer the public served identifier when one is available.
  const useId = servedName && !servedName.includes('/') ? servedName : modelId;
  try {
    if (state.sessionId) await setModel(state.sessionId, useId);
    state.model = useId;
    $('#pillModelName').textContent = shortName(servedName || display);
    reconcileImageCapability();
  } catch (err) {
    console.error('model switch after load failed:', err);
    toast(
      '模型已載入，但切換對話失敗：' + ((err && err.message) || err),
      false,
    );
  }
  refreshModelModal(); // fresh /model/list → 該列亮起綠燈「已啟動」
  const acceptedEscalation =
    pendingModelEscalation &&
    pendingModelEscalation.modelId === modelId &&
    state.sessionId
      ? { ...pendingModelEscalation, sessionId: state.sessionId }
      : null;
  if (acceptedEscalation) {
    pendingModelEscalation = null;
    bus.emit('model:escalation-ready', acceptedEscalation);
  }
}

function failModelLoad(modelId, msg) {
  modelLoading = null;
  clearTimeout(modelPollTimer);
  modelLoadError = { modelId, msg: msg || '載入失敗' };
  toast('模型載入失敗：' + (msg || ''), false);
  refreshModelModal(); // red text on the item; click-to-retry restored
}

// ---------- footer pills ----------
// 拆兩個選單（語意分層）：
//  核准模式（approval）＝確認每步／自動編輯／全自動，三檔 → POST /approval。
//  互動模式（interaction）＝一般／規劃，規劃也走 Go 引擎的 approval 欄位（ModePlan），
//   但語意上是「先研擬計畫經確認才動手」，與核准模式正交。
// 兩個 pill 共用同一個 /approval 端點，由 applyMode() 統一仲裁避免打架：
//  互動＝規劃時送 plan、核准模式 pill 鎖定顯示「規劃中」；互動回一般時補送回記住的核准模式值。
// label 文案以設定頁「核准模式」為唯一來源（settings.js APPROVAL_MODES），
// 兩處不同步會被用戶抓（M8 回饋：「這兩篇要統一，已設定的文字為主」）。
const APPROVALS = [
  {
    id: 'default',
    label: '確認每步',
    desc: '執行指令與檔案變更前先詢問。',
    ic: 'lock',
  },
  {
    id: 'auto-edit',
    label: '自動編輯',
    desc: '自動核准編輯，其他動作仍會詢問。',
    ic: 'pen',
  },
  {
    id: 'yolo',
    label: '全自動',
    desc: '自動執行指令與編輯；破壞性操作仍會詢問。',
    ic: 'lockOpen',
  },
];
const APPROVAL_MODE_KEY = 'meadow.approvalMode';
const INTERACTS = [
  {
    id: 'normal',
    label: '一般',
    desc: '直接依核准模式設定執行。',
    ic: 'bars',
  },
  {
    id: 'plan',
    label: '規劃',
    desc: '先研擬計畫，經你確認後才動手。',
    ic: 'todo',
  },
];

function initPills() {
  const pop = document.createElement('div');
  pop.className = 'pop';
  document.body.appendChild(pop);

  const pillModel = $('#pillModel');
  pillModel.setAttribute('aria-haspopup', 'dialog');
  pillModel.setAttribute('aria-controls', 'modelModal');
  pillModel.setAttribute('aria-expanded', 'false');
  const pillResearch = $('#pillResearch');
  const pillMode = $('#pillMode'); // 核准模式
  const pillInteract = $('#pillInteract'); // 互動模式
  const pillFolder = $('#pillFolder');

  state.researchMode = localStorage.getItem(RESEARCH_MODE_KEY) === 'research';
  const renderResearchPill = () => {
    if (!pillResearch) return;
    pillResearch.classList.toggle('on', state.researchMode);
    pillResearch.setAttribute('aria-pressed', String(state.researchMode));
    pillResearch.title = state.researchMode
      ? '資料搜集模式已開啟；下一輪會搜尋、建立 RAG 並輸出帶引用文件'
      : '開啟資料搜集模式';
  };
  renderResearchPill();
  pillResearch?.addEventListener('click', () => {
    state.researchMode = !state.researchMode;
    localStorage.setItem(
      RESEARCH_MODE_KEY,
      state.researchMode ? 'research' : 'auto',
    );
    renderResearchPill();
    toast(
      state.researchMode
        ? '資料搜集模式已開啟：下一輪會先分析需求，再搜尋、閱讀並建立 RAG。'
        : '資料搜集模式已關閉：恢復由模型判斷任務。',
      true,
    );
  });

  buildModelModal();
  void loadInferenceControls();
  refetchModels(); // 開機首抓只填快取；模型權重等到送出訊息才載入。
  if (new URLSearchParams(window.location.search).has('autoHover')) {
    setTimeout(() => {
      openModelModal();
      console.log('[model-picker:hover] QA first level opened without click');
    }, 500);
  }

  // --- 統一狀態機：approval ∈ {default,auto-edit,yolo}，interact ∈ {normal,plan} ---
  // curApproval 記住「非規劃」的核准模式選擇，規劃結束回一般時補送回去。
  // applyMode 是兩個 pill 的唯一寫入口；render 不打 server，網路只在 commit() 發生。
  // Start each session with the user's saved choice. First-time Desktop
  // sessions may edit the selected workspace, but host command execution still
  // requires a visible confirmation. Users can deliberately opt into yolo and
  // that explicit choice remains sticky.
  const savedApproval = localStorage.getItem(APPROVAL_MODE_KEY);
  const hasSavedApproval = APPROVALS.some((x) => x.id === savedApproval);
  let curApproval = hasSavedApproval ? savedApproval : 'auto-edit';
  if (!hasSavedApproval) {
    localStorage.setItem(APPROVAL_MODE_KEY, curApproval);
  }
  let curInteract = 'normal';

  // 規劃中，核准模式 pill 灰掉顯示「規劃中」、點不開；回一般才恢復。
  const planActive = () => curInteract === 'plan';

  const renderModePill = () => {
    const nameEl = $('#pillModeName');
    const ic = pillMode.querySelector('.ic');
    if (planActive()) {
      nameEl.textContent = '規劃中';
      if (ic) ic.innerHTML = ICONS.todo;
      pillMode.classList.add('locked');
      pillMode.setAttribute('aria-disabled', 'true');
      pillMode.title = '規劃模式中由互動模式接管；切回「一般」可調整核准模式';
    } else {
      const found = APPROVALS.find((x) => x.id === curApproval);
      nameEl.textContent = found ? found.label : curApproval;
      if (ic && found) ic.innerHTML = ICONS[found.ic];
      pillMode.classList.remove('locked');
      pillMode.removeAttribute('aria-disabled');
      pillMode.title = '核准模式';
    }
  };
  const renderInteractPill = () => {
    const found = INTERACTS.find((x) => x.id === curInteract);
    $('#pillInteractName').textContent = found ? found.label : curInteract;
    const ic = pillInteract.querySelector('.ic');
    if (ic && found) ic.innerHTML = ICONS[found.ic];
  };
  const renderPills = () => {
    renderModePill();
    renderInteractPill();
  };

  // POST 一次 /approval：plan 時送 'plan'，否則送目前核准模式值。
  commitApprovalMode = async () => {
    if (!state.sessionId) return false;
    const mode = planActive() ? 'plan' : curApproval;
    try {
      await daemon.setApproval(state.sessionId, mode);
      return true;
    } catch (err) {
      // Agentd can be restarted independently while the Desktop window stays
      // open. A stale live-session id must recover before the user's first
      // prompt instead of leaving the whole composer permanently unusable.
      if (err?.status === 404) {
        const staleSession = state.sessionId;
        const replacement = await startNewThread();
        if (
          replacement &&
          state.sessionId &&
          state.sessionId !== staleSession
        ) {
          try {
            await daemon.setApproval(state.sessionId, mode);
            return true;
          } catch (retryErr) {
            console.error('setApproval recovery failed:', retryErr);
          }
        }
      }
      console.error('setApproval failed:', err);
      return false;
    }
  };

  // 唯一寫入口：傳 null 表示「保持現值」。先 render（即時反映）再 commit（送 server）。
  // 進規劃不動 curApproval（記住），離開規劃自動補送回記住的核准模式值。
  async function applyMode(approval, interact) {
    if (approval != null) {
      curApproval = approval;
      localStorage.setItem(APPROVAL_MODE_KEY, approval);
    }
    if (interact != null) curInteract = interact;
    renderPills();
    await commitApprovalMode();
  }
  // 把 daemon 回吐的 approval mode 反向映射成兩軸狀態（SSE echo / resume 對齊用）。
  const reflectFromMode = (m) => {
    if (m === 'plan') {
      curInteract = 'plan';
    } else if (APPROVALS.some((x) => x.id === m)) {
      curInteract = 'normal';
      curApproval = m;
    } else {
      return;
    }
    renderPills();
  };
  renderPills();
  // Apply the saved/default permission envelope before the first agent action.
  // This is deliberately fire-and-forget because initPills is synchronous;
  // prompt submission still serializes through the session daemon.
  void commitApprovalMode();

  // --- model pill: click opens/closes the first panel. The model setting row
  // inside that panel owns second-level hover discovery. ---
  for (const trigger of [pillModel]) {
    trigger.addEventListener('click', () => {
      if (modelModalOpen() && modelModalAnchor === trigger) closeModelModal();
      else openModelModal(trigger);
    });
  }

  // --- 共用 .pop 選單列（icon＋label＋描述，TS Select min-w-64） ---
  const makeItem = (m, activeId, onPick) => {
    const it = document.createElement('div');
    it.className = 'it mode-it' + (m.id === activeId ? ' on' : '');
    it.innerHTML =
      `<div class="mi-top">${ICONS[m.ic]}<span class="mi-lb"></span>` +
      (m.id === activeId ? '<span class="chk">✓</span>' : '') +
      `</div><div class="mi-desc"></div>`;
    it.querySelector('.mi-lb').textContent = m.label;
    it.querySelector('.mi-desc').textContent = m.desc;
    it.addEventListener('click', () => {
      closePop();
      onPick(m.id);
    });
    return it;
  };

  // --- 核准模式 pill：規劃中鎖定不開；選檔＝設 approval、互動軸不動 ---
  pillMode.addEventListener('click', () => {
    if (planActive()) return; // 規劃中由互動模式接管
    if (openedPop && openedPop.anchor === pillMode) {
      closePop();
      return;
    }
    closePop();
    pop.innerHTML = '<div class="hd">核准模式</div>';
    for (const m of APPROVALS)
      pop.appendChild(makeItem(m, curApproval, (id) => applyMode(id, null)));
    openPop(pop, pillMode);
  });

  // --- 互動模式 pill：一般／規劃；切換驅動核准模式 pill 鎖定／解鎖 ---
  pillInteract.addEventListener('click', () => {
    if (openedPop && openedPop.anchor === pillInteract) {
      closePop();
      return;
    }
    closePop();
    pop.innerHTML = '<div class="hd">互動模式</div>';
    for (const m of INTERACTS)
      pop.appendChild(makeItem(m, curInteract, (id) => applyMode(null, id)));
    openPop(pop, pillInteract);
  });

  // --- folder pill（#checkoutRow）: native picker → persist cwd → reload ---
  // Locked once the thread has messages: a started chat stays bound to its
  // folder (the pill turns into a location indicator); a fresh thread can
  // still switch freely.
  const folderAbove = $('#folderAbove');
  const reflectFolderLock = () => {
    const locked = !!state.hasMessages;
    pillFolder.classList.toggle('locked', locked);
    const ic = pillFolder.querySelector('.ic');
    if (ic) ic.innerHTML = locked ? ICONS.lock : ICONS.folder;
    if (locked) pillFolder.title = '對話已綁定此資料夾，開新對話可更換';
    else pillFolder.removeAttribute('title');
    // 新對話＝置中模式：composer 垂直置中、資料夾 selector 浮在輸入框上方、
    // 下方的資料夾 pill 收起；開聊回到沉底佈局（鎖定 pill 在 checkoutRow）。
    document.body.classList.toggle('fresh-thread', !locked);
    if (folderAbove) {
      folderAbove.hidden = locked;
      const nm = $('#folderAboveName');
      if (nm) nm.textContent = shortName(state.cwd) || '選擇資料夾';
    }
  };

  const pickWorkspaceFolder = async () => {
    if (state.hasMessages) return; // conversation started: folder is fixed
    closePop();
    try {
      const r = await daemon.pickFolder();
      if (r && r.path) {
        localStorage.setItem(CWD_KEY, r.path);
        location.reload();
      }
    } catch (err) {
      console.error('pickFolder failed:', err);
    }
  };
  pillFolder.addEventListener('click', pickWorkspaceFolder);
  const folderAboveBtn = $('#folderAboveBtn');
  if (folderAboveBtn)
    folderAboveBtn.addEventListener('click', pickWorkspaceFolder);

  // Conversation-started tracking. 'thread:switched' fires on every switch
  // (new chat + resume) → unlock; then either the live SSE echo of a sent
  // prompt ('update:user_message_chunk') or the resumed-history payload
  // ('history:load', emitted by sidebar.js after the switch) re-locks.
  bus.on('thread:switched', () => {
    state.hasMessages = false;
    resetInferenceControls();
    reflectFolderLock();
    void loadInferenceControls();
  });
  bus.on('update:user_message_chunk', () => {
    state.hasMessages = true;
    reflectFolderLock();
  });
  bus.on('history:load', (msgs) => {
    // empty history (edge case) behaves like a fresh thread: stay unlocked
    if (Array.isArray(msgs) ? msgs.length > 0 : !!msgs) {
      state.hasMessages = true;
      reflectFolderLock();
    }
  });
  reflectFolderLock();

  bus.on('model_catalog_changed', () => {
    void refetchModels();
  });

  // keep labels in sync with daemon-side switches；
  // model 空值（未載入且非載入中）→ 回「請選擇模型」
  bus.on('model_status', (s) => {
    if (!s) return;
    // Idle unload is reported by the shared status poll even when the picker is
    // closed. Invalidate persisted live badges immediately; the next picker
    // open will still perform an authoritative /model/list probe.
    if (s.status !== 'ready') {
      let changed = false;
      modelCache = modelCache.map((m) => {
        if (m.live !== true && m.active !== true) return m;
        changed = true;
        return { ...m, live: false, active: false };
      });
      if (changed) {
        try {
          localStorage.setItem(
            MODEL_CACHE_KEY,
            JSON.stringify(cacheableModelList(modelCache)),
          );
        } catch {
          /* private mode */
        }
        if (modelModalOpen()) paintModelRows();
      }
    } else if (s.model) {
      const resident = String(s.model).trim().toLowerCase();
      const connectionId = typeof s.connectionId === 'string' ? s.connectionId : '';
      let changed = false;
      modelCache = modelCache.map((m) => {
        const candidates = [m.modelId, m.name, displayName(m)]
          .filter(Boolean)
          .map((value) => String(value).trim().toLowerCase());
        const live = m.delivery === 'api' || m.connectionId
          ? (m.connectionId || '') === connectionId && m.modelId === s.model
          : !connectionId && candidates.some(
            (value) =>
              value === resident ||
              shortName(value).toLowerCase() ===
                shortName(resident).toLowerCase(),
          );
        if (m.live === live && m.active === live) return m;
        changed = true;
        return { ...m, live, active: live };
      });
      if (changed) {
        try {
          localStorage.setItem(
            MODEL_CACHE_KEY,
            JSON.stringify(cacheableModelList(modelCache)),
          );
        } catch {
          /* private mode */
        }
        if (modelModalOpen()) paintModelRows();
      }
    }
    // A status poll describes the shared runtime, which may currently serve a
    // different provider. It cannot rebind this conversation by model ID alone.
    if ((s.connectionId || '') !== (state.modelConnectionID || '')) return;
    const el = $('#pillModelName');
    const statusModelIsCurrent =
      !!s.model && (!modelFetched || modelCatalogContains(s.model));
    const selectedModelIsCurrent =
      !!state.model && (!modelFetched || modelCatalogContains(state.model));
    if (selectedModelIsCurrent) {
      // Keep the model bound to this conversation. The shared Core may be
      // serving a background thread temporarily; status polling is
      // observational and must not change the active thread's model.
      el.textContent = selectedModelName(state.model);
      reconcileImageCapability();
    } else if (statusModelIsCurrent) {
      state.model = s.model;
      el.textContent = selectedModelName(s.model);
      reconcileImageCapability();
    } else if (!modelLoading) {
      if (modelFetched && s.model) state.model = '';
      el.textContent = '請選擇模型';
    }
  });
  bus.on('model_switched', (d) => {
    const m = d && (d.model || d.modelId || d.name);
    if (typeof m === 'string' && m) {
      state.model = m;
      $('#pillModelName').textContent = selectedModelName(m);
      reconcileImageCapability();
    }
    resetInferenceControls();
    void loadInferenceControls();
  });
  bus.on('inference_controls_changed', (d) => {
    if (!d || (d.sessionId && d.sessionId !== state.sessionId)) return;
    // This payload has no server revision, so its arrival order cannot tell
    // whether it predates a confirmed HTTP save. Treat it as invalidation.
    if (inferenceControlsSaving) inferenceControlsRefreshRequested = true;
    else void loadInferenceControls();
  });
  bus.on('model:escalate', async (d) => {
    const modelId = String((d && d.modelId) || '').trim();
    if (!modelId) return;
    const entry = modelCache.find((m) =>
      [m.modelId, m.name, displayName(m)].some(
        (value) =>
          String(value || '')
            .trim()
            .toLowerCase() === modelId.toLowerCase(),
      ),
    );
    if (!entry) {
      if (d && typeof d.onError === 'function') d.onError();
      toast(`找不到可用模型：${modelId}`, false);
      return;
    }
    pendingModelEscalation = {
      modelId: entry.modelId,
      resumePrompt: String((d && d.resumePrompt) || '').trim(),
      sessionId: state.sessionId,
    };
    if (entry.live === true) {
      try {
        if (state.sessionId) await setModel(state.sessionId, entry.modelId);
        const acceptedEscalation = {
          ...pendingModelEscalation,
          sessionId: state.sessionId,
        };
        pendingModelEscalation = null;
        bus.emit('model:escalation-ready', acceptedEscalation);
      } catch (err) {
        pendingModelEscalation = null;
        if (d && typeof d.onError === 'function') d.onError();
        toast('任務接續失敗：' + ((err && err.message) || err), false);
      }
      return;
    }
    beginModelLoad(entry.modelId, displayName(entry));
  });
  // daemon-side switches (e.g. slash command / resume) → reconcile both axes
  bus.on('approval_mode_changed', (d) => {
    const m = d && (d.mode || d.approvalMode);
    if (typeof m === 'string' && m) reflectFromMode(m);
  });
  bus.on('session_created', (session) => {
    // A fresh server session starts with its configured default mode, while
    // the composer pill intentionally keeps the user's current selection.
    // Re-apply that visible selection to the new session immediately so the
    // UI cannot say「全自動」while write_file still opens a permission card.
    void commitApprovalMode();
    // Model identity is different: the Core resident/selected model is
    // authoritative. Reflect the model bound by the backend so a cold launch
    // cannot display a stale localStorage picker choice above another model's
    // session.
    const modelId = String((session && session.modelId) || '').trim();
    if (modelId) {
      state.model = modelId;
      $('#pillModelName').textContent = selectedModelName(modelId);
      clearRetiredSessionModel();
    }
    resetInferenceControls();
    void loadInferenceControls();
  });
}

// ---------- pending cards ----------
const permCards = new Map(); // requestId -> element
const uiCards = new Map(); // requestId -> element

function permissionCardCancelled(requestId, label = '✕ 已取消') {
  const card = permCards.get(requestId);
  if (!card || card.classList.contains('done')) return false;
  card.classList.add('done');
  const tlHost = document.getElementById('timeline');
  if (tlHost) tlHost.appendChild(card);
  const top = card.querySelector('.perm-top');
  const verdict = document.createElement('div');
  verdict.className = 'verdict';
  verdict.textContent = label;
  (top || card).appendChild(verdict);
  const details = card.querySelector('.perm-details');
  if (details) details.open = false;
  const err = card.querySelector('.err');
  if (err) err.hidden = true;
  return true;
}

function cancelPermissionCardsForSession(sessionId) {
  let count = 0;
  for (const [requestId, card] of permCards) {
    if (!sessionId || card.dataset.sessionId === sessionId)
      count += permissionCardCancelled(requestId);
  }
  return count;
}

function permissionAlreadyResolved(err) {
  const status = Number(err && err.status);
  const message = String((err && err.message) || err || '').toLowerCase();
  return (
    status === 404 ||
    status === 409 ||
    message.includes('permission already resolved') ||
    message.includes('already-resolved') ||
    message.includes('unknown permission')
  );
}

// drop already-decided cards when a fresh prompt starts
function sweepResolvedCards() {
  for (const [id, el] of [...permCards]) {
    if (el.classList.contains('done')) {
      el.remove();
      permCards.delete(id);
    }
  }
  for (const [id, el] of [...uiCards]) {
    if (el.classList.contains('resolved')) {
      el.remove();
      uiCards.delete(id);
    }
  }
}

const PERM_LABELS = {
  approve: '允許',
  approve_always: '一律允許',
  reject: '拒絕',
};
const KIND_TO_ID = {
  allow_once: 'approve',
  allow_always: 'approve_always',
  reject_once: 'reject',
  reject_always: 'reject',
};

function permissionSummary(input) {
  if (!input || typeof input !== 'object') return String(input || '');
  const preferred = ['query', 'url', 'file_path', 'path', 'command'];
  for (const key of preferred) {
    const value = String(input[key] || '').trim();
    if (value) return `${key}: ${value}`;
  }
  const keys = Object.keys(input);
  return keys.length ? keys.slice(0, 3).join(' · ') : '沒有額外參數';
}

function initPermissionCards() {
  const area = $('#pendingArea');


  bus.on('permission_request', (d) => {
    if (!d || !d.requestId || permCards.has(d.requestId)) return;
    const tc = d.toolCall || {};
    let raw;
    try {
      raw = JSON.stringify(tc.rawInput ?? {}, null, 2);
    } catch {
      raw = String(tc.rawInput);
    }
    if (raw.length > 20000) raw = raw.slice(0, 20000) + '\n…';

    const card = document.createElement('div');
    card.className = 'perm';
    card.dataset.sessionId = d.sessionId || state.sessionId || '';
    card.innerHTML = `
      <div class="perm-top">
        <div class="ti">⚠ 權限請求 · ${esc(tc.title || tc.kind || '工具')}</div>
      </div>
      <details class="perm-details">
        <summary><span class="perm-summary">${esc(permissionSummary(tc.rawInput))}</span><span class="perm-more">查看參數</span></summary>
        <pre>${esc(raw)}</pre>
      </details>
      <div class="bts"></div>
      <div class="err" hidden></div>`;

    const bts = card.querySelector('.bts');
    const opts =
      d.options && d.options.length
        ? d.options
        : [
            { optionId: 'approve' },
            { optionId: 'approve_always' },
            { optionId: 'reject' },
          ];
    for (const o of opts) {
      const id = o.optionId || KIND_TO_ID[o.kind] || 'reject';
      const b = document.createElement('button');
      b.className = id === 'approve' ? 'y' : id === 'reject' ? 'n' : '';
      b.textContent = PERM_LABELS[id] || o.name || id;
      b.addEventListener('click', async () => {
        for (const x of bts.querySelectorAll('button')) x.disabled = true;
        try {
          await daemon.vote(d.sessionId || state.sessionId, d.requestId, id);
          // final state is painted by permission_resolved
        } catch (err) {
          // Stop/cancel can win immediately before this click reaches the
          // daemon. A stale vote is already a terminal state, not a UI error.
          if (permissionAlreadyResolved(err)) {
            permissionCardCancelled(d.requestId);
            return;
          }
          for (const x of bts.querySelectorAll('button')) x.disabled = false;
          const e2 = card.querySelector('.err');
          e2.hidden = false;
          e2.textContent = '送出失敗：' + ((err && err.message) || err);
        }
      });
      bts.appendChild(b);
    }
    // Fourth action (TS parity): reject this tool AND cancel the whole turn.
    const cancelBtn = document.createElement('button');
    cancelBtn.className = 'n';
    cancelBtn.textContent = '取消整輪';
    cancelBtn.addEventListener('click', async () => {
      for (const x of bts.querySelectorAll('button')) x.disabled = true;
      try {
        const sid = d.sessionId || state.sessionId;
        // Cancel the run first. The server atomically resolves every pending
        // permission, so parallel web_search cards cannot race a reject vote.
        await daemon.cancel(sid);
        permissionCardCancelled(d.requestId);
      } catch (err) {
        if (permissionAlreadyResolved(err)) {
          permissionCardCancelled(d.requestId);
          return;
        }
        for (const x of bts.querySelectorAll('button')) x.disabled = false;
        const e2 = card.querySelector('.err');
        e2.hidden = false;
        e2.textContent = '送出失敗：' + ((err && err.message) || err);
      }
    });
    bts.appendChild(cancelBtn);
    permCards.set(d.requestId, card);
    area.appendChild(card);
  });

  bus.on('permission_resolved', (d) => {
    if (!d || !d.requestId) return;
    const card = permCards.get(d.requestId);
    const o = d.outcome || {};
    const inner = o.outcome && typeof o.outcome === 'object' ? o.outcome : o;
    const oid = inner.optionId;
    const cancelled =
      inner.outcome === 'cancelled' || o.outcome === 'cancelled';
    if (cancelled) {
      permissionCardCancelled(d.requestId);
      return;
    }
    if (!card || card.classList.contains('done')) return;
    card.classList.add('done'); // existing CSS hides .bts
    // 已決定的卡收進時間軸尾端（原本一直黏在 composer 上方很礙眼）：
    // 之後的訊息自然把它往上推，視覺上停在它發生的那段對話下面。
    const tlHost = document.getElementById('timeline');
    if (tlHost) tlHost.appendChild(card);
    const v = document.createElement('div');
    v.className = 'verdict';
    v.textContent = `✓ 已決定：${PERM_LABELS[oid] || oid || ''}`;
    const top = card.querySelector('.perm-top');
    (top || card).appendChild(v);
    const details = card.querySelector('.perm-details');
    if (details) details.open = false;
  });
}

function initUserInputCards() {
  const area = $('#pendingArea');

  const closeChoiceDialog = (card) => {
    const dialog = card?._choiceDialog;
    if (!dialog) return;
    if (dialog.open) dialog.close();
    ($('#timeline') || area).appendChild(card);
    dialog.remove();
    card._choiceDialog = null;
  };


  const placePendingCard = (card) => {
    if (!card || card.classList.contains('resolved')) return;
    const active = card.dataset.sessionId === state.sessionId;
    card.hidden = !active;
    if (!active) {
      closeChoiceDialog(card);
      return;
    }
    const timeline = $('#timeline');
    if (card._qs?.[0]?.presentation === 'choice_dialog' && !card._choiceDismissed) {
      if (card._choiceDialog?.open) return;
      const dialog = document.createElement('dialog');
      dialog.className = 'uinput-choice-dialog';
      dialog.setAttribute('aria-label', card._qs[0].question);
      const dismiss = document.createElement('button');
      dismiss.type = 'button';
      dismiss.className = 'choice-dismiss';
      dismiss.textContent = '×';
      dismiss.setAttribute('aria-label', '稍後決定');
      const deferChoice = () => {
        card._choiceDismissed = true;
        closeChoiceDialog(card);
        // Dismiss only the overlay. The unanswered card remains available;
        // neither Escape nor closing the dialog grants source reuse.
        card.scrollIntoView({ block: 'nearest' });
      };
      dismiss.addEventListener('click', deferChoice);
      dialog.addEventListener('cancel', (event) => {
        event.preventDefault();
        deferChoice();
      });
      dialog.append(dismiss, card);
      document.body.appendChild(dialog);
      card._choiceDialog = dialog;
      dialog.showModal();
      dismiss.focus();
      return;
    }
    (timeline || area).appendChild(card);
    requestAnimationFrame(() =>
      card.scrollIntoView({ block: 'nearest', behavior: 'smooth' }),
    );
  };

  bus.on('user_input_request', (d) => {
    if (!d || !d.requestId || uiCards.has(d.requestId)) return;
    const qs = d.questions || [];
    if (!qs.length) return;

    const card = document.createElement('div');
    card.className = 'card uinput';
    card.dataset.sessionId = d.sessionId || state.sessionId || '';
    card._qs = qs; // kept for resolve-time summary
    const answers = {}; // qid -> label | [labels]
    const customAnswers = {}; // qid -> free-form answer
    let idx = 0;
    let submitted = false;

    const answered = (q) => {
      const a = answers[q.id];
      const custom = String(customAnswers[q.id] || '').trim();
      return (
        custom !== '' ||
        (q.multiSelect
          ? Array.isArray(a) && a.length > 0
          : typeof a === 'string' && a !== '')
      );
    };

    const submittedAnswers = () => {
      const result = {};
      for (const q of qs) {
        const custom = String(customAnswers[q.id] || '').trim();
        if (q.multiSelect) {
          const selected = Array.isArray(answers[q.id])
            ? [...answers[q.id]]
            : [];
          if (custom) selected.push(`其他：${custom}`);
          result[q.id] = selected;
        } else {
          result[q.id] = custom || String(answers[q.id] || '');
        }
      }
      return result;
    };

    function render() {
      const q = qs[idx];
      card.innerHTML = `
        <div class="qhead">
          ${q.header ? `<span class="badge">${esc(q.header)}</span>` : ''}
          <span class="card-title" style="margin:0">需要你的回答</span>
          ${qs.length > 1 ? `<span class="steps">第 ${idx + 1} / ${qs.length} 題</span>` : ''}
        </div>
        <div class="card-body qtext"></div>
        <div class="opts"></div>
        <div class="nav"></div>
        <div class="err" hidden></div>`;
      card.querySelector('.qtext').textContent = q.question || '';

      const optsEl = card.querySelector('.opts');
      for (const o of q.options || []) {
        const el = document.createElement('button');
        el.type = 'button';
        el.className = 'opt';
        el.innerHTML = `<span class="lb"></span>${o.description ? '<span class="desc"></span>' : ''}`;
        el.querySelector('.lb').textContent = o.label;
        if (o.description) {
          el.querySelector('.desc').textContent = o.description;
          el.title = o.description;
        }
        const sel = q.multiSelect
          ? Array.isArray(answers[q.id]) && answers[q.id].includes(o.label)
          : answers[q.id] === o.label;
        el.classList.toggle('sel', sel);
        el.setAttribute('aria-pressed', String(sel));
        el.addEventListener('click', () => {
          if (submitted) return;
          if (q.multiSelect) {
            // toggle
            const cur = Array.isArray(answers[q.id]) ? answers[q.id] : [];
            answers[q.id] = cur.includes(o.label)
              ? cur.filter((x) => x !== o.label)
              : [...cur, o.label];
          } else {
            // single choice: click = select
            answers[q.id] = o.label;
            customAnswers[q.id] = '';
          }
          if (!q.multiSelect && idx < qs.length - 1) {
            idx++;
            render();
            return;
          }
          if (!q.multiSelect && qs.length === 1 && !q.allowOther) {
            render();
            submitAnswers();
            return;
          }
          render();
        });
        optsEl.appendChild(el);
      }

      if (q.allowOther) {
        const other = document.createElement('label');
        other.className = 'other-answer';
        other.innerHTML =
          '<span>其他，我想補充</span><input type="text" autocomplete="off" maxlength="600">';
        const input = other.querySelector('input');
        input.placeholder = '輸入選項以外的需求…';
        input.value = customAnswers[q.id] || '';
        input.disabled = submitted;
        input.addEventListener('input', () => {
          customAnswers[q.id] = input.value;
          if (!q.multiSelect && input.value.trim()) {
            answers[q.id] = '';
            for (const option of optsEl.querySelectorAll('.opt')) {
              option.classList.remove('sel');
              option.setAttribute('aria-pressed', 'false');
            }
          }
          const next = card.querySelector('.nav .primary');
          if (next) next.disabled = submitted || !answered(q);
        });
        optsEl.appendChild(other);
      }

      const nav = card.querySelector('.nav');
      if (idx > 0) {
        const back = document.createElement('button');
        back.className = 'btn';
        back.textContent = '上一題';
        back.disabled = submitted;
        back.addEventListener('click', () => {
          if (!submitted) {
            idx--;
            render();
          }
        });
        nav.appendChild(back);
      }
      const last = idx === qs.length - 1;
      if (last && !q.multiSelect && qs.length === 1 && !q.allowOther) {
        nav.hidden = true;
        return;
      }
      const next = document.createElement('button');
      next.className = 'btn primary';
      next.textContent = last ? '送出回答' : '下一題';
      next.disabled = submitted || !answered(q);
      next.addEventListener('click', () => {
        if (submitted) return;
        if (!last) {
          idx++;
          render();
          return;
        }
        submitAnswers();
      });
      nav.appendChild(next);
    }

    async function submitAnswers() {
      if (submitted) return;
      const q = qs[idx];
      const missing = qs.findIndex((q2) => !answered(q2));
      if (missing >= 0) {
        idx = missing;
        render();
        return;
      }
      submitted = true;
      card.classList.add('submitting');
      for (const b of card.querySelectorAll('button')) b.disabled = true;
      const primary = card.querySelector('.nav .primary');
      if (primary) primary.textContent = '送出中…';
      try {
        await daemon.answer(
          d.sessionId || state.sessionId,
          d.requestId,
          submittedAnswers(),
        );
        // collapse is painted by user_input_resolved
      } catch (err) {
        submitted = false;
        card.classList.remove('submitting');
        render();
        const e2 = card.querySelector('.err');
        e2.hidden = false;
        e2.textContent = '送出失敗：' + ((err && err.message) || err);
      }
    }

    render();
    uiCards.set(d.requestId, card);
    placePendingCard(card);
  });

  bus.on('user_input_resolved', (d) => {
    if (!d || !d.requestId) return;
    const card = uiCards.get(d.requestId);
    if (!card || card.classList.contains('resolved')) return;
    closeChoiceDialog(card);
    card.classList.add('resolved');
    card.hidden = card.dataset.sessionId !== state.sessionId;
    if (d.cancelled) {
      card.innerHTML = '<div class="card-title">已取消，尚未選擇</div>';
      return;
    }
    if (d.superseded) {
      card.innerHTML = '<div class="card-title">↪ 已由專家引導取代</div>';
      return;
    }
    const qs = card._qs || [];
    const answers = d.answers || {};
    const lines = Object.keys(answers)
      .map((qid) => {
        const q = qs.find((x) => x.id === qid);
        const a = answers[qid];
        const label = Array.isArray(a) ? a.join('、') : String(a);
        return `<div class="ansline">${esc(q ? q.question : qid)}：<b>${esc(label)}</b></div>`;
      })
      .join('');
    card.innerHTML = `<div class="card-title">✓ 已回答</div>${lines}`;
  });

  bus.on('thread:switched', () => {
    for (const card of uiCards.values()) placePendingCard(card);
  });
}

// ---------- context ring (#ctxMeter) ----------
function initCtxMeter() {
  const host = $('#ctxMeter');
  const R = 10.5; // 26px ring, like the Electron meter
  const C = 2 * Math.PI * R;
  let kind = ''; // 'ring' | 'text'
  let contextUsed = 0;
  let contextMax = 0;
  let contextEstimated = true;
  let ctxPop = null;

  function ensure(k) {
    if (kind === k) return;
    kind = k;
    if (k === 'ring') {
      host.innerHTML = `
        <div class="ring">
          <svg viewBox="0 0 26 26" width="26" height="26" aria-hidden="true">
            <circle class="track" cx="13" cy="13" r="${R}" fill="none" stroke-width="2.5"></circle>
            <circle class="arc" cx="13" cy="13" r="${R}" fill="none" stroke-width="2.5"
              stroke-linecap="round" stroke-dasharray="${C}" stroke-dashoffset="${C}"></circle>
          </svg>
          <span class="pct"></span>
          <div class="tip"></div>
        </div>`;
    } else {
      host.innerHTML = `<div class="ring text"><span class="cnt"></span><div class="tip"></div></div>`;
    }
  }

  function paintContext() {
    if (!contextMax) return;
    ensure('ring');
    const pct = Math.max(0, Math.min(100, (contextUsed / contextMax) * 100));
    host
      .querySelector('.arc')
      .setAttribute('stroke-dashoffset', String(C - (pct / 100) * C));
    const percentage = pct > 0 && pct < 1 ? '<1' : String(Math.round(pct));
    const usageLabel = `上下文使用率 ${percentage}%`;
    host.querySelector('.pct').textContent = `${percentage}%`;
    host.title = usageLabel;
    host.setAttribute('aria-label', usageLabel);
    host.querySelector('.tip').textContent =
      `${usageLabel} · ${contextEstimated ? '約 ' : ''}${fmtK(contextUsed)}/${fmtContextCapacity(contextMax)} · 點擊整理`;
    const ring = host.querySelector('.ring');
    ring.classList.toggle('warn', pct >= 80 && pct < 95);
    ring.classList.toggle('crit', pct >= 95);
    if (ctxPop && ctxPop.classList.contains('open')) paintContextPopover();
  }

  async function refreshContext() {
    const sid = state.sessionId;
    if (!sid) return;
    try {
      const data = await daemon.contextStatus(sid);
      if (state.sessionId !== sid) return;
      contextUsed = Number(data.usedTokens) || 0;
      // Prefer the live worker's physical window. Older/unknown runtime replies
      // can still return 0, so retain the last known capacity as a fallback.
      contextMax = Number(data.contextWindowTokens) || contextMax || 131072;
      contextEstimated = !!data.estimated;
      paintContext();
    } catch (e) {
      console.error('context status failed:', e);
    }
  }

  function paintContextPopover(message = '') {
    if (!ctxPop) return;
    if (!contextMax) contextMax = 131072;
    const pct = contextMax
      ? Math.min(100, (contextUsed / contextMax) * 100)
      : 0;
    ctxPop.innerHTML =
      `<div class="hd">${esc(fmtContextCapacity(contextMax))} 模型工作記憶</div>` +
      `<div class="ctx-value">${contextEstimated ? '約 ' : ''}${fmtK(contextUsed)} / ${fmtContextCapacity(contextMax)} tokens</div>` +
      `<div class="ctx-bar"><i style="width:${pct.toFixed(1)}%"></i></div>` +
      (message
        ? `<div class="ctx-note">${esc(message)}</div>`
        : '<div class="ctx-note">接近上限時會整理模型看到的舊內容；完整對話仍保留。手動整理會保留目標、限制、決策、檔案、驗證與待辦。</div>') +
      '<button type="button" class="ctx-compact">整理並保留重點</button>';
    const button = ctxPop.querySelector('.ctx-compact');
    button.disabled = !!state.busy;
    if (state.busy) button.title = '任務執行中，完成後才能整理';
    button.addEventListener('click', async (e) => {
      e.stopPropagation();
      button.disabled = true;
      button.textContent = '正在整理…';
      try {
        const data = await daemon.compactContext(state.sessionId);
        contextUsed = Number(data.afterTokens) || contextUsed;
        contextEstimated = true;
        paintContext();
        paintContextPopover(
          `已從 ${fmtK(data.beforeTokens)} 整理為 ${fmtK(data.afterTokens)} tokens`,
        );
      } catch (e2) {
        paintContextPopover('整理失敗：' + ((e2 && e2.message) || e2));
      }
    });
  }

  ensure('ring');
  host.querySelector('.pct').textContent = '0%';
  host.title = '上下文使用率 · 正在讀取容量';
  host.setAttribute('aria-label', host.title);
  host.querySelector('.tip').textContent = '上下文使用率 · 正在讀取容量';
  ctxPop = document.createElement('div');
  ctxPop.className = 'pop ctx-pop';
  document.body.appendChild(ctxPop);
  host.addEventListener('click', async (e) => {
    e.stopPropagation();
    if (ctxPop.classList.contains('open')) {
      ctxPop.classList.remove('open');
      return;
    }
    await refreshContext();
    paintContextPopover();
    const rect = host.getBoundingClientRect();
    ctxPop.classList.add('open');
    ctxPop.style.left = `${Math.max(8, Math.min(rect.left - 80, window.innerWidth - (ctxPop.offsetWidth || 280) - 8))}px`;
    ctxPop.style.bottom = `${window.innerHeight - rect.top + 8}px`;
    ctxPop.style.top = 'auto';
  });
  document.addEventListener('click', () => ctxPop.classList.remove('open'));
  bus.on('thread:switched', () => {
    contextUsed = 0;
    contextMax = 0;
    contextEstimated = true;
    ensure('ring');
    host.querySelector('.pct').textContent = '0%';
    host.title = '上下文使用率 · 正在讀取容量';
    host.setAttribute('aria-label', host.title);
    host.querySelector('.tip').textContent = '上下文使用率 · 正在讀取容量';
    refreshContext();
  });
  bus.on('inference_progress', (progress) => {
    if (
      !progress ||
      (progress.sessionId && progress.sessionId !== state.sessionId)
    )
      return;
    if (Number(progress.contextWindowTokens) > 0)
      contextMax = Number(progress.contextWindowTokens);
    const reported = Number(progress.promptTokens) || 0;
    const observed = reported || Number(progress.promptEstimatedTokens) ||
      Number(progress.prefillTotalTokens) || 0;
    if (observed > 0) {
      contextUsed = observed;
      contextEstimated = reported <= 0;
    }
    paintContext();
  });
  bus.on('context_compaction', (data) => {
    if (!data || data.sessionId !== state.sessionId) return;
    if (data.status === 'completed') {
      contextUsed = Number(data.afterTokens) || contextUsed;
      contextEstimated = true;
      paintContext();
    }
  });

  // 記憶體用量 pill（git 旁，用戶回饋）：本地模型記憶體管理的第一眼訊號。
  // pill 常駐（fresh 模式也在），15s 輪詢＋卸載/載入後立即刷新。
  const memPill = $('#memPill');
  const GB = 1024 * 1024 * 1024;
  let memPop = null;
  let lastMem = null; // pill 與 popover 共用同一次抓取（同源同拍）
  const fmtGB = (b) => (b / GB).toFixed(1);
  async function refreshMem() {
    if (!memPill) return;
    let m = null;
    try {
      m = await daemon.memStats();
    } catch {
      memPill.hidden = true;
      return;
    }
    if (!m || !m.totalBytes) {
      memPill.hidden = true;
      return;
    }
    lastMem = m;
    memPill.hidden = false;
    const used = (m.modelBytes + m.otherBytes) / m.totalBytes;
    const pct = Math.max(0, Math.min(100, used * 100));
    // 百分比圓環（用戶回饋：不要 emoji icon）＋ N/64G 小字
    const R = 5.2;
    const C = 2 * Math.PI * R;
    memPill.innerHTML =
      `<svg viewBox="0 0 13 13" width="13" height="13" aria-hidden="true">` +
      `<circle cx="6.5" cy="6.5" r="${R}" fill="none" stroke="rgba(255,255,255,.14)" stroke-width="2"></circle>` +
      `<circle cx="6.5" cy="6.5" r="${R}" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"` +
      ` stroke-dasharray="${C}" stroke-dashoffset="${C - (pct / 100) * C}" transform="rotate(-90 6.5 6.5)"></circle>` +
      `</svg>` +
      `<span>${fmtGB(m.modelBytes + m.otherBytes)}/${Math.round(m.totalBytes / GB)}G</span>`;
    memPill.classList.toggle('hot', used >= 0.85);
    if (memPop && !memPop.hidden) paintMemPop(m);
  }
  function paintMemPop(m) {
    if (!memPop) return;
    const modelPct = (m.modelBytes / m.totalBytes) * 100;
    const otherPct = (m.otherBytes / m.totalBytes) * 100;
    memPop.innerHTML =
      '<div class="hd">記憶體用量</div>' +
      `<div class="mem-row"><span class="sw model"></span>模型 ${fmtGB(m.modelBytes)} GB</div>` +
      `<div class="mem-row"><span class="sw other"></span>其他 ${fmtGB(m.otherBytes)} GB</div>` +
      `<div class="mem-row dim">共 ${Math.round(m.totalBytes / GB)} GB</div>` +
      `<div class="mem-bar"><span class="model" style="width:${modelPct}%"></span><span class="other" style="width:${otherPct}%"></span></div>`;
  }
  if (memPill) {
    memPop = document.createElement('div');
    memPop.className = 'pop mem-pop';
    document.body.appendChild(memPop);
    memPill.addEventListener('click', async (e) => {
      e.stopPropagation();
      if (memPop.classList.contains('open')) {
        memPop.classList.remove('open');
        return;
      }
      // 同一次抓取同時更新 pill 與明細，不再各走各的時間軸
      await refreshMem();
      if (!lastMem) return;
      paintMemPop(lastMem);
      const r = memPill.getBoundingClientRect();
      memPop.classList.add('open'); // 先開才量得到實寬
      const pw = memPop.offsetWidth || 250;
      const left = Math.min(
        Math.max(8, r.left - 40),
        window.innerWidth - pw - 8,
      );
      memPop.style.left = left + 'px';
      memPop.style.bottom = window.innerHeight - r.top + 8 + 'px';
      memPop.style.top = 'auto';
    });
    document.addEventListener('click', () => memPop.classList.remove('open'));
    refreshMem();
    setInterval(refreshMem, 10000);
    bus.on('model:changed', refreshMem);
  }
  refreshMemHook = refreshMem;

  // checkoutRow 的累計用量標示（master 旁，用戶回饋）：per-turn usage 累加
  let threadTokens = 0;
  const usagePill = $('#usagePill');
  const paintUsagePill = () => {
    if (!usagePill) return;
    usagePill.hidden = threadTokens <= 0;
    usagePill.textContent = `累計處理 ${fmtK(threadTokens)} tokens`;
    usagePill.title = `本次開啟後累計處理 ${threadTokens.toLocaleString()} tokens，包含各次模型請求的重複輸入；不是當下上下文大小或帳單費用，快取折扣需依供應商紀錄`;
  };
  bus.on('thread:switched', () => {
    threadTokens = 0;
    paintUsagePill();
  });

  bus.on('usage', (u) => {
    if (!u) return;
    const total =
      Number(
        u.totalTokens ?? (u.promptTokens || 0) + (u.completionTokens || 0),
      ) || 0;
    if (total > 0) {
      threadTokens += total;
      paintUsagePill();
    }
    const contextPromptTokens = Object.prototype.hasOwnProperty.call(
      u,
      'contextPromptTokens',
    )
      ? Number(u.contextPromptTokens)
      : Number(u.promptTokens);
    if (contextPromptTokens > 0) {
      contextUsed = contextPromptTokens;
      contextEstimated = false;
    }
    const max = Number(u.maxTokens) || 0;
    if (max > 0) {
      contextMax = max;
      paintContext();
    } else {
      paintContext();
    }
  });
  refreshContext();
}

export function initComposer() {
  initInput();
  initAttachments();
  initPills();
  const modelPill = $('#pillModelName');
  if (modelPill)
    modelPill.textContent = state.model
      ? selectedModelName(state.model)
      : '請選擇模型';
  initPermissionCards();
  initUserInputCards();
  initCtxMeter();
}
