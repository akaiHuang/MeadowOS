// Presentation only: user scrolling can temporarily uncover the conversation.
// Keep the scroll viewport stable; never change send, queue or model state.
export function initMobileReading(mq) {
  const body = document.body;
  const scroller = document.getElementById('scroll');
  const composer = document.getElementById('composerWrap');
  const terminal = document.getElementById('termDrawer');
  let previousTop = scroller.scrollTop;
  let gestureUntil = 0;
  let touchActive = false;
  let restoreTimer;
  let measuredHeight = 0;

  const nearLatest = () =>
    scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 48;
  const visible = (node) => node && node.getClientRects().length > 0;
  const anyVisible = (selector) =>
    [...document.querySelectorAll(selector)].some(visible);
  const needsComposer = () =>
    composer.contains(document.activeElement) ||
    anyVisible('#pendingArea > *') ||
    visible(document.getElementById('taskQueue')) ||
    anyVisible('#attachStrip > *') ||
    Boolean(document.getElementById('busy').textContent.trim()) ||
    anyVisible('.pop.open, #modelModal:not([hidden])') ||
    !terminal.hidden ||
    body.classList.contains('fresh-thread') ||
    body.classList.contains('mobile-work-open') ||
    !body.classList.contains('side-collapsed');

  function reveal() {
    clearTimeout(restoreTimer);
    gestureUntil = 0;
    body.classList.remove('mobile-reading');
  }
  function restoreAfterPause() {
    clearTimeout(restoreTimer);
    restoreTimer = setTimeout(() => {
      if (touchActive) restoreAfterPause();
      else reveal();
    }, 900);
  }
  function measure() {
    if (!mq.matches) {
      reveal();
      body.style.removeProperty('--mobile-composer-height');
      body.style.removeProperty('--mobile-terminal-height');
      measuredHeight = 0;
      return;
    }
    const pinned = nearLatest();
    const height = Math.ceil(composer.getBoundingClientRect().height);
    if (height !== measuredHeight) {
      measuredHeight = height;
      body.style.setProperty('--mobile-composer-height', `${height}px`);
      // Growing a draft/approval must keep the latest message above the input.
      if (pinned) scroller.scrollTop = scroller.scrollHeight;
    }
    body.style.setProperty(
      '--mobile-terminal-height',
      `${terminal.hidden ? 0 : Math.ceil(terminal.getBoundingClientRect().height)}px`,
    );
    previousTop = scroller.scrollTop;
    if (needsComposer()) reveal();
  }
  function armGesture(event) {
    if (!mq.matches || event.target.closest('.leaflet-container')) return;
    gestureUntil = performance.now() + 1200;
  }
  scroller.addEventListener(
    'wheel',
    (event) => {
      if (Math.abs(event.deltaY) > Math.abs(event.deltaX)) armGesture(event);
    },
    { passive: true },
  );
  scroller.addEventListener(
    'touchstart',
    (event) => {
      if (event.touches.length !== 1) return;
      armGesture(event);
      touchActive = gestureUntil > performance.now();
    },
    { passive: true },
  );
  scroller.addEventListener(
    'touchmove',
    (event) => {
      if (event.touches.length === 1) armGesture(event);
      else {
        touchActive = false;
        reveal();
      }
    },
    { passive: true },
  );
  const endTouch = () => {
    touchActive = false;
    if (body.classList.contains('mobile-reading')) restoreAfterPause();
  };
  scroller.addEventListener('touchend', endTouch, { passive: true });
  scroller.addEventListener('touchcancel', endTouch, { passive: true });
  scroller.addEventListener('keydown', (event) => {
    if (
      [
        'ArrowUp',
        'ArrowDown',
        'PageUp',
        'PageDown',
        'Home',
        'End',
        ' ',
      ].includes(event.key)
    )
      armGesture(event);
  });
  scroller.addEventListener(
    'scroll',
    () => {
      const top = scroller.scrollTop;
      const movement = Math.abs(top - previousTop);
      previousTop = top;
      if (!mq.matches || nearLatest() || needsComposer()) {
        reveal();
        return;
      }
      if (movement < 2 || performance.now() > gestureUntil) return;
      body.classList.add('mobile-reading');
      // Native touch inertia can continue after the final touchmove event.
      gestureUntil = performance.now() + 1200;
      restoreAfterPause();
    },
    { passive: true },
  );
  composer.addEventListener('focusin', reveal);
  document.addEventListener('conversation:latest', reveal);
  mq.addEventListener('change', () => {
    reveal();
    measure();
  });
  new ResizeObserver(measure).observe(composer);
  new ResizeObserver(measure).observe(terminal);
  new MutationObserver(() => {
    if (needsComposer()) reveal();
  }).observe(composer, {
    childList: true,
    subtree: true,
    attributes: true,
    attributeFilter: ['hidden'],
  });
  // Opening a drawer/popover or changing threads must never retain a hidden input.
  new MutationObserver(() => {
    if (body.classList.contains('mobile-reading') && needsComposer()) reveal();
  }).observe(body, { attributes: true, attributeFilter: ['class'] });
  measure();
  return reveal;
}
