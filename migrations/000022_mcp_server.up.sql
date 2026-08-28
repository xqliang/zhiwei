-- 全局 MCP 服务清单：dsh agent 连接的外部/内置 MCP 服务。启禁/增删经 /api/agent/mcp 管理，
-- 生效走 cordisgen 重写配置 + 运行时 mcp/apply 热插拔（见 spec 2026-08-28-mcp-management）。
-- id 用雪花 ID（应用层 ids.New 生成，与 agent_conversation 一致）；内置 zhiwei 行固定 id=1。
CREATE TABLE mcp_server (
  id           BIGINT UNSIGNED NOT NULL,
  server_key   VARCHAR(64)  NOT NULL,
  display_name VARCHAR(128) NOT NULL,
  transport    VARCHAR(32)  NOT NULL,
  url          TEXT NULL,
  command      VARCHAR(255) NULL,
  args         JSON NULL,
  env          JSON NULL,
  enabled      TINYINT(1) NOT NULL DEFAULT 1,
  builtin      TINYINT(1) NOT NULL DEFAULT 0,
  created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_mcp_server_key (server_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO mcp_server (id, server_key, display_name, transport, url, enabled, builtin)
VALUES (1, 'zhiwei', '知微内置工具', 'streamable-http', '', 1, 1);
