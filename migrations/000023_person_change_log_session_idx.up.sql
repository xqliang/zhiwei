-- person_change_log 补 session_id 索引（2026-08-28）：转写详情 timeline 展示「该录音触发的
-- profile 平面变更」按 session_id 过滤该审计表。此表只追加、无限增长，新查询会落到用户请求路径
-- GET /api/sessions/{id}，无索引则全表扫描且随时间退化；sibling 表 person_attribute /
-- person_relationship 均有 idx_session，此处补齐一致性。
ALTER TABLE person_change_log ADD INDEX idx_session (session_id);
