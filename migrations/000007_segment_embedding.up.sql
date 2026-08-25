-- 逐段声纹向量（speaker stage 提取、此前算完即丢）。用于详情页按「每个 ASR 段」
-- 展示与声纹库的相似度 top-N——一句话可能混多个人，段级相似度才能审计
-- diarization 切分/归属是否正确（段级 top-1 不是归属说话人 → 该段可能切错/归错）。
-- 256×float32 = 1024B/段；仅新处理的会话有值，存量会话为 NULL（重新识别后回填）。
ALTER TABLE transcript_segment ADD COLUMN embedding LONGBLOB NULL AFTER speaker_id;
