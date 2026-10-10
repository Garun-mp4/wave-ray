const SIMULATED_SOURCES = new Set(['simulate', 'simulated', 'simulation']);

export function escapeHtml(value) {
  return String(value == null ? '' : value).replace(/[&<>"']/g, character => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;'
  })[character]);
}

export function formatAge(milliseconds) {
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return '—';
  if (milliseconds < 1000) return 'just now';
  const seconds = Math.floor(milliseconds / 1000);
  if (seconds < 60) return seconds + 's ago';
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return minutes + 'm ago';
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return hours + 'h ago';
  return Math.floor(hours / 24) + 'd ago';
}

export function formatBytes(value) {
  if (!Number.isFinite(value) || value < 0) return '—';
  if (value < 1024) return value + ' B';
  if (value < 1024 * 1024) return (value / 1024).toFixed(1) + ' KB';
  return (value / (1024 * 1024)).toFixed(1) + ' MB';
}

export function formatDate(value) {
  if (!value) return '—';
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return '—';
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

export function getSourcePresentation({ health, latest, ageMs, socketState, dataSource }) {
  const source = String((latest && latest.source) || (health && health.source) || '').toLowerCase();
  const sourceState = String((health && health.source_state) || '').toLowerCase();
  if ((latest && latest._simulated) || SIMULATED_SOURCES.has(source) || sourceState === 'synthetic' ||
      dataSource === 'server-simulated' || dataSource === 'simulated') {
    return { label: 'Synthetic', detail: 'Generated data · not a measurement', tone: 'warning', mode: 'synthetic' };
  }
  if (socketState === 'auth-required' || dataSource === 'auth-required') {
    return { label: 'Authorization required', detail: 'Add a server API token in Settings', tone: 'danger', mode: 'auth' };
  }
  if (latest && latest.status === 'stale' || Number.isFinite(ageMs) && ageMs > 10_000) {
    return { label: 'Stale', detail: 'Last frame is outside the server freshness window', tone: 'warning', mode: 'stale' };
  }
  if (source.endsWith(':offline') || sourceState === 'offline' || dataSource === 'unreachable') {
    return { label: 'No sensor data', detail: 'Server is running without a live source', tone: 'muted', mode: 'offline' };
  }
  if (latest && latest.timestamp) {
    const mode = String(latest.input_mode || '').toLowerCase();
    const isCsi = mode.includes('csi') || source.includes('esp32');
    return {
      label: isCsi ? 'Device stream' : 'Signal stream',
      detail: isCsi ? 'Frame received · device evidence is not person localization' : 'Frame received · signal-derived data',
      tone: 'success',
      mode: isCsi ? 'device' : 'signal'
    };
  }
  if (health) {
    return {
      label: socketState === 'connected' ? 'Connected · awaiting frames' : 'Waiting for frames',
      detail: source ? 'Configured source: ' + source : 'No recent sensing frame is available',
      tone: 'muted',
      mode: 'waiting'
    };
  }
  return { label: 'Server unavailable', detail: 'No current API response', tone: 'danger', mode: 'unavailable' };
}

export function normalizeMeshNodes(payload) {
  const nodes = payload && payload.nodes;
  if (Array.isArray(nodes)) return nodes.map(item => ({ ...item }));
  if (!nodes || typeof nodes !== 'object') return [];
  return Object.entries(nodes).map(([nodeId, item]) => ({ node_id: nodeId, ...item }));
}

export function hasUsableSignalField(payload) {
  const field = payload && payload.signal_field;
  if (!field || !Array.isArray(field.grid_size) || !Array.isArray(field.values)) return false;
  const size = field.grid_size;
  const cellCount = size[0] * size[2];
  return Number.isInteger(size[0]) && Number.isInteger(size[2]) &&
    size[0] > 0 && size[2] > 0 && size[0] <= 256 && size[2] <= 256 &&
    cellCount <= 16384 && cellCount === field.values.length &&
    field.values.every(value => Number.isFinite(value));
}

export function capabilityState(features, key) {
  if (!features || !(key in features)) return 'unknown';
  return features[key] === true ? 'available' : 'unavailable';
}
