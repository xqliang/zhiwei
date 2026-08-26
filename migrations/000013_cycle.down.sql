-- 只回滚 person_cycle；person_metric 由 feat 000011_metric.down 负责（合并对账去撞表）。
DROP TABLE IF EXISTS person_cycle;
