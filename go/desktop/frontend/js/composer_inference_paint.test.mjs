import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { JSDOM } from 'jsdom';

const source = readFileSync(process.env.MEADOW_COMPOSER_SOURCE || new URL('./composer.js', import.meta.url), 'utf8');
const limits = readFileSync(new URL('./run_limits.js', import.meta.url), 'utf8');
function section(begin, end) {
  const offset = source.indexOf(begin);
  assert.ok(offset >= 0);
  return source.slice(offset, source.indexOf(end, offset));
}
function fixture(mode = 'evo') {
  const dom = new JSDOM('<button id="pillModel"><span id="pillEffortValue"></span></button><section id="modelInference"></section>', { url: 'https://fixture.invalid' });
  dom.window.localStorage.setItem('meadow.evoTokenBudget.v1', 'generous');
  const context = {
    document: dom.window.document, localStorage: dom.window.localStorage,
    $: selector => dom.window.document.querySelector(selector),
    state: { sessionId: 'fresh-session' }, inferenceControls: null,
    inferenceControlsSaving: false,
    modelModalRef: { inference: dom.window.document.querySelector('#modelInference') },
    updateInferenceControls() {},
  };
  // Use the actual budget module and complete paint path. In particular, do
  // not inject removed imports: that would hide a startup ReferenceError.
  vm.runInNewContext('const budgetAPI = (() => {\n' + limits.replace(/^export /gm, '') +
    '\nreturn { runLimitOptions, setRunLimitExecutionMode }; })();\nconst { runLimitOptions } = budgetAPI;\n' +
    section('const EFFORTS =', 'async function loadInferenceControls()') +
    section('function paintEffortShortcut()', 'async function refetchModels(') +
    '\nglobalThis.paint = paintInferenceControls; globalThis.setMode = budgetAPI.setRunLimitExecutionMode;', context);
  context.setMode(mode);
  const profiles = ['minimal', 'low', 'medium', 'high', 'max'].map(effort => ({
    effort, label: effort, description: `${effort} reasoning`,
    thinkingMode: effort === 'minimal' ? 'off' : 'on', budgetLimited: false,
  }));
  const ready = {
    maturity: 'experimental', reasoningEffort: 'max', effectiveEffort: 'minimal',
    thinkingSupported: true, effortSupported: true, thinkingMode: 'off',
    effortProfiles: profiles, effectiveProfile: profiles[0],
  };
  return { dom, context, ready, panel: context.modelModalRef.inference };
}

test('inference paint renders loading, then usable Max and Thinking controls with Evo 3M', () => {
  const h = fixture();
  h.context.paint();
  assert.match(h.panel.textContent, /讀取中/);
  h.context.inferenceControls = h.ready;
  h.context.paint();
  assert.equal(h.panel.querySelector('input[type="range"]').disabled, false);
  assert.equal(h.panel.querySelector('input[type="range"]').value, '4');
  assert.equal(h.panel.querySelector('#thinkingToggle').getAttribute('aria-pressed'), 'false');
  assert.equal(h.panel.querySelector('#effortRunTokenLimit').textContent, '每次任務額度：300 萬 tokens');
  assert.match(h.dom.window.document.querySelector('#pillModel').getAttribute('aria-label'), /300 萬 tokens/);
  h.dom.window.close();
});

test('ordinary Max paint stays 1M and failed controls render an actionable error', () => {
  const h = fixture('codex-style');
  h.context.inferenceControls = h.ready; h.context.paint();
  assert.equal(h.panel.querySelector('#effortRunTokenLimit').textContent, '每次任務額度：100 萬 tokens');
  assert.match(h.dom.window.document.querySelector('#pillModel').getAttribute('aria-label'), /100 萬 tokens/);
  h.context.inferenceControls = { maturity: 'error', note: 'controlled failure' };
  h.context.paint();
  assert.match(h.panel.textContent, /推理設定暫時無法讀取/);
  assert.match(h.panel.textContent, /controlled failure/);
  h.dom.window.close();
});
