-- 画像 P3：metric（时序指标）+ cycle（周期/日程，敏感）两平面（spec §4.5/§4.6）。
-- metric 是测点流：每个时间戳一行（情绪/体重/熬夜…），无「当前值」概念。
-- cycle 含下次预测（anchor+period，非医疗建议），敏感数据：本地存储、前端默认折叠（spec §9）。
CREATE TABLE person_metric (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  person_id    BIGINT NOT NULL,
  metric_key   VARCHAR(32) NOT NULL,               -- emotion|state|weight|sleep_late|diet|health
  value_num    DECIMAL(10,3) NULL,                 -- 数值型（体重 kg、熬夜 0/1）；类别型为 NULL
  value_text   VARCHAR(256) NULL,                  -- 类别/描述（情绪='焦虑'、饮食='火锅'）；数值型为 NULL
  unit         VARCHAR(16) NULL,
  measured_at  DATETIME(3) NOT NULL,               -- 测点时间（LLM 未给则落 session 时间）
  -- 横切字段（与既有平面一致，spec §3）
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
  KEY idx_person_key_time (person_id, metric_key, measured_at),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

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
