import { api } from './api.js';
import {
  escapeHtml, formatAge, formatBytes, formatDate,
  getSourcePresentation, hasUsableSignalField, normalizeMeshNodes
} from './view-model.js';
import { apiService, API_TOKEN_STORAGE_KEY } from '../services/api.service.js';
import { sensingService } from '../services/sensing.service.js?v=2';
import { renderSpace, mountSpace } from './space.js?v=3';

let spaceView = null;

const ROUTES = {
  overview: { title: 'Monitor', description: 'Your signal source, current readings, and next steps.' },
  space: { title: 'Space', description: 'Arrange your ESP32 sensors and access points in 3D. Coordinates are relative to your installation origin.' },
  devices: { title: 'Devices', description: 'Check node freshness, CSI sequence, and mesh clock synchronization reported by the server.' },
  calibration: { title: 'Calibration', description: 'Bind selected node IDs to a room reference, capture a stable CSI grid, and inspect the saved baseline and trust state.' },
  recordings: { title: 'Recordings', description: 'Record incoming server frames and inspect the labeled datasets available to the activity trainer.' },
  models: { title: 'Models', description: 'Manage registered artifacts and train the supported activity classifier from eligible labeled recordings.' },
  integrations: { title: 'Integrations', description: 'Review provider capabilities and server-side integration status. Credentials stay in the backend environment.' },
  settings: { title: 'Settings', description: 'Manage server access, browser preferences, and integrations.' },
  help: { title: 'Help & capabilities', description: 'A short guide to signal sources, calibration, synthetic data, and what this backend can currently establish.' }
};

const state = {
  route: 'overview',
  health: null,
  healthError: null,
  info: null,
  infoError: null,
  latest: null,
  latestError: null,
  socketState: 'disconnected',
  dataSource: 'reconnecting',
  page: {},
  busy: false,
  flash: null,
  pendingRouteRequest: 0,
  pendingGlobalRequest: 0,
  lastGlobalRefresh: 0
};

try {
  apiService.setAuthToken(localStorage.getItem(API_TOKEN_STORAGE_KEY) || null);
} catch {
  apiService.setAuthToken(null);
}

const main = document.getElementById('main');
const toastRegion = document.getElementById('toast-region');
const icon = name => '<svg aria-hidden="true"><use href="workspace/icons.svg#icon-' + safe(name).replace(/-/g, '_') + '"></use></svg>';
const safe = value => escapeHtml(value);

function button(label, action, variant, extra) {
  return '<button type="button" class="button ' + (variant || '') + '" data-action="' + safe(action) + '" ' + (extra || '') + '>' + label + '</button>';
}

function pageHeader(route, actions) {
  const meta = ROUTES[route];
  return '<header class="page-header"><div class="page-heading"><h1 class="page-title">' + safe(meta.title) + '</h1><p class="page-description">' +
    safe(meta.description) + '</p></div>' + (actions ? '<div class="page-actions">' + actions + '</div>' : '') + '</header>';
}

function card(title, body, options) {
  const opts = options || {};
  const subtitle = opts.subtitle ? '<p class="card-subtitle">' + safe(opts.subtitle) + '</p>' : '';
  const extra = opts.extra || '';
  const bodyClass = opts.bodyClass || 'card-body';
  return '<section class="card ' + (opts.className || '') + '"><header class="card-header"><div class="card-header-copy"><h2 class="card-title">' +
    safe(title) + '</h2>' + subtitle + '</div>' + extra + '</header><div class="' + bodyClass + '">' + body +
    '</div>' + (opts.footer ? '<footer class="card-footer">' + opts.footer + '</footer>' : '') + '</section>';
}

function disclosure(title, body, extra = '') {
  return '<details class="advanced-section ' + extra + '"><summary>' + safe(title) + '</summary><div class="advanced-body">' + body + '</div></details>';
}

function signalLabel(value) {
  const labels = { absent: 'Quiet signal', present_still: 'Still signal', moving: 'Motion detected' };
  return labels[value] || String(value || '—').replaceAll('_', ' ');
}

function badge(label, tone) {
  const allowed = new Set(['success', 'warning', 'danger', 'info', 'neutral']);
  const name = allowed.has(tone) ? tone : 'neutral';
  return '<span class="badge badge-' + name + '">' + safe(label) + '</span>';
}

function banner(message, tone, withIcon) {
  const klass = tone === 'danger' ? 'error-banner' : tone === 'warning' ? 'warning-banner' : '';
  return '<div class="info-banner ' + klass + '">' + (withIcon === false ? '' : icon('circle-alert')) +
    '<span>' + safe(message) + '</span></div>';
}

function emptyState(iconName, title, message, action) {
  return '<div class="empty-state"><span class="empty-state-icon">' + icon(iconName) +
    '</span><h3>' + safe(title) + '</h3><p>' + safe(message) + '</p>' + (action || '') + '</div>';
}

function currentLatest() {
  const frame = state.latest;
  if (!frame || typeof frame !== 'object' || frame._simulated || frame.source_state === 'synthetic' ||
      /simulat|synthetic|replay|demo/i.test(frame.source || '')) return null;
  return frame;
}

function sourcePresentation() {
  if ([state.healthError, state.infoError, state.latestError].some(error => error && error.status === 401)) {
    return { label: 'Authorization required', detail: 'Add a server API token in Settings', tone: 'danger', mode: 'auth' };
  }
  return getSourcePresentation({
    health: state.health,
    latest: currentLatest(),
    ageMs: currentAge(),
    socketState: state.socketState,
    dataSource: state.dataSource
  });
}

function currentAge() {
  const latest = currentLatest();
  if (latest && Number.isFinite(latest.timestamp)) return Math.max(0, Date.now() - latest.timestamp * 1000);
  if (latest && Number.isFinite(latest.age_ms)) return latest.age_ms;
  if (state.health && Number.isFinite(state.health.last_frame_age_ms)) return state.health.last_frame_age_ms;
  return null;
}

function displayNumber(value, digits, unit) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
  return value.toFixed(digits == null ? 1 : digits) + (unit || '');
}

function setGlobalStatus() {
  const health = state.health;
  const apiStatus = document.getElementById('api-status-indicator');
  const apiText = document.getElementById('api-status-text');
  const sourceChip = document.getElementById('source-chip');
  const sourceText = document.getElementById('source-chip-text');
  const freshness = document.getElementById('freshness-text');
  const source = sourcePresentation();
  const isUp = Boolean(health);
  const auth = (state.healthError && state.healthError.status === 401) ||
    (state.infoError && state.infoError.status === 401);

  if (apiStatus && apiText) {
    apiStatus.classList.toggle('is-up', isUp && !auth);
    apiStatus.classList.toggle('is-down', !isUp && !auth);
    apiStatus.classList.toggle('is-warning', auth);
    apiText.textContent = auth ? 'Authorization needed' : isUp ? 'API online' : 'API offline';
  }
  if (sourceChip && sourceText) {
    sourceChip.className = 'source-chip ' + (source.tone === 'success' ? 'is-success' :
      source.tone === 'warning' ? 'is-warning' : source.tone === 'danger' ? 'is-danger' : '');
    sourceText.textContent = source.label;
  }
  if (freshness) {
    const age = currentAge();
    freshness.textContent = age == null ? (health ? 'Waiting for frames' : 'No API response') : formatAge(age);
  }

  const notice = document.getElementById('source-notice');
  if (notice) {
    const show = source.mode === 'synthetic' || source.mode === 'stale' ||
      source.mode === 'offline' || source.mode === 'auth' || source.mode === 'unavailable';
    notice.hidden = !show;
    notice.className = 'source-notice tone-' + (source.tone || 'muted');
    notice.textContent = source.label + ' — ' + source.detail;
  }

  const dot = document.getElementById('sidebar-runtime-dot');
  const title = document.getElementById('sidebar-runtime-title');
  const detail = document.getElementById('sidebar-runtime-detail');
  if (dot && title && detail) {
    dot.className = 'runtime-dot ' + (isUp ? (auth ? 'is-warning' : 'is-up') : 'is-down');
    title.textContent = auth ? 'Token required' : isUp ? 'Go backend online' : 'Backend unavailable';
    detail.textContent = health
      ? (state.info && state.info.version ? 'API ' + state.info.version : 'Health check passed')
      : (state.healthError ? state.healthError.message : 'Waiting for API response');
  }
  const version = document.getElementById('footer-version');
  if (version) version.textContent = state.info && state.info.version
    ? 'Go backend ' + state.info.version
    : 'Backend version unavailable';
  updateLiveOverviewFields();
}

function updateLiveOverviewFields() {
  const latest = currentLatest();
  const features = latest && latest.features;
  const age = currentAge();
  const source = sourcePresentation();
  const values = {
    'data-rssi': features ? displayNumber(features.mean_rssi, 1, ' dBm') : '—',
    'data-variance': features ? displayNumber(features.variance, 2) : '—',
    'data-motion': latest && latest.classification ? signalLabel(latest.classification.motion_level) : '—',
    'data-frame-age': age == null ? '—' : formatAge(age),
    'data-source-label': source.label,
    'data-source-detail': source.detail,
    'data-backend-source': state.health && state.health.source || '—',
    'data-source-age': age == null ? '—' : formatAge(age),
    'data-node-count': source.mode !== 'synthetic' && state.health && Number.isFinite(state.health.nodes) ? String(state.health.nodes) : '—',
    'data-measurement-mode': latest && latest.input_mode || 'awaiting frame',
    'data-frames': state.health && Number.isFinite(state.health.frames_received) ? String(state.health.frames_received) : '—'
  };
  Object.entries(values).forEach(([key, value]) => {
    const node = main.querySelector('[' + key + ']');
    if (node) node.textContent = value;
  });
  const sourceIcon = main.querySelector('[data-source-icon]');
  if (sourceIcon) {
    sourceIcon.className = 'source-status-icon' + (source.tone === 'success' ? ' is-success' : source.tone === 'warning' ? ' is-warning' : source.tone === 'danger' ? ' is-danger' : '');
    sourceIcon.innerHTML = icon(source.mode === 'synthetic' ? 'activity' : 'wifi');
  }
  const canvas = main.querySelector('#signal-field-canvas');
  if (canvas) paintSignalField(canvas, latest);
}

function paintSignalField(canvas, payload) {
  const empty = main.querySelector('#signal-empty');
  const field = payload && payload.signal_field;
  if (!hasUsableSignalField(payload)) {
    canvas.hidden = true;
    if (empty) empty.hidden = false;
    return;
  }

  const columns = field.grid_size[0];
  const rows = field.grid_size[2];
  const bounds = canvas.getBoundingClientRect();
  if (!bounds.width || !bounds.height) return;
  const pixelRatio = Math.min(window.devicePixelRatio || 1, 2);
  const width = Math.max(1, Math.round(bounds.width * pixelRatio));
  const height = Math.max(1, Math.round(bounds.height * pixelRatio));

  canvas.hidden = false;
  if (empty) empty.hidden = true;
  if (canvas.width !== width || canvas.height !== height) {
    canvas.width = width;
    canvas.height = height;
  }
  const context = canvas.getContext('2d');
  if (!context) return;

  context.clearRect(0, 0, width, height);
  const palette = getComputedStyle(document.documentElement);
  context.fillStyle = palette.getPropertyValue('--color-bg-marketing').trim();
  context.fillRect(0, 0, width, height);
  const light = document.documentElement.dataset.theme === 'light';
  const low = light ? [240, 241, 242] : [25, 26, 27];
  const high = [94, 106, 210];
  const cellWidth = width / columns;
  const cellHeight = height / rows;
  const gap = Math.max(1, Math.round(pixelRatio));
  for (let row = 0; row < rows; row += 1) {
    for (let column = 0; column < columns; column += 1) {
      const value = Math.max(0, Math.min(1, field.values[row * columns + column]));
      const mix = 0.06 + value * 0.28;
      const red = Math.round(low[0] + (high[0] - low[0]) * mix);
      const green = Math.round(low[1] + (high[1] - low[1]) * mix);
      const blue = Math.round(low[2] + (high[2] - low[2]) * mix);
      context.fillStyle = 'rgb(' + red + ' ' + green + ' ' + blue + ')';
      context.fillRect(
        column * cellWidth + gap / 2,
        row * cellHeight + gap / 2,
        Math.max(1, cellWidth - gap),
        Math.max(1, cellHeight - gap)
      );
    }
  }
}

function renderShell(route) {
  state.route = route;
  document.title = 'Wave Ray — ' + ROUTES[route].title;
  const heading = document.getElementById('breadcrumb-current');
  if (heading) heading.textContent = ROUTES[route].title;
  document.querySelectorAll('[data-route]').forEach(link => {
    if (link.dataset.route === route) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  });
  renderPage();
  closeMobileNav();
}

function renderPage() {
  const data = state.page[state.route] || {};
  let content;
  switch (state.route) {
    case 'overview': content = renderOverview(); break;
    case 'space': content = renderSpace(data); break;
    case 'devices': content = renderDevices(data); break;
    case 'calibration': content = renderCalibration(data); break;
    case 'recordings': content = renderRecordings(data); break;
    case 'models': content = renderModels(data); break;
    case 'integrations': content = renderIntegrations(data); break;
    case 'settings': content = renderSettings(); break;
    case 'help': content = renderHelp(); break;
    default: content = renderOverview();
  }
  if (data.loading && Object.keys(data).length === 1) content = '<div class="loading-view" role="status"><span class="loading-spinner"></span>Loading…</div>';
  const opened = state.renderedRoute === state.route ? Array.from(main.querySelectorAll('details[open]'), detail => detail.querySelector('summary')?.textContent) : [];
  spaceView?.dispose();
  spaceView = null;
  main.innerHTML = pageHeader(state.route, state.route === 'integrations' ? '<a class="button" href="#settings">Back to settings</a>' : '') + content;
  main.querySelectorAll('details').forEach(detail => { detail.open = opened.includes(detail.querySelector('summary')?.textContent); });
  state.renderedRoute = state.route;
  main.dataset.sourceMode = sourcePresentation().mode;
  if (state.route === 'overview') updateLiveOverviewFields();
  if (state.route === 'devices') setupDeviceFilter();
  if (state.route === 'space' && !data.loading) spaceView = mountSpace(main, data, layout => api.post('/api/v1/space/layout', layout));
}

function renderOverview() {
  const source = sourcePresentation();
  const latest = currentLatest();
  const feature = latest && latest.features;
  const synthetic = source.mode === 'synthetic';
  const nodeCount = !synthetic && state.health && Number.isFinite(state.health.nodes) ? state.health.nodes : null;
  const sourceBlock = '<section class="monitor-source"><div><h2 data-source-label>' + safe(source.label) + '</h2><p data-source-detail>' + safe(source.detail) + '</p></div><a class="button" href="' + (source.mode === 'auth' ? '#settings' : '#devices') + '">' + (source.mode === 'auth' ? 'Configure access' : 'View devices') + '</a></section>';
  const readings = '<section class="readings"><h2>Latest signal</h2><dl class="reading-list">' +
    '<div><dt>Motion label</dt><dd data-motion>' + safe(latest && latest.classification ? latest.classification.motion_level || '—' : '—') + '</dd></div>' +
    '<div><dt>Received signal strength</dt><dd data-rssi>' + safe(feature ? displayNumber(feature.mean_rssi, 1, ' dBm') : '—') + '</dd></div>' +
    '<div><dt>Signal variance</dt><dd data-variance>' + safe(feature ? displayNumber(feature.variance, 2) : '—') + '</dd></div>' +
    '<div><dt>Reporting nodes</dt><dd data-node-count>' + safe(nodeCount == null ? '—' : nodeCount) + '</dd></div>' +
    '<div><dt>Last frame</dt><dd data-source-age>' + safe(currentAge() == null ? '—' : formatAge(currentAge())) + '</dd></div></dl><p class="section-note">' + (synthetic ? 'Synthetic input from this server is blocked. No measurements are displayed.' : 'Signal features describe the radio channel; they do not locate a person.') + '</p></section>';
  const steps = '<section class="setup-guide"><h2>' + (synthetic ? 'Connect your sensors' : 'Continue setup') + '</h2><p>' + (synthetic ? 'Set the server source to esp32 and connect a CSI receiver.' : 'Check input quality before capturing a room baseline.') + '</p><ol class="setup-steps">' +
    '<li><a href="#devices"><strong>Check devices</strong><span>Confirm fresh CSI and synchronized nodes</span>' + icon('arrow-up-right') + '</a></li>' +
    '<li><a href="#calibration"><strong>Calibrate the room</strong><span>Bind sensors and capture a stable baseline</span>' + icon('arrow-up-right') + '</a></li>' +
    '<li><a href="#recordings"><strong>Record a sample</strong><span>Save labeled data for activity training</span>' + icon('arrow-up-right') + '</a></li></ol><a class="text-link" href="#help">Setup guide and measurement limits</a></section>';
  const fieldExists = hasUsableSignalField(latest);
  const diagnostics = '<details class="diagnostics"><summary>Signal diagnostics<span>Normalized grid · no metric coordinates</span></summary><div class="diagnostics-body"><div class="signal-canvas-wrap"><canvas id="signal-field-canvas" class="signal-canvas" aria-label="Normalized signal diagnostic; not a room map"></canvas><div id="signal-empty" class="canvas-empty"' + (fieldExists ? ' hidden' : '') + '><strong>No signal field available</strong><span>Waiting for a frame containing a diagnostic grid.</span></div></div><p class="section-note">Sample indices, not metres. This grid cannot show people, objects, or floors.</p></div></details>';
  return (['auth', 'offline', 'unavailable'].includes(source.mode) ? sourceBlock : '') + '<div class="monitor-columns">' + readings + steps + '</div><p class="measurement-limit">Person coordinates, pose, and calibrated heart or breathing rates are not available in this installation.</p>' + diagnostics;
}

function renderDevices(data) {
  const nodes = data.nodes || [];
  const mesh = data.meshNodes || [];
  const rows = nodes.map(node => {
    const sync = mesh.find(item => String(item.node_id) === String(node.node_id));
    const active = node.status === 'active';
    const fresh = node.csi_status === 'active';
    const sequence = node.csi_sequence == null ? '—' : String(node.csi_sequence);
    const fps = sync && Number.isFinite(sync.csi_fps_ema) ? sync.csi_fps_ema.toFixed(1) + ' Hz' : '—';
    const offset = sync && Number.isFinite(sync.offset_us) ? String(sync.offset_us) + ' μs' : '—';
    return '<tr data-device-row><td><span class="device-identity"><span class="device-avatar">' + icon('radio') +
      '</span><span class="table-primary">Node ' + safe(node.node_id) + '<span class="table-meta mono">ID ' + safe(node.node_id) +
      '</span></span></span></td><td>' + badge(active ? 'Active' : 'Stale', active ? 'success' : 'warning') +
      '</td><td>' + badge(fresh ? 'CSI active' : 'No fresh CSI', fresh ? 'info' : 'neutral') +
      '</td><td class="mono">' + safe(fresh ? formatAge(node.csi_last_seen_ms) : '—') +
      '</td><td class="mono">' + safe(sequence) + '</td><td class="mono">' + safe(fps) +
      '</td><td class="mono">' + safe(offset) + '</td><td>' + (node.position_configured ? 'Configured' : 'Not set') + '</td></tr>';
  }).join('');
  const nodeTable = nodes.length
    ? '<div class="table-wrap"><table class="data-table"><thead><tr><th>Device</th><th>Node</th><th>CSI</th><th>CSI last seen</th><th>Sequence</th><th>CSI rate</th><th>Clock offset</th><th>Position</th></tr></thead><tbody>' + rows + '</tbody></table></div>'
    : emptyState('radio', data.error ? 'Device list unavailable' : 'No devices have reported', data.error || 'Nodes appear after the server receives device telemetry or mesh sync.', '<a class="button button-small" href="#help">View setup guide</a>');
  const headerTools = '<label class="device-filter-label"><span class="visually-hidden">Filter devices</span><input class="text-input device-filter" type="search" placeholder="Filter nodes…" aria-label="Filter devices"></label>';
  const deviceCard = card('Nodes', nodeTable, { className: 'col-12', bodyClass: 'card-body-tight', subtitle: 'Status is based on server timestamps; configured position does not expose a coordinate.' });
  const meshRows = mesh.length
    ? '<div class="table-wrap"><table class="data-table"><thead><tr><th>Node</th><th>Role</th><th>Sync</th><th>Sequence</th><th>CSI rate</th><th>Sync age</th></tr></thead><tbody>' +
      mesh.map(node => '<tr><td class="table-primary">Node ' + safe(node.node_id) + '</td><td>' + (node.is_leader ? 'Leader' : 'Follower') +
        '</td><td>' + badge(node.is_valid ? 'Valid' : 'No fresh sync', node.is_valid ? 'success' : 'warning') +
        '</td><td class="mono">' + safe(node.sequence == null ? '—' : node.sequence) +
        '</td><td class="mono">' + safe(Number.isFinite(node.csi_fps_ema) ? node.csi_fps_ema.toFixed(1) + ' Hz' : '—') +
        '</td><td class="mono">' + safe(node.staleness_ms == null ? '—' : formatAge(node.staleness_ms)) + '</td></tr>').join('') +
      '</tbody></table></div>'
    : emptyState('radio', 'No mesh synchronization packets', 'Mesh timing appears only after nodes send sync beacons.');
  const error = data.error ? '<div class="col-12">' + banner(data.error, 'warning') + '</div>' : '';
  return '<div class="layout-grid">' + error +
    '<div class="col-12"><div class="device-summary"><span><strong>' + safe(nodes.length) + '</strong> known nodes</span><span><strong>' +
    safe(nodes.filter(node => node.status === 'active').length) + '</strong> recently active</span><span><strong>' +
    safe(nodes.filter(node => node.csi_status === 'active').length) + '</strong> with fresh CSI</span><span><strong>' +
    safe(mesh.length) + '</strong> mesh snapshots</span></div>' + headerTools + '</div>' + deviceCard + disclosure('Mesh synchronization details', meshRows, 'col-12') + '</div>';
}

function setupDeviceFilter() {
  const input = main.querySelector('.device-filter');
  if (!input) return;
  input.addEventListener('input', () => {
    const query = input.value.trim().toLowerCase();
    main.querySelectorAll('[data-device-row]').forEach(row => {
      row.hidden = query !== '' && !row.textContent.toLowerCase().includes(query);
    });
  });
}

function calibrationIdentity(status) {
  if (!status) return null;
  return {
    boot_epoch: status.boot_epoch,
    session_id: status.session_id || undefined,
    binding_digest: status.binding_digest || undefined,
    source_node_ids: Array.isArray(status.source_node_ids) ? status.source_node_ids : undefined
  };
}

function renderCalibration(data) {
  const status = data.status || null;
  const active = Boolean(status && status.active);
  const calibrated = Boolean(status && status.calibrated);
  const canReset = Boolean(status && status.model_id && status.boot_epoch);
  const synthetic = sourcePresentation().mode === 'synthetic';
  const nodes = state.page.devices && state.page.devices.nodes || [];

  const error = data.error ? banner(data.error, 'warning') : '';
  const selectedGrid = status && (status.source_grid || status.model_receipt && status.model_receipt.source_grid);
  const gridEvidence = status && status.grid_evidence;
  const candidateMarkup = selectedGrid
    ? '<dl class="kv-list"><div class="kv-row"><dt>Selected source node</dt><dd>Node ' + safe(status.source_node_id == null ? '—' : status.source_node_id) +
      '</dd></div><div class="kv-row"><dt>CSI grid</dt><dd>' + safe(selectedGrid.n_subcarriers) + ' subcarriers · PPDU ' + safe(selectedGrid.ppdu_type) +
      '</dd></div><div class="kv-row"><dt>Samples observed</dt><dd>' + safe(gridEvidence && gridEvidence.sample_count != null ? gridEvidence.sample_count : status.frame_count || '—') +
      '</dd></div><div class="kv-row"><dt>Observed rate</dt><dd>' + safe(gridEvidence && Number.isFinite(gridEvidence.rate_hz) ? gridEvidence.rate_hz.toFixed(2) + ' Hz' : '—') +
      '</dd></div><div class="kv-row"><dt>Latest grid evidence</dt><dd>' + safe(status.grid_latest_seen_ms != null ? formatAge(status.grid_latest_seen_ms) : '—') +
      '</dd></div></dl>'
    : emptyState('radio', 'No CSI grid selected yet', 'The backend evaluates eligible raw CSI grids when capture starts. If the selected source lacks stable evidence, the API returns its eligibility details.');

  const nodeChoices = nodes.length
    ? '<ul class="check-list">' + nodes.map(node => '<li><label class="check-row"><input type="checkbox" name="source_node_ids" value="' +
      safe(node.node_id) + '" ' + (node.csi_status === 'active' ? 'checked' : '') + '><span><strong>Node ' + safe(node.node_id) +
      '</strong><small>' + safe(node.status) + ' · CSI ' + safe(node.csi_status) + (node.position_configured ? ' · position configured' : '') + '</small></span></label></li>').join('') +
      '</ul>'
    : emptyState('radio', 'No nodes to bind', 'Start the server source and wait for node telemetry before starting room calibration.');

  const startForm = '<form data-form="calibration-start"><div class="form-grid">' +
    '<div class="form-field full"><label class="form-label" for="cal-room">Room reference</label><input class="text-input" id="cal-room" name="room" maxlength="80" required placeholder="e.g. lab-west / floor-2"><span class="form-hint">Use a stable name for this room and sensor group. This does not define a floor plan.</span></div>' +
    '<div class="form-field"><label class="form-label" for="cal-source">Grid source node</label><select class="select-input" id="cal-source" name="source_node_id" required>' +
    nodes.map(node => '<option value="' + safe(node.node_id) + '">Node ' + safe(node.node_id) + '</option>').join('') +
    '</select><span class="form-hint">The backend selects an eligible raw CSI grid from live evidence.</span></div>' +
    '<div class="form-field"><span class="form-label">Bound nodes</span>' + nodeChoices + '</div></div>' +
    '<div class="form-actions"><button class="button button-primary" type="submit" ' + (active || nodes.length === 0 || synthetic ? 'disabled' : '') + '>Start baseline capture</button></div>' + (synthetic ? '<p class="form-hint">Connect a live raw CSI source before starting room calibration.</p>' : '') + '</form>';

  const stateLabel = active ? safe(status.status || 'capturing') : calibrated ? 'Saved baseline' : status ? safe(status.status || 'Not calibrated') : 'Unavailable';
  const capture = '<div class="kv-list">' +
    '<div class="kv-row"><dt>Calibration state</dt><dd>' + badge(stateLabel, calibrated ? 'success' : active ? 'info' : 'warning') + '</dd></div>' +
    '<div class="kv-row"><dt>Frame progress</dt><dd class="mono">' + safe(status && status.frame_count != null ? status.frame_count : '—') + ' / ' + safe(status && status.min_frames || 1000) + '</dd></div>' +
    '<div class="kv-row"><dt>Elapsed capture</dt><dd class="mono">' + safe(status && Number.isFinite(status.elapsed_s) ? Math.floor(status.elapsed_s / 60) + 'm ' + Math.floor(status.elapsed_s % 60) + 's' : '—') + ' / 10m</dd></div>' +
    '<div class="kv-row"><dt>CSI grid</dt><dd>' + safe(status && status.source_grid ? status.source_grid.n_subcarriers + ' subcarriers · PPDU ' + status.source_grid.ppdu_type : '—') + '</dd></div>' +
    '<div class="kv-row"><dt>Missing contributors</dt><dd>' + safe(status && Array.isArray(status.missing_source_node_ids) && status.missing_source_node_ids.length ? status.missing_source_node_ids.join(', ') : status ? 'None reported' : '—') + '</dd></div>' +
    '<div class="kv-row"><dt>Trust authority</dt><dd>' + safe(status && status.trust && status.trust.authority || 'abstained') + '</dd></div>' +
    '</div>' +
    (active ? '<div class="form-actions">' + button('Stop and save baseline', 'calibration-stop', 'button-primary') + button('Cancel capture', 'calibration-cancel', 'button-danger') + '</div>' : '') +
    (canReset ? '<div class="form-actions">' + button('Reset saved baseline', 'calibration-reset', 'button-danger') + '</div>' : '');

  const truth = '<form data-form="ground-truth"><div class="form-grid"><div class="form-field"><label class="form-label" for="gt-count">Observed people present</label><input class="text-input" id="gt-count" name="count" type="number" min="1" step="1" required placeholder="Enter a positive count"><span class="form-hint">Enter a count verified independently. It tunes the per-node count divisor only and does not establish person localization.</span></div><div class="form-field"><label class="form-label">Current deduplication factor</label><span class="mono">' +
    safe(data.dedup && Number.isFinite(data.dedup.dedup_factor) ? data.dedup.dedup_factor : '—') +
    '</span><span class="form-hint">Calibration manager persists this setting when available.</span></div></div><div class="form-actions"><button class="button" type="submit" ' +
    (!data.dedup || data.dedup.available !== true ? 'disabled' : '') + '>Save ground-truth count</button></div></form>';

  const trustReasons = status && status.trust && Array.isArray(status.trust.reasons) ? status.trust.reasons : [];
  const trustMarkup = status && status.trust
    ? '<div class="kv-list"><div class="kv-row"><dt>Authority</dt><dd>' + safe(status.trust.authority || 'abstained') + '</dd></div>' +
      '<div class="kv-row"><dt>Authorized</dt><dd>' + safe(status.trust.authorized === true ? 'Yes' : 'No') + '</dd></div>' +
      '<div class="kv-row"><dt>Reasons</dt><dd>' + safe(trustReasons.length ? trustReasons.join(', ') : '—') + '</dd></div></div>'
    : emptyState('shield_check', 'Trust unavailable', data.error || 'The calibration manager is unavailable or has not returned a trust record.');

  return '<div class="layout-grid">' + (error ? '<div class="col-12">' + error + '</div>' : '') +
    '<div class="col-7">' + card('Room binding', startForm, { subtitle: 'Choose the sensor IDs that belong to this room.' }) + '</div>' +
    '<div class="col-5">' + card('Capture status', capture, { subtitle: 'Requires at least 1,000 accepted frames over at least 10 minutes.' }) + '</div>' +
    disclosure('CSI grid and measurement trust', '<h3>Selected CSI grid</h3>' + candidateMarkup + '<h3>Measurement trust</h3>' + trustMarkup, 'col-12') +
    disclosure('Advanced: count calibration', '<p class="section-note">Only adjusts the count-fusion factor; does not enable accepted people counting.</p>' + truth, 'col-12') + '</div>';
}

function renderRecordings(data) {
  const recordings = data.recordings || [];
  const active = recordings.find(item => item.status === 'recording');
  const list = recordings.length
    ? '<div class="table-wrap"><table class="data-table"><thead><tr><th>Recording</th><th>Frames</th><th>Size</th><th>Source watermark</th><th>Updated</th><th>Status</th><th></th></tr></thead><tbody>' +
      recordings.map(item => '<tr><td class="table-primary mono">' + safe(item.id) + '</td><td class="mono">' + safe(item.frames == null ? '—' : item.frames) +
        '</td><td class="mono">' + safe(formatBytes(item.size_bytes)) + '</td><td>' + safe(item.watermark || '—') +
        '</td><td>' + safe(item.modified_epoch ? formatDate(item.modified_epoch * 1000) : '—') + '</td><td>' +
        badge(item.status || 'unknown', item.status === 'recording' ? 'danger' : item.status === 'completed' ? 'success' : 'warning') +
        '</td><td><div class="table-actions">' + (item.status === 'recording' ? button('Stop', 'recording-stop', 'button-small') :
          button('Delete', 'recording-delete', 'button-small button-danger', 'data-id="' + safe(item.id) + '"')) + '</div></td></tr>').join('') +
      '</tbody></table></div>'
    : emptyState('database', 'No recordings yet', data.recordingsError || 'Start a recording to store the frames received by the Go server.');
  const datasets = data.datasets || [];
  const datasetRows = datasets.length
    ? '<div class="table-wrap"><table class="data-table"><thead><tr><th>Dataset</th><th>Class label</th><th>Eligible</th><th>Size</th><th>Reason</th></tr></thead><tbody>' +
      datasets.map(item => '<tr><td class="table-primary mono">' + safe(item.id) + '</td><td><span class="dataset-label">' + safe(item.label) +
        '</span></td><td>' + badge(item.eligible ? 'Eligible' : 'Ineligible', item.eligible ? 'success' : 'warning') +
        '</td><td class="mono">' + safe(formatBytes(item.size_bytes)) + '</td><td>' + safe(item.reason || '—') + '</td></tr>').join('') +
      '</tbody></table></div>'
    : emptyState('box', 'No labeled datasets', data.datasetsError || 'Name a recording with a class label to create an activity-training dataset.');
  const recordForm = '<form data-form="recording-start" class="recording-controls"><div class="form-field"><label class="form-label" for="recording-label">Activity label (optional)</label><input class="text-input" id="recording-label" name="label" maxlength="32" placeholder="e.g. walking"><span class="form-hint">A label creates train_LABEL_ID.jsonl for the supported activity classifier.</span></div><button class="button button-primary" type="submit" ' +
    (active ? 'disabled' : '') + '>Start recording</button></form>' +
    (active ? '<div class="form-actions">' + button('Stop recording', 'recording-stop', 'button-danger') + '</div>' : '');
  const errors = [data.recordingsError, data.datasetsError].filter(Boolean).map(message => banner(message, 'warning')).join('');
  return '<div class="layout-grid">' + (errors ? '<div class="col-12 banner-stack">' + errors + '</div>' : '') +
    '<div class="col-12">' + card('Capture', recordForm, { subtitle: 'Only frames arriving at the backend can be recorded.' }) + '</div>' +
    '<div class="col-12">' + card('Recordings', list, { bodyClass: 'card-body-tight', subtitle: active ? 'A recording is active; source watermark shown below.' : 'The server stores local JSONL files. Raw CSI amplitudes remain subject to backend privacy controls.' }) + '</div>' +
    '<div class="col-12">' + card('Training datasets', datasetRows, { bodyClass: 'card-body-tight', subtitle: 'Datasets use labeled recording-file holdout; this is not independent environment validation.' }) + '</div></div>';
}

function renderModels(data) {
  const models = data.models || [];
  const active = data.activeModel && data.activeModel.status !== 'no_model' ? data.activeModel : null;
  const activeCard = active
    ? '<div class="model-card"><div class="model-card-top"><div><h3>' + safe(active.name || active.model_id || active.id || 'Loaded model') +
      '</h3><span class="table-meta mono">' + safe(active.model_id || active.id || '—') + '</span></div>' + badge('Loaded', 'success') + '</div>' +
      '<div class="model-meta"><span>Inference<strong>' + (active.inference_available ? 'Available' : 'Unavailable') + '</strong></span>' +
      '<span>Frames processed<strong>' + safe(active.frames_processed == null ? '—' : active.frames_processed) + '</strong></span>' +
      '<span>Validation accuracy<strong>' + (typeof active.validation_accuracy === 'number' ? (active.validation_accuracy * 100).toFixed(1) + '% · held-out files' : 'Not reported') + '</strong></span>' +
      '<span>Validation samples<strong>' + safe(active.validation_samples == null ? '—' : active.validation_samples) + '</strong></span></div>' +
      '<div class="form-actions">' + button('Unload model', 'model-unload', 'button-small') + '</div></div>'
    : emptyState('box', 'No active model', data.activeError || 'No model is currently loaded.');
  const modelList = models.length
    ? '<div class="models-grid">' + models.map(model => {
      const id = model.id || model.model_id;
      const loaded = active && String(active.model_id || active.id) === String(id);
      return '<article class="model-card"><div class="model-card-top"><div><h3>' + safe(model.name || id || 'Unnamed model') +
        '</h3><span class="table-meta mono">' + safe(id || '—') + '</span></div>' +
        badge(loaded ? 'Loaded' : model.loadable ? 'Loadable' : 'Unsupported', loaded ? 'success' : model.loadable ? 'info' : 'warning') + '</div>' +
        '<p class="model-description">' + safe(model.description || 'No model description was provided.') + '</p>' +
        '<div class="model-meta"><span>Architecture<strong>' + safe(model.architecture || 'Not reported') + '</strong></span>' +
        '<span>Version<strong>' + safe(model.version || '—') + '</strong></span>' +
        '<span>Size<strong>' + safe(formatBytes(model.size_bytes)) + '</strong></span>' +
        '<span>Created<strong>' + safe(formatDate(model.created_at)) + '</strong></span></div>' +
        '<div class="form-actions">' + (loaded ? badge('Active', 'success') : model.loadable === true ?
          button('Load', 'model-load', 'button-small button-primary', 'data-id="' + safe(id) + '"') : '<span class="form-hint">This artifact is not loadable by the current backend.</span>') +
          (id ? button('Delete', 'model-delete', 'button-small button-danger', 'data-id="' + safe(id) + '"') : '') +
        '</div></article>';
    }).join('') + '</div>'
    : emptyState('box', 'No registered models', data.modelsError || 'Place a supported artifact in the backend model directory or train an activity classifier.');

  const training = data.training || null;
  const datasets = data.datasets || [];
  const eligible = datasets.filter(dataset => dataset.eligible);
  const activeTraining = Boolean(training && training.active);
  const epochs = Number(training && training.epoch) || 0;
  const totalEpochs = Number(training && training.total_epochs) || 0;
  const progressPercent = totalEpochs > 0 ? Math.max(0, Math.min(100, epochs / totalEpochs * 100)) : 0;
  const trainState = activeTraining
    ? '<div class="progress-copy"><span>' + safe(training.phase || 'Training') + ' · epoch ' + safe(epochs) + ' of ' + safe(totalEpochs) +
      '</span><span>' + safe(Math.round(progressPercent)) + '%</span></div><div class="progress-track" role="progressbar" aria-label="Activity model training progress" aria-valuemin="0" aria-valuemax="100" aria-valuenow="' +
      safe(Math.round(progressPercent)) + '"><div class="progress-fill" style="width:' + safe(progressPercent) + '%"></div></div><dl class="kv-list progress-details"><div class="kv-row"><dt>Training samples</dt><dd>' +
      safe(training.train_samples == null ? '—' : training.train_samples) + '</dd></div><div class="kv-row"><dt>Validation samples</dt><dd>' +
      safe(training.validation_samples == null ? '—' : training.validation_samples) + '</dd></div><div class="kv-row"><dt>Validation accuracy</dt><dd>' +
      safe(typeof training.validation_accuracy === 'number' ? (training.validation_accuracy * 100).toFixed(1) + '%' : '—') +
      '</dd></div></dl><div class="form-actions">' + button('Cancel training', 'training-stop', 'button-danger') + '</div>'
    : '<form data-form="training-start"><div class="banner-stack">' +
      (training && training.enabled === false ? banner(training.reason || 'Training is unavailable on this backend.', 'warning') : '') +
      banner('This trains an activity classifier only. Validation holds out recording files from the supplied dataset; it does not establish pose, localization, or generalization to a new room.', 'info') +
      '</div><fieldset class="dataset-fieldset" ' + (training && training.enabled === false ? 'disabled' : '') + '><legend class="form-label">Eligible labeled datasets</legend>' +
      (eligible.length ? '<ul class="check-list">' + eligible.map(dataset => '<li><label class="check-row"><input type="checkbox" name="dataset_ids" value="' +
        safe(dataset.id) + '"><span><strong>' + safe(dataset.label) + '</strong><small>' + safe(dataset.id) +
        ' · ' + safe(formatBytes(dataset.size_bytes)) + '</small></span></label></li>').join('') + '</ul>' :
        '<p class="form-hint">No eligible datasets. Add at least two distinct labeled recording files for a class.</p>') +
      '<details class="training-options"><summary>Training parameters</summary><div class="form-grid training-config"><div class="form-field"><label class="form-label" for="train-epochs">Epochs</label><input class="text-input" type="number" id="train-epochs" name="epochs" min="1" max="2000" value="100" required></div>' +
      '<div class="form-field"><label class="form-label" for="train-batch">Batch size</label><input class="text-input" type="number" id="train-batch" name="batch_size" min="1" max="4096" value="32" required></div>' +
      '<div class="form-field"><label class="form-label" for="train-rate">Learning rate</label><input class="text-input" type="number" id="train-rate" name="learning_rate" min="0.000001" max="1" step="0.000001" value="0.0003" required></div>' +
      '<div class="form-field"><label class="form-label" for="train-patience">Early-stop patience</label><input class="text-input" type="number" id="train-patience" name="patience" min="1" max="1000" value="15" required></div></div></details>' +
      '<div class="form-actions"><button class="button button-primary" type="submit" ' + (eligible.length < 2 || training && training.enabled === false ? 'disabled' : '') + '>Start activity training</button></div></fieldset></form>';

  return '<div class="layout-grid">' + (data.modelsError || data.trainingError || data.datasetsError
    ? '<div class="col-12 banner-stack">' + [data.modelsError, data.trainingError, data.datasetsError].filter(Boolean).map(message => banner(message, 'warning')).join('') + '</div>' : '') +
    '<div class="col-5">' + card('Active model', activeCard, { subtitle: 'Loading a file does not mean its inference quality is validated.' }) + '</div>' +
    '<div class="col-7">' + card('Model registry', modelList, { subtitle: 'Artifact metadata reported by the Go backend.' }) + '</div>' +
    '<div class="col-12">' + card('Activity-model training', trainState, { subtitle: activeTraining ? 'Progress from the Go training manager.' : 'Create datasets from labeled recordings first.' }) + '</div>' +
    '</div>';
}

function renderIntegrations(data) {
  const vendors = data.vendors || [];
  const pollStatus = Array.isArray(data.pollStatus) ? data.pollStatus : [];
  const vendorCards = vendors.length
    ? '<div class="integration-grid">' + vendors.map(item => {
      const name = String(item.vendor || 'Provider').replace(/_/g, ' ');
      const status = pollStatus.find(value => value.vendor === item.vendor);
      const availability = item.availability || 'unknown';
      const tone = status && status.enabled ? (status.state === 'connected' ? 'success' : 'warning') :
        availability === 'available' ? 'info' : 'neutral';
      const label = status && status.enabled ? status.state : availability.replace(/_/g, ' ');
      const capabilities = Array.isArray(item.capabilities) ? item.capabilities : [];
      return '<article class="integration-card"><div class="integration-card-header"><h3>' + safe(name) + '</h3>' +
        badge(label, tone) + '</div><p>' + safe(item.reason || 'Provider capability descriptor.') + '</p><div class="tag-list">' +
        capabilities.map(capability => '<span class="tag">' + safe(String(capability).replace(/_/g, ' ')) + '</span>').join('') +
        '</div><p>Hardware validated: <strong>' + safe(item.hardware_validated ? 'yes' : 'no') + '</strong></p></article>';
    }).join('') + '</div>'
    : emptyState('plug', 'Provider catalog unavailable', data.vendorsError || 'The backend did not return its integration capability catalog.');
  const mqtt = data.mqtt || null;
  const mqttState = mqtt
    ? '<dl class="kv-list"><div class="kv-row"><dt>Enabled</dt><dd>' + safe(mqtt.enabled ? 'Yes' : 'No') + '</dd></div>' +
      '<div class="kv-row"><dt>Connection</dt><dd>' + safe(mqtt.state || (mqtt.connected ? 'connected' : 'disconnected')) + '</dd></div>' +
      '<div class="kv-row"><dt>Known nodes</dt><dd>' + safe(mqtt.known_nodes == null ? '—' : mqtt.known_nodes) + '</dd></div>' +
      '<div class="kv-row"><dt>Last published</dt><dd>' + safe(formatDate(mqtt.last_message_at)) + '</dd></div></dl>' +
      (mqtt.last_error ? '<p class="inline-error">' + safe(mqtt.last_error) + '</p>' : '')
    : emptyState('plug', 'MQTT status unavailable', data.mqttError || 'No MQTT status was returned.');
  const errors = [data.vendorsError, data.pollError, data.mqttError].filter(Boolean);
  return '<div class="layout-grid">' + (errors.length ? '<div class="col-12 banner-stack">' + errors.map(message => banner(message, 'warning')).join('') + '</div>' : '') +
    disclosure('Supported provider catalog', vendorCards, 'col-12') +
    '<div class="col-6">' + card('Provider polling', pollStatus.length ? '<div class="kv-list">' + pollStatus.map(item =>
      '<div class="kv-row"><dt>' + safe(item.vendor) + '</dt><dd>' + badge(item.enabled ? item.state : 'Not configured', item.enabled ? 'info' : 'neutral') + '</dd></div>').join('') + '</div>' :
      emptyState('plug', 'No provider pollers configured', 'Polling requires server-side provider settings and credentials.') , { subtitle: 'Credentials are never sent to or displayed by this browser.' }) + '</div>' +
    '<div class="col-6">' + card('MQTT publishing', mqttState, { subtitle: 'Availability and errors are reported by the server.' }) + '</div>' +
    '<div class="col-12">' + banner('The provider catalog currently describes limited capabilities. It does not add RF providers as CSI receivers or enable person localization.', 'warning') + '</div></div>';
}

function hasSavedToken() {
  try { return Boolean(localStorage.getItem(API_TOKEN_STORAGE_KEY)); } catch { return false; }
}

function renderSettings() {
  const source = state.health && state.health.source || 'Unavailable';

  const hasToken = hasSavedToken();
  const summary = '<dl class="kv-list"><div class="kv-row"><dt>API origin</dt><dd class="mono">' + safe(window.location.origin) + '</dd></div>' +
    '<div class="kv-row"><dt>Backend</dt><dd>' + safe(state.info && state.info.backend || '—') + '</dd></div>' +
    '<div class="kv-row"><dt>Configured source</dt><dd>' + safe(source) + '</dd></div>' +
    '<div class="kv-row"><dt>API token</dt><dd>' + badge(hasToken ? 'Saved · hidden' : 'Not saved', hasToken ? 'success' : 'neutral') + '</dd></div></dl>';
  const authForm = '<form data-form="token-save"><div class="form-grid"><div class="form-field full"><label class="form-label" for="api-token">Server bearer token</label><input class="text-input" id="api-token" name="token" type="password" autocomplete="new-password" spellcheck="false" placeholder="' +
    (hasToken ? 'Saved token is hidden · enter a replacement' : 'Paste a token issued for this server') +
    '"><span class="form-hint">Stored in this browser origin’s localStorage and sent only to same-origin API routes. It is not printed in diagnostics.</span></div></div>' +
    '<div class="form-actions"><button class="button button-primary" type="submit">Save token</button>' +
    (hasToken ? button('Clear token', 'token-clear', 'button-danger') : '') +
    button('Test API access', 'token-test', '') + '</div></form>';
  return '<div class="settings-content">' + card('API access', authForm, { subtitle: 'Only needed when this server requires a bearer token.' }) +
    disclosure('Connection details', summary) +
    '<section class="settings-links"><h2>Server configuration</h2><a class="button" href="#integrations">' + icon('plug') + 'Integrations</a><a class="text-link" href="#help">Setup guide and capabilities</a><p class="section-note">Provider credentials stay on the server. Keep the service bound to localhost unless you configure authenticated network access.</p></section></div>';

}

function renderHelp() {
  const entries = [
    ['RSSI and CSI', 'RSSI is received signal strength. CSI is a richer per-subcarrier channel observation. The backend derives signal features from device input, but a Wi-Fi signal is not itself a person coordinate.'],
    ['Real measurements only', 'Without hardware input, the server waits and the browser shows no readings. Query parameters and saved demo preferences cannot start a simulator. Synthetic frames from an older server are rejected.'],
    ['Spatial view', 'Space shows operator-configured ESP32 and router positions in local metres, including negative X, Y and Z. These positions persist on the server. Routers are configured references, not discovered objects. CSI hotspots are signal diagnostics and never become person coordinates.'],
    ['Calibration', 'Room calibration stores a mean/variance baseline for a bound CSI grid. It requires 1,000 accepted frames over at least 10 minutes with a stable raw grid. This baseline does not enable accepted person localization or vital signs.'],
    ['Recordings and training', 'Recordings store frames received by the backend. A label in the recording name creates a candidate activity dataset. The Go trainer supports activity classification; file holdout results do not prove accuracy in another room.'],
    ['People and biometrics', 'The API explicitly reports person localization, validated multi-person pose, and calibrated vital signs as unavailable. Edge counts and signal classifications are not accepted person detections or medical measurements.']
  ];
  const cards = entries.map(item => '<article class="help-card"><h3>' + safe(item[0]) + '</h3><p>' + safe(item[1]) + '</p></article>').join('');
  const flow = '<ol class="help-steps"><li>Start the Go backend and configure the ESP32 source.</li><li>Open Devices and wait for fresh node and CSI status.</li><li>Capture a room baseline only after eligible raw CSI is available.</li><li>Record a labeled activity sample and review the server’s source watermark.</li><li>Train only the activity task if datasets are eligible.</li></ol>';
  return '<div class="layout-grid"><div class="col-12">' + banner('Do not treat a 3D hotspot, estimated count, or model confidence as a validated person location or vital sign.', 'warning') + '</div>' +
    '<div class="col-8"><div class="help-grid">' + cards + '</div></div><div class="col-4">' + card('Getting started', flow) + '</div></div>';
}

async function refreshGlobal() {
  const requestId = ++state.pendingGlobalRequest;
  const results = await Promise.allSettled([
    api.get('/health/ready'),
    api.get('/api/v1/info'),
    api.get('/api/v1/sensing/latest')
  ]);
  const [healthResult, infoResult, latestResult] = results;
  if (requestId !== state.pendingGlobalRequest) return;
  if (healthResult.status === 'fulfilled') {
    state.health = healthResult.value;
    state.healthError = null;
  } else {
    state.health = null;
    state.healthError = healthResult.reason;
  }
  if (infoResult.status === 'fulfilled') {
    state.info = infoResult.value;
    state.infoError = null;
  } else {
    state.info = null;
    state.infoError = infoResult.reason;
  }
  if (latestResult.status === 'fulfilled') {
    const payload = latestResult.value;
    const latest = payload && (payload._simulated || payload.source_state === 'synthetic' || /simulat|synthetic|demo|replay/i.test(payload.source || '')) ? null : payload;
    state.latestError = null;
    if (latest && latest.status === 'stale') {
      state.latest = latest;
    } else if (latest && latest.status === 'no data yet') {
      if (!state.socketLatest) state.latest = latest;
    } else if (!state.socketLatest ||
        !Number.isFinite(state.socketLatest.timestamp) ||
        Number(latest && latest.timestamp) >= Number(state.socketLatest.timestamp)) {
      state.latest = latest;
    } else {
      state.latest = state.socketLatest;
    }
  } else {
    state.latestError = latestResult.reason;
    if (!state.socketLatest) state.latest = null;
  }
  state.dataSource = sensingService.dataSource;
  state.socketState = sensingService.state;
  state.lastGlobalRefresh = Date.now();
  setGlobalStatus();
  if (state.route === 'overview' && main.dataset.sourceMode !== sourcePresentation().mode) renderPage();
}

function fulfilled(results, index, fallback) {
  return results[index].status === 'fulfilled' ? results[index].value : fallback;
}

function rejectedMessage(results, index) {
  return results[index].status === 'rejected' ? results[index].reason.message : null;
}

async function loadRoute(route, options) {
  if (!['space', 'devices', 'calibration', 'recordings', 'models', 'integrations'].includes(route)) return;
  const quiet = Boolean(options && options.quiet);
  const requestId = ++state.pendingRouteRequest;
  if (!quiet) {
    state.page[route] = { ...(state.page[route] || {}), loading: true };
    renderPage();
  }
  const load = async paths => Promise.allSettled(paths.map(path => api.get(path)));
  let result;
  switch (route) {
    case 'space': {
      const responses = await load(['/api/v1/space/layout', '/api/v1/nodes']);
      result = {
        layout: fulfilled(responses, 0, {}).layout || null,
        nodes: fulfilled(responses, 1, {}).nodes || [],
        error: [rejectedMessage(responses, 0), rejectedMessage(responses, 1)].filter(Boolean).join(' · ')
      };
      break;
    }
    case 'devices': {
      const responses = await load(['/api/v1/nodes', '/api/v1/mesh']);
      result = {
        nodes: fulfilled(responses, 0, {}).nodes || [],
        meshNodes: normalizeMeshNodes(fulfilled(responses, 1, {})),
        error: [rejectedMessage(responses, 0), rejectedMessage(responses, 1)].filter(Boolean).join(' · ')
      };
      break;
    }
    case 'calibration': {
      const responses = await load(['/api/v1/calibration/status', '/api/v1/config/dedup-factor', '/api/v1/nodes']);
      const status = fulfilled(responses, 0, null);
      const nodePayload = fulfilled(responses, 2, {});
      state.page.devices = { nodes: Array.isArray(nodePayload.nodes) ? nodePayload.nodes : [] };
      result = {
        status,
        dedup: fulfilled(responses, 1, null),
        error: [rejectedMessage(responses, 0), rejectedMessage(responses, 1), rejectedMessage(responses, 2)].filter(Boolean).join(' · ')
      };
      break;
    }
    case 'recordings': {
      const responses = await load(['/api/v1/recording/list', '/api/v1/datasets']);
      result = {
        recordings: fulfilled(responses, 0, {}).recordings || [],
        datasets: fulfilled(responses, 1, {}).datasets || [],
        recordingsError: rejectedMessage(responses, 0),
        datasetsError: rejectedMessage(responses, 1)
      };
      break;
    }
    case 'models': {
      const responses = await load(['/api/v1/models', '/api/v1/models/active', '/api/v1/train/status', '/api/v1/datasets']);
      const activePayload = fulfilled(responses, 1, null);
      result = {
        models: fulfilled(responses, 0, {}).models || [],
        activeModel: activePayload && activePayload.status !== 'no_model' ? activePayload : null,
        training: fulfilled(responses, 2, null),
        datasets: fulfilled(responses, 3, {}).datasets || [],
        modelsError: rejectedMessage(responses, 0),
        activeError: rejectedMessage(responses, 1),
        trainingError: rejectedMessage(responses, 2),
        datasetsError: rejectedMessage(responses, 3)
      };
      break;
    }
    case 'integrations': {
      const responses = await load(['/api/v1/rf/vendors', '/api/v1/rf/vendors/status', '/api/v1/integrations/mqtt/status']);
      result = {
        vendors: fulfilled(responses, 0, []),
        pollStatus: fulfilled(responses, 1, []),
        mqtt: fulfilled(responses, 2, null),
        vendorsError: rejectedMessage(responses, 0),
        pollError: rejectedMessage(responses, 1),
        mqttError: rejectedMessage(responses, 2)
      };
      break;
    }
    default:
      return;
  }

  if (requestId !== state.pendingRouteRequest || state.route !== route) return;
  state.page[route] = { ...result, loading: false };
  if (quiet && route === 'space' && spaceView && !result.error) {
    spaceView.update(state.page[route]);
    return;
  }
  if (!quiet || !isEditingForm()) renderPage();
}

function isEditingForm() {
  const active = document.activeElement;
  return Boolean(active && main.contains(active) &&
    (active.matches('input, textarea, select, button, summary') || active.isContentEditable));
}

function parseRoute() {
  const hash = window.location.hash.slice(1).toLowerCase();
  const route = hash;
  return Object.prototype.hasOwnProperty.call(ROUTES, route) ? route : 'overview';
}

function navigateTo(route) {
  if (!Object.prototype.hasOwnProperty.call(ROUTES, route)) return;
  if (window.location.hash !== '#' + route) {
    window.location.hash = route;
  } else if (state.route !== route) {
    activateRoute(route);
  }
}

function activateRoute(route) {
  renderShell(route);
  void refreshGlobal();
  void loadRoute(route);
}

function showToast(message, type) {
  if (!toastRegion) return;
  const toast = document.createElement('div');
  toast.className = 'toast' + (type ? ' is-' + type : '');
  toast.textContent = message;
  toastRegion.appendChild(toast);
  window.setTimeout(() => toast.remove(), 4200);
}

function closeMobileNav(restoreFocus) {
  document.body.classList.remove('nav-open');
  const sidebar = document.getElementById('sidebar');
  if (sidebar) sidebar.inert = window.matchMedia('(max-width: 900px)').matches;
  const mobile = window.matchMedia('(max-width: 900px)').matches;
  const toggle = document.getElementById('menu-toggle');
  const appFrame = document.querySelector('.app-frame');
  if (appFrame) appFrame.inert = false;
  if (sidebar) {
    sidebar.removeAttribute('aria-modal');
    sidebar.removeAttribute('role');
  }
  if (toggle) {
    toggle.setAttribute('aria-expanded', 'false');
    toggle.setAttribute('aria-label', 'Open navigation');
    if (restoreFocus) toggle.focus();
  }
  const scrim = document.getElementById('mobile-scrim');
  if (scrim) scrim.hidden = true;
}

function toggleMobileNav() {
  const isOpen = !document.body.classList.contains('nav-open');
  document.body.classList.toggle('nav-open', isOpen);
  const sidebar = document.getElementById('sidebar');
  if (sidebar) sidebar.inert = !isOpen && window.matchMedia('(max-width: 900px)').matches;
  const toggle = document.getElementById('menu-toggle');
  const scrim = document.getElementById('mobile-scrim');
  const mobile = window.matchMedia('(max-width: 900px)').matches;
  const appFrame = document.querySelector('.app-frame');
  if (appFrame) appFrame.inert = isOpen && mobile;
  if (sidebar && isOpen && mobile) {
    sidebar.setAttribute('role', 'dialog');
    sidebar.setAttribute('aria-modal', 'true');
  } else if (sidebar) {
    sidebar.removeAttribute('aria-modal');
    sidebar.removeAttribute('role');
  }
  if (toggle) {
    toggle.setAttribute('aria-expanded', String(isOpen));
    toggle.setAttribute('aria-label', isOpen ? 'Close navigation' : 'Open navigation');
  }
  if (scrim) scrim.hidden = !isOpen;
  if (isOpen) {
    const firstLink = document.querySelector('#sidebar [data-route]');
    if (firstLink) firstLink.focus();
  }
}

function setBusy(buttonNode, busy) {
  if (!buttonNode) return;
  buttonNode.disabled = busy;
  if (busy) {
    buttonNode.dataset.originalLabel = buttonNode.textContent;
    buttonNode.textContent = 'Working…';
  } else if (buttonNode.dataset.originalLabel) {
    buttonNode.textContent = buttonNode.dataset.originalLabel;
    delete buttonNode.dataset.originalLabel;
  }
}

async function performAction(buttonNode, task, successMessage) {
  if (state.busy) return;
  state.busy = true;
  main.setAttribute('aria-busy', 'true');
  setBusy(buttonNode, true);
  try {
    await task();
    if (successMessage) showToast(successMessage, 'success');
    await refreshGlobal();
    await loadRoute(state.route, { quiet: true });
  } catch (error) {
    showToast(error && error.message ? error.message : 'Action failed. Try again.', 'error');
  } finally {
    state.busy = false;
    main.removeAttribute('aria-busy');
    setBusy(buttonNode, false);
    if (state.route === 'settings' || state.route === 'calibration') renderPage();
  }
}

function selectedSourceNode(form, nodeIDs) {
  const selected = Number(new FormData(form).get('source_node_id'));
  return nodeIDs.includes(selected) ? selected : nodeIDs[0];
}

async function makeBindingDigest(room, nodeIDs) {
  if (!globalThis.crypto || !globalThis.crypto.subtle || !globalThis.TextEncoder) {
    throw new Error('This browser cannot create a secure room-binding digest. Open Wave Ray on localhost or HTTPS.');
  }
  const canonical = JSON.stringify({
    schema: 'ruview-room-binding-v1',
    room: room.trim().normalize('NFKC'),
    source_node_ids: nodeIDs.slice().sort((a, b) => a - b)
  });
  const bytes = await globalThis.crypto.subtle.digest('SHA-256', new TextEncoder().encode(canonical));
  return Array.from(new Uint8Array(bytes)).map(value => value.toString(16).padStart(2, '0')).join('');
}

async function submitForm(form) {
  const formName = form.dataset.form;
  const values = new FormData(form);
  try {
    if (formName === 'calibration-start') {
      const room = String(values.get('room') || '').trim();
      const nodes = Array.from(new Set(Array.from(form.querySelectorAll('input[name="source_node_ids"]:checked'))
        .map(input => Number(input.value)).filter(Number.isInteger))).sort((left, right) => left - right);
      if (!room) throw new Error('Enter a room reference.');
      if (!nodes.length) throw new Error('Select at least one node to bind.');
      const sourceNode = selectedSourceNode(form, nodes);
      const bindingDigest = await makeBindingDigest(room, nodes);
      await api.post('/api/v1/calibration/start?source_node_id=' + encodeURIComponent(sourceNode), {
        binding_digest: bindingDigest,
        source_node_ids: nodes
      });
      showToast('Calibration capture started. The server will validate fresh CSI grid evidence.', 'success');
    } else if (formName === 'ground-truth') {
      const count = Number(values.get('count'));
      if (!Number.isSafeInteger(count) || count < 1) throw new Error('Enter a positive whole-number count.');
      const result = await api.post('/api/v1/config/ground-truth', { count });
      showToast(result.note || 'Ground-truth count saved.', 'success');
    } else if (formName === 'recording-start') {
      const label = String(values.get('label') || '').trim().normalize('NFKD')
        .replace(/[\u0300-\u036f]/g, '').toLowerCase().replace(/[^a-z0-9]+/g, '-')
        .replace(/^-|-$/g, '').slice(0, 32);
      const suffix = Date.now().toString(36);
      const id = label ? 'train_' + label.replace(/-/g, '_') + '_' + suffix : 'rec_' + suffix;
      const result = await api.post('/api/v1/recording/start', { id });
      state.flash = result.watermark ? 'Recording started · ' + result.watermark : 'Recording started.';
      showToast(state.flash, 'success');
    } else if (formName === 'training-start') {
      const datasetIDs = values.getAll('dataset_ids').map(String);
      if (datasetIDs.length < 2) throw new Error('Select at least two eligible recording files.');
      const config = {
        epochs: Number(values.get('epochs')),
        batch_size: Number(values.get('batch_size')),
        learning_rate: Number(values.get('learning_rate')),
        patience: Number(values.get('patience'))
      };
      if (!Object.values(config).every(Number.isFinite)) throw new Error('Check the training settings.');
      await api.post('/api/v1/train/start', { dataset_ids: datasetIDs, config });
      showToast('Activity-model training started.', 'success');
    } else if (formName === 'token-save') {
      const token = String(values.get('token') || '').trim();
      if (!token) throw new Error('Enter a token to replace the current value. The saved value is never shown.');
      try {
        localStorage.setItem(API_TOKEN_STORAGE_KEY, token);
      } catch {
        throw new Error('This browser does not allow local storage, so the token was not saved.');
      }
      apiService.setAuthToken(token);
      sensingService.reconnect();
      showToast('Token saved in this browser.', 'success');
    }
  } catch (error) {
    showToast(error && error.message ? error.message : 'Could not save the requested change.', 'error');
  }
  await refreshGlobal();
  await loadRoute(state.route, { quiet: true });
}

async function handleAction(target) {
  const action = target.dataset.action;
  const id = target.dataset.id || '';
  if (action === 'refresh') {
    await performAction(target, async () => {
      await refreshGlobal();
      await loadRoute(state.route, { quiet: false });
    });
    return;
  }
  if (action === 'token-clear') {
    if (!window.confirm('Clear the saved Wave Ray API token from this browser?')) return;
    await performAction(target, async () => {
      localStorage.removeItem(API_TOKEN_STORAGE_KEY);
      apiService.setAuthToken(null);
      sensingService.reconnect();
    }, 'Saved token cleared.');
    return;
  }
  if (action === 'token-test') {
    await performAction(target, async () => { await api.get('/api/v1/info'); }, 'The API responded successfully.');
    return;
  }
  if (action === 'calibration-stop' || action === 'calibration-cancel' || action === 'calibration-reset') {
    const status = state.page.calibration && state.page.calibration.status;
    const identity = calibrationIdentity(status);
    if (!identity || !identity.boot_epoch) {
      showToast('The server has not returned a current calibration identity.', 'error');
      return;
    }
    if (action === 'calibration-reset' && !window.confirm('Reset the saved CSI baseline? This removes the stored baseline, not recordings.')) return;
    if (action === 'calibration-cancel' && !window.confirm('Cancel this capture? The current calibration session will not be saved.')) return;
    const endpoint = action === 'calibration-stop' ? '/api/v1/calibration/stop' :
      action === 'calibration-cancel' ? '/api/v1/calibration/cancel' : '/api/v1/calibration/reset';
    await performAction(target, () => api.post(endpoint, identity),
      action === 'calibration-stop' ? 'Calibration stop requested.' :
        action === 'calibration-cancel' ? 'Calibration capture cancelled.' : 'Saved baseline reset.');
    return;
  }
  if (action === 'ground-truth') return;
  if (action === 'recording-stop') {
    await performAction(target, () => api.post('/api/v1/recording/stop', {}), 'Recording stopped.');
    return;
  }
  if (action === 'recording-delete') {
    if (!id || !window.confirm('Delete recording ' + id + '? This removes its JSONL file from the server.')) return;
    await performAction(target, () => api.delete('/api/v1/recording/' + encodeURIComponent(id)), 'Recording deleted.');
    return;
  }
  if (action === 'model-load') {
    if (!id) return;
    await performAction(target, () => api.post('/api/v1/models/load', { model_id: id }), 'Model loaded into the registry.');
    return;
  }
  if (action === 'model-unload') {
    await performAction(target, () => api.post('/api/v1/models/unload', {}), 'Active model unloaded.');
    return;
  }
  if (action === 'model-delete') {
    if (!id || !window.confirm('Delete model ' + id + ' from the backend registry?')) return;
    await performAction(target, () => api.delete('/api/v1/models/' + encodeURIComponent(id)), 'Model deleted.');
    return;
  }
  if (action === 'training-stop') {
    if (!window.confirm('Cancel the active activity-model training job?')) return;
    await performAction(target, () => api.post('/api/v1/train/stop', {}), 'Training cancellation requested.');
  }
}

function bindEvents() {
  document.querySelector('.skip-link').addEventListener('click', event => { event.preventDefault(); main.focus(); });
  window.addEventListener('ruview-theme-change', updateLiveOverviewFields);
  main.addEventListener('toggle', event => { if (event.target.matches('.diagnostics')) updateLiveOverviewFields(); }, true);
  document.getElementById('refresh-button').addEventListener('click', async event => {
    const target = event.currentTarget;
    target.classList.add('is-refreshing');
    await refreshGlobal();
    await loadRoute(state.route, { quiet: false });
    window.setTimeout(() => target.classList.remove('is-refreshing'), 350);
  });
  document.getElementById('menu-toggle').addEventListener('click', toggleMobileNav);
  document.getElementById('sidebar-close').addEventListener('click', () => closeMobileNav(true));
  document.getElementById('mobile-scrim').addEventListener('click', () => closeMobileNav(true));
  window.addEventListener('resize', () => {
    const sidebar = document.getElementById('sidebar');
    const mobile = window.matchMedia('(max-width: 900px)').matches;
    if (sidebar) sidebar.inert = mobile && !document.body.classList.contains('nav-open');
    if (!mobile && document.body.classList.contains('nav-open')) closeMobileNav(false);
    if (!mobile && sidebar) {
      sidebar.removeAttribute('aria-modal');
      sidebar.removeAttribute('role');
    }
    updateLiveOverviewFields();
  }, { passive: true });
  document.querySelectorAll('[data-route]').forEach(link => {
    link.addEventListener('click', () => closeMobileNav(false));
  });
  main.addEventListener('click', event => {
    const target = event.target.closest('[data-action]');
    if (target) void handleAction(target);
  });
  main.addEventListener('submit', event => {
    const form = event.target.closest('form[data-form]');
    if (!form) return;
    event.preventDefault();
    void submitForm(form);
  });
  window.addEventListener('hashchange', () => {
    const next = parseRoute();
    if (window.location.hash.slice(1) !== next) window.location.hash = next;
    if (next !== state.route) activateRoute(next);
  });
  window.addEventListener('keydown', event => {
    if (!document.body.classList.contains('nav-open')) return;
    if (event.key === 'Escape') {
      closeMobileNav(true);
      return;
    }
    if (event.key === 'Tab') {
      const sidebar = document.getElementById('sidebar');
      const focusable = Array.from(sidebar.querySelectorAll('a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])'))
        .filter(node => !node.hidden && node.getClientRects().length);
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    }
  });
  sensingService.onStateChange(next => {
    state.socketState = next;
    state.dataSource = sensingService.dataSource;
    setGlobalStatus();
  });
  sensingService.onData(data => {
    const frame = data && data.data && typeof data.data === 'object' ? data.data : data;
    if (frame && typeof frame === 'object') {
      state.socketLatest = frame;
      state.latest = frame;
      spaceView?.measurement(frame);
    }
    state.socketState = sensingService.state;
    state.dataSource = sensingService.dataSource;
    setGlobalStatus();
  });
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) {
      void refreshGlobal();
      void loadRoute(state.route, { quiet: true });
    }
  });
}

function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) return;
  navigator.serviceWorker.register(new URL('../sw.js', import.meta.url))
    .catch(error => console.warn('Wave Ray offline shell registration failed:', error));
}
async function init() {
  registerServiceWorker();
  bindEvents();
  activateRoute(parseRoute());
  sensingService.start();
  window.setInterval(() => {
    if (document.hidden) return;
    void refreshGlobal();
    if (state.route !== 'overview' && state.route !== 'settings' && !isEditingForm()) {
      void loadRoute(state.route, { quiet: true });
    }
  }, 12000);
}

init().catch(error => {
  console.error('Wave Ray workspace failed to initialize:', error);
  main.innerHTML = pageHeader('overview', '') + '<div class="inline-error">The workspace could not initialize. Refresh the page and inspect the browser console.</div>';
});
