'use strict';

/*
 * Delta web interface.
 *
 * A small single-page app without frameworks or build step. Overview:
 *
 *   - The server pushes the complete forwarder list once per second (and after
 *     every change) over a server-sent event stream (/api/stream). It is stored
 *     in `state.forwarders`, and the current page's `update()` re-renders from it.
 *   - Pages are selected by the URL hash: #/ (list), #/f/<id> (detail),
 *     #/events, #/captures, #/settings, #/help. Each page function renders into <main>
 *     and returns an object with optional hooks:
 *       update()   called whenever new forwarder data arrives
 *       onEvent()  called for every new event-log entry
 *       redraw()   called when the color theme changes (charts read their colors from CSS)
 *       destroy()  called when the user leaves the page
 *   - Changes (create, start, delete …) are sent to the JSON API with apiRequest().
 *     The server then pushes the new state, which updates every open tab.
 */

/* ================= small helpers ================= */

// Short querySelector helpers: $ returns one element, $$ an array of all matches.
const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

// Every value inserted into HTML templates must go through escapeHtml.
const escapeHtml = value => String(value ?? '').replace(/[&<>"']/g, ch => (
  { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));

// Small per-browser preferences (sort order, last tab …). Storage can be
// unavailable (private mode), so every access is guarded.
const preferences = {
  get(key, fallback) {
    try {
      const raw = localStorage.getItem('delta-' + key);
      return raw === null ? fallback : JSON.parse(raw);
    } catch { return fallback; }
  },
  set(key, value) {
    try { localStorage.setItem('delta-' + key, JSON.stringify(value)); } catch { /* ignore */ }
  },
};

/* ---------- number and time formatting ---------- */

// 1234567 -> "1.23 MB" (decimal units, as usual for network traffic)
function formatBytes(bytes) {
  let value = Number(bytes) || 0;
  const units = ['B', 'kB', 'MB', 'GB', 'TB'];
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) { value /= 1000; unit++; }
  const digits = unit === 0 ? 0 : value < 10 ? 2 : value < 100 ? 1 : 0;
  return value.toFixed(digits) + ' ' + units[unit];
}
const formatRate = bytesPerSecond => formatBytes(bytesPerSecond) + '/s';

// 15300 -> "15k"
function formatCount(count) {
  let value = Number(count) || 0;
  if (value < 1000) return value % 1 ? value.toFixed(1) : String(value);
  const suffixes = ['k', 'M', 'G'];
  let i = -1;
  while (value >= 1000 && i < suffixes.length - 1) { value /= 1000; i++; }
  return (value < 10 ? value.toFixed(1) : value.toFixed(0)) + suffixes[i];
}

// 3725000 ms -> "1h 2m"
function formatDuration(milliseconds) {
  let seconds = Math.max(0, Math.floor(milliseconds / 1000));
  if (seconds < 60) return seconds + 's';
  let minutes = Math.floor(seconds / 60); seconds %= 60;
  if (minutes < 60) return `${minutes}m ${seconds}s`;
  let hours = Math.floor(minutes / 60); minutes %= 60;
  if (hours < 24) return `${hours}h ${minutes}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

function timeAgo(time) {
  if (!time) return '—';
  const elapsed = Date.now() - new Date(time).getTime();
  return elapsed < 1500 ? 'just now' : formatDuration(elapsed) + ' ago';
}

const zeroPad = (number, width = 2) => String(number).padStart(width, '0');

// unix ms -> "14:03:59.123"
function formatClockTime(milliseconds) {
  const d = new Date(milliseconds);
  return `${zeroPad(d.getHours())}:${zeroPad(d.getMinutes())}:${zeroPad(d.getSeconds())}.${zeroPad(d.getMilliseconds(), 3)}`;
}

const formatDateTime = time => new Date(time).toLocaleString();

// Same rule as the server uses for capture file names ("GPS Deck" -> "GPS-Deck").
const toFileName = name => name.replace(/[^A-Za-z0-9_-]+/g, '-').replace(/^-+|-+$/g, '');

// Let the browser save data as a file.
function downloadFile(fileName, data, mimeType = 'application/json') {
  const url = URL.createObjectURL(data instanceof Blob ? data : new Blob([data], { type: mimeType }));
  const link = Object.assign(document.createElement('a'), { href: url, download: fileName });
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/* ================= icons ================= */

// Inline SVG icons (24x24, drawn with strokes, colored via currentColor).
const ICON_PATHS = {
  play: '<path d="M7 4.5v15l12.5-7.5z"/>',
  stop: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
  restart: '<path d="M21 12a9 9 0 1 1-2.64-6.36L21 8"/><path d="M21 3v5h-5"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  edit: '<path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/>',
  trash: '<path d="M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6M10 11v6M14 11v6"/>',
  copy: '<rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/>',
  more: '<circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/>',
  layers: '<path d="M12 2 2 7l10 5 10-5-10-5Z"/><path d="m2 17 10 5 10-5M2 12l10 5 10-5"/>',
  bell: '<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/>',
  disc: '<circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="3"/>',
  sliders: '<path d="M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6"/>',
  logout: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5M21 12H9"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  moon: '<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z"/>',
  download: '<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3"/>',
  upload: '<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M17 8l-5-5-5 5M12 3v12"/>',
  pause: '<rect x="6" y="5" width="4" height="14" rx="1"/><rect x="14" y="5" width="4" height="14" rx="1"/>',
  x: '<path d="M18 6 6 18M6 6l12 12"/>',
  back: '<path d="m15 18-6-6 6-6"/>',
  tag: '<path d="M20.6 13.4 13.4 20.6a2 2 0 0 1-2.8 0L2 12V2h10l8.6 8.6a2 2 0 0 1 0 2.8Z"/><circle cx="7" cy="7" r="1.5"/>',
  eye: '<path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12Z"/><circle cx="12" cy="12" r="3"/>',
  users: '<path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/>',
  alert: '<path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z"/><path d="M12 9v4M12 17h.01"/>',
  activity: '<path d="M22 12h-4l-3 9L9 3l-3 9H2"/>',
  arrowIn: '<path d="M12 5v14M19 12l-7 7-7-7"/>',
  arrowOut: '<path d="M12 19V5M5 12l7-7 7 7"/>',
  record: '<circle cx="12" cy="12" r="6" fill="currentColor"/>',
  key: '<circle cx="7.5" cy="15.5" r="4.5"/><path d="m10.7 12.3 9.8-9.8M17 6l3 3M14 9l2 2"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 16v-4M12 8h.01"/>',
  note: '<path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9Z"/><path d="M14 3v6h6M8 13h8M8 17h5"/>',
  help: '<circle cx="12" cy="12" r="9"/><path d="M9.1 9a3 3 0 0 1 5.8 1c0 2-3 3-3 3M12 17h.01"/>',
};
const icon = (name, extraClass = '') => `<svg class="i ${extraClass}" viewBox="0 0 24 24">${ICON_PATHS[name] || ''}</svg>`;
// Logo: a river that splits into channels (a river delta), like an input fanning out to several outputs.
const LOGO_SVG = '<svg viewBox="0 0 24 24"><path d="M12 3v6.5M12 9.5c0 4-4.5 6-6.5 11M12 9.5V20.5M12 9.5c0 4 4.5 6 6.5 11"/></svg>';
const APP_NAME = 'Delta';
const APP_TAGLINE = 'the NMEA Proxy';
// Layered waves drawn at the bottom of the login and setup pages.
const WAVES_SVG = `<svg class="waves" viewBox="0 0 1440 320" preserveAspectRatio="none">
  <path d="M0 150c180-50 360-50 540 0s360 50 540 0 270-60 360-40V320H0Z"/>
  <path d="M0 210c200-40 400-40 600 5s380 40 560 0 230-30 280-20V320H0Z"/>
  <path d="M0 260c220-30 420-25 640 10s400 25 560-5 180-20 240-15V320H0Z"/></svg>`;

/* ================= forwarder helpers ================= */

// Endpoint modes as offered in the editor: [value, description].
const ENDPOINT_MODES = {
  input: [
    ['tcp-listen', 'TCP server — sources connect to Delta'],
    ['udp-listen', 'UDP listen — receive datagrams on a port'],
    ['tcp-connect', 'TCP client — Delta connects to a source'],
  ],
  output: [
    ['tcp-listen', 'TCP server — clients connect and receive data'],
    ['tcp-connect', 'TCP client — Delta connects to a target'],
    ['udp-send', 'UDP send — datagrams to a remote host'],
  ],
};
const MODE_LABELS = { 'tcp-listen': 'TCP listen', 'udp-listen': 'UDP listen', 'tcp-connect': 'TCP connect', 'udp-send': 'UDP send' };
const isListenMode = mode => mode.endsWith('-listen');
// Listen endpoints show "*:port" when they listen on all interfaces.
const endpointAddress = endpoint => `${isListenMode(endpoint.mode) ? (endpoint.host || '*') : endpoint.host}:${endpoint.port}`;
// Every remote address of a tcp-connect endpoint: host:port first, then the additional hosts.
const allEndpointAddresses = endpoint => [endpointAddress(endpoint), ...(endpoint.targets || []).map(target => `${target.host}:${target.port}`)];
// "+2" behind the first address when a tcp-connect endpoint has more hosts; the hint lists them all.
function moreHostsBadge(endpoint) {
  const extraCount = endpoint.targets?.length || 0;
  if (!extraCount) return '';
  return `<span class="more-hosts" title="${escapeHtml(`Delta keeps a connection to each of these hosts: ${allEndpointAddresses(endpoint).join(', ')}`)}">+${extraCount}</span>`;
}

// Endpoint states that mean "working"; everything else is shown in warning color.
const HEALTHY_STATES = new Set(['listening', 'connected', 'open', 'receiving', 'sending']);
const HEALTH_LABELS = { ok: 'Running', degraded: 'Degraded', error: 'Error', stopped: 'Stopped' };
const HEALTH_HINTS = {
  ok: 'Running: all endpoints are connected or listening',
  degraded: 'Degraded: running, but at least one endpoint is not connected (Delta keeps retrying)',
  error: 'Error: the forwarder could not start (e.g. port already in use); Delta retries every 15 s',
  stopped: 'Stopped',
};
// What each endpoint state means (hint on the state badges).
const STATE_HINTS = {
  listening: 'Port is open and waiting for connections or datagrams',
  connected: 'Connection to the remote host is established',
  connecting: 'Trying to connect to the remote host…',
  down: 'Not connected. Delta retries automatically with growing delays (up to 10 s)',
  open: 'UDP socket is open, waiting for the first datagram',
  receiving: 'Datagrams are arriving',
  sending: 'Datagrams are being sent',
  unreachable: 'The remote host reported "port unreachable". Delta keeps trying',
  partial: 'Connected to some of the remote hosts, but not all. Delta keeps retrying the others',
};
// What each endpoint mode does (hint on the mode badges).
const MODE_HINTS = {
  'tcp-listen': 'TCP server: Delta opens this port and others connect to it',
  'udp-listen': 'UDP: Delta receives datagrams on this port from any sender',
  'tcp-connect': 'TCP client: Delta connects to this remote host and port',
  'udp-send': 'UDP: Delta sends every message as a datagram to this host and port',
};
const HEALTH_SORT_ORDER = { error: 0, degraded: 1, ok: 2, stopped: 3 };
const healthClass = health => (health === 'stopped' ? '' : health);
const hasIssue = fwd => fwd.health === 'error' || fwd.health === 'degraded' || fwd.firing?.length > 0;
const peerNoun = mode => (mode === 'udp-listen' ? 'sources' : mode === 'tcp-listen' ? 'clients' : 'connections');

// The server sends config fields and live state in one object. configOf
// extracts only the configuration (for editing and exporting).
function configOf(fwd) {
  const { id, name, tags, comment, framing, autostart, idle_alert, input, outputs, created } = fwd;
  return { id, name, tags, comment, framing, autostart, idle_alert, input, outputs, created };
}

// Make sure optional fields have safe defaults, so rendering never fails on
// a missing field (fields may be left out of the JSON when empty).
function normalizeForwarder(fwd) {
  fwd.tags = Array.isArray(fwd.tags) ? fwd.tags : [];
  fwd.comment = fwd.comment || '';
  fwd.clients_in = fwd.clients_in || 0;
  fwd.clients_out = fwd.clients_out || 0;
  fwd.outputs = fwd.outputs || [];
  fwd.firing = fwd.firing || [];
  fwd.rate = fwd.rate || { in: 0, out: 0, msgs: 0 };
  return fwd;
}

// Explains which endpoints of a running forwarder are not working and why,
// e.g. "Output 1 → 192.168.2.108:10001: host unreachable: …".
function endpointProblems(fwd) {
  if (fwd.state !== 'running') return [];
  const endpoints = [['Input', fwd.input, fwd.in_status],
    ...fwd.outputs.map((out, i) => [out.name ? `Output "${out.name}"` : `Output ${i + 1}`, out, fwd.out_status?.[i]])];
  const problems = [];
  for (const [label, endpoint, status] of endpoints) {
    if (!status || HEALTHY_STATES.has(status.state)) continue;
    const reason = status.error || status.state; // e.g. still "connecting" before the first error
    const addressCount = allEndpointAddresses(endpoint).length;
    const address = addressCount > 1 ? `(${addressCount} hosts)` : endpointAddress(endpoint);
    problems.push({ label, address, state: status.state, reason, queued: status.queued || 0 });
  }
  return problems;
}

// Small "[TCP LISTEN] *:5000" badge, with a colored dot for the live state.
function endpointBadge(endpoint, status, isRunning) {
  const stateHint = status ? `${status.state}: ${STATE_HINTS[status.state] || ''}${status.error && !HEALTHY_STATES.has(status.state) ? ' — ' + status.error : ''}` : '';
  const dot = isRunning && status
    ? `<span class="dotc" style="background:var(--${HEALTHY_STATES.has(status.state) ? 'ok' : 'warn'})" title="${escapeHtml(stateHint)}"></span>`
    : '';
  return `<span class="ep">${dot}<span class="m" title="${escapeHtml(MODE_HINTS[endpoint.mode] || '')}">${MODE_LABELS[endpoint.mode] || endpoint.mode}</span><span class="a">${escapeHtml(endpointAddress(endpoint))}</span>${moreHostsBadge(endpoint)}</span>`;
}

/* ================= API & global state ================= */

// Sends a request to the Delta API and returns the parsed JSON response.
// Throws an Error with the server's message if the request fails.
async function apiRequest(method, path, body, sendRawBody = false) {
  const options = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    if (sendRawBody) {
      options.body = body; // e.g. an uploaded file
    } else {
      options.headers['Content-Type'] = 'application/json';
      options.body = JSON.stringify(body);
    }
  }
  const response = await fetch(path, options);
  if (response.status === 401 && path !== '/api/login' && path !== '/api/session') {
    onSessionExpired();
    throw new Error('Session expired, please sign in again');
  }
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || response.statusText);
  return data;
}

const state = {
  forwarders: [],               // latest list pushed by the server
  forwardersById: new Map(),
  sparklineHistory: new Map(),  // forwarder id -> last 60 inbound rates (for the list sparklines)
  lastSparklineSampleAt: 0,
  sort: preferences.get('sort', { key: 'name', direction: 1 }),
  filter: { text: '', status: 'all', tag: '' },
  selectedIds: new Set(),       // list view: checked rows for bulk actions
  currentPage: null,            // hooks of the visible page (see top of file)
  liveStream: null,             // EventSource of /api/stream
  updateStatus: null,           // state of the update check (GET /api/update, "update" live events)
  isWaitingForRestart: false,   // an update is being installed; see waitForRestart
  connectionLostAt: 0,          // when the live stream last dropped (Date.now()); a restart always drops it
  isConnected: true,
  localAddresses: null,         // IP addresses of the Delta machine (bind address dropdown); null = not loaded yet
};

// Stores a new forwarder list from the server and refreshes the screen.
function receiveForwarderList(list) {
  state.forwarders = list.map(normalizeForwarder);
  state.forwardersById = new Map(state.forwarders.map(fwd => [fwd.id, fwd]));

  // The list may arrive more often than once per second (after changes);
  // record at most one sparkline sample per second.
  const now = Date.now();
  if (now - state.lastSparklineSampleAt > 900) {
    state.lastSparklineSampleAt = now;
    for (const fwd of state.forwarders) {
      const samples = state.sparklineHistory.get(fwd.id) || [];
      samples.push(fwd.rate.in);
      if (samples.length > 60) samples.shift();
      state.sparklineHistory.set(fwd.id, samples);
    }
  }
  for (const id of state.selectedIds) {
    if (!state.forwardersById.has(id)) state.selectedIds.delete(id); // deleted meanwhile
  }
  updateSidebar();
  state.currentPage?.update?.();
}

// Adds or replaces one forwarder locally, e.g. right after creating it, so
// the UI does not have to wait for the next push from the server.
function receiveForwarder(fwd) {
  const others = state.forwarders.filter(other => other.id !== fwd.id);
  receiveForwarderList([...others, fwd]);
}

// Opens the live stream. EventSource reconnects by itself after network
// errors; only if the server rejected us (e.g. session expired) it stays closed.
function connectLiveStream() {
  state.liveStream?.close();
  const stream = new EventSource('/api/stream');
  state.liveStream = stream;
  stream.addEventListener('forwarders', message => {
    setConnected(true);
    receiveForwarderList(JSON.parse(message.data));
  });
  stream.addEventListener('event', message => state.currentPage?.onEvent?.(JSON.parse(message.data)));
  stream.addEventListener('update', message => receiveUpdateStatus(JSON.parse(message.data)));
  stream.onerror = async () => {
    setConnected(false);
    if (stream.readyState === EventSource.CLOSED) {
      // Still logged in? Then retry. A 401 means the session is gone, e.g.
      // because Delta restarted (sessions live in memory): show the login form.
      try {
        const response = await fetch('/api/session', { credentials: 'same-origin' });
        if (response.status === 401) return onSessionExpired();
      } catch { /* server unreachable right now: retry below */ }
      setTimeout(connectLiveStream, 2000);
    }
  };
}

// Stores a new state of the update check: shows the "update available" badge
// in the sidebar, refreshes the Settings page, and when Delta restarts into a
// new version, reloads the page once the new version answers.
function receiveUpdateStatus(status) {
  if (!status) return;
  const previousState = state.updateStatus?.state;
  state.updateStatus = status;
  const badge = $('#nav-update');
  if (badge) {
    const showBadge = status.available && status.state !== 'restarting';
    badge.classList.toggle('hidden', !showBadge);
    badge.title = showBadge ? `Delta ${status.latest} is available (running ${status.current}). Install it under Settings → Updates` : '';
  }
  state.currentPage?.onUpdateStatus?.(status);
  // Start waiting as soon as the install runs: the "restarting" event itself
  // often does not reach the browser, because Delta shuts down right after it.
  const isInstalling = ['installing', 'pending', 'restarting'].includes(status.state);
  if (isInstalling && !state.isWaitingForRestart) waitForRestart(status);
}

// After an update Delta replaces itself and starts again within a second or
// two. Sessions live in memory, so the restart logs everybody out: a 401 (or
// a different version) means the new process is up, and the page reloads.
function waitForRestart(status) {
  state.isWaitingForRestart = true;
  const startedWaitingAt = Date.now();
  let lastProgressAt = startedWaitingAt;
  const stopWaiting = () => { clearInterval(timer); state.isWaitingForRestart = false; };
  const timer = setInterval(async () => {
    if (state.updateStatus?.state === 'idle') return stopWaiting(); // the install failed; Settings shows why
    try {
      const response = await fetch('/api/session', { credentials: 'same-origin' });
      const session = response.ok ? await response.json() : null;
      // A 401 alone may just be an expired session while the old version is
      // still downloading. Only after the live stream dropped (Delta went
      // away) does it mean the new version is up.
      const restarted = state.connectionLostAt >= startedWaitingAt;
      if (response.status === 401 && !restarted) {
        stopWaiting();
        onSessionExpired();
        return;
      }
      if (response.status === 401 || (session && session.version !== status.current)) {
        stopWaiting();
        // Shown on the login form after the reload (see showEntryPage).
        try { sessionStorage.setItem('delta-login-message', `Delta was updated to ${status.latest}. Please sign in again.`); } catch { /* ignore */ }
        location.reload();
        return;
      }
    } catch { /* server is restarting right now */ }
    // While "pending" (installed, restart waits for a capture or replay to end) there is no time limit.
    if (state.updateStatus?.state === 'pending') lastProgressAt = Date.now();
    if (Date.now() - lastProgressAt > 10 * 60 * 1000) { // the download may take a while on a slow link
      stopWaiting();
      showToast('Delta did not come back after the update. Check the server and the event log.', 'error');
    }
  }, 1000);
}

function setConnected(connected) {
  if (!connected) state.connectionLostAt = Date.now();
  if (state.isConnected === connected) return;
  state.isConnected = connected;
  $('#connection-lost')?.classList.toggle('hidden', connected);
}

function onSessionExpired() {
  state.liveStream?.close();
  state.liveStream = null;
  state.currentPage?.destroy?.();
  state.currentPage = null;
  closeModal();
  showEntryPage('Your session has ended. Please sign in again.');
}

/* ================= UI building blocks ================= */

// Short notification in the bottom right corner.
function showToast(message, type = '') {
  const toast = document.createElement('div');
  toast.className = 'toast ' + type;
  toast.innerHTML = (type === 'error' ? icon('alert') : '') + `<span>${escapeHtml(message)}</span>`;
  $('#toasts').append(toast);
  setTimeout(() => toast.remove(), type === 'error' ? 6000 : 3000);
}
const showError = error => showToast(error.message || String(error), 'error');

/* ---------- hover hints ---------- */

// Any element with a title attribute shows a styled hint bubble when the
// mouse rests on it. The title is moved to data-hint so the browser's own
// (slow, unstyled) tooltip does not appear as well.
const hintBubble = document.createElement('div');
hintBubble.className = 'hint-bubble';
let hintTimer = null;
let hintTarget = null;

function hideHint() {
  clearTimeout(hintTimer);
  hintTarget = null;
  hintBubble.classList.remove('show');
}

function showHint(element) {
  const text = element.dataset.hint;
  if (!text) return;
  if (!hintBubble.isConnected) document.body.append(hintBubble);
  hintBubble.textContent = text;
  const anchor = element.getBoundingClientRect();
  const bubble = hintBubble.getBoundingClientRect();
  // Prefer above the element; below if there is no room. Keep inside the window.
  let top = anchor.top - bubble.height - 8;
  if (top < 6) top = anchor.bottom + 8;
  const left = Math.min(Math.max(6, anchor.left + anchor.width / 2 - bubble.width / 2), innerWidth - bubble.width - 6);
  hintBubble.style.top = top + 'px';
  hintBubble.style.left = left + 'px';
  hintBubble.classList.add('show');
}

document.addEventListener('mouseover', event => {
  const element = event.target.closest?.('[title], [data-hint]');
  if (element === hintTarget) return;
  if (element?.hasAttribute('title')) {
    element.dataset.hint = element.getAttribute('title');
    element.removeAttribute('title');
  }
  // Live parts of the page are redrawn every second, which replaces the element
  // under the mouse with a new one with the same hint, and the browser reports
  // that as a new "mouseover". Hiding and showing the bubble again then made it
  // flicker. Instead the bubble simply moves over to the new element.
  if (element && hintTarget && !hintTarget.isConnected && element.dataset.hint === hintTarget.dataset.hint) {
    const bubbleWasShown = hintBubble.classList.contains('show');
    hintTarget = element;
    if (bubbleWasShown) {
      showHint(element);
    } else {  // still waiting to show it: wait for the new element instead
      clearTimeout(hintTimer);
      hintTimer = setTimeout(() => { if (hintTarget === element && element.isConnected) showHint(element); }, 350);
    }
    return;
  }
  hideHint();
  if (!element) return;
  hintTarget = element;
  hintTimer = setTimeout(() => { if (hintTarget === element && element.isConnected) showHint(element); }, 350);
});
document.addEventListener('mousedown', hideHint);
// The mouse left the window: no element gets a "mouseover", so hide here.
document.addEventListener('mouseout', event => { if (!event.relatedTarget) hideHint(); });
window.addEventListener('scroll', event => {
  if (hintTarget && (event.target === document || event.target.contains?.(hintTarget))) hideHint();
}, true);

/* ---------- dropdown menu ---------- */

let openMenuElement = null;
let openMenuAnchor = null; // the button that opened the menu
function closeMenu() { openMenuElement?.remove(); openMenuElement = null; openMenuAnchor = null; }

// items: [{ label, icon, danger?, onSelect }] or '-' for a separator line.
function openMenu(anchorElement, items) {
  if (openMenuAnchor === anchorElement) { closeMenu(); return; } // second click on the button closes it
  closeMenu();
  const menu = document.createElement('div');
  menu.className = 'menu';
  for (const item of items) {
    if (item === '-') { menu.append(document.createElement('hr')); continue; }
    const button = document.createElement('button');
    button.className = item.danger ? 'danger' : '';
    button.innerHTML = icon(item.icon) + escapeHtml(item.label);
    button.onclick = event => { event.stopPropagation(); closeMenu(); item.onSelect(); };
    menu.append(button);
  }
  document.body.append(menu);
  // Open below the anchor, or above it if there is not enough room.
  const anchor = anchorElement.getBoundingClientRect();
  const size = menu.getBoundingClientRect();
  const fitsBelow = anchor.bottom + size.height + 8 <= innerHeight;
  menu.style.top = (fitsBelow ? anchor.bottom + 4 : anchor.top - size.height - 4) + 'px';
  menu.style.left = Math.max(8, anchor.right - size.width) + 'px';
  openMenuElement = menu;
  openMenuAnchor = anchorElement;
}
// Clicking anywhere else closes the menu. The click that opened the menu also
// reaches this handler, so clicks on the menu's own button are ignored here.
document.addEventListener('click', event => {
  if (!openMenuElement) return;
  if (openMenuElement.contains(event.target) || openMenuAnchor?.contains(event.target)) return;
  closeMenu();
});
// Close the menu when the page (or a box containing its button) scrolls, since
// the menu would no longer sit next to its button. Other scrolling boxes, such
// as the live inspector scrolling to new data, must not close it.
window.addEventListener('scroll', event => {
  if (openMenuAnchor && (event.target === document || event.target.contains?.(openMenuAnchor))) closeMenu();
}, true);

/* ---------- modal dialogs ---------- */

let modalOverlay = null;
function closeModal() { modalOverlay?.remove(); modalOverlay = null; }

// Shows a dialog and returns its overlay element. Clicking beside the dialog closes it.
function openModal(innerHtml, sizeClass = '') {
  closeModal();
  const overlay = document.createElement('div');
  overlay.className = 'overlay';
  overlay.innerHTML = `<div class="modal ${sizeClass}">${innerHtml}</div>`;
  overlay.addEventListener('mousedown', event => { if (event.target === overlay) closeModal(); });
  document.body.append(overlay);
  modalOverlay = overlay;
  setTimeout(() => $('input:not([type=checkbox]), select', overlay)?.focus(), 30);
  return overlay;
}
document.addEventListener('keydown', event => {
  if (event.key === 'Escape') { closeMenu(); closeModal(); }
  // "?" opens the help page, unless the user is typing or not logged in (no sidebar).
  if (event.key === '?' && !modalOverlay && !event.target.matches('input, textarea, select') && $('#main')) location.hash = '#/help';
});

// Yes/no question. Resolves to true if confirmed. `messageHtml` may contain markup.
function askConfirmation(title, messageHtml, confirmLabel = 'Confirm', isDangerous = true) {
  return new Promise(resolve => {
    const overlay = openModal(`
      <div class="modal-h"><h2>${escapeHtml(title)}</h2></div>
      <div class="modal-b"><div class="muted">${messageHtml}</div></div>
      <div class="modal-f">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn ${isDangerous ? 'danger solid' : 'primary'}" data-confirm>${escapeHtml(confirmLabel)}</button>
      </div>`, 'small');
    const finish = answer => { closeModal(); resolve(answer); };
    $('[data-cancel]', overlay).onclick = () => finish(false);
    $('[data-confirm]', overlay).onclick = () => finish(true);
    $('[data-confirm]', overlay).focus();
  });
}

// Asks for one line of text. Resolves to the text, or null if cancelled.
function askForText(title, label, initialValue = '', hint = '') {
  return new Promise(resolve => {
    const overlay = openModal(`
      <form>
        <div class="modal-h"><h2>${escapeHtml(title)}</h2></div>
        <div class="modal-b"><label class="f">${escapeHtml(label)}<input name="answer" value="${escapeHtml(initialValue)}">${hint ? `<span class="hint">${escapeHtml(hint)}</span>` : ''}</label></div>
        <div class="modal-f"><button type="button" class="btn" data-cancel>Cancel</button><button class="btn primary">OK</button></div>
      </form>`, 'small');
    $('[data-cancel]', overlay).onclick = () => { closeModal(); resolve(null); };
    $('form', overlay).onsubmit = event => {
      event.preventDefault();
      const answer = event.target.answer.value.trim();
      closeModal();
      resolve(answer);
    };
  });
}

// Replaces an element's content only if it changed. Used for parts that are
// re-rendered every second, so clicks and hover are not disturbed needlessly.
function setHtmlIfChanged(element, html) {
  if (element.lastRenderedHtml === html) return false;
  element.innerHTML = html;
  element.lastRenderedHtml = html;
  return true;
}

/* ================= charts ================= */

// The sparklines of the forwarder list are drawn here on a small canvas (dozens
// of them update every second, too many for a chart library). The traffic
// chart of the detail page uses uPlot (web/vendor, see renderTrafficTab).

const cssVariable = name => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

// Sizes the canvas for sharp drawing on high-DPI screens and clears it.
// Returns [context, width, height] in CSS pixels.
function prepareCanvas(canvas) {
  const pixelRatio = window.devicePixelRatio || 1;
  const width = canvas.clientWidth, height = canvas.clientHeight;
  if (canvas.width !== Math.round(width * pixelRatio) || canvas.height !== Math.round(height * pixelRatio)) {
    canvas.width = Math.round(width * pixelRatio);
    canvas.height = Math.round(height * pixelRatio);
  }
  const context = canvas.getContext('2d');
  context.setTransform(pixelRatio, 0, 0, pixelRatio, 0, 0);
  context.clearRect(0, 0, width, height);
  return [context, width, height];
}

// Tiny line chart of the last 60 seconds (forwarder list).
function drawSparkline(canvas, values, color) {
  const [context, width, height] = prepareCanvas(canvas);
  if (!width || values.length < 2) return;
  const maxValue = Math.max(...values) || 1;
  const stepX = width / 59;
  const firstSlot = 60 - values.length; // right-align: newest value at the right edge
  const points = values.map((value, i) => [(i + firstSlot) * stepX, height - 2 - (value / maxValue) * (height - 5)]);
  context.beginPath();
  points.forEach(([x, y], i) => (i ? context.lineTo(x, y) : context.moveTo(x, y)));
  context.strokeStyle = color;
  context.lineWidth = 1.5;
  context.lineJoin = 'round';
  context.stroke();
  // light fill below the line
  context.lineTo(points[points.length - 1][0], height);
  context.lineTo(points[0][0], height);
  context.globalAlpha = 0.12;
  context.fillStyle = color;
  context.fill();
  context.globalAlpha = 1;
}

/* ================= login, layout and navigation ================= */

const appRoot = $('#app');

function renderLoginPage(message = '') {
  appRoot.innerHTML = `
    <div class="login">
      <form class="card">
        <div class="logo">${LOGO_SVG}</div>
        <h1>${APP_NAME}</h1>
        <div class="tagline">${APP_TAGLINE}</div>
        <p>Sign in to manage your forwarders</p>
        <input type="password" name="password" placeholder="Password" autocomplete="current-password" required autofocus>
        <button class="btn primary">Sign in</button>
        <div class="form-err" style="text-align:center;margin:0">${escapeHtml(message)}</div>
      </form>
      ${WAVES_SVG}
    </div>`;
  const form = $('form', appRoot);
  form.onsubmit = async event => {
    event.preventDefault();
    const button = $('button', form);
    button.disabled = true;
    try {
      await apiRequest('POST', '/api/login', { password: form.password.value });
      startApp();
    } catch (error) {
      $('.form-err', form).textContent = error.message;
      form.password.select();
    } finally {
      button.disabled = false;
    }
  };
}

// The first-start page, shown while the config has no password yet. Until it
// is submitted the server refuses every other request. Besides the password
// (required) it asks for the main settings, filled with the values from the
// config file (`defaults` from GET /api/setup), so a new installation can be
// set up in one step. Everything can be changed later under Settings.
function renderSetupPage(defaults = {}) {
  const value = key => escapeHtml(defaults[key] ?? '');
  appRoot.innerHTML = `
    <div class="login">
      <form class="card setup-form" novalidate>
        <div class="logo">${LOGO_SVG}</div>
        <h1>Welcome to ${APP_NAME}</h1>
        <div class="tagline">${APP_TAGLINE}</div>
        <p>First start: choose a password and check the basic settings. Everything except the password is optional and can be changed later.</p>

        <div class="sect">
          <div class="sect-t">${icon('key')}Password</div>
          <div class="grid2">
            <label class="f" title="Protects the web interface and the REST API. Stored only as a secure hash">Password<input type="password" name="password" placeholder="at least 8 characters" autocomplete="new-password" minlength="8" required autofocus></label>
            <label class="f" title="Type the password again to rule out typos">Repeat<input type="password" name="repeat" placeholder="repeat password" autocomplete="new-password" required></label>
          </div>
        </div>

        <div class="sect">
          <div class="sect-t">${icon('sliders')}Web interface</div>
          <div class="grid2">
            <label class="f" title="Port of this web interface. A change takes effect after Delta restarts">Port<input type="number" name="port" min="1" max="65535" step="1" value="${value('port')}"></label>
            <label class="f" title="IP address the web interface listens on. Empty = all interfaces. A change takes effect after Delta restarts">Bind address<input name="bind" value="${value('bind')}" placeholder="all interfaces"></label>
          </div>
          <label class="f" title="How long a login stays valid before you have to sign in again">Stay signed in for (hours)<input type="number" name="sessionHours" min="1" max="8760" step="1" value="${value('session_hours')}" style="max-width:160px"></label>
          <div class="hint faint" id="setup-address-note">Currently reachable at ${value('web_addr')}. A new port or address takes effect after Delta restarts; the <code>-port</code> and <code>-bind</code> options override it.</div>
        </div>

        <div class="sect">
          <div class="sect-t">${icon('download')}Files</div>
          <label class="f" title="Where recorded captures are stored. A relative path is next to the config file">Data directory<input name="dataDir" value="${value('data_dir')}" placeholder="delta-data"></label>
          <div class="grid2">
            <label class="f" title="File every event is appended to (syslog-style text lines). A relative path is next to the config file; off keeps events in memory only">Event log file<input name="eventLog" value="${value('event_log')}" placeholder="delta-events.log"></label>
            <label class="f" title="When the event log reaches this size it is renamed with a timestamp and a new file is started">Rotate event log at (MB)<input type="number" name="eventLogMaxMB" min="1" step="1" value="${value('event_log_max_mb')}"></label>
          </div>
          <div class="hint faint">Config file: ${value('config_path')}. Relative paths are resolved next to it.</div>
        </div>

        <div class="sect">
          <div class="sect-t">${icon('bell')}Alerts</div>
          <label class="f" title="Optional. Receives a JSON POST when an alert fires or resolves (Slack, Mattermost, Teams, Discord, ntfy …)">Webhook URL<input name="webhook" value="${value('webhook_url')}" placeholder="optional, e.g. https://hooks.slack.com/…"></label>
          <label class="f" title="A forwarder that failed to start or has a disconnected endpoint for this long raises an alert">Alert when a forwarder is down for (s)<input type="number" name="downAfter" min="1" step="1" value="${value('down_after')}" style="max-width:160px"></label>
        </div>

        <div class="sect">
          <div class="sect-t">${icon('download')}Updates</div>
          <label class="chk" title="Ask GitHub for a new Delta release shortly after startup and once a day. Needs internet access; nothing is sent except the request itself"><input type="checkbox" name="updateCheck" ${defaults.update_check ? 'checked' : ''}> Check for updates</label>
          <label class="chk" title="Download, verify and install a new release by itself, then restart Delta. Forwarders are back within seconds"><input type="checkbox" name="updateAutoInstall" ${defaults.update_auto_install ? 'checked' : ''} ${defaults.update_check ? '' : 'disabled'}> Install updates automatically</label>
          <div class="hint faint">Off by default, for servers without internet access. You can check and install by hand under Settings at any time.</div>
        </div>

        <button class="btn primary" title="Save the password and these settings, then open Delta">Save &amp; start</button>
        <div class="form-err" style="text-align:center;margin:0"></div>
      </form>
      ${WAVES_SVG}
    </div>`;
  const form = $('form', appRoot);
  const showFormError = text => { $('.form-err', form).textContent = text; };
  // Automatic install only makes sense while the check is on.
  form.updateCheck.onchange = () => {
    form.updateAutoInstall.disabled = !form.updateCheck.checked;
    if (!form.updateCheck.checked) form.updateAutoInstall.checked = false;
  };
  form.onsubmit = async event => {
    event.preventDefault();
    if (form.password.value.length < 8) return showFormError('The password must have at least 8 characters.');
    if (form.password.value !== form.repeat.value) return showFormError('The passwords do not match.');
    const button = $('button.primary', form);
    button.disabled = true;
    try {
      const result = await apiRequest('POST', '/api/setup', { // also logs us in
        password: form.password.value,
        port: Number(form.port.value) || 0,
        bind: form.bind.value.trim(),
        session_hours: Number(form.sessionHours.value) || 0,
        data_dir: form.dataDir.value.trim(),
        event_log: form.eventLog.value.trim(),
        event_log_max_mb: Number(form.eventLogMaxMB.value) || 0,
        webhook_url: form.webhook.value.trim(),
        down_after: Number(form.downAfter.value) || 0,
        update_check: form.updateCheck.checked,
        update_auto_install: form.updateCheck.checked && form.updateAutoInstall.checked,
      });
      startApp();
      if (result.restart_needed) showToast('Settings saved. The new web port / address takes effect after Delta restarts.');
    } catch (error) {
      showFormError(error.message);
      button.disabled = false;
    }
  };
}

// Decides what to show when the page loads or the session ends:
// the setup page (no password yet), the login form, or the app.
async function showEntryPage(loginMessage = '') {
  // A message left before a reload, e.g. "Delta was updated" (see waitForRestart).
  try {
    loginMessage = sessionStorage.getItem('delta-login-message') || loginMessage;
    sessionStorage.removeItem('delta-login-message');
  } catch { /* storage blocked: no message */ }
  try {
    const setup = await apiRequest('GET', '/api/setup');
    if (setup.setup_required) return renderSetupPage(setup.defaults);
  } catch { /* server unreachable: fall through to the login form */ }
  try {
    await apiRequest('GET', '/api/session');
    startApp();
  } catch {
    renderLoginPage(loginMessage);
  }
}

// The theme follows the operating system unless the user picked one (stored in localStorage).
function isDarkTheme() {
  const chosenTheme = document.documentElement.dataset.theme;
  return chosenTheme ? chosenTheme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
}

// Sidebar with navigation and live totals, plus the <main> area for pages.
function renderAppLayout() {
  appRoot.innerHTML = `
    <div class="shell">
      <aside class="side">
        <div class="brand" title="${APP_NAME} – ${APP_TAGLINE}: forwards NMEA and other data streams between TCP/UDP endpoints">
          <div class="logo">${LOGO_SVG}</div>
          <div><div class="brand-name">${APP_NAME}</div><div class="brand-sub">${APP_TAGLINE}</div></div>
        </div>
        <nav class="nav">
          <a href="#/" data-nav="list" title="All forwarders with their live state">${icon('layers')}<span>Forwarders</span><span class="count hidden" id="nav-issue-count" title="Forwarders with a problem"></span></a>
          <a href="#/events" data-nav="events" title="Log of connections, state changes, errors and alerts">${icon('bell')}<span>Events</span></a>
          <a href="#/captures" data-nav="captures" title="Recorded traffic: download it or replay it into a forwarder">${icon('disc')}<span>Captures</span></a>
          <a href="#/settings" data-nav="settings" title="Alerts, updates, password, backup and import">${icon('sliders')}<span>Settings</span><span class="count update hidden" id="nav-update">new</span></a>
          <a href="#/help" data-nav="help" title="How Delta works (shortcut: ?)">${icon('help')}<span>Help</span></a>
        </nav>
        <div class="side-foot">
          <div class="live" id="live-totals" title="Live totals of all running forwarders, per second"></div>
          <div class="row">
            <button class="btn ghost icon" id="theme-toggle" title="Toggle theme">${icon(isDarkTheme() ? 'sun' : 'moon')}</button>
            <span class="grow"></span>
            <button class="btn ghost sm" id="logout" title="End this session">${icon('logout')}<span>Sign out</span></button>
          </div>
        </div>
      </aside>
      <main class="main" id="main"></main>
    </div>
    <div class="conn-lost hidden" id="connection-lost">Connection lost — reconnecting…</div>`;

  $('#theme-toggle').onclick = () => {
    const newTheme = isDarkTheme() ? 'light' : 'dark';
    document.documentElement.dataset.theme = newTheme;
    try { localStorage.setItem('delta-theme', newTheme); } catch { /* ignore */ }
    $('#theme-toggle').innerHTML = icon(newTheme === 'dark' ? 'sun' : 'moon');
    state.currentPage?.redraw?.(); // charts read their colors from CSS
  };
  $('#logout').onclick = async () => {
    await apiRequest('POST', '/api/logout').catch(() => {});
    state.liveStream?.close();
    state.currentPage?.destroy?.();
    state.currentPage = null;
    renderLoginPage();
  };
}

// Live totals in the sidebar and the red issue counter next to "Forwarders".
function updateSidebar() {
  const totalsBox = $('#live-totals');
  if (!totalsBox) return;
  let rateIn = 0, rateOut = 0, runningCount = 0, issueCount = 0;
  for (const fwd of state.forwarders) {
    rateIn += fwd.rate.in;
    rateOut += fwd.rate.out;
    if (fwd.state === 'running') runningCount++;
    if (hasIssue(fwd)) issueCount++;
  }
  totalsBox.innerHTML = `
    <div class="lbl"><span class="dotc" style="background:var(--accent)"></span>In <span class="grow"></span><span class="v">${formatRate(rateIn)}</span></div>
    <div class="lbl"><span class="dotc" style="background:var(--out)"></span>Out <span class="grow"></span><span class="v">${formatRate(rateOut)}</span></div>
    <div class="lbl" style="margin-top:4px"><span class="dotc" style="background:var(--ok)"></span>Running <span class="grow"></span><span class="v">${runningCount}/${state.forwarders.length}</span></div>`;
  const issueBadge = $('#nav-issue-count');
  issueBadge.textContent = issueCount;
  issueBadge.classList.toggle('hidden', !issueCount);
}

// Shows the page that belongs to the current URL hash.
function showPageForUrl() {
  state.currentPage?.destroy?.();
  state.currentPage = null;
  closeMenu();
  closeModal();
  // e.g. "#/f/<forwarder id>" or "#/captures/<capture name>"
  const [, section, forwarderId] = (location.hash.slice(1) || '/').split('/');
  const navName = section === 'f' ? 'list' : section || 'list';
  $$('.nav a').forEach(link => link.classList.toggle('active', link.dataset.nav === navName));

  const main = $('#main');
  main.onclick = main.oninput = main.onchange = null; // remove the previous page's handlers
  window.scrollTo(0, 0);
  if (section === 'f' && forwarderId) state.currentPage = renderDetailPage(main, decodeURIComponent(forwarderId));
  else if (section === 'events') state.currentPage = renderEventsPage(main);
  else if (section === 'captures' && forwarderId) state.currentPage = renderCaptureViewerPage(main, decodeURIComponent(forwarderId));
  else if (section === 'captures') state.currentPage = renderCapturesPage(main);
  else if (section === 'settings') state.currentPage = renderSettingsPage(main);
  else if (section === 'help') state.currentPage = renderHelpPage(main, forwarderId && decodeURIComponent(forwarderId)); // help.js
  else state.currentPage = renderForwarderListPage(main);
}

async function startApp() {
  // Pages set their own scroll position (top, or a help section); the browser
  // restoring the old position after a reload would undo that.
  history.scrollRestoration = 'manual';
  renderAppLayout();
  try {
    receiveForwarderList(await apiRequest('GET', '/api/forwarders'));
  } catch (error) { showError(error); }
  apiRequest('GET', '/api/update').then(receiveUpdateStatus).catch(() => {});
  connectLiveStream();
  window.onhashchange = showPageForUrl;
  showPageForUrl();
}

/* ================= actions used on several pages ================= */

// action: "start" | "stop" | "restart"
async function setForwardersRunning(ids, action) {
  try {
    if (ids.length === 1) await apiRequest('POST', `/api/forwarders/${ids[0]}/${action}`);
    else await apiRequest('POST', '/api/bulk', { ids, action });
    const pastTense = { start: 'Started', stop: 'Stopped', restart: 'Restarted' }[action];
    showToast(ids.length === 1
      ? `${pastTense} ${state.forwardersById.get(ids[0])?.name ?? ''}`
      : `${pastTense} ${ids.length} forwarders`);
  } catch (error) { showError(error); }
}

// Asks for confirmation, then deletes. Resolves to true if deleted.
async function deleteForwarders(ids) {
  const names = ids.map(id => state.forwardersById.get(id)?.name).filter(Boolean);
  const what = ids.length === 1 ? `<b>${escapeHtml(names[0])}</b>` : `<b>${ids.length} forwarders</b>`;
  const title = ids.length === 1 ? 'Delete forwarder' : 'Delete forwarders';
  if (!await askConfirmation(title, `${what} will be stopped and removed. Captures are kept.`, 'Delete')) return false;
  try {
    if (ids.length === 1) await apiRequest('DELETE', '/api/forwarders/' + ids[0]);
    else await apiRequest('POST', '/api/bulk', { ids, action: 'delete' });
    ids.forEach(id => state.selectedIds.delete(id));
    showToast(`Deleted ${ids.length === 1 ? names[0] : ids.length + ' forwarders'}`);
    return true;
  } catch (error) { showError(error); return false; }
}

// Downloads the configuration of the given forwarders as a JSON file (importable in Settings).
function exportForwarders(ids) {
  const configs = ids.map(id => state.forwardersById.get(id)).filter(Boolean).map(configOf);
  const fileName = configs.length === 1 ? `delta-${toFileName(configs[0].name) || 'forwarder'}.json` : 'delta-forwarders.json';
  downloadFile(fileName, JSON.stringify({ forwarders: configs }, null, 2));
}

// Asks for a JSON file and imports the forwarders in it (new and stopped).
// Accepts our export format {"forwarders": [...]} and a plain array.
function importForwardersFromFile() {
  const fileInput = Object.assign(document.createElement('input'), { type: 'file', accept: '.json,application/json' });
  fileInput.onchange = async () => {
    const file = fileInput.files[0];
    if (!file) return;
    try {
      const parsed = JSON.parse(await file.text());
      const forwarders = Array.isArray(parsed) ? parsed : parsed.forwarders;
      if (!Array.isArray(forwarders)) throw new Error('No forwarders found in this file');
      const result = await apiRequest('POST', '/api/import', { forwarders });
      showToast(`Imported ${result.imported} forwarder(s) — they are stopped, start them when ready`);
    } catch (error) { showError(error); }
  };
  fileInput.click();
}

// The "⋯" menu of a forwarder (list row and detail page).
function openForwarderMenu(anchorElement, fwd, afterDelete) {
  openMenu(anchorElement, [
    { label: 'Edit', icon: 'edit', onSelect: () => openForwarderEditor(fwd) },
    { label: 'Duplicate', icon: 'copy', onSelect: () => openForwarderEditor({ ...configOf(fwd), id: undefined, name: fwd.name + ' copy' }) },
    { label: 'Export JSON', icon: 'download', onSelect: () => exportForwarders([fwd.id]) },
    '-',
    { label: 'Delete', icon: 'trash', danger: true, onSelect: async () => { if (await deleteForwarders([fwd.id])) afterDelete?.(); } },
  ]);
}

/* ================= page: forwarder list ================= */

// Table columns: [header, sort key (null = not sortable), CSS class, hover hint]
const LIST_COLUMNS = [
  ['', null],
  ['Name', 'name', '', 'Click to sort by name'],
  ['Input', null, 'hide-sm', 'Where the data comes from'],
  ['Outputs', null, 'hide-sm', 'Where the data is sent to (every output gets a copy)'],
  ['Traffic in', 'rate', 'right', 'Received bytes per second; the line shows the last 60 seconds'],
  ['Msg/s', 'msgs', 'right hide-sm', 'Received messages per second (with line framing: NMEA sentences per second)'],
  ['Clients in', 'clientsIn', 'right hide-sm', 'Sources connected to the input: TCP clients, active UDP senders or the remote host Delta connected to'],
  ['Clients out', 'clientsOut', 'right hide-sm', 'Receivers connected to the outputs: TCP clients and the targets Delta connected to'],
  ['Drops', 'drops', 'right hide-sm', 'Messages thrown away because a receiver was too slow or a buffer was full'],
  ['', null],
];

// Value used for sorting by each sortable column.
const SORT_VALUE = {
  name: fwd => fwd.name.toLowerCase(),
  rate: fwd => fwd.rate.in,
  msgs: fwd => fwd.rate.msgs,
  clientsIn: fwd => fwd.clients_in,
  clientsOut: fwd => fwd.clients_out,
  drops: fwd => fwd.drops,
  state: fwd => HEALTH_SORT_ORDER[fwd.health],
};

function renderForwarderListPage(main) {
  main.innerHTML = `
    <div class="page-head">
      <div class="grow"><h1>Forwarders</h1><div class="sub" id="list-subtitle"></div></div>
      <button class="btn" data-action="import" title="Add forwarders from a JSON file exported by Delta. They are added as new and stay stopped.">${icon('upload')}Import</button>
      <button class="btn" data-action="export-all" title="Download the configuration of all forwarders as a JSON file">${icon('download')}Export</button>
      <button class="btn primary" data-action="new" title="Create a forwarder (shortcut: N)">${icon('plus')}New forwarder</button>
    </div>
    <div class="tiles" id="summary-tiles"></div>
    <div id="bulk-bar"></div>
    <div class="card">
      <div class="toolbar" style="padding:12px 12px 0">
        <div class="search">${icon('search')}<input id="search" placeholder="Search…" title="Search name, address, tag ( / )" value="${escapeHtml(state.filter.text)}"></div>
        <div class="seg" id="status-filter">
          ${[['all', 'All', 'Show all forwarders'], ['running', 'Running', 'Only running forwarders'],
            ['issues', 'Issues', 'Only forwarders that failed, have a disconnected endpoint or a firing alert'], ['stopped', 'Stopped', 'Only stopped forwarders']]
            .map(([value, label, hint]) => `<button data-status="${value}" class="${state.filter.status === value ? 'on' : ''}" title="${hint}">${label}</button>`).join('')}
        </div>
        <select id="tag-filter" title="Show only forwarders with this tag"></select>
      </div>
      <div class="tbl-wrap" style="margin-top:8px"><table class="t">
        <thead id="table-head"></thead>
        <tbody id="table-body"></tbody>
      </table></div>
      <div id="empty-message"></div>
    </div>`;

  // Rows are rebuilt only when this signature changes (structure, names,
  // states …). Numbers that change every second are patched in place.
  let tableSignature = '';

  // Forwarders after search/filter, in the chosen sort order.
  function visibleForwarders() {
    const searchText = state.filter.text.trim().toLowerCase();
    const visible = state.forwarders.filter(fwd => {
      const status = state.filter.status;
      if (status === 'running' && fwd.state !== 'running') return false;
      if (status === 'stopped' && fwd.state !== 'stopped') return false;
      if (status === 'issues' && !hasIssue(fwd)) return false;
      if (state.filter.tag && !fwd.tags.includes(state.filter.tag)) return false;
      if (searchText) {
        const searchable = [fwd.name, fwd.comment, ...fwd.tags, fwd.input.mode, ...allEndpointAddresses(fwd.input),
          ...fwd.outputs.flatMap(out => [out.mode, out.name, ...allEndpointAddresses(out)])].join(' ').toLowerCase();
        if (!searchable.includes(searchText)) return false;
      }
      return true;
    });
    const sortValue = SORT_VALUE[state.sort.key] || SORT_VALUE.name;
    visible.sort((a, b) => {
      const valueA = sortValue(a), valueB = sortValue(b);
      return (valueA < valueB ? -1 : valueA > valueB ? 1 : 0) * state.sort.direction;
    });
    return visible;
  }

  function renderTableHead(visible) {
    const allSelected = visible.length > 0 && visible.every(fwd => state.selectedIds.has(fwd.id));
    $('#table-head').innerHTML = '<tr>' + LIST_COLUMNS.map(([label, sortKey, cssClass, hint], index) => {
      if (index === 0) return `<th style="width:34px"><input type="checkbox" data-select-all ${allSelected ? 'checked' : ''} title="Select all shown forwarders"></th>`;
      const arrow = state.sort.key === sortKey ? `<span class="arrow">${state.sort.direction > 0 ? '↑' : '↓'}</span>` : '';
      const fullHint = hint && sortKey ? `${hint}. Click to sort` : hint;
      return `<th class="${sortKey ? 'sort' : ''} ${cssClass || ''}" ${sortKey ? `data-sort="${sortKey}"` : ''} ${fullHint ? `title="${escapeHtml(fullHint)}"` : ''}>${label}${arrow}</th>`;
    }).join('') + '</tr>';
  }

  function renderRow(fwd) {
    const isRunning = fwd.state === 'running';
    const isSelected = state.selectedIds.has(fwd.id);
    const alertPill = fwd.firing.length ? `<span class="pill error" title="Alert firing">${icon('alert')}${fwd.firing.join(', ')}</span>` : '';
    const recordingPill = fwd.capture ? `<span class="pill error" title="Recording">${icon('record')}REC</span>` : '';
    const tagChips = fwd.tags.map(tag => `<span class="tag ${state.filter.tag === tag ? 'on' : ''}" data-tag="${escapeHtml(tag)}" title="Click to show only forwarders tagged “${escapeHtml(tag)}”">${escapeHtml(tag)}</span>`).join('');
    const problem = endpointProblems(fwd)[0];
    const problemText = fwd.error || (problem && `${problem.label}: ${problem.reason}`);
    const errorText = problemText ? `<span class="ferr" title="${escapeHtml(problemText)}">${escapeHtml(problemText)}</span>` : '';
    const secondLine = alertPill + recordingPill + tagChips + errorText;
    // The comment as one quiet line under the name, cut off with "…" when it is
    // too long (CSS). The hover bubble shows all of it, line breaks included.
    const commentLine = fwd.comment ? `<div class="fcomment" title="${escapeHtml(fwd.comment)}">${escapeHtml(fwd.comment)}</div>` : '';
    return `<tr data-id="${fwd.id}" class="${isSelected ? 'sel' : ''}">
      <td><input type="checkbox" data-select ${isSelected ? 'checked' : ''} title="Select for bulk actions (start, stop, tag, export, delete …)"></td>
      <td><div class="fname">
        <span class="dot ${healthClass(fwd.health)} ${fwd.health === 'ok' ? 'pulse' : ''}" title="${HEALTH_HINTS[fwd.health]}"></span>
        <div class="fname-text"><a href="#/f/${fwd.id}" title="Open details, live inspector and charts">${escapeHtml(fwd.name)}</a>${commentLine}${secondLine ? `<div class="tags">${secondLine}</div>` : ''}</div>
      </div></td>
      <td class="hide-sm">${endpointBadge(fwd.input, fwd.in_status, isRunning)}</td>
      <td class="hide-sm"><div class="eps">${fwd.outputs.map((out, i) => endpointBadge(out, fwd.out_status?.[i], isRunning)).join('')}</div></td>
      <td><div class="traffic"><canvas class="spark"></canvas><span class="mono" data-value="rate" style="min-width:70px;text-align:right"></span></div></td>
      <td class="num hide-sm" data-value="msgs"></td>
      <td class="num hide-sm" data-value="clients-in"></td>
      <td class="num hide-sm" data-value="clients-out"></td>
      <td class="num hide-sm" data-value="drops"></td>
      <td><div class="actions">
        ${isRunning || fwd.state === 'error'
          ? `<button class="btn icon sm stop" data-action="stop" title="Stop (stays stopped after a restart of Delta)">${icon('stop')}</button>`
          : `<button class="btn icon sm go" data-action="start" title="Start (also starts automatically with Delta)">${icon('play')}</button>`}
        <button class="btn icon sm ghost" data-action="menu" title="Edit, duplicate, export or delete">${icon('more')}</button>
      </div></td>
    </tr>`;
  }

  function renderSummaryTiles() {
    let runningCount = 0, rateIn = 0, rateOut = 0, messagesPerSecond = 0, clientsIn = 0, clientsOut = 0, issueCount = 0;
    for (const fwd of state.forwarders) {
      if (fwd.state === 'running') runningCount++;
      rateIn += fwd.rate.in;
      rateOut += fwd.rate.out;
      messagesPerSecond += fwd.rate.msgs;
      clientsIn += fwd.clients_in;
      clientsOut += fwd.clients_out;
      if (hasIssue(fwd)) issueCount++;
    }
    // The tiles are built once; afterwards only their numbers change. Replacing
    // them every second would disturb the hover hint of the tile under the mouse.
    const tiles = $('#summary-tiles');
    if (!tiles.querySelector('[data-tile]')) {
      tiles.innerHTML = `
        <div class="tile" title="Running forwarders / all forwarders"><div class="k">${icon('layers')}Running</div><div class="v"><span data-tile="running"></span><small data-tile="total"></small></div></div>
        <div class="tile" title="Bytes per second received by all inputs together"><div class="k">${icon('arrowIn')}Inbound</div><div class="v" data-tile="in"></div></div>
        <div class="tile" title="Bytes per second sent by all outputs together. Higher than inbound when data goes to several outputs or clients"><div class="k">${icon('arrowOut')}Outbound</div><div class="v" data-tile="out"></div></div>
        <div class="tile" title="Messages received per second by all inputs (with line framing: NMEA sentences)"><div class="k">${icon('activity')}Messages</div><div class="v"><span data-tile="messages"></span><small>/s</small></div></div>
        <div class="tile" title="Sources connected to the inputs of all forwarders: TCP clients, active UDP senders and remote hosts Delta connected to"><div class="k">${icon('arrowIn')}Clients in</div><div class="v" data-tile="clients-in"></div></div>
        <div class="tile" title="Receivers connected to the outputs of all forwarders: TCP clients and targets Delta connected to"><div class="k">${icon('arrowOut')}Clients out</div><div class="v" data-tile="clients-out"></div></div>
        <div class="tile" data-tile-issues title="Forwarders that failed to start, have a disconnected endpoint or a firing alert. Use the Issues filter to list them"><div class="k">${icon('alert')}Issues</div><div class="v" data-tile="issues"></div></div>`;
    }
    const tileValues = {
      running: runningCount, total: `/ ${state.forwarders.length}`, in: formatRate(rateIn), out: formatRate(rateOut),
      messages: formatCount(Math.round(messagesPerSecond)), 'clients-in': clientsIn, 'clients-out': clientsOut, issues: issueCount,
    };
    for (const [name, value] of Object.entries(tileValues)) {
      const valueElement = tiles.querySelector(`[data-tile="${name}"]`);
      if (valueElement.textContent !== String(value)) valueElement.textContent = value;
    }
    tiles.querySelector('[data-tile-issues]').classList.toggle('bad', issueCount > 0);
    const count = state.forwarders.length;
    $('#list-subtitle').textContent = `${count} forwarder${count === 1 ? '' : 's'} · ${runningCount} running`;
  }

  function renderTagFilter() {
    const allTags = [...new Set(state.forwarders.flatMap(fwd => fwd.tags))].sort();
    const optionsHtml = `<option value="">All tags</option>`
      + allTags.map(tag => `<option ${state.filter.tag === tag ? 'selected' : ''}>${escapeHtml(tag)}</option>`).join('');
    const select = $('#tag-filter');
    setHtmlIfChanged(select, optionsHtml); // keep an open dropdown open
    select.classList.toggle('hidden', !allTags.length);
  }

  function renderBulkBar() {
    const count = state.selectedIds.size;
    $('#bulk-bar').innerHTML = count ? `<div class="bulk">
      <b>${count} selected</b><span class="grow"></span>
      <button class="btn sm" data-bulk="start" title="Start all selected forwarders">${icon('play')}Start</button>
      <button class="btn sm" data-bulk="stop" title="Stop all selected forwarders">${icon('stop')}Stop</button>
      <button class="btn sm" data-bulk="restart" title="Restart all selected forwarders">${icon('restart')}Restart</button>
      <button class="btn sm" data-bulk="tag" title="Add a tag to (or remove it from) the selected forwarders">${icon('tag')}Tag</button>
      <button class="btn sm" data-bulk="export" title="Download the selected forwarders as a JSON file">${icon('download')}Export</button>
      <button class="btn sm" data-bulk="delete" title="Stop and delete the selected forwarders">${icon('trash')}Delete</button>
      <button class="btn sm icon" data-bulk="clear" title="Clear selection">${icon('x')}</button></div>` : '';
  }

  function update() {
    const visible = visibleForwarders();
    renderSummaryTiles();
    renderTagFilter();

    const newSignature = JSON.stringify([state.sort, state.filter.tag,
      visible.map(fwd => [fwd.id, fwd.name, fwd.tags, fwd.comment, fwd.health, fwd.state, fwd.error, fwd.firing, !!fwd.capture,
        fwd.input, fwd.outputs, fwd.in_status?.state, fwd.in_status?.error, fwd.out_status?.map(out => [out.state, out.error]), state.selectedIds.has(fwd.id)])]);
    if (newSignature !== tableSignature) {
      tableSignature = newSignature;
      renderTableHead(visible);
      let rowsHtml = '';
      for (const fwd of visible) rowsHtml += renderRow(fwd);
      $('#table-body').innerHTML = rowsHtml;
      $('#empty-message').innerHTML = visible.length ? '' : state.forwarders.length
        ? `<div class="empty">No forwarder matches the current filter.</div>`
        : `<div class="empty"><div class="big">${icon('layers')}</div><h2>No forwarders yet</h2>
           <p>A forwarder receives data on an input, from one sender or many, and sends it to one or more outputs.</p>
           <div class="row" style="justify-content:center">
             <button class="btn primary" data-action="new">${icon('plus')}Create your first forwarder</button>
             <button class="btn" data-action="import">${icon('upload')}Import from file</button>
           </div></div>`;
    }

    // Patch the live numbers of every row.
    const sparklineColor = cssVariable('--accent');
    for (const row of $$('#table-body tr[data-id]')) {
      const fwd = state.forwardersById.get(row.dataset.id);
      if (!fwd) continue;
      const isRunning = fwd.state === 'running';
      $('[data-value=rate]', row).textContent = isRunning ? formatRate(fwd.rate.in) : '—';
      $('[data-value=msgs]', row).textContent = isRunning ? formatCount(Math.round(fwd.rate.msgs)) : '—';
      $('[data-value=clients-in]', row).textContent = isRunning ? fwd.clients_in : '—';
      $('[data-value=clients-out]', row).textContent = isRunning ? fwd.clients_out : '—';
      const dropsCell = $('[data-value=drops]', row);
      dropsCell.textContent = isRunning ? formatCount(fwd.drops) : '—';
      dropsCell.classList.toggle('bad', fwd.drops > 0);
      drawSparkline($('canvas', row), state.sparklineHistory.get(fwd.id) || [], sparklineColor);
    }
    renderBulkBar();
  }

  async function runBulkAction(action) {
    const ids = [...state.selectedIds];
    if (action === 'clear') state.selectedIds.clear();
    else if (action === 'export') exportForwarders(ids);
    else if (action === 'delete') await deleteForwarders(ids);
    else if (action === 'tag') {
      const tag = await askForText('Add tag', `Tag for ${ids.length} forwarder(s)`, '', 'Prefix with "-" to remove the tag instead.');
      if (tag) {
        const remove = tag.startsWith('-');
        await apiRequest('POST', '/api/bulk', { ids, action: remove ? 'untag' : 'tag', value: remove ? tag.slice(1) : tag }).catch(showError);
      }
    } else {
      await setForwardersRunning(ids, action); // start / stop / restart
    }
    update();
  }

  // One click handler for the whole page (event delegation).
  main.onclick = async event => {
    const target = event.target;
    const row = target.closest('tr[data-id]');
    const fwd = row && state.forwardersById.get(row.dataset.id);
    const actionButton = target.closest('[data-action]');
    const action = actionButton?.dataset.action;

    if (action === 'new') return openForwarderEditor();
    if (action === 'import') return importForwardersFromFile();
    if (action === 'export-all') return exportForwarders(state.forwarders.map(other => other.id));
    if (fwd && (action === 'start' || action === 'stop')) return setForwardersRunning([fwd.id], action);
    if (fwd && action === 'menu') return openForwarderMenu(actionButton, fwd);
    if (target.dataset.tag !== undefined) { // clicking a tag filters by it (click again to clear)
      state.filter.tag = state.filter.tag === target.dataset.tag ? '' : target.dataset.tag;
      return update();
    }
    const statusFilter = target.closest('[data-status]')?.dataset.status;
    if (statusFilter) {
      state.filter.status = statusFilter;
      $$('#status-filter button').forEach(button => button.classList.toggle('on', button.dataset.status === statusFilter));
      return update();
    }
    const sortKey = target.closest('[data-sort]')?.dataset.sort;
    if (sortKey) {
      // Same column again reverses the order; numbers start with the largest value.
      const direction = state.sort.key === sortKey ? -state.sort.direction : sortKey === 'name' ? 1 : -1;
      state.sort = { key: sortKey, direction };
      preferences.set('sort', state.sort);
      return update();
    }
    const bulkAction = target.closest('[data-bulk]')?.dataset.bulk;
    if (bulkAction) return runBulkAction(bulkAction);
    // A click anywhere else on a row opens the forwarder.
    if (fwd && !target.closest('a, button, input')) location.hash = '#/f/' + fwd.id;
  };
  main.onchange = event => {
    const target = event.target;
    if (target.matches('[data-select]')) {
      const id = target.closest('tr').dataset.id;
      if (target.checked) state.selectedIds.add(id); else state.selectedIds.delete(id);
    } else if (target.matches('[data-select-all]')) {
      for (const fwd of visibleForwarders()) {
        if (target.checked) state.selectedIds.add(fwd.id); else state.selectedIds.delete(fwd.id);
      }
    } else if (target.id === 'tag-filter') {
      state.filter.tag = target.value;
    }
    update();
  };
  main.oninput = event => {
    if (event.target.id === 'search') { state.filter.text = event.target.value; update(); }
  };

  // Keyboard shortcuts: "/" focuses the search, "n" opens the "new forwarder" dialog.
  const handleShortcut = event => {
    if (modalOverlay || event.target.matches('input, textarea, select')) return;
    if (event.key === '/') { event.preventDefault(); $('#search').focus(); }
    if (event.key === 'n') { event.preventDefault(); openForwarderEditor(); }
  };
  document.addEventListener('keydown', handleShortcut);

  update();
  return {
    update,
    redraw: () => { tableSignature = ''; update(); },
    destroy: () => document.removeEventListener('keydown', handleShortcut),
  };
}

/* ================= forwarder editor (create / edit dialog) ================= */

function newForwarderTemplate() {
  // 10110 is the common port for NMEA over IP.
  return {
    name: '', tags: [], framing: 'lines', idle_alert: 0,
    input: { mode: 'tcp-listen', host: '', port: 10110 },
    outputs: [{ mode: 'tcp-listen', host: '', port: 10111 }],
  };
}

// Loads the IP addresses of the Delta machine once (GET /api/interfaces).
// Network interfaces rarely change, so the list is kept until the page reloads.
async function loadLocalAddresses() {
  if (!state.localAddresses) state.localAddresses = await apiRequest('GET', '/api/interfaces');
  return state.localAddresses;
}

// Dropdown for the bind address of a listening endpoint: "all interfaces" or
// one of the machine's IP addresses. An address from the config that this
// machine does not have (any more) stays selectable, marked as not found.
function bindAddressSelect(endpoint, path) {
  const currentHost = endpoint.host || '';
  const addresses = state.localAddresses || [];
  const options = [['', 'All interfaces']];
  for (const local of addresses) {
    const note = local.loopback ? 'only this machine' : local.interface;
    options.push([local.address, `${local.address} — ${note}`]);
  }
  if (currentHost && !addresses.some(local => local.address === currentHost)) {
    options.push([currentHost, state.localAddresses ? `${currentHost} — not found on this machine` : currentHost]);
  }
  return `<select data-path="${path}.host">${options.map(([value, label]) =>
    `<option value="${escapeHtml(value)}" ${value === currentHost ? 'selected' : ''}>${escapeHtml(label)}</option>`).join('')}</select>`;
}

// Form fields of one endpoint. Every input has data-path, the location of its
// value in the draft object (e.g. "outputs.0.port"); see openForwarderEditor.
function endpointFormFields(endpoint, path, direction) {
  const isListen = isListenMode(endpoint.mode);
  const modeOptions = ENDPOINT_MODES[direction]
    .map(([value, label]) => `<option value="${value}" ${endpoint.mode === value ? 'selected' : ''}>${label}</option>`).join('');

  let modeSpecificFields = '';
  if (endpoint.mode === 'tcp-listen') {
    modeSpecificFields += `<label class="f" title="Further connections are rejected when this many clients are connected. Empty = no limit">Max clients<input type="number" min="0" data-path="${path}.max_clients" value="${endpoint.max_clients || ''}" placeholder="unlimited"></label>`;
  }
  if (direction === 'output') {
    const defaultQueue = endpoint.mode === 'tcp-listen' ? '1024 per client' : '10000';
    modeSpecificFields += `<label class="f" title="How many messages may wait for a slow or disconnected receiver. When full, the oldest are dropped">Queue (messages)<input type="number" min="0" data-path="${path}.queue" value="${endpoint.queue || ''}" placeholder="${defaultQueue}"></label>`;
  }
  // tcp-connect: more remote hosts besides the one above, one connection each.
  let additionalHostsHtml = '';
  if (endpoint.mode === 'tcp-connect') {
    const hint = direction === 'input'
      ? 'Delta connects to every host and merges what they send'
      : 'Delta connects to every host and sends each one a copy (each with its own buffer)';
    const rows = (endpoint.targets || []).map((target, index) => `
      <div class="target-row">
        <label class="f" title="Hostname or IP address of another remote device">Host ${index + 2}<input data-path="${path}.targets.${index}.host" value="${escapeHtml(target.host || '')}" placeholder="hostname or IP" required></label>
        <label class="f" title="Port on that device">Port<input type="number" min="1" max="65535" data-path="${path}.targets.${index}.port" value="${target.port || ''}" required></label>
        <button type="button" class="btn icon sm ghost" data-remove-target="${path}" data-target-index="${index}" title="Remove this host">${icon('trash')}</button>
      </div>`).join('');
    additionalHostsHtml = `${rows}
      <div class="row"><button type="button" class="btn sm" data-add-target="${path}" title="Add another remote host. ${hint}">${icon('plus')}Add host</button>
        <span class="hint muted" style="font-size:12px">${hint}.</span></div>`;
  }

  return `
    <label class="f" title="${escapeHtml(MODE_HINTS[endpoint.mode] || '')}">Mode<select data-path="${path}.mode" data-rerender>${modeOptions}</select></label>
    <div class="grid3">
      ${isListen
        ? `<label class="f" title="Local IP address to listen on. “All interfaces” accepts connections from every network; an address of one interface accepts only connections that arrive there; 127.0.0.1 accepts only programs on the Delta machine">Bind address${bindAddressSelect(endpoint, path)}</label>`
        : `<label class="f" title="Hostname or IP address of the remote device">${endpoint.targets?.length ? 'Host 1' : 'Host'}<input data-path="${path}.host" value="${escapeHtml(endpoint.host || '')}" placeholder="hostname or IP" required></label>`}
      <label class="f" title="${isListen ? 'Local port. Other machines can only connect if the firewall allows it' : 'Port on the remote device'}">Port<input type="number" min="1" max="65535" data-path="${path}.port" value="${endpoint.port || ''}" required></label>
      ${direction === 'output' ? `<label class="f" title="Optional name for this output, shown in the overview and in events">Label<input data-path="${path}.name" value="${escapeHtml(endpoint.name || '')}" placeholder="optional"></label>` : '<span></span>'}
    </div>
    ${additionalHostsHtml}
    ${modeSpecificFields ? `<div class="grid2">${modeSpecificFields}</div>` : ''}`;
}

// Opens the editor. Without argument (or with an object without id) it
// creates a new forwarder; otherwise it edits the given one.
function openForwarderEditor(existing) {
  const isNew = !existing?.id;
  // The form edits this copy; it is sent to the server when saving.
  const draft = structuredClone(existing ? configOf(existing) : newForwarderTemplate());
  if (isNew) delete draft.id;
  draft.tags = draft.tags || [];
  const isRunning = !isNew && state.forwardersById.get(existing.id)?.state === 'running';

  const overlay = openModal(`
    <form novalidate>
      <div class="modal-h"><h2>${isNew ? 'New forwarder' : 'Edit ' + escapeHtml(existing.name)}</h2></div>
      <div class="modal-b">
        <div class="sect">
          <div class="grid2">
            <label class="f" title="A name you recognize, e.g. where the data comes from">Name<input data-path="name" value="${escapeHtml(draft.name)}" required placeholder="e.g. GPS bridge deck"></label>
            <label class="f" title="Optional keywords for filtering, e.g. gps, ais, bridge">Tags<input data-path="tags" value="${escapeHtml(draft.tags.join(', '))}" placeholder="comma separated"></label>
          </div>
          <div class="grid2">
            <label class="f">Idle alert (s)<input type="number" min="0" data-path="idle_alert" value="${draft.idle_alert || ''}" placeholder="off"><span class="hint">Alert when no data arrives for this long</span></label>
          </div>
          <label class="f" title="Free-text notes for you and your colleagues, e.g. where the device is, who to call, or why the forwarder exists. Shown on the detail page">Comment<textarea data-path="comment" rows="2" maxlength="4000" placeholder="optional notes">${escapeHtml(draft.comment || '')}</textarea></label>
          <label class="f">Data format
            <div class="seg" id="framing-choice">
              <button type="button" data-framing="lines" class="${draft.framing === 'lines' ? 'on' : ''}" title="Recommended for NMEA: complete sentences are forwarded, never half a sentence">NMEA / text lines</button>
              <button type="button" data-framing="raw" class="${draft.framing !== 'lines' ? 'on' : ''}" title="For binary data or bit streams: bytes are passed on exactly as they arrive, as fast as possible">Raw byte stream</button>
            </div>
            <span class="hint" id="framing-hint"></span>
          </label>
        </div>
        <div class="sect"><div class="sect-t">${icon('arrowIn')}Input</div><div class="epbox" id="input-fields"></div></div>
        <div class="sect">
          <div class="sect-t">${icon('arrowOut')}Outputs <span class="grow"></span><button type="button" class="btn sm" data-add-output>${icon('plus')}Add output</button></div>
          <div class="stack" id="output-fields"></div>
        </div>
      </div>
      <div class="modal-f">
        <span class="form-err" id="form-error"></span>
        <button type="button" class="btn" data-cancel>Cancel</button>
        ${isNew
          ? `<button type="submit" class="btn" data-submit="create" title="Save the forwarder without starting it">Create</button>
             <button type="submit" class="btn primary" data-submit="create-and-start">${icon('play')}Create &amp; start</button>`
          : `<button type="submit" class="btn primary" data-submit="save">${isRunning ? 'Save &amp; restart' : 'Save'}</button>`}
      </div>
    </form>`);

  function renderFramingHint() {
    $('#framing-hint', overlay).textContent = draft.framing === 'lines'
      ? 'Data is split at line ends — every message (e.g. $GPRMC…*hh) is forwarded whole.'
      : 'Bytes are forwarded exactly as they arrive, with no framing.';
  }
  // Re-rendered when the mode of an endpoint changes (different fields) or outputs are added/removed.
  function renderEndpointFields() {
    $('#input-fields', overlay).innerHTML = endpointFormFields(draft.input, 'input', 'input');
    $('#output-fields', overlay).innerHTML = draft.outputs.map((output, index) => `
      <div class="epbox">
        <div class="row"><b style="font-size:13px">Output ${index + 1}</b><span class="grow"></span>
          ${draft.outputs.length > 1 ? `<button type="button" class="btn icon sm ghost" data-remove-output="${index}" title="Remove">${icon('trash')}</button>` : ''}</div>
        ${endpointFormFields(output, 'outputs.' + index, 'output')}
      </div>`).join('');
  }
  renderFramingHint();
  renderEndpointFields();
  // The bind address dropdown needs the machine's addresses; draw the fields again once they are loaded.
  if (!state.localAddresses) loadLocalAddresses().then(() => { if (overlay.isConnected) renderEndpointFields(); }).catch(showError);

  // Writes value into the draft at a dotted path like "outputs.0.port".
  function setDraftValue(path, value) {
    const keys = path.split('.');
    let target = draft;
    for (const key of keys.slice(0, -1)) target = target[key];
    target[keys[keys.length - 1]] = value;
  }

  // Keep the draft in sync with every form field.
  overlay.addEventListener('input', event => {
    const field = event.target;
    const path = field.dataset.path;
    if (!path) return;
    let value = field.value;
    if (path === 'tags') value = value.split(',').map(tag => tag.trim()).filter(Boolean);
    else if (field.type === 'number') value = value === '' ? 0 : Number(value);
    if (path.endsWith('.mode')) {
      // A remote host is no bind address (and the other way round): clear it when
      // the endpoint switches between a listen and a connect/send mode.
      const endpoint = path === 'input.mode' ? draft.input : draft.outputs[Number(path.split('.')[1])];
      if (isListenMode(endpoint.mode) !== isListenMode(value)) endpoint.host = '';
    }
    setDraftValue(path, value);
    if (field.dataset.rerender !== undefined) renderEndpointFields();
  });
  overlay.addEventListener('click', event => {
    const target = event.target;
    if (target.closest('[data-cancel]')) return closeModal();
    const framingButton = target.closest('[data-framing]');
    if (framingButton) {
      draft.framing = framingButton.dataset.framing;
      $$('#framing-choice button', overlay).forEach(button => button.classList.toggle('on', button === framingButton));
      renderFramingHint();
    }
    if (target.closest('[data-add-output]')) {
      const lastOutput = draft.outputs[draft.outputs.length - 1];
      draft.outputs.push({ mode: 'udp-send', host: '', port: (lastOutput?.port || 10110) + 1 });
      renderEndpointFields();
    }
    // Additional hosts of a tcp-connect endpoint. data-*-target holds the
    // endpoint's path: "input" or "outputs.<index>".
    const endpointAtPath = path => (path === 'input' ? draft.input : draft.outputs[Number(path.split('.')[1])]);
    const addTargetButton = target.closest('[data-add-target]');
    if (addTargetButton) {
      const endpoint = endpointAtPath(addTargetButton.dataset.addTarget);
      endpoint.targets = [...(endpoint.targets || []), { host: '', port: endpoint.port || 10110 }];
      renderEndpointFields();
      $(`[data-path="${addTargetButton.dataset.addTarget}.targets.${endpoint.targets.length - 1}.host"]`, overlay)?.focus();
    }
    const removeTargetButton = target.closest('[data-remove-target]');
    if (removeTargetButton) {
      endpointAtPath(removeTargetButton.dataset.removeTarget).targets.splice(Number(removeTargetButton.dataset.targetIndex), 1);
      renderEndpointFields();
    }
    const removeButton = target.closest('[data-remove-output]');
    if (removeButton) {
      draft.outputs.splice(Number(removeButton.dataset.removeOutput), 1);
      renderEndpointFields();
    }
  });

  // The server validates the draft; its error message is shown in the dialog.
  $('form', overlay).onsubmit = async event => {
    event.preventDefault();
    const submitMode = event.submitter?.dataset.submit;
    const buttons = $$('.modal-f button', overlay);
    buttons.forEach(button => (button.disabled = true));
    $('#form-error', overlay).textContent = '';
    try {
      const saved = isNew
        ? await apiRequest('POST', '/api/forwarders' + (submitMode === 'create-and-start' ? '?start=1' : ''), draft)
        : await apiRequest('PUT', '/api/forwarders/' + existing.id, draft);
      receiveForwarder(saved); // show it immediately, before the next live update
      closeModal();
      if (saved.state === 'error') showToast(`Saved, but could not start: ${saved.error}`, 'error');
      else showToast(isNew ? `Created ${saved.name}` : `Saved ${saved.name}`);
      if (isNew) location.hash = '#/f/' + saved.id;
    } catch (error) {
      $('#form-error', overlay).textContent = error.message;
      buttons.forEach(button => (button.disabled = false));
    }
  };
}

/* ================= page: forwarder detail ================= */

function renderDetailPage(main, forwarderId) {
  main.innerHTML = `
    <a class="crumb" href="#/">${icon('back')}Forwarders</a>
    <div id="detail-content"></div>`;
  const content = $('#detail-content', main);
  let isRendered = false; // the page skeleton is built once the forwarder is known
  let activeTab = null;

  const TABS = [
    ['traffic', 'Traffic', renderTrafficTab, 'Throughput chart of the last 10 minutes'],
    ['inspect', 'Inspector', renderInspectorTab, 'Watch the data passing through, live'],
    ['clients', 'Connections', renderConnectionsTab, 'Every connected client and remote host; disconnect single clients'],
    ['events', 'Events', renderForwarderEventsTab, 'Log of this forwarder: connects, disconnects, errors, alerts'],
    ['captures', 'Captures', renderForwarderCapturesTab, 'Record the received data to a file and replay it later'],
    ['config', 'Configuration', renderConfigTab, 'Settings of this forwarder as summary and JSON'],
  ];

  function renderSkeleton() {
    content.innerHTML = `
      <div class="page-head">
        <span class="dot" id="health-dot"></span>
        <div class="grow" style="min-width:0"><h1 id="detail-title"></h1><div class="sub" id="detail-subtitle"></div></div>
        <div class="row" id="detail-actions"></div>
      </div>
      <div id="banners"></div>
      <div id="detail-comment"></div>
      <div class="flow"><div id="flow-input"></div><div class="wire" id="flow-wires"></div><div class="outs" id="flow-outputs"></div></div>
      <div class="tiles" id="detail-tiles"></div>
      <div class="tabs" id="tabs">${TABS.map(([key, label, , hint]) => `<button data-tab="${key}" title="${hint}">${label}</button>`).join('')}</div>
      <div id="tab-content"></div>`;
    isRendered = true;
    showTab(preferences.get('tab', 'traffic'));
  }

  function showTab(tabKey) {
    const tab = TABS.find(([key]) => key === tabKey) || TABS[0];
    activeTab?.destroy?.();
    preferences.set('tab', tab[0]);
    $$('#tabs button', content).forEach(button => button.classList.toggle('on', button.dataset.tab === tab[0]));
    const tabContent = $('#tab-content', content);
    tabContent.innerHTML = '';
    tabContent.onclick = null;
    activeTab = tab[2](tabContent, forwarderId);
  }

  // One box of the flow diagram (input on the left, outputs on the right).
  function flowNodeHtml(endpoint, status, title, fwd, isInput) {
    const isRunning = fwd.state === 'running';
    const isHealthy = status && HEALTHY_STATES.has(status.state);
    const borderClass = !isRunning || !status ? 'off' : isHealthy ? 'ok' : 'bad';
    const statePill = isRunning && status ? `<span class="pill ${isHealthy ? 'ok' : 'warn'}" title="${escapeHtml(STATE_HINTS[status.state] || '')}">${escapeHtml(status.state)}</span>` : '';
    const counters = isRunning && status ? `<div class="meta">
        <span><b>${formatBytes(status.bytes)}</b> ${isInput ? 'received' : 'sent'}</span>
        <span><b>${status.peer_count}</b> ${peerNoun(endpoint.mode)}</span>
        ${status.drops ? `<span style="color:var(--warn)"><b>${formatCount(status.drops)}</b> dropped</span>` : ''}
        ${status.reconnects ? `<span><b>${status.reconnects}</b> reconnects</span>` : ''}
        ${status.queued ? `<span style="color:var(--warn)"><b>${formatCount(status.queued)}</b> buffered</span>` : ''}</div>` : '';
    // Several remote hosts: one line per host with its own state (and error) instead of one address.
    const hasSeveralHosts = (endpoint.targets?.length || 0) > 0;
    let addressHtml = `<div class="addr">${escapeHtml(endpointAddress(endpoint))}</div>`;
    if (hasSeveralHosts) {
      const connectionByAddress = new Map((status?.connections || []).map(connection => [connection.addr, connection]));
      addressHtml = `<div class="host-list">${allEndpointAddresses(endpoint).map(address => {
        const connection = isRunning ? connectionByAddress.get(address) : null;
        const isConnected = connection?.state === 'connected';
        const dot = connection ? `<span class="dotc" style="background:var(--${isConnected ? 'ok' : 'warn'})"></span>` : '<span class="dotc" style="background:var(--off)"></span>';
        const hint = connection ? `${connection.state}${connection.error ? ': ' + connection.error : ''}${connection.queued ? ` · ${connection.queued} messages buffered` : ''}` : 'not running';
        return `<div class="host-line" title="${escapeHtml(hint)}">${dot}<span class="addr">${escapeHtml(address)}</span>${connection && !isConnected ? `<span class="host-state">${escapeHtml(connection.state)}</span>` : ''}</div>`;
      }).join('')}</div>`;
    }
    const errorLine = isRunning && status?.error && !isHealthy ? `<div class="err">${escapeHtml(status.error)}</div>` : '';
    return `<div class="node ${borderClass}">
      <div class="top"><span class="ep"><span class="m" title="${escapeHtml(MODE_HINTS[endpoint.mode] || '')}">${MODE_LABELS[endpoint.mode]}</span></span><span class="ttl">${escapeHtml(endpoint.name || title)}</span><span class="grow"></span>${statePill}</div>
      ${addressHtml}
      ${counters}${errorLine}
    </div>`;
  }

  // Draws curved lines from the input box to every output box. They are
  // animated (CSS) while data is flowing.
  function drawFlowWires(isFlowing) {
    const wireArea = $('#flow-wires', content);
    const inputNode = $('#flow-input .node', content);
    const outputNodes = $$('#flow-outputs .node', content);
    if (!wireArea || !inputNode) return;
    wireArea.classList.toggle('on', isFlowing);
    const area = wireArea.getBoundingClientRect();
    if (area.width < 30) { wireArea.innerHTML = ''; return; } // stacked layout on phones: no wires
    const inputBox = inputNode.getBoundingClientRect();
    const startY = inputBox.top + inputBox.height / 2 - area.top;
    const width = area.width;
    const paths = outputNodes.map(node => {
      const box = node.getBoundingClientRect();
      const endY = box.top + box.height / 2 - area.top;
      return `<path d="M0 ${startY} C ${width / 2} ${startY}, ${width / 2} ${endY}, ${width} ${endY}"/>`;
    }).join('');
    wireArea.innerHTML = `<svg>${paths}</svg>`;
  }

  function update() {
    const fwd = state.forwardersById.get(forwarderId);
    if (!fwd) {
      // Unknown id: either deleted, or not yet received (just created).
      if (isRendered) { location.hash = '#/'; return; }
      content.innerHTML = `<div class="empty"><h2>Forwarder not found</h2><p>It may have been deleted.</p></div>`;
      return;
    }
    if (!isRendered) renderSkeleton();
    const isRunning = fwd.state === 'running';
    const isFlowing = isRunning && fwd.rate.in > 0;

    $('#health-dot', content).className = `dot ${healthClass(fwd.health)} ${fwd.health === 'ok' ? 'pulse' : ''}`;
    setHtmlIfChanged($('#detail-title', content),
      `${escapeHtml(fwd.name)} <span class="pill ${healthClass(fwd.health)}" style="vertical-align:3px;margin-left:6px">${HEALTH_LABELS[fwd.health]}</span>`);
    setHtmlIfChanged($('#detail-subtitle', content), [
      fwd.tags.map(tag => `<span class="tag">${escapeHtml(tag)}</span>`).join(' '),
      fwd.framing === 'lines' ? 'NMEA / line framing' : 'Raw stream',
      isRunning ? `running for ${formatDuration(Date.now() - new Date(fwd.since))}` : `${fwd.state} since ${formatDateTime(fwd.since)}`,
    ].filter(Boolean).join(' · '));
    setHtmlIfChanged($('#detail-actions', content), `
      ${isRunning || fwd.state === 'error'
        ? `<button class="btn stop" data-action="stop" title="Stop forwarding. It stays stopped after a restart of Delta">${icon('stop')}Stop</button>`
        : `<button class="btn go" data-action="start" title="Start forwarding. It also starts automatically with Delta">${icon('play')}Start</button>`}
      <button class="btn" data-action="restart" ${isRunning ? '' : 'disabled'} title="Stop and start again: all connections are re-established">${icon('restart')}Restart</button>
      <button class="btn" data-action="edit" title="Change the configuration. A running forwarder restarts">${icon('edit')}Edit</button>
      <button class="btn icon" data-action="menu" title="Duplicate, export or delete">${icon('more')}</button>`);

    let bannersHtml = '';
    if (fwd.state === 'error') {
      bannersHtml += `<div class="banner error">${icon('alert')}<span><b>Start failed:</b> ${escapeHtml(fwd.error)}${fwd.autostart ? ' — retrying every 15 s.' : ''}</span></div>`;
    }
    const problems = endpointProblems(fwd);
    if (problems.length) {
      const lines = problems.map(problem => {
        const buffered = problem.queued ? ` · ${formatCount(problem.queued)} messages buffered` : '';
        return `<div><b>${escapeHtml(problem.label)}</b> <span class="mono">${escapeHtml(problem.address)}</span> is ${escapeHtml(problem.state)}: ${escapeHtml(problem.reason)}${buffered}</div>`;
      }).join('');
      bannersHtml += `<div class="banner warn">${icon('alert')}<div class="grow">${lines}
        <div style="opacity:.8;margin-top:2px">Delta keeps retrying automatically; buffered data is delivered once the connection works.</div></div></div>`;
    }
    for (const alertKind of fwd.firing) {
      const reason = fwd.alert_reasons?.[alertKind];
      bannersHtml += `<div class="banner error">${icon('bell')}<span>Alert <b>${escapeHtml(alertKind)}</b> is firing${reason ? ': ' + escapeHtml(reason) : ''}</span></div>`;
    }
    if (fwd.capture) {
      bannersHtml += `<div class="banner info">${icon('record')}<span class="grow">Recording <b>${escapeHtml(fwd.capture.name)}</b> — ${formatCount(fwd.capture.records)} messages, ${formatBytes(fwd.capture.bytes)}</span><button class="btn sm" data-action="stop-capture">${icon('stop')}Stop</button></div>`;
    }
    if (fwd.replay) {
      bannersHtml += `<div class="banner info">${icon('play')}<span class="grow">Replaying <b>${escapeHtml(fwd.replay.capture)}</b>${fwd.replay.loop ? ' (loop)' : ''} — ${formatCount(fwd.replay.sent)} messages sent</span><button class="btn sm" data-action="stop-replay">${icon('stop')}Stop</button></div>`;
    }
    setHtmlIfChanged($('#banners', content), bannersHtml);
    setHtmlIfChanged($('#detail-comment', content), fwd.comment
      ? `<div class="comment-box" title="Comment of this forwarder. Change it with Edit">${icon('note')}<div class="comment-text">${escapeHtml(fwd.comment)}</div></div>` : '');

    const inputChanged = setHtmlIfChanged($('#flow-input', content), flowNodeHtml(fwd.input, fwd.in_status, 'Input', fwd, true));
    const outputsChanged = setHtmlIfChanged($('#flow-outputs', content),
      fwd.outputs.map((out, i) => flowNodeHtml(out, fwd.out_status?.[i], 'Output ' + (i + 1), fwd, false)).join(''));
    if (inputChanged || outputsChanged || !$('#flow-wires svg', content)) drawFlowWires(isFlowing);
    else $('#flow-wires', content).classList.toggle('on', isFlowing);

    const dash = '—';
    setHtmlIfChanged($('#detail-tiles', content), `
      <div class="tile" title="Bytes per second received by the input"><div class="k">${icon('arrowIn')}Inbound</div><div class="v">${isRunning ? formatRate(fwd.rate.in) : dash}</div></div>
      <div class="tile" title="Bytes per second sent by all outputs together (each output and each client gets its own copy)"><div class="k">${icon('arrowOut')}Outbound</div><div class="v">${isRunning ? formatRate(fwd.rate.out) : dash}</div></div>
      <div class="tile" title="Messages received per second (with line framing: NMEA sentences)"><div class="k">${icon('activity')}Messages</div><div class="v">${isRunning ? formatCount(Math.round(fwd.rate.msgs)) : dash}<small>/s</small></div></div>
      <div class="tile" title="Sources connected to the input: TCP clients, active UDP senders or the remote host Delta connected to. Details on the Connections tab"><div class="k">${icon('arrowIn')}Clients in</div><div class="v">${isRunning ? fwd.clients_in : dash}</div></div>
      <div class="tile" title="Receivers connected to the outputs: TCP clients and targets Delta connected to. Details on the Connections tab"><div class="k">${icon('arrowOut')}Clients out</div><div class="v">${isRunning ? fwd.clients_out : dash}</div></div>
      <div class="tile ${fwd.drops ? 'bad' : ''}" title="Messages thrown away since the start because a receiver was too slow or a buffer was full"><div class="k">${icon('alert')}Dropped</div><div class="v">${isRunning ? formatCount(fwd.drops) : dash}</div></div>
      <div class="tile" title="When the input last received data"><div class="k">${icon('info')}Last data</div><div class="v" style="font-size:16px;padding-top:5px">${isRunning && fwd.last_data ? timeAgo(fwd.last_data) : dash}</div></div>`);

    activeTab?.update?.(fwd);
  }

  // Handles the header buttons, banners and tab bar. Tabs handle their own content clicks.
  main.onclick = async event => {
    const tabButton = event.target.closest('[data-tab]');
    if (tabButton) return showTab(tabButton.dataset.tab);
    const button = event.target.closest('[data-action]');
    if (!button || !button.closest('.page-head, #banners')) return;
    const fwd = state.forwardersById.get(forwarderId);
    switch (button.dataset.action) {
      case 'start': case 'stop': case 'restart': return setForwardersRunning([forwarderId], button.dataset.action);
      case 'edit': return openForwarderEditor(fwd);
      case 'menu': return openForwarderMenu(button, fwd, () => { location.hash = '#/'; });
      case 'stop-capture': return apiRequest('POST', `/api/forwarders/${forwarderId}/capture-stop`).catch(showError);
      case 'stop-replay': return apiRequest('POST', `/api/forwarders/${forwarderId}/replay-stop`).catch(showError);
    }
  };
  const redrawWiresOnResize = () => {
    const fwd = state.forwardersById.get(forwarderId);
    drawFlowWires(fwd?.state === 'running' && fwd.rate.in > 0);
  };
  window.addEventListener('resize', redrawWiresOnResize);

  update();
  return {
    update,
    redraw: () => activeTab?.redraw?.(),
    onEvent: event => activeTab?.onEvent?.(event),
    destroy: () => { activeTab?.destroy?.(); window.removeEventListener('resize', redrawWiresOnResize); },
  };
}

/* ---------- detail tab: traffic chart ---------- */

const CHART_HEIGHT = 260; // pixels, axes included

function renderTrafficTab(container, forwarderId) {
  let metric = preferences.get('metric', 'bytes'); // "bytes" or "msgs"
  // Parallel arrays, oldest first: loaded from the server, then extended by the live updates.
  const history = { times: [], bytesIn: [], bytesOut: [], messages: [] };
  let chart = null; // the uPlot instance; rebuilt when the metric or the theme changes

  container.innerHTML = `<div class="card">
    <div class="card-h">
      <h2>Throughput</h2>
      <span class="grow"></span>
      <div class="seg" id="metric-choice"><button data-metric="bytes" title="Data volume per second, received (In) and sent (Out)">Bytes/s</button><button data-metric="msgs" title="Received messages per second">Messages/s</button></div>
    </div>
    <div class="card-b"><div class="chart-box" id="chart"></div></div>
  </div>`;
  const chartBox = $('#chart', container);

  // uPlot takes columns: the x values (unix seconds) and one array per line.
  const chartData = () => [history.times.map(time => time / 1000),
    ...(metric === 'msgs' ? [history.messages] : [history.bytesIn, history.bytesOut])];

  // Builds the chart. Colors and fonts come from the CSS tokens, so it is
  // built again when the theme changes (redraw).
  function buildChart() {
    chart?.destroy();
    $$('#metric-choice button', container).forEach(button => button.classList.toggle('on', button.dataset.metric === metric));
    const formatValue = metric === 'msgs' ? value => formatCount(Math.round(value)) + '/s' : formatRate;
    const line = (label, colorToken) => {
      const color = cssVariable(colorToken);
      return { label, stroke: color, width: 1.8, fill: color + '22', points: { show: false },
        value: (_, value) => (value == null ? '–' : formatValue(value)) }; // shown in the legend at the cursor
    };
    const axis = { stroke: cssVariable('--faint'), font: '11px ' + cssVariable('--sans'), ticks: { show: false },
      grid: { stroke: cssVariable('--border'), width: 1 } };
    chart = new uPlot({
      width: chartBox.clientWidth, height: CHART_HEIGHT, // the legend with the values at the cursor goes below
      series: [
        { label: 'Time', value: (_, seconds) => (seconds == null ? '–' : formatClockTime(seconds * 1000).slice(0, 8)) },
        ...(metric === 'msgs' ? [line('Messages in', '--accent')] : [line('In', '--accent'), line('Out', '--out')]),
      ],
      axes: [
        // Time axis: whole-second steps (the data has one point per second), labels in the 24 h clock of the rest of the UI.
        { ...axis, space: 70, incrs: [1, 2, 5, 10, 15, 30, 60, 120, 300, 600],
          values: (_, ticks) => ticks.map(seconds => formatClockTime(seconds * 1000).slice(0, 8)) },
        { ...axis, size: 72, values: (_, ticks) => ticks.map(formatValue) },
      ],
      scales: { y: { range: (_, min, max) => [0, max > 0 ? max * 1.1 : 1] } }, // always start at 0
      cursor: { y: false, drag: { x: false, y: false } }, // a vertical line only; no zooming: the chart follows the live data
    }, chartData(), chartBox);
  }

  $('#metric-choice', container).onclick = event => {
    const chosen = event.target.closest('[data-metric]')?.dataset.metric;
    if (chosen) { metric = chosen; preferences.set('metric', chosen); buildChart(); }
  };

  buildChart();
  apiRequest('GET', `/api/forwarders/${forwarderId}/history`).then(loaded => {
    history.times.unshift(...loaded.t);
    history.bytesIn.unshift(...loaded.in);
    history.bytesOut.unshift(...loaded.out);
    history.messages.unshift(...loaded.msgs);
    chart.setData(chartData());
  }).catch(showError);
  const resizeObserver = new ResizeObserver(() => chart.setSize({ width: chartBox.clientWidth, height: CHART_HEIGHT }));
  resizeObserver.observe(chartBox);

  return {
    // Append one point per second from the live updates (max. 10 minutes).
    update(fwd) {
      const now = Date.now();
      if (history.times.length && now - history.times[history.times.length - 1] < 900) return;
      history.times.push(now);
      history.bytesIn.push(fwd.rate.in);
      history.bytesOut.push(fwd.rate.out);
      history.messages.push(fwd.rate.msgs);
      if (history.times.length > 600) {
        for (const values of Object.values(history)) values.shift();
      }
      chart.setData(chartData());
    },
    redraw: buildChart,
    destroy: () => { resizeObserver.disconnect(); chart.destroy(); },
  };
}

/* ---------- message views (live inspector and capture viewer) ---------- */

// Bytes -> string with one character per byte (Latin-1). Used for filtering
// and NMEA parsing; character i is always byte i.
const bytesToBinaryString = bytes => {
  let text = '';
  for (let i = 0; i < bytes.length; i++) text += String.fromCharCode(bytes[i]);
  return text;
};
// Visible symbols for common control characters.
const CONTROL_CHAR_SYMBOLS = { 0: '␀', 9: '␉', 10: '␊', 13: '␍' };
const toHex2 = byte => byte.toString(16).padStart(2, '0');

// How much of a long message is drawn until the user expands it. Drawing
// a 16 KB bit-stream chunk in full would make the list slow.
const COLLAPSED_TEXT_CHARS = 400;
const COLLAPSED_HEX_BYTES = 64;

// Length of bytes without trailing line-ending characters (\r and \n).
function lengthWithoutLineEnding(bytes) {
  let end = bytes.length;
  while (end > 0 && (bytes[end - 1] === 10 || bytes[end - 1] === 13)) end--;
  return end;
}

// Parses the filter input: "/…/flags" is a regular expression, any other
// text a case-insensitive substring. Returns null for an empty filter and
// throws for an invalid regular expression.
function parseFilterInput(input) {
  if (!input) return null;
  const regexMatch = input.match(/^\/(.+)\/([a-z]*)$/);
  if (regexMatch) return new RegExp(regexMatch[1], regexMatch[2].replace('g', ''));
  return new RegExp(input.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'i');
}

// Marks which characters of `text` the filter matches (1 = highlight), or
// returns null if nothing matches. Index i corresponds to byte i of the message.
function filterHighlightMask(text, filterRegex) {
  if (!filterRegex) return null;
  const everyMatch = new RegExp(filterRegex.source, filterRegex.flags + 'g');
  let mask = null;
  for (const match of text.matchAll(everyMatch)) {
    if (!match[0].length) continue; // e.g. /x*/ also matches empty strings
    mask ??= new Uint8Array(text.length);
    mask.fill(1, match.index, match.index + match[0].length);
  }
  return mask;
}

// Builds HTML from pieces that belong to message positions: `pieces` are
// [position, html]; highlighted positions are wrapped in <mark>.
function joinWithHighlights(pieces, mask) {
  if (!mask) return pieces.map(([, html]) => html).join('');
  let result = '', markOpen = false;
  for (const [position, html] of pieces) {
    const marked = mask[position] === 1;
    if (marked !== markOpen) { result += marked ? '<mark>' : '</mark>'; markOpen = marked; }
    result += html;
  }
  return markOpen ? result + '</mark>' : result;
}

// Text view: printable ASCII as is, other bytes as a dim symbol or \xHH.
// The line ending at the very end of a message is not shown (every NMEA
// sentence has one); line breaks or control bytes inside a message are.
function renderAsText(bytes, mask, maxChars = Infinity) {
  const end = Math.min(lengthWithoutLineEnding(bytes), maxChars);
  const pieces = [];
  for (let i = 0; i < end; i++) {
    const byte = bytes[i];
    pieces.push([i, byte >= 32 && byte < 127
      ? escapeHtml(String.fromCharCode(byte))
      : `<span class="cc">${CONTROL_CHAR_SYMBOLS[byte] || '\\x' + toHex2(byte)}</span>`]);
  }
  return joinWithHighlights(pieces, mask);
}

// Hex view: classic hex dump, 16 bytes per line (offset, hex, ASCII).
function renderAsHex(bytes, mask, maxBytes = Infinity) {
  const end = Math.min(bytes.length, maxBytes);
  let html = '';
  for (let lineStart = 0; lineStart < end; lineStart += 16) {
    const lineEnd = Math.min(lineStart + 16, end);
    const hexPieces = [], asciiPieces = [];
    for (let i = lineStart; i < lineEnd; i++) {
      hexPieces.push([i, toHex2(bytes[i]) + (i < lineEnd - 1 ? ' ' : '')]);
      asciiPieces.push([i, bytes[i] >= 32 && bytes[i] < 127 ? escapeHtml(String.fromCharCode(bytes[i])) : '.']);
    }
    const padding = ' '.repeat((16 - (lineEnd - lineStart)) * 3);
    html += `<span class="o">${lineStart.toString(16).padStart(4, '0')}</span>  ${joinWithHighlights(hexPieces, mask)}${padding}  <span class="s">${joinWithHighlights(asciiPieces, mask)}</span>\n`;
  }
  return `<div class="hex">${html}</div>`;
}

// NMEA view: "$GPRMC,…*6A" is shown as talker (GP), sentence type (RMC),
// the fields, and the checksum with a ✓ or ✗. Other data falls back to text.
function renderAsNmea(bytes, mask, maxChars = Infinity) {
  const sentence = bytesToBinaryString(bytes.subarray(0, lengthWithoutLineEnding(bytes)));
  if (!/^[$!]/.test(sentence)) return renderAsText(bytes, mask, maxChars);
  // One colored span per part; `part` keeps message positions for highlighting.
  const part = (start, end, cssClass) => {
    const pieces = [];
    for (let i = start; i < end; i++) pieces.push([i, escapeHtml(sentence[i])]);
    return `<span class="${cssClass}">${joinWithHighlights(pieces, mask)}</span>`;
  };

  const starIndex = sentence.lastIndexOf('*');
  const bodyEnd = starIndex > 0 ? starIndex : sentence.length; // the checksum starts at "*"
  const firstComma = sentence.indexOf(',');
  const addressEnd = firstComma > 0 && firstComma < bodyEnd ? firstComma : bodyEnd;
  const talkerEnd = sentence[1] === 'P' ? 2 : Math.min(3, addressEnd); // "$P…" is proprietary: no two-letter talker
  let html = part(0, talkerEnd, 'tk') + part(talkerEnd, addressEnd, 'ty');

  let position = addressEnd;
  while (position < bodyEnd) { // sentence[position] is a comma
    if (position >= maxChars) break; // collapsed: the rest is shown after expanding
    html += part(position, position + 1, 'sep');
    position++;
    const nextComma = sentence.indexOf(',', position);
    const fieldEnd = nextComma < 0 || nextComma > bodyEnd ? bodyEnd : nextComma;
    html += fieldEnd > position ? part(position, fieldEnd, 'fd') : '<span class="faint">∅</span>';
    position = fieldEnd;
  }

  if (starIndex > 0 && position >= bodyEnd) {
    // The NMEA checksum is the XOR of all characters between "$" and "*".
    let checksum = 0;
    for (let i = 1; i < bodyEnd; i++) checksum ^= sentence.charCodeAt(i);
    const expected = checksum.toString(16).toUpperCase().padStart(2, '0');
    const received = sentence.slice(starIndex + 1, starIndex + 3).toUpperCase();
    const isValid = received === expected;
    html += `<span class="ck ${isValid ? 'ok' : 'bad'}" title="${isValid ? 'Checksum valid' : `Checksum wrong, expected ${expected}`}">${part(starIndex, starIndex + 3, '')} ${isValid ? '✓' : '✗'}</span>`;
  }
  return `<div class="nm">${html}</div>`;
}

// Turns a message from the server ({ t, s, n, d: base64 }) into what the views use.
function decodeMessage(row) {
  const binary = atob(row.d || '');
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return { time: row.t, source: row.s, length: row.n, bytes, text: binary, expanded: false };
}

let nextMessageId = 1;

// A scrolling list of messages with Text / NMEA / Hex views, used by the live
// inspector and the capture viewer. Built for large amounts of data:
//   - new messages are drawn in batches once per animation frame,
//   - at most `maxRows` rows are in the page; older ones are removed,
//   - long messages are collapsed and can be expanded with a click,
//   - nothing is drawn while the browser tab is hidden.
// Filter matches are highlighted with <mark>.
function createMessageList(logElement, { formatTime, maxRows, maxKept, followNewest, emptyText }) {
  let messages = [];        // kept in memory, so view/filter changes can redraw them
  let waitingToDraw = [];   // added since the last frame
  let frameRequested = false;
  let viewMode = 'nmea';
  let filterRegex = null;
  let isPaused = false;
  let currentEmptyText = emptyText;

  const matchesFilter = message => !filterRegex || filterRegex.test(message.text);

  function rowHtml(message) {
    const mask = filterHighlightMask(message.text, filterRegex);
    const shownLength = Math.min(message.bytes.length, viewMode === 'hex' ? COLLAPSED_HEX_BYTES : COLLAPSED_TEXT_CHARS);
    const isCollapsed = !message.expanded && message.bytes.length > shownLength;
    const limit = isCollapsed ? shownLength : Infinity;
    const content = viewMode === 'hex' ? renderAsHex(message.bytes, mask, limit)
      : viewMode === 'nmea' ? renderAsNmea(message.bytes, mask, limit)
      : renderAsText(message.bytes, mask, limit);
    let toggle = '';
    if (isCollapsed) {
      toggle = ` <button class="expand" data-expand="${message.id}" title="Show the whole message">… ${formatCount(message.bytes.length - shownLength)} more bytes</button>`;
    } else if (message.expanded) {
      toggle = ` <button class="expand" data-expand="${message.id}" title="Show only the beginning again">show less</button>`;
    }
    const cutByServer = message.length > message.bytes.length
      ? ` <span class="cc" title="Only the first ${formatBytes(message.bytes.length)} of this ${formatBytes(message.length)} message were transferred. Raise “Preview” to see more">(first ${formatBytes(message.bytes.length)})</span>`
      : '';
    return `<div class="ir" data-message="${message.id}"><span class="tm">${formatTime(message.time)}</span><span class="sr" title="${escapeHtml(message.source)}">${escapeHtml(message.source)}</span><span class="ln" title="${message.length} bytes">${message.length}</span><span class="dt">${content}${toggle}${cutByServer}</span></div>`;
  }

  function showEmptyText() {
    logElement.innerHTML = `<div class="empty" style="padding:40px">${currentEmptyText}</div>`;
  }

  function drawAll() {
    waitingToDraw = [];
    const shown = messages.filter(matchesFilter).slice(followNewest ? -maxRows : 0, followNewest ? undefined : maxRows);
    if (!shown.length) return showEmptyText();
    logElement.innerHTML = shown.map(rowHtml).join('');
    if (followNewest) logElement.scrollTop = logElement.scrollHeight;
  }

  function drawWaiting() {
    frameRequested = false;
    if (document.hidden || isPaused) return; // drawn later by drawAll
    const batch = waitingToDraw.filter(matchesFilter).slice(-maxRows);
    waitingToDraw = [];
    if (!batch.length) return;
    const wasAtBottom = logElement.scrollTop + logElement.clientHeight >= logElement.scrollHeight - 40;
    if (logElement.querySelector('.empty')) logElement.innerHTML = '';
    logElement.insertAdjacentHTML('beforeend', batch.map(rowHtml).join(''));
    while (logElement.childElementCount > maxRows) logElement.firstElementChild.remove();
    if (followNewest && wasAtBottom) logElement.scrollTop = logElement.scrollHeight;
  }

  // Expand or collapse one message.
  logElement.addEventListener('click', event => {
    const button = event.target.closest('[data-expand]');
    if (!button) return;
    const message = messages.find(candidate => candidate.id === Number(button.dataset.expand));
    const row = button.closest('.ir');
    if (!message || !row) return;
    message.expanded = !message.expanded;
    row.outerHTML = rowHtml(message);
  });
  const redrawWhenVisible = () => { if (!document.hidden) drawAll(); };
  document.addEventListener('visibilitychange', redrawWhenVisible);

  return {
    get messages() { return messages; },
    add(newMessages) {
      for (const message of newMessages) message.id = nextMessageId++;
      messages.push(...newMessages);
      if (messages.length > maxKept) messages.splice(0, messages.length - maxKept);
      waitingToDraw.push(...newMessages);
      if (waitingToDraw.length > maxRows) waitingToDraw.splice(0, waitingToDraw.length - maxRows);
      if (!frameRequested && !document.hidden && !isPaused) {
        frameRequested = true;
        requestAnimationFrame(drawWaiting);
      }
    },
    clear() { messages = []; drawAll(); },
    setView(mode) { viewMode = mode; drawAll(); },
    setFilter(regex) { filterRegex = regex; drawAll(); },
    setPaused(paused) { isPaused = paused; if (!paused) drawAll(); },
    setEmptyText(text) { currentEmptyText = text; if (!messages.length) showEmptyText(); },
    shownMessages: () => messages.filter(matchesFilter),
    destroy() { document.removeEventListener('visibilitychange', redrawWhenVisible); },
  };
}

// Toolbar parts shared by the live inspector and the capture viewer.
const VIEW_MODE_BUTTONS = `
  <div class="seg" data-role="view-mode">
    <button data-view="text" title="Readable text; unprintable bytes are shown as dim symbols">Text</button>
    <button data-view="nmea" title="NMEA sentences split into talker, type and fields, with checksum check (✓ / ✗)">NMEA</button>
    <button data-view="hex" title="Every byte in hexadecimal, 16 per line, with ASCII next to it">Hex</button>
  </div>`;
const PREVIEW_SIZE_SELECT = selected => `
  <select data-role="preview-size" title="How much of each message is transferred to the browser. Larger values show more of long messages (bit streams) but need more bandwidth">
    ${[[256, '256 B'], [1024, '1 KB'], [4096, '4 KB'], [16384, '16 KB']].map(([value, label]) => `<option value="${value}" ${value === selected ? 'selected' : ''}>Preview ${label}</option>`).join('')}
  </select>`;
const COLUMN_RESIZE_HANDLE = column => `<span class="col-resize" data-resize-column="${column}" title="Drag to change the width of this column. Double-click to reset it"></span>`;
const MESSAGE_LIST_HEADER = timeHint => `
  <div class="ir ir-head">
    <span title="${timeHint}">Time${COLUMN_RESIZE_HANDLE('time')}</span>
    <span class="sr" title="Address of the sender (IP:port)">Source${COLUMN_RESIZE_HANDLE('source')}</span>
    <span class="ln" title="Size of the message in bytes">Bytes${COLUMN_RESIZE_HANDLE('bytes')}</span>
    <span title="Content of the message">Data</span>
  </div>`;

// Column widths of the message list (in px) that can be changed by dragging
// the handles in the header. Stored per browser and shared by the live
// inspector and the capture viewer.
const MESSAGE_COLUMN_WIDTHS = {
  // The minimum widths keep the header labels readable.
  time: { cssVariable: '--col-time', minimumWidth: 44 },
  source: { cssVariable: '--col-source', minimumWidth: 60 },
  bytes: { cssVariable: '--col-bytes', minimumWidth: 48 },
};
const MAXIMUM_COLUMN_WIDTH = 600;

// Makes the columns of a message list resizable. `cardElement` contains the
// header and the rows; the widths are CSS variables on it, so header and
// rows always line up and newly drawn rows need no extra work.
function bindColumnResizing(cardElement) {
  const applyWidth = (column, width) => {
    const { cssVariable, minimumWidth } = MESSAGE_COLUMN_WIDTHS[column];
    const clampedWidth = Math.round(Math.min(MAXIMUM_COLUMN_WIDTH, Math.max(minimumWidth, width)));
    cardElement.style.setProperty(cssVariable, clampedWidth + 'px');
    return clampedWidth;
  };
  const saveWidth = (column, width) => {
    const widths = preferences.get('insp-columns', {});
    if (width === null) delete widths[column]; else widths[column] = width;
    preferences.set('insp-columns', widths);
  };
  for (const [column, width] of Object.entries(preferences.get('insp-columns', {}))) {
    if (MESSAGE_COLUMN_WIDTHS[column] && Number.isFinite(width)) applyWidth(column, width);
  }

  $$('[data-resize-column]', cardElement).forEach(handle => {
    const column = handle.dataset.resizeColumn;
    const headerCell = handle.parentElement;
    handle.addEventListener('pointerdown', event => {
      if (event.button !== 0) return;
      event.preventDefault();
      const startX = event.clientX;
      const startWidth = headerCell.getBoundingClientRect().width;
      let currentWidth = startWidth;
      // Pointer capture keeps the move events coming while the mouse is outside the handle.
      handle.setPointerCapture(event.pointerId);
      handle.classList.add('dragging');
      document.body.classList.add('resizing-columns');
      const onMove = moveEvent => { currentWidth = applyWidth(column, startWidth + moveEvent.clientX - startX); };
      const onEnd = () => {
        handle.removeEventListener('pointermove', onMove);
        handle.removeEventListener('pointerup', onEnd);
        handle.removeEventListener('pointercancel', onEnd);
        handle.classList.remove('dragging');
        document.body.classList.remove('resizing-columns');
        saveWidth(column, Math.round(currentWidth));
      };
      handle.addEventListener('pointermove', onMove);
      handle.addEventListener('pointerup', onEnd);
      handle.addEventListener('pointercancel', onEnd);
    });
    // Double-click goes back to the default width from style.css.
    handle.addEventListener('dblclick', () => {
      cardElement.style.removeProperty(MESSAGE_COLUMN_WIDTHS[column].cssVariable);
      saveWidth(column, null);
    });
  });
}

// Legend for the footer of the message list: explains the symbol that the
// NMEA view shows for an empty field. Only visible in the NMEA view.
const EMPTY_FIELD_LEGEND = `<span data-role="nmea-legend" title="An NMEA field with nothing between its two commas, e.g. the empty field in “$GPRMC,,A”"><span class="faint">∅</span> empty field</span>`;

// Connects the view buttons of a toolbar to a message list.
function bindViewModeButtons(container, messageList, initialMode) {
  const buttons = $$('[data-role=view-mode] button', container);
  const select = mode => {
    buttons.forEach(button => button.classList.toggle('on', button.dataset.view === mode));
    $$('[data-role=nmea-legend]', container).forEach(legend => legend.classList.toggle('hidden', mode !== 'nmea'));
    preferences.set('insp-view', mode);
    messageList.setView(mode);
  };
  buttons.forEach(button => (button.onclick = () => select(button.dataset.view)));
  select(initialMode);
}

/* ---------- detail tab: live inspector ---------- */

function renderInspectorTab(container, forwarderId) {
  let maxPerSecond = preferences.get('insp-rate', 100);
  let previewBytes = preferences.get('insp-preview', 1024);
  let isPaused = false;
  let receivedCount = 0, skippedCount = 0, newWhilePaused = 0;
  let stream = null; // EventSource of /api/forwarders/<id>/inspect

  container.innerHTML = `<div class="card insp">
    <div class="insp-bar">
      ${VIEW_MODE_BUTTONS}
      <div class="search">${icon('search')}<input data-role="filter" placeholder="Filter: text or /regex/" title="Only show messages containing this text; matches are highlighted. Use /…/ for a regular expression, e.g. /^\\$GP(RMC|GGA)/"></div>
      <select data-role="max-rate" title="Maximum messages per second sent to the browser. The rest is skipped (forwarding is never affected)">
        ${[10, 50, 100, 500, 2000].map(rate => `<option value="${rate}" ${rate === maxPerSecond ? 'selected' : ''}>${rate} / s</option>`).join('')}
      </select>
      ${PREVIEW_SIZE_SELECT(previewBytes)}
      <span class="grow"></span>
      <button class="btn sm" data-role="pause" title="Freeze the view to read it. New messages are counted and shown when you resume">${icon('pause')}Pause</button>
      <button class="btn sm" data-role="clear" title="Remove all shown messages">${icon('x')}Clear</button>
      <button class="btn sm" data-role="save" title="Download the shown messages as a text file">${icon('download')}</button>
    </div>
    ${MESSAGE_LIST_HEADER('Time the message was received (your local time)')}
    <div class="insp-log" data-role="log"></div>
    <div class="insp-foot"><span data-role="status"></span>${EMPTY_FIELD_LEGEND}<span class="grow"></span><span data-role="counters"></span></div>
  </div>`;
  const find = role => $(`[data-role=${role}]`, container);

  const messageList = createMessageList(find('log'), {
    formatTime: formatClockTime, maxRows: 500, maxKept: 2000, followNewest: true,
    emptyText: 'Start the forwarder to inspect its live data.',
  });
  bindViewModeButtons(container, messageList, preferences.get('insp-view', 'nmea'));
  bindColumnResizing($('.insp', container));

  function renderCounters() {
    find('counters').textContent = `${formatCount(receivedCount)} received · ${formatCount(skippedCount)} skipped by the limits`
      + (isPaused && newWhilePaused ? ` · ${newWhilePaused} new while paused` : '');
  }

  function openStream() {
    closeStream();
    const fwd = state.forwardersById.get(forwarderId);
    if (fwd?.state !== 'running') {
      find('status').innerHTML = '<span class="dotc" style="background:var(--off)"></span> Not running';
      messageList.setEmptyText('Start the forwarder to inspect its live data.');
      return;
    }
    stream = new EventSource(`/api/forwarders/${forwarderId}/inspect?rate=${maxPerSecond}&bytes=${previewBytes}`);
    find('status').innerHTML = '<span class="dotc" style="background:var(--ok)"></span> Live';
    messageList.setEmptyText('Waiting for data…');
    // The server sends batches: { rows: [{ t, s, n, d (base64) }], skipped }
    stream.onmessage = event => {
      const batch = JSON.parse(event.data);
      skippedCount = batch.skipped;
      const newMessages = (batch.rows || []).map(decodeMessage);
      receivedCount += newMessages.length;
      if (isPaused) newWhilePaused += newMessages.length;
      messageList.add(newMessages);
      renderCounters();
    };
    stream.addEventListener('end', closeStream); // the forwarder was stopped
  }
  function closeStream() { stream?.close(); stream = null; }

  find('filter').oninput = event => {
    try {
      messageList.setFilter(parseFilterInput(event.target.value));
      event.target.style.borderColor = '';
    } catch {
      event.target.style.borderColor = 'var(--err)'; // invalid regular expression
    }
  };
  find('max-rate').onchange = event => {
    maxPerSecond = Number(event.target.value);
    preferences.set('insp-rate', maxPerSecond);
    openStream(); // the limits are applied on the server
  };
  find('preview-size').onchange = event => {
    previewBytes = Number(event.target.value);
    preferences.set('insp-preview', previewBytes);
    openStream();
  };
  find('pause').onclick = event => {
    isPaused = !isPaused;
    event.currentTarget.innerHTML = isPaused ? icon('play') + 'Resume' : icon('pause') + 'Pause';
    if (!isPaused) newWhilePaused = 0;
    messageList.setPaused(isPaused);
    renderCounters();
  };
  find('clear').onclick = () => {
    receivedCount = 0;
    newWhilePaused = 0;
    messageList.clear();
    renderCounters();
  };
  find('save').onclick = () => {
    const lines = messageList.shownMessages()
      .map(message => `${new Date(message.time).toISOString()}\t${message.source}\t${message.text.replace(/[\r\n]+$/, '')}`);
    downloadFile(`delta-${toFileName(state.forwardersById.get(forwarderId)?.name || 'data')}-inspect.txt`, lines.join('\n'), 'text/plain');
  };

  let lastKnownState = state.forwardersById.get(forwarderId)?.state;
  openStream();
  renderCounters();
  return {
    // Connect or disconnect when the forwarder is started or stopped.
    update(fwd) {
      if (fwd.state !== lastKnownState) { lastKnownState = fwd.state; openStream(); }
    },
    destroy() { closeStream(); messageList.destroy(); },
  };
}

/* ---------- page: capture viewer ---------- */

// "+01:02.345" (minutes:seconds since the recording started)
function formatCaptureOffset(milliseconds) {
  const totalSeconds = milliseconds / 1000;
  const minutes = Math.floor(totalSeconds / 60);
  const hours = Math.floor(minutes / 60);
  const seconds = (totalSeconds % 60).toFixed(3).padStart(6, '0');
  return hours ? `+${hours}:${zeroPad(minutes % 60)}:${seconds}` : `+${zeroPad(minutes)}:${seconds}`;
}

// Shows the records of a capture file with the same views as the live
// inspector. Records are loaded page by page while scrolling; the search
// runs on the server, so it covers the whole file, not only loaded pages.
function renderCaptureViewerPage(main, captureName) {
  let previewBytes = preferences.get('insp-preview', 1024);
  let filterText = '';
  let nextPosition = 0;     // byte position for the next page; -1 = end of file
  let isLoading = false;
  let loadGeneration = 0;   // ignores answers of outdated requests (after the filter changed)
  let fileSize = 0, readPosition = 0;

  main.innerHTML = `
    <a class="crumb" href="#/captures">${icon('back')}Captures</a>
    <div class="page-head">
      <div class="grow" style="min-width:0"><h1 class="mono" style="font-size:17px;overflow-wrap:anywhere">${escapeHtml(captureName)}</h1>
        <div class="sub" data-role="summary">Loading…</div></div>
      <button class="btn" data-capture-action="replay" data-name="${escapeHtml(captureName)}" title="Send this recording into a running forwarder, as if its input received it">${icon('play')}Replay</button>
      <button class="btn" data-capture-action="download-raw" data-name="${escapeHtml(captureName)}" title="Download payload only (e.g. NMEA log)">${icon('download')}Raw</button>
      <button class="btn" data-capture-action="download-dcap" data-name="${escapeHtml(captureName)}" title="Download with timestamps (.dcap)">${icon('download')}.dcap</button>
    </div>
    <div class="card insp" style="height:calc(100vh - 190px);min-height:420px">
      <div class="insp-bar">
        ${VIEW_MODE_BUTTONS}
        <div class="search">${icon('search')}<input data-role="filter" placeholder="Search: text or /regex/" title="Searches the whole capture on the server; matches are highlighted. Use /…/ for a regular expression, add i for ignore case: /gga/i"></div>
        ${PREVIEW_SIZE_SELECT(previewBytes)}
      </div>
      ${MESSAGE_LIST_HEADER('Time since the recording started (minutes:seconds)')}
      <div class="insp-log" data-role="log"></div>
      <div class="insp-foot"><span data-role="progress"></span>${EMPTY_FIELD_LEGEND}<span class="grow"></span>
        <button class="btn sm" data-role="load-more" title="Load the next records (also happens automatically when you scroll down)">Load more</button></div>
    </div>`;
  const find = role => $(`[data-role=${role}]`, main);
  const log = find('log');
  const messageList = createMessageList(log, {
    formatTime: formatCaptureOffset, maxRows: 5000, maxKept: 5000, followNewest: false, emptyText: 'Loading…',
  });
  bindViewModeButtons(main, messageList, preferences.get('insp-view', 'nmea'));
  bindColumnResizing($('.insp', main));

  function renderProgress() {
    const loaded = messageList.messages.length;
    const percent = fileSize ? Math.round((readPosition / fileSize) * 100) : 100;
    const atEnd = nextPosition < 0;
    find('progress').textContent = `${formatCount(loaded)} records ${filterText ? 'found' : 'loaded'} · ${percent} % of ${formatBytes(fileSize)} read${atEnd ? ' · end of file' : ''}`;
    find('summary').textContent = `${formatBytes(fileSize)} · ${filterText ? `search “${filterText}”` : 'all records'}`;
    const isFull = loaded >= 5000;
    find('load-more').classList.toggle('hidden', atEnd || isFull);
    if (isFull && !atEnd) find('progress').textContent += ' · showing the first 5000, refine the search to see others';
  }

  async function loadNextPage() {
    if (isLoading || nextPosition < 0 || messageList.messages.length >= 5000) return;
    isLoading = true;
    const generation = loadGeneration;
    find('load-more').disabled = true;
    try {
      const page = await apiRequest('GET', `/api/captures/${encodeURIComponent(captureName)}/records?`
        + new URLSearchParams({ from: nextPosition, limit: 300, bytes: previewBytes, filter: filterText }));
      if (generation !== loadGeneration) return; // the search changed meanwhile
      nextPosition = page.next;
      fileSize = page.file_size;
      readPosition = page.position;
      messageList.add(page.records.map(decodeMessage));
      if (!messageList.messages.length) {
        messageList.setEmptyText(nextPosition < 0 ? (filterText ? 'No record matches the search.' : 'This capture is empty.') : 'Searching…');
      }
      renderProgress();
    } catch (error) {
      showError(error);
    } finally {
      if (generation === loadGeneration) { isLoading = false; find('load-more').disabled = false; }
    }
    // A search with few matches may need several pages before the list fills the screen.
    if (generation === loadGeneration && nextPosition >= 0 && log.scrollHeight <= log.clientHeight + 100) loadNextPage();
  }

  function restart() {
    loadGeneration++;
    isLoading = false;
    nextPosition = 0;
    messageList.clear();
    messageList.setEmptyText('Loading…');
    loadNextPage();
  }

  // Load the next page when the user scrolls near the end.
  log.addEventListener('scroll', () => {
    if (log.scrollTop + log.clientHeight >= log.scrollHeight - 300) loadNextPage();
  });
  find('load-more').onclick = loadNextPage;
  let searchTimer = null;
  find('filter').oninput = event => {
    const input = event.target.value;
    try {
      messageList.setFilter(parseFilterInput(input)); // highlighting in the browser
      event.target.style.borderColor = '';
    } catch {
      event.target.style.borderColor = 'var(--err)';
      return;
    }
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => { filterText = input; restart(); }, 350); // search once typing pauses
  };
  find('preview-size').onchange = event => {
    previewBytes = Number(event.target.value);
    preferences.set('insp-preview', previewBytes);
    restart();
  };
  main.onclick = event => {
    const captureButton = event.target.closest('[data-capture-action]');
    if (captureButton) handleCaptureButton(captureButton, () => {});
  };

  restart();
  return { destroy: () => { loadGeneration++; clearTimeout(searchTimer); messageList.destroy(); } };
}

/* ---------- detail tab: connections ---------- */

// Lists every connected peer of every endpoint. Refreshed every 2 seconds,
// because the live stream does not include peer lists.
function renderConnectionsTab(container, forwarderId) {
  async function loadAndRender() {
    try {
      const fwd = await apiRequest('GET', '/api/forwarders/' + forwarderId);
      if (fwd.state !== 'running') {
        container.innerHTML = `<div class="card"><div class="empty">The forwarder is not running.</div></div>`;
        return;
      }
      // [endpoint key for the API, endpoint config, live status, label]
      const endpoints = [
        ['in', fwd.input, fwd.in_status, 'Input'],
        ...fwd.outputs.map((out, i) => [`out:${i}`, out, fwd.out_status[i], out.name || `Output ${i + 1}`]),
      ];
      let rowsHtml = '';
      for (const [endpointKey, endpoint, status, label] of endpoints) {
        const peers = (status.peers || []).sort((a, b) => a.remote.localeCompare(b.remote));
        for (const peer of peers) {
          // UDP senders have no connection that could be closed.
          const disconnectButton = endpoint.mode === 'udp-listen' ? ''
            : `<button class="btn sm" data-disconnect="${endpointKey}" data-peer="${peer.id}" title="Close this connection">${icon('x')}Disconnect</button>`;
          rowsHtml += `<tr>
            <td>${escapeHtml(label)} ${endpointBadge(endpoint)}</td>
            <td class="mono">${escapeHtml(peer.remote)}</td>
            <td>${timeAgo(peer.since).replace(' ago', '')}</td>
            <td>${timeAgo(peer.last)}</td>
            <td class="num">${formatBytes(peer.bytes)}</td>
            <td class="right">${disconnectButton}</td>
          </tr>`;
        }
      }
      container.innerHTML = `<div class="card"><div class="tbl-wrap"><table class="t compact">
        <thead><tr><th>Endpoint</th><th>Remote</th><th>Connected</th><th>Last data</th><th class="right">Bytes</th><th></th></tr></thead>
        <tbody>${rowsHtml || '<tr><td colspan="6" class="muted" style="text-align:center;padding:30px">No active connections</td></tr>'}</tbody>
      </table></div></div>`;
    } catch (error) {
      container.innerHTML = `<div class="banner error">${escapeHtml(error.message)}</div>`;
    }
  }
  container.onclick = async event => {
    const button = event.target.closest('[data-disconnect]');
    if (!button) return;
    try {
      await apiRequest('POST', `/api/forwarders/${forwarderId}/kick`, { endpoint: button.dataset.disconnect, peer: Number(button.dataset.peer) });
      showToast('Connection closed');
      setTimeout(loadAndRender, 300);
    } catch (error) { showError(error); }
  };
  loadAndRender();
  const refreshTimer = setInterval(loadAndRender, 2000);
  return { destroy: () => clearInterval(refreshTimer) };
}

/* ---------- event log (page and detail tab) ---------- */

function eventRowHtml(event, showForwarderColumn) {
  const forwarderCell = !showForwarderColumn ? ''
    : `<td class="nowrap">${event.fid ? `<a href="#/f/${event.fid}">${escapeHtml(event.name)}</a>` : '<span class="faint">system</span>'}</td>`;
  return `<tr>
    <td class="nowrap mono faint" style="width:160px">${formatDateTime(event.t)}</td>
    <td class="ev-lvl"><span class="pill ${escapeHtml(event.level)}">${escapeHtml(event.level)}</span></td>
    ${forwarderCell}
    <td class="ev-msg">${escapeHtml(event.msg)}</td>
  </tr>`;
}

function renderForwarderEventsTab(container, forwarderId) {
  let events = [];
  container.innerHTML = `<div class="card"><div class="tbl-wrap"><table class="t compact">
    <thead><tr><th>Time</th><th title="info = normal, warn = a problem Delta handles itself, error = needs attention">Level</th><th>Message</th></tr></thead>
    <tbody id="event-rows"><tr><td class="muted" colspan="3">Loading…</td></tr></tbody></table></div></div>`;
  const render = () => {
    $('#event-rows', container).innerHTML = events.length
      ? events.slice(0, 300).map(event => eventRowHtml(event, false)).join('')
      : '<tr><td class="muted" colspan="3" style="text-align:center;padding:30px">No events yet</td></tr>';
  };
  apiRequest('GET', `/api/events?forwarder=${forwarderId}&limit=300`).then(loaded => { events = loaded; render(); }).catch(showError);
  return {
    onEvent: event => {
      if (event.fid === forwarderId) { events.unshift(event); render(); }
    },
  };
}

function renderEventsPage(main) {
  let events = [], levelFilter = 'all', searchText = '';
  main.innerHTML = `
    <div class="page-head"><div class="grow"><h1>Events</h1><div class="sub">Connections, state changes, errors and alerts — newest first</div></div></div>
    <div class="card">
      <div class="toolbar" style="padding:12px">
        <div class="search">${icon('search')}<input id="event-search" placeholder="Search events"></div>
        <div class="seg" id="level-filter">
          ${[['all', 'All events'], ['info', 'Normal events: connects, starts, stops'], ['warn', 'Problems Delta handles itself, e.g. lost connections'], ['error', 'Problems that need attention, e.g. failed starts and alerts']]
            .map(([level, hint]) => `<button data-level="${level}" class="${level === 'all' ? 'on' : ''}" title="${hint}">${level[0].toUpperCase() + level.slice(1)}</button>`).join('')}
        </div>
      </div>
      <div class="tbl-wrap"><table class="t compact">
        <thead><tr><th>Time</th><th title="info = normal, warn = a problem Delta handles itself, error = needs attention">Level</th><th title="Forwarder the event belongs to; “system” for Delta itself">Forwarder</th><th>Message</th></tr></thead>
        <tbody id="event-rows"></tbody></table></div>
    </div>`;
  const render = () => {
    const search = searchText.toLowerCase();
    const shown = events
      .filter(event => (levelFilter === 'all' || event.level === levelFilter)
        && (!search || `${event.name} ${event.msg}`.toLowerCase().includes(search)))
      .slice(0, 1000);
    $('#event-rows', main).innerHTML = shown.length
      ? shown.map(event => eventRowHtml(event, true)).join('')
      : '<tr><td class="muted" colspan="4" style="text-align:center;padding:30px">No events</td></tr>';
  };
  main.oninput = event => {
    if (event.target.id === 'event-search') { searchText = event.target.value; render(); }
  };
  main.onclick = event => {
    const level = event.target.closest('[data-level]')?.dataset.level;
    if (!level) return;
    levelFilter = level;
    $$('#level-filter button', main).forEach(button => button.classList.toggle('on', button.dataset.level === level));
    render();
  };
  apiRequest('GET', '/api/events?limit=5000').then(loaded => { events = loaded; render(); }).catch(showError);
  return {
    onEvent: event => {
      events.unshift(event);
      if (events.length > 5000) events.pop();
      render();
    },
  };
}

/* ---------- captures (page and detail tab) ---------- */

function captureTableHtml(files, emptyHtml) {
  if (!files.length) return `<div class="empty">${emptyHtml}</div>`;
  const rows = files.map(file => {
    const name = escapeHtml(file.name);
    return `<tr>
      <td class="mono"><a href="#/captures/${encodeURIComponent(file.name)}" title="Open in the capture viewer">${name}</a> ${file.active ? `<span class="pill error">${icon('record')}recording</span>` : ''}</td>
      <td class="num">${formatBytes(file.size)}</td>
      <td class="nowrap">${formatDateTime(file.modified)}</td>
      <td><div class="actions">
        <button class="btn sm" data-capture-action="inspect" data-name="${name}" title="Look at the recorded messages, with search and Text / NMEA / Hex views">${icon('eye')}Inspect</button>
        <button class="btn sm" data-capture-action="replay" data-name="${name}" ${file.active ? 'disabled' : ''} title="Send this recording into a running forwarder, as if its input received it">${icon('play')}Replay</button>
        <button class="btn sm" data-capture-action="download-raw" data-name="${name}" title="Download payload only (e.g. NMEA log)">${icon('download')}Raw</button>
        <button class="btn sm" data-capture-action="download-dcap" data-name="${name}" title="Download with timestamps (.dcap)">${icon('download')}.dcap</button>
        <button class="btn icon sm ghost" data-capture-action="delete" data-name="${name}" title="Delete" ${file.active ? 'disabled' : ''}>${icon('trash')}</button>
      </div></td>
    </tr>`;
  }).join('');
  return `<div class="tbl-wrap"><table class="t compact">
    <thead><tr><th title="File name: forwarder name and start time of the recording">Capture</th><th class="right">Size</th><th>Created</th><th></th></tr></thead>
    <tbody>${rows}</tbody></table></div>`;
}

// Handles the buttons of captureTableHtml. reloadList refreshes the table afterwards.
async function handleCaptureButton(button, reloadList, preselectedForwarderId) {
  const name = button.dataset.name;
  const encodedName = encodeURIComponent(name);
  switch (button.dataset.captureAction) {
    case 'inspect': location.hash = '#/captures/' + encodedName; break;
    case 'download-raw': location.href = `/api/captures/${encodedName}?format=raw`; break;
    case 'download-dcap': location.href = `/api/captures/${encodedName}`; break;
    case 'replay': openReplayDialog(name, preselectedForwarderId); break;
    case 'delete':
      if (await askConfirmation('Delete capture', `<b>${escapeHtml(name)}</b> will be deleted permanently.`, 'Delete')) {
        await apiRequest('DELETE', `/api/captures/${encodedName}`).then(() => showToast('Capture deleted')).catch(showError);
        reloadList();
      }
  }
}

function openReplayDialog(captureName, preselectedForwarderId) {
  const runningForwarders = state.forwarders.filter(fwd => fwd.state === 'running');
  if (!runningForwarders.length) return showToast('Start a forwarder first — replayed data is sent to its outputs.', 'error');
  let timing = 'original'; // "original" | "interval" | "max"

  const overlay = openModal(`<form>
    <div class="modal-h"><h2>Replay capture</h2></div>
    <div class="modal-b">
      <div class="muted mono" style="font-size:12.5px">${escapeHtml(captureName)}</div>
      <label class="f">Into forwarder
        <select name="forwarder">${runningForwarders.map(fwd => `<option value="${fwd.id}" ${fwd.id === preselectedForwarderId ? 'selected' : ''}>${escapeHtml(fwd.name)}</option>`).join('')}</select>
        <span class="hint">The data is sent to all outputs of this forwarder, as if its input had received it.</span>
      </label>
      <label class="f">Timing
        <div class="seg" id="timing-choice">
          <button type="button" data-timing="original" class="on">Original</button>
          <button type="button" data-timing="interval">Fixed interval</button>
          <button type="button" data-timing="max">As fast as possible</button>
        </div>
      </label>
      <div class="grid2">
        <label class="f" id="speed-field">Speed<input type="number" name="speed" value="1" min="0.01" step="any"><span class="hint">1 = real time, 10 = ten times faster</span></label>
        <label class="f hidden" id="interval-field">Interval (ms)<input type="number" name="interval" value="100" min="1" step="1"><span class="hint">Delay between messages</span></label>
        <label class="chk" style="align-self:end;margin-bottom:8px"><input type="checkbox" name="loop"> Loop forever</label>
      </div>
    </div>
    <div class="modal-f"><span class="form-err" id="replay-error"></span><button type="button" class="btn" data-cancel>Cancel</button><button class="btn primary">${icon('play')}Start replay</button></div>
  </form>`, 'small');

  $('#timing-choice', overlay).onclick = event => {
    const chosen = event.target.closest('[data-timing]')?.dataset.timing;
    if (!chosen) return;
    timing = chosen;
    $$('#timing-choice button', overlay).forEach(button => button.classList.toggle('on', button.dataset.timing === chosen));
    $('#speed-field', overlay).classList.toggle('hidden', chosen !== 'original');
    $('#interval-field', overlay).classList.toggle('hidden', chosen !== 'interval');
  };
  $('[data-cancel]', overlay).onclick = closeModal;
  $('form', overlay).onsubmit = async event => {
    event.preventDefault();
    const form = event.target;
    // speed 0 and interval 0 together mean "as fast as possible"
    const options = {
      forwarder: form.forwarder.value,
      loop: form.loop.checked,
      speed: timing === 'original' ? Number(form.speed.value) || 1 : 0,
      interval_ms: timing === 'interval' ? Number(form.interval.value) || 100 : 0,
    };
    try {
      await apiRequest('POST', `/api/captures/${encodeURIComponent(captureName)}/replay`, options);
      closeModal();
      showToast('Replay started');
    } catch (error) { $('#replay-error', overlay).textContent = error.message; }
  };
}

function renderForwarderCapturesTab(container, forwarderId) {
  container.innerHTML = `<div class="stack">
    <div class="card" id="recording-controls"></div>
    <div class="card"><div class="card-h"><h2>Captures of this forwarder</h2><span class="grow"></span><a href="#/captures">All captures →</a></div><div id="capture-list"></div></div>
  </div>`;

  // Capture files are named "<forwarder name>-<date>.dcap".
  const reloadList = () => apiRequest('GET', '/api/captures').then(files => {
    const prefix = toFileName(state.forwardersById.get(forwarderId)?.name || '') + '-';
    $('#capture-list', container).innerHTML = captureTableHtml(files.filter(file => file.name.startsWith(prefix)), 'No captures yet.');
  }).catch(showError);

  let lastControlsKey = '';
  let wasRecording = false;
  function renderRecordingControls(fwd) {
    const controlsKey = JSON.stringify([fwd.state, !!fwd.capture, fwd.capture?.records]);
    if (controlsKey === lastControlsKey) return;
    lastControlsKey = controlsKey;
    if (wasRecording && !fwd.capture) reloadList(); // a recording just finished
    wasRecording = !!fwd.capture;

    const controls = $('#recording-controls', container);
    if (fwd.capture) {
      controls.innerHTML = `<div class="card-b row wrap"><span class="pill error">${icon('record')}Recording</span><b class="mono">${escapeHtml(fwd.capture.name)}</b>
        <span class="muted">${formatCount(fwd.capture.records)} messages · ${formatBytes(fwd.capture.bytes)} · since ${new Date(fwd.capture.started).toLocaleTimeString()}</span>
        <span class="grow"></span><button class="btn" data-recording="stop">${icon('stop')}Stop recording</button></div>`;
      return;
    }
    // Keep the form (and what the user typed) if it is already shown.
    const startButton = controls.querySelector('[data-recording=start]');
    if (startButton) { startButton.disabled = fwd.state !== 'running'; return; }
    controls.innerHTML = `<div class="card-b">
      <div class="row wrap" style="align-items:flex-end;gap:12px">
        <label class="f">Stop after (seconds)<input type="number" name="max-seconds" min="0" placeholder="no limit" style="width:150px"></label>
        <label class="f">Max size (MB)<input type="number" name="max-mb" min="0" placeholder="no limit" style="width:150px"></label>
        <button class="btn primary" data-recording="start" ${fwd.state === 'running' ? '' : 'disabled'}>${icon('record')}Start recording</button>
        <span class="muted grow" style="font-size:12.5px">Records everything the input receives, with timestamps. Replay it later into any forwarder.</span>
      </div></div>`;
  }

  container.onclick = async event => {
    const recordingAction = event.target.closest('[data-recording]')?.dataset.recording;
    if (recordingAction === 'start') {
      const limits = {
        max_seconds: Number($('[name=max-seconds]', container).value) || 0,
        max_mb: Number($('[name=max-mb]', container).value) || 0,
      };
      await apiRequest('POST', `/api/forwarders/${forwarderId}/capture-start`, limits)
        .then(() => { showToast('Recording started'); reloadList(); }).catch(showError);
    } else if (recordingAction === 'stop') {
      await apiRequest('POST', `/api/forwarders/${forwarderId}/capture-stop`).catch(showError);
    }
    const captureButton = event.target.closest('[data-capture-action]');
    if (captureButton) handleCaptureButton(captureButton, reloadList, forwarderId);
  };
  reloadList();
  renderRecordingControls(state.forwardersById.get(forwarderId));
  return { update: renderRecordingControls };
}

function renderCapturesPage(main) {
  main.innerHTML = `
    <div class="page-head">
      <div class="grow"><h1>Captures</h1><div class="sub">Recorded traffic — download it, or replay it into a running forwarder</div></div>
      <input type="file" id="upload-input" class="hidden">
      <button class="btn" data-action="upload" title="Upload a .dcap file or a plain NMEA log (one sentence per line) to replay it">${icon('upload')}Upload file</button>
    </div>
    <div id="active-replays"></div>
    <div class="card" id="capture-list"><div class="empty">Loading…</div></div>`;

  const reloadList = () => apiRequest('GET', '/api/captures').then(files => {
    $('#capture-list', main).innerHTML = captureTableHtml(files, `<div class="big">${icon('disc')}</div><h2>No captures yet</h2>
      <p>Open a forwarder and start a recording on its Captures tab,<br>or upload a .dcap file or an NMEA log (one message per line).</p>`);
  }).catch(showError);

  function renderActiveReplays() {
    const replaying = state.forwarders.filter(fwd => fwd.replay);
    const html = !replaying.length ? '' : `<div class="card" style="margin-bottom:16px"><div class="card-h"><h2>Active replays</h2></div>
      <div class="tbl-wrap"><table class="t compact">
      <thead><tr><th>Forwarder</th><th>Capture</th><th class="right">Progress</th><th></th></tr></thead>
      <tbody>
      ${replaying.map(fwd => `<tr>
        <td><a href="#/f/${fwd.id}">${escapeHtml(fwd.name)}</a></td>
        <td class="mono">${escapeHtml(fwd.replay.capture)}</td>
        <td class="num">${formatCount(fwd.replay.sent)} sent${fwd.replay.loop ? ' · loop' : ''}</td>
        <td class="right"><button class="btn sm" data-stop-replay="${fwd.id}">${icon('stop')}Stop</button></td>
      </tr>`).join('')}
      </tbody></table></div></div>`;
    setHtmlIfChanged($('#active-replays', main), html);
  }

  main.onclick = async event => {
    if (event.target.closest('[data-action=upload]')) return $('#upload-input', main).click();
    const stopReplayButton = event.target.closest('[data-stop-replay]');
    if (stopReplayButton) return apiRequest('POST', `/api/forwarders/${stopReplayButton.dataset.stopReplay}/replay-stop`).catch(showError);
    const captureButton = event.target.closest('[data-capture-action]');
    if (captureButton) handleCaptureButton(captureButton, reloadList);
  };
  main.onchange = async event => {
    if (event.target.id !== 'upload-input' || !event.target.files[0]) return;
    const file = event.target.files[0];
    try {
      const result = await apiRequest('POST', '/api/captures?name=' + encodeURIComponent(file.name), file, true);
      showToast('Uploaded as ' + result.name);
      reloadList();
    } catch (error) { showError(error); }
    event.target.value = ''; // allow uploading the same file again
  };

  reloadList();
  renderActiveReplays();
  let wasAnyRecording = state.forwarders.some(fwd => fwd.capture);
  return {
    update() {
      renderActiveReplays();
      // Reload the file list when a recording starts or ends.
      const isAnyRecording = state.forwarders.some(fwd => fwd.capture);
      if (isAnyRecording !== wasAnyRecording) { wasAnyRecording = isAnyRecording; reloadList(); }
    },
  };
}

/* ---------- detail tab: configuration ---------- */

function renderConfigTab(container, forwarderId) {
  function render() {
    const config = configOf(state.forwardersById.get(forwarderId));
    const inputExtras = config.input.max_clients ? ` (max ${config.input.max_clients} clients)` : '';
    const outputRows = config.outputs.map((out, i) => {
      const extras = (out.max_clients ? ` (max ${out.max_clients} clients)` : '') + (out.queue ? ` · queue ${out.queue}` : '');
      return `<dt>Output ${i + 1}</dt><dd>${out.name ? escapeHtml(out.name) + ': ' : ''}${MODE_LABELS[out.mode]} ${escapeHtml(allEndpointAddresses(out).join(', '))}${extras}</dd>`;
    }).join('');
    container.innerHTML = `<div class="grid2" style="align-items:start">
      <div class="card">
        <div class="card-h"><h2>Summary</h2><span class="grow"></span><button class="btn sm" data-config-action="edit">${icon('edit')}Edit</button></div>
        <div class="card-b"><dl class="kv">
          <dt>ID</dt><dd>${escapeHtml(config.id)}</dd>
          <dt>Created</dt><dd>${formatDateTime(config.created)}</dd>
          ${config.comment ? `<dt>Comment</dt><dd style="white-space:pre-wrap;font-family:var(--sans);font-size:13px">${escapeHtml(config.comment)}</dd>` : ''}
          <dt>Data format</dt><dd>${config.framing === 'lines' ? 'NMEA / text lines' : 'Raw byte stream'}</dd>
          <dt>Start with Delta</dt><dd>${config.autostart ? 'yes' : 'no'}</dd>
          <dt>Idle alert</dt><dd>${config.idle_alert ? config.idle_alert + ' s' : 'off'}</dd>
          <dt>Input</dt><dd>${MODE_LABELS[config.input.mode]} ${escapeHtml(allEndpointAddresses(config.input).join(', '))}${inputExtras}</dd>
          ${outputRows}
        </dl></div>
      </div>
      <div class="card">
        <div class="card-h"><h2>JSON</h2><span class="grow"></span>
          <button class="btn sm" data-config-action="copy">${icon('copy')}Copy</button>
          <button class="btn sm" data-config-action="export">${icon('download')}Export</button>
        </div>
        <pre class="mono" style="margin:0;padding:16px 18px;overflow:auto;max-height:420px">${escapeHtml(JSON.stringify(config, null, 2))}</pre>
      </div>
    </div>`;
  }
  container.onclick = event => {
    const action = event.target.closest('[data-config-action]')?.dataset.configAction;
    const fwd = state.forwardersById.get(forwarderId);
    if (action === 'edit') openForwarderEditor(fwd);
    if (action === 'export') exportForwarders([forwarderId]);
    if (action === 'copy') navigator.clipboard?.writeText(JSON.stringify(configOf(fwd), null, 2)).then(() => showToast('Copied'), showError);
  };
  render();
  let renderedConfig = JSON.stringify(configOf(state.forwardersById.get(forwarderId)));
  return {
    // Only re-render when the configuration changed (not on every live update).
    update(fwd) {
      const currentConfig = JSON.stringify(configOf(fwd));
      if (currentConfig !== renderedConfig) { renderedConfig = currentConfig; render(); }
    },
  };
}

/* ================= page: settings ================= */

function renderSettingsPage(main) {
  main.innerHTML = `<div class="page-head"><div class="grow"><h1>Settings</h1><div class="sub">Stored in the Delta config file</div></div></div><div id="settings-body" class="muted">Loading…</div>`;
  apiRequest('GET', '/api/settings').then(settings => {
    const body = $('#settings-body', main);
    body.className = '';
    const configuredAddress = `${settings.bind}:${settings.port}`;
    const addressNote = settings.web_addr !== configuredAddress
      ? ` <span class="faint">(config: ${escapeHtml(settings.bind || '*')}:${settings.port})</span>` : '';
    body.innerHTML = `<div class="grid2" style="align-items:start">
      <div class="stack">
        <form class="card" id="alert-settings">
          <div class="card-h">${icon('bell')}<h2>Alerts</h2></div>
          <div class="card-b stack">
            <label class="f">Webhook URL<input name="webhook" value="${escapeHtml(settings.webhook_url)}" placeholder="https://hooks.slack.com/…"><span class="hint">Receives a JSON POST when an alert fires or resolves. Works with Slack, Mattermost, Teams, Discord, ntfy and others.</span></label>
            <label class="f">Alert when a forwarder is down for (s)<input type="number" name="downAfter" min="1" value="${settings.down_after}" style="max-width:160px"><span class="hint">Down = failed to start, or an endpoint is not connected. Idle alerts are set per forwarder.</span></label>
            <div class="row"><button class="btn primary">Save</button><button type="button" class="btn" id="test-webhook" title="Send a test message to the webhook URL above">Send test</button></div>
          </div>
        </form>
        <form class="card" id="event-log-settings">
          <div class="card-h">${icon('activity')}<h2>Event log</h2></div>
          <div class="card-b stack">
            <label class="f">Log file<input name="eventLog" value="${escapeHtml(settings.event_log)}" placeholder="delta-events.log" title="File every event is appended to. A relative path is next to the config file; off keeps the events in memory only"><span class="hint">Every event is appended to this file (one syslog-style text line per event) and the newest 5000 are loaded again after a restart. A relative path is next to the config file. Enter <code>off</code> to keep events in memory only.</span></label>
            <label class="f">Rotate at (MB)<input type="number" name="eventLogMaxMB" min="1" step="1" value="${settings.event_log_max_mb}" style="max-width:160px" title="When the file reaches this size it is renamed with a timestamp and a new file is started"><span class="hint">A full file is renamed to <code>&lt;name&gt;-&lt;timestamp&gt;.log</code> (replacing the older one), so at most twice this size is used.</span></label>
            <div class="muted" id="event-log-path" title="The file Delta is writing to right now">In use: ${escapeHtml(settings.event_log_path || 'off (memory only)')}</div>
            <div class="row"><button class="btn primary" title="Switch to this log file and size">Save</button></div>
          </div>
        </form>
        <form class="card" id="password-settings">
          <div class="card-h">${icon('key')}<h2>Password</h2></div>
          <div class="card-b stack">
            <label class="f">Current password<input type="password" name="current" autocomplete="current-password" required></label>
            <div class="grid2">
              <label class="f">New password<input type="password" name="newPassword" autocomplete="new-password" minlength="8" required></label>
              <label class="f">Repeat<input type="password" name="repeat" autocomplete="new-password" required></label>
            </div>
            <div class="row"><button class="btn primary">Change password</button></div>
          </div>
        </form>
      </div>
      <div class="stack">
        <form class="card" id="update-settings">
          <div class="card-h">${icon('download')}<h2>Updates</h2></div>
          <div class="card-b stack">
            <label class="chk" title="Ask GitHub for a new Delta release shortly after startup and once a day. Needs internet access"><input type="checkbox" name="updateCheck" ${settings.update_check ? 'checked' : ''}> Check for updates at startup and once a day</label>
            <label class="chk" title="Download, verify and install a new release by itself, then restart Delta. Waits while a capture or replay is running"><input type="checkbox" name="updateAutoInstall" ${settings.update_auto_install ? 'checked' : ''} ${settings.update_check ? '' : 'disabled'}> Install new versions automatically and restart</label>
            <div id="update-status"></div>
            <div class="row">
              <button type="button" class="btn" id="update-check-now" title="Ask GitHub for the latest release now (works even when the automatic check is off)">Check now</button>
              <button type="button" class="btn primary hidden" id="update-install" title="Download the new version, verify its checksum, replace the binary and restart Delta"></button>
            </div>
            <span class="hint faint">Installs <code>${escapeHtml(settings.update_asset)}</code> from the GitHub release after checking it against <code>SHA256SUMS</code>. The running binary is kept as <code>.old</code>. Delta needs write access to the folder of its binary.</span>
          </div>
        </form>
        <div class="card">
          <div class="card-h">${icon('download')}<h2>Backup</h2></div>
          <div class="card-b stack">
            <div class="muted">Export all forwarders as JSON, or import forwarders from a file. Imported forwarders are added as new and stopped.</div>
            <div class="row">
              <a class="btn" href="/api/export" title="Download all forwarders as a JSON file">${icon('download')}Export all</a>
              <button class="btn" id="import-button" title="Add forwarders from a JSON file. They are added as new and stay stopped">${icon('upload')}Import…</button>
            </div>
          </div>
        </div>
        <div class="card">
          <div class="card-h">${icon('info')}<h2>About</h2></div>
          <div class="card-b"><dl class="kv">
            <dt>Version</dt><dd>Delta ${escapeHtml(settings.version)}</dd>
            <dt>Config file</dt><dd>${escapeHtml(settings.config_path)}</dd>
            <dt>Data directory</dt><dd>${escapeHtml(settings.data_dir)}</dd>
            <dt>Event log</dt><dd id="about-event-log">${escapeHtml(settings.event_log_path || 'off (memory only)')}</dd>
            <dt>Web interface</dt><dd>${escapeHtml(settings.web_addr)}${addressNote}</dd>
            <dt>REST API</dt><dd>curl -u :PASSWORD ${escapeHtml(location.origin)}/api/forwarders</dd>
          </dl></div>
        </div>
      </div>
    </div>`;

    $('#alert-settings', main).onsubmit = async event => {
      event.preventDefault();
      const form = event.target;
      await apiRequest('PUT', '/api/settings', { webhook_url: form.webhook.value.trim(), down_after: Number(form.downAfter.value) || 30 })
        .then(() => showToast('Alert settings saved')).catch(showError);
    };
    $('#event-log-settings', main).onsubmit = async event => {
      event.preventDefault();
      const form = event.target;
      await apiRequest('PUT', '/api/settings', { event_log: form.eventLog.value.trim(), event_log_max_mb: Number(form.eventLogMaxMB.value) || 10 })
        .then(saved => {
          // Show the file actually in use (a relative path is resolved against the config file).
          const inUse = saved.event_log_path || 'off (memory only)';
          $('#event-log-path', main).textContent = `In use: ${inUse}`;
          $('#about-event-log', main).textContent = inUse;
          form.eventLog.value = saved.event_log;
          showToast('Event log settings saved');
        }).catch(showError);
    };
    $('#test-webhook', main).onclick = async () => {
      const url = $('#alert-settings', main).webhook.value.trim();
      if (!url) return showToast('Enter a webhook URL first', 'error');
      await apiRequest('POST', '/api/settings/test-webhook', { url }).then(() => showToast('Test notification sent')).catch(showError);
    };
    $('#password-settings', main).onsubmit = async event => {
      event.preventDefault();
      const form = event.target;
      if (form.newPassword.value !== form.repeat.value) return showToast('The new passwords do not match', 'error');
      await apiRequest('PUT', '/api/settings', { current_password: form.current.value, password: form.newPassword.value })
        .then(() => { showToast('Password changed'); form.reset(); }).catch(showError);
    };
    $('#import-button', main).onclick = importForwardersFromFile;

    // ---- updates ----
    const updateForm = $('#update-settings', main);
    const saveUpdateSettings = () => {
      updateForm.updateAutoInstall.disabled = !updateForm.updateCheck.checked;
      if (!updateForm.updateCheck.checked) updateForm.updateAutoInstall.checked = false;
      apiRequest('PUT', '/api/settings', {
        update_check: updateForm.updateCheck.checked,
        update_auto_install: updateForm.updateCheck.checked && updateForm.updateAutoInstall.checked,
      }).then(() => showToast('Update settings saved')).catch(showError);
    };
    updateForm.updateCheck.onchange = saveUpdateSettings;
    updateForm.updateAutoInstall.onchange = saveUpdateSettings;
    $('#update-check-now', main).onclick = () => apiRequest('POST', '/api/update/check')
      .then(status => {
        receiveUpdateStatus(status);
        if (!status.available) showToast(`Delta ${status.current} is the latest version`);
      }).catch(showError);
    $('#update-install', main).onclick = () => {
      const latest = state.updateStatus?.latest;
      askConfirmation(`Install Delta ${latest}?`,
        'Delta downloads the new version, checks it, replaces its binary and restarts. Forwarders stop for a moment and start again by themselves. Running captures and replays end (captures are saved). Everybody has to sign in again afterwards.',
        'Install & restart', false).then(confirmed => {
        if (confirmed) apiRequest('POST', '/api/update/install').then(receiveUpdateStatus).catch(showError);
      });
    };
    // Fresh from the server; it wins over the copy kept from live events.
    receiveUpdateStatus(settings.update);
  }).catch(showError);

  // The status line and the install button of the Updates card.
  function showUpdateStatus(status) {
    const statusBox = $('#update-status', main);
    if (!statusBox || !status) return;
    const checked = status.checked_at ? `last checked ${timeAgo(status.checked_at)}` : 'not checked yet';
    let html;
    if (status.state === 'checking') html = `<span class="muted">Checking GitHub for a new release…</span>`;
    else if (status.state === 'installing') html = `<span class="muted">Downloading and installing Delta ${escapeHtml(status.latest)}…</span>`;
    else if (status.state === 'pending') html = `<span class="muted">Delta ${escapeHtml(status.latest)} is installed. Delta restarts as soon as the running captures and replays have ended.</span>`;
    else if (status.state === 'restarting') html = `<span class="muted">Delta ${escapeHtml(status.latest)} installed. Restarting…</span>`;
    else if (status.available) html = `<b>Delta ${escapeHtml(status.latest)} is available</b> <span class="muted">(running ${escapeHtml(status.current)}, ${checked})</span>`
      + (status.release_url ? ` · <a href="${escapeHtml(status.release_url)}" target="_blank" rel="noopener" title="What is new in this release (opens GitHub)">release notes</a>` : '');
    else if (status.development) html = `<span class="muted">Self-built version (${escapeHtml(status.current)}): it is never replaced by a release.${status.latest ? ` Latest release: ${escapeHtml(status.latest)} (${checked}).` : ''}</span>`;
    else if (status.latest) html = `<span class="muted">Delta ${escapeHtml(status.current)} is up to date (${checked})</span>`;
    else html = `<span class="muted">Running Delta ${escapeHtml(status.current)}, ${checked}</span>`;
    if (status.error) html += `<div class="form-err" style="margin:6px 0 0" title="The reason the last check or install failed">${escapeHtml(status.error)}</div>`;
    setHtmlIfChanged(statusBox, html);

    const busy = status.state !== 'idle';
    const installButton = $('#update-install', main);
    installButton.classList.toggle('hidden', !status.available);
    installButton.textContent = `Install ${status.latest || ''}`;
    installButton.disabled = busy;
    $('#update-check-now', main).disabled = busy;
  }
  return { onUpdateStatus: showUpdateStatus };
}

/* ================= start ================= */

showEntryPage();
