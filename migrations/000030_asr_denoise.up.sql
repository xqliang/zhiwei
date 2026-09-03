-- ASR 前降噪（DeepFilterNet3）设置：每用户一行，无行=默认值（全关 + 21dB）。
-- denoise_atten_lim：降噪强度（DeepFilterNet atten-lim，dB）——增强信号与原始信号的
-- 混合比例上限，越大降噪越强（0=不降噪，实测常用 12~40）。
-- denoise_voiceprint：声纹域降噪开关——开启后声纹的录入（speaker stage 自动登记）、
-- 添加（段录音纹/上传录音）、对比（检索基准向量）全部先用降噪音频提向。
CREATE TABLE IF NOT EXISTS asr_settings (
  user_id             BIGINT      NOT NULL PRIMARY KEY,
  denoise_enabled     TINYINT(1)  NOT NULL DEFAULT 0,
  denoise_atten_lim   DOUBLE      NOT NULL DEFAULT 21,
  denoise_voiceprint  TINYINT(1)  NOT NULL DEFAULT 0,
  updated_at          DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
