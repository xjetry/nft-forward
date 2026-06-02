-- Per-forward data plane selector: 'kernel' (nftables DNAT, default) or
-- 'userspace' (embedded TCP relay). The composite invariant
-- "userspace => proto='tcp'" is enforced at the HTTP handler + nft.Validate
-- (SQLite cannot ALTER TABLE ADD CONSTRAINT, and rebuilding the forwards
-- table for one CHECK is not worth it); panel proto is already only tcp/udp.
ALTER TABLE forwards ADD COLUMN mode TEXT NOT NULL DEFAULT 'kernel'
    CHECK(mode IN ('kernel','userspace'));
