import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { JSDOM } from 'jsdom';

const SOURCE = readFileSync(
  new URL('./model_connections.js', import.meta.url),
  'utf8',
);
const KINDS = ['managed-api', 'provider-api', 'local-api', 'meadow-core'];
const MODEL = {
  id: 'verified-model',
  name: 'Verified model',
  supportsImages: true,
};
const emptyCatalog = () => ({
  enabled: true,
  mode: 'connections',
  kinds: KINDS.map((id) => ({ id })),
  connections: [],
});
const response = (body, status = 200) => ({
  ok: status >= 200 && status < 300,
  status,
  json: async () => body,
});
const settle = async () => {
  for (let i = 0; i < 5; i += 1) await new Promise(setImmediate);
};
function deferred() {
  let resolve;
  const promise = new Promise((yes) => {
    resolve = yes;
  });
  return { resolve, promise };
}

// A detached DOM and mocked native requests validate form behavior and races.
// No browser, native bridge, real credential, model, or network request is used.
async function boot({
  catalog = emptyCatalog(),
  discovery = { cores: [], detection: { installed: false } },
  requests: handler,
} = {}) {
  const dom = new JSDOM('<main id="settings"></main>', {
    url: 'https://settings.invalid',
  });
  const calls = [];
  const events = [];
  const timers = new Map();
  const context = {
    document: dom.window.document,
    AbortController,
    bus: { emit: (...args) => events.push(args) },
    setTimeout: (callback) => {
      const id = timers.size + 1;
      timers.set(id, callback);
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
    fetch: async (path, options) => {
      const call = {
        path,
        options,
        body: options.body ? JSON.parse(options.body) : undefined,
      };
      calls.push(call);
      if (handler) {
        const result = handler(call);
        if (result !== undefined) return result;
      }
      if (path === '/native/model-providers') return response(catalog);
      if (path.endsWith('/local-core')) return response(discovery);
      if (path.endsWith('/managed-status'))
        return response({
          available: false,
          status: { configured: false, allowed: false },
        });
      if (path.endsWith('/verify'))
        return response({
          verificationId: 'verified-token',
          models: [MODEL],
          expiresInSeconds: 600,
        });
      if (path.endsWith('/save'))
        return response({ saved: true, activation: 'applied' });
      throw new Error(`Unexpected request: ${path}`);
    },
  };
  const importLine =
    "import { bus } from './core.js?v=raven-execution-20261006a';";
  assert.equal(SOURCE.split(importLine).length - 1, 1);
  vm.runInNewContext(
    `${SOURCE.replace(importLine, '').replace(/^export /gm, '')}\nglobalThis.build = buildModelConnections;`,
    context,
  );
  const dispose = context.build(dom.window.document.querySelector('#settings'));
  await settle();
  const $ = (selector) => dom.window.document.querySelector(selector);
  const input = (selector, value) => {
    const node = $(selector);
    node.value = value;
    node.dispatchEvent(new dom.window.Event('input', { bubbles: true }));
  };
  const checkProvider = (id) => {
    const node = $(`#settingsCloudProvider-${id}`);
    node.checked = true;
    node.dispatchEvent(new dom.window.Event('change', { bubbles: true }));
  };
  const submit = () =>
    $('#settingsModelConnectionForm').dispatchEvent(
      new dom.window.Event('submit', { bubbles: true, cancelable: true }),
    );
  return {
    dom,
    $,
    input,
    checkProvider,
    submit,
    calls,
    events,
    dispose,
    timers,
  };
}

test('empty catalog starts with four direct icon choices and three steps', async () => {
  const ui = await boot();
  assert.equal(ui.$('.settings-model-connections').dataset.setupStep, '1');
  assert.equal(ui.$('.model-source-chooser').hidden, false);
  assert.deepEqual(
    [...ui.dom.window.document.querySelectorAll('[data-source]')].map(
      (item) => item.dataset.source,
    ),
    ['managed-api', 'provider-api', 'meadow-core', 'local-api'],
  );
  assert.equal(ui.$('#settingsModelProvider').hidden, true);
  assert.equal(ui.$('#settingsModelConnectionForm').hidden, true);
  ui.$('#settingsSource-provider-api').click();
  assert.equal(ui.$('.settings-model-connections').dataset.setupStep, '2');
  assert.equal(ui.$('.model-provider-grid').hidden, false);
  assert.equal(ui.$('#settingsModelAPIKey').hidden, true);
  ui.checkProvider('openai');
  assert.equal(ui.$('#settingsModelAPIKey').hidden, false);
  assert.equal(
    ui.$('#settingsModelBaseURL').value,
    'https://api.openai.com/v1',
  );
  assert.equal(ui.$('#settingsVerifyModelProvider').disabled, true);
  ui.input('#settingsModelAPIKey', 'synthetic-secret');
  assert.equal(ui.$('#settingsVerifyModelProvider').disabled, false);
  assert.equal(
    ui.dom.window.document.activeElement,
    ui.$('#settingsModelAPIKey'),
    'updates retain the focused key input',
  );
  ui.dispose();
});

test('Anthropic verification binds its protocol, clears the key, and only presents verified models', async () => {
  const ui = await boot();
  ui.$('#settingsSource-provider-api').click();
  ui.checkProvider('anthropic');
  ui.input('#settingsModelAPIKey', 'synthetic-anthropic-secret');
  ui.submit();
  await settle();
  const verify = ui.calls.find((call) => call.path.endsWith('/verify'));
  assert.deepEqual(verify.body, {
    kind: 'provider-api',
    name: 'Anthropic',
    baseUrl: 'https://api.anthropic.com',
    protocol: 'anthropic',
    useStoredKey: false,
    apiKey: 'synthetic-anthropic-secret',
  });
  assert.equal(ui.$('#settingsModelAPIKey').value, '');
  assert.equal(ui.$('.settings-model-connections').dataset.setupStep, '3');
  assert.equal(ui.$('.model-provider-grid').hidden, true);
  assert.equal(ui.$('.settings-model-choice input').value, MODEL.id);
  assert.equal(
    ui.dom.window.document.documentElement.outerHTML.includes(
      'synthetic-anthropic-secret',
    ),
    false,
  );
  ui.$('#settingsSelectAllModels').click();
  ui.$('#settingsSaveModels').click();
  await settle();
  assert.deepEqual(JSON.parse(JSON.stringify(ui.events)), [
    ['model_catalog_changed', {}],
  ]);
  assert.match(
    ui.$('.settings-model-connection-summary').textContent,
    /設定完成/,
  );
  ui.dispose();
});

test('saved-but-restart-required does not announce a live catalog refresh', async () => {
  const ui = await boot({
    requests: (call) =>
      call.path.endsWith('/save')
        ? response({ saved: true, activation: 'restart-required' })
        : undefined,
  });
  ui.$('#settingsSource-local-api').click();
  ui.submit();
  await settle();
  ui.$('#settingsSelectAllModels').click();
  ui.$('#settingsSaveModels').click();
  await settle();
  assert.equal(ui.events.length, 0);
  assert.match(
    ui.$('.settings-model-connection-summary').textContent,
    /請重新啟動/,
  );
  ui.dispose();
});

test('installed but stopped Core is not described as absent; local pairing is the simple path', async () => {
  const ui = await boot({
    discovery: {
      cores: [],
      detection: {
        installed: true,
        running: false,
        paired: false,
        state: 'installed',
      },
    },
  });
  ui.$('#settingsSource-meadow-core').click();
  await settle();
  assert.match(
    ui.$('#settingsLocalCoreStatus').textContent,
    /已安裝.*尚未執行/,
  );
  assert.equal(ui.$('#settingsModelAPIKey').hidden, true);
  assert.equal(ui.$('#settingsModelAdvanced').hidden, true);
  assert.equal(ui.$('#settingsCoreInvitation').closest('details').open, false);
  ui.dispose();
});

test('closing during verification aborts the native request and ignores its late result', async () => {
  const pending = deferred();
  const ui = await boot({
    requests: (call) =>
      call.path.endsWith('/verify') ? pending.promise : undefined,
  });
  ui.$('#settingsSource-provider-api').click();
  ui.checkProvider('deepseek');
  ui.input('#settingsModelAPIKey', 'synthetic-secret');
  ui.submit();
  await settle();
  const call = ui.calls.find((row) => row.path.endsWith('/verify'));
  ui.dispose();
  assert.equal(call.options.signal.aborted, true);
  assert.equal(ui.$('#settingsModelAPIKey').value, '');
  pending.resolve(response({ verificationId: 'late-token', models: [MODEL] }));
  await settle();
  assert.equal(ui.$('.settings-model-choices').hidden, true);
  assert.equal(ui.$('.settings-model-choice'), null);
  assert.equal(ui.timers.size, 0);
});

test('changing the provider returns to step two and discards a typed key', async () => {
  const ui = await boot();
  ui.$('#settingsSource-provider-api').click();
  ui.checkProvider('openai');
  ui.input('#settingsModelAPIKey', 'synthetic-secret');
  ui.checkProvider('deepseek');
  assert.equal(ui.$('#settingsModelAPIKey').value, '');
  assert.equal(ui.$('#settingsCloudProvider-openai').checked, false);
  assert.equal(ui.$('#settingsCloudProvider-deepseek').checked, true);
  assert.equal(
    ui.$('#settingsModelBaseURL').value,
    'https://api.deepseek.com/v1',
  );
  ui.$('#settingsChangeModelSource').click();
  assert.equal(ui.$('.settings-model-connections').dataset.setupStep, '1');
  assert.equal(ui.$('#settingsModelConnectionForm').hidden, true);
  ui.dispose();
});

test('editing retains an existing credential only while endpoint and protocol both match', async () => {
  const connection = {
    id: 'anthropic-existing',
    kind: 'provider-api',
    name: 'My Anthropic',
    baseUrl: 'https://api.anthropic.com',
    protocol: 'anthropic',
    configured: true,
    models: [MODEL],
  };
  const ui = await boot({
    catalog: { ...emptyCatalog(), connections: [connection] },
  });
  ui.$('[data-connection-action="edit"]').click();
  assert.equal(ui.$('#settingsModelStoredKey').checked, true);
  assert.equal(ui.$('#settingsModelProtocol').value, 'anthropic');
  ui.$('#settingsModelProtocol').value = 'openai-compatible';
  ui.$('#settingsModelProtocol').dispatchEvent(
    new ui.dom.window.Event('change', { bubbles: true }),
  );
  assert.equal(ui.$('#settingsModelStoredKey').checked, false);
  assert.equal(ui.$('#settingsModelAPIKey').value, '');
  ui.dispose();
});

test('unchecking a provider clears its draft instead of immediately checking it again', async () => {
  const ui = await boot();
  ui.$('#settingsSource-provider-api').click();
  ui.checkProvider('openai');
  ui.input('#settingsModelAPIKey', 'synthetic-secret');
  ui.$('#settingsCloudProvider-openai').checked = false;
  ui.$('#settingsCloudProvider-openai').dispatchEvent(
    new ui.dom.window.Event('change', { bubbles: true }),
  );
  assert.equal(ui.$('#settingsCloudProvider-openai').checked, false);
  assert.equal(ui.$('#settingsModelAPIKey').value, '');
  assert.equal(ui.$('#settingsModelAPIKey').hidden, true);
  assert.equal(ui.$('#settingsVerifyModelProvider').disabled, true);
  ui.dispose();
});

test('editing a saved Core refreshes its existing connection and retains its model choices', async () => {
  const connection = {
    id: 'core-existing',
    kind: 'meadow-core',
    name: 'My Core',
    baseUrl: 'http://127.0.0.1:18081/v1',
    configured: true,
    models: [MODEL],
  };
  const ui = await boot({
    catalog: { ...emptyCatalog(), connections: [connection] },
  });
  ui.$('[data-connection-action="edit"]').click();
  assert.equal(ui.$('#settingsVerifyModelProvider').hidden, false);
  assert.equal(ui.$('#settingsModelAPIKey').hidden, true);
  assert.equal(ui.$('.settings-model-core-pairing').hidden, true);
  assert.equal(
    ui.dom.window.document.activeElement,
    ui.$('#settingsVerifyModelProvider'),
  );
  ui.submit();
  await settle();
  const verify = ui.calls.find((call) => call.path.endsWith('/verify'));
  assert.deepEqual(verify.body, {
    connectionId: 'core-existing',
    kind: 'meadow-core',
    name: 'My Core',
    baseUrl: 'http://127.0.0.1:18081/v1',
    useStoredKey: true,
  });
  assert.equal(ui.$('.settings-model-choice input').checked, true);
  ui.dispose();
});

test('changing a provider while editing replaces the original connection safely', async () => {
  const connection = {
    id: 'openai-existing',
    kind: 'provider-api',
    name: 'My OpenAI',
    baseUrl: 'https://api.openai.com/v1',
    protocol: 'openai-compatible',
    configured: true,
    models: [MODEL],
  };
  const ui = await boot({
    catalog: { ...emptyCatalog(), connections: [connection] },
  });
  ui.$('[data-connection-action="edit"]').click();
  ui.checkProvider('xai');
  assert.equal(ui.$('#settingsModelStoredKey').checked, false);
  ui.input('#settingsModelAPIKey', 'synthetic-new-secret');
  ui.submit();
  await settle();
  const verify = ui.calls.find((call) => call.path.endsWith('/verify'));
  assert.equal(verify.body.connectionId, 'openai-existing');
  assert.equal(verify.body.baseUrl, 'https://api.x.ai/v1');
  assert.equal(verify.body.useStoredKey, false);
  ui.dispose();
});

test('keyboard focus follows source, verified models, saved completion and source return', async () => {
  const ui = await boot();
  ui.$('#settingsSource-provider-api').focus();
  ui.$('#settingsSource-provider-api').click();
  assert.equal(
    ui.dom.window.document.activeElement,
    ui.$('#settingsCloudProvider-openai'),
  );
  ui.checkProvider('openai');
  ui.input('#settingsModelAPIKey', 'synthetic-secret');
  ui.submit();
  await settle();
  assert.equal(
    ui.dom.window.document.activeElement,
    ui.$('.settings-model-choice input'),
  );
  ui.$('#settingsSelectAllModels').click();
  ui.$('#settingsSaveModels').focus();
  ui.$('#settingsSaveModels').click();
  await settle();
  assert.equal(ui.dom.window.document.activeElement, ui.$('#settingsAddModel'));
  ui.$('#settingsAddModel').click();
  assert.equal(
    ui.dom.window.document.activeElement,
    ui.$('#settingsSource-managed-api'),
  );
  ui.$('#settingsSource-local-api').click();
  ui.$('#settingsChangeModelSource').focus();
  ui.$('#settingsChangeModelSource').click();
  assert.equal(
    ui.dom.window.document.activeElement,
    ui.$('#settingsSource-managed-api'),
  );
  ui.dispose();
});

test('legacy Core inventory is distinguished from a standalone Core installation', async () => {
  const ui = await boot({
    discovery: {
      cores: [],
      detection: {
        installed: false,
        running: false,
        legacyRuntimeFound: true,
        state: 'legacy-runtime',
        detail: '/private/path/never-render',
      },
    },
  });
  ui.$('#settingsSource-meadow-core').click();
  await settle();
  assert.match(
    ui.$('#settingsLocalCoreStatus').textContent,
    /找到舊版 Core 啟動設定/,
  );
  assert.doesNotMatch(
    ui.$('#settingsLocalCoreStatus').textContent,
    /尚未找到|已安裝|private/,
  );
  ui.dispose();
});

test('public Core health is shown as running with unverified pairing', async () => {
  const ui = await boot({
    discovery: {
      cores: [],
      detection: {
        installed: false,
        running: true,
        paired: false,
        state: 'running-unverified',
        detail: 'secret-never-render',
      },
    },
  });
  ui.$('#settingsSource-meadow-core').click();
  await settle();
  assert.match(
    ui.$('#settingsLocalCoreStatus').textContent,
    /運作中.*尚未確認配對/,
  );
  assert.doesNotMatch(
    ui.$('#settingsLocalCoreStatus').textContent,
    /已確認本機 Core 身分|secret/,
  );
  ui.dispose();
});
