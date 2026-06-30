CREATE TABLE IF NOT EXISTS api_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    token        TEXT    NOT NULL UNIQUE,
    disabled     INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
);
