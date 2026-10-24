import assert from 'node:assert/strict';
import test from 'node:test';
import { createSidebarSessionCache, sidebarListingNotice } from './sidebar_session_cache.js';

const deferred = () => {
  let resolve, reject;
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail; });
  return { promise, resolve, reject };
};
const row = { sessionId: 'saved', title: 'Previous success' };

test('stalled project does not block a working project; polls coalesce without starvation', async () => {
  const slow = deferred();
  const calls = [];
  const changed = [];
  const cache = createSidebarSessionCache((cwd, signal) => {
    calls.push({ cwd, signal });
    return cwd === 'slow' ? slow.promise : Promise.resolve({ sessions: [row] });
  }, (cwd) => changed.push(cwd), 30);
  const pending = cache.refresh('slow');
  for (let i = 0; i < 10; i++) assert.equal(cache.refresh('slow'), pending);
  await cache.refresh('working');
  assert.deepEqual(cache.snapshot('working').rows, [row]);
  assert.deepEqual(changed, ['working']);
  assert.equal(cache.snapshot('slow').status, 'loading');
  await pending;
  assert.equal(calls.filter((call) => call.cwd === 'slow').length, 1);
  assert.equal(calls.find((call) => call.cwd === 'slow').signal.aborted, true);
  assert.equal(cache.snapshot('slow').status, 'unavailable');
  await cache.refresh('slow');
  assert.equal(calls.filter((call) => call.cwd === 'slow').length, 1);
  // The fetch mock ignores AbortSignal. Its late completion cannot overwrite
  // the timeout receipt or silently cause a background retry.
  slow.resolve({ sessions: [] });
  await Promise.resolve();
  assert.equal(cache.snapshot('slow').status, 'unavailable');
});

test('timeouts and cancellation retain last successful rows until explicit retry', async () => {
  let mode = 'ok';
  let calls = 0;
  const cache = createSidebarSessionCache(() => {
    calls++;
    if (mode === 'timeout') return new Promise(() => {});
    if (mode === 'cancel') return Promise.reject(new DOMException('cancelled', 'AbortError'));
    return Promise.resolve({ sessions: [row] });
  }, () => {}, 5);
  await cache.refresh('cached');
  mode = 'timeout';
  await cache.refresh('cached');
  assert.deepEqual(cache.snapshot('cached').rows, [row]);
  assert.match(sidebarListingNotice(cache.snapshot('cached')), /顯示上次資料/);
  await cache.refresh('cached');
  assert.equal(calls, 2);
  mode = 'cancel';
  await cache.refresh('cached', { retryFailed: true });
  assert.deepEqual(cache.snapshot('cached').rows, [row]);
  assert.equal(cache.snapshot('cached').status, 'unavailable');
  mode = 'ok';
  await cache.refresh('cached', { retryFailed: true });
  assert.equal(cache.snapshot('cached').status, 'ready');
});

test('unknown, unavailable, malformed and successful empty lists stay distinct', async () => {
  const cache = createSidebarSessionCache((cwd) => Promise.resolve(
    cwd === 'empty' ? { sessions: [] } : { error: 'missing list' }), () => {}, 30);
  assert.match(sidebarListingNotice(cache.snapshot('new')), /正在載入/);
  await cache.refresh('bad');
  assert.equal(cache.snapshot('bad').rows, undefined);
  assert.match(sidebarListingNotice(cache.snapshot('bad')), /無法載入/);
  await cache.refresh('empty');
  assert.equal(cache.snapshot('empty').status, 'ready');
  assert.deepEqual(cache.snapshot('empty').rows, []);
  assert.equal(sidebarListingNotice(cache.snapshot('empty')), '');
});

test('removed and re-added project ignores its superseded old response', async () => {
  const old = deferred();
  let calls = 0;
  const cache = createSidebarSessionCache(() => ++calls === 1
    ? old.promise : Promise.resolve({ sessions: [row] }), () => {}, 15);
  const pending = cache.refresh('same');
  await Promise.resolve();
  cache.sync([]);
  await cache.refresh('same');
  old.resolve({ sessions: [{ sessionId: 'obsolete' }] });
  await pending;
  assert.deepEqual(cache.snapshot('same').rows, [row]);
});
