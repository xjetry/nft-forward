-- A composite's effective billing multiplier used to be the sum of its
-- per-hop multipliers, aggregated in memory. Billing now reads the entry
-- node's own rate_multiplier, so bake the sum into the composite's column to
-- keep existing pricing unchanged. node_hops.traffic_multiplier stays as a
-- dormant column (no reads or meaningful writes afterwards).
UPDATE nodes SET rate_multiplier = COALESCE(
    (SELECT SUM(traffic_multiplier) FROM node_hops WHERE node_hops.node_id = nodes.id),
    rate_multiplier)
WHERE node_type = 'composite';
