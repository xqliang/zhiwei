-- 全局技能（Agent Skills）清单：dsh skills 插件从 data/agent-skills/enabled/ 热加载 SKILL.md，
-- 本表记元数据（来源/描述/启禁态/全文预览）。安装/启禁/删除经 /api/agent/skills 管理，
-- 磁盘是生效真源、DB 是元数据镜像（先磁盘后 DB，见 spec 2026-08-28-skill-management §6）。
CREATE TABLE agent_skill (
  id           BIGINT UNSIGNED NOT NULL,
  name         VARCHAR(64)  NOT NULL,
  display_name VARCHAR(128) NOT NULL DEFAULT '',
  source       VARCHAR(255) NOT NULL DEFAULT '',
  description  TEXT NOT NULL,
  enabled      TINYINT(1) NOT NULL DEFAULT 1,
  content      MEDIUMTEXT NOT NULL,
  installed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_agent_skill_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
