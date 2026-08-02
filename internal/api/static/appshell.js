/* notiphy installed-PWA shell.
 *
 * This is what the Home Screen icon opens. It shows what is waiting on you,
 * what is running, and — importantly — repairs its own push subscription,
 * because iOS silently expires them after a few weeks of not opening the app.
 */

(function () {
  'use strict';

  var el = {
    conn: document.getElementById('conn'),
    subState: document.getElementById('sub-state'),
    enableCard: document.getElementById('enable-card'),
    enableBtn: document.getElementById('enable'),
    enableStatus: document.getElementById('enable-status'),
    pending: document.getElementById('pending'),
    activities: document.getElementById('activities')
  };

  function isIOS() {
    return /iphone|ipad|ipod/i.test(navigator.userAgent) ||
      (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function urlBase64ToUint8Array(base64String) {
    var padding = '='.repeat((4 - (base64String.length % 4)) % 4);
    var base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
    var raw = window.atob(base64);
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; ++i) out[i] = raw.charCodeAt(i);
    return out;
  }

  /* The badge is the closest free analogue to a Live Activity glance: it
   * persists on the Home Screen icon without the app running. */
  function setBadge(n) {
    if (!('setAppBadge' in navigator)) return;
    if (n > 0) {
      navigator.setAppBadge(n).catch(function () {});
    } else {
      navigator.clearAppBadge().catch(function () {});
    }
  }

  function renderPending(list) {
    if (!list.length) {
      el.pending.innerHTML = '<p class="empty">Nothing right now.</p>';
      return;
    }
    el.pending.innerHTML = list.map(function (a) {
      var buttons = a.type === 'text'
        ? '<a class="btn btn-primary" style="width:100%; text-align:center" href="' + esc(a.approvalUrl) + '">Reply</a>'
        : '<div class="btn-row">' +
            '<button class="btn-primary" data-answer="' + (a.type === 'yes_no' ? 'yes' : 'approve') + '" data-secret="' + esc(a.secret) + '">' +
              (a.type === 'yes_no' ? 'Yes' : 'Approve') + '</button>' +
            '<button class="btn-danger" data-answer="' + (a.type === 'yes_no' ? 'no' : 'deny') + '" data-secret="' + esc(a.secret) + '">' +
              (a.type === 'yes_no' ? 'No' : 'Deny') + '</button>' +
          '</div>';

      return '<div class="card ask">' +
        '<h3>' + esc(a.title) + '</h3>' +
        '<p>' + esc(a.body) + '</p>' +
        buttons +
      '</div>';
    }).join('');

    // Answering in-app is one tap, same as a native action button would be.
    Array.prototype.forEach.call(el.pending.querySelectorAll('button[data-answer]'), function (b) {
      b.addEventListener('click', function () {
        var secret = b.getAttribute('data-secret');
        var answer = b.getAttribute('data-answer');
        b.disabled = true;
        b.textContent = '…';
        fetch('/a/' + secret + '/' + answer, { method: 'POST' })
          .then(function () { refresh(); })
          .catch(function () {
            b.disabled = false;
            b.textContent = answer;
          });
      });
    });
  }

  function renderActivities(list) {
    if (!list.length) {
      el.activities.innerHTML = '<p class="empty">Nothing running.</p>';
      return;
    }
    el.activities.innerHTML = list.map(function (a) {
      var pct = Math.round((a.progress || 0) * 100);
      return '<div class="card">' +
        '<div class="row"><strong>' + esc(a.title) + '</strong>' +
        '<span class="tiny">' + pct + '%</span></div>' +
        '<div class="bar"><i style="width:' + pct + '%"></i></div>' +
        '<span class="tiny">' + esc(a.status || '') + '</span> ' +
        '<a class="tiny" style="float:right" href="' + esc(a.liveUrl) + '">open</a>' +
      '</div>';
    }).join('');
  }

  function refresh() {
    return fetch('/api/app/state', { headers: { Accept: 'application/json' } })
      .then(function (r) {
        if (!r.ok) throw new Error('state ' + r.status);
        return r.json();
      })
      .then(function (s) {
        el.conn.textContent = '';
        renderPending(s.pending || []);
        renderActivities(s.activities || []);
        setBadge(s.pendingCount || 0);

        if (s.subscribed) {
          el.enableCard.classList.add('hide');
          el.subState.textContent = 'Notifications on.';
        } else {
          el.enableCard.classList.remove('hide');
          el.subState.textContent = 'Notifications are off.';
        }
        return s;
      })
      .catch(function (err) {
        el.conn.textContent = 'offline';
        el.subState.textContent = 'Could not reach the server.';
        throw err;
      });
  }

  /* Subscribe, or silently repair an expired subscription.
   *
   * Called on every launch — not just when the user taps Enable — because the
   * failure mode this guards against is invisible: iOS drops the subscription,
   * pushes stop arriving, and nothing tells you until you notice you missed
   * an approval. */
  function ensureSubscribed(interactive) {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
      if (interactive) {
        el.enableStatus.textContent =
          'This install cannot receive Web Push. On iOS, open notiphy from the Home Screen icon.';
      }
      return Promise.resolve(false);
    }

    return navigator.serviceWorker.register('/sw.js', { scope: '/' })
      .then(function () { return navigator.serviceWorker.ready; })
      .then(function (reg) {
        return reg.pushManager.getSubscription().then(function (existing) {
          if (existing) return { reg: reg, sub: existing, fresh: false };

          // Only prompt for permission on an explicit tap; a silent repair
          // must not throw a permission dialog at someone who just opened
          // the app to read a notification.
          if (Notification.permission !== 'granted') {
            if (!interactive) return { reg: reg, sub: null, fresh: false };
            return Notification.requestPermission().then(function (p) {
              if (p !== 'granted') throw new Error('permission ' + p);
              return { reg: reg, sub: null, fresh: true };
            });
          }
          return { reg: reg, sub: null, fresh: true };
        });
      })
      .then(function (ctx) {
        if (!ctx || !ctx.reg) return false;
        if (ctx.sub) return postSubscription(ctx.sub);

        if (Notification.permission !== 'granted') return false;
        return fetch('/api/app/key')
          .then(function (r) {
            if (!r.ok) throw new Error('server has no VAPID key');
            return r.json();
          })
          .then(function (cfg) {
            return ctx.reg.pushManager.subscribe({
              userVisibleOnly: true,
              applicationServerKey: urlBase64ToUint8Array(cfg.publicKey)
            });
          })
          .then(postSubscription);
      })
      .catch(function (err) {
        if (interactive) el.enableStatus.textContent = 'Failed: ' + err.message;
        return false;
      });
  }

  function postSubscription(sub) {
    var payload = sub.toJSON();
    payload.name = (isIOS() ? 'iPhone' : 'Device') + ' (notiphy app)';
    payload.platform = isIOS() ? 'ios' : 'web';
    return fetch('/api/app/subscribe', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(function (r) { return r.ok; });
  }

  if (el.enableBtn) {
    el.enableBtn.addEventListener('click', function () {
      el.enableBtn.disabled = true;
      el.enableStatus.textContent = 'Enabling…';
      ensureSubscribed(true).then(function (ok) {
        el.enableBtn.disabled = false;
        if (ok) {
          el.enableStatus.textContent = '';
          refresh();
        }
      });
    });
  }

  // Repair the subscription and refresh whenever the app comes to the front,
  // which on iOS is the only reliable moment we get to run any code at all.
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) {
      refresh().catch(function () {});
      ensureSubscribed(false);
    }
  });

  refresh()
    .then(function () { return ensureSubscribed(false); })
    .catch(function () {});

  // A slow poll keeps the list current while the app is open without leaning
  // on push, which iOS may coalesce or delay.
  setInterval(function () {
    if (!document.hidden) refresh().catch(function () {});
  }, 15000);
})();
