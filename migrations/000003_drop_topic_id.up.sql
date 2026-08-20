-- 收缩期：代码已切到 memory_topic/todo_topic 关联表，删除冗余的单值 topic_id 列与索引。
-- memory 表有 idx_topic 索引（见 000001）；todo 表仅有 topic_id 列、无 idx_topic（000001 即如此）。
ALTER TABLE memory DROP KEY idx_topic, DROP COLUMN topic_id;
ALTER TABLE todo    DROP COLUMN topic_id;
