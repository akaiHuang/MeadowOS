import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('./harness_controls.js', import.meta.url), 'utf8');
const context = { setTimeout, clearTimeout, console };
vm.runInNewContext(source.replace(/^import .*;\n/gm, '').replace(/^export /gm, '') +
  '\nglobalThis.factory = createHarnessControlsController;', context);
const create = context.factory;
test('same-chat next send revalidates a failed read without an execution picker', async () => {
  let reads = 0;
  const h = setup({ harnessControls: async (sid) => {
    if (++reads === 1) throw new Error('synthetic offline');
    return response(sid);
  } });
  await h.controller.selectSession();
  assert.match(h.controller.snapshot().error, /synthetic offline/);
  assert.equal((await h.controller.ensureReady('one')).effectiveMode, 'codex-style');
  assert.equal(reads, 2);
  assert.equal(h.controller.snapshot().error, '');
});

test('headless initialization registers admission and preserves the server default', async () => {
  const handlers = new Map();
  let admission;
  const ctx = {
    setTimeout, clearTimeout, console,
    state: { sessionId: 'one' },
    daemon: { harnessControls: async (sid) => response(sid) },
    bus: { on(name, callback) { handlers.set(name, callback); } },
    registerPromptAdmissionCheck(check) { admission = check; },
    setEvoRunLimitAvailability() {},
    setRunLimitExecutionMode() {},
    document: { querySelector() { throw new Error('No experimental picker may be required'); } },
  };
  vm.runInNewContext(source.replace(/^import .*;\n/gm, '').replace(/^export /gm, '') +
    '\ninitHarnessControls();', ctx);
  await new Promise(setImmediate);
  assert.equal(typeof admission, 'function');
  assert.equal((await admission('one')).selectedMode, '');
  assert.equal(handlers.has('thread:switched'), true);
});

test('automatic same-session selection cannot erase a settled failed write', async () => {
  const h = setup({ setHarnessControls: async () => { throw new Error('synthetic write failure'); } });
  await h.controller.selectSession();
  await h.controller.save('raven-code');
  await h.controller.selectSession();
  await assert.rejects(h.controller.ensureReady('one'), /synthetic write failure/);
  h.switchTo('two');
  await h.controller.selectSession();
  h.switchTo('one');
  await h.controller.selectSession();
  await assert.rejects(h.controller.ensureReady('one'), /synthetic write failure/);
  await h.controller.refresh();
  assert.equal((await h.controller.ensureReady('one')).selectedMode, '');
});

test('a failed automatic read cannot turn a settled write failure into send-time recovery', async () => {
  let reads = 0;
  const h = setup({
    harnessControls: async (sid) => {
      if (++reads === 2) throw new Error('synthetic automatic read failure');
      return response(sid);
    },
    setHarnessControls: async () => { throw new Error('synthetic write failure'); },
  });
  await h.controller.selectSession();
  await h.controller.save('raven-code');
  await h.controller.selectSession();
  assert.equal(h.controller.snapshot().controls, null);
  assert.match(h.controller.snapshot().error, /synthetic automatic read failure/);
  await assert.rejects(h.controller.ensureReady('one'));
  assert.equal(reads, 2, 'send must not perform an automatic read that clears a failed write');
  await h.controller.refresh();
  assert.equal(reads, 3);
  assert.equal((await h.controller.ensureReady('one')).selectedMode, '');
  assert.equal(h.controller.snapshot().error, '');
});
const modes = [
  { id: '', label: '自動' },
  { id: 'kernel', label: 'Meadow Kernel' },
  { id: 'codex-style', label: 'Meadow' },
  { id: 'raven-code', label: 'Raven Code' },
];
const response = (sessionId, patch = {}) => ({ sessionId, modelId: 'local-35b',
  selectedMode: '', effectiveMode: 'codex-style', activeMode: '', running: false,
  appliesNextRun: true, preferenceStatus: 'default', modes, ...patch });
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function setup(api = {}, extra = {}) {
  let session = 'one';
  const calls = [];
  const snapshots = [];
  const controller = create({
    api: {
      harnessControls: async (sid) => { calls.push(['GET', sid]); return response(sid); },
      setHarnessControls: async (sid, body) => {
        calls.push(['POST', sid, body.mode]);
        return response(sid, { selectedMode: body.mode, effectiveMode: body.mode || 'codex-style' });
      }, ...api,
    },
    currentSession: () => session,
    onChange: (view) => snapshots.push(view), ...extra,
  });
  return { controller, calls, snapshots, switchTo: (sid) => { session = sid; } };
}

test('passive load preserves deployment default; each explicit choice is persisted by API', async () => {
  const h = setup();
  await h.controller.selectSession();
  assert.equal(h.controller.snapshot().controls.selectedMode, '');
  assert.equal(h.controller.snapshot().controls.effectiveMode, 'codex-style');
  assert.deepEqual(h.calls, [['GET', 'one']]);
  for (const mode of ['raven-code', 'kernel', 'codex-style', '']) {
    await h.controller.save(mode);
    assert.equal(h.controller.snapshot().controls.selectedMode, mode);
    assert.deepEqual(h.calls.at(-1), ['POST', 'one', mode]);
  }
  const before = h.calls.length;
  await h.controller.save('unknown');
  assert.equal(h.calls.length, before);
});

test('saving retains confirmed selection and admitted active identity until acknowledgement', async () => {
  const saved = deferred();
  const h = setup({
    harnessControls: async (sid) => response(sid, { running: true, activeMode: 'codex-style' }),
    setHarnessControls: () => saved.promise,
  });
  await h.controller.selectSession();
  const save = h.controller.save('raven-code');
  assert.equal(h.controller.snapshot().saving, true);
  assert.equal(h.controller.snapshot().controls.selectedMode, '');
  saved.resolve(response('one', { selectedMode: 'raven-code', effectiveMode: 'raven-code',
    running: true, activeMode: 'codex-style' }));
  await save;
  assert.equal(h.controller.snapshot().controls.selectedMode, 'raven-code');
  assert.equal(h.controller.snapshot().controls.activeMode, 'codex-style');
  assert.equal(h.controller.snapshot().saving, false);
});

test('late reads after a session switch cannot overwrite new session selection', async () => {
  const old = deferred();
  const h = setup({ harnessControls: (sid) => sid === 'one' ? old.promise :
    Promise.resolve(response(sid, { selectedMode: 'kernel', effectiveMode: 'kernel' })) });
  const first = h.controller.selectSession();
  h.switchTo('two');
  await h.controller.selectSession();
  old.resolve(response('one', { selectedMode: 'raven-code', effectiveMode: 'raven-code' }));
  await first;
  assert.equal(h.controller.snapshot().sessionId, 'two');
  assert.equal(h.controller.snapshot().controls.selectedMode, 'kernel');
});

test('late save completion cannot clear a new session save owner', async () => {
  const old = deferred(), fresh = deferred();
  const h = setup({ setHarnessControls: (sid) => sid === 'one' ? old.promise : fresh.promise });
  await h.controller.selectSession();
  const oldSave = h.controller.save('raven-code');
  h.switchTo('two');
  await h.controller.selectSession();
  const freshSave = h.controller.save('kernel');
  old.resolve(response('one', { selectedMode: 'raven-code', effectiveMode: 'raven-code' }));
  await oldSave;
  assert.equal(h.controller.snapshot().saving, true);
  assert.equal(h.controller.snapshot().sessionId, 'two');
  fresh.resolve(response('two', { selectedMode: 'kernel', effectiveMode: 'kernel' }));
  await freshSave;
  assert.equal(h.controller.snapshot().controls.selectedMode, 'kernel');
  assert.equal(h.controller.snapshot().saving, false);
});

test('model change invalidates pending read even when session ID is unchanged', async () => {
  const old = deferred();
  let reads = 0;
  const h = setup({ harnessControls: (sid) => ++reads === 1 ? old.promise :
    Promise.resolve(response(sid, { modelId: 'deepseek', effectiveMode: 'kernel' })) });
  const first = h.controller.selectSession();
  await h.controller.selectSession();
  old.resolve(response('one'));
  await first;
  assert.equal(h.controller.snapshot().controls.modelId, 'deepseek');
  assert.equal(h.controller.snapshot().controls.effectiveMode, 'kernel');
});

test('notifications during POST cause authoritative reread rather than payload rollback', async () => {
  const saved = deferred();
  let reads = 0;
  const h = setup({
    harnessControls: async (sid) => response(sid, ++reads === 1 ? {} :
      { selectedMode: 'kernel', effectiveMode: 'kernel' }),
    setHarnessControls: () => saved.promise,
  });
  await h.controller.selectSession();
  const save = h.controller.save('raven-code');
  await h.controller.refresh();
  await h.controller.refresh();
  assert.equal(reads, 1);
  saved.resolve(response('one', { selectedMode: 'raven-code', effectiveMode: 'raven-code' }));
  await save;
  assert.equal(reads, 2);
  assert.equal(h.controller.snapshot().controls.selectedMode, 'kernel');
});

test('failed save keeps confirmed selection and exposes uncertainty without retrying POST', async () => {
  let posts = 0;
  const h = setup({ setHarnessControls: async () => { posts++; throw new Error('HTTP 503'); } });
  await h.controller.selectSession();
  await h.controller.save('raven-code');
  assert.equal(h.controller.snapshot().controls.selectedMode, '');
  assert.match(h.controller.snapshot().error, /未確認儲存.*HTTP 503/);
  assert.equal(h.controller.snapshot().saving, false);
  assert.equal(posts, 1);
  await h.controller.refresh();
  assert.equal(h.controller.snapshot().error, '');
  assert.equal(posts, 1);
});

test('malformed/foreign server identity is visible and never becomes a selectable confirmation', async () => {
  const h = setup({ harnessControls: async () => response('foreign') });
  await h.controller.selectSession();
  assert.equal(h.controller.snapshot().controls, null);
  assert.match(h.controller.snapshot().error, /回覆不完整/);
  await h.controller.save('raven-code');
  assert.equal(h.calls.length, 0);
});

test('timed-out POST retains write ownership until settlement and reconciles late confirmation', async () => {
  const saved = deferred();
  let persisted = '', posts = 0;
  const h = setup({
    harnessControls: async (sid) => response(sid, { selectedMode: persisted,
      effectiveMode: persisted || 'codex-style' }),
    setHarnessControls: () => { posts++; return saved.promise; },
  }, { timeoutMs: 5 });
  await h.controller.selectSession();
  await h.controller.save('raven-code');
  assert.match(h.controller.snapshot().error, /尚未確認.*逾時/);
  assert.equal(h.controller.snapshot().saving, true);
  await assert.rejects(h.controller.ensureReady('one'), /尚未確認/);
  await h.controller.refresh();
  await h.controller.save('kernel');
  assert.equal(posts, 1);
  persisted = 'raven-code';
  saved.resolve(response('one', { selectedMode: 'raven-code', effectiveMode: 'raven-code' }));
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.controller.snapshot().controls.selectedMode, 'raven-code');
  assert.equal(h.controller.snapshot().saving, false);
  assert.equal(h.controller.snapshot().error, '');
});

test('same-session model switch does not release outstanding write or admit a second save', async () => {
  const saved = deferred();
  let posts = 0;
  const h = setup({ setHarnessControls: () => { posts++; return saved.promise; } });
  await h.controller.selectSession();
  const save = h.controller.save('raven-code');
  await h.controller.selectSession();
  assert.equal(h.controller.snapshot().saving, true);
  await h.controller.save('kernel');
  assert.equal(posts, 1);
  saved.resolve(response('one', { selectedMode: 'raven-code', effectiveMode: 'raven-code' }));
  await save;
  assert.equal(h.controller.snapshot().saving, false);
});

test('failed save alert remains visible after automatic notification reconciliation', async () => {
  const saved = deferred();
  const h = setup({ setHarnessControls: () => saved.promise });
  await h.controller.selectSession();
  const save = h.controller.save('raven-code');
  await h.controller.refresh();
  saved.reject(new Error('HTTP 503'));
  await save;
  assert.match(h.controller.snapshot().error, /未確認儲存.*HTTP 503/);
  assert.equal(h.controller.snapshot().controls.selectedMode, '');
  await h.controller.refresh({ preserveError: true });
  assert.match(h.controller.snapshot().error, /未確認儲存.*HTTP 503/);
  await h.controller.refresh();
  assert.equal(h.controller.snapshot().error, '');
});

test('timeout while chat is offscreen remains visible on return and retains pending ownership', async () => {
  const saved = deferred();
  let persisted = '';
  const h = setup({
    harnessControls: async (sid) => response(sid, { selectedMode: persisted,
      effectiveMode: persisted || 'codex-style' }),
    setHarnessControls: () => saved.promise,
  }, { timeoutMs: 5 });
  await h.controller.selectSession();
  const save = h.controller.save('raven-code');
  h.switchTo('two');
  await h.controller.selectSession();
  await save;
  assert.equal(h.controller.snapshot().error, '');
  h.switchTo('one');
  await h.controller.selectSession();
  assert.equal(h.controller.snapshot().saving, true);
  assert.match(h.controller.snapshot().error, /尚未確認.*逾時/);
  persisted = 'raven-code';
  saved.resolve(response('one', { selectedMode: persisted, effectiveMode: persisted }));
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.controller.snapshot().saving, false);
  assert.equal(h.controller.snapshot().error, '');
  assert.equal(h.controller.snapshot().controls.selectedMode, 'raven-code');
});

test('admission waits pending selection and returns only confirmed persisted runtime', async () => {
  const saved = deferred();
  const h = setup({ setHarnessControls: () => saved.promise });
  await h.controller.selectSession();
  const save = h.controller.save('raven-code');
  let admitted = false;
  const ready = h.controller.ensureReady('one').then((value) => { admitted = true; return value; });
  await Promise.resolve();
  assert.equal(admitted, false);
  saved.resolve(response('one', { selectedMode: 'raven-code', effectiveMode: 'raven-code' }));
  await save;
  assert.equal((await ready).effectiveMode, 'raven-code');
});

test('failed pending selection blocks admission without fallback or automatic clearing', async () => {
  const saved = deferred();
  const h = setup({ setHarnessControls: () => saved.promise });
  await h.controller.selectSession();
  const save = h.controller.save('raven-code');
  const ready = h.controller.ensureReady('one');
  saved.reject(new Error('HTTP 503'));
  await save;
  await assert.rejects(ready, /未確認儲存.*HTTP 503/);
  await assert.rejects(h.controller.ensureReady('one'), /未確認儲存.*HTTP 503/);
  assert.equal(h.controller.snapshot().controls.selectedMode, '');
});

test('HTTP success returning another selection cannot acknowledge the requested runtime', async () => {
  const h = setup({ setHarnessControls: async (sid) => response(sid) });
  await h.controller.selectSession();
  await h.controller.save('raven-code');
  assert.match(h.controller.snapshot().error, /伺服器未確認所選執行方式/);
  await assert.rejects(h.controller.ensureReady('one'), /伺服器未確認所選執行方式/);
  assert.equal(h.controller.snapshot().controls.selectedMode, '');
});

test('admission rechecks session after pending write and rejects unknown preference state', async () => {
  const saved = deferred();
  const h = setup({ setHarnessControls: () => saved.promise });
  await h.controller.selectSession();
  const save = h.controller.save('raven-code');
  const ready = h.controller.ensureReady('one');
  h.switchTo('two');
  await h.controller.selectSession();
  saved.resolve(response('one', { selectedMode: 'raven-code', effectiveMode: 'raven-code' }));
  await save;
  await assert.rejects(ready, /對話已切換/);
  const invalid = setup({ harnessControls: async (sid) => response(sid, { preferenceStatus: 'invalid' }) });
  await invalid.controller.selectSession();
  await assert.rejects(invalid.controller.ensureReady('one'), /執行方式尚未確認/);
});

test('selection beginning when initial read publishes still blocks readiness until acknowledgement', async () => {
  const saved = deferred();
  let scheduled = false;
  const h = setup({ setHarnessControls: () => saved.promise }, {
    onChange: (view) => {
      if (!scheduled && view.controls) {
        scheduled = true;
        queueMicrotask(() => void h.controller.save('raven-code'));
      }
    },
  });
  let admitted = false;
  const ready = h.controller.ensureReady('one').then((value) => { admitted = true; return value; });
  for (let index = 0; index < 10; index++) await Promise.resolve();
  assert.equal(h.controller.snapshot().saving, true);
  assert.equal(admitted, false);
  saved.resolve(response('one', { selectedMode: 'raven-code', effectiveMode: 'raven-code' }));
  assert.equal((await ready).effectiveMode, 'raven-code');
});

test('first send joins startup and SSE reads instead of rejecting discarded confirmation', async () => {
  const observed = deferred();
  let reads = 0, admitted = false;
  const h = setup({ harnessControls: () => { reads++; return observed.promise; } });
  const initial = h.controller.selectSession();
  const ready = h.controller.ensureReady('one').then((value) => { admitted = true; return value; });
  const invalidations = Array.from({ length: 5 }, () => h.controller.refresh({ preserveError: true }));
  await new Promise(setImmediate);
  assert.equal(reads, 1, 'startup, send and concurrent SSE must share one native observation');
  assert.equal(admitted, false);
  assert.equal(h.controller.snapshot().loading, true);
  observed.resolve(response('one'));
  await Promise.all([initial, ...invalidations]);
  assert.equal((await ready).effectiveMode, 'codex-style');
  assert.equal(h.controller.snapshot().loading, false);
});

test('admission follows a replacement model read rather than rejecting the superseded read', async () => {
  const old = deferred(), current = deferred();
  let reads = 0, admitted = false;
  const h = setup({ harnessControls: () => ++reads === 1 ? old.promise : current.promise });
  const initial = h.controller.selectSession();
  const ready = h.controller.ensureReady('one').then((value) => { admitted = true; return value; });
  await new Promise(setImmediate);
  const changed = h.controller.selectSession();
  await new Promise(setImmediate);
  assert.equal(reads, 2, 'model invalidation must create a distinct observation');
  old.resolve(response('one'));
  await initial;
  await new Promise(setImmediate);
  assert.equal(admitted, false, 'the previous model cannot authorize the current model');
  current.resolve(response('one', { modelId: 'deepseek', effectiveMode: 'kernel' }));
  await changed;
  assert.equal((await ready).effectiveMode, 'kernel');
  assert.equal(h.controller.snapshot().controls.modelId, 'deepseek');
});

test('send joins an automatic refresh even when older confirmed controls exist', async () => {
  const changed = deferred();
  let reads = 0, admitted = false;
  const h = setup({ harnessControls: (sid) => ++reads === 1 ? Promise.resolve(response(sid)) : changed.promise });
  await h.controller.selectSession();
  const automatic = h.controller.refresh({ preserveError: true });
  const ready = h.controller.ensureReady('one').then((value) => { admitted = true; return value; });
  await new Promise(setImmediate);
  assert.equal(admitted, false, 'a pending authoritative invalidation must settle before sending');
  assert.equal(reads, 2);
  changed.resolve(response('one', { selectedMode: 'kernel', effectiveMode: 'kernel' }));
  await automatic;
  assert.equal((await ready).effectiveMode, 'kernel');
});

test('coalesced automatic recovery retains a failed write until explicit rereading', async () => {
  const observed = deferred();
  let reads = 0;
  const h = setup({
    harnessControls: (sid) => ++reads === 1 ? Promise.resolve(response(sid)) : observed.promise,
    setHarnessControls: async () => { throw new Error('synthetic failed write'); },
  });
  await h.controller.selectSession();
  await h.controller.save('raven-code');
  const automatic = h.controller.refresh({ preserveError: true });
  const coalesced = h.controller.refresh({ preserveError: true });
  await assert.rejects(h.controller.ensureReady('one'), /synthetic failed write/);
  const explicit = h.controller.refresh();
  observed.resolve(response('one'));
  await Promise.all([automatic, coalesced, explicit]);
  assert.equal(reads, 2);
  assert.equal((await h.controller.ensureReady('one')).effectiveMode, 'codex-style');
});

test('a shared observation failure and unavailable preference still prevent sending', async () => {
  const observed = deferred();
  let reads = 0;
  const h = setup({ harnessControls: () => { reads++; return observed.promise; } });
  const initial = h.controller.selectSession();
  const ready = h.controller.ensureReady('one');
  const rejected = assert.rejects(ready, /synthetic offline/);
  const automatic = h.controller.refresh({ preserveError: true });
  observed.reject(new Error('synthetic offline'));
  await Promise.all([initial, automatic, rejected]);
  assert.equal(reads, 1);
  const unavailable = setup({ harnessControls: async (sid) => response(sid, { preferenceStatus: 'unavailable' }) });
  await unavailable.controller.selectSession();
  await assert.rejects(unavailable.controller.ensureReady('one'), /執行方式尚未確認/);
});
