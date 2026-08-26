-- 画像 P5 跟进（spec §13 F5 后续）：人物删除（原「归档」）的**级联恢复**支持。
-- 问题：归档级联把六平面 active/pending 行批量置 dismissed 时，不记这些行 dismiss 前的
-- 状态——恢复人物后无法区分「级联 dismissed 的行」和「用户手动删的行」，导致恢复语义
-- 只能是「人物回来、平面数据全丢」（service_manual.go 旧注释里的刻意取舍）。
-- 方案：六平面各加一列 pre_dismiss_status VARCHAR(16) NULL：
--   - 归档级联 dismiss 时写入该行**dismiss 前的状态**（active|pending）；
--   - 手动删除（SetStatusExt 单行置 dismissed）不碰这列 → 保持 NULL；
--   - 恢复时只翻回 status='dismissed' AND pre_dismiss_status IS NOT NULL 的行
--     （status=pre_dismiss_status，同时清 NULL），手动删过的行天然不被误恢复。
-- person 表本身不需要：人物恢复目标状态由 API 指定（active），无需记忆。
-- 无需额外索引：恢复按 person_id 过滤，各表已有 idx_person* 前缀索引覆盖。
-- 注：MySQL 列定义内 COMMENT 必须在 AFTER 之前，否则 1064 语法错。

ALTER TABLE person_attribute
  ADD COLUMN pre_dismiss_status VARCHAR(16) NULL COMMENT '归档级联 dismiss 前的状态（active|pending）；NULL=非级联（手动删/正常行）' AFTER status;

ALTER TABLE person_relationship
  ADD COLUMN pre_dismiss_status VARCHAR(16) NULL COMMENT '归档级联 dismiss 前的状态（active|pending）；NULL=非级联（手动删/正常行）' AFTER status;

ALTER TABLE person_event
  ADD COLUMN pre_dismiss_status VARCHAR(16) NULL COMMENT '归档级联 dismiss 前的状态（active|pending）；NULL=非级联（手动删/正常行）' AFTER status;

ALTER TABLE person_metric
  ADD COLUMN pre_dismiss_status VARCHAR(16) NULL COMMENT '归档级联 dismiss 前的状态（active|pending）；NULL=非级联（手动删/正常行）' AFTER status;

ALTER TABLE person_cycle
  ADD COLUMN pre_dismiss_status VARCHAR(16) NULL COMMENT '归档级联 dismiss 前的状态（active|pending）；NULL=非级联（手动删/正常行）' AFTER status;

ALTER TABLE person_activity
  ADD COLUMN pre_dismiss_status VARCHAR(16) NULL COMMENT '归档级联 dismiss 前的状态（active|pending）；NULL=非级联（手动删/正常行）' AFTER status;
