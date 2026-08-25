-- 用户画像/人物系统 P1（spec 2026-08-24-person-profile-system-design §4）。
-- 4 张表：person 主体 / person_attribute 属性平面 / person_relationship 关系平面 /
-- person_change_log 统一审计（只追加，永不 update/delete）。
-- 回填（owner person + speaker→person）不在此做：雪花 ID 由 Go 侧
-- repo.EnsurePersonBootstrap 启动时幂等生成，迁移只建表。
-- 横切字段（source/confidence/epistemic_type/status/溯源/supersedes_id/version）
-- 在 attribute 与 relationship 两表结构一致，见 spec §3。

CREATE TABLE person (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  display_name VARCHAR(128) NOT NULL,
  speaker_id   BIGINT NULL,                        -- 可选关联声纹；一个声纹至多绑一个人
  is_owner     TINYINT(1) NOT NULL DEFAULT 0,      -- 「我」本人，全局至多一个
  summary      TEXT NULL,
  source       VARCHAR(8) NOT NULL DEFAULT 'manual',  -- manual|llm（llm=抽取自动新建）
  status       VARCHAR(16) NOT NULL DEFAULT 'active', -- active|pending|merged|dismissed
  created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_speaker (speaker_id),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE person_attribute (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  person_id     BIGINT NOT NULL,
  attr_key      VARCHAR(64) NOT NULL,              -- 目录 key 或自由 key（落「其他」组）
  value_text    TEXT NOT NULL,
  value_type    VARCHAR(16) NOT NULL DEFAULT 'text', -- text|enum|bool|date|number
  confidence    DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed', -- observed|inferred|predicted|suggested
  source        VARCHAR(8) NOT NULL DEFAULT 'manual',    -- manual|llm
  status        VARCHAR(16) NOT NULL DEFAULT 'active',   -- active|pending|superseded|dismissed
  session_id    BIGINT NULL,
  memory_id     BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id BIGINT NULL,                       -- 冲突 pending 指向当前 active 行
  version       INT NOT NULL DEFAULT 1,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_key_status (person_id, attr_key, status),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE person_relationship (
  id                BIGINT PRIMARY KEY,
  user_id           BIGINT NOT NULL DEFAULT 1,
  person_id         BIGINT NOT NULL,               -- 主体
  related_person_id BIGINT NULL,                   -- 对端人物（组织关系可空）
  relation_type     VARCHAR(24) NOT NULL,          -- 配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他
  direction         VARCHAR(8) NULL,               -- upstream|downstream|peer（上下游）
  org_name          VARCHAR(128) NULL,
  label             VARCHAR(128) NULL,             -- 自由称呼（「大儿子」「张总」）
  confidence        DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type    VARCHAR(16) NOT NULL DEFAULT 'observed',
  source            VARCHAR(8) NOT NULL DEFAULT 'manual',
  status            VARCHAR(16) NOT NULL DEFAULT 'active',
  session_id        BIGINT NULL,
  memory_id         BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id     BIGINT NULL,
  version           INT NOT NULL DEFAULT 1,
  created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person (person_id, status),
  KEY idx_related (related_person_id),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE person_change_log (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  person_id     BIGINT NOT NULL,
  entity_kind   VARCHAR(16) NOT NULL,              -- person|attribute|relationship（P2+ 扩 event/metric/…）
  entity_id     BIGINT NULL,                       -- 目标行 id（删除后仍留历史）
  attr_key      VARCHAR(64) NULL,                  -- attribute 平面冗余，便于按字段查历史
  change_type   VARCHAR(16) NOT NULL,              -- create|update|confirm|dismiss|supersede|delete|reaffirm
  changed_by    VARCHAR(8) NOT NULL,               -- user|llm
  old_value     JSON NULL,                         -- 变更前快照（JSON 文本）
  new_value     JSON NULL,
  confidence    DECIMAL(5,4) NULL,
  epistemic_type VARCHAR(16) NULL,
  session_id    BIGINT NULL,
  memory_id     BIGINT NULL,
  transcript_segment_ids JSON NULL,
  note          TEXT NULL,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_person_kind_time (person_id, entity_kind, created_at),
  KEY idx_entity (entity_kind, entity_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
