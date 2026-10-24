// Ephemeral native chat preedit only. Committed text stays on the RFB path.
// This local adapter never submits a chat, changes a draft or receives Agent keys.
export function createNativePreedit(input, container, changed) {
    const endpoint = 'http://127.0.0.1:60531';
    const clientId = crypto.randomUUID();
    let state = null, composing = false, disposed = false, owned = false;
    let acquiring = false, sequence = 0, wanted = null, sending = false;
    let leaseAt = 0, reading = false, refreshing = null;
    const api = async (path, body) => {
        const response = await fetch(endpoint + path, {
            method: body ? 'POST' : 'GET', mode: 'cors', cache: 'no-store',
            headers: { 'X-Meadow-IME': '1', 'X-Meadow-IME-Client': clientId,
                ...(body ? { 'Content-Type': 'application/json' } : {}) },
            ...(body ? { body: JSON.stringify(body) } : {}),
        });
        if (!response.ok) throw new Error('native preedit unavailable');
        return response.json();
    };
    const snapshot = () => ({ text: Array.from(input.value).slice(0, 256).join(''),
        cursor: Math.min(256, Array.from(input.value.slice(0, input.selectionStart ?? input.value.length)).length),
        active: composing });
    async function pump() {
        if (sending || (disposed && wanted?.active !== false) || !state?.active) return;
        sending = true;
        const epoch = state.epoch;
        try {
            if (!owned || Date.now() - leaseAt > 2000) {
                acquiring = true; changed();
                const lease = await api('/ime/lease', { clientId, epoch });
                if (state?.epoch !== epoch) return;
                owned = lease.owned === true;
                leaseAt = Date.now();
                if (!owned) { wanted = null; return; }
            }
            // Disconnect may race an in-flight write. Drain only the final
            // inactive presentation after disposal, never another text update.
            while (wanted && (!disposed || wanted.active === false) && state?.epoch === epoch) {
                const payload = wanted;
                wanted = null;
                await api('/ime/preedit', { epoch, sequence: ++sequence, ...payload });
            }
        } catch {
            owned = false;
            state = null;
            wanted = null;
        } finally {
            sending = acquiring = false;
            changed();
            if (wanted && (!disposed || wanted.active === false)) pump();
        }
    }
    async function refresh() {
        if (reading || disposed || document.hidden || document.activeElement !== input) return;
        reading = true;
        try {
            const next = await api('/ime/state');
            if (disposed) return;
            if (state?.epoch !== next.epoch) owned = false;
            state = next;
            if (composing && state.active) { wanted = snapshot(); pump(); }
        } catch { state = null; owned = false; }
        finally { reading = false; changed(); }
    }
    function update() {
        if (!composing || disposed) return;
        wanted = snapshot();
        if (state?.active) pump();
        else refresh();
    }
    function end() {
        composing = false;
        if (state?.active && (owned || acquiring)) {
            wanted = { text: '', cursor: 0, active: false };
            pump();
        }
        changed();
    }
    function geometry() {
        if (!state?.active || !state.caret) return null;
        const canvas = container.querySelector('canvas');
        if (!canvas || !canvas.width || !canvas.height) return null;
        const bounds = canvas.getBoundingClientRect();
        const { x, y, width, height } = state.caret;
        if (![x, y, width, height].every(Number.isFinite)) return null;
        return { x: bounds.left + x * bounds.width / canvas.width,
            y: bounds.top + y * bounds.height / canvas.height,
            height: Math.max(1, height * bounds.height / canvas.height) };
    }
    input.addEventListener('focus', refresh);
    input.addEventListener('blur', end);
    window.addEventListener('blur', end);
    refreshing = setInterval(() => {
        if (!disposed) refresh();
    }, 750);
    refreshing.unref?.();
    return {
        get presenting() { return composing && state?.active && (owned || acquiring); },
        geometry,
        begin() { composing = true; update(); },
        update, end,
        dispose() {
            end();
            disposed = true;
            clearInterval(refreshing);
            input.removeEventListener('focus', refresh);
            input.removeEventListener('blur', end);
            window.removeEventListener('blur', end);
        },
    };
}
