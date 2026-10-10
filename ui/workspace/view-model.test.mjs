import test from 'node:test';
import assert from 'node:assert/strict';
import {
  capabilityState,
  escapeHtml,
  formatAge,
  formatBytes,
  formatDate,
  getSourcePresentation,
  hasUsableSignalField,
  normalizeMeshNodes
} from './view-model.js';

test('escapeHtml encodes every HTML-sensitive character', () => {
  assert.equal(escapeHtml(`<img src=x onerror='alert(1)'>&`), '&lt;img src=x onerror=&#39;alert(1)&#39;&gt;&amp;');
});

test('formatAge represents the freshness boundary without turning invalid values into measurements', () => {
  assert.equal(formatAge(0), 'just now');
  assert.equal(formatAge(59_999), '59s ago');
  assert.equal(formatAge(60_000), '1m ago');
  assert.equal(formatAge(-1), '—');
  assert.equal(formatAge(Number.NaN), '—');
});

test('formatBytes keeps units readable and rejects invalid input', () => {
  assert.equal(formatBytes(512), '512 B');
  assert.equal(formatBytes(1536), '1.5 KB');
  assert.equal(formatBytes(2 * 1024 * 1024), '2.0 MB');
  assert.equal(formatBytes(-1), '—');
});

test('formatDate handles timestamps and malformed server values', () => {
  assert.notEqual(formatDate('2026-01-01T00:00:00Z'), '—');
  assert.equal(formatDate('not-a-date'), '—');
  assert.equal(formatDate(null), '—');
});

test('source presentation never treats synthetic or stale frames as live measurements', () => {
  assert.equal(getSourcePresentation({
    health: { source: 'esp32' }, latest: { source: 'simulate', timestamp: 1 }, socketState: 'connected'
  }).mode, 'synthetic');
  assert.equal(getSourcePresentation({
    health: {}, latest: { status: 'stale', source: 'esp32' }, socketState: 'connected'
  }).mode, 'stale');
  assert.equal(getSourcePresentation({
    health: {}, latest: { status: 'no data yet' }, socketState: 'disconnected'
  }).mode, 'waiting');
});

test('staleness follows the 10-second backend freshness window', () => {
  assert.equal(getSourcePresentation({ health: {}, latest: { timestamp: 1 }, ageMs: 10_001 }).mode, 'stale');
});

test('authorization and offline states are explicit', () => {
  assert.equal(getSourcePresentation({ health: null, latest: null, socketState: 'auth-required' }).mode, 'auth');
  assert.equal(getSourcePresentation({ health: { source_state: 'offline' }, latest: null }).mode, 'offline');
});

test('mesh node normalization accepts array and map payloads', () => {
  assert.deepEqual(normalizeMeshNodes({ nodes: [{ node_id: 2 }] }), [{ node_id: 2 }]);
  assert.deepEqual(normalizeMeshNodes({ nodes: { '7': { is_valid: true } } }), [{ node_id: '7', is_valid: true }]);
  assert.deepEqual(normalizeMeshNodes({ nodes: null }), []);
});

test('signal field validation rejects malformed or unbounded payloads', () => {
  assert.equal(hasUsableSignalField({ signal_field: { grid_size: [2, 1, 2], values: [0, 0.3, 0.7, 1] } }), true);
  assert.equal(hasUsableSignalField({ signal_field: { grid_size: [2, 1, 2], values: [0, 0.3, Infinity, 1] } }), false);
  assert.equal(hasUsableSignalField({ signal_field: { grid_size: [257, 1, 257], values: Array(257 * 257).fill(0) } }), false);
  assert.equal(hasUsableSignalField({ signal_field: { grid_size: [2, 1, 2], values: [0, 1] } }), false);
});

test('capabilities distinguish available, unavailable, and unknown', () => {
  assert.equal(capabilityState({ wifi_sensing: true }, 'wifi_sensing'), 'available');
  assert.equal(capabilityState({ person_localization: false }, 'person_localization'), 'unavailable');
  assert.equal(capabilityState({}, 'vitals'), 'unknown');
  assert.equal(capabilityState(null, 'vitals'), 'unknown');
});