-- Initial schema.
--
-- Conventions:
--   * Timestamps are INTEGER Unix seconds in UTC.
--   * Calendar dates (reminders, traffic cycles) are TEXT 'YYYY-MM-DD' in the
--     panel time zone.
--   * Byte counts are INTEGER bytes; rates are REAL bytes per second.
--   * Tokens, secrets and session ids are stored only as hashes.

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL DEFAULT 'admin' CHECK (role IN ('admin', 'viewer')),
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    last_login_at INTEGER
) STRICT;

CREATE TABLE sessions (
    id_hash    TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    ip         TEXT    NOT NULL DEFAULT '',
    user_agent TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE setup_state (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    code_hash  TEXT    NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE hosts (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT    NOT NULL,
    sort                 INTEGER NOT NULL DEFAULT 0,
    note                 TEXT    NOT NULL DEFAULT '',
    agent_id             TEXT    UNIQUE,
    secret_hash          TEXT,
    hostname             TEXT,
    os                   TEXT,
    platform             TEXT,
    platform_version     TEXT,
    kernel               TEXT,
    arch                 TEXT,
    cpu_model            TEXT,
    cpu_cores            INTEGER,
    agent_version        TEXT,
    last_seen_at         INTEGER,
    created_at           INTEGER NOT NULL,
    remote_ip            TEXT,
    remote_ip_changed_at INTEGER,
    -- Renewal reminder; remind_start NULL means disabled.
    remind_start         TEXT,
    remind_day           INTEGER CHECK (remind_day BETWEEN 1 AND 31),
    cycle_months         INTEGER CHECK (cycle_months IN (1, 3, 6, 12)),
    acked_on             TEXT,
    snoozed_on           TEXT,
    -- Traffic accounting.
    traffic_mode         TEXT    CHECK (traffic_mode IN ('out', 'in', 'both')),
    traffic_reset_day    INTEGER CHECK (traffic_reset_day BETWEEN 1 AND 28),
    traffic_limit_bytes  INTEGER CHECK (traffic_limit_bytes > 0),
    -- Per-host interface exclusion override; NULL means use the default.
    net_iface_exclude    TEXT
) STRICT;

CREATE TABLE enroll_tokens (
    token_hash TEXT    PRIMARY KEY,
    host_id    INTEGER NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
) STRICT;

CREATE INDEX enroll_tokens_host_id ON enroll_tokens (host_id);

CREATE TABLE metrics_raw (
    host_id    INTEGER NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    ts         INTEGER NOT NULL,
    cpu        REAL,
    mem_used   INTEGER,
    mem_total  INTEGER,
    swap_used  INTEGER,
    swap_total INTEGER,
    load1      REAL,
    load5      REAL,
    load15     REAL,
    disk_used  INTEGER,
    disk_total INTEGER,
    disk_free  INTEGER,
    rx_rate    REAL,
    tx_rate    REAL,
    uptime     INTEGER,
    PRIMARY KEY (host_id, ts)
) STRICT, WITHOUT ROWID;

-- metrics_1m and metrics_1h hold averages of the raw columns (uptime is the
-- last value in the bucket), plus maxima for the bursty metrics. ts is the
-- bucket start.
CREATE TABLE metrics_1m (
    host_id     INTEGER NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    ts          INTEGER NOT NULL,
    cpu         REAL,
    cpu_max     REAL,
    mem_used    INTEGER,
    mem_total   INTEGER,
    swap_used   INTEGER,
    swap_total  INTEGER,
    load1       REAL,
    load5       REAL,
    load15      REAL,
    disk_used   INTEGER,
    disk_total  INTEGER,
    disk_free   INTEGER,
    rx_rate     REAL,
    rx_rate_max REAL,
    tx_rate     REAL,
    tx_rate_max REAL,
    uptime      INTEGER,
    PRIMARY KEY (host_id, ts)
) STRICT, WITHOUT ROWID;

CREATE TABLE metrics_1h (
    host_id     INTEGER NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    ts          INTEGER NOT NULL,
    cpu         REAL,
    cpu_max     REAL,
    mem_used    INTEGER,
    mem_total   INTEGER,
    swap_used   INTEGER,
    swap_total  INTEGER,
    load1       REAL,
    load5       REAL,
    load15      REAL,
    disk_used   INTEGER,
    disk_total  INTEGER,
    disk_free   INTEGER,
    rx_rate     REAL,
    rx_rate_max REAL,
    tx_rate     REAL,
    tx_rate_max REAL,
    uptime      INTEGER,
    PRIMARY KEY (host_id, ts)
) STRICT, WITHOUT ROWID;

CREATE TABLE traffic_state (
    host_id       INTEGER NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    iface         TEXT    NOT NULL,
    last_rx_bytes INTEGER NOT NULL,
    last_tx_bytes INTEGER NOT NULL,
    last_mono_ms  INTEGER NOT NULL,
    last_ts       INTEGER NOT NULL,
    PRIMARY KEY (host_id, iface)
) STRICT, WITHOUT ROWID;

CREATE TABLE traffic_cycle (
    host_id     INTEGER NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    cycle_start TEXT    NOT NULL,
    rx_bytes    INTEGER NOT NULL DEFAULT 0,
    tx_bytes    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (host_id, cycle_start)
) STRICT, WITHOUT ROWID;
