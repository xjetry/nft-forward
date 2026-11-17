-- A rule's header names its logical path: rules.node_id is the entry, and
-- rules.via_node_ids the layers after it. Its hop rows name the same path a
-- second time: each hop carries the logical node it belongs to
-- (rule_hops.via_node_id), and adjacent hops of one logical node form a
-- segment. Nothing kept the two in step. A rule built from an explicit list of
-- hops kept whatever node_id the request gave and an empty via_node_ids, and a
-- rule whose middle node was deleted kept the deleted id in via_node_ids.
--
-- The hop rows are what is installed on the nodes, and their via_node_id is
-- what accounting and suppression group by, so the header is recomputed from
-- them: the first segment's logical node is the entry, the others in order are
-- the layers. RegenerateRule writes the header this way from now on.
--
-- A rule is left as it is when it has no hops, or when the logical node of its
-- first hop no longer exists: rules.node_id is a foreign key into nodes.
WITH heads AS (
  SELECT rule_id, position, via_node_id
    FROM (SELECT rule_id, position, via_node_id,
                 LAG(via_node_id) OVER (PARTITION BY rule_id ORDER BY position) AS prev
            FROM rule_hops)
   WHERE prev IS NULL OR prev <> via_node_id
)
UPDATE rules SET
  node_id = (SELECT h.via_node_id FROM heads h WHERE h.rule_id = rules.id ORDER BY h.position LIMIT 1),
  via_node_ids = (SELECT json_group_array(t.via_node_id)
                    FROM (SELECT h.via_node_id FROM heads h
                           WHERE h.rule_id = rules.id
                           ORDER BY h.position LIMIT -1 OFFSET 1) t)
WHERE EXISTS (SELECT 1 FROM nodes n
               WHERE n.id = (SELECT h.via_node_id FROM heads h WHERE h.rule_id = rules.id ORDER BY h.position LIMIT 1));
