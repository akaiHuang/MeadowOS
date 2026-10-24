import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { JSDOM } from 'jsdom';

const source = readFileSync(new URL('./run_limits.js', import.meta.url), 'utf8');
function setup(value = null) {
  const dom = new JSDOM('<main></main>', { url: 'https://fixture.invalid' });
  if (value !== null) dom.window.localStorage.setItem('meadow.runLimits.v1', value);
  const context = { document: dom.window.document, localStorage: dom.window.localStorage };
  vm.runInNewContext(source.replace(/^export /gm, '') + `
    globalThis.api = { runLimitOptions, buildRunLimitControl,
      buildEvoTokenBudgetControl, snapshotRunLimitPreferences, setRunLimitExecutionMode, runTokenBudget,
      setEvoRunLimitAvailability: typeof setEvoRunLimitAvailability === 'function' ? setEvoRunLimitAvailability : () => {},
      presets: RUN_LIMIT_PRESETS };`, context);
  return { ...context.api, dom };
}

test('confirmed Evo offers an explicit 60 minute pre-send choice', () => {
  const h = setup();
  h.setEvoRunLimitAvailability(true);
  const select = h.buildRunLimitControl();
  assert.ok([...select.options].some((option) => /60/.test(option.textContent)), 'Evo must expose its existing one-hour task cap in settings');
  h.dom.window.close();
});

test('separate Evo token choices require confirmed effective mode, not host availability', () => {
  const h = setup();
  h.setEvoRunLimitAvailability(true);
  const select = h.buildEvoTokenBudgetControl();
  h.dom.window.document.body.append(select);
  assert.equal(select.value, 'effort');
  assert.equal(select.disabled, true);
  h.setRunLimitExecutionMode('codex-style');
  assert.equal(select.disabled, true);
  h.setRunLimitExecutionMode('evo');
  assert.equal(select.disabled, false);
  for (const [value, expected] of [['extended', 2000000], ['generous', 3000000], ['effort', 1000000]]) {
    select.value = value;
    select.dispatchEvent(new h.dom.window.Event('change'));
    assert.equal(h.runLimitOptions('max').runTokenLimit, expected);
    assert.equal(h.runTokenBudget('max'), 1000000);
  }
  select.value = 'generous'; select.dispatchEvent(new h.dom.window.Event('change'));
  h.setRunLimitExecutionMode('codex-style');
  assert.equal(h.runLimitOptions('max').runTokenLimit, 1000000);
  h.setRunLimitExecutionMode('');
  assert.equal(h.runLimitOptions('max').runTokenLimit, 1000000);
  h.dom.window.close();
});

test('captured Evo choice survives later preference edits but resolves against confirmed mode', () => {
  const h = setup(); h.setRunLimitExecutionMode('evo');
  const select = h.buildEvoTokenBudgetControl();
  select.value = 'extended'; select.dispatchEvent(new h.dom.window.Event('change'));
  const captured = h.snapshotRunLimitPreferences();
  const admitted = h.runLimitOptions('high', captured, 'evo');
  select.value = 'generous'; select.dispatchEvent(new h.dom.window.Event('change'));
  assert.equal(h.runLimitOptions('high', captured, 'evo').runTokenLimit, 2000000);
  assert.equal(admitted.runTokenLimit, 2000000);
  assert.equal(h.runLimitOptions('high').runTokenLimit, 3000000);
  assert.equal(h.runLimitOptions('high', captured, 'codex-style').runTokenLimit, 750000);
  h.dom.window.close();
});

test('Evo availability never automatically extends default or selected 20 minute budgets', () => {
  for (const [stored, expected] of [[null, 600000], ['extended', 1200000]]) {
    const h = setup(stored);
    h.setEvoRunLimitAvailability(true);
    assert.equal(h.runLimitOptions().runTimeoutMs, expected);
    h.dom.window.close();
  }
});

test('explicit Evo preference affects only newly snapshotted tasks', () => {
  const h = setup('extended');
  h.setEvoRunLimitAvailability(true);
  const admitted = h.runLimitOptions('medium');
  const select = h.buildRunLimitControl();
  h.dom.window.document.body.append(select);
  select.value = 'evoExtended';
  select.dispatchEvent(new h.dom.window.Event('change'));
  assert.equal(h.runLimitOptions('medium').runTimeoutMs, 3600000);
  assert.equal(admitted.runTimeoutMs, 1200000, 'existing task/queue snapshots remain immutable');
  assert.equal(admitted.runTokenLimit, 500000);
  assert.equal(h.runLimitOptions('medium').runTokenLimit, 500000);
  h.dom.window.close();
});

test('an unconfirmed or non-Evo host cannot apply the stored Evo-only preset', () => {
  const h = setup('evoExtended');
  assert.equal(h.runLimitOptions().runTimeoutMs, 600000);
  const select = h.buildRunLimitControl();
  h.dom.window.document.body.append(select);
  assert.equal([...select.options].some((option) => option.value === 'evoExtended'), false);
  h.setEvoRunLimitAvailability(true);
  assert.equal(select.value, 'evoExtended');
  assert.equal(h.runLimitOptions().runTimeoutMs, 3600000);
  h.setEvoRunLimitAvailability(false);
  assert.equal(select.value, 'standard');
  assert.equal(h.runLimitOptions().runTimeoutMs, 600000);
  h.dom.window.close();
});
