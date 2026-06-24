-- Optional proxy URI for a custom (non-landing) exit. When set, the rule can
-- offer a relay proxy URI on the rules page (host:port rewritten to the entry).
-- Landing-node exits leave this empty: their URI is resolved at view time from
-- the owner's landing source by exit_host:exit_port, not snapshotted here.
ALTER TABLE rules ADD COLUMN exit_uri TEXT NOT NULL DEFAULT '';
