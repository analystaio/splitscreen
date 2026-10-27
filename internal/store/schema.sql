-- Splitscreen gateway store.
--
-- The gateway is the only writer. SQLite is deliberate: the audit log and
-- routing state must not depend on a database living on the infrastructure the
-- gateway exists to help debug.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

-- A thread is sticky to a runner for its lifetime: session state and
-- transcripts live on that runner's disk, so a binding cannot be moved without
-- abandoning the session.
CREATE TABLE IF NOT EXISTS threads (
    id               TEXT PRIMARY KEY,
    surface          TEXT NOT NULL,
    channel          TEXT NOT NULL,
    runner           TEXT NOT NULL,
    session_id       TEXT,
    bundle_version   INTEGER NOT NULL DEFAULT 0,
    stale            INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    last_activity_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_threads_runner ON threads(runner);
CREATE INDEX IF NOT EXISTS idx_threads_channel ON threads(surface, channel);

-- One inbound message and the agentic work it triggers. The unit of accounting
-- and the join key between routing, audit, and cost.
CREATE TABLE IF NOT EXISTS turns (
    id                  TEXT PRIMARY KEY,
    thread_id           TEXT NOT NULL,
    channel             TEXT NOT NULL,
    runner              TEXT NOT NULL,
    surface_user        TEXT NOT NULL,
    session_id          TEXT,
    model               TEXT,

    -- Four counters, never collapsed: cache reads bill at a fraction of input
    -- and cache writes at a premium, so any summed figure is wrong by an order
    -- of magnitude on long threads.
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,

    cost_computed_usd   REAL,
    cost_reported_usd   REAL,
    price_table_version TEXT,
    ttl_hint            TEXT,

    -- usage_known = 0 means the adapter could not report usage. Rollups must
    -- count these separately; rendering them as zero is worse than nothing.
    usage_known         INTEGER NOT NULL DEFAULT 0,

    status              TEXT NOT NULL,
    error               TEXT,
    started_at          TEXT NOT NULL,
    ended_at            TEXT,
    duration_ms         INTEGER,
    num_tool_calls      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_turns_thread ON turns(thread_id);
CREATE INDEX IF NOT EXISTS idx_turns_runner_started ON turns(runner, started_at);
CREATE INDEX IF NOT EXISTS idx_turns_user ON turns(surface_user);

-- Generic audit stream. Anything not worth its own table lands here so the
-- record is complete by construction.
CREATE TABLE IF NOT EXISTS events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    at           TEXT NOT NULL,
    kind         TEXT NOT NULL,
    runner       TEXT,
    thread_id    TEXT,
    turn_id      TEXT,
    surface_user TEXT,
    detail       TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_at ON events(at);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events(kind, at);

CREATE TABLE IF NOT EXISTS permissions (
    request_id    TEXT PRIMARY KEY,
    thread_id     TEXT,
    turn_id       TEXT,
    runner        TEXT NOT NULL,
    tool          TEXT NOT NULL,
    input         TEXT,
    decision      TEXT,
    decided_by    TEXT,
    policy_denied INTEGER NOT NULL DEFAULT 0,
    reason        TEXT,
    requested_at  TEXT NOT NULL,
    decided_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_permissions_turn ON permissions(turn_id);

CREATE TABLE IF NOT EXISTS blobs (
    id           TEXT PRIMARY KEY,
    direction    TEXT NOT NULL,
    thread_id    TEXT,
    turn_id      TEXT,
    runner       TEXT,
    name         TEXT,
    mime         TEXT,
    size         INTEGER,
    sha256       TEXT,
    surface_user TEXT,
    ok           INTEGER,
    error        TEXT,
    at           TEXT NOT NULL
);

-- Every credential mint, whether granted or refused by policy.
CREATE TABLE IF NOT EXISTS credential_grants (
    request_id   TEXT PRIMARY KEY,
    runner       TEXT NOT NULL,
    kind         TEXT NOT NULL,
    resource     TEXT,
    thread_id    TEXT,
    turn_id      TEXT,
    surface_user TEXT,
    granted      INTEGER NOT NULL,
    reason       TEXT,
    expires_at   TEXT,
    at           TEXT NOT NULL
);

-- Proxied MCP invocations: the audit trail for third-party actions the agent
-- takes through gateway-held credentials.
CREATE TABLE IF NOT EXISTS mcp_calls (
    call_id      TEXT PRIMARY KEY,
    runner       TEXT,
    thread_id    TEXT,
    turn_id      TEXT,
    surface_user TEXT,
    server       TEXT NOT NULL,
    tool         TEXT NOT NULL,
    args         TEXT,
    ok           INTEGER,
    error        TEXT,
    duration_ms  INTEGER,
    at           TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mcp_calls_at ON mcp_calls(at);

-- Messages accepted while a runner was offline. Persisted before dispatch so a
-- runner restart cannot silently eat traffic.
CREATE TABLE IF NOT EXISTS queued_messages (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    runner    TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    frame     TEXT NOT NULL,
    at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_queued_runner ON queued_messages(runner, id);

-- The latest subscription rate-limit reading per account and window, as the
-- harness reported it at the end of a turn. Keyed by account rather than
-- runner because the limit belongs to the account: every runner sharing a
-- token draws from, and refreshes, the same windows.
CREATE TABLE IF NOT EXISTS rate_limits (
    account      TEXT NOT NULL,
    limit_window TEXT NOT NULL,
    utilization  REAL NOT NULL,
    resets_at    INTEGER,
    status       TEXT,
    runner       TEXT NOT NULL,
    at           TEXT NOT NULL,
    PRIMARY KEY (account, limit_window)
);
