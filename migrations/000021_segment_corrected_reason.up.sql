-- 统一自动纠正标记（2026-08-28 需求）：区分两类自动改判。
-- phantom=幽灵历史声纹改判(配 corrected_from_speaker_id) | short=过短噪声段并入最近在场说话人(corrected_from 为 NULL)。
ALTER TABLE transcript_segment
  ADD COLUMN corrected_reason VARCHAR(16) NULL
  COMMENT '自动纠正原因 phantom|short；NULL=未纠正';
