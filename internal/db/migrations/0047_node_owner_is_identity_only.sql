-- A node's owner (nodes.owner_id) now means one thing: the account in whose
-- name commands issued from that node are executed. It no longer implies that
-- the owner may use the node. "May this user use this node" has a single
-- answer everywhere — admins may, everyone else needs a grant (user_nodes) —
-- because per-node rule caps, traffic quota, rate limit and per-grant billing
-- all hang off the grant row.

-- Non-admin owners used to be able to create rules on their own node from the
-- node side, and to probe through it, on ownership alone. Give each of them
-- the grant that ownership used to imply, so nothing they could do stops
-- working; from here on the grant is what authorizes it and an admin can
-- limit or revoke it like any other.
INSERT INTO user_nodes (user_id, node_id, max_forwards, granted_at)
SELECT n.owner_id, n.id, 9999, CAST(strftime('%s','now') AS INTEGER)
  FROM nodes n
  JOIN users u ON u.id = n.owner_id
 WHERE u.role <> 'admin'
   AND NOT EXISTS (SELECT 1 FROM user_nodes un WHERE un.user_id = n.owner_id AND un.node_id = n.id);

-- A machine without an owner cannot create rules from the node side or hand
-- its local rules over to the panel, since both need an account to belong to.
-- Machines that have none get the first admin, the same account
-- UpsertSelfNode gives the panel host's own node. Composites are not machines
-- and have no owner. On a database with no admin yet the subquery yields NULL
-- and nothing changes.
UPDATE nodes
   SET owner_id = (SELECT id FROM users WHERE role = 'admin' ORDER BY id LIMIT 1)
 WHERE owner_id IS NULL AND node_type <> 'composite';

-- Every rule has a non-empty name. Rules created from the node side used to be
-- stored with an empty one; name them the way such rules are named now: the
-- comment if there is one, otherwise the exit address.
UPDATE rules
   SET name = CASE
         WHEN TRIM(comment) <> '' THEN TRIM(comment)
         WHEN instr(exit_host, ':') > 0 THEN '[' || exit_host || ']:' || exit_port
         ELSE exit_host || ':' || exit_port
       END
 WHERE TRIM(name) = '';
