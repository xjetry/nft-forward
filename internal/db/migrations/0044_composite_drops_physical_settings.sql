-- unidirectional and no_direct_exit are settings of physical nodes: the first is
-- read from the node that reports a counter batch, the second from the last
-- physical hop of a chain. A composite node is neither, so a value stored on it
-- never took effect. The panel used to accept these switches on composites;
-- clear what was stored so every composite row agrees with what the writers now
-- enforce (they refuse composite nodes).
UPDATE nodes SET unidirectional = 0, no_direct_exit = 0 WHERE node_type = 'composite';
