import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const core = readFileSync(new URL('./core.js', import.meta.url), 'utf8')
  .replace(/^import[^\n]+\n/gm, '').replace(/^export /gm, '');
const timeline = readFileSync(new URL('./timeline.js', import.meta.url), 'utf8');
const userProjector = timeline.slice(timeline.indexOf('function onUserChunk('), timeline.indexOf('\n// --- thought blocks ---'));
const composer = readFileSync(new URL('./composer.js', import.meta.url), 'utf8');
const usageStart = composer.indexOf('  let threadTokens = 0;');
const usageProjector = composer.slice(usageStart, composer.indexOf('  refreshContext();', usageStart));

function harness() {
  const streams = [], rows = [], requests = [], usages = [];
  const context = { console, setTimeout, clearTimeout, URL, Request, Response,
    document: { documentElement: { dataset: { frontendMode: 'production' } } },
    fetch: async (path) => { requests.push(path); return Response.json({ protected: false, authenticationRequired: true }); },
    EventSource: class {
      constructor(path) { this.path = path; this.handlers = new Map(); streams.push(this); }
      addEventListener(type, callback) { this.handlers.set(type, callback); }
      close() { this.closed = true; }
    },
  };
  vm.runInNewContext(`${core}\nglobalThis.api = { daemon, state, bus, connectEvents, disconnectEvents, setClientAuthentication };
    globalThis.retainedIDCount = () => activeConnection?.projection?.seenIds.size;`, context);
  const h = context.api;
  Object.assign(context, h, { rows, usages, usagePill: {},
    $: () => context.usagePill, fmtK: String, paintContext() {},
    contentText: u => u.content?.text || '', messageAttachments: () => [],
    hideWait() {}, finishWait() {}, finalizeAgent() {}, finalizeThought() {},
    resetProgressTrail() {}, showWait() {},
    userMessageRow: (u, options) => ({ row: { text: u.content.text, isSteer: options.isSteer }, resend: {} }),
    tl: { appendChild: row => rows.push(row) },
  });
  vm.runInNewContext(`(() => {
    let turnOpen = false, turnHasUser = false, wait = null, waitingForUser = false;
    let latestInferenceProgress, openThought, thoughtFallbackRun = 0, turnStart, pendingUserReply, last;
    let contextUsed = 0, contextEstimated = false, contextMax = 0;
    ${userProjector}
    bus.on('update:user_message_chunk', onUserChunk);
    bus.on('usage', value => usages.push(value));
    ${usageProjector}
    globalThis.projectedTokens = () => threadTokens;
  })();`, context);
  const account = subject => ({ authenticated: true, state: 'authorized_general', subject,
    entitlement: { leaseID: 'lease-' + subject, deviceID: 'device-' + subject, revocationEpoch: 0 } });
  const frame = (type, data, id, stream = streams.at(-1), onmessage = false) => {
    const event = { type, data: JSON.stringify({ type, data, ...(id == null ? {} : { id }) }),
      ...(id == null ? {} : { lastEventId: String(id) }) };
    (onmessage ? stream.onmessage : stream.handlers.get(type))(event);
  };
  return { ...h, streams, rows, usages, requests, frame, account, context,
    async ready() { await h.daemon.clientCapabilities(); h.setClientAuthentication(account('one')); },
    recover() { h.setClientAuthentication(null); h.setClientAuthentication(account('one')); },
    user(id, text = 'same user request', stream) { frame('session_update', { update: { sessionUpdate: 'user_message_chunk', content: { type: 'text', text } } }, id, stream); },
    usage(id, total, stream) { frame('usage', { totalTokens: total }, id, stream); },
    tokens: () => context.projectedTokens(),
  };
}

test('six account recoveries preserve one user and exact usage with the real 220900 receipt totals', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  const totals = [2255,3273,4772,4902,7355,7675,7834,9183,4624,4890,12820,13813,16553,16875,16971,17402,18925,19118,21012,4510,4794,12811,13167,14175,20147,22054,22224,23699,3869,4122,6388,8820,9041];
  h.user('1'); totals.slice(0, 32).forEach((n, i) => h.usage(String(i + 2), n));
  assert.equal(h.tokens(), 371032);
  for (let pass = 0; pass < 6; pass++) {
    h.recover();
    h.user('1');
    totals.slice(0, pass === 5 ? 33 : 32).forEach((n, i) => h.usage(String(i + 2), n));
  }
  assert.equal(h.rows.length, 1, 'replayed original user must not become six apparent Guide messages');
  assert.equal(h.tokens(), 380073, 'usage must match the 33 durable provider receipts exactly');
  assert.equal(h.usages.length, 33);
  assert.ok(h.streams.slice(1).every(stream => stream.path.endsWith('?lastEventId=33')));
  assert.equal(h.requests.some(path => /\/(prompt|steer|guide)(?:\?|$)/.test(path)), false);
});

test('one event delivered through both handlers is projected once, while distinct equal-text user events remain Guides', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  h.user('1'); h.user('1'); h.user('2');
  h.frame('usage', { totalTokens: 7 }, '3');
  h.frame('usage', { totalTokens: 7 }, '3', undefined, true);
  assert.deepEqual(h.rows.map(row => row.isSteer), [false, true]);
  assert.equal(h.tokens(), 7);
});

test('out-of-order unseen positive IDs survive and cannot rewind the recovery cursor', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  h.usage('10', 10); h.usage('9', 9); h.usage('9', 9); h.recover();
  assert.equal(h.tokens(), 19);
  assert.equal(h.streams.at(-1).path, '/session/one/events?lastEventId=10');
});

test('decimal IDs beyond safe Number precision stay distinct and use the exact cursor digits', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  h.usage('1900000000000000001', 1); h.usage('1900000000000000002', 2); h.usage('1900000000000000001', 1);
  h.recover();
  assert.equal(h.tokens(), 3);
  assert.ok(h.streams.at(-1).path.endsWith('lastEventId=1900000000000000002'));
});

test('missing, zero and invalid IDs do not deduplicate legitimate equal events by content', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  h.usage('8', 1);
  for (const id of [undefined, undefined, '0', '-1', 'not-an-id']) h.usage(id, 2);
  assert.equal(h.usages.length, 6);
  assert.equal(h.tokens(), 11);
  h.recover(); assert.ok(h.streams.at(-1).path.endsWith('lastEventId=8'));
});

test('deliberate history reload and fresh sessions reset projection deduplication', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  h.usage('1', 3); h.bus.emit('thread:switched'); h.connectEvents('one', 0); h.usage('1', 3);
  assert.equal(h.tokens(), 3);
  h.bus.emit('thread:switched'); h.connectEvents('one'); h.usage('1', 4);
  assert.equal(h.tokens(), 4);
  h.bus.emit('thread:switched'); h.state.sessionId = 'two'; h.connectEvents('two'); h.usage('1', 5);
  assert.equal(h.tokens(), 5);
});

test('history reload while unavailable invalidates a retained recovery cursor', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one'); h.usage('5', 5);
  h.setClientAuthentication(null); h.bus.emit('history:load', []); h.setClientAuthentication(h.account('one'));
  assert.equal(h.streams.at(-1).path, '/session/one/events');
  h.usage('5', 5); assert.equal(h.usages.length, 2);
});

test('old stream and another verified account cannot advance or inherit the retained cursor', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  const old = h.streams.at(-1); h.usage('7', 7); h.recover(); h.usage('100', 100, old);
  h.recover(); assert.ok(h.streams.at(-1).path.endsWith('lastEventId=7'));
  h.setClientAuthentication(h.account('two')); assert.equal(h.state.sessionId, null);
  h.state.sessionId = 'one'; h.connectEvents('one'); h.usage('7', 2);
  assert.equal(h.tokens(), 9);
});

test('the bounded seen set retains the complete latest native 8000-frame overlap', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  for (let id = 1; id <= 8300; id++) h.usage(String(id), 1);
  assert.ok(h.context.retainedIDCount() <= 8192);
  h.recover();
  for (let id = 301; id <= 8300; id++) h.usage(String(id), 1);
  assert.equal(h.tokens(), 8300);
  assert.equal(h.usages.length, 8300);
});

test('malformed JSON and unknown event types cannot poison a later valid event ID', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  const stream = h.streams.at(-1);
  stream.onmessage({ type: 'usage', lastEventId: '7', data: '{' });
  stream.onmessage({ type: 'not-public', lastEventId: '7', data: JSON.stringify({ type: 'not-public', id: 7, data: {} }) });
  h.usage('7', 7);
  assert.equal(h.tokens(), 7);
});

test('envelope fallback preserves exact decimal strings and rejects unsafe rounded numeric cursors', async () => {
  const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one');
  const stream = h.streams.at(-1);
  const withoutSSEID = id => stream.onmessage({ type: 'usage', data: JSON.stringify({ type: 'usage', id, data: { totalTokens: 1 } }) });
  withoutSSEID(7); withoutSSEID(7);
  withoutSSEID('1900000000000000001'); withoutSSEID('1900000000000000001');
  withoutSSEID(1900000000000000002); withoutSSEID(1900000000000000002);
  assert.equal(h.tokens(), 4, 'unsafe numeric IDs have no safe event identity, so preserve both deliveries');
  h.recover(); assert.ok(h.streams.at(-1).path.endsWith('lastEventId=1900000000000000001'));
});

test('same-subject lease/device/epoch replacement never inherits old replay state', async () => {
  for (const [field, value] of [['leaseID', 'new-lease'], ['deviceID', 'new-device'], ['revocationEpoch', 2]]) {
    const h = harness(); await h.ready(); h.state.sessionId = 'one'; h.connectEvents('one'); h.usage('7', 7);
    h.setClientAuthentication(null); h.setClientAuthentication(null);
    const next = h.account('one'); next.entitlement[field] = value;
    h.setClientAuthentication(next); assert.equal(h.state.sessionId, null);
    h.state.sessionId = 'one'; h.connectEvents('one'); h.usage('7', 2);
    assert.equal(h.tokens(), 9);
  }
});
