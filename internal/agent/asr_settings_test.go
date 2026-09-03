package agent

// asr_settings_test.go：设置页「音频降噪」端点测试（GET 默认值 / PUT 指针合并 / 越界 400）。
// 复用 entity_test.go 的 injectUser + RegisterAgent 装配模式。
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

func asrSettingsHandler(t *testing.T, uid int64) *AgentHandler {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	clean := func() { _, _ = db.Exec("DELETE FROM asr_settings WHERE user_id = ?", uid) }
	clean()
	t.Cleanup(clean)
	return &AgentHandler{AsrSettings: &repo.AsrSettingsRepo{DB: db}}
}

func doASR(h *AgentHandler, uid int64, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Use(injectUser(uid))
	RegisterAgent(r, h)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestASRSettingsAPI(t *testing.T) {
	const uid = int64(7101)
	h := asrSettingsHandler(t, uid)

	// 1) GET 默认：无行 → 关 + 21dB。
	rec := doASR(h, uid, "GET", "/api/agent/asr-settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		DenoiseEnabled    bool    `json:"denoise_enabled"`
		DenoiseAttenLim   float64 `json:"denoise_atten_lim"`
		DenoiseVoiceprint bool    `json:"denoise_voiceprint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DenoiseEnabled || got.DenoiseAttenLim != 21 || got.DenoiseVoiceprint {
		t.Fatalf("默认应为 关+21dB+声纹关: %s", rec.Body.String())
	}

	// 2) PUT 全量保存 → 读回。
	rec = doASR(h, uid, "PUT", "/api/agent/asr-settings",
		`{"denoise_enabled":true,"denoise_atten_lim":35,"denoise_voiceprint":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doASR(h, uid, "GET", "/api/agent/asr-settings", "")
	json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.DenoiseEnabled || got.DenoiseAttenLim != 35 || !got.DenoiseVoiceprint {
		t.Fatalf("保存后应为 开+35dB+声纹开: %s", rec.Body.String())
	}

	// 3) PUT 指针合并：只传 enabled，强度保持 35。
	rec = doASR(h, uid, "PUT", "/api/agent/asr-settings", `{"denoise_enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 合并 code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doASR(h, uid, "GET", "/api/agent/asr-settings", "")
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.DenoiseEnabled || got.DenoiseAttenLim != 35 || !got.DenoiseVoiceprint {
		t.Fatalf("合并后应为 关+35dB+声纹开（未传字段保持现值）: %s", rec.Body.String())
	}

	// 4) 强度越界 → 400（不落库）。
	rec = doASR(h, uid, "PUT", "/api/agent/asr-settings", `{"denoise_atten_lim":101}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界应 400，code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doASR(h, uid, "GET", "/api/agent/asr-settings", "")
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.DenoiseAttenLim != 35 || !got.DenoiseVoiceprint {
		t.Fatalf("越界请求不应改值: %s", rec.Body.String())
	}

	// 5) 未装配（AsrSettings=nil）→ 503。
	rec = doASR(&AgentHandler{}, uid, "GET", "/api/agent/asr-settings", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503，code=%d", rec.Code)
	}
}
