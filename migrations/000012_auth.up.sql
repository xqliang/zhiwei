-- 多用户阶段1：用户表 + 服务端会话表（cookie 存 token，服务端查表定 userID）。
-- 现有数据全 user_id=1 → 播种 id=1 的 owner 用户，存量数据归它。
CREATE TABLE app_user (
  id BIGINT PRIMARY KEY,
  username VARCHAR(64) NOT NULL,
  password_hash VARCHAR(100) NOT NULL DEFAULT '', -- bcrypt；空=未设密码(首登由 ZW_OWNER_PASSWORD 引导)
  display_name VARCHAR(64) NOT NULL DEFAULT '',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_user_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO app_user (id, username, display_name) VALUES (1, 'owner', '我');

CREATE TABLE user_session (
  token CHAR(64) PRIMARY KEY,                      -- 随机 32 字节 hex
  user_id BIGINT NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  expires_at DATETIME(3) NOT NULL,
  KEY idx_user_session_user (user_id),
  KEY idx_user_session_exp (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
