# notiphy

[![CI](https://github.com/NicoMancinelli/notiphy/actions/workflows/ci.yml/badge.svg)](https://github.com/NicoMancinelli/notiphy/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/NicoMancinelli/notiphy.svg)](https://pkg.go.dev/github.com/NicoMancinelli/notiphy)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Self-hosted webhooks → phone notifications, approvals, and Live Activities.
A free, single-binary alternative to [Hark](https://hark.ryan.ceo), with the
same API and nothing behind a paywall.

```bash
curl -X POST https://notiphy.example/hooks/whk_… \
  -H 'Content-Type: application/json' \
  -d '{"title":"Deploy","body":"Build finished"}'
```

---

## Read this first: what Apple lets you have for free

notiphy is free and self-hosted. Apple's push platform is not, and it is worth
being blunt about where that bites:

- **A free/personal Apple team cannot use push notifications at all.** The
  `aps-environment` entitlement is Apple Developer Program-only. There is no
  sideloading trick around it — a custom iOS app on a free account receives
  nothing.
- **iOS Web Push works with no Apple account**, but WebKit silently ignores
  notification action buttons, and the web platform has no Live Activity API.

So: **Live Activities, the Dynamic Island, and native one-tap buttons on iPhone
require the $99/yr Apple Developer Program.** Anything claiming otherwise is
wrong.

What notiphy does about it is make the server the product and the delivery
mechanism swappable, so the free path works today and the paid path is a config
change rather than a rewrite.

| | ntfy (iOS) | Web Push PWA (iOS) | ntfy (Android) | notiphy iOS app + APNs |
|---|---|---|---|---|
| Push notification | ✅ | ✅ | ✅ | ✅ |
| Title / body / image / tap-URL | ✅ | ✅ | ✅ | ✅ |
| One-tap Approve/Deny | in the app | in the app | ✅ native | ✅ native |
| Live Activity / Dynamic Island | ❌ → live web page | ❌ → live web page | n/a | ✅ |
| No third party in the loop | ❌ ntfy.sh poll relay | ✅ fully self-hosted | ✅ | ✅ |
| Cost | free | free | free | $99/yr |

**Approvals still work on iPhone for free.** Install the PWA to your Home
Screen and you get an app that lists everything waiting on you with Approve /
Deny right there — one tap, same as native. Coming from a notification costs one
extra tap versus Hark, because WebKit will not render action buttons on the
notification itself.

The Home Screen icon also carries a **badge** with the number of pending
approvals, which is the closest free stand-in for a Live Activity glance.

---

## Install

### Docker

```bash
docker run -d --name notiphy -p 8080:8080 -v ./data:/data \
  -e NOTIPHY_BASE_URL=http://localhost:8080 \
  ghcr.io/nicomancinelli/notiphy:latest
```

Or with compose, which documents every option inline:

```bash
docker compose up -d
```

Images are multi-arch (`linux/amd64`, `linux/arm64`), so a Raspberry Pi or an
arm64 LXC works the same as an x86 box.

### Prebuilt binary

Grab a tarball from [Releases](https://github.com/NicoMancinelli/notiphy/releases)
— static, no dependencies, nothing to install alongside it:

```bash
tar -xzf notiphy_v0.0.1_linux_amd64.tar.gz
./notiphy serve --base-url http://localhost:8080
```

### From source

```bash
go install github.com/NicoMancinelli/notiphy/cmd/notiphy@latest
notiphy serve --base-url http://localhost:8080
```

## Quick start

Then:

1. Open the dashboard at `http://localhost:8080/`.
2. **Add a device** — visit `/subscribe` *on your phone* and pick either:
   - **Web Push** — no app, no Apple account, nothing third-party. On iOS you
     must Add to Home Screen first; Apple only grants Web Push to installed PWAs.
   - **ntfy** — install the ntfy app, subscribe it to a topic, register that
     topic. Android gets real one-tap Approve/Deny buttons this way.
3. **Create a webhook token** on the dashboard, or:

```bash
./notiphy token create --name github-actions
```

4. Send something:

```bash
export NOTIPHY_URL=http://localhost:8080
export NOTIPHY_TOKEN=whk_…
./notiphy send "Build finished" --title Deploy
```

---

## Approve Claude Code from your phone

The reason this exists. Your agent asks, your phone answers, the agent
continues — you never went back to the terminal.

```bash
notiphy hook --print-config
```

That prints a snippet for `~/.claude/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash|Write|Edit",
        "hooks": [{ "type": "command", "command": "notiphy hook --timeout 5m" }]
      }
    ]
  }
}
```

**What actually leaves your machine:** the agent name, the tool name, the
project directory, and how many arguments the call had. That is all. Command
text, file contents, diffs, URLs, and environment never go anywhere — a
notification lands on a lock screen, and it is not a secure channel.

> `Claude Code wants to run Bash in notiphy (2 arguments)`

**It fails open.** If notiphy is unreachable the hook returns `ask`, so Claude
Code falls back to its normal terminal prompt instead of hanging. A notification
server going down should never wedge your agent.

Use it standalone too:

```bash
notiphy ask "Deploy to prod?" --approval --wait --timeout 15m
echo $?   # 0 approved · 2 denied · 4 timed out · 7 no devices registered
```

---

## The app ("notiphy-lite")

Add notiphy to your Home Screen from `/subscribe` and it behaves like an app:

- **Waiting on you** — every pending approval, answerable in one tap
- **Running** — live activities with progress
- **Badge** on the icon for the pending count
- **Self-repairing subscription** — iOS silently expires Web Push
  subscriptions after a few weeks of not opening the app, so the shell
  re-registers on every launch rather than going quietly dead

No Apple Developer account, no App Store, nothing third-party. What it cannot
do is render a Lock Screen Live Activity or put buttons on the notification
itself — those need the native app.

## Live Activities

The state machine is real regardless of what your devices can render — start,
update, and end always succeed and are stored server-side.

```bash
notiphy activity start --key deploy --style ring --title "Deploy #184" --status Building --progress 0.1
notiphy activity update --key deploy --status Testing --progress 0.6
notiphy activity end    --key deploy --status Shipped --progress 1
```

- **With APNs:** a genuine ActivityKit card, all nine Hark layouts, Dynamic Island.
- **Without it:** `/live/:id` is a full-screen page that updates over SSE. The
  notification taps through to it, so you watch the deploy progress live in
  Safari. Notifications fire only on start, end, a status change, or a progress
  jump of at least `activity_progress_step` — otherwise a build reporting 1% at a
  time would bury you.

---

## API

Wire-compatible with Hark, so existing payloads and docs port over unchanged.
Nothing is gated — there is no `402`.

| Method | Path | |
|---|---|---|
| POST | `/hooks/:token` | notify; only `body` is required |
| POST | `/hooks/:token/live-activities` | start → `201` |
| PATCH | `/hooks/:token/live-activities/:id` | merge-update |
| POST | `/hooks/:token/live-activities/:id/end` | end |
| GET | `/hooks/:token/events/:eventId` | poll a response |
| POST | `/hooks/:token/events/:eventId/cancel` | withdraw |

```jsonc
{
  "body": "Build finished",          // required
  "title": "Deploy",
  "imageUrl": "https://…",
  "url": "https://…",                // tap target
  "priority": 4,                     // 1-5; 4+ breaks through Focus
  "deviceIds": ["dev_…"],            // default: all enabled devices
  "response": {                      // ask a question
    "type": "approval",              // approval | yes_no | text
    "expiresInSeconds": 900,
    "correlationId": "your-ref",
    "callback": { "url": "https://…", "token": "bearer-token" }
  }
}
```

**Idempotency.** Send an `Idempotency-Key` header (1–200 chars). An identical
replay returns the original result with `"idempotent": true` and pushes nothing;
the same key with a different payload returns `409` rather than silently
diverging.

**Callbacks** are queued in SQLite and retried five times, from immediate out to
an hour, surviving restarts — the point is that your CI job can stop waiting.

**Pushes are retried too**, five attempts from 5 seconds out to 10 minutes. A
transient ntfy outage should not silently drop an approval and leave an agent
waiting on a notification that never arrives. Retries stop early if the question
has already been answered elsewhere.

**Rate limiting** is off by default — this is your server. Set
`rate_limit_per_minute` to enable Hark-compatible `429` responses with
`Retry-After`.

Status codes match Hark: `200 201 202 400 404 409 429 502`.

---

## Exposing it

notiphy has two kinds of endpoint, and they want different treatment:

- **Capability URLs** — `/hooks/:token`, `/a/:secret`, `/live/:id`. The secret
  in the path *is* the credential. Safe to hand to CI or embed in a notification.
- **Operator surface** — the dashboard, `/subscribe`, `/api/*`. These add
  devices and mint tokens, so they cannot carry a capability token of their own.

**Set `admin_token` before exposing notiphy beyond a private network.** Without
one, anyone who can reach the server can register their own device and start
receiving your notifications. The server warns loudly at boot.

### Tailscale (recommended)

Tailnet-only — nothing is reachable from the internet:

```bash
tailscale serve --bg 8080
```

Public HTTPS ingress, for webhooks from GitHub Actions and friends:

```bash
tailscale funnel --bg 8080
```

Set `base_url` to the resulting `https://<host>.<tailnet>.ts.net`. If you use
Funnel, set `admin_token` too.

---

## Configuration

Flags > environment (`NOTIPHY_*`) > YAML file > defaults.
See [`notiphy.example.yaml`](notiphy.example.yaml) for every option.

The one that matters most is **`base_url`**: notifications embed absolute links
back to the server, so if it is wrong your approvals tap through to nothing.

---

## Turning on Live Activities later

Once you have an Apple Developer Program membership:

1. Build and install the notiphy iOS app (ActivityKit widget extension, plus a
   notification service extension for images and `UNNotificationCategory`
   actions for one-tap approve/deny).
2. Set `apns_key_file`, `apns_key_id`, `apns_team_id`, and `apns_topic`.
3. Restart.

Nothing else changes. The router notices a transport whose capabilities outrank
the others and starts preferring it; every caller's payload stays identical.

---

## Architecture

```
notiphy (one static binary, ~20 MB)
├── API          Hark-compatible webhook endpoints
├── Web          embedded: dashboard, approval page, live view, PWA
├── Transports   ntfy · webpush · apns   (pluggable, capability-negotiated)
├── Store        SQLite, single file, pure-Go driver (CGO_ENABLED=0)
└── CLI          serve · send · ask · activity · token · device · hook
```

Everything hangs off one interface in
[`internal/transport/transport.go`](internal/transport/transport.go). The router
reads each transport's `Caps()` and degrades explicitly: no button support means
the approval becomes a tap-through link; no Live Activity support means the
activity becomes a live web page plus throttled milestone pushes. Callers never
change their payload.

```bash
go test ./...
```

---

## License

MIT
