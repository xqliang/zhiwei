-- 画像 pet（宠物）平面：一只宠物一行，挂人物名下（spec 2026-08-27-pet-plane-design.md）。
-- 自然键 = (person_id, name)：同人同名视为同一只（nickname 不参与匹配）。
-- 有版本取代语义：同名更新走 supersede（字段级合并整行重写），不同名 = 新宠物追加。
CREATE TABLE person_pet (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  person_id    BIGINT NOT NULL,
  name         VARCHAR(64) NOT NULL,               -- 宠物名（自然键成分）
  nickname     VARCHAR(64) NULL,                   -- 小名
  species      VARCHAR(16) NOT NULL,               -- 类别：狗|猫|鸟|鱼|兔|仓鼠|爬行|其他（解析层收敛，库不设 CHECK）
  breed        VARCHAR(64) NULL,                   -- 品种自由文本（柯基/布偶猫…）
  gender       VARCHAR(8) NULL,                    -- 公|母（不做强枚举校验）
  age_text     VARCHAR(32) NULL,                   -- 年龄原始表述（「3岁」「8个月」）
  birthday     DATE NULL,                          -- 生日（LLM 按年龄估算；手动录入必填）
  likes        VARCHAR(256) NULL,                  -- 喜好/习惯
  -- 横切字段（与既有平面一致，spec §3）
  confidence    DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed',
  source        VARCHAR(8) NOT NULL DEFAULT 'manual',
  status        VARCHAR(16) NOT NULL DEFAULT 'active',
  pre_dismiss_status VARCHAR(16) NULL,             -- 级联 dismiss 前状态（人物删除/恢复级联用）
  session_id    BIGINT NULL,
  memory_id     BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id BIGINT NULL,
  version       INT NOT NULL DEFAULT 1,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_status (person_id, status),
  KEY idx_person_name (person_id, name),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
