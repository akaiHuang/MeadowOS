// Reference artwork is sampled ONCE into a glyph field. The visible canvas
// draws only ASCII text, never the reference bitmap or a video frame.
const SOURCES = {
  welcome: { crop: [28, 28, 842, 855], light: false },
  files: { crop: null, light: false },
  preview: { crop: [32, 80, 856, 810], light: false },
  local: { crop: null, light: false, file: 'local-dark-v2' },
  login: { crop: null, light: false, file: 'login-bonsai-v1' },
};
const CHARS = '.,:;+*o%@#';
const hash = (x, y) => ((x * 73 + y * 139 + x * y * 17) % 997) / 997;
const clamp = (n, a, b) => Math.max(a, Math.min(b, n));

export function loginGrowthState(ms, reduced = false) {
  const t = reduced ? 14 : ((ms / 1000) % 18 + 18) % 18;
  return { phase: t < 2 ? 'drop' : t < 3 ? 'splash' : t < 8 ? 'grow' : 'hold',
    growth: clamp((t - 3) / 5, 0, 1), drop: clamp(t / 2, 0, 1),
    splash: clamp(t - 2, 0, 1) };
}

export function createAsciiScene(canvas, { scene = 'welcome', theme = 'dark' } = {}) {
  const spec = SOURCES[scene] || SOURCES.welcome;
  const ctx = canvas.getContext('2d', { alpha: false });
  let active = false, paused = false, destroyed = false, raf = 0;
  let previous = 0, elapsed = scene === 'login' ? 11000 : 0, frame = 0, points = [];
  let width = 1, height = 1, cell = 1, left = 0, top = 0;
  const cols = scene === 'login' ? 120 : 132, rows = cols;
  let light = theme === 'light';
  const background = () => scene === 'login' ? (light ? '#e5edb4' : '#202020') : (light ? '#f4f0e7' : '#0f0f0f');
  const palette = (kind) => scene === 'login'
    ? (light ? { pink: '#d43182', green: '#68752d', neutral: '#414b31' } : { pink: '#ea92b8', green: '#a5cc6c', neutral: '#e3e3cc' })[kind]
    : light
    ? ({ pink: '#dd3183', green: '#879c1b', neutral: '#232522' }[kind])
    : ({ pink: '#ec8eb5', green: '#95cc65', neutral: '#dddccd' }[kind]);
  canvas.dataset.theme = light ? 'light' : 'dark';
  canvas.dataset.status = 'loading';

  function sample(img) {
    if (scene === 'login') {
      // Match the approved web field: max-pool original pixels so fine strokes survive.
      const buffer = document.createElement('canvas');
      buffer.width = img.naturalWidth; buffer.height = img.naturalHeight;
      const bc = buffer.getContext('2d', { willReadFrequently: true });
      bc.drawImage(img, 0, 0);
      const pixels = bc.getImageData(0, 0, buffer.width, buffer.height).data;
      for (let y = 0; y < rows; y++) for (let x = 0; x < cols; x++) {
        let peak = 0, r = 0, g = 0, b = 0;
        for (let sy = Math.floor(y * buffer.height / rows); sy < Math.floor((y + 1) * buffer.height / rows); sy++)
          for (let sx = Math.floor(x * buffer.width / cols); sx < Math.floor((x + 1) * buffer.width / cols); sx++) {
            const i = (sy * buffer.width + sx) * 4;
            const value = Math.max(pixels[i], pixels[i + 1], pixels[i + 2]) * pixels[i + 3] / 255;
            if (value > peak) { peak = value; r = pixels[i]; g = pixels[i + 1]; b = pixels[i + 2]; }
          }
        const strength = Math.max(r, g, b) / 255;
        if (strength <= .23) continue;
        const green = g > r * 1.08 && g > b * 1.15;
        points.push({ x, y, strength, green, seed: hash(x, y),
          kind: r > g * 1.2 && b > g ? 'pink' : green ? 'green' : 'neutral',
          char: '.:;+*o%@#'[Math.min(8, Math.floor(strength * 9))] });
      }
      buffer.width = buffer.height = 0;
      return;
    }
    const crop = spec.crop || [0, 0, img.width, img.height];
    // Crops are measured against the approved 1488 x 1056 UI drafts.
    const scale = spec.crop ? img.width / 1488 : 1;
    const [sx, sy, sw, sh] = crop.map((v) => v * scale);
    const buffer = document.createElement('canvas');
    buffer.width = cols * 3;
    buffer.height = rows * 3;
    const bc = buffer.getContext('2d', { willReadFrequently: true });
    bc.fillStyle = spec.light ? '#f4f0e7' : '#0f0f0f';
    bc.fillRect(0, 0, buffer.width, buffer.height);
    const fitted = Math.min(buffer.width / sw, buffer.height / sh);
    const dw = sw * fitted, dh = sh * fitted;
    bc.drawImage(img, sx, sy, sw, sh, (buffer.width - dw) / 2,
      (buffer.height - dh) / 2, dw, dh);
    const pixels = bc.getImageData(0, 0, buffer.width, buffer.height).data;
    for (let y = 0; y < rows; y++) {
      for (let x = 0; x < cols; x++) {
        let strongest = 0, color = [0, 0, 0];
        for (let dy = 0; dy < 3; dy++) for (let dx = 0; dx < 3; dx++) {
          const i = ((y * 3 + dy) * buffer.width + x * 3 + dx) * 4;
          const r = pixels[i], g = pixels[i + 1], b = pixels[i + 2];
          const strength = spec.light
            ? Math.max(244 - r, 240 - g, 231 - b) / 244
            : (Math.max(r, g, b) - 15) / 240;
          if (strength > strongest) { strongest = strength; color = [r, g, b]; }
        }
        if (strongest < (spec.light ? 0.19 : 0.16)) continue;
        const [r, g, b] = color;
        const pink = r > g * 1.2 && b > g * 1.05;
        const green = g > b * 1.15 && g > r * 0.85;
        points.push({ x, y, strength: strongest, kind: pink ? 'pink' : green ? 'green' : 'neutral', green,
          seed: hash(x, y), char: CHARS[Math.floor(clamp(strongest, 0, .99) * CHARS.length)] });
      }
    }
    // The source buffer is not retained and never used by the animation loop.
    buffer.width = buffer.height = 0;
  }

  function glyph(char, x, y, color, opacity = 1) {
    ctx.fillStyle = color;
    ctx.globalAlpha = opacity;
    ctx.fillText(char, left + x * cell, top + y * cell);
  }

  function draw() {
    if (destroyed || !ctx) return;
    ctx.globalAlpha = 1;
    ctx.fillStyle = background();
    ctx.fillRect(0, 0, width, height);
    ctx.font = `500 ${cell * 1.25}px Menlo, Consolas, monospace`;
    ctx.textBaseline = 'top';
    const t = elapsed / 1000;
    const growth = loginGrowthState(elapsed, paused);
    if (scene === 'login') {
      canvas.dataset.phase = growth.phase;
      // Edge-to-edge quiet texture; this is actual character drawing, not a bitmap.
      ctx.fillStyle = palette('green'); ctx.globalAlpha = .055;
      ctx.font = '10px Menlo, monospace';
      for (let y = 0; y < height; y += 22) for (let x = 0; x < width; x += 19)
        ctx.fillText('.,:+'[Math.floor(hash(x, y) * 4)], x, y);
      ctx.font = `600 ${cell * 1.15}px Menlo, Consolas, monospace`;
    }
    for (const p of points) {
      let x = p.x, y = p.y, char = p.char;
      const nx = x / cols, ny = y / rows;
      let opacity = clamp(.62 + p.strength * .55, .62, 1);
      if (scene === 'login') {
        const soil = 94;
        // Keep the pot fixed. Reveal branching structure from soil upwards;
        // leaves follow the same rising growth front, not a global image fade.
        if (y < soil && (growth.growth === 0 || y < soil - growth.growth * 90)) continue;
        opacity = light ? .7 : .9;
      } else if ((scene === 'welcome' && ny > .59 || scene === 'local' && ny > .76) && p.green) {
        // Discrete glyph-grid wind: blades change slant as the wave passes.
        const wind = Math.sin(t * 1.3 + nx * 9 + ny * 4);
        x += Math.round(wind * (1.2 + (1 - ny) * 2));
        char = wind > .35 ? '/' : wind < -.35 ? '\\' : '|';
        if (p.seed > .68) char = wind > 0 ? 'v' : 'Y';
      } else if (scene === 'files' && nx > .36 && nx < .75) {
        const flow = Math.floor(t * 6 + p.seed * 20);
        char = '.:;+<>*'[flow % 7];
        x += Math.round(Math.sin(t * 2 + p.seed * 6) * 2);
        opacity *= .6 + .4 * Math.sin(t * 2 - nx * 10) ** 2;
      } else if (scene === 'preview' && p.green && ny < .73) {
        const scan = (t * .14) % 1;
        opacity *= paused || ny < scan ? 1 : .65;
        if (p.seed > .5) char = '.,:;+=#'[Math.floor(t * 3 + p.seed * 30) % 7];
      } else if (p.seed > .96 && p.strength < .65) {
        char = '.:+*'[Math.floor(t * 1.5 + p.seed * 10) % 4];
      }
      glyph(char, x, y, palette(p.kind), opacity);
    }
    if (scene === 'login' && growth.phase === 'drop') {
      // The enlarged background bleeds outside the panel; keep the water cue
      // in viewport coordinates so it remains visible at every window size.
      ctx.font = '600 22px Menlo, monospace';
      ctx.fillStyle = palette('pink'); ctx.globalAlpha = .85;
      const waterX = width * .62, waterY = height * (.06 + growth.drop ** 2 * .80);
      ctx.fillText('o', waterX, waterY);
      ctx.globalAlpha = .6;
      ctx.fillText('.', waterX, waterY - 20);
    } else if (scene === 'login' && growth.phase === 'splash') {
      ctx.font = '600 20px Menlo, monospace';
      ctx.fillStyle = palette('pink'); ctx.globalAlpha = 1 - growth.splash * .7;
      for (let i = 0; i < 9; i++) {
        const a = i / 8 * Math.PI;
        ctx.fillText(i % 2 ? '.' : '+', width * .62 + Math.cos(a) * (8 + growth.splash * 65),
          height * .86 - Math.sin(a) * Math.sin(growth.splash * Math.PI) * 45);
      }
    } else if (scene === 'files') {
      for (let i = 0; i < 38; i++) {
        const u = (t * .12 + i / 38) % 1;
        glyph('>:+*'[i % 4], cols * (.37 + u * .35), rows * .48 + Math.sin(u * 5 + i) * 9,
          palette(i % 7 === 0 ? 'pink' : 'green'), Math.sin(u * Math.PI));
      }
    } else if (scene === 'local') {
      // All activity stays inside the laptop screen, not in an external cloud.
      for (let i = 0; i < 12; i++) {
        const u = (t * .1 + i / 12) % 1;
        glyph(i % 3 ? '.' : '+', cols * (.29 + u * .40), rows * (.34 + (i % 3) * .07),
          palette(i % 5 ? 'green' : 'pink'), Math.sin(u * Math.PI) * .8);
      }
    }
    ctx.globalAlpha = 1;
    canvas.dataset.frame = String(++frame);
  }

  function resize() {
    if (destroyed) return;
    const bounds = canvas.getBoundingClientRect();
    if (!bounds.width || !bounds.height) return;
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    canvas.width = Math.round(bounds.width * dpr);
    canvas.height = Math.round(bounds.height * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    width = bounds.width; height = bounds.height;
    cell = Math.min(width / cols, height / rows);
    left = (width - cols * cell) / 2;
    top = (height - rows * cell) / 2;
    if (scene === 'login') {
      cell = Math.min(width * 1.16, height * .94) / cols * 2.5;
      left = (width - cols * cell) / 2 + width * .06;
      top = (height - rows * cell) / 2;
      canvas.dataset.artScale = '2.5';
    }
    draw();
  }

  function tick(now) {
    raf = 0;
    if (destroyed || !active || paused || document.hidden || !canvas.isConnected) return;
    if (now - previous >= 1000 / 15) {
      elapsed += previous ? Math.min(now - previous, 100) : 0;
      previous = now;
      draw();
    }
    raf = requestAnimationFrame(tick);
  }
  function sync() {
    cancelAnimationFrame(raf); raf = 0; previous = 0;
    if (!destroyed && active && !paused && !document.hidden && points.length)
      raf = requestAnimationFrame(tick);
  }
  const observer = new ResizeObserver(resize);
  observer.observe(canvas);
  document.addEventListener('visibilitychange', sync);
  const img = new Image();
  const ready = new Promise((resolve) => {
    img.onload = () => {
      if (!destroyed) {
        sample(img); canvas.dataset.status = 'ready'; resize(); sync();
      }
      resolve();
    };
    img.onerror = () => {
      if (!destroyed) { canvas.dataset.status = 'error'; canvas.setAttribute('aria-label', 'ASCII 插圖載入失敗，仍可繼續介紹'); }
      resolve();
    };
  });
  img.src = new URL(`../assets/ascii-onboarding/${spec.file || (scene in SOURCES ? scene : 'welcome')}.png`, import.meta.url).href;
  return {
    ready,
    setTheme(value) {
      light = value === 'light';
      canvas.dataset.theme = light ? 'light' : 'dark';
      if (active) draw();
    },
    setActive(value) { active = value; if (active) resize(); sync(); },
    setPaused(value) { paused = value; if (active) draw(); sync(); },
    destroy() {
      destroyed = true; active = false; cancelAnimationFrame(raf);
      observer.disconnect(); document.removeEventListener('visibilitychange', sync);
      points = []; canvas.width = canvas.height = 0;
    },
  };
}
