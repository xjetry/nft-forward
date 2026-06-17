ALTER TABLE nodes ADD COLUMN last_upgrade_at INTEGER;
ALTER TABLE nodes ADD COLUMN last_upgrade_version TEXT;
ALTER TABLE nodes ADD COLUMN last_upgrade_status TEXT;
ALTER TABLE nodes ADD COLUMN last_upgrade_error TEXT;
