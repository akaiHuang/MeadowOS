// A stalled project must not hold the whole sidebar hostage. Failed entries
// stay frozen until an explicit user action: aborting fetch does not prove the
// server's filesystem work stopped, so background polling must not retry it.
export function createSidebarSessionCache(load, changed, timeoutMs = 5000) {
  const entries = new Map();
  const snapshot = (cwd) => {
    const entry = entries.get(cwd);
    return entry ? { rows: entry.rows, status: entry.status } : { rows: undefined, status: 'loading' };
  };
  const sync = (paths) => {
    const current = new Set(paths);
    for (const [cwd, entry] of entries) {
      if (!current.has(cwd)) {
        entries.delete(cwd);
        entry.controller?.abort();
      }
    }
  };
  const refresh = (cwd, { retryFailed = false } = {}) => {
    let entry = entries.get(cwd);
    if (!entry) {
      entry = { rows: undefined, status: 'loading', flight: null };
      entries.set(cwd, entry);
    }
    if (entry.flight) return entry.flight;
    if (entry.status === 'unavailable' && !retryFailed) return Promise.resolve(snapshot(cwd));
    const controller = new AbortController();
    entry.controller = controller;
    entry.status = 'loading';
    const attempt = {};
    entry.attempt = attempt;
    let timer;
    const timeout = new Promise((_, reject) => {
      timer = setTimeout(() => {
        controller.abort();
        reject(new Error('Sidebar session list timed out'));
      }, timeoutMs);
    });
    const request = Promise.resolve().then(() => load(cwd, controller.signal));
    entry.flight = Promise.race([request, timeout]).then((result) => {
      if (!Array.isArray(result?.sessions)) throw new Error('Session list unavailable');
      if (entries.get(cwd) === entry && entry.attempt === attempt) {
        entry.rows = result.sessions;
        entry.status = 'ready';
      }
    }).catch(() => {
      if (entries.get(cwd) === entry && entry.attempt === attempt) entry.status = 'unavailable';
    }).finally(() => {
      clearTimeout(timer);
      if (entries.get(cwd) === entry && entry.attempt === attempt) {
        entry.flight = null;
        changed(cwd);
      }
    });
    return entry.flight;
  };
  return { snapshot, sync, refresh };
}

export function sidebarListingNotice(snapshot) {
  if (snapshot.status === 'unavailable') return snapshot.rows === undefined
    ? '無法載入對話列表；展開專案可重試'
    : '更新失敗，顯示上次資料；展開專案可重試';
  return snapshot.rows === undefined ? '正在載入對話列表…' : '';
}
