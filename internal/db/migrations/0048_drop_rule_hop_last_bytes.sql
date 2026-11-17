-- rule_hops.last_bytes (0001), last_bytes_up / last_bytes_down (0018): every
-- counter batch overwrote them with the batch's last sample for the hop, and
-- the rule and node detail responses serialized them, but nothing read them —
-- not the web UI, not /api/v1, not the agent. The throughput the UI shows
-- comes from the panel's in-memory speed cache, which never read these columns.
ALTER TABLE rule_hops DROP COLUMN last_bytes;
ALTER TABLE rule_hops DROP COLUMN last_bytes_up;
ALTER TABLE rule_hops DROP COLUMN last_bytes_down;
