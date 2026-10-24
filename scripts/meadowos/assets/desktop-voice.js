// Host microphone/speaker bridge. VNC transports neither. No Agent credentials
// or provider keys enter this page; final transcripts enter the native chat.
const ENDPOINT = 'http://127.0.0.1:60531';

export function pcmWave(chunks, sourceRate) {
    const count = chunks.reduce((n, chunk) => n + chunk.length, 0);
    const input = new Int16Array(count);
    let cursor = 0;
    for (const chunk of chunks) { input.set(chunk, cursor); cursor += chunk.length; }
    const frames = Math.floor(count * 16000 / sourceRate);
    const bytes = new ArrayBuffer(44 + frames * 2);
    const view = new DataView(bytes);
    const str = (offset, value) => [...value].forEach((c, i) => view.setUint8(offset + i, c.charCodeAt(0)));
    str(0, 'RIFF'); view.setUint32(4, 36 + frames * 2, true); str(8, 'WAVE'); str(12, 'fmt ');
    view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
    view.setUint32(24, 16000, true); view.setUint32(28, 32000, true);
    view.setUint16(32, 2, true); view.setUint16(34, 16, true); str(36, 'data'); view.setUint32(40, frames * 2, true);
    for (let i = 0; i < frames; i++) {
        const from = i * sourceRate / 16000, until = (i + 1) * sourceRate / 16000;
        let total = 0, weight = 0;
        for (let j = Math.floor(from); j < Math.ceil(until) && j < count; j++) {
            const share = Math.min(j + 1, until) - Math.max(j, from);
            total += input[j] * share; weight += share;
        }
        view.setInt16(44 + i * 2, Math.round(total / (weight || 1)), true);
    }
    return new Blob([bytes], { type: 'audio/wav' });
}

export function installDesktopVoice(rfb) {
    if (rfb._meadowVoiceDispose) rfb._meadowVoiceDispose();
    let disposed = false, timer, native, epoch, stream, context, capture, source;
    let starting = false, failedEpoch = null, busy = false, speechID = null, audio, audioURL;
    let captureGeneration = 0, owned = false, leaseAt = 0;
    let attemptedSpeech = null, speechAttempts = 0, retrySpeechAt = 0;
    const clientId = crypto.randomUUID();
    const desktop = document.getElementById('noVNC_container');
    let chunks = [], quiet = 0, spoken = 0, frames = 0, energyAt = 0;
    let enabledByGesture = false;
    const button = document.createElement('button');
    button.textContent = '啟用麥克風與語音';
    button.style.cssText = 'display:none;position:fixed;bottom:24px;left:50%;transform:translateX(-50%);z-index:20000;border:1px solid #ffffff35;border-radius:22px;padding:12px 20px;background:#26292ce8;color:#fff;font:14px system-ui;backdrop-filter:blur(16px);cursor:pointer';
    button.setAttribute('aria-label', '啟用 VoiceOS 麥克風與語音播放');
    document.body.appendChild(button);
    const api = async (path, options = {}) => {
        const response = await fetch(ENDPOINT + path, { ...options, mode: 'cors', cache: 'no-store',
            headers: { 'X-Meadow-Voice': '1', 'X-Meadow-Voice-Client': clientId, ...options.headers } });
        if (!response.ok) {
            let message = `語音服務 ${response.status}`;
            try { const value = await response.json(); message = value.error || value.message || message; } catch {}
            const error = new Error(message); error.status = response.status; throw error;
        }
        return response;
    };
    const action = (value, capturedEpoch = epoch) => api('/voice/action', { method: 'POST',
        headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...value, epoch: capturedEpoch }) }).catch(() => {});
    const resetCapture = () => { chunks = []; quiet = spoken = frames = 0; };
    const stopCapture = () => {
        captureGeneration++;
        capture?.port.postMessage({ type: 'stop' }); capture?.disconnect(); source?.disconnect();
        stream?.getTracks().forEach(track => track.stop()); stream = capture = source = null;
        context?.close().catch(() => {}); context = null; resetCapture();
    };
    const stopSpeech = () => {
        if (audio) { audio.onended = null; audio.onerror = null; audio.pause(); audio.src = ''; audio = null; }
        if (audioURL) URL.revokeObjectURL(audioURL); audioURL = null;
    };
    const lease = async (release = false) => {
        const capturedEpoch = epoch;
        const reply = await (await api('/voice/lease', { method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ clientId, epoch: capturedEpoch, release }) })).json();
        if (disposed || document.hidden || epoch !== capturedEpoch) {
            if (!release && reply.owned) api('/voice/lease', { method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ clientId, epoch: capturedEpoch, release: true }) }).catch(() => {});
            return false;
        }
        owned = !release && reply.owned === true; leaseAt = performance.now();
        return owned;
    };
    async function transcribe(parts, sampleRate, capturedEpoch, generation) {
        busy = true;
        try {
            const response = await api('/voice/transcribe', { method: 'POST',
                headers: { 'Content-Type': 'audio/wav', 'X-Meadow-Voice-Epoch': capturedEpoch },
                body: pcmWave(parts, sampleRate) });
            const result = await response.json();
            if (!disposed && !document.hidden && owned && generation === captureGeneration &&
                    epoch === capturedEpoch && native?.microphoneRequested && result.text?.trim()) {
                await action({ type: 'transcript', text: result.text, id: crypto.randomUUID() }, capturedEpoch);
            }
        } catch (error) {
            if (epoch === capturedEpoch && generation === captureGeneration) await action({ type: 'microphone', status: 'error', message: error.message }, capturedEpoch);
        } finally { busy = false; resetCapture(); }
    }
    async function startCapture() {
        if (disposed || starting || stream || document.hidden || !owned || !native?.microphoneRequested || failedEpoch === epoch) return;
        if (!enabledByGesture) { button.style.display = 'block'; return; }
        starting = true;
        const capturedEpoch = epoch;
        const generation = ++captureGeneration;
        let pendingMedia = null;
        try {
            if (!navigator.mediaDevices?.getUserMedia) throw new Error('此瀏覽器無法存取麥克風。');
            button.textContent = '請允許瀏覽器使用麥克風'; button.style.display = 'block';
            const media = pendingMedia = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true }, video: false });
            if (disposed || document.hidden || generation !== captureGeneration || epoch !== capturedEpoch || !native?.microphoneRequested) return;
            if (!(await lease())) { button.textContent = '語音正在另一個分頁使用'; button.style.display = 'block'; return; }
            if (disposed || document.hidden || generation !== captureGeneration || epoch !== capturedEpoch) return;
            stream = media; pendingMedia = null;
            context = new (window.AudioContext || window.webkitAudioContext)();
            await context.resume();
            await context.audioWorklet.addModule(new URL('./voice-capture-worklet.js', import.meta.url));
            if (disposed || document.hidden || generation !== captureGeneration || epoch !== capturedEpoch) return;
            source = context.createMediaStreamSource(stream);
            capture = new AudioWorkletNode(context, 'pcm16-capture');
            source.connect(capture); capture.connect(context.destination);
            capture.port.onmessage = ({ data }) => {
                if (generation !== captureGeneration || document.hidden || !owned || data.type !== 'audio' || busy || audio || !native?.microphoneRequested) return;
                const samples = new Int16Array(data.pcm);
                chunks.push(samples); frames += samples.length;
                const duration = samples.length / context.sampleRate;
                if (data.rms > .014) { spoken += duration; quiet = 0; } else quiet += duration;
                if (performance.now() - energyAt > 200) {
                    energyAt = performance.now(); action({ type: 'energy', energy: Math.min(1, data.rms * 9) });
                }
                if (!spoken && frames > context.sampleRate * .35) { chunks.shift(); frames -= samples.length; }
                if (spoken > .24 && (quiet > 1.0 || frames > context.sampleRate * 25)) {
                    const parts = chunks; const rate = context.sampleRate; resetCapture();
                    transcribe(parts, rate, capturedEpoch, generation);
                }
            };
            stream.getTracks().forEach(track => track.addEventListener('ended', () => {
                if (generation !== captureGeneration) return;
                failedEpoch = capturedEpoch; stopCapture(); action({ type: 'microphone', status: 'stopped' }, capturedEpoch);
            }, { once: true }));
            button.style.display = 'none';
            action({ type: 'microphone', status: 'listening' }, capturedEpoch);
        } catch (error) {
            if (generation !== captureGeneration || epoch !== capturedEpoch) return;
            failedEpoch = capturedEpoch; stopCapture();
            await action({ type: 'microphone', status: error.name === 'NotAllowedError' ? 'denied' : 'error', message: error.message }, capturedEpoch);
            button.textContent = '重試麥克風'; button.style.display = 'block';
        } finally {
            // Permission can resolve after VoiceOS exits, or the lease can
            // fail before this stream becomes the active capture. Never leave
            // that locally held microphone running.
            pendingMedia?.getTracks().forEach(track => track.stop());
            starting = false;
        }
    }
    async function speak(item) {
        const capturedEpoch = epoch;
        if (attemptedSpeech !== item.id) { attemptedSpeech = item.id; speechAttempts = 0; }
        speechAttempts++;
        speechID = item.id; stopSpeech();
        try {
            const response = await api('/voice/synthesize', { method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ text: item.text, id: item.id, epoch: capturedEpoch }) });
            const blob = await response.blob();
            if (disposed || document.hidden || !owned || capturedEpoch !== epoch || speechID !== item.id) return;
            audioURL = URL.createObjectURL(blob); audio = new Audio(audioURL);
            audio.onended = () => { stopSpeech(); resetCapture(); action({ type: 'speaking', status: 'finished', id: item.id }, capturedEpoch); };
            audio.onerror = () => { stopSpeech(); action({ type: 'speaking', status: 'error', id: item.id, message: '語音播放失敗。' }, capturedEpoch); };
            await audio.play();
            action({ type: 'speaking', status: 'started', id: item.id }, capturedEpoch);
        } catch (error) {
            if (capturedEpoch !== epoch || speechID !== item.id) return;
            stopSpeech();
            if (error.status === 429 && speechAttempts < 4 && owned && !document.hidden) {
                speechID = null; retrySpeechAt = performance.now() + 800 * speechAttempts; return;
            }
            if (capturedEpoch === epoch) action({ type: 'speaking', status: 'error', id: item.id, message: error.message }, capturedEpoch);
        }
    }
    button.addEventListener('click', async event => {
        event.stopPropagation(); enabledByGesture = true; failedEpoch = null;
        if (!(await lease())) { button.textContent = '語音正在另一個分頁使用'; return; }
        await startCapture();
    });
    // Opening VoiceOS and pressing its orb are real interactions on the VNC
    // canvas. Reuse that gesture for the host microphone permission flow; the
    // native WebKit surface must not open a second microphone in the VM.
    // Capture still waits for the native active + microphoneRequested state.
    const desktopGesture = event => {
        if (!event.isTrusted || event.target?.tagName !== 'CANVAS') return;
        enabledByGesture = true;
    };
    desktop?.addEventListener('pointerdown', desktopGesture, { capture: true });
    async function poll() {
        try {
            const { native: state } = await (await api('/voice/state')).json();
            if (disposed) return;
            if (epoch !== state?.epoch) {
                if (owned) lease(true).catch(() => {});
                stopCapture(); stopSpeech(); speechID = null; failedEpoch = null; epoch = state?.epoch; owned = false;
            }
            native = state;
            if (!native?.active || document.hidden) { stopCapture(); stopSpeech(); button.style.display = 'none'; }
            else {
                if (enabledByGesture && (!owned || performance.now() - leaseAt > 2500)) await lease();
                if (!owned) {
                    stopCapture(); stopSpeech();
                    if (native.microphoneRequested) button.style.display = 'block';
                    return;
                }
                // Browser permission may remain pending until the user acts.
                // Keep polling native close/pause and renewing the owned lease.
                if (native.microphoneRequested) startCapture();
                else { if (stream) { stopCapture(); action({ type: 'microphone', status: 'stopped' }); } button.style.display = 'none'; }
                if (native.speech?.id && native.speech.id !== speechID && performance.now() >= retrySpeechAt) speak(native.speech);
            }
        } catch {
            owned = false; stopCapture(); stopSpeech();
            // A disconnected voice service used to remove the only visible
            // diagnostic, leaving an inert orb with no way to retry.
            button.textContent = '語音連線中斷，按一下重試';
            button.style.display = native?.active ? 'block' : 'none';
        }
        finally { if (!disposed) timer = setTimeout(poll, native?.active ? 300 : 1800); }
    }
    const visibility = () => {
        if (document.hidden) { stopCapture(); stopSpeech(); if (owned) lease(true).catch(() => {}); owned = false; }
    };
    document.addEventListener('visibilitychange', visibility);
    const dispose = () => { disposed = true; clearTimeout(timer); stopCapture(); stopSpeech();
        document.removeEventListener('visibilitychange', visibility);
        desktop?.removeEventListener('pointerdown', desktopGesture, { capture: true });
        if (owned) lease(true).catch(() => {}); owned = false; button.remove(); };
    rfb._meadowVoiceDispose = dispose;
    rfb.addEventListener('disconnect', dispose, { once: true });
    window.addEventListener('pagehide', dispose, { once: true });
    poll();
}
