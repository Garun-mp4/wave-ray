import test from 'node:test';
import assert from 'node:assert/strict';

const values = new Map();
globalThis.localStorage = {
  getItem(key) { return values.get(key) ?? null; },
  setItem(key, value) { values.set(key, String(value)); },
  removeItem(key) { values.delete(key); }
};
globalThis.window = { location: { origin: 'http://127.0.0.1:4173' } };

const { api, request, WorkspaceApiError } = await import('./api.js');

test('request only allows same-origin API paths', async () => {
  await assert.rejects(request('https://example.invalid/api/v1/info'), TypeError);
  await assert.rejects(request('/workspace/app.js'), TypeError);
  await assert.rejects(request('//example.invalid/api/v1/info'), TypeError);
});

test('request attaches the locally stored bearer token and encodes JSON', async () => {
  values.set('ruview-api-token', 'local-test-token');
  let captured;
  globalThis.fetch = async (url, init) => {
    captured = { url, init };
    return new Response(JSON.stringify({ ok: true }), {
      status: 200,
      headers: { 'content-type': 'application/json' }
    });
  };
  const payload = await api.post('/api/v1/calibration/start', { source_node_ids: [1, 2] });
  assert.deepEqual(payload, { ok: true });
  assert.equal(captured.init.headers.get('Authorization'), 'Bearer local-test-token');
  assert.equal(captured.init.headers.get('Content-Type'), 'application/json');
  assert.equal(captured.init.body, JSON.stringify({ source_node_ids: [1, 2] }));
  assert.equal(captured.url.origin, window.location.origin);
  assert.equal(JSON.stringify(captured).includes('local-test-token'), false);
});

test('API errors preserve status and code for explicit UI states', async () => {
  values.clear();
  globalThis.fetch = async () => new Response(JSON.stringify({
    error: 'token required', error_code: 'unauthorized'
  }), { status: 401, headers: { 'content-type': 'application/json' } });
  await assert.rejects(api.get('/health/ready'), error => {
    assert.ok(error instanceof WorkspaceApiError);
    assert.equal(error.status, 401);
    assert.equal(error.code, 'unauthorized');
    assert.equal(error.message, 'token required');
    return true;
  });
});

test('network failures become a bounded offline error', async () => {
  globalThis.fetch = async () => { throw new Error('internal detail'); };
  await assert.rejects(api.get('/api/v1/info'), error => {
    assert.ok(error instanceof WorkspaceApiError);
    assert.equal(error.status, 0);
    assert.equal(error.code, 'network_unavailable');
    assert.equal(error.message.includes('internal detail'), false);
    return true;
  });
});