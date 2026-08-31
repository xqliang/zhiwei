-- 回滚 Phase 2 搜索配置列。
ALTER TABLE agent_config
  DROP COLUMN search_api_key,
  DROP COLUMN search_engine;
