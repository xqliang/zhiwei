-- entity_disabled：被用户「禁用」的自动实体名（去拷贝化后的持久停用机制）。
-- 实时聚合(AssembleEntities)不再维护 entity_kb 的 auto 拷贝，自动实体无稳定行可置 enabled=0；
-- 改为把「想持久停用」的名字记在此表，纠错白名单组装时(mergeWhitelist)按 canonical 剔除。
-- 与 entity_kb 的 ai_ci 唯一键一致，canonical 大小写不敏感(写入方统一 ToLower 归一)。
CREATE TABLE entity_disabled (
  user_id    BIGINT UNSIGNED NOT NULL,
  canonical  VARCHAR(128) NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_entity_disabled (user_id, canonical),
  KEY idx_entity_disabled_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 数据搬运（去拷贝化前存量 entity_kb 的 auto 行处理；均幂等，可重跑）：
-- 1) 被禁用的 auto 名 → entity_disabled（持久停用，跨实时刷新保留）。
INSERT IGNORE INTO entity_disabled (user_id, canonical)
SELECT user_id, canonical FROM entity_kb WHERE source = 'auto' AND enabled = 0;
-- 2) 删除全部 auto 拷贝（启用/禁用都删；禁用态已迁走，启用态由实时聚合重建）。
DELETE FROM entity_kb WHERE source = 'auto';
-- 3) auto_sources 里移除 task（待办不再进实体词，见需求②）。
UPDATE entity_settings
SET auto_sources = JSON_REMOVE(auto_sources, JSON_UNQUOTE(JSON_SEARCH(auto_sources, 'one', 'task')))
WHERE JSON_SEARCH(auto_sources, 'one', 'task') IS NOT NULL;
