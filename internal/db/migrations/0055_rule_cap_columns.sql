-- 账号与授权上的条数上限数的是规则：一条规则占一个名额，不论它展开成几跳。列名里的
-- forwards 是规则还叫“转发”时留下的；界面、报错与 Go 代码都叫它规则数上限，列名跟上。
-- 只改列名：每一行的值原样留下。
ALTER TABLE users RENAME COLUMN max_forwards TO max_rules;
ALTER TABLE user_nodes RENAME COLUMN max_forwards TO max_rules;
