(function installProductionGuard() {
  'use strict';

  const host = String(window.location.hostname || '').toLowerCase();
  const isLoopback =
    host === 'localhost' || host === '127.0.0.1' || host === '::1';
  const isProduction = !isLoopback;
  document.documentElement.dataset.frontendMode = isProduction
    ? 'production'
    : 'development';
  if (!isProduction) return;

  const blockedPath = /^\/(?:_?debug|_?internal|metrics|pprof)(?:\/|$)/i;
  const originalFetch = window.fetch.bind(window);
  window.fetch = function guardedFetch(input, init) {
    const raw = input instanceof Request ? input.url : input;
    const url = new URL(String(raw || ''), window.location.href);
    if (
      url.origin !== window.location.origin ||
      blockedPath.test(url.pathname)
    ) {
      return Promise.reject(new TypeError('Endpoint unavailable'));
    }
    return originalFetch(input, init);
  };

  const NativeEventSource = window.EventSource;
  const eventPath = /^\/session\/[^/]+\/(?:events|term\/[^/]+\/stream)$/;
  window.EventSource = class GuardedEventSource extends NativeEventSource {
    constructor(input, config) {
      const url = new URL(String(input || ''), window.location.href);
      if (
        url.origin !== window.location.origin ||
        !eventPath.test(url.pathname)
      ) {
        throw new TypeError('Event stream unavailable');
      }
      super(url.href, config);
    }
  };

  window.WebSocket = class DisabledWebSocket {
    constructor() {
      throw new TypeError('WebSocket unavailable');
    }
  };

  const discard = function () {};
  for (const level of ['debug', 'info', 'log', 'warn', 'error']) {
    try {
      console[level] = discard;
    } catch (_) {}
  }

  const isDesktopShell =
    host === 'wails.localhost' ||
    window.location.protocol === 'wails:' ||
    window.location.protocol === 'file:';
  if (isDesktopShell) {
    window.addEventListener(
      'keydown',
      (event) => {
        const key = String(event.key || '').toLowerCase();
        const inspectorChord =
          key === 'f12' ||
          (((event.ctrlKey && event.shiftKey) ||
            (event.metaKey && event.altKey)) &&
            ['c', 'i', 'j'].includes(key));
        if (!inspectorChord) return;
        event.preventDefault();
        event.stopImmediatePropagation();
      },
      true,
    );
  }
})();
