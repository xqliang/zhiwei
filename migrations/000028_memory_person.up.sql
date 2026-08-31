-- 记忆归属人（2026-08-31 需求）：录音来源的记忆按「来源段说话人绑定的 person」归属——
-- 思敏说的话是思敏的记忆，而不是当前用户的（此前所有记忆都默认算用户头上）。
-- 取值：extract stage 由候选的来源段 speaker_id 多数投票 → person（speaker↔person 双向绑定
-- 不变式已有）；对话来源（conversation_id）与无法归属的记忆为 NULL。
ALTER TABLE memory ADD COLUMN person_id BIGINT UNSIGNED NULL,
  ADD INDEX idx_memory_person (person_id);
