-- Manual display order for nodes, controlled by drag-and-drop in the admin
-- node list. Backfill to the current id order so existing installs keep their
-- ordering until an admin reorders. Lists sort by (sort_order, id).
ALTER TABLE nodes ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0;
UPDATE nodes SET sort_order = id;
