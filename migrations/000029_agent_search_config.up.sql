-- 联网搜索配置（Phase 2）：与 identity/soul 共用 agent_config 单例行。
-- search_engine: auto|bing|duckduckgo|tavily（默认 auto = 免 key 引擎链优先）
-- search_api_key: 可选；tavily 等付费后端用，免 key 后端留空(NULL)
ALTER TABLE agent_config
  ADD COLUMN search_engine  VARCHAR(32) NOT NULL DEFAULT 'auto',
  ADD COLUMN search_api_key TEXT        NULL;
