-- Per-grant (user+node) shared rate limit in MB/s (0 = unlimited). Shaping
-- policy lives on the grant, like the traffic quota: all rules priced by one
-- grant share a single bucket. rules.bandwidth_mbps stays as a dead column
-- (dropping needs a table rebuild) and is zeroed so stale per-rule caps cannot
-- leak back through old code paths.
ALTER TABLE user_nodes ADD COLUMN rate_limit_mbytes INTEGER NOT NULL DEFAULT 0;
UPDATE rules SET bandwidth_mbps = 0;
