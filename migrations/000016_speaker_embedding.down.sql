-- 000012 回滚：删除多条声纹表。speaker.embedding 聚合代表仍在（从未被 drop），
-- 只是丢失「多条样本 + 备注」信息，退回单向量模型。
DROP TABLE IF EXISTS speaker_embedding;
