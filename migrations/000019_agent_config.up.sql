-- 知微 agent 人设配置（全局单份）：identity(身份定位) + soul(性格/语气)。
-- 每轮注入到「发给 dsh 的文本」最前（复用 runTurn 的 Head/Seeds 注入链路）——不改进程级 persona、
-- 不重启 dsh，编辑即时生效。单行单例：id 恒为 1（CHECK 约束兜底）。
CREATE TABLE agent_config (
  id         TINYINT UNSIGNED NOT NULL DEFAULT 1,
  identity   TEXT NOT NULL,
  soul       TEXT NOT NULL,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  CONSTRAINT chk_agent_config_singleton CHECK (id = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
