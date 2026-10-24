import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

// Exercise the actual modal traversal helper. WebKit can omit buttons from
// native Tab traversal, so each step must be explicit, including interior ones.
const source = readFileSync(new URL('./commercial.js', import.meta.url), 'utf8');
const helpers = source.slice(source.indexOf('function focusable(root)'), source.indexOf('function createOverlay('));

function harness() {
  const document = { activeElement: null };
  const items = ['login', 'input', 'theme', 'account'].map(id => ({
    id, hidden: false, disabled: false, blocked: false,
    closest() { return this.blocked ? {} : null; },
    getClientRects() { return [{}]; },
    focus() { document.activeElement = this; },
  }));
  let handler;
  const overlay = {
    querySelectorAll: () => items.filter(item => !item.disabled),
    addEventListener(type, callback) { handler = callback; },
    removeEventListener(type, callback) { if (handler === callback) handler = null; },
  };
  const context = { document };
  vm.runInNewContext(`${helpers}\nglobalThis.trap = trapDialog;`, context);
  const release = context.trap(overlay);
  const press = (key, options = {}) => {
    const event = { key, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, ...options };
    handler?.(event);
    return event;
  };
  return { document, items, press, release };
}

test('every Tab and Shift+Tab step explicitly cycles through visible dialog controls', () => {
  const h = harness();
  h.items[0].focus();
  for (const expected of ['input', 'theme', 'account', 'login']) {
    assert.equal(h.press('Tab').defaultPrevented, true);
    assert.equal(h.document.activeElement.id, expected);
  }
  for (const expected of ['account', 'theme', 'input', 'login']) {
    assert.equal(h.press('Tab', { shiftKey: true }).defaultPrevented, true);
    assert.equal(h.document.activeElement.id, expected);
  }
});

test('disabled, hidden and inert controls are skipped; an external focus enters the dialog', () => {
  const h = harness();
  h.items[0].disabled = true;
  h.items[1].hidden = true;
  h.items[2].blocked = true;
  assert.equal(h.press('Tab').defaultPrevented, true);
  assert.equal(h.document.activeElement.id, 'account');
  assert.equal(h.press('Tab', { shiftKey: true }).defaultPrevented, true);
  assert.equal(h.document.activeElement.id, 'account');
});

test('ordinary input and OS close shortcuts remain untouched and released traps stop intercepting', () => {
  const h = harness();
  h.items[1].focus();
  for (const [key, options] of [['a', {}], ['ArrowLeft', {}], ['q', { metaKey: true }], ['w', { metaKey: true }]]) {
    assert.equal(h.press(key, options).defaultPrevented, false);
    assert.equal(h.document.activeElement.id, 'input');
  }
  h.release();
  assert.equal(h.press('Tab').defaultPrevented, false);
});
