-- Nodes are classified only by role (entry / middle layer) and type (single /
-- composite). The hidden flag was a pure presentation toggle that never
-- affected forwarding, sync or authorization; dropping the column removes the
-- concept outright, so previously hidden nodes and their rules simply show up
-- in the admin lists again.
ALTER TABLE nodes DROP COLUMN hidden;
