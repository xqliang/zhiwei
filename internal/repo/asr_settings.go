// asr_settings.go：ASR 前降噪（DeepFilterNet3）的每用户配置存取。
// 模式对齐 entity_settings：每用户一行，无行=默认值（关 + 21dB）而非错误——
// asr stage 与设置页 GET 都直接可用。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

// AsrSettings ASR 降噪配置（每用户一行）。
// DenoiseAttenLim 是 DeepFilterNet 的 atten-lim（dB）：增强信号与原始信号的混合
// 上限——越大降噪越强，0 等效不降噪；常用 12~40，默认 21。
// DenoiseVoiceprint 是「声纹域降噪」开关：开启后声纹的录入（speaker stage 自动
// 登记）、添加（段录音纹/上传录音）、对比（检索基准向量）全部先用降噪音频提向。
// 注意声纹域一致性：既有声纹库从未降噪音频登记——DFN3 对干净语音近似透传（向量
// 几乎不变），混域影响小；但嘈杂环境下新旧域混用会引入额外方差，切换开关后建议
// 对重要人物重新录声纹。
type AsrSettings struct {
	UserID            int64     `db:"user_id" json:"user_id"`
	DenoiseEnabled    bool      `db:"denoise_enabled" json:"denoise_enabled"`
	DenoiseAttenLim   float64   `db:"denoise_atten_lim" json:"denoise_atten_lim"`
	DenoiseVoiceprint bool      `db:"denoise_voiceprint" json:"denoise_voiceprint"`
	UpdatedAt         time.Time `db:"updated_at" json:"updated_at"`
}

// AsrSettingsRepo ASR 降噪配置存取。
type AsrSettingsRepo struct{ DB *sqlx.DB }

// AsrDenoiseAttenRange 降噪强度的合法区间（dB）。DeepFilterNet 的 atten-lim 超过
// ~40dB 后效果趋同（人耳/ASR 无感），上限 100 只是防御性收口。
var AsrDenoiseAttenRange = [2]float64{0, 100}

// Get 读配置；从未配置（无行）返回默认值（关闭 + 21dB）而非错误。
func (r *AsrSettingsRepo) Get(ctx context.Context, userID int64) (*AsrSettings, error) {
	var s AsrSettings
	err := r.DB.QueryRowxContext(ctx,
		`SELECT user_id, denoise_enabled, denoise_atten_lim, denoise_voiceprint, updated_at
		 FROM asr_settings WHERE user_id = ?`, userID).
		Scan(&s.UserID, &s.DenoiseEnabled, &s.DenoiseAttenLim, &s.DenoiseVoiceprint, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return &AsrSettings{UserID: userID, DenoiseEnabled: false, DenoiseAttenLim: 21, DenoiseVoiceprint: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Upsert 写配置（单用户一行）。atten 越界（∉[0,100]）在应用层拒绝。
func (r *AsrSettingsRepo) Upsert(ctx context.Context, userID int64, enabled bool, attenLim float64, voiceprint bool) error {
	if attenLim < AsrDenoiseAttenRange[0] || attenLim > AsrDenoiseAttenRange[1] {
		return errors.New("降噪强度须在 [0,100] dB")
	}
	_, err := r.DB.ExecContext(ctx,
		`INSERT INTO asr_settings (user_id, denoise_enabled, denoise_atten_lim, denoise_voiceprint)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE denoise_enabled = VALUES(denoise_enabled),
		                           denoise_atten_lim = VALUES(denoise_atten_lim),
		                           denoise_voiceprint = VALUES(denoise_voiceprint)`,
		userID, enabled, attenLim, voiceprint)
	return err
}
