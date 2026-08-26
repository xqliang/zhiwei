-- 000011 回滚：去掉六平面的 pre_dismiss_status 列。
-- 注意：回滚会丢失「哪些行是级联 dismissed」的标记——回滚后再归档/恢复将退回
-- 「恢复不带回平面数据」的旧语义。仅开发环境使用。
ALTER TABLE person_activity   DROP COLUMN pre_dismiss_status;
ALTER TABLE person_cycle      DROP COLUMN pre_dismiss_status;
ALTER TABLE person_metric     DROP COLUMN pre_dismiss_status;
ALTER TABLE person_event      DROP COLUMN pre_dismiss_status;
ALTER TABLE person_relationship DROP COLUMN pre_dismiss_status;
ALTER TABLE person_attribute  DROP COLUMN pre_dismiss_status;
