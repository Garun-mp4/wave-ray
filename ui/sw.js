// RuView Service Worker - Offline caching for the dashboard shell
// Strategy: Network-first for API calls, Cache-first for static assets

// Bumped when shared and lab UI styles change so clients leave old offline assets.
const CACHE_NAME = 'ruview-v21';

// Requests whose response depends on the caller's credentials. These must never
// be served from the Cache API.
//
// The Cache API is NOT the HTTP cache: it ignores `Cache-Control` completely, so
// the `no-store, no-cache, must-revalidate` the server already sends on
// `/oauth/status` has no effect here. A cached signed-out response was returned
// to the page forever, and only a hard reload — which bypasses the service
// worker entirely — showed the true state. (ADR-271.)
const NEVER_CACHE_PREFIXES = ['/oauth/'];

// What may be served cache-first. Previously cache-first was the *catch-all* for
// everything outside `/api/` and `/health/`, which meant any endpoint added at a
// new path was frozen on first response. An allowlist fails safe instead: an
// unrecognised path goes to the network untouched.
const STATIC_ASSET = /\.(?:js|mjs|css|html|json|png|jpe?g|gif|svg|ico|webp|woff2?|ttf|map)$/i;
const SHELL_ASSETS = [
  '/ui/',
  '/ui/index.html',
  '/ui/manifest.json',
  '/ui/workspace/app.js?v=11',
  '/ui/workspace/api.js',
  '/ui/workspace/view-model.js',
  '/ui/workspace/workspace.css?v=11',
  '/ui/workspace/design-tokens.css?v=7',
  '/ui/workspace/theme.js?v=2',
  '/ui/workspace/fonts/InterVariable.woff2',
  '/ui/workspace/icons.svg',
  '/ui/workspace/favicon.svg',
  '/ui/services/api.service.js',
  '/ui/services/sensing.service.js',
  '/ui/services/ws-ticket.js',
  '/ui/config/api.config.js',
  '/ui/utils/backend-detector.js',
];

// Install - cache shell assets
self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => {
      return cache.addAll(SHELL_ASSETS).catch((err) => {
        // Don't fail install if some assets are missing (dev mode)
        console.warn('[SW] Some assets failed to cache:', err);
      });
    })
  );
  self.skipWaiting();
});

// Activate - clean old caches
self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((keys) => {
      return Promise.all(
        keys
          .filter((key) => key !== CACHE_NAME)
          .map((key) => caches.delete(key))
      );
    })
  );
  self.clients.claim();
});

// Fetch - network-first for API, cache-first for static
self.addEventListener('fetch', (event) => {
  const { request } = event;
  const url = new URL(request.url);

  // Skip non-GET requests
  if (request.method !== 'GET') return;

  // Skip WebSocket upgrade requests
  if (request.headers.get('Upgrade') === 'websocket') return;

  // Skip cross-origin requests
  if (url.origin !== self.location.origin) return;

  // Credentialed endpoints: hands off entirely. Not networkFirst — that still
  // writes a copy into the cache, which would be replayed the moment the server
  // is briefly unreachable, silently reinstating a stale sign-in state.
  if (NEVER_CACHE_PREFIXES.some((prefix) => url.pathname.startsWith(prefix))) {
    // Signing out is the one moment we know cached data belongs to a session
    // that is ending. Observed, not intercepted — the request itself still goes
    // straight to the network. `waitUntil` keeps the worker alive for the purge
    // even though the navigation is what the browser is really waiting on.
    if (url.pathname === '/oauth/logout') {
      event.waitUntil(purgeNonShell());
    }
    return;
  }

  // API calls: network-first with cache fallback
  if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/health/')) {
    event.respondWith(networkFirst(request));
    return;
  }

  // Fetch HTML on every visit so an installed shell can receive redesigns.
  if (request.mode === 'navigate') {
    event.respondWith(navigationFirst(request));
    return;
  }
  // Versioned assets can remain cache-first.
  if (STATIC_ASSET.test(url.pathname)) {
    event.respondWith(cacheFirst(request));
  }

  // Anything else is left alone and goes to the network as normal.
});

async function navigationFirst(request) {
  const url = new URL(request.url);
  const shell = ['/ui/', '/ui/index.html'].includes(url.pathname);
  if (shell) url.search = '';
  try {
    const response = await fetch(request);
    if (response.ok) {
      const cache = await caches.open(CACHE_NAME);
      await cache.put(url.toString(), response.clone());
    }
    return response;
  } catch {
    const cached = await caches.match(url.toString());
    if (cached) return cached;
    return new Response('Offline. Start the RuView server and reload.', { status: 503 });
  }
}

async function cacheFirst(request) {
  const url = new URL(request.url);
  // Only normalize shell navigations. Sign-in redirects back to
  // `/ui/?signed_in=<ms>`, which should reuse the same shell entry. Static
  // assets keep their query strings so URLs such as `style.css?v=14` actually
  // invalidate an older cached version.
  const shellNavigation = request.mode === 'navigate'
    && ['/ui/', '/ui/index.html'].includes(url.pathname);
  const cached = shellNavigation
    ? await caches.match(request, { ignoreSearch: true })
    : await caches.match(request);
  if (cached) return cached;

  try {
    const response = await fetch(request);
    if (response.ok) {
      const cache = await caches.open(CACHE_NAME);
      // Keep query-bearing asset versions distinct; canonicalize only shell
      // redirects so auth timestamps do not create unbounded cache entries.
      if (shellNavigation) url.search = '';
      cache.put(url.toString(), response.clone());
    }
    return response;
  } catch {
    // Return offline fallback for HTML navigation
    if (request.headers.get('Accept')?.includes('text/html')) {
      const fallback = await caches.match('/index.html');
      if (fallback) return fallback;
    }
    return new Response('Offline', { status: 503, statusText: 'Service Unavailable' });
  }
}

/**
 * Network-only, with an explicit offline signal.
 *
 * This used to cache every successful `/api/` response and replay it whenever
 * the network failed. Two things are wrong with that now:
 *
 * 1. **Authorization.** API responses are per-user once auth is on, but the
 *    cache is keyed by URL alone and nothing purges it at sign-out. Sign in as
 *    A, load sensing data, sign out, sign in as B, lose the network — B is
 *    served A's data with no authorization check at all. That is the same
 *    defect class as the cached `/oauth/status`: the Cache API happily outlives
 *    the session that produced its contents.
 * 2. **Correctness.** This is a live sensing dashboard. Replaying a stale pose
 *    or presence reading as if it were current is its own defect — it can show
 *    a room as occupied after the person has left.
 *
 * The offline shell (HTML/CSS/JS) is still cached; only the data is not. If
 * offline data replay is wanted back, it needs a per-session cache key and a
 * purge on sign-out, not a URL-keyed shared cache.
 */
async function networkFirst(request) {
  try {
    return await fetch(request);
  } catch {
    return new Response(JSON.stringify({ error: 'offline' }), {
      status: 503,
      headers: { 'Content-Type': 'application/json' }
    });
  }
}

/**
 * Drop everything except the static shell.
 *
 * Called when the user signs out. Belt-and-braces: nothing user-specific should
 * be in the cache after the `networkFirst` change above, but a cache populated
 * by an OLDER worker on this browser can still hold API responses, and that
 * worker's entries survive into this one under the same name.
 */
async function purgeNonShell() {
  const cache = await caches.open(CACHE_NAME);
  const keys = await cache.keys();
  await Promise.all(
    keys
      .filter((req) => {
        const p = new URL(req.url).pathname;
        return p.startsWith('/api/') || p.startsWith('/health/');
      })
      .map((req) => cache.delete(req))
  );
}
