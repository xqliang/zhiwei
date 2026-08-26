-- 画像 P2：event 平面（人物大事记，spec §4.4）。
-- 有日期的一次性事件（结婚/毕业/旅行/聚会/会议/生病/学会…）；
-- 与 list 属性（看过的书等速览）互补：属性记「有过的」，event 记「某次发生的」。
CREATE TABLE person_event (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  person_id     BIGINT NOT NULL,
  event_type    VARCHAR(32) NOT NULL,              -- 里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他
  title         VARCHAR(512) NOT NULL,
  description   TEXT NULL,
  occurred_at   DATETIME(3) NULL,                  -- 事件发生时间（可能只精确到日/月，解析失败为 NULL）
  end_at        DATETIME(3) NULL,                  -- 跨天事件（旅行/会议）
  location      VARCHAR(256) NULL,
  related_person_ids JSON NULL,                    -- 同场人物（MVP 单元素或空，见计划头决策 1）
  importance    DECIMAL(5,4) NOT NULL DEFAULT 0.5,
  -- 横切字段（与 attribute/relationship 平面一致，spec §3）
  confidence    DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed',
  source        VARCHAR(8) NOT NULL DEFAULT 'manual',
  status        VARCHAR(16) NOT NULL DEFAULT 'active',
  session_id    BIGINT NULL,
  memory_id     BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id BIGINT NULL,
  version       INT NOT NULL DEFAULT 1,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_time (person_id, occurred_at),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
