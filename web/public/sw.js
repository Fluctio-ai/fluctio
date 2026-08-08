// Fluctio PWA service worker — minimal pass-through.
//
// Registered to satisfy Chromium's installability criteria (a service
// worker with a fetch event listener). It caches nothing and never calls
// respondWith, so every request goes straight to the network and app
// updates are never blocked by stale cached assets. Safe to ship as-is.
self.addEventListener('install', () => {
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('fetch', () => {
  // Intentionally empty: let the browser handle every request normally.
});
