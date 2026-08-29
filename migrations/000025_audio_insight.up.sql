-- P1 音频场景与情绪理解（spec §2）。合并回 main 前复查 main 最新迁移号（并行分支撞号）。
-- 会话级声学环境（1:1 挂 transcript）。
ALTER TABLE transcript ADD COLUMN acoustic_scene   VARCHAR(32)  NOT NULL DEFAULT '' AFTER confidence;
ALTER TABLE transcript ADD COLUMN background_sounds JSON        NULL              AFTER acoustic_scene;
ALTER TABLE transcript ADD COLUMN weather_cues      VARCHAR(32)  NOT NULL DEFAULT '' AFTER background_sounds;
ALTER TABLE transcript ADD COLUMN overall_mood      VARCHAR(128) NOT NULL DEFAULT '' AFTER weather_cues;

-- 每会话每说话人情绪状态（可选富化，独立表不污染热表 transcript_segment）。
CREATE TABLE speaker_session_state (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  transcript_id BIGINT NOT NULL,
  session_id    BIGINT NOT NULL,
  speaker_label VARCHAR(32)  NOT NULL DEFAULT '',
  speaker_id    BIGINT NULL,
  emotion       VARCHAR(32)  NOT NULL DEFAULT '',
  micro_emotion VARCHAR(64)  NOT NULL DEFAULT '',
  mental_state  VARCHAR(64)  NOT NULL DEFAULT '',
  confidence    DOUBLE NOT NULL DEFAULT 0,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_sss_session (session_id),
  KEY idx_sss_transcript (transcript_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
