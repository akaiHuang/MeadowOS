// Native DOM port of the approved iPhone / status-preview motion.
export const patterns = ['orbit', 'spiral', 'dice', 'sweep', 'zoom'];
export function nextPattern(current, random = Math.random) {
  const candidates = patterns.filter((p) => p !== current);
  return candidates[
    Math.min(candidates.length - 1, Math.floor(random() * candidates.length))
  ];
}
export function dotState(pattern, index, elapsed) {
  const phase = (Math.max(0, elapsed) % 1.8) / 1.8;
  const gx = (index % 3) - 1,
    gy = Math.floor(index / 3) - 1;
  let pulse = 0,
    x = 0,
    y = 0;
  const cyclic = (target, width) => {
    const distance = Math.abs(phase - target);
    return Math.max(0, 1 - Math.min(distance, 1 - distance) / width);
  };
  if (pattern === 'orbit') {
    const position = [0, 1, 2, 5, 8, 7, 6, 3].indexOf(index);
    pulse =
      position < 0
        ? 0.34 + 0.16 * Math.sin(phase * Math.PI * 2)
        : cyclic(position / 8, 0.16);
    if (position >= 0) {
      x = gx * pulse * 0.9;
      y = gy * pulse * 0.9;
    }
  } else if (pattern === 'spiral') {
    pulse = cyclic([0, 1, 2, 5, 8, 7, 6, 3, 4].indexOf(index) / 9, 0.14);
    x = -gx * (1 - pulse) * 0.7;
    y = -gy * (1 - pulse) * 0.7;
  } else if (pattern === 'dice') {
    const faces = [[0, 2, 4, 6, 8], [4], [0, 2, 6, 8]];
    const jump = Math.sin(((phase * 3) % 1) * Math.PI);
    const lit = faces[Math.floor(phase * 3) % 3].includes(index);
    pulse = lit ? 0.78 + 0.22 * jump : 0.08;
    y = lit ? -jump * 2.2 : 0;
  } else if (pattern === 'sweep') {
    const head = phase < 0.5 ? phase * 4 : (1 - phase) * 4;
    pulse = Math.max(0, 1 - Math.abs((index % 3) - head) / 1.15);
    x = (phase < 0.5 ? 1 : -1) * pulse * 1.2;
  } else {
    const outward = (Math.sin(phase * Math.PI * 2 - Math.PI / 2) + 1) / 2;
    pulse = Math.max(
      0.08,
      1 - Math.abs(Math.hypot(gx, gy) / Math.sqrt(2) - outward) * 1.45,
    );
    x = gx * (outward - 0.5) * 1.4;
    y = gy * (outward - 0.5) * 1.4;
  }
  pulse = Math.min(1, Math.max(0, pulse));
  return { opacity: 0.22 + pulse * 0.78, scale: 0.72 + pulse * 0.48, x, y };
}

// One owner per live row. Archived rows own no timer or media listeners.
export function animateNineDots(element) {
  const dots = [...element.children];
  const media = matchMedia('(prefers-reduced-motion: reduce)');
  let pattern = patterns[Math.floor(Math.random() * patterns.length)];
  let timer = 0,
    started = 0,
    disposed = false;
  const paint = (elapsed, moving) => {
    element.dataset.pattern = pattern;
    element.dataset.animated = String(moving);
    dots.forEach((dot, i) => {
      const s = moving
        ? dotState(pattern, i, elapsed)
        : { opacity: i === 4 ? 1 : 0.58, scale: 1, x: 0, y: 0 };
      dot.style.opacity = s.opacity;
      dot.style.transform = `translate(${((i % 3) - 1) * 7 + s.x}px, ${(Math.floor(i / 3) - 1) * 7 + s.y}px) scale(${s.scale})`;
    });
  };
  const sync = () => {
    clearInterval(timer);
    timer = 0;
    const moving = !disposed && !media.matches && !document.hidden;
    started = performance.now();
    paint(0, moving);
    if (moving)
      timer = setInterval(() => {
        let elapsed = (performance.now() - started) / 1000;
        if (elapsed >= 10) {
          pattern = nextPattern(pattern);
          started = performance.now();
          elapsed = 0;
        }
        paint(elapsed, true);
      }, 50);
  };
  media.addEventListener('change', sync);
  document.addEventListener('visibilitychange', sync);
  sync();
  return () => {
    disposed = true;
    clearInterval(timer);
    media.removeEventListener('change', sync);
    document.removeEventListener('visibilitychange', sync);
    paint(0, false);
  };
}
