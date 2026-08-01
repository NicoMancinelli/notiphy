/* notiphy device registration (subscribe page). */

(function () {
  'use strict';

  var statusEl = document.getElementById('status');

  function say(el, msg, kind) {
    el.textContent = msg;
    el.style.color = kind === 'err' ? 'var(--err)' :
                     kind === 'ok' ? 'var(--ok-fg)' : 'var(--muted)';
  }

  /* iOS grants Web Push only to a site launched from the Home Screen. Detect
   * that up front so we prompt to install rather than failing at subscribe. */
  function isIOS() {
    return /iphone|ipad|ipod/i.test(navigator.userAgent) ||
      (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
  }

  function isStandalone() {
    return window.navigator.standalone === true ||
      window.matchMedia('(display-mode: standalone)').matches;
  }

  function show(id) {
    var el = document.getElementById(id);
    if (el) el.classList.remove('hide');
  }

  var supported = 'serviceWorker' in navigator && 'PushManager' in window;

  // Order matters here. iOS Safari does not expose PushManager at all until the
  // site has been added to the Home Screen, so a plain feature test reports
  // "unsupported" on exactly the devices that need the install instructions.
  // Checking for iOS first is what turns a dead end into a next step.
  if (isIOS() && !isStandalone()) {
    show('ios-install');
  } else if (!supported) {
    show('wp-unsupported');
  } else {
    show('wp-ui');
  }

  /* base64url -> Uint8Array, the format applicationServerKey requires. */
  function urlBase64ToUint8Array(base64String) {
    var padding = '='.repeat((4 - (base64String.length % 4)) % 4);
    var base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
    var raw = window.atob(base64);
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; ++i) out[i] = raw.charCodeAt(i);
    return out;
  }

  var btn = document.getElementById('wp-btn');
  if (btn) {
    btn.addEventListener('click', function () {
      btn.disabled = true;
      say(statusEl, 'Requesting permission…');

      Notification.requestPermission().then(function (permission) {
        if (permission !== 'granted') {
          throw new Error('Notification permission was ' + permission);
        }
        return navigator.serviceWorker.register('/sw.js', { scope: '/' });
      })
      .then(function (reg) {
        say(statusEl, 'Registering…');
        return navigator.serviceWorker.ready.then(function () { return reg; });
      })
      .then(function (reg) {
        return fetch('/api/webpush/key')
          .then(function (r) {
            if (!r.ok) throw new Error('server has no VAPID key configured');
            return r.json();
          })
          .then(function (cfg) {
            return reg.pushManager.subscribe({
              userVisibleOnly: true,
              applicationServerKey: urlBase64ToUint8Array(cfg.publicKey)
            });
          });
      })
      .then(function (sub) {
        var payload = sub.toJSON();
        payload.name = (isIOS() ? 'iPhone' : 'Browser') + ' (Web Push)';
        payload.platform = isIOS() ? 'ios' : 'web';
        return fetch('/api/webpush/subscribe', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
      })
      .then(function (r) {
        return r.json().then(function (body) {
          if (!r.ok) throw new Error(body.error || 'registration failed');
          return body;
        });
      })
      .then(function (body) {
        say(statusEl, 'Done — this device is registered (' + body.deviceId + ').', 'ok');
        btn.textContent = 'Registered';
      })
      .catch(function (err) {
        say(statusEl, 'Failed: ' + err.message, 'err');
        btn.disabled = false;
      });
    });
  }

  /* ntfy registration. */
  var form = document.getElementById('ntfy-form');
  if (form) {
    var ntfyStatus = document.getElementById('ntfy-status');
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var topic = document.getElementById('ntfy-topic').value.trim();
      var server = document.getElementById('ntfy-server').value.trim();
      var name = document.getElementById('ntfy-name').value.trim();

      if (!topic) {
        say(ntfyStatus, 'A topic is required.', 'err');
        return;
      }

      say(ntfyStatus, 'Registering…');
      fetch('/api/devices', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: name || 'ntfy device',
          transport: 'ntfy',
          platform: isIOS() ? 'ios' : 'android',
          config: { topic: topic, server: server }
        })
      })
      .then(function (r) {
        return r.json().then(function (body) {
          if (!r.ok) throw new Error(body.error || 'registration failed');
          return body;
        });
      })
      .then(function (body) {
        say(ntfyStatus, 'Registered (' + body.device.id + '). Sending a test…', 'ok');
        return fetch('/api/devices/' + body.device.id + '/test', { method: 'POST' });
      })
      .then(function (r) {
        if (!r.ok) {
          return r.json().then(function (b) { throw new Error(b.error || 'test push failed'); });
        }
        say(ntfyStatus, 'Registered. Check your phone for the test notification.', 'ok');
      })
      .catch(function (err) {
        say(ntfyStatus, 'Failed: ' + err.message, 'err');
      });
    });
  }
})();
