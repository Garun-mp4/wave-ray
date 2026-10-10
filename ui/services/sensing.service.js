import { resolveWsUrl, probeWsAuthRequired } from './ws-ticket.js';
import { apiService } from './api.service.js';
/**
 * Sensing WebSocket Service
 *
 * Manages the connection to the Go sensing WebSocket server
 * (ws://localhost:8765) and provides a callback-based API for the UI.
 *
 * Reconnects after outages without generating replacement measurements.
 */

const SENSING_WS_PORT_BY_HTTP_PORT = {
  // Docker image: HTTP UI/API on 3000, sensing stream on 3001.
  '3000': '3001',
  // Python sensing stack: UI on 8080, sensing stream on 8765.
  '8080': '8765',
};

export function buildSensingWsUrl(locationLike = (typeof window !== 'undefined' ? window.location : null)) {
  const protocol = locationLike && locationLike.protocol === 'https:' ? 'wss:' : 'ws:';
  const host = locationLike && locationLike.host ? locationLike.host : 'localhost:3001';
  const hostname = locationLike && locationLike.hostname ? locationLike.hostname : host.split(':')[0];
  const port = locationLike && locationLike.port ? locationLike.port : '';
  const wsPort = SENSING_WS_PORT_BY_HTTP_PORT[port];
  const wsHost = wsPort ? `${hostname}:${wsPort}` : host;

  return `${protocol}//${wsHost}/ws/sensing`;
}

const SENSING_WS_URL = buildSensingWsUrl();
const RECONNECT_DELAYS = [1000, 2000, 4000, 8000, 16000];
const MAX_RECONNECT_ATTEMPTS = 20;
export class SensingService {
  constructor() {
    /** @type {WebSocket|null} */
    this._ws = null;
    this._listeners = new Set();
    this._stateListeners = new Set();
    this._reconnectAttempt = 0;
    this._reconnectTimer = null;

    // Guards the await window inside `_connect()` (see the comment there).
    this._connectInFlight = false;
    // Bumped per connect attempt and by `stop()`; lets a socket recognise that
    // it has been abandoned and stop driving reconnect state.
    this._connectEpoch = 0;
    // Connection state: disconnected | connecting | connected | reconnecting |
    // simulated | auth-required
    this._state = 'disconnected';
    // #2099: why the server refused us, while in `auth-required`:
    //   'missing'  — the server needs a token and the browser has none
    //   'rejected' — the stored token was refused
    this._authRequired = null;
    // Data-source label exposed to the UI:
    //   "live"              — real ESP32 hardware connected
    //   "server-simulated"  — server is running but using synthetic data (no hardware)
    //   "reconnecting"      — WebSocket disconnected, retrying
    //   "simulated"         — client-side fallback simulation (server unreachable)
    //   "auth-required"     — the server refused us for lack of a valid token
    this._dataSource = 'reconnecting';
    // The raw source string from the server (e.g. "esp32", "simulated", "simulate")
    this._serverSource = null;
    this._lastMessage = null;

    // Ring buffer of recent RSSI values for sparkline
    this._rssiHistory = [];
    this._maxHistory = 60;
  }

  // ---- Public API --------------------------------------------------------

  /** Connect to the measurement stream. Outages never generate frames. */
  start() {
    void this._connect();
  }

  /** Stop the service entirely. */
  stop() {
    // Invalidate any attempt still inside its await window, so a socket opened
    // after this call does not resurrect the service.
    this._connectEpoch++;
    this._connectInFlight = false;
    this._clearTimers();
    if (this._ws) {
      this._ws.close(1000, 'client stop');
      this._ws = null;
    }
    this._setState('disconnected');
  }

  /** Register a callback for sensing data updates. Returns unsubscribe fn. */
  onData(callback) {
    this._listeners.add(callback);
    // Immediately push last known data if available
    if (this._lastMessage) callback(this._lastMessage);
    return () => this._listeners.delete(callback);
  }

  /** Register a callback for connection state changes. Returns unsubscribe fn. */
  onStateChange(callback) {
    this._stateListeners.add(callback);
    callback(this._state);
    return () => this._stateListeners.delete(callback);
  }

  /** Get the RSSI sparkline history (array of floats). */
  getRssiHistory() {
    return [...this._rssiHistory];
  }

  /** Get per-node RSSI history (object keyed by node_id). */
  getPerNodeRssiHistory() {
    return { ...(this._perNodeRssiHistory || {}) };
  }

  /** Current connection state. */
  get state() {
    return this._state;
  }

  /**
   * #2099: null, or why the server refused the stream —
   * 'missing' (no token in this browser) or 'rejected' (stored token refused).
   */
  get authRequired() {
    return this._authRequired;
  }

  /**
   * Current data source label.
   * "live"         — frames are arriving from the real ESP32 over WebSocket
   * "reconnecting" — WebSocket disconnected; actively retrying, no frames emitted
   * "unreachable"  — retries exhausted, client simulation off; NO frames emitted
   *                  and whatever is on screen is the last live reading, stale
   * "simulated"    — client simulation explicitly enabled and running; every
   *                  frame on screen is invented
   */
  get dataSource() {
    return this._dataSource;
  }

  // ---- Connection --------------------------------------------------------

  // async because the server gates `/ws/sensing` (ADR-272) and a browser
  // cannot set an Authorization header on an upgrade — so we mint a
  // single-use ticket first. Minted per connect attempt, never cached: a
  // ticket is valid once and expires in seconds, so reusing one across
  // reconnects would fail on the second attempt.
  async _connect() {
    if (this._ws && this._ws.readyState <= WebSocket.OPEN) return;
    // The readyState guard above cannot stand alone: this function awaits
    // ticket minting before it assigns `this._ws`, so two callers entering
    // during that window BOTH pass the guard and BOTH open a socket. The
    // second assignment orphans the first — a socket nobody references, still
    // open on the server, whose `onclose` fires later and calls
    // `_scheduleReconnect()` even though a healthy socket is live. That walks
    // `_reconnectAttempt` upward with no outage and eventually trips the
    // simulation fallback. Observed on the server as connect/disconnect churn
    // on every page load.
    if (this._connectInFlight) return;
    this._connectInFlight = true;

    // Identifies this attempt for the lifetime of its socket. `stop()` and any
    // superseding attempt bump it, so a late event from an abandoned socket is
    // recognised and ignored rather than driving reconnect state.
    const epoch = ++this._connectEpoch;

    this._setState('connecting');

    let url = SENSING_WS_URL;
    let authRequired = null;
    try {
      ({ url, authRequired } = await resolveWsUrl(SENSING_WS_URL));
    } catch {
      // Ticket minting is best-effort: against a server with auth off, or one
      // predating ADR-272, connecting without a ticket is correct.
    }

    if (epoch !== this._connectEpoch) {
      // Superseded or stopped while minting. The ticket is single-use and now
      // spent; the current attempt mints its own.
      this._connectInFlight = false;
      return;
    }

    if (authRequired) {
      // #2099: the stored token was refused. Retrying with it cannot succeed.
      this._connectInFlight = false;
      this._enterAuthRequired(authRequired);
      return;
    }
    const ticketed = url !== SENSING_WS_URL;
    let opened = false;

    let ws;
    try {
      ws = new WebSocket(url);
    } catch (err) {
      console.warn('[Sensing] WebSocket constructor failed:', err.message);
      this._connectInFlight = false;
      this._setDataSource('unreachable');
      this._scheduleReconnect();
      return;
    }

    this._ws = ws;
    this._connectInFlight = false;

    /** True while `ws` is still the socket this service is driving. */
    const isCurrent = () => epoch === this._connectEpoch && this._ws === ws;

    ws.onopen = () => {
      if (!isCurrent()) {
        ws.close(1000, 'superseded');
        return;
      }
      console.info('[Sensing] Connected to', SENSING_WS_URL);
      opened = true;
      this._authRequired = null;
      this._reconnectAttempt = 0;

      this._setState('connected');
      // Don't assume "live" yet — wait for first frame's source field.
      // Fetch server status to determine actual data source immediately.
      this._detectServerSource();
    };

    ws.onmessage = (evt) => {
      if (!isCurrent()) return;
      try {
        const data = JSON.parse(evt.data);
        this._handleData(data);
      } catch (e) {
        console.warn('[Sensing] Invalid message:', e.message);
      }
    };

    ws.onerror = () => {
      // onerror is always followed by onclose, so we handle reconnect there
    };

    ws.onclose = (evt) => {
      if (!isCurrent()) {
        // An abandoned socket closing is not an outage. Reconnect state
        // belongs to whichever attempt is current.
        console.debug('[Sensing] Ignoring close from superseded socket');
        return;
      }
      console.info('[Sensing] Connection closed (code=%d)', evt.code);
      this._ws = null;
      if (evt.code !== 1000 && !opened && !ticketed) {
        // #2099: a refused upgrade looks like any other failure from here, so
        // ask the server whether it wants a token before retrying forever.
        void this._handleRefusedConnect(epoch);
      } else if (evt.code !== 1000) {
        this._scheduleReconnect();
      } else {
        this._setState('disconnected');
        this._setDataSource('reconnecting');
      }
    };
  }

  async _handleRefusedConnect(epoch) {
    const reason = await probeWsAuthRequired();
    // stop(), reconnect() or another attempt took over during the probe.
    if (epoch !== this._connectEpoch || this._ws) return;
    if (reason) {
      this._enterAuthRequired(reason);
    } else {
      this._scheduleReconnect();
    }
  }

  /**
   * #2099: stop the retry loop and say why. Nothing is retried until the user
   * supplies a token (QuickSettings reloads the page on save) or asks for a
   * reconnect.
   */
  _enterAuthRequired(reason) {
    console.warn('[Sensing] Server requires an API token (%s); not retrying', reason);
    this._clearTimers();
    this._reconnectAttempt = 0;
    this._authRequired = reason;
    this._setState('auth-required');
    this._setDataSource('auth-required');
  }

  /** Retry now, e.g. from the header's reconnect button. */
  reconnect() {
    this._clearTimers();
    this._authRequired = null;
    this._reconnectAttempt = 0;
    void this._connect();
  }

  _scheduleReconnect() {
    // A retry is already pending; stacking timers would multiply the attempt
    // count for a single outage.
    if (this._reconnectTimer) return;

    if (this._reconnectAttempt >= MAX_RECONNECT_ATTEMPTS) {
      // With simulation off there is nothing to give up *for*: keep retrying at
      // the ceiling delay so the UI recovers by itself when the server returns,
      // rather than requiring a hard refresh.
      this._setDataSource('unreachable');
    }

    const delay = RECONNECT_DELAYS[Math.min(this._reconnectAttempt, RECONNECT_DELAYS.length - 1)];
    // Hold at the cap rather than counting into the thousands over a long
    // outage; the delay is already saturated and the number is only read by a
    // human in the console.
    this._reconnectAttempt = Math.min(this._reconnectAttempt + 1, MAX_RECONNECT_ATTEMPTS);
    console.info('[Sensing] Reconnecting in %dms (attempt %d/%d)', delay, this._reconnectAttempt, MAX_RECONNECT_ATTEMPTS);

    this._setState('reconnecting');
    if (this._dataSource !== 'unreachable') this._setDataSource('reconnecting');

    this._reconnectTimer = setTimeout(() => {
      this._reconnectTimer = null;
      void this._connect();
    }, delay);

  }

  // ---- Server source detection -------------------------------------------

  /**
   * Fetch `/api/v1/status` to find out if the server is using real
   * hardware or simulation. Called once on WebSocket open.
   */
  async _detectServerSource() {
    // ADR-295 (issue #1526): an unreachable or unauthorized status endpoint is
    // an *unknown* state — it must NOT collapse to "live". Prefer the canonical
    // `source_state` the server now returns; on any error stay conservative
    // until a real frame's `source` field promotes us.
    //
    // Send the bearer token via `apiService.getHeaders()` so the probe can
    // actually succeed under the documented secure posture (API auth
    // enabled) instead of always 401ing and relying solely on the
    // conservative fallback below (issue #1526, suggested fix #1).
    try {
      const resp = await fetch('/api/v1/status', { headers: apiService.getHeaders() });
      if (resp.ok) {
        const json = await resp.json();
        this._applyServerSource(json.source, json.source_state);
      } else {
        this._setDataSource('unreachable');
      }
    } catch {
      this._setDataSource('unreachable');
    }
  }

  /**
   * Map a raw server source string (and optional canonical ADR-295
   * `source_state`) to the UI data-source label.
   */
  _applyServerSource(rawSource, sourceState) {
    this._serverSource = rawSource;
    if (/simulat|synthetic|demo|replay/i.test(rawSource || '') || sourceState === 'synthetic') {
      this._setDataSource('server-simulated');
    } else if (sourceState === 'disconnected' || sourceState === 'offline' || sourceState === 'stale' || String(rawSource).endsWith(':offline')) {
      this._setDataSource('reconnecting');
    } else if (['esp32', 'wifi', 'live'].includes(rawSource) || sourceState === 'live_verified' || sourceState === 'live_unverified' || sourceState === 'connected') {
      this._setDataSource('live');
    } else {
      this._setDataSource('reconnecting');
    }
  }

  /** @return {string|null} Raw server source (e.g. "esp32", "simulated") */
  get serverSource() {
    return this._serverSource;
  }

  // ---- Data handling -----------------------------------------------------

  _handleData(data) {
    const frame = data?.data && typeof data.data === 'object' ? data.data : data;
    if (!frame || frame._simulated || frame.source_state === 'synthetic' || /simulat|synthetic|replay|demo/i.test(frame.source || '')) {
      this._lastMessage = null;
      this._setDataSource('unreachable');
      return;
    }
    this._lastMessage = data;

    // Track the server's source field from each frame so the UI
    // can react if the server switches between esp32 ↔ simulated at runtime.
    if (data.source && this._state === 'connected') {
      const raw = data.source;
      if (raw !== this._serverSource) {
        this._applyServerSource(raw);
      }
    }

    // Update RSSI history for sparkline
    if (data.features && data.features.mean_rssi != null) {
      this._rssiHistory.push(data.features.mean_rssi);
      if (this._rssiHistory.length > this._maxHistory) {
        this._rssiHistory.shift();
      }
    }

    // Per-node RSSI tracking
    if (!this._perNodeRssiHistory) this._perNodeRssiHistory = {};
    if (data.node_features) {
      for (const nf of data.node_features) {
        if (!this._perNodeRssiHistory[nf.node_id]) {
          this._perNodeRssiHistory[nf.node_id] = [];
        }
        this._perNodeRssiHistory[nf.node_id].push(nf.rssi_dbm);
        if (this._perNodeRssiHistory[nf.node_id].length > this._maxHistory) {
          this._perNodeRssiHistory[nf.node_id].shift();
        }
      }
    }

    // Notify all listeners
    for (const cb of this._listeners) {
      try {
        cb(data);
      } catch (e) {
        console.error('[Sensing] Listener error:', e);
      }
    }
  }

  // ---- State management --------------------------------------------------

  _setState(newState) {
    if (newState === this._state) return;
    this._state = newState;
    for (const cb of this._stateListeners) {
      try { cb(newState); } catch (e) { /* ignore */ }
    }
  }

  /**
   * Update the dataSource label and notify state listeners so the UI can
   * react without needing a separate subscription.
   * @param {'live'|'server-simulated'|'reconnecting'|'unreachable'|'simulated'|'auth-required'} source
   */
  _setDataSource(source) {
    if (source === this._dataSource) return;
    this._dataSource = source;
    // Re-use the same state-listener channel — listeners receive the
    // connection state but can read dataSource via service.dataSource.
    for (const cb of this._stateListeners) {
      try { cb(this._state); } catch (e) { /* ignore */ }
    }
  }

  _clearTimers() {

    if (this._reconnectTimer) {
      clearTimeout(this._reconnectTimer);
      this._reconnectTimer = null;
    }
  }
}

// Singleton
export const sensingService = new SensingService();
