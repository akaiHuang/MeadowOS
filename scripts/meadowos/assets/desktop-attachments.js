// Host Files enter only the foreground native chat's private byte mailbox.
// Original File bytes are sent one at a time. No host filesystem path, Agent
// credential, inference call or attachment-count limit belongs in this page.
const ENDPOINT = 'http://127.0.0.1:60531';
const MAX_FILE_BYTES = 20 * 1024 * 1024;
const MIME_BY_EXTENSION = { png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg',
    gif: 'image/gif', webp: 'image/webp' };

export function installDesktopAttachments(rfb, container) {
    rfb._meadowAttachmentsDispose?.();
    let disposed = false, state = null, reading = null, busy = false, hovering = false;
    let lastError = '', total = 0, staged = 0;
    const pending = [], requests = new Set(), clientId = crypto.randomUUID();
    const overlay = document.createElement('div');
    overlay.setAttribute('role', 'status');
    overlay.setAttribute('aria-live', 'polite');
    overlay.style.cssText = 'display:none;position:fixed;pointer-events:none;z-index:19999;box-sizing:border-box;border:2px solid #8edcff;border-radius:16px;background:#102b3820;box-shadow:inset 0 0 0 4px #d9f6ff20;color:#effaff;font:14px system-ui';
    const caption = document.createElement('div');
    caption.style.cssText = 'position:absolute;left:50%;bottom:22px;transform:translateX(-50%);max-width:85%;padding:9px 15px;border-radius:16px;background:#182833ed;white-space:pre-wrap;overflow-wrap:anywhere;text-align:center';
    overlay.appendChild(caption);
    document.body.appendChild(overlay);
    const api = async (path, options = {}) => {
        if (disposed) throw new Error('附件傳送已中斷');
        const controller = new AbortController();
        requests.add(controller);
        const timeout = setTimeout(() => controller.abort(), 20000);
        try {
            const response = await fetch(ENDPOINT + path, { ...options, mode: 'cors', cache: 'no-store',
                signal: controller.signal, headers: { 'X-Meadow-Attachments': '1',
                    'X-Meadow-Attachments-Client': clientId, ...options.headers } });
            const value = await response.json();
            if (!response.ok) {
                const error = new Error(value.message || '圖片未能加入原生對話');
                error.code = value.error; throw error;
            }
            return value;
        } finally { clearTimeout(timeout); requests.delete(controller); }
    };
    function geometry() {
        const canvas = container?.querySelector('canvas');
        if (!state?.active || !state.rect || !canvas?.width || !canvas?.height) return null;
        const bounds = canvas.getBoundingClientRect(), rect = state.rect;
        if (![bounds.left, bounds.top, bounds.width, bounds.height, rect.x, rect.y, rect.width, rect.height].every(Number.isFinite)) return null;
        return { left: bounds.left + rect.x * bounds.width / canvas.width,
            top: bounds.top + rect.y * bounds.height / canvas.height,
            width: rect.width * bounds.width / canvas.width,
            height: rect.height * bounds.height / canvas.height, canvas, bounds };
    }
    function draw() {
        const rect = geometry();
        const visible = !disposed && rect && (hovering || busy || lastError);
        overlay.style.display = visible ? 'block' : 'none';
        if (!visible) return;
        for (const key of ['left', 'top', 'width', 'height']) overlay.style[key] = `${rect[key]}px`;
        caption.textContent = lastError || (busy ? `正在加入圖片 ${staged} / ${total}…` : '放開以加入目前對話');
        overlay.style.borderColor = lastError ? '#ffc2b4' : '#8edcff';
    }
    async function refresh() {
        if (disposed || document.hidden) { state = null; return null; }
        if (reading) return reading;
        reading = api('/attachments/state').then(value => { state = value; return value; })
            .catch(() => { state = null; return null; }).finally(() => { reading = null; draw(); });
        return reading;
    }
    function point(event) {
        const rect = geometry();
        if (!rect || !Number.isFinite(event.clientX) || !Number.isFinite(event.clientY)
                || event.clientX < rect.left || event.clientX > rect.left + rect.width
                || event.clientY < rect.top || event.clientY > rect.top + rect.height) return null;
        return { x: (event.clientX - rect.bounds.left) * rect.canvas.width / rect.bounds.width,
            y: (event.clientY - rect.bounds.top) * rect.canvas.height / rect.bounds.height };
    }
    function filesDrag(event) { return Array.from(event.dataTransfer?.types || []).includes('Files'); }
    function dragover(event) {
        if (!filesDrag(event)) return;
        // Suppress the browser's default file navigation even outside native
        // chat admission. Text, links and normal noVNC pointer input are untouched.
        event.preventDefault(); event.stopPropagation();
        hovering = Boolean(point(event));
        event.dataTransfer.dropEffect = hovering ? 'copy' : 'none';
        if (hovering) lastError = '';
        draw(); refresh();
    }
    function dragleave(event) {
        if (!container.contains?.(event.relatedTarget)) { hovering = false; draw(); }
    }
    async function drop(event) {
        if (!filesDrag(event)) return;
        event.preventDefault(); event.stopPropagation(); hovering = false;
        const files = Array.from(event.dataTransfer.files || []); // File handles only, never eager bytes.
        const location = { clientX: event.clientX, clientY: event.clientY };
        const native = await refresh();
        const position = point(location);
        if (disposed || !files.length || !native?.active || !position) { draw(); return; }
        if (!busy) { total = staged = 0; lastError = ''; }
        total += files.length;
        pending.push({ files, epoch: native.epoch, point: position, collectionId: crypto.randomUUID() });
        pump();
    }
    const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
    async function finalReceipt(eventId) {
        const until = Date.now() + 60000;
        while (!disposed && Date.now() < until) {
            const value = await api('/attachments/receipt?eventId=' + eventId);
            if (value.status === 'staged') return value;
            if (value.status === 'failed') {
                const error = new Error(value.message || '圖片未能加入原生對話');
                error.code = value.error; throw error;
            }
            await sleep(150);
        }
        throw new Error(disposed ? '附件傳送已中斷' : '原生對話尚未確認附件；原始檔已保留');
    }
    async function send(file, collection) {
        const maximum = Math.min(MAX_FILE_BYTES, state?.maxFileBytes || MAX_FILE_BYTES);
        if (!Number.isSafeInteger(file.size) || file.size < 1 || file.size > maximum) throw new Error('每張圖片須介於 1 byte 至 20 MiB');
        const mime = file.type || MIME_BY_EXTENSION[file.name.split('.').at(-1)?.toLowerCase()];
        if (!Object.values(MIME_BY_EXTENSION).includes(mime)) throw new Error('請使用 PNG、JPEG、GIF 或 WebP 圖片');
        const native = await refresh();
        if (!native?.active || native.epoch !== collection.epoch) {
            const error = new Error('原生對話已切換，尚未加入的圖片請重新拖入'); error.code = 'stale_epoch'; throw error;
        }
        const lease = await api('/attachments/lease', { method: 'POST',
            headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientId,
                epoch: collection.epoch, point: collection.point }) });
        if (!lease.owned) { const error = new Error('另一個分頁正在加入圖片'); error.code = 'attachment_not_owned'; throw error; }
        const bytes = await file.arrayBuffer();
        if (disposed) throw new Error('附件傳送已中斷');
        const sha256 = Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes)),
            byte => byte.toString(16).padStart(2, '0')).join('');
        const eventId = crypto.randomUUID().replaceAll('-', '');
        const descriptor = { eventId, epoch: collection.epoch, collectionId: collection.collectionId,
            name: file.name, mime, size: file.size, sha256 };
        await api('/attachments/upload', { method: 'POST', headers: { 'Content-Type': 'application/octet-stream',
            'X-Meadow-Attachments-Meta': encodeURIComponent(JSON.stringify(descriptor)) }, body: bytes });
        // HTTP 202 acknowledges a mailbox, never successful native admission.
        await finalReceipt(eventId);
    }
    async function pump() {
        if (busy || disposed) return;
        busy = true; draw();
        try {
            while (pending.length && !disposed) {
                const collection = pending.shift();
                for (const file of collection.files) {
                    if (disposed) break;
                    try { await send(file, collection); staged++; }
                    catch (error) {
                        lastError = `${file.name}: ${error.message}`; draw();
                        if (['stale_epoch', 'attachment_not_owned', 'outside_chat'].includes(error.code)) break;
                    }
                    draw();
                }
                if (!disposed) await api('/attachments/lease', { method: 'POST',
                    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ clientId,
                        epoch: collection.epoch, release: true }) }).catch(() => {});
            }
        } finally { busy = false; draw(); }
    }
    const hide = () => { if (!busy) lastError = ''; hovering = false; draw(); };
    const visibility = () => { if (document.hidden) hide(); else refresh(); };
    function dispose() {
        if (disposed) return;
        disposed = true; pending.length = 0;
        for (const controller of requests) controller.abort();
        clearInterval(timer);
        container?.removeEventListener('dragover', dragover, true);
        container?.removeEventListener('dragenter', dragover, true);
        container?.removeEventListener('dragleave', dragleave, true);
        container?.removeEventListener('drop', drop, true);
        document.removeEventListener('visibilitychange', visibility);
        window.removeEventListener('blur', hide);
        window.removeEventListener('resize', draw);
        rfb.removeEventListener?.('disconnect', dispose);
        overlay.remove();
    }
    container?.addEventListener('dragover', dragover, true);
    container?.addEventListener('dragenter', dragover, true);
    container?.addEventListener('dragleave', dragleave, true);
    container?.addEventListener('drop', drop, true);
    document.addEventListener('visibilitychange', visibility);
    window.addEventListener('blur', hide);
    window.addEventListener('resize', draw);
    rfb.addEventListener?.('disconnect', dispose, { once: true });
    const timer = setInterval(() => { if (!document.hidden) refresh(); }, 750);
    timer.unref?.();
    rfb._meadowAttachmentsDispose = dispose;
    refresh();
    return { dispose };
}
