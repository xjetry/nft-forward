-- A binding edge "downstream may be attached behind upstream" in a rule's
-- chain. mode is the junction segment's forwarding mode (upstream segment's
-- tail hop -> downstream segment's head) captured into rules at expand time.
CREATE TABLE node_bindings (
    upstream_node_id   INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    downstream_node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    mode TEXT NOT NULL DEFAULT 'userspace' CHECK(mode IN ('kernel','userspace')),
    PRIMARY KEY (upstream_node_id, downstream_node_id)
);
