-- A composite is a line made of machines and runs no agent of its own, yet its
-- row used to be created as a machine and then retyped, and the machine writers
-- accepted composites. What that left behind:
--   secret: random, and an agent presenting it was accepted as the composite.
--   port_range: the default range, never read; listen ports are allocated on
--     the physical hops.
--   relay hosts, dispatch and upgrade results, connection state: written by the
--     relay-host, resync and upgrade endpoints and by an agent connected with
--     the composite's secret. No derivation reads them (a composite's online
--     state and relay addresses come from its members); the admin dashboard
--     printed a stale relay host or address in its entry-address column.
--   owner_id: settable only by a direct call to the owner endpoint (the pages
--     have no owner control). A composite has no node-side commands to run on
--     anyone's behalf; the access check also read it as the owner's access,
--     which is what a grant expresses.
-- The writers now refuse composites and new composites are created without
-- machine columns; clear what is stored so every composite row agrees.
UPDATE nodes SET
  owner_id = NULL, address = '', secret = '', relay_host = '', relay_host_v6 = '',
  relay_host_declared = 0, relay_host_v6_declared = 0, online = 0,
  agent_version = '', agent_sha = '', last_seen = NULL, last_apply_at = NULL,
  last_error = NULL, last_warning = '', port_range = '',
  last_upgrade_at = NULL, last_upgrade_version = NULL, last_upgrade_status = NULL, last_upgrade_error = NULL,
  unidirectional = 0, no_direct_exit = 0
WHERE node_type = 'composite';
