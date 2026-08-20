-- 代办/记忆 ↔ topic 多对多关联表（spec §3）。
-- 本迁移仅「扩张」：新增关联表 + 回填存量单值 topic_id；保留 topic_id 列，
-- 由 000003 在代码切换到关联表后删除（expand/contract，保增量 green 提交）。
CREATE TABLE memory_topic (
  memory_id  BIGINT NOT NULL,
  topic_id   BIGINT NOT NULL,
  source     VARCHAR(8) NOT NULL DEFAULT 'ai',  -- ai=抽取自动, user=手动
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (memory_id, topic_id),
  KEY idx_topic (topic_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE todo_topic (
  todo_id  BIGINT NOT NULL,
  topic_id BIGINT NOT NULL,
  source   VARCHAR(8) NOT NULL DEFAULT 'ai',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (todo_id, topic_id),
  KEY idx_topic (topic_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 存量单值关联回填进关联表（source='ai'）
INSERT IGNORE INTO memory_topic (memory_id, topic_id, source)
  SELECT id, topic_id, 'ai' FROM memory WHERE topic_id IS NOT NULL;
INSERT IGNORE INTO todo_topic (todo_id, topic_id, source)
  SELECT id, topic_id, 'ai' FROM todo WHERE topic_id IS NOT NULL;
