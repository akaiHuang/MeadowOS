import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { webcrypto } from 'node:crypto';
import { JSDOM } from 'jsdom';

// Exercise the real attachment and input functions. Browser/file APIs are
// controlled fixtures; no native app, filesystem picker, or model is called.
const source = readFileSync(new URL('./composer.js', import.meta.url), 'utf8')
  .split('// ---------- popover plumbing')[0]
  .replace(/^import[\s\S]*?from '[^']+';\n/gm, '');

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

class Surface {
  listeners = new Map();
  addEventListener(name, callback) {
    const entries = this.listeners.get(name) || [];
    entries.push(callback);
    this.listeners.set(name, entries);
  }
  dispatchEvent(event) {
    for (const callback of this.listeners.get(event.type) || []) callback(event);
  }
}

class Node extends Surface {
  constructor() {
    super();
    this.children = [];
    this.value = '';
    this.style = {};
    this.scrollHeight = 20;
    this.classList = { add() {}, remove() {}, toggle() {} };
  }
  set innerHTML(value) { this.children = []; this.html = value; }
  get innerHTML() { return this.html || ''; }
  appendChild(node) { this.children.push(node); return node; }
  setAttribute() {}
  focus() {}
  click() { this.dispatchEvent({ type: 'click' }); }
}

function harness({ authenticated = true, authorizePrompt, promptFn, queueFn,
  runOptions = {}, reasoningEffort = 'medium', researchMode = false,
  budgetPreset, harnessMode = 'evo', inferenceRead = null } = {}) {
  const preferences = new Map([['meadow.evoTokenBudget.v1', budgetPreset || 'effort']]);
  const limitsContext = { localStorage: {
    getItem: key => preferences.get(key) || null,
    setItem: (key, value) => preferences.set(key, value),
  } };
  vm.runInNewContext(readFileSync(new URL('./run_limits.js', import.meta.url), 'utf8').replace(/^export /gm, '') +
    '\nglobalThis.api = { runLimitOptions, snapshotRunLimitPreferences, setRunLimitExecutionMode };', limitsContext);
  limitsContext.api.setRunLimitExecutionMode(harnessMode);
  let access = authenticated;
  let accountGeneration = 0;
  let capability = Promise.resolve(true);
  let fetchGate = null;
  let editionGate = null;
  let controlReads = 0;
  const reads = [];
  const fetches = [];
  const messages = [];
  const native = new Map();
  const nodes = new Map();
  for (const id of ['composer', 'input', 'imageAttachInput', 'attachImageBtn', 'attachStrip', 'sendBtn', 'stopBtn', 'taskQueue']) {
    nodes.set(`#${id}`, new Node());
  }
  const window = new Surface();
  window.wails = { Events: { On: (name, callback) => native.set(name, callback) } };
  window.matchMedia = () => ({ matches: false });
  const document = new Surface();
  document.querySelector = selector => nodes.get(selector) || null;
  document.createElement = () => new Node();
  // Busy reflection now paints the canonical activity indicator in the shell.
  // Use a real root element while retaining the focused attachment fixtures.
  const activityDocument = new JSDOM('<html></html>').window.document;
  document.documentElement = activityDocument.documentElement;
  document.getElementById = id => nodes.get('#'+id) || null;
  const bus = new Surface();
  bus.on = bus.addEventListener.bind(bus);
  bus.emit = (name, data) => {
    for (const callback of bus.listeners.get(name) || []) callback(data);
  };
  class Reader {
    readAsDataURL(file) { this.file = file; reads.push(this); }
    abort() { this.aborted = true; this.onabort?.(); }
    complete(result) { this.result = result; this.onload?.(); }
  }
  const context = {
    console, window, document, bus, crypto: webcrypto,
    performance: { now: (() => { let now = 0; return () => now += 1000; })() },
    state: { sessionId: 'same-session', model: 'vision', cwd: '/fixture', researchMode },
    hasClientAccess: () => access,
    clientAccountGeneration: () => accountGeneration,
    documentMediaType: name => /\.txt$/.test(name || '') ? 'text/plain' : '',
    verifyImageCapability: () => capability,
    currentModelSupportsImages: () => true,
    FileReader: Reader, File: class {}, Blob,
    location: { protocol: 'https:' },
    toast: (...args) => messages.push(args),
    fetch: url => {
      fetches.push(url);
      return fetchGate?.promise || Promise.reject(new Error('unexpected fetch'));
    },
    daemon: { switchEdition: () => editionGate.promise,
      prompt: promptFn || (async () => ({ stopReason: 'end_turn' })),
      queueAdd: queueFn || (async () => { throw new Error('unexpected queue delivery'); }) },
    ensureHarnessControlsReady: async () => ({ effectiveMode: harnessMode }),
    authorizePromptDelivery: authorizePrompt || (async () => null),
    consumePromptDelivery: async () => {},
    sweepResolvedCards() {},
    inferenceControls: inferenceRead ? null : { maturity: 'ready', reasoningEffort },
    loadInferenceControls: async () => { context.inferenceControls = await inferenceRead(); },
    snapshotRunLimitPreferences: limitsContext.api.snapshotRunLimitPreferences,
    runLimitOptions: (effort, captured, mode) => {
      controlReads++;
      if (budgetPreset !== undefined) return limitsContext.api.runLimitOptions(effort, captured, mode);
      return typeof runOptions === 'function' ? runOptions(effort) : { ...runOptions };
    },
    renderComposerQueue() {},
  };
  vm.runInNewContext(source + `\ncommitApprovalMode = async () => true;
    initAttachments(); initInput();
    globalThis.api = { addImageFile, addPDFFile, addDocumentFile,
      addNativeDroppedImage, addNativeDroppedPDF, addNativeDroppedDocument,
      restoreAttachments, fileDataURL,
      attachments: () => attachState.slice(), owners: () => nativeDropOwners.size };`, context);
  return {
    ...context.api, nodes, native, window, bus, reads, fetches, messages,
    setAccess(value) { access = value; },
    changeAccount(value, event = 'commercial:change') {
      access = value;
      accountGeneration++;
      if (event === 'authentication_required') bus.emit(event, {});
      else window.dispatchEvent({ type: event, detail: { authenticated: true } });
    },
    setCapability(value) { capability = value; },
    delayFetch() { fetchGate = deferred(); return fetchGate; },
    delayEdition() { editionGate = deferred(); return editionGate; },
    controlReads: () => controlReads,
    changeEffort(value) { context.inferenceControls.reasoningEffort = value; },
    changeBudget(value) { preferences.set('meadow.evoTokenBudget.v1', value); },
    changeHarnessMode(value) { harnessMode = value; limitsContext.api.setRunLimitExecutionMode(value); },
    changeResearchMode(value) { context.state.researchMode = value; },
    setBusy(value) {
      bus.emit('run_state', { sessionId: context.state.sessionId,
        status: value ? 'running' : 'idle' });
    },
    emit(name, data) { native.get(name)?.({ data }); },
  };
}

const flush = async () => { for (let i = 0; i < 8; i++) await new Promise(setImmediate); };
const snapshot = value => JSON.parse(JSON.stringify(value));

for (const [kind, preset, expected] of [['send', 'extended', 2000000], ['resend', 'generous', 3000000]]) {
  test(`${kind} pins explicit Evo budget across delayed inference controls and setting edits`, async () => {
    const inference = deferred(); const deliveries = [];
    const h = harness({ budgetPreset: preset, inferenceRead: () => inference.promise,
      promptFn: async (...args) => { deliveries.push(snapshot(args)); return { stopReason: 'end_turn' }; } });
    if (kind === 'send') {
      h.nodes.get('#input').value = 'finish the timer'; h.window.__meadowSubmitComposer();
    } else h.bus.emit('message:resend', { sessionId: 'same-session', text: 'finish the timer' });
    await flush(); assert.equal(deliveries.length, 0);
    h.changeBudget(preset === 'extended' ? 'generous' : 'extended');
    inference.resolve({ maturity: 'ready', reasoningEffort: 'max' });
    await flush(); assert.equal(deliveries.length, 1);
    assert.equal(deliveries[0][3].runTokenLimit, expected);
  });
}

test('ordinary mode with an Evo preference keeps normal Max after inference read', async () => {
  const inference = deferred(); const deliveries = [];
  const h = harness({ budgetPreset: 'generous', harnessMode: 'codex-style', inferenceRead: () => inference.promise,
    promptFn: async (...args) => { deliveries.push(snapshot(args)); return { stopReason: 'end_turn' }; } });
  h.nodes.get('#input').value = 'ordinary run'; h.window.__meadowSubmitComposer();
  inference.resolve({ maturity: 'ready', reasoningEffort: 'max' }); await flush();
  assert.equal(deliveries[0][3].runTokenLimit, 1000000);
});

test('explicit Evo reflection retry preserves ordinary authorization and sends only opaque recovery metadata', async () => {
  const authorization = deferred(); const deliveries = []; const requests = []; const settled = [];
  const h = harness({ budgetPreset: 'generous', reasoningEffort: 'max',
    authorizePrompt: (sid, id) => { requests.push({ sid, id }); return authorization.promise; },
    promptFn: async (...args) => { deliveries.push(snapshot(args)); return { stopReason: 'end_turn' }; } });
  h.bus.on('evo:reflection-retry-settled', (value) => settled.push(value));
  h.bus.emit('evo:retry-reflection', { sessionId: 'other', runId: 'task-123' });
  h.bus.emit('evo:retry-reflection', { sessionId: 'same-session', runId: '../task-123' });
  await flush(); assert.equal(requests.length, 0);
  h.bus.emit('evo:retry-reflection', { sessionId: 'same-session', runId: 'task-123' });
  await flush(); assert.equal(requests.length, 1); assert.equal(deliveries.length, 0);
  authorization.resolve({ requestID: requests[0].id }); await flush();
  assert.equal(deliveries.length, 1);
  assert.equal(deliveries[0][3].evoResumeReflectionRunId, 'task-123');
  assert.equal(deliveries[0][3].runTokenLimit, 3000000);
  assert.equal(deliveries[0][4].requestID, requests[0].id);
  assert.equal(settled.length, 1);
});

test('Evo queue snapshots its clicked budget before authorization and later preferences', async () => {
  const authorization = deferred(); const queued = [];
  const h = harness({ budgetPreset: 'extended', authorizePrompt: () => authorization.promise,
    queueFn: async (...args) => { queued.push(snapshot(args)); return { item: { id: 'q' }, items: [{ id: 'q' }] }; } });
  h.setBusy(true); h.nodes.get('#input').value = 'next timer'; h.window.__meadowSubmitComposer();
  await flush(); h.changeBudget('generous'); authorization.resolve(null); await flush();
  assert.equal(queued.length, 1); assert.equal(queued[0][3].runTokenLimit, 2000000);
});

test('Evo lost-response retry preserves original explicit budget and request identity', async () => {
  const deliveries = [];
  const h = harness({ budgetPreset: 'extended', authorizePrompt: async (_sid, id) => ({ requestID: id }),
    promptFn: async (...args) => { deliveries.push(snapshot(args)); if (deliveries.length === 1) throw new Error('lost response'); return { stopReason: 'end_turn' }; } });
  h.nodes.get('#input').value = 'same Evo request'; h.window.__meadowSubmitComposer(); await flush();
  h.changeBudget('generous'); h.changeEffort('minimal'); h.window.__meadowSubmitComposer(); await flush();
  assert.equal(deliveries.length, 2);
  assert.equal(deliveries[0][3].runTokenLimit, 2000000);
  assert.deepEqual(deliveries[1], deliveries[0]);
});

test('lost authorization response preserves a restored submission ID; editing creates a fresh send', async () => {
  const requests = [];
  const h = harness({ authorizePrompt: async (_sid, id) => {
    requests.push(id);
    if (requests.length === 1) throw new TypeError('synthetic lost response');
    return { requestID: id };
  } });
  h.nodes.get('#input').value = 'same words';
  h.window.__meadowSubmitComposer();
  await flush();
  assert.equal(h.nodes.get('#input').value, 'same words');
  h.window.__meadowSubmitComposer();
  await flush();
  assert.equal(requests.length, 2);
  assert.equal(requests[1], requests[0], 'retry must recover the grant already accepted by the server');
  h.nodes.get('#input').value = 'same words';
  h.nodes.get('#input').dispatchEvent({ type: 'input' });
  h.window.__meadowSubmitComposer();
  await flush();
  assert.equal(requests.length, 3);
  assert.notEqual(requests[2], requests[1], 'a new user submission may have identical text');
});

test('lost prompt response retries the original options, attachments and identity despite changed controls', async () => {
  const requests = [];
  const deliveries = [];
  let timeout = 300000;
  const h = harness({ reasoningEffort: 'high', researchMode: true,
    runOptions: effort => ({ runTimeoutMs: timeout,
      runTokenLimit: effort === 'high' ? 750000 : 1000000 }),
    authorizePrompt: async (_sid, id) => { requests.push(id); return { requestID: id }; },
    promptFn: async (...args) => {
      deliveries.push(snapshot(args));
      if (deliveries.length === 1) throw new TypeError('synthetic lost prompt response');
      return { stopReason: 'end_turn' };
    },
  });
  h.restoreAttachments([
    { kind: 'document', name: 'context.txt', dataURL: 'data:text/plain;base64,YQ==' },
    'data:image/png;base64,AA==']);
  h.nodes.get('#input').value = 'keep the original request';
  h.window.__meadowSubmitComposer();
  await flush();
  assert.equal(deliveries.length, 1);
  assert.deepEqual(deliveries[0][3], { runTimeoutMs: 300000, runTokenLimit: 750000, mode: 'research' });
  assert.equal(deliveries[0][2].length, 2);
  assert.equal(h.nodes.get('#input').value, 'keep the original request');
  assert.equal(h.attachments().length, 2);

  timeout = 1200000;
  h.changeEffort('max');
  h.changeResearchMode(false);
  h.setBusy(true);
  h.window.__meadowSubmitComposer();
  await flush();
  assert.equal(requests.length, 1, 'an unresolved prompt retry cannot authorize or become a queue item while busy');
  assert.equal(deliveries.length, 1);
  assert.equal(h.nodes.get('#input').value, 'keep the original request');
  assert.ok(h.messages.some(([message]) => message.includes('前次訊息回執尚未確認')));

  h.setBusy(false);
  h.window.__meadowSubmitComposer();
  await flush();
  assert.equal(requests.length, 2);
  assert.equal(requests[1], requests[0]);
  assert.equal(deliveries.length, 2);
  assert.deepEqual(deliveries[1], deliveries[0], 'the retried delivery must preserve all original request arguments');
  assert.equal(h.nodes.get('#input').value, '');
  assert.equal(h.attachments().length, 0);
});

test('lost queue response still retries queue when idle with original options, attachments and identity', async () => {
  const requests = [];
  const queued = [];
  let prompts = 0;
  let timeout = 600000;
  const h = harness({ reasoningEffort: 'high', researchMode: false,
    runOptions: effort => ({ runTimeoutMs: timeout,
      runTokenLimit: effort === 'high' ? 750000 : 1000000 }),
    authorizePrompt: async (_sid, id) => { requests.push(id); return { requestID: id }; },
    promptFn: async () => { prompts++; return { stopReason: 'end_turn' }; },
    queueFn: async (...args) => {
      queued.push(snapshot(args));
      if (queued.length === 1) throw new TypeError('synthetic lost queue response');
      const item = { id: 'same-queue-item', status: 'pending' };
      return { item, items: [item] };
    },
  });
  h.setBusy(true);
  h.restoreAttachments([{ kind: 'pdf', path: '/private/context.pdf', name: 'context.pdf' }]);
  h.nodes.get('#input').value = 'queue this original request';
  h.window.__meadowSubmitComposer();
  await flush();
  assert.equal(queued.length, 1);
  assert.deepEqual(queued[0][3], { runTimeoutMs: 600000, runTokenLimit: 750000, mode: 'auto' });
  assert.equal(queued[0][2].length, 1);
  assert.equal(h.nodes.get('#input').value, 'queue this original request');

  timeout = 1200000;
  h.changeEffort('max');
  h.changeResearchMode(true);
  h.setBusy(false);
  h.window.__meadowSubmitComposer();
  await flush();
  assert.equal(requests.length, 2);
  assert.equal(requests[1], requests[0]);
  assert.equal(queued.length, 2);
  assert.deepEqual(queued[1], queued[0], 'idle state cannot change the original queue request or its frozen payload');
  assert.equal(prompts, 0);
  assert.equal(h.nodes.get('#input').value, '');
  assert.equal(h.attachments().length, 0);
});

const pdf = { type: 'application/pdf', name: 'evidence.pdf', size: 12 };
const documentFile = { type: 'text/plain', name: 'evidence.txt', size: 12 };
const imageFile = { type: 'image/png', name: 'evidence.png', size: 12 };
const nativePayload = id => ({
  drop_id: id,
  pdfs: [{ path: '/private/evidence.pdf', name: 'evidence.pdf' }],
  documents: [{ path: '/private/evidence.txt', name: 'evidence.txt' }],
});

test('guest file ingress does no file reads, data fetches, attachment or submit work', async () => {
  const h = harness({ authenticated: false });
  await h.addImageFile(imageFile);
  await h.addPDFFile(pdf);
  await h.addDocumentFile(documentFile);
  await h.addNativeDroppedImage({ url: 'data:image/png;base64,AA==' });
  h.addNativeDroppedPDF(nativePayload('guest').pdfs[0]);
  h.addNativeDroppedDocument(nativePayload('guest').documents[0]);
  h.restoreAttachments(['data:image/png;base64,AA==']);
  h.emit('meadow:files-drop-started', { drop_id: 'guest' });
  h.emit('meadow:image-files-dropped', nativePayload('guest'));
  await assert.rejects(h.fileDataURL(pdf));
  h.nodes.get('#input').value = 'keep my unsent words';
  h.window.__meadowSubmitComposer();
  h.emit('meadow:composer-submit', {});
  await Promise.resolve();
  assert.equal(h.nodes.get('#input').value, 'keep my unsent words');
  assert.equal(h.reads.length, 0);
  assert.equal(h.fetches.length, 0);
  assert.equal(h.attachments().length, 0);
  assert.equal(h.owners(), 0);
  assert.equal(h.controlReads(), 0);
});

test('admitted native drop requires its start and consumes ownership once', () => {
  const h = harness();
  h.emit('meadow:image-files-dropped', nativePayload('untracked'));
  h.emit('meadow:image-files-dropped', { pdfs: nativePayload('').pdfs });
  assert.equal(h.attachments().length, 0);
  h.emit('meadow:files-drop-started', { drop_id: 'admitted' });
  h.emit('meadow:image-files-dropped', nativePayload('admitted'));
  h.emit('meadow:image-files-dropped', nativePayload('admitted'));
  assert.equal(h.attachments().length, 2);
  assert.equal(h.owners(), 0);
});

test('a current authorized document read still retains its original bytes and name', async () => {
  const h = harness();
  const pending = h.addDocumentFile(documentFile);
  h.reads[0].complete('data:text/plain;base64,YQ==');
  await pending;
  assert.equal(h.attachments().length, 1);
  assert.equal(h.attachments()[0].dataURL, 'data:text/plain;base64,YQ==');
  assert.equal(h.attachments()[0].name, 'evidence.txt');
});

for (const { event, authenticated, label } of [
  { event: 'commercial:change', authenticated: false, label: 'commercial:change' },
  { event: 'authentication_required', authenticated: false, label: 'authentication_required' },
  { event: 'commercial:change', authenticated: true, label: 'direct authenticated account replacement' },
]) {
  test(`${label} clears attachment readers and owners while preserving typed draft`, async () => {
    const h = harness();
    h.nodes.get('#input').value = 'human draft remains';
    h.addNativeDroppedPDF(nativePayload('').pdfs[0]);
    h.emit('meadow:files-drop-started', { drop_id: 'old' });
    const pendingRead = h.addDocumentFile(documentFile);
    assert.equal(h.reads.length, 1);
    h.changeAccount(authenticated, event);
    assert.equal(h.reads[0].aborted, true);
    assert.equal(h.attachments().length, 0);
    assert.equal(h.owners(), 0);
    assert.equal(h.nodes.get('#input').value, 'human draft remains');
    h.changeAccount(true);
    h.reads[0].complete('data:text/plain;base64,YQ==');
    h.emit('meadow:image-files-dropped', nativePayload('old'));
    await pendingRead;
    assert.equal(h.attachments().length, 0);
    assert.equal(h.messages.length, 0);
    h.emit('meadow:files-drop-started', { drop_id: 'new' });
    h.emit('meadow:image-files-dropped', nativePayload('new'));
    assert.equal(h.attachments().length, 2);
  });
}

test('old image capability result cannot start a file read after another login', async () => {
  const h = harness();
  const gate = deferred();
  h.setCapability(gate.promise);
  const pending = h.addImageFile(imageFile);
  h.changeAccount(false);
  h.changeAccount(true);
  gate.resolve(true);
  await pending;
  assert.equal(h.reads.length, 0);
  assert.equal(h.attachments().length, 0);
});

test('late native image fetch cannot read its body after logout and reauthentication', async () => {
  const h = harness();
  const gate = h.delayFetch();
  let bodyReads = 0;
  const pending = h.addNativeDroppedImage({ url: 'data:image/png;base64,AA==' });
  h.changeAccount(false);
  h.changeAccount(true);
  gate.resolve({ blob: async () => { bodyReads++; return {}; } });
  await pending;
  assert.equal(bodyReads, 0);
  assert.equal(h.reads.length, 0);
  assert.equal(h.attachments().length, 0);
});

test('an old edition failure cannot overwrite a draft typed after account recovery', async () => {
  const h = harness();
  const gate = h.delayEdition();
  h.nodes.get('#input').value = '/badai';
  h.window.__meadowSubmitComposer();
  assert.equal(h.nodes.get('#input').value, '');
  h.changeAccount(false);
  h.changeAccount(true);
  h.nodes.get('#input').value = 'new human draft';
  gate.reject(new Error('previous edition operation failed'));
  await new Promise(setImmediate);
  assert.equal(h.nodes.get('#input').value, 'new human draft');
});
