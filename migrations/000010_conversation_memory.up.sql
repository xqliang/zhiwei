-- 对话转记忆（Plan 3b）数据层：见 spec §6.3 / §12。
-- 承接 000009_agent 顶部预告：memory 加 conversation_id + session_id 放宽可空。
-- 【迁移号协调：已重编号】原为 000006_conversation_memory，与 main 的 000006_person 撞号，
--            已重编号为 000010_conversation_memory（排在 main v8 之后；仅 ALTER 000001 期的 memory）。

-- 1) 新增对话溯源列（可空；录音来源的记忆此列为 NULL）。
ALTER TABLE memory ADD COLUMN conversation_id BIGINT NULL AFTER session_id;

-- 2) 放宽 session_id 为可空（对话来源的记忆此列为 NULL；录音来源仍写 session_id）。
--    无 FK，直接 MODIFY；保持 BIGINT 类型与其余属性不变。
ALTER TABLE memory MODIFY COLUMN session_id BIGINT NULL;

-- 3) conversation_id 检索/幂等删除用索引（按会话删旧记忆、按会话查）。
ALTER TABLE memory ADD KEY idx_mem_conversation (conversation_id);
