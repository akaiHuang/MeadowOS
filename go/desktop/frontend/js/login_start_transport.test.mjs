import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createServer } from 'node:http';
import vm from 'node:vm';
import { buildStartRequest, PRODUCT_AUTH_SCHEMA } from './commercial_contract.js';

const source = readFileSync(new URL('./core.js', import.meta.url), 'utf8')
  .replace("import { runLimitOptions } from './run_limits.js';", '')
  .replace("import { VM_LAB_BUILD } from './vm_lab_build.js';", 'const VM_LAB_BUILD = false;')
  .replace(/^export /gm, '');

function deferred() {
  let resolve;
  const promise = new Promise(yes => { resolve = yes; });
  return { promise, resolve };
}

function loadCore(fetchImpl) {
  const calls = [];
  const context = {
    console, setTimeout, clearTimeout, AbortController,
    document: { documentElement: { dataset: { frontendMode: 'production' } } },
    fetch: (path, options) => {
      calls.push({ path, ...options });
      return fetchImpl(path, options);
    },
  };
  vm.runInNewContext(source + '\nglobalThis.api = daemon;', context);
  return { daemon: context.api, calls };
}

test('signed-out AuthStart forwards AbortSignal separately from the unchanged public request', async () => {
  const response = { schema: PRODUCT_AUTH_SCHEMA, state: 'login_pending' };
  const h = loadCore(async () => ({ ok: true, status: 200, json: async () => response }));
  const controller = new AbortController();
  const body = buildStartRequest();
  assert.equal(await h.daemon.authStart(body, { signal: controller.signal }), response);
  assert.equal(h.calls.length, 1, 'login bootstrap must not wait for account capabilities');
  const [call] = h.calls;
  assert.equal(call.path, '/auth/start');
  assert.equal(call.method, 'POST');
  assert.equal(call.signal, controller.signal, 'pass the exact controller signal to fetch');
  assert.deepEqual(JSON.parse(JSON.stringify(call.headers)), { 'Content-Type': 'application/json' });
  assert.equal(call.body, JSON.stringify(body));
  assert.deepEqual(JSON.parse(call.body), { schema: PRODUCT_AUTH_SCHEMA, mode: 'browser' });
  assert.equal(Object.hasOwn(body, 'signal'), false);
});

test('AuthStart remains compatible with callers that omit transport options', async () => {
  const h = loadCore(async () => ({ ok: true, status: 200, json: async () => ({ ok: true }) }));
  assert.deepEqual(await h.daemon.authStart(buildStartRequest()), { ok: true });
  assert.equal(Object.hasOwn(h.calls[0], 'signal'), false);
  assert.equal(h.calls[0].body, JSON.stringify(buildStartRequest()));
});

// Real fetch uses only an owned ephemeral loopback server. No Agent, Firebase,
// model provider, current login, or external account service is contacted.
async function loopback(t, partialBody = false) {
  const received = deferred();
  const readingBody = deferred();
  const requests = [];
  const server = createServer((request, response) => {
    let body = '';
    request.setEncoding('utf8');
    request.on('data', chunk => { body += chunk; });
    request.on('end', () => {
      requests.push({ path: request.url, method: request.method, body });
      if (partialBody) {
        response.writeHead(200, { 'Content-Type': 'application/json' });
        response.write('{"schema":"meadow.product-auth.v1",');
      }
      received.resolve();
    });
  });
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  t.after(async () => {
    server.closeAllConnections();
    await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
  });
  const address = server.address();
  const h = loadCore(async (path, options) => {
    const response = await fetch(`http://127.0.0.1:${address.port}${path}`, options);
    const readJSON = response.json.bind(response);
    response.json = () => {
      readingBody.resolve();
      return readJSON();
    };
    return response;
  });
  return { ...h, received, readingBody, requests };
}

test('aborting AuthStart before headers rejects its real fetch', { timeout: 5000 }, async t => {
  const h = await loopback(t);
  const controller = new AbortController();
  const result = h.daemon.authStart(buildStartRequest(), { signal: controller.signal });
  const rejected = assert.rejects(result, { name: 'AbortError' });
  await h.received.promise;
  controller.abort();
  await rejected;
  assert.deepEqual(h.requests, [{ path: '/auth/start', method: 'POST', body: JSON.stringify(buildStartRequest()) }]);
  assert.equal(h.calls.length, 1, 'cancellation must not retry AuthStart');
});

test('aborting AuthStart while JSON is streaming rejects instead of resolving null', { timeout: 5000 }, async t => {
  const h = await loopback(t, true);
  const controller = new AbortController();
  const result = h.daemon.authStart(buildStartRequest(), { signal: controller.signal });
  const rejected = assert.rejects(result, { name: 'AbortError' });
  await h.readingBody.promise;
  controller.abort();
  await rejected;
  assert.equal(h.requests.length, 1);
  assert.equal(h.calls.length, 1);
});

test('an already aborted AuthStart never reaches the loopback service', { timeout: 5000 }, async t => {
  const h = await loopback(t);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(h.daemon.authStart(buildStartRequest(), { signal: controller.signal }), { name: 'AbortError' });
  assert.equal(h.requests.length, 0);
});
