-- rules.disabled (0001): the push predicate read it and rule payloads carried
-- it, but no statement ever wrote it, so every row holds the 0 default. A rule
-- stops forwarding only through the suppression conditions (owner disabled or
-- expired, a quota reached, a node on its path disabled); there is no per-rule
-- switch.
ALTER TABLE rules DROP COLUMN disabled;
