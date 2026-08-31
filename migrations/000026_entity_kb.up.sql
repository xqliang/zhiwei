-- 实体知识库（ASR 实体纠错用）：每用户一份。canonical=规范名（纠正目标）。
-- source: auto=流水线刷新时从 person/pet/topic/todo 等同步重建；manual=设置页手动录入（刷新不动）。
-- pinyin=归一化拼音（小写无声调、音节空格分隔，CJK 匹配键）；metaphone=拉丁名/代号的
-- 归一化形（仅小写字母数字，拉丁匹配键）——本列不再用 Double Metaphone 算法（无成熟
-- 维护的 Go 实现），拉丁相似度直接走 Jaro-Winkler（见 internal/entity/phonetic.go）。
CREATE TABLE entity_kb (
  id         BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,
  canonical  VARCHAR(128) NOT NULL,
  kind       VARCHAR(32)  NOT NULL,
  pinyin     VARCHAR(256) NULL,
  metaphone  VARCHAR(64)  NULL,
  source     VARCHAR(16)  NOT NULL DEFAULT 'auto',
  source_ref VARCHAR(64)  NULL,
  enabled    TINYINT(1)   NOT NULL DEFAULT 1,
  note       VARCHAR(256) NULL,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_entity_kb (user_id, canonical, kind),
  KEY idx_entity_kb_user (user_id, enabled)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 实体纠错功能配置（每用户一份）。
-- auto_sources: JSON 数组，自动入库的 kind 列表（如 ["person","pet","project","task","topic","speaker"]）。
CREATE TABLE entity_settings (
  user_id              BIGINT UNSIGNED NOT NULL,
  correction_enabled   TINYINT(1)   NOT NULL DEFAULT 1,
  confidence_threshold DECIMAL(3,2) NOT NULL DEFAULT 0.80,
  auto_sources         JSON NULL,
  updated_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 转写段实体纠错明细：该段被应用的纠正数组
-- [{orig, corrected, canonical, confidence}]，配合 corrected_reason='entity'（徽章+对照展示）。
ALTER TABLE transcript_segment
  ADD COLUMN entity_edits JSON NULL COMMENT '实体纠错明细数组';
