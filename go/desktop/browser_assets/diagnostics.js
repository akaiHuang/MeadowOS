(() => {
  if (window.__meadowEmbedDiagnostics) return;
  const emit = (kind, level, message, sourceURL = location.href, line = 0, column = 0, details = {}) => {
    try {
      window.webkit.messageHandlers.meadowDiagnostics.postMessage({
        kind, level, message: String(message || '').slice(0, 8192),
        ts: new Date().toISOString(), source_url: String(sourceURL || '').slice(0, 4096),
        line: Number(line) || 0, column: Number(column) || 0, details,
        document_id: JSON.parse(window.__meadowWebMCPBridge.describe()).document_id
      });
    } catch (_) { /* Native collection unavailable; never manufacture evidence. */ }
  };
  const text = value => {
    try { return typeof value === 'string' ? value : JSON.stringify(value); }
    catch (_) { return String(value); }
  };
  ['log', 'info', 'warn', 'error', 'debug'].forEach(level => {
    const original = console[level];
    console[level] = function(...args) {
      emit('console', level, args.map(text).join(' '));
      if (original) return original.apply(console, args);
    };
  });
  window.addEventListener('error', event => {
    if (event.target && event.target !== window) {
      const target = event.target;
      emit('resource', 'error', 'Resource failed to load', target.src || target.href || location.href,
        0, 0, {tag: String(target.tagName || '')});
    } else {
      emit('runtime', 'error', event.message || 'Runtime error', event.filename || location.href,
        event.lineno, event.colno);
    }
  }, true);
  window.addEventListener('unhandledrejection', event => {
    emit('unhandledrejection', 'error', text(event.reason));
  });
  let pending = 0, lastActivity = performance.now(), networkInstalled = true;
  const begin = () => { pending += 1; lastActivity = performance.now(); };
  const end = () => { pending = Math.max(0, pending - 1); lastActivity = performance.now(); };
  try {
    const originalFetch = window.fetch;
    if (typeof originalFetch === 'function') window.fetch = function(...args) {
      begin();
      try {
        return originalFetch.apply(this, args).then(value => { end(); return value; }, error => { end(); throw error; });
      } catch (error) { end(); throw error; }
    };
    const originalSend = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.send = function(...args) {
      let finished = false;
      const complete = () => { if (!finished) { finished = true; end(); } };
      begin(); this.addEventListener('loadend', complete, {once: true});
      try { return originalSend.apply(this, args); }
      catch (error) { complete(); this.removeEventListener('loadend', complete); throw error; }
    };
    const observer = new PerformanceObserver(list => {
      if (list.getEntries().length) lastActivity = performance.now();
    });
    observer.observe({entryTypes: ['resource']});
    window.addEventListener('load', () => { lastActivity = performance.now(); }, {once: true});
  } catch (_) { networkInstalled = false; }
  Object.defineProperty(window, '__meadowEmbedNetwork', {
    value: Object.freeze({get pending() { return pending; }, get lastActivity() { return lastActivity; },
      get installed() { return networkInstalled; }}),
    configurable: false, enumerable: false, writable: false
  });
  Object.defineProperty(window, '__meadowEmbedDiagnostics', {
    value: Object.freeze({coverage: Object.freeze({console: true, runtime: true, resource: true, unhandledrejection: true})}),
    configurable: false, enumerable: false, writable: false
  });
})();
