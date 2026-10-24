/* Capture the browser's native sample rate. ASR resampling happens on the server;
   the original higher-rate recording is retained for voice reference selection. */
class PCM16Capture extends AudioWorkletProcessor {
  constructor() {
    super();
    this.chunkFrames = Math.round(sampleRate * 0.02);
    this.pcm = new Int16Array(this.chunkFrames);
    this.cursor = 0;
    this.energy = 0;
    this.peak = 0;
    this.muted = false;
    this.active = true;
    this.port.onmessage = ({ data }) => {
      if (data.type === 'mute') this.muted = Boolean(data.muted);
      if (data.type === 'stop') this.active = false;
    };
  }
  process(inputs, outputs) {
    // The output is deliberately silent; connecting to destination keeps the
    // processor running without playing the microphone through the speakers.
    for (const output of outputs) for (const channel of output) channel.fill(0);
    if (!this.active) return false;
    const channels = inputs[0];
    if (!channels || !channels.length) return true;
    const frames = channels[0].length;
    for (let i = 0; i < frames; i++) {
      let value = 0;
      if (!this.muted) {
        for (const channel of channels) value += channel[i] || 0;
        value = Math.max(-1, Math.min(1, value / channels.length));
      }
      this.energy += value * value;
      this.peak = Math.max(this.peak, Math.abs(value));
      this.pcm[this.cursor++] = Math.round(value * (value < 0 ? 32768 : 32767));
      if (this.cursor === this.chunkFrames) {
        const pcm = this.pcm.buffer;
        this.port.postMessage({ type: 'audio', pcm, rms: Math.sqrt(this.energy / this.chunkFrames), peak: this.peak }, [pcm]);
        this.pcm = new Int16Array(this.chunkFrames);
        this.cursor = 0;
        this.energy = 0;
        this.peak = 0;
      }
    }
    return true;
  }
}
registerProcessor('pcm16-capture', PCM16Capture);
