-- The events a host is in the middle of, such as being offline. A row is
-- written when the event starts and deleted when it ends, so each start
-- and end is notified once, across panel restarts as well.
CREATE TABLE alerts (
    host_id INTEGER NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    -- What the event is: 'offline'.
    kind    TEXT    NOT NULL,
    -- When the event started, as a Unix second.
    since   INTEGER NOT NULL,
    PRIMARY KEY (host_id, kind)
) STRICT, WITHOUT ROWID;
