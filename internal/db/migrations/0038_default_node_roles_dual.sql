-- Grant both roles (entry|via = 3) to every real agent node so existing nodes
-- can serve as an entry and as a middle layer without a manual toggle. The
-- panel's built-in self node is excluded: it never acts as a middle layer, and
-- a via bit would only surface it in binding candidate lists.
UPDATE nodes SET roles = 3 WHERE node_type != 'self';
