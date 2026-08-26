-- 画像 cycle（周期/日程，敏感）平面（spec §4.6）。原 main 000009_metric_cycle 改号而来。
-- ⚠️ 合并对账：person_metric 已由 feat 的 000011_metric 建表（与本文件原 CREATE 撞表），
-- 故此处只建 person_cycle，person_metric CREATE 已删除。
-- cycle 含下次预测（anchor+period，非医疗建议），敏感数据：本地存储、前端默认折叠（spec §9）。
CREATE TABLE person_cycle (
  id              BIGINT PRIMARY KEY,
  user_id         BIGINT NOT NULL DEFAULT 1,
  person_id       BIGINT NOT NULL,
  cycle_type      VARCHAR(16) NOT NULL,            -- menstrual|medication|injection|followup
  label           VARCHAR(128) NULL,               -- 药名/针名/'生理期'（自然键成分，NULL 视为 ''）
  anchor_date     DATE NULL,                       -- 上次起始（预测锚点）
  period_days     INT NULL,                        -- 周期天数
  duration_days   INT NULL,                        -- 单次持续
  dosage          VARCHAR(64) NULL,
  frequency_text  VARCHAR(64) NULL,                -- 频次（'每日两次'）
  next_predicted_at DATE NULL,                     -- = anchor+period；估算非医疗建议（spec §9）
  -- 横切字段同上
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
  KEY idx_person_type (person_id, cycle_type, status),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
