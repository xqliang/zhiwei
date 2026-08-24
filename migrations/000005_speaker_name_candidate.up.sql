-- 说话人名字候选（speakername stage 用 LLM 从对话上下文推断的称呼建议）。
-- 一个说话人 N 行候选；uk_speaker_name 唯一键支撑跨 session upsert 累积
-- （confidence 取 GREATEST、证据留最新），见 repo.Upsert。
-- 仅作建议：不改 speaker.name；用户采纳（改名）后整组删除。
CREATE TABLE speaker_name_candidate (
  id                BIGINT PRIMARY KEY,
  speaker_id        BIGINT NOT NULL,                       -- 归属说话人（speaker.id）
  name              VARCHAR(128) NOT NULL,                 -- 候选名（张总/王哥/张三…）
  confidence        DOUBLE NOT NULL DEFAULT 0,             -- 置信度 [0,1]，展示排序键
  evidence          VARCHAR(512) NULL,                     -- 依据：简短引用 + 时间点
  source_session_id BIGINT NULL,                           -- 最近一次产生该候选的会话
  created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_speaker_name (speaker_id, name),
  KEY idx_speaker (speaker_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
