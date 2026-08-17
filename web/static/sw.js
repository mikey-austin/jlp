// JLP PWA shell service worker.
//
// Cache name is versioned: jlp-shell-v3. Bump the suffix whenever the
// precached shell assets below change so the activate handler evicts
// the stale cache on the next load (later tasks must respect this).
// v3: Phase 4 Task 8 adds record.js.
// v4: Phase 4 Task D changes app.js (the delete-confirmation dialog)
// and components.css (its styles), both precached below. Without this
// bump an existing learner keeps the v3 copies and their 削除 buttons
// do nothing at all — the dialog handler is in the app.js they never
// re-fetch. Found by browser verification, not by reasoning: the
// buttons rendered and were simply inert until the cache was cleared.
// v5: components.css gains .icon-btn (the logout glyph fix) and the
// delete triggers become .btn--danger. This bump was forgotten when
// .icon-btn shipped, so returning learners were served new markup
// against the v4 stylesheet and the logout button rendered with no
// styling at all — worse than the blank circle it was fixing. Forgetting
// it is now a test failure, not a matter of memory: see
// TestServiceWorkerCacheNameTracksPrecachedAssets.
const CACHE_NAME = "jlp-shell-v5";

const PRECACHE_URLS = [
  "/offline",
  "/static/fonts/fonts.css",
  "/static/fonts/instrument-sans-latin-400-normal.woff2",
  "/static/fonts/instrument-sans-latin-500-normal.woff2",
  "/static/fonts/instrument-sans-latin-600-normal.woff2",
  "/static/fonts/instrument-sans-latin-700-normal.woff2",
  "/static/fonts/jetbrains-mono-latin-400-normal.woff2",
  "/static/fonts/jetbrains-mono-latin-500-normal.woff2",
  "/static/fonts/jetbrains-mono-latin-700-normal.woff2",
  "/static/css/tokens.css",
  "/static/css/components.css",
  "/static/css/app.css",
  "/static/js/htmx.min.js",
  "/static/js/alpine.min.js",
  "/static/js/theme.js",
  "/static/js/app.js",
  "/static/js/record.js",
  "/static/manifest.webmanifest",
  "/static/icons/icon.svg",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => cache.addAll(PRECACHE_URLS))
  );
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((names) =>
        Promise.all(
          names
            .filter((name) => name !== CACHE_NAME)
            .map((name) => caches.delete(name))
        )
      )
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (event) => {
  const { request } = event;

  // Network-first for navigations, falling back to the cached offline
  // shell when the network is unavailable.
  if (request.mode === "navigate") {
    event.respondWith(
      fetch(request).catch(() => caches.match("/offline"))
    );
    return;
  }

  // Cache-first for static assets.
  const url = new URL(request.url);
  if (url.pathname.startsWith("/static/")) {
    event.respondWith(
      caches.match(request).then((cached) => cached || fetch(request))
    );
  }
});
