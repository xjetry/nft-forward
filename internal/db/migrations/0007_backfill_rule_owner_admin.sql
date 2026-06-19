-- Orphan rules (owner_id IS NULL) are admin-managed: bind them to the earliest
-- admin so ownership listings and traffic accounting have a subject. Admin API
-- creation now sets owner_id directly; this backfills rules created before that.
-- No-op when no admin exists yet (fresh DB before bootstrap) or nothing is orphaned.
UPDATE rules
SET owner_id = (SELECT id FROM users WHERE role = 'admin' ORDER BY id LIMIT 1)
WHERE owner_id IS NULL
  AND EXISTS (SELECT 1 FROM users WHERE role = 'admin');
