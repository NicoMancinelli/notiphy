-- notiphy schema. All timestamps are unix seconds (INTEGER) so comparisons and
-- expiry sweeps are plain integer math.

CREATE TABLE IF NOT EXISTS accounts (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tokens (
    id         TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    token      TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    revoked    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tokens_account ON tokens(account_id);

CREATE TABLE IF NOT EXISTS devices (
    id         TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name       TEXT NOT NULL DEFAULT '',
    transport  TEXT NOT NULL,
    platform   TEXT NOT NULL DEFAULT 'other',
    config     TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    last_seen  INTEGER,
    disabled   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_devices_account ON devices(account_id, disabled);

CREATE TABLE IF NOT EXISTS events (
    id         TEXT PRIMARY KEY,
    account_id TEXT NOT NULL,
    token_id   TEXT NOT NULL,
    title      TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL DEFAULT '',
    image_url  TEXT NOT NULL DEFAULT '',
    url        TEXT NOT NULL DEFAULT '',
    priority   INTEGER NOT NULL DEFAULT 3,
    delivered  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_account_created ON events(account_id, created_at DESC);

CREATE TABLE IF NOT EXISTS responses (
    id             TEXT PRIMARY KEY,
    event_id       TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    type           TEXT NOT NULL,
    status         TEXT NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    answer         TEXT NOT NULL DEFAULT '',
    answered_by    TEXT NOT NULL DEFAULT '',
    callback_url   TEXT NOT NULL DEFAULT '',
    callback_token TEXT NOT NULL DEFAULT '',
    secret         TEXT NOT NULL UNIQUE,
    expires_at     INTEGER NOT NULL,
    answered_at    INTEGER,
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_responses_event ON responses(event_id);
CREATE INDEX IF NOT EXISTS idx_responses_pending ON responses(status, expires_at);

CREATE TABLE IF NOT EXISTS activities (
    id                     TEXT PRIMARY KEY,
    account_id             TEXT NOT NULL,
    token_id               TEXT NOT NULL,
    key                    TEXT NOT NULL DEFAULT '',
    title                  TEXT NOT NULL DEFAULT '',
    status                 TEXT NOT NULL DEFAULT '',
    progress               REAL,
    symbol                 TEXT NOT NULL DEFAULT '',
    accent_color           TEXT NOT NULL DEFAULT '',
    style                  TEXT NOT NULL DEFAULT 'standard',
    state                  TEXT NOT NULL DEFAULT 'active',
    seq                    INTEGER NOT NULL DEFAULT 0,
    last_notified_progress REAL NOT NULL DEFAULT 0,
    last_notified_status   TEXT NOT NULL DEFAULT '',
    expires_at             INTEGER NOT NULL,
    stale_at               INTEGER NOT NULL,
    created_at             INTEGER NOT NULL,
    updated_at             INTEGER NOT NULL,
    ended_at               INTEGER
);
CREATE INDEX IF NOT EXISTS idx_activities_account ON activities(account_id, state);
-- One live activity per key per account, matching Hark's "one per device" rule
-- while still allowing distinct keys (deploy, tests, ...) to coexist.
CREATE UNIQUE INDEX IF NOT EXISTS idx_activities_key_active
    ON activities(account_id, key) WHERE state = 'active' AND key <> '';

CREATE TABLE IF NOT EXISTS deliveries (
    id          TEXT PRIMARY KEY,
    event_id    TEXT NOT NULL DEFAULT '',
    activity_id TEXT NOT NULL DEFAULT '',
    device_id   TEXT NOT NULL,
    transport   TEXT NOT NULL,
    ok          INTEGER NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deliveries_event ON deliveries(event_id);

-- Idempotency-Key support. payload_hash lets a replay with a *different* body
-- return 409 while an identical replay returns the original response.
CREATE TABLE IF NOT EXISTS idempotency (
    token_id     TEXT NOT NULL,
    key          TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    status_code  INTEGER NOT NULL,
    response     TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (token_id, key)
);

-- Outbound response callbacks, retried with backoff.
CREATE TABLE IF NOT EXISTS callbacks (
    id          TEXT PRIMARY KEY,
    response_id TEXT NOT NULL,
    url         TEXT NOT NULL,
    token       TEXT NOT NULL DEFAULT '',
    payload     TEXT NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    next_at     INTEGER NOT NULL,
    done        INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_callbacks_due ON callbacks(done, next_at);
