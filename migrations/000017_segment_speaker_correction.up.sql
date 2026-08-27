-- 幽灵历史声纹纠正（2026-08-27 需求）：ASR 过度切分出的组常命中历史库某真人；纠正 pass
-- 把这类组整组改判给同录音里真正解释它的在场说话人，并在段上记录被顶掉的原历史说话人 id。
-- 非 NULL = 该段被自动纠正过 → 前端"已修改"徽章 + 审计 + 手动改回依据。
ALTER TABLE transcript_segment
  ADD COLUMN corrected_from_speaker_id BIGINT NULL
  COMMENT '幽灵历史声纹纠正：被自动顶掉的原历史说话人 id；非 NULL=该段已被自动改判';
