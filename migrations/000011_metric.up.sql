-- person_metric：画像第 5 平面（时间序列个人指标：情绪/体重/睡眠等）。
-- 与 person_event 同构，但为「连续测点」特化：数值列 value_num + 单位 unit +
-- 全精度 measured_at；append-only（每测点一行，正常写入不单值 supersede）；
-- 自然键 (person_id, metric_key, measured_at[+value]) 含时间，同次抽取多读数不塌缩。
CREATE TABLE person_metric (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  person_id BIGINT NOT NULL,
  metric_key VARCHAR(32) NOT NULL,            -- emotion|weight|sleep|mood_energy|diet|health|height|waist|chest|hip|body_fat（catalog）
  value_num DECIMAL(10,3) NULL,               -- 数值（体重kg/情绪-1..1/睡眠h）；曲线只画非空者
  value_text VARCHAR(256) NULL,               -- 类别描述（情绪='焦虑'/饮食='火锅'）
  unit VARCHAR(16) NULL,                       -- kg|h|…
  measured_at DATETIME(3) NOT NULL,            -- 测点时间（全精度，勿抹平到当天零点）
  confidence DECIMAL(4,3) NOT NULL DEFAULT 1.000, -- 抽取确定性（与 value 载荷分离）
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed',
  source VARCHAR(16) NOT NULL DEFAULT 'manual',      -- manual|extract
  status VARCHAR(16) NOT NULL DEFAULT 'active',       -- active|pending|superseded|dismissed
  session_id BIGINT NULL,
  memory_id BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id BIGINT NULL,                   -- 仅手动纠错用（正常写入不置）
  note VARCHAR(512) NULL,
  version INT NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_metric_time (person_id, metric_key, measured_at),
  KEY idx_person_metric_status (person_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
