-- 知微 MVP 全量 schema：雪花 ID 主键，无 AUTO_INCREMENT。
-- 集成测试库 zhiwei_test 由 init-testdb 目标创建。

CREATE TABLE audio_session (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  source VARCHAR(16) NOT NULL,                -- web_upload | web_record
  filename VARCHAR(512) NOT NULL,
  storage_path VARCHAR(1024) NOT NULL,
  duration_ms BIGINT NOT NULL DEFAULT 0,
  mime VARCHAR(64) NOT NULL DEFAULT '',
  status VARCHAR(16) NOT NULL DEFAULT 'uploaded', -- uploaded|processing|completed|failed
  job_id BIGINT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user_time (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE pipeline_job (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  session_id BIGINT NOT NULL,
  stage VARCHAR(16) NOT NULL,                 -- asr|segment|extract|quality|commit|done
  status VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|running|failed|done
  attempt INT NOT NULL DEFAULT 0,
  last_error TEXT NULL,
  trace JSON NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_status_id (status, id),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE transcript (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  session_id BIGINT NOT NULL,
  language VARCHAR(16) NOT NULL DEFAULT 'zh-CN',
  full_text MEDIUMTEXT NULL,
  confidence DECIMAL(5,4) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_session (session_id),
  KEY idx_user_time (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE transcript_segment (
  id BIGINT PRIMARY KEY,
  transcript_id BIGINT NOT NULL,
  sequence_no INT NOT NULL,
  speaker_label VARCHAR(16) NOT NULL DEFAULT '',
  text TEXT NOT NULL,
  start_ms BIGINT NOT NULL DEFAULT 0,
  end_ms BIGINT NOT NULL DEFAULT 0,
  confidence DECIMAL(5,4) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_transcript (transcript_id, sequence_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE topic (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  name VARCHAR(256) NOT NULL,
  description TEXT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active', -- suggested|active|dismissed
  created_by VARCHAR(8) NOT NULL DEFAULT 'ai',  -- ai|user
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE memory (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  type VARCHAR(32) NOT NULL,                  -- event|fact|decision|idea|problem|preference
  title VARCHAR(512) NOT NULL DEFAULT '',
  content TEXT NOT NULL,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed', -- observed|inferred|suggested
  importance DECIMAL(5,4) NOT NULL DEFAULT 0.5,
  confidence DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  topic_id BIGINT NULL,
  session_id BIGINT NOT NULL,
  transcript_segment_ids JSON NULL,
  event_at DATETIME(3) NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active', -- active|superseded|dismissed
  embedding LONGBLOB NULL,
  version INT NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user_time (user_id, event_at),
  KEY idx_topic (topic_id),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE todo (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  title VARCHAR(512) NOT NULL,
  source_memory_id BIGINT NULL,
  topic_id BIGINT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'suggested', -- suggested|confirmed|done|dismissed
  due_at DATETIME(3) NULL,
  confidence DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE daily_review (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  review_date DATE NOT NULL,
  content JSON NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|ready|failed
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_user_date (user_id, review_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE agent_message (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  role VARCHAR(16) NOT NULL,                  -- user|assistant
  content TEXT NOT NULL,
  citations JSON NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_user_time (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
