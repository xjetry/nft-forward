-- Cumulative raw bytes forwarded by each physical node (uplink + downlink),
-- folded in from agent counter samples. This is an operator metric, separate
-- from billing: no rate multiplier, no unidirectional uplink-only reduction,
-- and user traffic cycle resets never zero it.
CREATE TABLE node_raw_traffic (
    node_id   INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    raw_bytes INTEGER NOT NULL DEFAULT 0
);
