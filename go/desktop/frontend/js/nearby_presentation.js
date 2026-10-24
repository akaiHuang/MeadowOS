// Structured provider observations only. No place is inferred from model prose.
import { safeSourceURL } from './search_presentation.js?v=search-thinking-20260928a';

const views = new WeakMap();
let nextViewId = 0;
const boundedText = (value, limit = 500) => typeof value === 'string' ? value.trim().slice(0, limit) : '';
const coordinate = (value, bound) => typeof value === 'number' && Number.isFinite(value) && Math.abs(value) <= bound;
const node = (tag, className, text) => {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (text != null) element.textContent = text;
  return element;
};
const button = (label, className = '') => {
  const element = node('button', className, label);
  element.type = 'button';
  return element;
};
// Local, unmodified Lucide assets; provider text never becomes SVG or a URL.
const icon = (name) => {
  const element = node('span', 'meadow-place-icon');
  element.setAttribute('aria-hidden', 'true');
  element.style.setProperty('--nearby-icon', `url("${new URL(`../icons/nearby-${name}.svg`, import.meta.url).href}")`);
  return element;
};
const iconButton = (label, name, className) => {
  const element = button('', className);
  element.setAttribute('aria-label', label);
  element.title = label;
  element.append(icon(name));
  return element;
};

export function normalizePlaceCollection(value) {
  if (!value || typeof value !== 'object') return null;
  const places = [];
  const seen = new Set();
  for (const place of Array.isArray(value.places) ? value.places.slice(0, 30) : []) {
    if (!place || !coordinate(place.latitude, 90) || !coordinate(place.longitude, 180)) continue;
    const name = boundedText(place.name, 300);
    const id = boundedText(place.id, 200);
    const key = id || `${place.latitude},${place.longitude},${name}`;
    if (!name || seen.has(key)) continue;
    seen.add(key);
    places.push({
      id: key, name, latitude: place.latitude, longitude: place.longitude,
      address: boundedText(place.address), category: boundedText(place.category, 160),
      url: safeSourceURL(place.url),
      distanceMeters: typeof place.distanceMeters === 'number' && Number.isFinite(place.distanceMeters) && place.distanceMeters >= 0 ? place.distanceMeters : null,
      rating: typeof place.rating === 'number' && Number.isFinite(place.rating) && place.rating >= 0 && place.rating <= 5 ? place.rating : null,
      ratingCount: Number.isSafeInteger(place.ratingCount) && place.ratingCount >= 0 ? place.ratingCount : null,
    });
  }
  const center = value.center;
  return {
    query: boundedText(value.query, 300),
    runId: boundedText(value.runId, 160), callId: boundedText(value.callId, 160),
    provider: boundedText(value.provider, 160), attribution: boundedText(value.attribution, 800),
    observedAt: boundedText(value.observedAt, 100), recordedAt: boundedText(value.recordedAt, 100),
    center: center && coordinate(center.latitude, 90) && coordinate(center.longitude, 180) ? {
      latitude: center.latitude, longitude: center.longitude,
      label: boundedText(center.label, 300), source: boundedText(center.source, 100),
    } : null,
    places,
  };
}

const CATEGORY_LABELS = Object.freeze({
  'amenity:cafe': '咖啡廳', 'amenity:restaurant': '餐廳', 'amenity:fast_food': '輕食與速食',
  'amenity:library': '圖書館', 'amenity:school': '學校', 'amenity:kindergarten': '幼兒園',
  'amenity:childcare': '兒童照護', 'amenity:community_centre': '社區中心',
  'amenity:arts_centre': '藝文中心', 'amenity:theatre': '劇場', 'amenity:cinema': '電影院',
  'amenity:hospital': '醫院', 'amenity:clinic': '診所', 'amenity:pharmacy': '藥局',
  'leisure:park': '公園', 'leisure:playground': '遊戲場', 'leisure:sports_centre': '運動中心',
  'tourism:museum': '博物館', 'tourism:gallery': '藝廊', 'tourism:attraction': '景點',
  'shop:books': '書店', 'shop:supermarket': '超市', 'shop:convenience': '便利商店',
});
export function placeCategoryLabel(category) { return CATEGORY_LABELS[category] || ''; }

export function placeDistanceLabel(meters) {
  if (typeof meters !== 'number' || !Number.isFinite(meters) || meters < 0) return '';
  return `直線距離 ${meters < 1000 ? `${Math.round(meters / 10) * 10} 公尺` : `${(meters / 1000).toFixed(1)} 公里`}`;
}

// Maps URLs need no API key. Only the selected public place is sent on click;
// never include the user's location, collection center or a provider credential.
export function googleMapsPlaceURL(place) {
  if (!place || !coordinate(place.latitude, 90) || !coordinate(place.longitude, 180)) return null;
  const name = boundedText(place.name, 300);
  const address = boundedText(place.address);
  const coordinates = `${place.latitude},${place.longitude}`;
  const url = new URL('https://www.google.com/maps/search/');
  url.searchParams.set('api', '1');
  url.searchParams.set('query', name && address ? `${name}, ${address}` : coordinates);
  // Maps URLs have a 2,048-character limit; preserve the exact point when a
  // long encoded name/address would exceed it, rather than truncate an address.
  if (url.href.length > 2048) url.searchParams.set('query', coordinates);
  return url.href;
}

// The policy comes from this authenticated session, never a model/tool URL.
export function safeMapPolicy(value, sessionId, protocol = globalThis.location?.protocol) {
  const revision = value?.tileRevision || '';
  if (revision && !/^[a-f0-9]{16}$/.test(revision)) return null;
  const expected = `/session/${encodeURIComponent(sessionId)}/map-tiles/{z}/{x}/{y}${revision ? `?v=${revision}` : ''}`;
  if (!sessionId || !value || value.tileURL !== expected) return null;
  const mapbox = value.provider === 'mapbox';
  if (mapbox && (!revision || value.tileSize !== 512 || value.zoomOffset !== -1)) return null;
  const browserURL = !mapbox && value.browserTileURL === 'https://tile.openstreetmap.org/{z}/{x}/{y}.png' && ['http:', 'https:'].includes(protocol) ? value.browserTileURL : null;
  return { tileURL: browserURL || expected, provider: mapbox ? 'mapbox' : 'other', tileSize: mapbox ? 512 : 256, zoomOffset: mapbox ? -1 : 0, minZoom: mapbox ? 1 : 0, attributionURL: safeSourceURL(value.attributionURL), maxZoom: Math.min(18, Math.max(1, Number.isInteger(value.maxZoom) ? value.maxZoom : 18)), attribution: boundedText(value.attribution, 800) };
}

export function disposePlaceView(root) {
  const view = views.get(root);
  if (!view) return;
  view.disposed = true;
  view.observer?.disconnect();
  view.resizeObserver?.disconnect();
  if (view.layoutFrame) cancelAnimationFrame(view.layoutFrame);
  view.closeOverlay?.(false);
  view.map?.remove();
  views.delete(root);
}

export function createPlaceView(raw, options = {}) {
  const collection = normalizePlaceCollection(raw);
  if (!collection) return null;
  const root = node('section', 'meadow-place-collection');
  root.setAttribute('aria-label', `附近地點：${collection.query || '搜尋結果'}`);
  const head = node('div', 'meadow-place-heading');
  head.append(node('strong', '', collection.query || '附近地點'));
  if (collection.center?.label) head.append(node('span', '', collection.center.label));
  root.append(head);
  if (!collection.places.length) {
    root.append(node('p', 'meadow-place-empty', '這次地點搜尋沒有找到符合的結果。可以換個地點名稱或地區再找。'));
    return root;
  }
  head.classList.add('meadow-place-visually-hidden');
  const view = { disposed: false, map: null, selected: 0, markers: [], observer: null, resizeObserver: null, closeOverlay: null, layoutFrame: 0 };
  const viewId = `meadow-place-${++nextViewId}`;
  views.set(root, view);
  const stage = node('div', 'meadow-place-stage');
  const canvas = node('div', 'meadow-place-map');
  canvas.setAttribute('aria-label', '地點地圖，使用方向鍵移動地圖');
  // Leaflet 1.9 focuses the map on mousedown, restoring only window scroll.
  // Focus it first without scrolling the enclosing conversation; otherwise a
  // tall inline map can move under the pointer between press and release.
  canvas.addEventListener('mousedown', event => {
    if (event.button === 0) canvas.focus({ preventScroll: true });
  }, { capture: true });
  const status = node('p', 'meadow-place-map-status', '載入地圖…');
  status.setAttribute('role', 'status');
  const expand = iconButton('放大地圖', 'maximize', 'meadow-place-expand');
  const track = node('div', 'meadow-place-track');
  track.setAttribute('aria-label', '地點清單，可左右滑動');
  track.setAttribute('role', 'list');
  track.tabIndex = 0;
  let userScrolling = false;
  for (const event of ['pointerdown', 'wheel', 'keydown']) track.addEventListener(event, () => { userScrolling = true; }, { passive: true });
  const cards = [];
  const selects = [];
  function scrollToSelectedCard(behavior = 'auto') {
    if (view.disposed || !cards[view.selected]) return;
    userScrolling = false;
    track.scrollTo({ left: cards[view.selected].offsetLeft - track.offsetLeft - 12, behavior });
  }
  function restoreMapLayout(focusTarget = null) {
    if (view.disposed) return;
    if (view.layoutFrame) cancelAnimationFrame(view.layoutFrame);
    // Moving the stage into/out of a dialog resets native scroll offsets.
    // Re-align after its new width is laid out, preserving the selected place.
    view.layoutFrame = requestAnimationFrame(() => {
      view.layoutFrame = 0;
      if (view.disposed) return;
      view.map?.invalidateSize({ pan: false });
      scrollToSelectedCard();
      panToPlace(view.selected);
      if (focusTarget?.isConnected) focusTarget.focus({ preventScroll: true });
    });
  }
  function mapPadding() {
    // Keep markers in the visible map area above the overlaid card carousel.
    const size = canvas.getBoundingClientRect(), rail = track.getBoundingClientRect();
    const bottom = Math.max(0, size.bottom - rail.top) + 56;
    return { top: 62, bottom: Math.min(bottom, size.height * .52) };
  }
  function panToPlace(index) {
    if (!view.map) return;
    const place = collection.places[index], padding = mapPadding();
    const point = view.map.project([place.latitude, place.longitude]);
    point.y += (padding.bottom - padding.top) / 2;
    view.map.panTo(view.map.unproject(point), { animate: false });
  }
  function select(index, scroll = true, pan = true) {
    if (view.disposed || index < 0 || index >= cards.length) return;
    view.selected = index;
    cards.forEach((card, i) => card.classList.toggle('is-selected', i === index));
    selects.forEach((control, i) => control.setAttribute('aria-pressed', String(i === index)));
    view.markers.forEach((marker, i) => {
      marker.getElement()?.classList.toggle('is-selected', i === index);
      marker.getElement()?.setAttribute('aria-pressed', String(i === index));
      marker.setZIndexOffset(i === index ? 1000 : 0);
      if (i === index) marker.openTooltip(); else marker.closeTooltip();
    });
    if (scroll) {
      scrollToSelectedCard(matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth');
    }
    if (pan) panToPlace(index);
  }
  collection.places.forEach((place, index) => {
    const item = node('article', 'meadow-place-card');
    item.setAttribute('role', 'listitem');
    const pick = button('', 'meadow-place-select');
    pick.setAttribute('aria-label', `在地圖選取 ${index + 1}. ${place.name}`);
    const thumbnail = node('span', 'meadow-place-thumbnail');
    thumbnail.setAttribute('aria-hidden', 'true');
    thumbnail.append(icon(place.category === 'amenity:cafe' ? 'coffee' : 'map-pin'));
    pick.append(thumbnail);
    const title = node('span', 'meadow-place-description');
    title.append(node('strong', '', place.name));
    const categoryLabel = placeCategoryLabel(place.category);
    const meta = node('span', 'meadow-place-category');
    if (place.rating != null) {
      const rating = node('span', 'meadow-place-rating');
      rating.setAttribute('aria-label', `評分 ${place.rating.toFixed(1)}${place.ratingCount != null ? `，${place.ratingCount} 則評分` : ''}`);
      rating.append(icon('star'), node('span', '', place.rating.toFixed(1)));
      meta.append(rating);
      if (categoryLabel) meta.append(node('span', 'meadow-place-separator', '·'));
    }
    if (categoryLabel) meta.append(node('span', '', categoryLabel));
    if (meta.childNodes.length) title.append(meta);
    const distance = placeDistanceLabel(place.distanceMeters);
    if (distance) title.append(node('span', 'meadow-place-details', distance));
    pick.append(title);
    pick.addEventListener('click', () => select(index));
    item.append(pick);
    // Original address remains accessible without turning the compact
    // carousel into a second full-height list. No invented photos or ratings.
    if (place.address) {
      item.append(node('p', 'meadow-place-address meadow-place-visually-hidden', place.address));
      item.title = place.address;
    }
    const details = node('span', 'meadow-place-visually-hidden', [
      categoryLabel, place.rating != null ? `評分 ${place.rating.toFixed(1)}${place.ratingCount != null ? `，${place.ratingCount} 則評分` : ''}` : '',
      distance, place.address,
    ].filter(Boolean).join('，'));
    details.id = `${viewId}-${index}-details`;
    pick.setAttribute('aria-describedby', details.id);
    item.append(details);
    const mapsURL = googleMapsPlaceURL(place);
    if (mapsURL) {
      const link = node('a', 'meadow-place-source');
      link.setAttribute('aria-label', `在 Google Maps 開啟 ${place.name}`);
      link.title = ['在 Google Maps 開啟', place.name, place.address].filter(Boolean).join(' · ');
      link.append(icon('arrow-up-right'));
      link.href = mapsURL; link.target = '_blank'; link.rel = 'noopener noreferrer';
      item.append(link);
    }
    cards.push(item); selects.push(pick); track.append(item);
  });
  // A swipe chooses the card at the leading edge; map clicks scroll to it.
  let scrollFrame = 0;
  track.addEventListener('scroll', () => {
    if (scrollFrame || view.disposed || !userScrolling) return;
    scrollFrame = requestAnimationFrame(() => {
      scrollFrame = 0;
      if (view.disposed || !userScrolling) return;
      const left = track.getBoundingClientRect().left + 12;
      let index = 0, distance = Infinity;
      cards.forEach((card, i) => {
        const delta = Math.abs(card.getBoundingClientRect().left - left);
        if (delta < distance) { distance = delta; index = i; }
      });
      if (track.scrollWidth > track.clientWidth && track.scrollLeft >= track.scrollWidth - track.clientWidth - 2) index = cards.length - 1;
      if (index !== view.selected) select(index, false);
    });
  });
  const attribution = node('div', 'meadow-place-attribution');
  if (collection.attribution || collection.provider) attribution.append(node('span', '', collection.attribution || collection.provider));
  const mapCredit = node('span', 'meadow-map-credit');
  attribution.append(mapCredit);
  stage.append(canvas, status, expand, track);
  root.append(stage, attribution);
  select(0, false, false);

  expand.addEventListener('click', () => {
    if (view.disposed || view.closeOverlay) return;
    const restoreFocus = expand;
    const dialog = node('dialog', 'meadow-place-dialog');
    dialog.setAttribute('aria-label', collection.query || '附近地點地圖');
    const toolbar = node('div', 'meadow-place-dialog-toolbar');
    toolbar.append(node('strong', '', collection.query || '附近地點'));
    const close = iconButton('關閉地圖', 'x', 'meadow-place-close');
    toolbar.append(close);
    const placeholder = document.createComment('inline map');
    stage.before(placeholder);
    dialog.append(toolbar, stage, attribution);
    document.body.append(dialog);
    expand.hidden = true;
    let closed = false;
    view.closeOverlay = (restore = true) => {
      if (closed) return;
      closed = true;
      placeholder.replaceWith(stage);
      root.append(attribution);
      expand.hidden = false;
      dialog.close(); dialog.remove();
      view.closeOverlay = null;
      restoreMapLayout(restore ? restoreFocus : null);
    };
    dialog.addEventListener('cancel', event => { event.preventDefault(); view.closeOverlay?.(); });
    close.addEventListener('click', () => view.closeOverlay?.());
    dialog.showModal();
    close.focus();
    restoreMapLayout();
  });

  async function mountMap() {
    view.observer?.disconnect();
    if (view.disposed || !root.isConnected) return;
    try {
      const [L, policy] = await Promise.all([
        import('../vendor/leaflet-1.9.4.js'),
        options.loadMapPolicy?.(),
      ]);
      if (view.disposed || !root.isConnected) return;
      const safe = safeMapPolicy(policy, options.sessionId);
      if (safe?.provider !== 'mapbox' && safe?.attribution) {
        const credit = node(safe.attributionURL ? 'a' : 'span', '', safe.attribution);
        if (safe.attributionURL) { credit.href = safe.attributionURL; credit.target = '_blank'; credit.rel = 'noopener noreferrer'; }
        mapCredit.replaceChildren(credit);
      }
      const map = L.map(canvas, { zoomControl: false, attributionControl: false, scrollWheelZoom: false, minZoom: safe?.minZoom || 0, maxZoom: safe?.maxZoom || 18 });
      view.map = map;
      L.control.zoom({ position: 'topright' }).addTo(map);
      if (safe?.provider === 'mapbox') {
        // Provider identity is host-owned. Place collection attribution remains
        // separate and unchanged; model/provider text cannot inject these links.
        const link = (label, href) => {
          const element = node('a', '', label);
          element.href = href; element.target = '_blank'; element.rel = 'noopener noreferrer';
          return element;
        };
        const feedback = link('Improve this map', 'https://apps.mapbox.com/feedback/');
        mapCredit.replaceChildren(link('© Mapbox', 'https://www.mapbox.com/about/maps/'), link('© OpenStreetMap', 'https://www.openstreetmap.org/copyright'), feedback);
        const updateFeedback = () => {
          const center = map.getCenter();
          feedback.href = `https://apps.mapbox.com/feedback/#/${center.lng.toFixed(5)}/${center.lat.toFixed(5)}/${map.getZoom()}`;
        };
        map.on('moveend zoomend', updateFeedback);
        const logo = L.control({ position: 'bottomleft' });
        logo.onAdd = () => {
          const element = link('', 'https://www.mapbox.com/about/maps/');
          element.className = 'meadow-mapbox-logo';
          element.setAttribute('aria-label', 'Mapbox');
          const image = node('img');
          image.src = new URL('../vendor/mapbox-logo.svg', import.meta.url).href;
          image.alt = 'Mapbox'; image.width = 85; image.height = 21;
          element.append(image);
          L.DomEvent.disableClickPropagation(element);
          return element;
        };
        logo.addTo(map);
      }
      const bounds = L.latLngBounds(collection.places.map(place => [place.latitude, place.longitude]));
      if (collection.center) bounds.extend([collection.center.latitude, collection.center.longitude]);
      const padding = mapPadding();
      map.fitBounds(bounds, { paddingTopLeft: [64, padding.top], paddingBottomRight: [64, padding.bottom], maxZoom: 15 });
      collection.places.forEach((place, index) => {
        const label = node('span', 'meadow-place-marker-label');
        const rated = place.rating != null;
        label.append(icon(rated ? 'star' : 'map-pin'), node('span', 'meadow-place-marker-text', rated ? place.rating.toFixed(1) : place.name));
        const width = rated ? 78 : Math.min(144, Math.max(76, 38 + Array.from(place.name).length * 10));
        const marker = L.marker([place.latitude, place.longitude], {
          icon: L.divIcon({ className: 'meadow-place-marker', html: label, iconSize: [width, 40], iconAnchor: [width / 2, 20] }),
          title: `${index + 1}. ${place.name}`, alt: place.name, keyboard: true,
        }).addTo(map);
        marker.bindTooltip(node('span', '', place.name), { direction: 'bottom', offset: [0, 18], className: 'meadow-place-marker-caption', opacity: 1, permanent: true });
        marker.getElement()?.setAttribute('aria-label', `選取 ${index + 1}. ${place.name}`);
        marker.on('click', () => select(index));
        view.markers.push(marker);
      });
      if (collection.center) {
        const label = node('span', '', collection.center.label || '搜尋中心');
        L.circleMarker([collection.center.latitude, collection.center.longitude], { radius: 6, color: '#ffffff', weight: 2, fillColor: '#337eea', fillOpacity: 1 }).addTo(map).bindTooltip(label);
      }
      select(view.selected, false, false);
      if (safe) {
        const tiles = L.tileLayer(safe.tileURL, { tileSize: safe.tileSize, zoomOffset: safe.zoomOffset, minZoom: safe.minZoom, maxZoom: safe.maxZoom, referrerPolicy: 'origin', updateWhenIdle: true, keepBuffer: 1 });
        let loaded = 0;
        tiles.on('tileload', () => { loaded++; status.hidden = true; });
        tiles.on('tileerror', () => { if (!loaded) { status.hidden = false; status.textContent = '底圖暫時無法載入，仍可選取地點與查看來源。'; } });
        tiles.addTo(map);
      } else {
        status.textContent = '底圖目前無法使用，仍可選取地點與查看來源。';
      }
      if (typeof ResizeObserver === 'function') {
        view.resizeObserver = new ResizeObserver(() => { if (!view.disposed) map.invalidateSize({ pan: false }); });
        view.resizeObserver.observe(canvas);
      }
    } catch {
      if (!view.disposed) status.textContent = '地圖暫時無法載入，請使用下方地點清單。';
    }
  }
  queueMicrotask(() => {
    if (view.disposed || !root.isConnected) return;
    if (typeof IntersectionObserver === 'function') {
      view.observer = new IntersectionObserver(entries => { if (entries.some(entry => entry.isIntersecting)) void mountMap(); }, { rootMargin: '80px' });
      view.observer.observe(root);
    } else void mountMap();
  });
  return root;
}
