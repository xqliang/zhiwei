-- 画像 P4：activity（生活轨迹）平面（spec §4.7）。
-- 活动流：每条 = 某时开始、（可选）持续多久的一次活动（做什么/工具/地点/通勤）。
-- 测点流语义（同 person_metric）：无「当前值」、无 supersedes_id——改口就是新活动或 dismiss 旧条。
CREATE TABLE person_activity (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  person_id    BIGINT NOT NULL,
  activity     VARCHAR(256) NOT NULL,              -- 做什么（开会/写代码/打球…）
  tool         VARCHAR(128) NULL,                  -- 什么工具（手机/电脑/健身房/汽车…）
  location     VARCHAR(256) NULL,
  commute_mode VARCHAR(24) NULL,                   -- 通勤方式（中文短串：地铁/开车/步行…；不做枚举强校验）
  started_at   DATETIME(3) NOT NULL,               -- 开始时间（LLM 未给则落 session 时间）
  duration_min INT NULL,                           -- 持续分钟数
  -- 横切字段（与既有平面一致，spec §3；无 supersedes_id，见顶部注释）
  confidence    DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed',
  source        VARCHAR(8) NOT NULL DEFAULT 'manual',
  status        VARCHAR(16) NOT NULL DEFAULT 'active',
  session_id    BIGINT NULL,
  memory_id     BIGINT NULL,
  transcript_segment_ids JSON NULL,
  version       INT NOT NULL DEFAULT 1,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_time (person_id, started_at),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
