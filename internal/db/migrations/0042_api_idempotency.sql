-- Records the rule a given Idempotency-Key already produced for a user, so an
-- agent that retries a create (network blip, at-least-once queue) replays the
-- original rule instead of allocating a second entry port + dispatching twice.
-- Keyed per user; rows are pruned by age on write (see SaveIdempotentRule).
CREATE TABLE IF NOT EXISTS api_idempotency (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    idem_key   TEXT    NOT NULL,
    rule_id    INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, idem_key)
);
