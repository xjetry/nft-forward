-- node_hops.traffic_multiplier (0014): a composite's price used to be the sum
-- of its members' multipliers. Since 0034 the price is the composite's own
-- nodes.rate_multiplier; member rows written after that store 0 here, and no
-- code reads the column. It was still selected, scanned and serialized with
-- every member row.
ALTER TABLE node_hops DROP COLUMN traffic_multiplier;

-- nodes.traffic_multiplier (0013): no code reads or writes it. A node's price
-- is nodes.rate_multiplier.
ALTER TABLE nodes DROP COLUMN traffic_multiplier;
