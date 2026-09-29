-- Traffic is accumulated per day rather than per billing cycle, so that the
-- usage of the current cycle can be summed afresh from its first day, and
-- stays right when the operator moves the reset day. traffic_cycle was
-- never written to, so nothing is carried over.
DROP TABLE traffic_cycle;

CREATE TABLE traffic_daily (
    host_id  INTEGER NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    -- The calendar date in the panel's time zone when the traffic was
    -- received, 'YYYY-MM-DD'.
    day      TEXT    NOT NULL,
    rx_bytes INTEGER NOT NULL DEFAULT 0 CHECK (rx_bytes >= 0),
    tx_bytes INTEGER NOT NULL DEFAULT 0 CHECK (tx_bytes >= 0),
    PRIMARY KEY (host_id, day)
) STRICT, WITHOUT ROWID;
