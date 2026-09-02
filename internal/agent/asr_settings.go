// asr_settings.go：设置页「音频降噪」端点（ASR 前 DeepFilterNet3 降噪的开关+强度）。
// 契约对齐 entity-settings：GET 返回配置，PUT 指针合并（未传字段保持现值），
// 强度越界 [0,100] → 400。路由经 registerASRSettingsRoutes 挂载（RegisterAgent 调用）。
package agent

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/repo"
)

func registerASRSettingsRoutes(r chi.Router, h *AgentHandler) {
	r.Get("/api/agent/asr-settings", h.getASRSettings)
	r.Put("/api/agent/asr-settings", h.putASRSettings)
}

// getASRSettings 返回降噪配置（无行=默认关+21dB，repo 层兜底）。
func (h *AgentHandler) getASRSettings(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.AsrSettings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "降噪配置不可用"})
		return
	}
	st, err := h.AsrSettings.Get(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"denoise_enabled":   st.DenoiseEnabled,
		"denoise_atten_lim": st.DenoiseAttenLim,
	})
}

// putASRSettings 保存降噪配置（指针合并；强度越界 [0,100] → 400）。
// 保存即时对新录音生效（asr stage 每次读库）；已在途/已完成的录音不受影响
// （降噪产物 {sid}.denoised.wav 幂等复用，见 pipeline.denoisedWAVPath）。
func (h *AgentHandler) putASRSettings(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.AsrSettings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "降噪配置不可用"})
		return
	}
	var body struct {
		DenoiseEnabled  *bool    `json:"denoise_enabled"`
		DenoiseAttenLim *float64 `json:"denoise_atten_lim"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	cur, err := h.AsrSettings.Get(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	enabled, atten := cur.DenoiseEnabled, cur.DenoiseAttenLim
	if body.DenoiseEnabled != nil {
		enabled = *body.DenoiseEnabled
	}
	if body.DenoiseAttenLim != nil {
		if *body.DenoiseAttenLim < repo.AsrDenoiseAttenRange[0] || *body.DenoiseAttenLim > repo.AsrDenoiseAttenRange[1] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "denoise_atten_lim 须在 [0,100] dB"})
			return
		}
		atten = *body.DenoiseAttenLim
	}
	if err := h.AsrSettings.Upsert(r.Context(), uid, enabled, atten); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"denoise_enabled": enabled, "denoise_atten_lim": atten,
	})
}
