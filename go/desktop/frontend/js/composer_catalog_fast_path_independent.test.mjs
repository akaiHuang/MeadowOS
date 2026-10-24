import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { configuredPickerRows } from './model_picker_catalog.js';

// Execute the actual publisher with controlled transports. No Wails, account,
// provider, service, or model is contacted. The optional source path permits a
// reproducible before/after check against a saved pre-fix composer.js.
const source = readFileSync(process.env.MEADOW_COMPOSER_TEST_SOURCE || new URL('./composer.js', import.meta.url), 'utf8');
function section(start, end) {
  const offset = source.indexOf(start);
  assert.notEqual(offset, -1, start);
  const limit = source.indexOf(end, offset);
  assert.notEqual(limit, -1, end);
  return source.slice(offset, limit);
}
const publisher = section('async function refetchModels(', 'function paintModelRows()');
const lookup = section('function modelEntryFor(', 'function currentModelSupportsImages(');
const tick = () => new Promise(resolve => setImmediate(resolve));
function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
const nativeCatalog = () => ({
  mode: 'connections', selectedConnectionId: 'heimdallm', selectedModelId: 'glm-5.3',
  connections: [{ id: 'heimdallm', name: 'HeimdaLLM', models: [
    { id: 'glm-5.3', name: 'GLM 5.3', supportsImages: false },
    { id: 'penclaw', name: 'Penclaw', supportsImages: true },
  ] }],
});
const ok = (body = nativeCatalog()) => ({ ok: true, status: 200, json: async () => body });
function fixture({ catalog = async () => ok(), discovery = () => new Promise(() => {}) } = {}) {
  const calls = { fetch: [], discovery: 0, loads: [], stored: [], retired: 0, rejectedImages: 0 };
  const timers = new Map();
  let nextTimer = 0;
  const pill = { textContent: 'Original Penclaw' };
  const original = [{ modelId: 'stale-penclaw', name: 'Original Penclaw' }];
  const context = vm.createContext({
    state: { sessionId: 'chat-A', modelConnectionID: 'heimdallm', model: 'glm-5.3' },
    access: true, accountGeneration: 1, attachmentAccountRevision: 0, attachmentSessionRevision: 0,
    modelFetchRevision: 0, modelListRefreshing: false, modelFetched: false, modelFetchFailed: false,
    modelCache: original, MODEL_CACHE_KEY: 'independent-fixture-models', imageCapabilityCheck: null,
    hasClientAccess: () => context.access,
    clientAccountGeneration: () => context.accountGeneration,
    modelModalOpen: () => false, paintModelRows() {},
    configuredPickerRows,
    fetch: async (url, options) => { calls.fetch.push({ url, options }); return catalog(); },
    daemon: {
      modelList: () => { calls.discovery++; return discovery(); },
      loadModel: async id => { calls.loads.push(id); },
    },
    setTimeout: callback => { const id = ++nextTimer; timers.set(id, callback); return id; },
    clearTimeout: id => timers.delete(id),
    localStorage: { setItem: (key, value) => calls.stored.push({ key, value: JSON.parse(value) }) },
    cacheableModelList: rows => rows,
    clearRetiredSessionModel: () => { calls.retired++; },
    rejectImageForCurrentModel: () => { calls.rejectedImages++; },
    $: () => pill, shortName: value => String(value).split('/').at(-1),
    displayName: model => model?.name || model?.modelId || '', isStandardProductModel: () => true,
  });
  vm.runInContext(`${lookup}\n${publisher}`, context, { filename: 'composer-catalog-independent-fixture.js' });
  vm.runInContext(section('async function verifyImageCapability(', 'function rejectImageForCurrentModel('), context);
  return { context, calls, pill, original, timers };
}
function assertUnpublished(f) {
  assert.equal(f.context.modelCache, f.original);
  assert.equal(f.pill.textContent, 'Original Penclaw');
  assert.equal(f.calls.stored.length, 0);
  assert.equal(f.calls.retired, 0);
}
async function immediatelySettled(promise) {
  let settled = false;
  promise.then(() => { settled = true; }, () => { settled = true; });
  await tick();
  assert.equal(settled, true, 'saved catalog must settle before discovery or the 12-second deadline');
  return promise;
}

test('authenticated saved catalog publishes both models immediately without starting discovery', async () => {
  const f = fixture();
  const rows = await immediatelySettled(f.context.refetchModels());
  assert.deepEqual(Array.from(rows, row => [row.connectionId, row.modelId]), [
    ['heimdallm', 'glm-5.3'], ['heimdallm', 'penclaw'],
  ]);
  assert.equal(f.calls.discovery, 0);
  assert.deepEqual(f.calls.fetch.map(call => [call.url, call.options.headers['X-Meadow-Settings']]), [
    ['/native/model-providers', '1'],
  ]);
  assert.equal(f.context.modelCache, rows);
  assert.equal(f.pill.textContent, 'GLM 5.3');
  assert.equal(f.calls.stored.length, 1);
  assert.deepEqual(f.calls.stored[0].value.map(row => row.modelId), ['glm-5.3', 'penclaw']);
  assert.equal(f.context.modelListRefreshing, false);
  assert.equal(f.context.modelFetchFailed, false);
  assert.equal(f.timers.size, 0);
});

test('explicit empty saved catalog clears stale models immediately without discovery', async () => {
  const f = fixture({ catalog: async () => ok({ mode: 'connections', connections: [] }) });
  const rows = await immediatelySettled(f.context.refetchModels());
  assert.deepEqual(Array.from(rows), []);
  assert.equal(f.calls.discovery, 0);
  assert.equal(f.context.modelCache.length, 0);
  assert.deepEqual(f.calls.stored[0].value, []);
  assert.equal(f.context.modelFetchFailed, false);
});

for (const [name, catalog] of [
  ['404 unsupported', async () => ({ ok: false, status: 404 })],
  ['legacy protected composition', async () => ok({ mode: 'protected' })],
  ['network failure', async () => { throw new Error('fixture offline'); }],
]) {
  test(`${name} retains legacy discovery fallback`, async () => {
    const legacy = [{ modelId: 'legacy-core', name: 'Legacy Core', live: true }];
    const f = fixture({ catalog, discovery: async () => legacy });
    f.context.state.modelConnectionID = '';
    const rows = await immediatelySettled(f.context.refetchModels());
    assert.equal(rows, legacy);
    assert.equal(f.calls.discovery, 1);
    assert.equal(f.context.modelCache, legacy);
    assert.equal(f.calls.stored.length, 1);
  });
}

for (const status of [401, 403]) {
  test(`catalog ${status} cannot fall back to a discoverable model or overwrite storage`, async () => {
    const f = fixture({ catalog: async () => ({ ok: false, status }),
      discovery: async () => [{ modelId: 'must-not-restore', live: true }] });
    assert.equal(await immediatelySettled(f.context.refetchModels()), null);
    assert.equal(f.calls.discovery, 0);
    assert.equal(f.context.modelFetchFailed, true);
    assertUnpublished(f);
  });
}

for (const [name, mutate] of [
  ['logout', c => { c.access = false; }],
  ['authenticated account replacement', c => { c.accountGeneration++; }],
  ['account attachment revision', c => { c.attachmentAccountRevision++; }],
  ['session revision', c => { c.attachmentSessionRevision++; }],
  ['session identity', c => { c.state.sessionId = 'chat-B'; }],
  ['connection identity', c => { c.state.modelConnectionID = 'other-provider'; }],
]) {
  test(`held catalog cannot publish after ${name}`, async () => {
    const gate = deferred();
    const f = fixture({ catalog: () => gate.promise });
    const pending = f.context.refetchModels();
    await tick();
    mutate(f.context);
    gate.resolve(ok());
    assert.equal(await immediatelySettled(pending), null);
    assert.equal(f.calls.discovery, 0);
    assertUnpublished(f);
  });
}

test('authenticated replacement with no session cannot recover an old account catalog', async () => {
  const gate = deferred();
  const f = fixture({ catalog: () => gate.promise });
  f.context.state.sessionId = null;
  f.context.state.modelConnectionID = '';
  const pending = f.context.refetchModels();
  await tick();
  f.context.accountGeneration++;
  gate.resolve(ok());
  assert.equal(await immediatelySettled(pending), null);
  assertUnpublished(f);
});

test('newer catalog publication is not overwritten by an older same-owner response', async () => {
  const old = deferred();
  let requests = 0;
  const f = fixture({ catalog: () => ++requests === 1 ? old.promise : Promise.resolve(ok()) });
  const oldRequest = f.context.refetchModels();
  await tick();
  const current = await immediatelySettled(f.context.refetchModels());
  const retired = f.calls.retired;
  old.resolve(ok({ ...nativeCatalog(), connections: [{ id: 'heimdallm', models: [{ id: 'obsolete-model' }] }] }));
  await immediatelySettled(oldRequest);
  assert.equal(f.context.modelCache, current);
  assert.equal(f.pill.textContent, 'GLM 5.3');
  assert.equal(f.calls.stored.length, 1);
  assert.equal(f.calls.retired, retired);
  assert.equal(f.calls.discovery, 0);
});

test('signed-out response never restores model cache or persists provider metadata', async () => {
  const f = fixture();
  f.context.access = false;
  assert.equal(await immediatelySettled(f.context.refetchModels()), null);
  assertUnpublished(f);
});

test('image verification ignores stale saved vision support when the connected runtime rejects images', async () => {
  const f = fixture({ discovery: async () => [{ modelId: 'penclaw', live: true, multimodal: false }] });
  f.context.state.model = 'penclaw';
  assert.equal(await immediatelySettled(f.context.verifyImageCapability('penclaw')), false);
  assert.equal(f.calls.discovery, 1);
  assert.deepEqual(f.calls.loads, []);
  assert.equal(f.calls.rejectedImages, 1);
  assert.equal(f.context.modelCache.find(row => row.modelId === 'penclaw').multimodal, false);
});

test('image verification accepts fresh vision capability without reloading an already connected model', async () => {
  const f = fixture({ discovery: async () => [{ modelId: 'penclaw', live: true, multimodal: true }] });
  f.context.state.model = 'penclaw';
  assert.equal(await immediatelySettled(f.context.verifyImageCapability('penclaw')), true);
  assert.equal(f.calls.discovery, 1);
  assert.deepEqual(f.calls.loads, []);
  assert.equal(f.calls.rejectedImages, 0);
});

test('cold image model loads once and verifies fresh runtime capability after activation', async () => {
  let reads = 0;
  const f = fixture({ discovery: async () => ++reads === 1
    ? [{ modelId: 'penclaw', live: false, multimodal: false }]
    : [{ modelId: 'penclaw', live: true, multimodal: true }] });
  f.context.state.model = 'penclaw';
  assert.equal(await immediatelySettled(f.context.verifyImageCapability('penclaw')), true);
  assert.equal(f.calls.discovery, 2);
  assert.deepEqual(f.calls.loads, ['penclaw']);
  assert.equal(f.calls.rejectedImages, 0);
});

test('missing runtime model cannot inherit saved vision support, including after activation', async () => {
  const f = fixture({ discovery: async () => [] });
  f.context.state.model = 'penclaw';
  assert.equal(await immediatelySettled(f.context.verifyImageCapability('penclaw')), false);
  assert.equal(f.calls.discovery, 2);
  assert.deepEqual(f.calls.loads, ['penclaw']);
  assert.equal(f.calls.rejectedImages, 1);
});

test('runtime discovery failure fails closed for an explicit image attachment', async () => {
  const f = fixture({ discovery: async () => { throw new Error('fixture runtime unavailable'); } });
  f.context.state.model = 'penclaw';
  await assert.rejects(f.context.verifyImageCapability('penclaw'), /無法確認模型的圖片能力/);
  assert.equal(f.calls.discovery, 1);
  assert.deepEqual(f.calls.loads, []);
  assertUnpublished(f);
});

test('account replacement during image runtime verification cannot authorize an attachment', async () => {
  const gate = deferred();
  const f = fixture({ discovery: () => gate.promise });
  f.context.state.model = 'penclaw';
  const pending = f.context.verifyImageCapability('penclaw');
  await tick();
  assert.equal(f.calls.discovery, 1);
  f.context.accountGeneration++;
  gate.resolve([{ modelId: 'penclaw', live: true, multimodal: true }]);
  await assert.rejects(pending, /無法確認模型的圖片能力/);
  assertUnpublished(f);
});
