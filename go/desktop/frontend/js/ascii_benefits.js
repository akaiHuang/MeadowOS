// Character-only illustrations reconstructed against the four approved comps.
// Stable silhouettes; only leaves, clock hands, ripples and speech animate.
const TAU = Math.PI * 2;

export function createBenefitsScene(
  canvas,
  { scene = 'create', theme = 'dark' } = {},
) {
  const ctx = canvas.getContext('2d');
  let active = false,
    paused = false,
    destroyed = false,
    raf = 0;
  let elapsed = 0,
    previous = null,
    frame = 0,
    width = 0,
    height = 0;
  let light = theme === 'light',
    reduced = false,
    amplitude = 1;
  let colors;
  canvas.dataset.status = ctx ? 'ready' : 'unavailable';

  function glyph(text, x, y, kind = 'ink', opacity = 1, size = 13, angle = 0) {
    ctx.save();
    ctx.translate(x, y);
    if (angle) ctx.rotate(angle);
    ctx.fillStyle = colors[kind];
    ctx.globalAlpha = opacity;
    ctx.font = '600 ' + size + 'px Menlo, Consolas, monospace';
    ctx.fillText(text, 0, 0);
    ctx.restore();
  }

  // Character spacing carries across segments, so curved contours don't
  // devolve into either disconnected noise or solid vector-like lines.
  function trace(
    points,
    kind = 'ink',
    opacity = 1,
    gap = 11,
    mark = null,
    size = 13,
  ) {
    let distance = 0;
    for (let i = 1; i < points.length; i++) {
      const [x, y] = points[i - 1],
        [xx, yy] = points[i];
      const dx = xx - x,
        dy = yy - y,
        length = Math.hypot(dx, dy);
      if (!length) continue;
      for (; distance < length; distance += gap) {
        const angle = Math.atan2(dy, dx);
        const c =
          mark ||
          (Math.abs(dy) < Math.abs(dx) * 0.48
            ? '-'
            : Math.abs(dx) < Math.abs(dy) * 0.48
              ? '|'
              : dx * dy > 0
                ? '\\'
                : '/');
        glyph(
          c,
          x + (dx * distance) / length,
          y + (dy * distance) / length,
          kind,
          opacity,
          size,
          mark === '-' ? angle : 0,
        );
      }
      distance -= length;
    }
  }
  const curve = (fn, count = 200) =>
    Array.from({ length: count + 1 }, (_, i) => fn(i / count));
  const ellipse = (
    x,
    y,
    rx,
    ry,
    kind = 'ink',
    opacity = 1,
    from = 0,
    to = TAU,
    gap = 11,
  ) =>
    trace(
      curve((u) => [
        x + Math.cos(from + u * (to - from)) * rx,
        y + Math.sin(from + u * (to - from)) * ry,
      ]),
      kind,
      opacity,
      gap,
      '-',
      14,
    );

  function bubble(x, y, w, h, kind = 'ink', tailRight = false) {
    const r = 15,
      tail = x + (tailRight ? w * 0.68 : w * 0.22);
    trace(
      [
        [x + r + 8, y],
        [x + w - r - 5, y],
      ],
      kind,
      1,
      11,
      '-',
      15,
    );
    trace(
      [
        [x, y + r + 10],
        [x, y + h - r - 4],
      ],
      kind,
      1,
      14,
      '|',
      15,
    );
    trace(
      [
        [x + w, y + r + 10],
        [x + w, y + h - r - 4],
      ],
      kind,
      1,
      14,
      '|',
      15,
    );
    [
      [x + r, y],
      [x, y + r],
      [x + w - r, y],
      [x + w, y + r],
      [x, y + h - r],
      [x + r, y + h],
      [x + w, y + h - r],
      [x + w - r, y + h],
    ].forEach(([xx, yy]) => glyph('+', xx, yy, kind, 1, 16));
    trace(
      [
        [x + r + 8, y + h],
        [tail, y + h],
        [tail + 19, y + h + 34],
        [tail + 34, y + h],
        [x + w - r - 4, y + h],
      ],
      kind,
      1,
      11,
    );
  }

  function create(t) {
    const left = [
      '           @@@',
      '         @@@@@@@',
      '       @@@@@@@@@',
      '     @@@@@@@@@@@@@',
      '    @@@@@@@@@@@@@@@',
      '  @@@@@@@@@@@@@@@@@@@',
      ' @@@@@@@  o  @@@@@@@@@',
      '@@@@@ o     o @@@@@@@@',
      ' @@@ o          @@@ o',
      '  @@@               o',
      '    @@',
    ];
    const right = [
      '       @@@',
      '    @@@@@@@',
      '   @@@@@@@@@',
      ' @@@@@@@@@@@@@',
      '@@@@@@@@@@@@@@@',
      '@@@@  o   @@@@@@',
      ' o         @@@@@@',
      '             @@@',
      '          o',
    ];
    const leaves = (rows, ox, oy, phase) =>
      rows.forEach((row, y) =>
        [...row].forEach((c, x) => {
          if (c === ' ') return;
          const sway = Math.sin(t * 1.35 + phase + y * 0.12) * 6 * amplitude;
          glyph(
            c,
            ox + x * 10 + sway,
            oy + y * 13 + Math.cos(t + x * 0.08) * amplitude * 1.5,
            'green',
            0.92 + Math.sin(t * 1.6 + x * 0.2 + y * 0.35) * 0.08,
            c === '@' ? 13.5 : 13,
          );
        }),
      );
    leaves(left, 30, 12, 0);
    leaves(right, 226, 128, 1.1);

    // Three sinuous strands, the negative spaces, and both side branches
    // reproduce the distinctive tree instead of two filled oval canopies.
    for (const offset of [0, 13, 27]) {
      trace(
        [
          [150 + offset, 100],
          [137 + offset, 138],
          [151 + offset, 166],
          [167 + offset, 193],
          [158 + offset, 220],
          [147 + offset, 241],
          [166 + offset, 285],
          [182 + offset, 319],
          [169 + offset, 339],
        ],
        'ink',
        1,
        12,
      );
    }
    trace(
      [
        [76, 121],
        [96, 143],
        [124, 155],
        [145, 164],
      ],
      'ink',
      1,
      10,
    );
    trace(
      [
        [87, 121],
        [106, 142],
        [136, 156],
      ],
      'ink',
      1,
      10,
    );
    trace(
      [
        [283, 203],
        [258, 232],
        [223, 253],
        [203, 268],
      ],
      'ink',
      1,
      11,
    );
    trace(
      [
        [296, 202],
        [272, 236],
        [238, 259],
        [211, 284],
      ],
      'ink',
      1,
      11,
    );
    glyph('*', 280, 52, 'rose', 1, 22);
    trace(
      [
        [271, 63],
        [262, 80],
      ],
      'ink',
      0.9,
      9,
    );
    glyph('o', 290, 80, 'green', 0.95, 13);

    // Tall screen and keyboard/trackpad proportions follow 01-create.png.
    trace(
      [
        [78, 268],
        [135, 268],
      ],
      'ink',
      1,
      10,
    );
    trace(
      [
        [215, 268],
        [283, 268],
        [283, 377],
        [77, 377],
        [77, 268],
      ],
      'ink',
      1,
      10,
    );
    trace(
      [
        [87, 278],
        [140, 278],
      ],
      'ink',
      0.95,
      10,
    );
    trace(
      [
        [220, 278],
        [274, 278],
        [274, 367],
        [88, 367],
        [88, 278],
      ],
      'ink',
      0.95,
      10,
    );
    [
      [142, 330],
      [126, 343],
      [115, 355],
      [154, 353],
      [213, 346],
      [239, 357],
      [259, 357],
      [227, 333],
    ].forEach(([x, y]) => glyph('^', x, y, 'ink', 1, 13));
    [
      [166, 348],
      [183, 359],
      [136, 361],
    ].forEach(([x, y]) => glyph('o', x, y, 'green', 1, 12));
    trace(
      [
        [78, 383],
        [45, 431],
        [315, 431],
        [282, 383],
      ],
      'ink',
      1,
      11,
    );
    trace(
      [
        [47, 440],
        [313, 440],
      ],
      'ink',
      0.95,
      10,
    );
    trace(
      [
        [87, 388],
        [275, 388],
      ],
      'ink',
      1,
      11,
    );
    trace(
      [
        [76, 400],
        [288, 400],
      ],
      'ink',
      1,
      11,
    );
    trace(
      [
        [118, 421],
        [125, 409],
        [228, 409],
        [235, 421],
      ],
      'ink',
      1,
      10,
    );
  }

  function time(t) {
    ellipse(266, 123, 116, 116);
    ellipse(266, 123, 105, 105, 'ink', 1);
    for (let i = 0; i < 8; i++) {
      const a = (i * TAU) / 8;
      glyph(
        i % 2 ? '.' : i % 4 ? '|' : '-',
        266 + Math.cos(a) * 91,
        123 + Math.sin(a) * 91,
        'ink',
        1,
        15,
      );
    }
    const angle = -0.5 * Math.PI + t * 0.24 * amplitude;
    trace(
      [
        [266, 123],
        [266 + Math.cos(angle) * 67, 123 + Math.sin(angle) * 67],
      ],
      'green',
      1,
      13,
      '@',
      15,
    );
    const hour = 0.52 + t * 0.02 * amplitude;
    trace(
      [
        [266, 123],
        [266 + Math.cos(hour) * 72, 123 + Math.sin(hour) * 72],
      ],
      'green',
      1,
      13,
      '@',
      15,
    );
    glyph('(@)', 266, 123, 'green', 1, 15);
    for (let i = 0; i < 3; i++) {
      const x = 10 + i * 51,
        y = 184 + i * 51,
        w = 70,
        h = 85;
      // Foreground note hides the preceding note's edge, as in the comp.
      ctx.clearRect(x - 6, y - 6, w + 12, h + 12);
      trace(
        [
          [x + 10, y],
          [x + w - 5, y],
        ],
        'ink',
        1,
        10,
      );
      trace(
        [
          [x, y + 10],
          [x, y + h - 5],
        ],
        'ink',
        1,
        11,
      );
      trace(
        [
          [x + w, y + 10],
          [x + w, y + h - 5],
        ],
        'ink',
        1,
        11,
      );
      trace(
        [
          [x + 10, y + h],
          [x + w - 5, y + h],
        ],
        'ink',
        1,
        10,
      );
      [
        [x, y],
        [x + w, y],
        [x, y + h],
        [x + w, y + h],
      ].forEach(([xx, yy]) => glyph('+', xx, yy, 'ink', 1, 15));
      for (let j = 0; j < 3; j++)
        trace(
          [
            [x + 12, y + 23 + j * 16],
            [x + 33, y + 23 + j * 16],
          ],
          'ink',
          1,
          10,
        );
      glyph(
        '✓',
        x + 51,
        y + 28,
        'green',
        0.8 + 0.2 * Math.sin(t * 1.5 - i) ** 2,
        28,
      );
    }
    for (let i = 0; i < 18; i++) {
      const progress = (i / 18 + t * 0.09 * amplitude) % 1;
      glyph(
        i % 4 === 0 ? '@' : '.',
        92 + progress * 118 + Math.sin(i * 2) * 15,
        170 + progress * 112 + Math.cos(i) * 22,
        'green',
        0.5 + 0.45 * Math.sin(progress * Math.PI),
        i % 4 === 0 ? 15 : 18,
      );
    }
    glyph('*', 185, 244, 'rose', 1, 23);
  }

  function calm(t) {
    // Organic thought bubble, not a rectangular speech box.
    const contour = [
      [66, 25],
      [187, 25],
      [219, 36],
      [238, 70],
      [252, 113],
      [252, 152],
      [239, 191],
      [215, 216],
      [185, 220],
      [113, 220],
      [98, 241],
      [68, 263],
      [84, 220],
      [58, 216],
      [33, 193],
      [17, 151],
      [17, 101],
      [33, 56],
      [53, 32],
      [66, 25],
    ];
    trace(contour, 'ink', 1, 11, null, 14);
    const wave = Math.sin(t * 0.9) * 3 * amplitude;
    const knot = [];
    let start = [196, 148];
    const bends = [
      [
        [160, 118],
        [172, 68],
        [205, 79],
      ],
      [
        [234, 102],
        [187, 139],
        [148, 163],
      ],
      [
        [83, 218],
        [64, 191],
        [90, 145],
      ],
      [
        [105, 117],
        [137, 121],
        [150, 108],
      ],
      [
        [187, 63],
        [129, 36],
        [111, 45],
      ],
      [
        [86, 78],
        [86, 123],
        [108, 145],
      ],
      [
        [126, 181],
        [176, 208],
        [181, 171],
      ],
      [
        [178, 132],
        [145, 99],
        [115, 85],
      ],
      [
        [79, 65],
        [36, 102],
        [51, 138],
      ],
      [
        [65, 167],
        [123, 157],
        [136, 135],
      ],
      [
        [154, 100],
        [145, 63],
        [166, 57],
      ],
      [
        [199, 51],
        [213, 83],
        [211, 107],
      ],
      [
        [206, 135],
        [225, 161],
        [243, 151],
      ],
    ];
    for (const [a, b, end] of bends) {
      const from = start;
      knot.push(
        ...curve((u) => {
          const v = 1 - u;
          return [
            v * v * v * from[0] +
              3 * v * v * u * a[0] +
              3 * v * u * u * b[0] +
              u * u * u * end[0],
            v * v * v * from[1] +
              3 * v * v * u * a[1] +
              3 * v * u * u * b[1] +
              u * u * u * end[1] +
              wave,
          ];
        }, 40),
      );
      start = end;
    }
    trace(knot, 'green', 1, 10, '-', 14);
    for (let i = 0; i < 9; i++) {
      const [x, y] = knot[Math.floor((i / 9) * knot.length)];
      glyph('@', x, y, 'green', 1, 15);
    }
    trace(
      curve((u) => [194 + u * 116, 130 + Math.sin(u * TAU) * 16 + u * 14]),
      'green',
      1,
      10,
      '-',
      14,
    );
    // Two pointed leaves unfold from the exiting strand.
    trace(
      [
        [299, 141],
        [300, 115],
        [312, 87],
        [338, 78],
        [357, 75],
        [353, 103],
        [339, 128],
        [317, 140],
        [299, 141],
      ],
      'green',
      1,
      9,
      '-',
      14,
    );
    trace(
      [
        [307, 151],
        [332, 148],
        [357, 161],
        [373, 190],
        [346, 188],
        [325, 175],
        [307, 151],
      ],
      'green',
      1,
      9,
      '-',
      14,
    );
    [
      [316, 124],
      [333, 106],
      [336, 163],
      [350, 174],
    ].forEach(([x, y]) => glyph('o', x, y, 'green', 1, 13));
    glyph('*', 274, 69, 'rose', 1, 23);
    for (let i = 0; i < 5; i++) {
      const breath = Math.sin(t * 1.65 - i * 0.72);
      ellipse(
        190,
        279 + i * 14,
        52 + i * 33 + breath * 5 * amplitude,
        11 + i * 8,
        'ink',
        0.75 + breath * 0.2,
        -0.16,
        Math.PI + 0.16,
        12,
      );
    }
  }

  function talk(t) {
    const drift = Math.sin(t * 1.2) * 5 * amplitude;
    bubble(22, 28 + drift, 198, 128, 'ink');
    bubble(211, 207 - drift, 174, 120, 'green', true);
    const cycle = t % 7;
    const count =
      cycle < 2 || cycle > 3.6
        ? 6
        : Math.max(1, Math.floor((cycle - 2) / 0.27) + 1);
    glyph(
      'Hello!'.slice(0, reduced || paused ? 6 : count),
      121,
      92 + drift,
      'ink',
      1,
      30,
    );
    glyph('你好', 298, 267 - drift, 'green', 1, 33);
    for (let i = 0; i < 6; i++)
      glyph(
        '.',
        102 + i * 17,
        192 + i * 10,
        'ink',
        0.6 + 0.4 * Math.sin(t * 1.5 - i * 0.7) ** 2,
        19,
      );
    glyph('*', 145, 218, 'rose', 1, 24);
  }

  function draw() {
    if (!ctx || destroyed || !active || !width || !height) return;
    ctx.clearRect(0, 0, width, height);
    colors = light
      ? { ink: '#30332c', green: '#617817', rose: '#bf3874' }
      : { ink: '#f2f0e7', green: '#70efb9', rose: '#f1acbf' };
    amplitude = reduced ? 0.12 : 1;
    const sceneHeight =
      { create: 455, time: 385, calm: 390, talk: 370 }[scene] || 455;
    const scale = Math.min(width / 410, height / sceneHeight);
    ctx.save();
    ctx.translate(
      (width - 400 * scale) / 2,
      (height - sceneHeight * scale) / 2,
    );
    ctx.scale(scale, scale);
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    (({ create, time, calm, talk })[scene] || create)(elapsed / 1000);
    ctx.restore();
    canvas.dataset.frame = String(++frame);
  }
  function resize() {
    if (!ctx || destroyed || !active) return;
    const bounds = canvas.getBoundingClientRect();
    if (!bounds.width || !bounds.height) return;
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    width = bounds.width;
    height = bounds.height;
    canvas.width = Math.round(width * dpr);
    canvas.height = Math.round(height * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    draw();
  }
  function tick(now) {
    raf = 0;
    if (
      destroyed ||
      !active ||
      paused ||
      document.hidden ||
      !canvas.isConnected
    )
      return;
    if (previous === null) previous = now;
    if (now - previous >= 1000 / 15) {
      elapsed += Math.min(now - previous, 150);
      previous = now;
      draw();
    }
    raf = requestAnimationFrame(tick);
  }
  function sync() {
    cancelAnimationFrame(raf);
    raf = 0;
    previous = null;
    if (ctx && !destroyed && active && !paused && !document.hidden)
      raf = requestAnimationFrame(tick);
    canvas.dataset.animated = String(
      active && !paused && !document.hidden && !destroyed,
    );
  }
  const observer = new ResizeObserver(resize);
  observer.observe(canvas);
  document.addEventListener('visibilitychange', sync);
  return {
    ready: Promise.resolve(),
    setTheme(value) {
      light = value === 'light';
      canvas.dataset.theme = light ? 'light' : 'dark';
      draw();
    },
    setActive(value) {
      active = value;
      if (active) resize();
      sync();
    },
    setReducedMotion(value) {
      reduced = value;
      canvas.dataset.reducedMotion = String(value);
      draw();
    },
    setPaused(value) {
      paused = value;
      canvas.dataset.paused = String(value);
      sync();
    },
    destroy() {
      destroyed = true;
      active = false;
      sync();
      observer.disconnect();
      document.removeEventListener('visibilitychange', sync);
      canvas.width = canvas.height = 0;
    },
  };
}
