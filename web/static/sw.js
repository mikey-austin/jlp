// JLP PWA shell service worker.
//
// Cache name is versioned: jlp-shell-v2. Bump the suffix whenever the
// precached shell assets below change so the activate handler evicts
// the stale cache on the next load (later tasks must respect this).
const CACHE_NAME = "jlp-shell-v2";

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
