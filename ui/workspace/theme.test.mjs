import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('./theme.js', import.meta.url), 'utf8');
function browser({ saved, light = false, blocked = false } = {}) {
  const dom = {}, events = {}, attributes = {};
  const root = { dataset: {}, style: {}, classList: { add() {}, remove() {} } };
  const button = { setAttribute: (key, value) => { attributes[key] = value; }, addEventListener: (key, fn) => { dom[key] = fn; } };
  const media = { matches: light, addEventListener: (key, fn) => { events.media = fn; } };
  const storage = new Map(saved ? [['ruview-theme', saved]] : []);
  const sandbox = {
    document: { documentElement: root, querySelector: () => ({ setAttribute() {} }), getElementById: () => button,
      addEventListener: (key, fn) => { dom[key] = fn; } },
    window: { matchMedia: () => media, dispatchEvent() {}, requestAnimationFrame: fn => fn(), addEventListener: (key, fn) => { events[key] = fn; } },
    localStorage: { getItem: key => { if (blocked) throw Error('blocked'); return storage.get(key); },
      setItem: (key, value) => { if (blocked) throw Error('blocked'); storage.set(key, value); } },
    Event: class { constructor(type) { this.type = type; } }
  };
  vm.runInNewContext(source, sandbox);
  dom.DOMContentLoaded();
  return { root, attributes, dom, events, storage };
}
test('theme follows the OS until an explicit choice and persists accessible toggle state', () => {
  const b = browser({ light: true });
  assert.equal(b.root.dataset.theme, 'light');
  b.events.media({ matches: false });
  assert.equal(b.root.dataset.theme, 'dark');
  b.dom.click();
  assert.equal(b.root.dataset.theme, 'light');
  assert.equal(b.storage.get('ruview-theme'), 'light');
  assert.equal(b.attributes['aria-label'], 'Switch to dark theme');
  assert.equal(b.attributes['aria-pressed'], 'false');
  b.events.media({ matches: false });
  assert.equal(b.root.dataset.theme, 'light');
});
test('saved choice wins over OS; invalid values use OS; blocked storage remains usable', () => {
  assert.equal(browser({ saved: 'dark', light: true }).root.dataset.theme, 'dark');
  assert.equal(browser({ saved: 'invalid', light: true }).root.dataset.theme, 'light');
  const b = browser({ blocked: true });
  b.dom.click();
  assert.equal(b.root.dataset.theme, 'light');
});
test('cross-tab theme changes update both palette and button', () => {
  const b = browser();
  b.events.storage({ key: 'ruview-theme', newValue: 'light' });
  assert.equal(b.root.style.colorScheme, 'light');
  assert.equal(b.attributes['aria-label'], 'Switch to dark theme');
  b.events.storage({ key: 'unrelated', newValue: 'dark' });
  assert.equal(b.root.dataset.theme, 'light');
});
