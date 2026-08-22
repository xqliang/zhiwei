-- 说话人声纹名册（speaker-voiceprint 特性，spec §5）。
-- 实际声纹向量存 FAISS（Python sidecar），embedding LONGBLOB 仅作灾备/重建索引用，
-- 与 memory.embedding 的 LONGBLOB 备份模式一致。
-- transcript_segment 增 speaker_id 外键列：后续流水线阶段把每个 ASR 说话人轮次
-- 解析为已登记 speaker 后回填（此前为 NULL）。
CREATE TABLE speaker (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  name         VARCHAR(128) NOT NULL,
  source       VARCHAR(8) NOT NULL DEFAULT 'auto',   -- enrolled=用户登记 | auto=自动聚类
  status       VARCHAR(16) NOT NULL DEFAULT 'active', -- active | dismissed
  embedding    LONGBLOB NULL,                         -- 256×float32=1024B 声纹备份，不外泄
  sample_count INT NOT NULL DEFAULT 0,                -- 参与建模的样本片段数
  created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- transcript_segment 增说话人外键列 + 索引（放在 speaker_label 之后，语义相邻）。
ALTER TABLE transcript_segment ADD COLUMN speaker_id BIGINT NULL AFTER speaker_label;
ALTER TABLE transcript_segment ADD KEY idx_speaker (speaker_id);
