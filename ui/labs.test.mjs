import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const observatory = readFileSync(fileURLToPath(new URL('./observatory.html', import.meta.url)), 'utf8');
const poseFusion = readFileSync(fileURLToPath(new URL('./pose-fusion.html', import.meta.url)), 'utf8');
const observatoryCss = readFileSync(fileURLToPath(new URL('./observatory/css/observatory.css', import.meta.url)), 'utf8');
const poseFusionCss = readFileSync(fileURLToPath(new URL('./pose-fusion/css/style.css', import.meta.url)), 'utf8');

test('legacy labs disclose experimental outputs and link back to supported operations', () => {
  for (const source of [observatory, poseFusion]) {
    assert.match(source, /class="research-disclaimer" role="note"/);
    assert.match(source, /not validated/);
    assert.match(source, /href="index\.html#overview"/);
  }
});

test('research pages do not fetch third-party fonts at runtime', () => {
  assert.doesNotMatch(observatoryCss, /fonts\.googleapis\.com/);
  assert.doesNotMatch(poseFusionCss, /fonts\.googleapis\.com/);
});