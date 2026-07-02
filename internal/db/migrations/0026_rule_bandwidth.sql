-- Per-rule bandwidth cap in Mbps (0 = unlimited). The data plane (tc HTB +
-- userspace token bucket) already honors nft.Rule.BandwidthMbps; this column is
-- the missing panel-side source so an admin can actually set the value.
ALTER TABLE rules ADD COLUMN bandwidth_mbps INTEGER NOT NULL DEFAULT 0;
