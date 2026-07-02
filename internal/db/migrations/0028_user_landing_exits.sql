-- Per-user landing-exit traffic ledger keyed by destination host:port. One
-- table carries both the materialized landing set (name/protocol/uri/present,
-- driving exit classification) and the quota ledger (quota_bytes/used_bytes,
-- 0 quota = unlimited). Sync never deletes rows: exits that drop out of the
-- subscription are flagged present=0 so a returning exit resumes its ledger.
CREATE TABLE user_landing_exits (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  host        TEXT    NOT NULL,
  port        INTEGER NOT NULL,
  name        TEXT    NOT NULL DEFAULT '',
  protocol    TEXT    NOT NULL DEFAULT '',
  uri         TEXT    NOT NULL DEFAULT '',
  present     INTEGER NOT NULL DEFAULT 1,
  quota_bytes INTEGER NOT NULL DEFAULT 0,
  used_bytes  INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, host, port)
);
