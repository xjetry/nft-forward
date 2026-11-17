-- 落地代理上的“落地/直连”标记整张表存在 settings 的一行里。它的键原来叫 node_roles，
-- 可它记的既不是 nodes 表的行（键是代理的“协议:主机:端口”），也不是节点可以占的
-- 位置（那是 nodes.seats）。只改键：值原样留下。还没有打过标记的库里没有这一行，
-- 这条语句不动任何东西。
UPDATE settings SET key = 'proxy_marks' WHERE key = 'node_roles';
