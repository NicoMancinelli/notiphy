/* notiphy service worker.
 *
 * Served from the origin root so its scope covers the whole site — a worker
 * under /static/ could only control /static/ and would never see these pushes.
 */

self.addEventListener('install', function () {
  // Take over immediately rather than waiting for existing tabs to close;
  // otherwise a fresh subscribe can land on a stale worker.
  self.skipWaiting();
});

self.addEventListener('activate', function (event) {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('push', function (event) {
  var data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch (e) {
    data = { title: 'notiphy', body: event.data ? event.data.text() : '' };
  }

  var options = {
    body: data.body || '',
    // Chrome/Android renders these; WebKit ignores them entirely, which is why
    // the notification body itself always taps through to the approval page.
    actions: (data.actions || []).map(function (a) {
      return { action: a.action, title: a.title };
    }),
    data: {
      url: data.url || '/',
      actions: data.actions || []
    },
    tag: data.tag || undefined,
    renotify: !!data.tag,
    requireInteraction: (data.priority || 3) >= 4,
    icon: '/static/icon-192.png',
    badge: '/static/icon-192.png'
  };

  if (data.image) {
    options.image = data.image;
  }

  event.waitUntil(
    self.registration.showNotification(data.title || 'notiphy', options)
  );
});

self.addEventListener('notificationclick', function (event) {
  var d = event.notification.data || {};
  event.notification.close();

  // An action button was tapped: answer directly without opening a window.
  if (event.action) {
    var match = (d.actions || []).filter(function (a) {
      return a.action === event.action;
    })[0];

    if (match && match.url) {
      event.waitUntil(
        fetch(match.url, {
          method: match.method || 'POST',
          body: match.body || undefined,
          headers: match.body ? { 'Content-Type': 'application/json' } : undefined
        }).catch(function () {
          // If the direct answer fails (offline, server unreachable), fall back
          // to opening the page so the decision is not silently lost.
          return self.clients.openWindow(d.url || '/');
        })
      );
      return;
    }
  }

  // Body tap: focus an existing tab on that URL if there is one, else open it.
  var target = d.url || '/';
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function (list) {
      for (var i = 0; i < list.length; i++) {
        if (list[i].url === target && 'focus' in list[i]) {
          return list[i].focus();
        }
      }
      return self.clients.openWindow(target);
    })
  );
});

/* A subscription can be rotated by the browser at any time — iOS does this
 * after long inactivity. Re-register so the device does not go quietly dead. */
self.addEventListener('pushsubscriptionchange', function (event) {
  event.waitUntil(
    fetch('/api/webpush/key')
      .then(function (r) { return r.json(); })
      .then(function (cfg) {
        return self.registration.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey: cfg.publicKey
        });
      })
      .then(function (sub) {
        return fetch('/api/webpush/subscribe', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(sub.toJSON())
        });
      })
  );
});
