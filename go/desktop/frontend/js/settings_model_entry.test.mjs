import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { JSDOM } from 'jsdom';

// Detached UI fixture verifies settings navigation, not native/provider access.
test('model setup opens its panel directly and Browser is enabled by default', async () => {
  const dom = new JSDOM('<button id="btnSettings">設定</button>', { url: 'https://fixture.invalid' });
  const handlers = new Map();
  let wizardCalls = 0;
  const ctx = {
    window: dom.window, document: dom.window.document,
    HTMLElement: dom.window.HTMLElement, localStorage: dom.window.localStorage,
    console, setTimeout, clearTimeout,
    state: { cwd: '/fixture' },
    bus: { on(name, fn) { handlers.set(name, fn); } },
    daemon: { updateStatus: async () => ({ state: 'idle' }) }, getTheme: () => 'dark', setTheme() {},
    buildModelConnections(body) { wizardCalls++; body.append(dom.window.document.createElement('form')); },
    fetch: async () => ({ ok: true, json: async () => ({ editors: [] }) }),
  };
  const src = readFileSync(new URL('./settings.js', import.meta.url), 'utf8')
    .replace(/^import[^\n]+\n/gm, '').replace(/^export /gm, '');
  vm.runInNewContext(src + '\ninitSettings();', ctx);
  handlers.get('settings:open-model')();
  const doc = dom.window.document;
  assert.equal(doc.querySelector('[aria-current="page"]').textContent, '設定模型');
  assert.equal(wizardCalls, 1);
  const panel = doc.querySelector('[data-settings-panel="model"]');
  for (const removed of ['目前模型', '記憶體策略', '圖片理解', '執行狀態'])
    assert.equal(panel.textContent.includes(removed), false);
  assert.ok(doc.querySelector('link[data-provider-wizard-styles]'));
  doc.querySelector('[data-settings-tab="general"]').click();
  const browser = doc.querySelector('[aria-label="只使用右側 Meadow Browser，預設啟用"]');
  assert.equal(browser.getAttribute('aria-checked'), 'true');
  assert.equal(browser.disabled, true);
  dom.window.close();
});
