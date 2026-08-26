package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/auth"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// AudioHandler 处理音频上传。
type AudioHandler struct {
	Sessions *repo.SessionRepo
	Jobs     *repo.JobRepo
	DataDir  string
}

// RegisterAudio 挂载音频相关路由。
func RegisterAudio(r chi.Router, sessions *repo.SessionRepo, jobs *repo.JobRepo, dataDir string) {
	h := &AudioHandler{Sessions: sessions, Jobs: jobs, DataDir: dataDir}
	r.Post("/api/audio", h.Upload)
}

var allowedExt = map[string]string{
	".wav": "audio/wav", ".mp3": "audio/mpeg", ".m4a": "audio/mp4",
	".webm": "audio/webm", ".ogg": "audio/ogg", ".flac": "audio/flac",
}

const maxUploadBytes = 200 << 20 // 200MB

func (h *AudioHandler) Upload(w http.ResponseWriter, r *http.Request) {
	// 多租户隔离（评审 I3）：取登录用户，未登录 401；入库时写 user_id，
	// 使录音归属登录用户（而非默认恒 1），后续列表/详情/删除按 user_id 隔离才生效。
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "解析上传失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少 file 字段", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	mime, ok := allowedExt[ext]
	if !ok {
		http.Error(w, fmt.Sprintf("不支持的音频格式: %s", ext), http.StatusUnsupportedMediaType)
		return
	}
	source := r.FormValue("source")
	if source != "web_record" {
		source = "web_upload"
	}

	if err := os.MkdirAll(filepath.Join(h.DataDir, "uploads"), 0o755); err != nil {
		http.Error(w, "存储目录创建失败", http.StatusInternalServerError)
		return
	}
	sid := ids.New()
	dst := filepath.Join(h.DataDir, "uploads", sid.String()+ext)
	out, err := os.Create(dst)
	if err != nil {
		http.Error(w, "文件创建失败", http.StatusInternalServerError)
		return
	}
	size, err := io.Copy(out, file)
	out.Close()
	if err != nil {
		os.Remove(dst)
		http.Error(w, "文件写入失败", http.StatusInternalServerError)
		return
	}

	s := &repo.AudioSession{
		ID: sid, UserID: uid.Int64(), Source: source, Filename: header.Filename,
		StoragePath: dst, DurationMS: 0, Mime: mime, Status: "processing",
	}
	if err := h.Sessions.Create(r.Context(), s); err != nil {
		http.Error(w, "session 入库失败", http.StatusInternalServerError)
		return
	}
	j := &repo.Job{SessionID: sid, Stage: "asr", Status: "pending"}
	if err := h.Jobs.Create(r.Context(), j); err != nil {
		http.Error(w, "job 入库失败", http.StatusInternalServerError)
		return
	}
	_ = h.Sessions.SetJobID(r.Context(), sid, j.ID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id": sid,
		"job_id":     j.ID,
		"size":       size,
	})
}
