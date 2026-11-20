// Service worker for the kid app: network first, falls back to the last
// cached copy of the kid page so the app still opens without connection.
const CACHE = "zockerzeit-kid-v1";
const SHELL = [
  "/static/app.css",
  "/static/kid.js",
  "/static/favicon.svg",
  "/static/fonts/bricolage-grotesque-latin-wght-normal.woff2",
  "/static/fonts/atkinson-hyperlegible-latin-400-normal.woff2",
  "/static/fonts/atkinson-hyperlegible-latin-700-normal.woff2",
];

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  event.respondWith(
    fetch(req)
      .then((res) => {
        if (res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy));
        }
        return res;
      })
      .catch(() => caches.match(req).then((hit) => hit || Response.error()))
  );
});
