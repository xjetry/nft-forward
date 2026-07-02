-- Session and API tokens move from plaintext-at-rest to SHA-256 hashes so a
-- leaked DB no longer yields replayable credentials. token_prefix keeps the
-- first 8 chars visible so users can still recognize their API token; the full
-- value only ever exists in the client after create/refresh. Existing sessions
-- are dropped (users re-login); existing API tokens are hashed in-place by the
-- Go backfill that keys off token_prefix='' (see hashLegacyAPITokens).
ALTER TABLE api_tokens ADD COLUMN token_prefix TEXT NOT NULL DEFAULT '';
DELETE FROM sessions;
