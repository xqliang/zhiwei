-- 反向：先摘除 transcript_segment 的 speaker_id 索引与列，再删 speaker 表
-- （顺序与 up 相反，避免残留悬空引用）。
ALTER TABLE transcript_segment DROP KEY idx_speaker;
ALTER TABLE transcript_segment DROP COLUMN speaker_id;
DROP TABLE IF EXISTS speaker;
