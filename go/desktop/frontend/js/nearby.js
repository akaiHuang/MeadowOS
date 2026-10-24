// Session-owned map and location presentation shared by Desktop and phone.
import { bus, daemon, state } from './core.js?v=raven-execution-20261006a';
import { createPlaceView, disposePlaceView, normalizePlaceCollection } from './nearby_presentation.js?v=nearby-google-link-20260930a';

const text = (value, limit = 500) => typeof value === 'string' ? value.trim().slice(0, limit) : '';
const node = (tag, className, value) => {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (value != null) element.textContent = value;
  return element;
};
const button = (label) => { const element = node('button', '', label); element.type = 'button'; return element; };
const terminalLabels = {
  granted: '已提供概略位置', manual: '已提供搜尋地區', denied: '未提供裝置位置',
  unavailable: '無法取得裝置位置', cancelled: '已略過位置分享',
};

export function approximateLocation(position) {
  const c = position?.coords;
  if (typeof c?.latitude !== 'number' || !Number.isFinite(c.latitude) || Math.abs(c.latitude) > 90 ||
      typeof c.longitude !== 'number' || !Number.isFinite(c.longitude) || Math.abs(c.longitude) > 180) return null;
  const result = { status: 'granted', latitude: Number(c.latitude.toFixed(3)), longitude: Number(c.longitude.toFixed(3)) };
  if (typeof c.accuracy === 'number' && Number.isFinite(c.accuracy) && c.accuracy >= 0) result.accuracyMeters = Math.max(100, Math.ceil(c.accuracy));
  return result;
}

export function initNearby({ host, place }) {
  let epoch = 0, placeRevision = 0, locationRevision = 0;
  let policy = null;
  const views = new Map();
  const requests = new Map();
  const resolved = new Set();
  const active = (sid, token) => sid === state.sessionId && token === epoch;

  function reset() {
    epoch++; placeRevision++; locationRevision++; policy = null;
    for (const view of views.values()) { disposePlaceView(view.node); view.node.remove(); }
    views.clear();
    for (const entry of requests.values()) { entry.stale = true; entry.node.remove(); }
    requests.clear(); resolved.clear();
  }
  function showPlaces(records, replace = true) {
    const seen = new Set();
    for (const raw of Array.isArray(records) ? records.slice(0, 100) : []) {
      const collection = normalizePlaceCollection(raw);
      if (!collection) continue;
      const key = `${collection.runId}\0${collection.callId}`;
      if (!collection.runId || !collection.callId || seen.has(key)) continue;
      seen.add(key);
      const signature = JSON.stringify(collection);
      if (views.get(key)?.signature === signature) continue;
      const old = views.get(key);
      if (old) { disposePlaceView(old.node); old.node.remove(); }
      const sid = state.sessionId, token = epoch;
      const item = createPlaceView(collection, {
        sessionId: sid,
        loadMapPolicy: () => {
          if (!active(sid, token)) return null;
          policy ||= daemon.mapConfig(sid).catch(() => null);
          return policy.then(result => active(sid, token) ? result : null);
        },
      });
      if (!item) continue;
      item.dataset.placeRun = collection.runId;
      item.dataset.placeCall = collection.callId;
      views.set(key, { node: item, signature });
      place(item, [collection]);
    }
    if (replace) for (const [key, view] of views) {
      if (!seen.has(key)) { disposePlaceView(view.node); view.node.remove(); views.delete(key); }
    }
  }
  function resolve(data) {
    if (!data || data.sessionId !== state.sessionId) return;
    const id = text(data.requestId, 160);
    if (!id) return;
    resolved.add(id);
    const entry = requests.get(id);
    if (!entry || entry.runId !== data.runId) return;
    entry.terminal = true;
    entry.node.classList.add('is-resolved');
    entry.title.textContent = '搜尋位置';
    entry.controls.hidden = true;
    entry.message.textContent = terminalLabels[data.status] || '位置請求已結束';
    entry.node.querySelector('.meadow-location-help')?.remove();
  }
  function showRequest(data, automatic = false) {
    if (!data || data.sessionId !== state.sessionId) return;
    const sid = data.sessionId, runId = text(data.runId, 160), id = text(data.requestId, 160), token = epoch;
    if (!id || !runId || resolved.has(id)) return;
    if (state.runId && runId !== state.runId) return;
    if (requests.has(id)) {
      if (automatic) void requests.get(id).locate();
      return;
    }
    const card = node('section', 'meadow-location-request');
    card.dataset.requestId = id; card.dataset.sessionId = sid; card.dataset.runId = runId;
    card.setAttribute('aria-label', '搜尋位置');
    const title = node('strong', '', automatic ? '正在取得目前位置' : '搜尋位置');
    card.append(title);
    if (text(data.reason)) card.append(node('p', 'meadow-location-reason', text(data.reason)));
    const help = node('p', 'meadow-location-help', automatic
      ? '若系統詢問，請選擇是否允許定位。只會提供概略位置。'
      : '此搜尋仍在等待位置。可以使用目前位置，或輸入地區。');
    card.append(help);
    const controls = node('div', 'meadow-location-controls');
    controls.hidden = automatic;
    const locate = button('使用目前位置');
    const manual = node('form', 'meadow-location-manual');
    const label = node('label', '', '或輸入地區、地址');
    const input = node('input');
    input.type = 'text'; input.maxLength = 300; input.placeholder = '例如：台北市大安區'; input.autocomplete = 'off';
    label.append(input);
    const submit = button('使用此地區'); submit.type = 'submit';
    manual.append(label, submit);
    const skip = button('略過');
    const message = node('p', 'meadow-location-status'); message.setAttribute('role', 'status');
    controls.append(locate, manual, skip);
    card.append(controls, message);
    const entry = { node: card, title, controls, message, runId, terminal: false, busy: false, stale: false, attempted: false };
    requests.set(id, entry);
    const stillCurrent = () => !entry.stale && !entry.terminal && active(sid, token) && (!state.runId || state.runId === runId);
    const setBusy = busy => {
      entry.busy = busy;
      controls.querySelectorAll('button, input').forEach(control => { control.disabled = busy; });
      if (entry.attempted) locate.disabled = true;
    };
    async function reply(result) {
      if (!stillCurrent()) return;
      setBusy(true);
      try {
        await daemon.replyLocation(sid, id, result);
        if (active(sid, token) && !entry.stale && !entry.terminal) resolve({ sessionId: sid, runId, requestId: id, status: result.status });
      } catch (error) {
        if (!stillCurrent()) return;
        if (error?.status === 404 || error?.status === 409) {
          resolve({ sessionId: sid, runId, requestId: id, status: 'cancelled' });
        } else {
          message.textContent = '位置回覆未送出，請選擇輸入地區或略過。';
          title.textContent = '搜尋位置';
          controls.hidden = false;
          setBusy(false);
        }
      }
    }
    async function requestDeviceLocation() {
      if (!stillCurrent() || entry.busy || entry.attempted) return;
      setBusy(true);
      controls.hidden = true;
      title.textContent = '正在取得目前位置';
      message.textContent = '正在取得概略位置…';
      // SSE can replay a finished request on reconnect. Confirm the exact
      // request is still pending before triggering the device permission UI.
      try {
        const pending = await daemon.locationRequests(sid);
        if (!stillCurrent()) return;
        if (!pending?.requests?.some(request => request.sessionId === sid && request.runId === runId && request.requestId === id)) {
          resolve({ sessionId: sid, runId, requestId: id, status: 'cancelled' });
          return;
        }
      } catch {
        if (!stillCurrent()) return;
        title.textContent = '搜尋位置';
        message.textContent = '暫時無法確認位置請求，請重試或輸入地區。';
        controls.hidden = false;
        setBusy(false);
        return;
      }
      entry.attempted = true;
      if (!navigator.geolocation || !window.isSecureContext) { void reply({ status: 'unavailable' }); return; }
      // The model's foreground location request goes straight to browser/OS
      // consent. Restoring history alone never calls this function.
      try {
        navigator.geolocation.getCurrentPosition(position => {
          if (!stillCurrent()) return;
          void reply(approximateLocation(position) || { status: 'unavailable' });
        }, error => {
          if (!stillCurrent()) return;
          void reply({ status: error?.code === 1 ? 'denied' : 'unavailable' });
        }, { enableHighAccuracy: false, timeout: 15000, maximumAge: 60000 });
      } catch { void reply({ status: 'unavailable' }); }
    }
    entry.locate = requestDeviceLocation;
    locate.addEventListener('click', () => { void requestDeviceLocation(); });
    manual.addEventListener('submit', event => {
      event.preventDefault();
      if (!stillCurrent() || entry.busy) return;
      const area = input.value.trim();
      if (!area) { message.textContent = '請先輸入地區或地址。'; input.focus(); return; }
      void reply({ status: 'manual', area });
    });
    skip.addEventListener('click', () => { if (!entry.busy) void reply({ status: 'cancelled' }); });
    host()?.append(card);
    if (automatic) void requestDeviceLocation();
  }
  async function hydrate() {
    const sid = state.sessionId, token = epoch, pRevision = placeRevision, lRevision = locationRevision;
    if (!sid) return;
    const [places, pending] = await Promise.allSettled([daemon.placeList(sid), daemon.locationRequests(sid)]);
    if (!active(sid, token)) return;
    if (places.status === 'fulfilled' && pRevision === placeRevision) showPlaces(places.value?.places);
    if (pending.status === 'fulfilled' && lRevision === locationRevision) {
      for (const request of pending.value?.requests || []) showRequest(request);
    }
  }
  bus.on('place_results', data => {
    if (data?.sessionId !== state.sessionId) return;
    placeRevision++;
    showPlaces(data.places);
  });
  bus.on('location_request', data => { if (data?.sessionId === state.sessionId) { locationRevision++; showRequest(data, true); } });
  bus.on('location_resolved', data => { if (data?.sessionId === state.sessionId) { locationRevision++; resolve(data); } });
  bus.on('turn_completed', data => {
    if (data?.sessionId && data.sessionId !== state.sessionId) return;
    for (const [requestId, entry] of requests) {
      if (!entry.terminal && (!data?.runId || data.runId === entry.runId)) resolve({ sessionId: state.sessionId, runId: entry.runId, requestId, status: 'cancelled' });
    }
  });
  return { reset, hydrate };
}
