import test from 'node:test';
import assert from 'node:assert/strict';
import { configuredPickerRows } from './model_picker_catalog.js';

test('same model name at two providers remains two independently bound rows', () => {
  const catalog = { mode: 'connections', selectedConnectionId: 'B', selectedModelId: 'same', connections: [
    { id: 'A', name: 'Provider A', models: [{ id: 'same', name: 'Same', supportsImages: false }] },
    { id: 'B', name: 'Provider B', models: [{ id: 'same', name: 'Same', supportsImages: true }] },
  ] };
  const rows = configuredPickerRows([{ modelId: 'same', live: true, active: true, multimodal: false }], catalog);
  assert.equal(rows.length, 2);
  assert.equal(rows[0].connectionId, 'A');
  assert.equal(rows[0].active, false);
  assert.equal(rows[0].live, false);
  assert.equal(rows[1].connectionId, 'B');
  assert.equal(rows[1].live, true);
  assert.equal(rows[1].multimodal, false, 'current native capability overrides cached setup metadata');
});
test('empty saved catalog clears legacy rows while an unrelated protected catalog retains its own rows', () => {
  const legacy = [{ modelId: 'legacy' }];
  assert.deepEqual(configuredPickerRows(legacy, { mode: 'connections', connections: [] }), []);
  assert.equal(configuredPickerRows(legacy, { mode: 'protected' }), legacy);
});
