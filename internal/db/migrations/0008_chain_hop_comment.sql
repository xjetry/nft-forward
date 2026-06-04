-- 链路跳的自定义备注。空串表示无自定义,RegenerateChain 回退到默认生成的
-- "链路 X · 第N跳"。独立于 forwards.comment 存在,因为 RegenerateChain 每次
-- 重建 forwards 都会覆盖其 comment,只有存在 chain_hops 上的自定义值才能在
-- 重算后保留。
ALTER TABLE chain_hops ADD COLUMN comment TEXT NOT NULL DEFAULT '';
