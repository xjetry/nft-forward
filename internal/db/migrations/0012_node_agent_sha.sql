-- The sha256 of the nft-agent binary a node is running, reported in the hello
-- frame. The panel compares it against the agent it would push to decide
-- whether a binary transfer is needed at all, versus a version-label-only sync.
ALTER TABLE nodes ADD COLUMN agent_sha TEXT NOT NULL DEFAULT '';
