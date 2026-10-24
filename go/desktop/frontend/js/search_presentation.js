// Search evidence presentation shared by the timeline and Progress. The caller
// owns the active session and authenticated requests; no external URL is fetched
// here, and source text is never interpreted as HTML or model instructions.
const galleryState = new WeakMap();

function text(value, limit = 4096) {
  return typeof value === 'string' ? value.slice(0, limit).trim() : '';
}

export function safeSourceURL(value) {
  if (typeof value !== 'string' || value.length > 4096) return null;
  try {
    const url = new URL(value);
    if (!['http:', 'https:'].includes(url.protocol) || !url.hostname ||
        url.username || url.password) return null;
    return url.href;
  } catch { return null; }
}

// Deduplicate within a tool call. The same page observed in a later call can
// carry different evidence, so group before normalizing session-wide records.
export function normalizeResearchSources(records) {
  const sources = [];
  const seen = new Set();
  for (const value of Array.isArray(records) ? records.slice(0, 200) : []) {
    if (!value || typeof value !== 'object') continue;
    const url = safeSourceURL(value.url);
    if (!url || seen.has(url)) continue;
    seen.add(url);
    const host = new URL(url).hostname.replace(/^www\./, '');
    const excerpt = value.excerpt ?? value.snippet ?? value.text;
    // Links use browser canonicalization, but preview authorization matches the
    // exact received URL. Preserve it through repeated normalization only when
    // it names the same validated source (default ports/Unicode may normalize).
    const receiptURL = safeSourceURL(value.receiptURL) === url
      ? value.receiptURL : value.url;
    sources.push({
      url,
      receiptURL,
      host,
      id: text(value.id, 160),
      title: text(value.title, 1000) || host,
      callId: text(value.callId ?? value.call_id, 160),
      runId: text(value.runId ?? value.run_id, 160),
      tool: text(value.tool, 100),
      excerpt: text(excerpt, 4000),
      contentKind: text(value.contentKind ?? value.content_kind, 100),
      thumbnailURL: text(value.thumbnailURL ?? value.thumbnailUrl ?? value.thumbnail_url),
      fetchedAt: text(value.fetchedAt ?? value.fetched_at, 100),
      observedAt: text(value.observedAt ?? value.observed_at, 100),
      fetchError: text(value.fetchError ?? value.fetch_error, 300),
      evidenceTruncated: value.evidenceTruncated === true || value.evidence_truncated === true,
      inputTruncated: value.inputTruncated === true || value.input_truncated === true,
      snippetTruncated: value.snippetTruncated === true || value.snippet_truncated === true ||
        (typeof excerpt === 'string' && excerpt.length > 4000),
    });
  }
  return sources;
}

export function groupResearchSourcesByCall(records) {
  const groups = new Map();
  for (const source of Array.isArray(records) ? records.slice(0, 200) : []) {
    if (!source || typeof source !== 'object') continue;
    const callId = text(source.callId ?? source.call_id, 160) || 'history';
    const runId = text(source.runId ?? source.run_id, 160);
    const key = `${runId}\u0000${callId}`;
    if (!groups.has(key)) groups.set(key, { callId, runId, records: [] });
    groups.get(key).records.push(source);
  }
  return [...groups.values()].map(({ callId, runId, records: rows }) => ({
    callId,
    runId,
    sources: normalizeResearchSources(rows),
  })).filter(group => group.sources.length);
}

export function sourceEvidenceLabel(source) {
  if (source.fetchError) return '頁面擷取未成功';
  if (source.contentKind === 'snippet') return '搜尋摘要';
  if (source.contentKind === 'coverage') return '頁面段落索引';
  if (['body', 'page', 'html', 'text', 'text/html', 'text/plain', 'pdf', 'application/pdf'].includes(source.contentKind)) {
    return '頁面擷取內容';
  }
  return source.excerpt ? '工具回傳內容' : '來源連結';
}

export function safeThumbnailData(value) {
  return typeof value === 'string' && value.length <= 33000 &&
    /^data:image\/jpeg;base64,[A-Za-z0-9+/]+={0,2}$/.test(value)
    ? value : null;
}

function element(tag, className, value) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (value != null) node.textContent = value;
  return node;
}

function formatObservedTime(value) {
  if (!value) return '';
  const date = new Date(value);
  return Number.isFinite(date.getTime())
    ? date.toLocaleString('zh-TW', { hour12: false }) : '';
}

function appendEvidence(item, source) {
  const observed = formatObservedTime(source.fetchedAt || source.observedAt);
  if (!source.excerpt && !source.fetchError && !source.contentKind && !observed) return;
  const details = element('details', 'meadow-source-evidence');
  details.append(element('summary', '', '查看來源資料'));
  const body = element('div', 'meadow-source-evidence-body');
  const meta = element('div', 'meadow-source-evidence-meta', sourceEvidenceLabel(source));
  if (source.evidenceTruncated || source.inputTruncated || source.snippetTruncated) {
    meta.append(document.createTextNode(' · 部分摘錄'));
  }
  body.append(meta);
  if (source.excerpt) body.append(element('p', 'meadow-source-excerpt', source.excerpt));
  if (source.fetchError) {
    body.append(element('p', 'meadow-source-warning', `擷取狀態：${source.fetchError}`));
  }
  if (observed) {
    body.append(element('span', 'meadow-source-observed', `${source.fetchedAt ? '擷取' : '記錄'}時間 · ${observed}`));
  }
  details.append(body);
  item.append(details);
}

function attachPreviews(gallery, state, cards, loadPreview) {
  if (typeof loadPreview !== 'function') return;
  const paint = async (card, source) => {
    state.observer?.unobserve(card);
    if (!gallery.isConnected || galleryState.get(gallery) !== state) return;
    let data;
    try { data = safeThumbnailData(await loadPreview(source)); }
    catch { return; } // Optional imagery never changes the evidence status.
    if (!data || !card.isConnected || galleryState.get(gallery) !== state) return;
    const cover = card.querySelector('.meadow-source-cover');
    if (!cover) return;
    const image = document.createElement('img');
    image.alt = '';
    image.decoding = 'async';
    image.addEventListener('load', () => {
      if (galleryState.get(gallery) === state) cover.classList.add('has-preview');
    }, { once: true });
    image.addEventListener('error', () => image.remove(), { once: true });
    image.src = data;
    cover.append(image);
  };
  if (typeof IntersectionObserver === 'function') {
    const sources = new Map(cards);
    state.observer = new IntersectionObserver(entries => {
      for (const entry of entries) {
        if (entry.isIntersecting) void paint(entry.target, sources.get(entry.target));
      }
    }, { rootMargin: '40px' });
    for (const [card] of cards) state.observer.observe(card);
  } else {
    // Wait until callers have attached a newly created section to the timeline.
    queueMicrotask(() => cards.forEach(([card, source]) => void paint(card, source)));
  }
}

export function disposeSearchGallery(gallery) {
  galleryState.get(gallery)?.observer?.disconnect();
  galleryState.delete(gallery);
}

export function updateSearchGallery(gallery, records, options = {}) {
  const sources = normalizeResearchSources(records);
  const previousScroll = gallery.querySelector('.meadow-source-track')?.scrollLeft || 0;
  disposeSearchGallery(gallery);
  gallery.replaceChildren();
  gallery.hidden = !sources.length;
  if (!sources.length) return gallery;
  const current = { observer: null };
  galleryState.set(gallery, current);
  gallery.className = 'meadow-source-gallery';
  gallery.setAttribute('aria-label', '實際搜尋來源');
  const header = element('div', 'meadow-source-header');
  header.append(element('span', '', `${options.label || '搜尋來源'} · ${sources.length}`));
  const controls = element('div', 'meadow-source-controls');
  controls.append(element('span', '', '左右滑動'));
  const track = element('div', 'meadow-source-track');
  track.setAttribute('role', 'list');
  track.setAttribute('aria-label', '搜尋來源，可左右滑動');
  track.tabIndex = 0;
  for (const [name, direction, glyph] of [['上一組來源', -1, '‹'], ['下一組來源', 1, '›']]) {
    const button = element('button', '', glyph);
    button.type = 'button';
    button.setAttribute('aria-label', name);
    button.addEventListener('click', () => {
      const step = (track.firstElementChild?.getBoundingClientRect().width || 280) + 12;
      track.scrollBy({ left: direction * step, behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
    });
    controls.append(button);
  }
  header.append(controls);
  const cards = [];
  sources.forEach((source, index) => {
    const item = element('div', 'meadow-source-item');
    item.setAttribute('role', 'listitem');
    const card = element('a', 'meadow-source-card');
    card.href = source.url;
    card.target = '_blank';
    card.rel = 'noopener noreferrer';
    card.setAttribute('aria-label', `${index + 1}. ${source.title}，${source.host}`);
    if (typeof options.openSource === 'function') {
      card.addEventListener('click', event => {
        if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
        event.preventDefault();
        Promise.resolve().then(() => options.openSource(source.url)).catch(() => {
          let failure = item.querySelector('.meadow-source-open-error');
          if (!failure) {
            failure = element('span', 'meadow-source-open-error', '無法開啟來源，請稍後重試。');
            failure.setAttribute('role', 'status');
            item.append(failure);
          }
        });
      });
    }
    const cover = element('div', 'meadow-source-cover');
    cover.setAttribute('aria-hidden', 'true');
    cover.append(element('span', 'meadow-source-monogram', source.host.slice(0, 2).toUpperCase()));
    card.append(cover, element('strong', '', source.title), element('span', 'meadow-source-domain', `${String(index + 1).padStart(2, '0')}  ${source.host}`));
    item.append(card);
    appendEvidence(item, source);
    track.append(item);
    cards.push([card, source]);
  });
  gallery.append(header, track);
  track.scrollLeft = previousScroll;
  attachPreviews(gallery, current, cards, options.loadPreview);
  return gallery;
}

export function createSearchGallery(records, options = {}) {
  if (!normalizeResearchSources(records).length) return null;
  return updateSearchGallery(element('section'), records, options);
}
