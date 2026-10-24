import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { configuredPickerRows } from './model_picker_catalog.js';

// Execute the actual picker handlers with controlled asynchronous transports.
// These fixtures do not launch Wails, authenticate a real account, or infer.
const source = readFileSync(new URL('./composer.js', import.meta.url), 'utf8');
const section = (start, end) => {
  const offset = source.indexOf(start);
  assert.notEqual(offset, -1, start);
  const limit = source.indexOf(end, offset);
  assert.notEqual(limit, -1, end);
  return source.slice(offset, limit);
};
const handlers = [
  section('function modelCatalogContains(', 'function currentModelSupportsImages('),
  section('async function onModelRowClick(', '// modal 開啟時'),
  section("  bus.on('model_status',", "  bus.on('model_switched',"),
].join('\n');
function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
const tick = () => new Promise(resolve => setImmediate(resolve));
const apiModel = (connectionId = 'provider-A', modelId = 'model-next', extra = {}) => ({
  delivery: 'api', connectionId, modelId, name: modelId, ...extra,
});

function fixture({ fetchGate, bodyGate, catalogGate, modelGate, newThreadGate, realCatalog = false } = {}) {
  const calls = { fetch: [], models: [], catalog: 0, created: 0, toast: [], reconcile: 0, persisted: 0, retired: 0 };
  const pill = { textContent: 'Original model' };
  const callbacks = new Map();
  const context = vm.createContext({
    console: { error() {} },
    window: { MeadowCommercial: { requestModelAccess: () => true } },
    state: { sessionId: 'chat-A', modelConnectionID: 'provider-A', model: 'original' },
    access: true, accountGeneration: 1, attachmentAccountRevision: 0, attachmentSessionRevision: 0,
    modelConnectionSelection: 0, modelListRefreshing: false, modelFetched: true,
    modelFetchRevision: 0, modelFetchFailed: false,
    modelCache: [], modelLoading: null, modelLoadError: null, modelConfirmId: null,
    MODEL_CACHE_KEY: 'fixture-cache',
    hasClientAccess: () => context.access,
    clientAccountGeneration: () => context.accountGeneration,
    syncCommercialModelMode() {}, paintModelRows() {}, closeModelModal() {},
    modelModalOpen: () => false,
    localStorage: { setItem() { calls.persisted++; } }, cacheableModelList: rows => rows,
    configuredPickerRows, setTimeout, clearTimeout,
    clearRetiredSessionModel() { calls.retired++; },
    daemon: { modelList: async () => [apiModel('provider-A', 'late-model')] },
    $: () => pill,
    shortName: value => String(value).split('/').at(-1),
    displayName: model => model?.name || model?.modelId || '',
    selectedModelName: modelId => context.modelEntryFor(modelId)?.name || modelId,
    isStandardProductModel: () => true,
    reconcileImageCapability: () => { calls.reconcile++; },
    toast: (...args) => calls.toast.push(args),
    bus: { on: (event, handler) => callbacks.set(event, handler) },
    fetch: async (url, options) => {
      if (url === '/native/model-providers') {
        if (catalogGate) await catalogGate.promise;
        return { ok: true, json: async () => ({ mode: 'connections',
          selectedConnectionId: 'provider-A', selectedModelId: 'late-model',
          connections: [{ id: 'provider-A', models: [{ id: 'late-model' }] }],
        }) };
      }
      calls.fetch.push({ url, body: JSON.parse(options.body) });
      if (fetchGate) await fetchGate.promise;
      return { ok: true, json: async () => {
        if (bodyGate) await bodyGate.promise;
        return { activation: 'applied' };
      } };
    },
    refetchModels: async () => {
      calls.catalog++;
      if (catalogGate) await catalogGate.promise;
    },
    setModel: async (sessionId, modelId) => {
      calls.models.push({ sessionId, modelId });
      if (modelGate) await modelGate.promise;
    },
    startNewThread: async () => {
      calls.created++;
      if (newThreadGate) await newThreadGate.promise;
      context.state.sessionId = 'created-chat';
      context.state.modelConnectionID = 'provider-B';
      context.state.model = '';
      context.attachmentSessionRevision++;
      return { sessionId: 'created-chat', connectionId: 'provider-B' };
    },
  });
  vm.runInContext(handlers, context, { filename: 'composer-picker-fixture.js' });
  if (realCatalog) vm.runInContext(
    section('async function refetchModels(', 'function paintModelRows()'), context,
    { filename: 'composer-catalog-fixture.js' },
  );
  return {
    context, calls, pill,
    choose: model => context.onModelRowClick(model),
    status: value => callbacks.get('model_status')(value),
    switchChat(id = 'chat-B', connectionId = 'provider-A') {
      context.state.sessionId = id;
      context.state.modelConnectionID = connectionId;
      context.state.model = `${id}-model`;
      context.attachmentSessionRevision++;
      pill.textContent = `${id} model`;
    },
    changeAccount() {
      // Even an immediately authenticated replacement account is a new owner.
      // Production does not enter guest state or advance attachment revisions
      // for this transition; only Core's authoritative generation must change.
      context.accountGeneration++;
      context.state.model = 'replacement-account-model';
      pill.textContent = 'Replacement account';
    },
  };
}

test('held catalog response cannot set the model of a newly selected chat', async () => {
  const gate = deferred();
  const f = fixture({ catalogGate: gate });
  const pending = f.choose(apiModel());
  await tick();
  assert.equal(f.calls.catalog, 1);
  f.switchChat();
  gate.resolve();
  await pending;
  assert.deepEqual(f.calls.models, []);
  assert.equal(f.context.state.model, 'chat-B-model');
  assert.equal(f.pill.textContent, 'chat-B model');
  assert.deepEqual(f.calls.toast, []);
});

test('held session model response stays pinned and cannot overwrite the next chat UI', async () => {
  const gate = deferred();
  const f = fixture({ modelGate: gate });
  const pending = f.choose(apiModel());
  await tick();
  assert.deepEqual(f.calls.models, [{ sessionId: 'chat-A', modelId: 'model-next' }]);
  f.switchChat('chat-C');
  gate.resolve();
  await pending;
  assert.equal(f.context.state.model, 'chat-C-model');
  assert.equal(f.pill.textContent, 'chat-C model');
  assert.equal(f.calls.reconcile, 0);
  assert.deepEqual(f.calls.toast, []);
});

for (const stage of ['fetchGate', 'bodyGate', 'catalogGate', 'modelGate']) {
  test(`replacement account invalidates a model selection during ${stage}`, async () => {
    const gate = deferred();
    const f = fixture({ [stage]: gate });
    const pending = f.choose(apiModel());
    await tick();
    f.changeAccount();
    gate.resolve();
    await pending;
    assert.equal(f.context.state.model, 'replacement-account-model');
    assert.equal(f.pill.textContent, 'Replacement account');
    assert.equal(f.calls.models.length, stage === 'modelGate' ? 1 : 0);
    assert.equal(f.calls.reconcile, 0);
    assert.deepEqual(f.calls.toast, []);
  });
}

test('a deliberate provider change binds only its returned new conversation', async () => {
  const f = fixture();
  await f.choose(apiModel('provider-B'));
  assert.equal(f.calls.created, 1);
  assert.deepEqual(f.calls.models, [{ sessionId: 'created-chat', modelId: 'model-next' }]);
  assert.equal(f.context.state.modelConnectionID, 'provider-B');
  assert.equal(f.context.state.model, 'model-next');
  assert.equal(f.calls.toast.length, 1);
});

test('a chat switch during deliberate creation invalidates the old picker intent', async () => {
  const gate = deferred();
  const f = fixture({ newThreadGate: gate });
  const pending = f.choose(apiModel('provider-B'));
  await tick();
  f.switchChat('manually-selected-chat');
  gate.resolve();
  await pending;
  assert.deepEqual(f.calls.models, []);
  assert.equal(f.calls.catalog, 0);
  assert.deepEqual(f.calls.toast, []);
});

test('late model failure does not show an error in another chat', async () => {
  const gate = deferred();
  const f = fixture({ modelGate: gate });
  const pending = f.choose(apiModel());
  await tick();
  f.switchChat();
  gate.reject(new Error('fixture transport failed'));
  await pending;
  assert.deepEqual(f.calls.toast, []);
});

test('signed-out picker action has no native side effects', async () => {
  const f = fixture();
  f.context.access = false;
  await f.choose(apiModel());
  assert.deepEqual(f.calls.fetch, []);
  assert.deepEqual(f.calls.models, []);
});

test('same model ID on providers A and B has exactly one live badge, without rebinding A', () => {
  const f = fixture();
  f.context.state.model = 'shared';
  f.context.modelCache = [
    apiModel('provider-A', 'shared', { name: 'A model', live: true, active: true }),
    apiModel('provider-B', 'shared', { name: 'B model', live: false, active: false }),
  ];
  f.status({ status: 'ready', connectionId: 'provider-B', model: 'shared' });
  assert.deepEqual(Array.from(f.context.modelCache, row => [row.connectionId, row.live, row.active]), [
    ['provider-A', false, false], ['provider-B', true, true],
  ]);
  assert.equal(f.context.state.modelConnectionID, 'provider-A');
  assert.equal(f.context.state.model, 'shared');
  assert.equal(f.pill.textContent, 'Original model');
  f.status({ status: 'ready', connectionId: 'provider-A', model: 'shared' });
  assert.equal(f.pill.textContent, 'A model');
});

test('API lookup and resident status preserve exact case and namespace within a connection', () => {
  const f = fixture();
  const rows = [
    apiModel('provider-A', 'org-a/shared', { multimodal: false }),
    apiModel('provider-A', 'org-b/shared', { multimodal: true }),
    apiModel('provider-A', 'Org-B/shared', { multimodal: false }),
    apiModel('provider-B', 'org-b/shared', { multimodal: false }),
  ];
  f.context.modelCache = rows;
  assert.equal(f.context.modelEntryFor('org-b/shared'), rows[1]);
  assert.equal(f.context.modelEntryFor('Org-B/shared'), rows[2]);
  assert.equal(f.context.modelEntryFor('shared'), null);
  assert.equal(f.context.modelEntryFor('ORG-B/shared'), null);
  assert.equal(f.context.modelCatalogContains('shared'), false);
  assert.equal(f.context.modelCatalogContains('ORG-B/shared'), false);
  assert.equal(f.context.modelCatalogContains('org-b/shared'), true);
  f.status({ status: 'ready', connectionId: 'provider-A', model: 'org-b/shared' });
  assert.deepEqual(Array.from(f.context.modelCache, row => row.live), [false, true, false, false]);
  f.context.state.modelConnectionID = 'provider-B';
  assert.equal(f.context.modelEntryFor('org-b/shared').connectionId, 'provider-B');
  f.context.state.modelConnectionID = '';
  assert.equal(f.context.modelEntryFor('org-b/shared'), null);
  assert.equal(f.context.modelCatalogContains('org-b/shared'), false);
});

test('legacy Core aliases remain compatible only in an unbound Core conversation', () => {
  const f = fixture();
  const legacy = { modelId: '/models/Meadow-35B', name: 'Meadow 35B' };
  f.context.modelCache = [legacy];
  f.context.state.modelConnectionID = '';
  assert.equal(f.context.modelEntryFor('meadow-35b'), legacy);
  assert.equal(f.context.modelCatalogContains('meadow-35b'), true);
  f.status({ status: 'ready', model: 'MEADOW-35B' });
  assert.equal(f.context.modelCache[0].live, true);
  f.context.state.modelConnectionID = 'provider-A';
  assert.equal(f.context.modelEntryFor('meadow-35b'), null);
  assert.equal(f.context.modelCatalogContains('meadow-35b'), false);
});

for (const ownerChange of ['chat', 'account', 'connection']) {
  test(`actual catalog publisher ignores a late partial join after ${ownerChange} changes`, async () => {
    const gate = deferred();
    const f = fixture({ catalogGate: gate, realCatalog: true });
    const original = [apiModel('provider-A', 'original')];
    f.context.modelCache = original;
    // The model-list half completes before the owner changes; the native
    // catalog half arrives afterward, exercising the actual allSettled join.
    const pending = f.context.refetchModels();
    await tick();
    if (ownerChange === 'chat') f.switchChat();
    else if (ownerChange === 'account') f.changeAccount();
    else f.context.state.modelConnectionID = 'provider-B';
    const expectedPill = f.pill.textContent;
    gate.resolve();
    assert.equal(await pending, null);
    assert.equal(f.context.modelCache, original);
    assert.equal(f.pill.textContent, expectedPill);
    assert.equal(f.calls.persisted, 0);
    assert.equal(f.calls.retired, 0);
    assert.equal(f.context.modelListRefreshing, false);
  });
}

test('actual catalog publisher still accepts a current owner response', async () => {
  const f = fixture({ realCatalog: true });
  await f.context.refetchModels();
  assert.equal(f.context.modelCache[0].modelId, 'late-model');
  assert.equal(f.calls.persisted, 1);
  assert.equal(f.calls.retired, 1);
  assert.equal(f.context.modelListRefreshing, false);
});

test('authenticated replacement with no session discards an already fulfilled old-account catalog half', async () => {
  const gate = deferred();
  const f = fixture({ catalogGate: gate, realCatalog: true });
  f.context.state.sessionId = null;
  f.context.state.modelConnectionID = '';
  const original = [];
  f.context.modelCache = original;
  const pending = f.context.refetchModels();
  await tick();
  f.changeAccount();
  assert.equal(f.context.access, true);
  assert.equal(f.context.attachmentAccountRevision, 0);
  assert.equal(f.context.attachmentSessionRevision, 0);
  // The transport guard rejects the still-pending native catalog half, while
  // allSettled retains the already fulfilled old-account modelList branch.
  gate.reject(new Error('fixture account changed'));
  assert.equal(await pending, null);
  assert.equal(f.context.modelCache, original);
  assert.equal(f.calls.persisted, 0);
  assert.equal(f.calls.retired, 0);
  assert.equal(f.pill.textContent, 'Replacement account');
});
