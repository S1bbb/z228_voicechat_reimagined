// Service worker нужен браузеру, чтобы чат можно было «установить» как приложение.
// Ничего не кэшируем: всегда берём свежую версию с сервера, чтобы обновления доезжали сразу.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));
self.addEventListener("fetch", () => {});
