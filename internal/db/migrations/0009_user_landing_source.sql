-- Per-user landing-node source. Both columns are optional and combine: nodes
-- parsed from the subscription URL are merged with the manually pasted URIs
-- (one per line). The source is panel-agnostic — any subscription that returns
-- a base64 list of proxy URIs works, not just Remnawave.
ALTER TABLE users ADD COLUMN landing_sub_url TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN landing_uris TEXT NOT NULL DEFAULT '';
