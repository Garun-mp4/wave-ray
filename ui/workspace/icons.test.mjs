import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const source = readFileSync(new URL('./icons.svg', import.meta.url), 'utf8');
const app = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const shell = readFileSync(new URL('../index.html', import.meta.url), 'utf8');
const available = new Set([...source.matchAll(/id="icon-([^"]+)"/g)].map(match => match[1]));

test('all workspace icon references resolve to a local SVG symbol', () => {
  const used = new Set();
  for (const match of app.matchAll(/icon\('([^']+)'\)/g)) used.add(match[1].replaceAll('-', '_'));
  for (const match of shell.matchAll(/#icon-([A-Za-z0-9_-]+)/g)) used.add(match[1]);
  for (const dynamic of ['activity', 'wifi', 'radio']) used.add(dynamic);
  const missing = [...used].filter(name => !available.has(name));
  assert.deepEqual(missing, []);
});