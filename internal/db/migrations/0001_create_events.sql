CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at DATETIME NOT NULL DEFAULT (datetime('now')),
    source     TEXT NOT NULL DEFAULT '',
    level      TEXT NOT NULL DEFAULT 'info',
    message    TEXT NOT NULL,
    meta       TEXT NOT NULL DEFAULT ''
);
