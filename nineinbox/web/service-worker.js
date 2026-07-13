const STATIC_CACHE = "nine-inbox-static-v1";
const SHARE_CACHE = "nine-inbox-shares-v1";
const STATIC_ASSETS = [
  "/inbox/", "/inbox/app.css", "/inbox/app.mjs", "/inbox/crypto.mjs",
  "/inbox/storage.mjs", "/inbox/qr.mjs", "/inbox/icon.svg", "/inbox/manifest.webmanifest",
];

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(STATIC_CACHE).then((cache) => cache.addAll(STATIC_ASSETS)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (event) => {
  event.waitUntil(caches.keys().then((keys) => Promise.all(keys.filter((key) => key.startsWith("nine-inbox-static-") && key !== STATIC_CACHE).map((key) => caches.delete(key)))).then(() => self.clients.claim()));
});

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (event.request.method === "POST" && url.pathname === "/inbox/share") {
    event.respondWith((async () => {
      const form = await event.request.formData();
      const file = form.get("files");
      const text = [form.get("title"), form.get("text"), form.get("url")].filter(Boolean).join("\n");
      const headers = new Headers();
      let body = text;
      if (file instanceof File && file.size > 0) {
        body = file;
        headers.set("X-Nine-Share-Kind", "file");
        headers.set("X-Nine-Share-Name", encodeURIComponent(file.name));
        headers.set("Content-Type", file.type || "application/octet-stream");
      } else {
        headers.set("X-Nine-Share-Kind", "text");
        headers.set("Content-Type", "text/plain; charset=utf-8");
      }
      const cache = await caches.open(SHARE_CACHE);
      await cache.put("/inbox/__pending-share", new Response(body, { headers }));
      return Response.redirect("/inbox/?share=1", 303);
    })());
    return;
  }
  if (event.request.method !== "GET" || url.origin !== location.origin || url.pathname.startsWith("/api/")) return;
  event.respondWith(caches.match(event.request).then((cached) => cached || fetch(event.request).then((response) => {
    if (response.ok && url.pathname.startsWith("/inbox/")) {
      const copy = response.clone();
      caches.open(STATIC_CACHE).then((cache) => cache.put(event.request, copy));
    }
    return response;
  })));
});
