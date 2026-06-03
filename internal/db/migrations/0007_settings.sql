-- 全局键值设置(单实例面板)。当前用于 panel_url:agent 反向连接面板的公网地址,
-- 用于生成节点安装命令;留空时页面回退到当前访问域名。
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
