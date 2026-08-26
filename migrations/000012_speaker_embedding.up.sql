-- 画像声纹多条样本（2026-08-26 需求）：一个人可录多条声纹；合并说话人时声纹**累加**而非丢弃；
-- 每条可写备注、显示创建时间。
-- 模型：speaker_embedding 一行 = 一条声纹样本（向量 + 备注 + 来源 + 样本数 + 创建时间）。
-- speaker.embedding 保持为**聚合代表**（全部样本向量均值 + L2 归一），仍是 FAISS 1:N 检索
-- 用的唯一向量——索引结构不变，只改「代表怎么算」：从单样本覆盖变成多样本聚合。
-- speaker.sample_count 相应 = 全部样本 sample_count 之和。
-- 回填不在 SQL 做（雪花 id 须 Go 侧生成）：repo.EnsureSpeakerEmbeddingBootstrap 启动幂等回填，
-- 对齐 person 表 000006 的 EnsurePersonBootstrap 模式。

CREATE TABLE speaker_embedding (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  speaker_id   BIGINT NOT NULL,                      -- 所属说话人
  note         VARCHAR(256) NULL,                    -- 备注（如「安静房间录」「会议室 8 月」；可空）
  embedding    LONGBLOB NOT NULL,                    -- 256×float32=1024B，与 speaker.embedding 同格式，不外泄
  sample_count INT NOT NULL DEFAULT 1,               -- 该条聚合的段向量数（手动录=1、抽取聚合=N）
  source       VARCHAR(8) NOT NULL DEFAULT 'manual', -- manual=手动录 | auto=抽取自动登记 | merge=合并迁入
  created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_speaker (speaker_id),
  KEY idx_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
