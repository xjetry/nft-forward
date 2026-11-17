-- 节点行与授权行上的这一列存的是“可以占链里的哪些位置”：入口（1）与中转（2），
-- 授权行上的 0 表示沿用节点自己的。链路推导把它叫位置（chain.Seat），列名里的
-- roles 与账号的角色（users.role）、落地代理上的落地/直连标记撞了名字。
-- 只改列名：每一行的值原样留下。
ALTER TABLE nodes RENAME COLUMN roles TO seats;
ALTER TABLE user_nodes RENAME COLUMN roles TO seats;
