(() => {
  'use strict';

  const STORAGE_KEY = 'meadow.voiceos.glaze.v1'; // gitleaks:allow -- Fixed localStorage preference namespace, not a credential.
  const PALETTES = window.MeadowInkMaterial.palettes;
  const PHASES = new Set(['idle', 'listening', 'thinking', 'speaking', 'error']);
  const PHASE_LABELS = {idle: '開始語音對話', listening: '正在聆聽，按一下暫停', thinking: 'Meadow 正在思考', speaking: 'Meadow 正在說話', error: '語音連線需要處理'};
  const orbButton = document.getElementById('voice-orb');
  const palettePanel = document.getElementById('palette-panel');
  const paletteOptions = document.getElementById('palette-options');
  const paletteClose = document.getElementById('palette-close');
  const workActions = document.getElementById('work-actions');
  const status = document.getElementById('voice-status');
  const orb = window.MeadowInkOrb.create(document.getElementById('glaze'));
  const edge = new window.MeadowEdgeLight(document.getElementById('edge-light'));
  const abort = new AbortController();
  const events = {signal: abort.signal};
  let storedPalette;
  try { storedPalette = localStorage.getItem(STORAGE_KEY); } catch (_) { /* file/private profiles may disable storage */ }
  const state = {phase: 'idle', energy: 0, palette: Object.hasOwn(PALETTES, storedPalette) ? storedPalette : 'meadow', muted: false, compact: false};
  let disposed = false;
  let longPress = 0;
  let suppressClick = false;
  let downPoint = null;
  let focusBeforePalette = null;

  function emit(type, details = {}) {
    const message = {type, ...details};
    const handler = window.webkit?.messageHandlers?.meadowVoiceOS;
    if (handler) handler.postMessage(message);
    else if (window.parent !== window) window.parent.postMessage({source: 'meadow-voiceos', ...message}, '*');
    // Standalone previews expose the same request without inventing microphone state.
    window.dispatchEvent(new CustomEvent('meadow-voiceos', {detail: message}));
  }

  function applyPalette(palette, notify = false) {
    if (!Object.hasOwn(PALETTES, palette)) return;
    state.palette = palette;
    const entry = PALETTES[palette];
    document.body.dataset.palette = palette;
    document.documentElement.style.setProperty('--glaze-color', entry.swatch);
    document.documentElement.style.setProperty('--bleed', entry.colors[2].map(value => Math.round(value * 255)).join(', '));
    orb?.setAppearance({style: 'glaze', palette});
    edge.setState({colors: entry.colors});
    paletteOptions.querySelectorAll('[role="radio"]').forEach(button => {
      const selected = button.dataset.palette === palette;
      button.setAttribute('aria-checked', String(selected));
      button.tabIndex = selected ? 0 : -1;
    });
    try { localStorage.setItem(STORAGE_KEY, palette); } catch (_) { /* native host also receives the selection */ }
    if (notify) emit('palette', {palette});
  }

  function showPalette() {
    if (state.compact) { emit('expand'); return; }
    if (!palettePanel.hidden) return;
    focusBeforePalette = document.activeElement;
    palettePanel.hidden = false;
    document.getElementById('presence').setAttribute('aria-hidden', 'true');
    orbButton.tabIndex = -1;
    paletteOptions.querySelector('[aria-checked="true"]')?.focus({preventScroll: true});
  }

  function hidePalette() {
    if (palettePanel.hidden) return;
    palettePanel.hidden = true;
    document.getElementById('presence').removeAttribute('aria-hidden');
    orbButton.tabIndex = 0;
    const returnFocus = focusBeforePalette instanceof HTMLElement && focusBeforePalette.tabIndex >= 0
      ? focusBeforePalette : orbButton;
    returnFocus.focus({preventScroll: true});
  }

  for (const [palette, entry] of Object.entries(PALETTES)) {
    const button = document.createElement('button');
    button.type = 'button'; button.className = 'palette-option';
    button.dataset.palette = palette; button.setAttribute('role', 'radio');
    button.setAttribute('aria-checked', 'false'); button.tabIndex = -1;
    const swatch = document.createElement('span');
    swatch.className = 'palette-swatch'; swatch.style.setProperty('--swatch', entry.swatch); swatch.setAttribute('aria-hidden', 'true');
    const label = document.createElement('span'); label.textContent = entry.label;
    button.append(swatch, label);
    button.addEventListener('click', () => applyPalette(palette, true), events);
    button.addEventListener('keydown', event => {
      const choices = [...paletteOptions.children];
      const index = choices.indexOf(button);
      const offsets = {ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1};
      if (!(event.key in offsets)) return;
      event.preventDefault();
      const next = choices[(index + offsets[event.key] + choices.length) % choices.length];
      applyPalette(next.dataset.palette, true); next.focus();
    }, events);
    paletteOptions.append(button);
  }

  function setState(update = {}) {
    if (disposed || !update || typeof update !== 'object') return getState();
    const previousPhase = state.phase;
    if (PHASES.has(update.phase)) state.phase = update.phase;
    if (previousPhase !== state.phase) state.energy = 0;
    if (Number.isFinite(update.energy)) state.energy = Math.min(1, Math.max(0, update.energy));
    if (typeof update.muted === 'boolean') state.muted = update.muted;
    if (typeof update.compact === 'boolean') {
      state.compact = update.compact;
      document.documentElement.dataset.compact = String(state.compact);
      document.body.dataset.compact = String(state.compact);
      if (state.compact) hidePalette();
      orb?.resize();
    }
    if (Object.hasOwn(PALETTES, update.palette)) applyPalette(update.palette);
    document.body.dataset.phase = state.phase;
    orbButton.setAttribute('aria-label', state.compact ? '展開 Meadow VoiceOS' : PHASE_LABELS[state.phase]);
    if (previousPhase !== state.phase) status.textContent = PHASE_LABELS[state.phase];
    // The microphone remains live while a background turn is thinking. Its
    // measured energy must still reach the orb when the user speaks again.
    const audible = (state.phase === 'listening' || state.phase === 'thinking' || state.phase === 'speaking') && !state.muted;
    orb?.setState(audible, state.muted);
    // The renderer expects RMS. Invert its transfer function for the host's 0..1 level.
    orb?.setInput(audible ? .003 + .035 * Math.pow(state.energy, 1 / .60) : 0);
    edge.setState({energy: audible ? state.energy : 0, compact: state.compact});
    return getState();
  }

  function getState() {
    return {...state, paletteOpen: !palettePanel.hidden, renderer: orb?.host.dataset.renderer || 'fallback', edgeRenderer: document.body.dataset.edgeRenderer || 'fallback'};
  }

  orbButton.addEventListener('click', () => {
    emit(state.compact ? 'expand' : 'toggle-microphone');
  }, events);
  document.addEventListener('click', event => {
    if (!suppressClick) return;
    suppressClick = false;
    event.preventDefault(); event.stopImmediatePropagation();
  }, {...events, capture: true});
  paletteClose.addEventListener('click', hidePalette, events);
  // Only the native host owns real work surfaces. The visual-only preview
  // must not offer buttons that cannot open a file or the current plan.
  workActions.hidden = !window.webkit?.messageHandlers?.meadowVoiceOS;
  workActions.querySelectorAll('button').forEach(button => {
    button.addEventListener('click', () => {
      hidePalette(); emit(button.dataset.action);
    }, events);
  });
  document.addEventListener('contextmenu', event => { event.preventDefault(); showPalette(); }, events);
  document.addEventListener('pointerdown', event => {
    // A fresh gesture clears a long-press that produced no compatibility click.
    suppressClick = false;
    if (!palettePanel.hidden && !palettePanel.contains(event.target)) {
      suppressClick = true; hidePalette(); return;
    }
    if (event.pointerType !== 'touch' || !palettePanel.hidden) return;
    downPoint = {x: event.clientX, y: event.clientY};
    clearTimeout(longPress);
    longPress = setTimeout(() => { suppressClick = true; showPalette(); }, 550);
  }, {...events, capture: true});
  document.addEventListener('pointermove', event => {
    if (downPoint && Math.hypot(event.clientX - downPoint.x, event.clientY - downPoint.y) > 8) clearTimeout(longPress);
  }, events);
  const endPointer = () => { clearTimeout(longPress); downPoint = null; };
  document.addEventListener('pointerup', endPointer, events);
  document.addEventListener('pointercancel', endPointer, events);
  document.addEventListener('keydown', event => {
    if (event.key === 'Escape') {
      event.preventDefault();
      if (!palettePanel.hidden) hidePalette(); else emit('collapse');
    } else if (event.key.toLowerCase() === 'p' && !event.metaKey && !event.ctrlKey && !event.altKey) {
      event.preventDefault();
      if (palettePanel.hidden) showPalette(); else hidePalette();
    } else if (event.key === 'Tab' && !palettePanel.hidden) {
      const stops = [...palettePanel.querySelectorAll('button')]
        .filter(button => button.tabIndex >= 0 && !button.closest('[hidden]'));
      const index = stops.indexOf(document.activeElement);
      event.preventDefault();
      stops[(index + (event.shiftKey ? stops.length - 1 : 1)) % stops.length]?.focus();
    }
  }, events);

  window.MeadowVoiceOS = {
    setState, getState,
    showPalette, hidePalette,
    palettes: Object.fromEntries(Object.entries(PALETTES).map(([key, entry]) => [key, {label: entry.label, swatch: entry.swatch}])),
    dispose() {
      if (disposed) return;
      disposed = true; clearTimeout(longPress); abort.abort(); orb?.dispose(); edge.dispose();
    },
  };
  applyPalette(state.palette);
  setState(state);
  emit('ready', {palette: state.palette});
})();
