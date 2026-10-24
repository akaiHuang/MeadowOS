// Readiness is a UI hint only. Agent owns the verified grant and durable debit.
export function createPromptGrantWarmup({ identity, prepare }) {
  let flight = null;
  let ready = null;
  let revision = 0;
  function invalidate() { revision += 1; ready = null; flight = null; }
  async function warm({ refresh = false } = {}) {
    const key = identity();
    if (!key) return null;
    if (flight?.key === key) return flight.promise;
    if (!refresh && ready?.key === key && ready.available > 0 && ready.expiresAt > Date.now() + 1000) return ready;
    const attempt = { key, revision, promise: null };
    attempt.promise = (async () => {
      const value = await prepare();
      if (identity() !== key || revision !== attempt.revision) return null;
      const expiresAt = Date.parse(value?.expires_at);
      if (value?.schema !== 'meadow.desktop-prompt-grant.v1' || typeof value.ready !== 'boolean' ||
          !Number.isSafeInteger(value.available) || value.available < 0 || value.available > 5 ||
          (value.ready && (value.available === 0 || !Number.isFinite(expiresAt) || expiresAt <= Date.now())))
        throw new Error('無法確認本機可用額度，請稍後重試。');
      ready = { key, available: value.ready ? value.available : 0, expiresAt };
      return ready;
    })();
    flight = attempt;
    try { return await attempt.promise; }
    finally { if (flight === attempt) flight = null; }
  }
  return { invalidate, warm, debit() {
    revision += 1;
    if (ready) ready.available = Math.max(0, ready.available - 1);
  } };
}
