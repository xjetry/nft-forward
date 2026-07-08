-- A token's scope bounds what the /api/v1 surface will honor: 'read' clears only
-- safe (GET) endpoints, 'readwrite' also clears mutating ones. Existing tokens
-- predate every write endpoint and only ever reached the read-only /info, so they
-- default to 'read' — shipping write endpoints must never silently arm an
-- already-issued token with write power.
ALTER TABLE api_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'read';
