import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

// Controlled DOM/HTTP/event fixtures. These tests execute the shipped module,
// but do not claim real browser layout, Wails, provider or installed-app proof.
const source = readFileSync(new URL('./harness_controls.js', import.meta.url), 'utf8');
const flush = async () => { await new Promise(setImmediate); await new Promise(setImmediate); };

class Element {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.handlers = new Map();
    this.hidden = false;
    this.textContent = '';
    this._value = '';
    this.selectedIndex = -1;
  }
  append(...children) { this.children.push(...children); }
  prepend(...children) { this.children.unshift(...children); }
  replaceChildren(...children) { this.children = children; }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, callback) {
    this.handlers.set(name, [...(this.handlers.get(name) || []), callback]);
  }
  fire(name) { for (const callback of this.handlers.get(name) || []) callback({ target: this }); }
  set value(value) {
    if (this.tagName === 'select') {
      this.selectedIndex = this.children.findIndex((option) => option.value === value);
      this._value = this.selectedIndex < 0 ? '' : value;
    } else this._value = value;
  }
  get value() { return this._value; }
}

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

function setup({ evo = true, evoStatus = async () => ({}) } = {}) {
  const nodes = [];
  const composer = new Element('div');
  composer.id = 'composerWrap';
  const handlers = new Map();
  const posts = [];
  const runLimitAvailability = [];
  const executionModes = [];
  const emitted = [];
  const state = { sessionId: 'one' };
  let selectedMode = '';
  let admission;
  const controls = (sessionId) => ({
    sessionId, modelId: 'fixture-model', selectedMode,
    effectiveMode: selectedMode || (evo ? 'evo' : 'codex-style'),
    activeMode: '', running: false, appliesNextRun: true, preferenceStatus: 'default',
    modes: [{ id: '', label: 'Meadow 預設' }, { id: 'codex-style', label: 'Meadow Coding' },
      ...(evo ? [{ id: 'evo', label: 'Meadow Evo' }] : [])],
  });
  const context = {
    setTimeout, clearTimeout, console, state,
    document: {
      createElement(tag) { const element = new Element(tag); nodes.push(element); return element; },
      getElementById(id) { return id === composer.id ? composer : nodes.find((node) => node.id === id); },
    },
    daemon: {
      harnessControls: async (sid) => controls(sid),
      setHarnessControls: async (sid, body) => {
        posts.push({ sessionId: sid, mode: body.mode });
        selectedMode = body.mode;
        return controls(sid);
      },
      evoStatus,
    },
    bus: {
      on(name, callback) { handlers.set(name, [...(handlers.get(name) || []), callback]); },
      emit(name, value) { emitted.push({ name, value }); },
    },
    registerPromptAdmissionCheck(check) { admission = check; },
    setEvoRunLimitAvailability(available) { runLimitAvailability.push(available); },
    setRunLimitExecutionMode(mode) { executionModes.push(mode); },
  };
  vm.runInNewContext(source.replace(/^import .*;\n/gm, '').replace(/^export /gm, '') + '\ninitHarnessControls();', context);
  return {
    state, composer, nodes, posts, runLimitAvailability, executionModes, emitted,
    panel: () => nodes.find((node) => node.id === 'evoControls'),
    select: () => nodes.find((node) => node.tagName === 'select'),
    status: () => nodes.find((node) => node.attributes.role === 'status'),
    open: () => nodes.find((node) => node.tagName === 'button'),
    retry: () => nodes.find((node) => node.tagName === 'button' && node.textContent === '重試回顧'),
    emit(name, value) { for (const callback of handlers.get(name) || []) callback(value); },
    admission: (sid) => admission(sid),
  };
}

test('native Evo default is a visible valid selection; explicit choices persist', async () => {
  const h = setup();
  await flush();
  const select = h.select();
  assert.ok(h.panel());
  assert.equal(h.panel().hidden, false);
  assert.equal(h.composer.children[0], h.panel());
  assert.equal(select.value, '');
  assert.notEqual(select.selectedIndex, -1, 'default must match a real option, not an empty native select');
  assert.match(select.children[select.selectedIndex].textContent, /Evo/);
  assert.equal((await h.admission('one')).effectiveMode, 'evo');
  assert.equal(h.posts.length, 0, 'rendering or admission must not change mode');
  assert.equal(h.runLimitAvailability.at(-1), true, 'confirmed Evo exposes the explicit long-task preset');
  assert.equal(h.executionModes.at(-1), 'evo');
  for (const mode of ['codex-style', 'evo', '']) {
    select.value = mode;
    select.fire('change');
    await flush();
    assert.equal(h.posts.at(-1).mode, mode);
    assert.equal(select.value, mode);
    assert.notEqual(select.selectedIndex, -1);
    assert.equal(select.disabled, false);
    assert.equal(h.executionModes.at(-1), mode || 'evo');
  }
});

test('reflection recovery is explicit, scoped, single click and retryable after admission failure', async () => {
  const h = setup();
  await flush();
  h.emit('evo_state', { sessionId: 'one', runId: 'task-123', status: 'incomplete', phase: 'ended', reflectionRecoveryAvailable: true });
  assert.equal(h.retry().hidden, false);
  assert.match(h.status().textContent, /回顧待恢復/);
  h.retry().fire('click');
  h.retry().fire('click');
  assert.equal(h.emitted.length, 1);
  assert.equal(h.emitted[0].name, 'evo:retry-reflection');
  assert.equal(h.emitted[0].value.runId, 'task-123');
  h.emit('evo:reflection-retry-settled', { sessionId: 'other', runId: 'task-123' });
  assert.equal(h.retry().disabled, true);
  h.emit('evo:reflection-retry-settled', { sessionId: 'one', runId: 'task-123' });
  assert.equal(h.retry().disabled, false);
  h.emit('evo_state', { sessionId: 'one', runId: 'task-456', status: 'running', phase: 'reflection' });
  assert.equal(h.retry().hidden, true);
  h.emit('evo_state', { sessionId: 'one', runId: 'task-456', status: 'completed', phase: 'ended', reflectionCompleted: true });
  assert.equal(h.retry().hidden, true);
});

test('a host without Evo cannot expose the Evo-only long-task preset', async () => {
  const h = setup({ evo: false });
  await flush();
  assert.equal(h.runLimitAvailability.at(-1), false);
});

test('an old same-session GET cannot replace a newer running SSE with a previous result', async () => {
  const pending = [];
  const h = setup({ evoStatus: () => { const read = deferred(); pending.push(read); return read.promise; } });
  await flush();
  assert.ok(pending.length > 0, 'fixture must hold a real read from the production module');
  h.emit('evo_state', { sessionId: 'one', runId: 'new-run', status: 'running', phase: 'executor', stage: 2 });
  assert.match(h.status().textContent, /執行中/);
  assert.equal(h.status().dataset.running, 'true');
  for (const read of pending) {
    read.resolve({ sessionId: 'one', runId: 'old-run', status: 'completed', phase: 'ended', resultWorkspace: '/owned-fixture/old-result' });
  }
  await flush();
  assert.match(h.status().textContent, /執行中/);
  assert.match(h.status().textContent, /第 2 階段/);
  assert.equal(h.status().dataset.running, 'true');
  assert.equal(h.open().hidden, true, 'old result action must remain hidden during the new run');
});

test('foreign-session Evo SSE cannot replace selected-session progress', async () => {
  const h = setup();
  await flush();
  h.emit('evo_state', { sessionId: 'one', runId: 'one-run', status: 'running', phase: 'executor', stage: 1 });
  h.emit('evo_state', { sessionId: 'other', runId: 'foreign-run', status: 'completed', resultWorkspace: '/foreign/result' });
  assert.match(h.status().textContent, /執行中/);
  assert.equal(h.open().hidden, true);
});

test('two active Evo stages are visible and clear when the pair drains', async () => {
  const h = setup(); await flush();
  h.emit('evo_state', { sessionId: 'one', runId: 'pair', status: 'running', phase: 'improver', stage: 3, activeStages: ['inv1.a1', 'dom1.a1'] });
  assert.match(h.status().textContent, /同時處理 2 個階段/);
  assert.equal(h.open().hidden, true);
  h.emit('evo_state', { sessionId: 'one', runId: 'pair', status: 'running', phase: 'improver', stage: 3, activeStages: ['dom1.a1'] });
  assert.doesNotMatch(h.status().textContent, /同時處理/);
  h.emit('evo_state', { sessionId: 'one', runId: 'pair', status: 'completed', phase: 'ended', stage: 8, activeStages: [], resultWorkspace: '/selected/result' });
  assert.doesNotMatch(h.status().textContent, /同時處理/);
  assert.equal(h.open().hidden, false);
});

test('exhausted Evo flow budget is visibly incomplete and never offers a result', async () => {
  const h = setup(); await flush();
  h.emit('evo_state', { sessionId: 'one', runId: 'budget-run', status: 'failed',
    phase: 'ended', stopReason: 'token_budget', resultWorkspace: '/old/non-adopted' });
  assert.equal(h.status().textContent, 'Evo · 流程預算已用完，尚未完成');
  assert.equal(h.status().dataset.running, 'false');
  assert.equal(h.open().hidden, true);
  h.emit('evo_state', { sessionId: 'one', runId: 'io-run', status: 'failed', phase: 'ended', error: 'IO failure' });
  assert.doesNotMatch(h.status().textContent, /預算已用完/);
});

test('non-Evo deployment retains headless admission without mounting or requesting Evo UI', async () => {
  let evoReads = 0;
  const h = setup({ evo: false, evoStatus: async () => { evoReads++; return {}; } });
  await flush();
  assert.equal(h.nodes.length, 0);
  assert.equal(h.composer.children.length, 0);
  assert.equal(h.panel(), undefined);
  assert.equal((await h.admission('one')).effectiveMode, 'codex-style');
  assert.equal(h.posts.length, 0);
  assert.equal(evoReads, 0);
});
