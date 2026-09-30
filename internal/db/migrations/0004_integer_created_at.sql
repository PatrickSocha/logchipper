-- Store created_at as Unix milliseconds (INTEGER) instead of datetime text,
-- so range filters and histogram buckets are plain integer arithmetic.
-- SQLite can't change a column's type in place, so rebuild the table.
CREATE TABLE events_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at INTEGER NOT NULL, -- Unix milliseconds, UTC
    source     TEXT NOT NULL DEFAULT '',
    level      TEXT NOT NULL DEFAULT 'info',
    message    TEXT NOT NULL,
    meta       TEXT NOT NULL DEFAULT ''
);

-- Rows whose text SQLite can't parse get 0 (1970): out of every view, and
-- the next retention purge removes them.
INSERT INTO events_new (id, created_at, source, level, message, meta)
SELECT id,
       COALESCE(CAST(ROUND((julianday(created_at) - 2440587.5) * 86400000) AS INTEGER), 0),
       source, level, message, meta
FROM events;

-- Keep the AUTOINCREMENT high-water mark so purged ids are never reused
-- (clients page the stream by id).
DELETE FROM sqlite_sequence WHERE name = 'events_new';
UPDATE sqlite_sequence SET name = 'events_new' WHERE name = 'events';

DROP TABLE events;
ALTER TABLE events_new RENAME TO events;

CREATE INDEX idx_events_created_at ON events(created_at);
-- level/source filters are always bounded by time, so lead with them and
-- seek straight to the range.
CREATE INDEX idx_events_level_created_at ON events(level, created_at);
CREATE INDEX idx_events_source_created_at ON events(source, created_at);
