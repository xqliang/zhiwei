-- 区分标题来源：''(未设/占位) | 'manual'(用户手动改) | 'auto'(模型生成)。
-- 用于「自动生成标题」判定：manual 永不覆盖；空/auto 可生成。
-- 合并回 main 前核对 main 最新迁移号，必要时重编号（并行分支撞号坑）。
ALTER TABLE agent_conversation
  ADD COLUMN title_source VARCHAR(16) NOT NULL DEFAULT '' AFTER title;
