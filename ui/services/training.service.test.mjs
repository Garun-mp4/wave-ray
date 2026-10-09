// Progress WebSocket contract checks for the Rust and Go training APIs.
//
// Run: node --test ui/services/training.service.test.mjs

import { test, beforeEach, afterEach } from 'node:test';
import assert from 'node:assert/strict';

const TOKEN_KEY = 'ruview-api-token';

let stored = {};
let ticketRequests = [];
let sockets = [];
let ticketSequence = 0;
const ticketFetch = async (path, init = {}) => {
  ticketRequests.push([path, init]);
  if (path !== '/api/v1/ws-ticket') return { ok: false, status: 404, json: async () => ({}) };
  return {
    ok: true,
    status: 200,
    json: async () => ({ ticket: `ticket-${++ticketSequence}` }),
  };
};

globalThis.localStorage = {
  getItem: (key) => (key in stored ? stored[key] : null),
  setItem: (key, value) => { stored[key] = String(value); },
  removeItem: (key) => { delete stored[key]; },
};
globalThis.window = { location: { protocol: 'http:', host: 'localhost:3000' } };
globalThis.fetch = ticketFetch;

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;

  constructor(url) {
    this.url = url;
    this.readyState = FakeWebSocket.CONNECTING;
    this.sent = [];
    sockets.push(this);
    queueMicrotask(() => {
      if (this.readyState !== FakeWebSocket.CONNECTING) return;
      this.readyState = FakeWebSocket.OPEN;
      this.onopen?.();
    });
  }

  send(message) { this.sent.push(message); }

  close(code = 1000, reason = '') {
    if (this.readyState === FakeWebSocket.CLOSED) return;
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code, reason });
  }

  receive(data) { this.onmessage?.({ data: JSON.stringify(data) }); }

  serverClose(code = 1006) {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code });
  }
}
globalThis.WebSocket = FakeWebSocket;

const { TrainingService } = await import('./training.service.js');

const waitFor = async (predicate, timeoutMs = 1500) => {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error('condition did not become true before timeout');
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
};

let service;

beforeEach(() => {
  stored = { [TOKEN_KEY]: 'long-lived-secret' };
  ticketRequests = [];
  sockets = [];
  ticketSequence = 0;
  globalThis.fetch = ticketFetch;
  service = new TrainingService();
});

afterEach(() => service.dispose());

test('progress stream exchanges bearer for a ticket and unwraps Rust/Go event envelopes', async () => {
  const progress = [];
  const raw = [];
  service.on('progress', (data) => progress.push(data));
  service.on('progress-message', (data) => raw.push(data));

  await service.connectProgressStream();
  await waitFor(() => sockets.length === 1 && sockets[0].readyState === FakeWebSocket.OPEN);

  assert.equal(sockets[0].url, 'ws://localhost:3000/ws/train/progress?ticket=ticket-1');
  assert.equal(ticketRequests.length, 1);
  assert.equal(ticketRequests[0][0], '/api/v1/ws-ticket');
  assert.equal(ticketRequests[0][1].headers.Authorization, 'Bearer long-lived-secret');
  assert.ok(!sockets[0].url.includes('long-lived-secret'), 'bearer token must not be placed in the URL');

  const envelope = { type: 'progress', data: { epoch: 4, train_loss: 0.25 } };
  sockets[0].receive(envelope);
  assert.deepEqual(progress, [envelope.data]);
  assert.deepEqual(raw, [envelope]);

  sockets[0].serverClose();
  await waitFor(() => sockets.length === 2);
  await waitFor(() => sockets[1].readyState === FakeWebSocket.OPEN);
  assert.equal(sockets[1].url, 'ws://localhost:3000/ws/train/progress?ticket=ticket-2');
  assert.equal(ticketRequests.length, 2, 'reconnect must mint a new single-use ticket');
});

test('a rejected stored bearer stops retries and signals the UI', async () => {
  globalThis.fetch = async (path, init = {}) => {
    ticketRequests.push([path, init]);
    return { ok: false, status: 401, json: async () => ({}) };
  };
  const authReasons = [];
  service.on('progress-auth-required', ({ reason }) => authReasons.push(reason));

  assert.equal(await service.connectProgressStream(), null);
  assert.deepEqual(authReasons, ['rejected']);
  assert.equal(sockets.length, 0, 'do not open or retry a socket with a rejected token');
  assert.equal(service.progressReconnectTimer, null);
});
