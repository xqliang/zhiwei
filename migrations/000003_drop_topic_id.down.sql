-- 反向：重建 topic_id 列与索引（NULL；多→单无法无损还原，仅恢复结构）。
-- 与 000001 一致：memory 带 idx_topic，todo 不带（原 schema 即如此，down→up 幂等）。
ALTER TABLE memory ADD COLUMN topic_id BIGINT NULL, ADD KEY idx_topic (topic_id);
ALTER TABLE todo    ADD COLUMN topic_id BIGINT NULL;
