// The service worker exists so the board can be installed, and so that opening
// it with no network says so in the board's own words rather than in the
// browser's error page.
//
// It deliberately caches nothing but the shell. Every card, every column and
// every badge comes from the server, so a cached page would be a board showing
// yesterday's work with no way to tell. Network first, and the only thing the
// cache ever answers is the offline page.
// The name carries the build's asset digest, filled in by the server. A worker
// is reinstalled only when its own bytes change, and this file's did not
// between releases, so the offline page stayed whatever it was the first time
// it was cached: forever, across every later deploy.
const SHELL = 'kanban-shell-__ASSET_VERSION__';
const OFFLINE = '/offline';

self.addEventListener('install', (e) => {
  // Fetched before the cache is opened. caches.open creates the cache whether
  // or not what goes in it arrives, and an install that fails is discarded
  // along with the worker that would have cleaned up after it, so a board
  // whose offline page answered 403 left an empty cache behind on every
  // attempt.
  e.waitUntil(
    fetch(OFFLINE)
      .then((res) => {
        if (!res.ok) { throw new Error('offline page: ' + res.status); }
        return caches.open(SHELL).then((c) => c.put(OFFLINE, res));
      })
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((names) => Promise.all(names.filter((n) => n !== SHELL).map((n) => caches.delete(n))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (e) => {
  // Only a page the user navigated to. A failed API call or a missing asset is
  // the app's problem to report, not something to answer with a page.
  if (e.request.mode !== 'navigate') { return; }
  e.respondWith(fetch(e.request).catch(() => caches.match(OFFLINE)));
});
