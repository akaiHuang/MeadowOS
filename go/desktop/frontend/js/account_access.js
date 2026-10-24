// A locked shell is separate from the login card's DOM lifetime. Native/Agent
// authorization remains authoritative; this keeps work controls and shortcuts
// unavailable while the account is being checked, signed out, or revoked.
export function initAccountAccessGuard({ isAllowed, onDenied }) {
  const locked = new Map();
  let restoreQueued = false;
  let gateSeen = false;
  let previouslyAllowed = false;
  const publicRoots = new Set(['commercialLoginGate', 'commercialAccount']);
  const passiveTags = new Set(['SCRIPT', 'STYLE', 'LINK']);

  const requestLogin = () => {
    if (restoreQueued || isAllowed()) return;
    restoreQueued = true;
    queueMicrotask(() => {
      restoreQueued = false;
      if (!isAllowed() && !document.getElementById('commercialLoginGate'))
        void onDenied();
    });
  };

  const sync = () => {
    const allowed = isAllowed();
    const gatePresent = Boolean(document.getElementById('commercialLoginGate'));
    gateSeen ||= gatePresent;
    const restoreGate = gateSeen || previouslyAllowed;
    previouslyAllowed = allowed;
    document.documentElement.dataset.accountAccess = allowed ? 'ready' : 'locked';
    for (const element of document.body.children) {
      if (passiveTags.has(element.tagName)) continue;
      const safe = publicRoots.has(element.id);
      if (!allowed && !safe) {
        if (!locked.has(element)) locked.set(element, {
          // Static shell roots already carry our initial locked marker.
          inert: element.hasAttribute('data-account-locked') ? false : element.inert,
        });
        element.inert = true;
        if (!element.hasAttribute('data-account-locked'))
          element.setAttribute('data-account-locked', '');
      } else if (locked.has(element)) {
        element.inert = locked.get(element).inert;
        element.removeAttribute('data-account-locked');
        locked.delete(element);
      }
    }
    for (const element of locked.keys()) if (!element.isConnected) locked.delete(element);
    if (!allowed && restoreGate) requestLogin();
  };

  const intercept = (event) => {
    if (isAllowed()) return;
    const target = event.target instanceof Element ? event.target : null;
    const publicTarget = target?.closest('#commercialLoginGate, #commercialAccount');
    const shortcut = event.type === 'keydown' && (event.metaKey || event.ctrlKey);
    if (shortcut && ['q', 'w'].includes(String(event.key).toLowerCase())) return;
    // Text editing remains available in account forms. App/window close is
    // handled by the OS. Work shortcuts never reach document/window handlers.
    const safeShortcut = !shortcut || ['a', 'c', 'v', 'x', 'z', 'y'].includes(String(event.key).toLowerCase());
    if (publicTarget && safeShortcut) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    if (!publicTarget) {
      const publicRoot = document.getElementById('commercialAccount') || document.getElementById('commercialLoginGate');
      publicRoot?.querySelector('button:not([disabled])')?.focus();
    }
    requestLogin();
  };
  for (const type of ['click', 'dblclick', 'pointerdown', 'keydown', 'focusin', 'submit', 'drop', 'paste', 'contextmenu'])
    window.addEventListener(type, intercept, true);
  window.addEventListener('commercial:change', sync);
  const observer = new MutationObserver(sync);
  observer.observe(document.body, { childList: true });
  sync();
  return sync;
}
