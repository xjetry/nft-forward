-- users.expires_at had two spellings of "never expires": NULL, and 0 (what the
-- endpoints that clear an expiry wrote). Every reader had to test for both, and
-- the user list, which serialized the row as it was, showed the difference to
-- its caller.
--
-- NULL is now the only spelling. The expiry predicate treated any value that
-- was not greater than 0 as "never", so negative values are folded in with 0.
UPDATE users SET expires_at = NULL WHERE expires_at <= 0;
