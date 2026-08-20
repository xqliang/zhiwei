package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// QueryHandler 会话/任务查询。
type QueryHandler struct {
	Sessions    *repo.SessionRepo
	Jobs        *repo.JobRepo
	Transcripts *repo.TranscriptRepo
	Memories    *repo.MemoryRepo // Sprint 2：详情附带 memory 卡片
	Todos       *repo.TodoRepo   // Sprint 2：详情附带 todo 卡片
}

// RegisterQuery 挂载查询路由。
func RegisterQuery(r chi.Router, h *QueryHandler) {
	r.Get("/api/sessions", h.ListSessions)
	r.Get("/api/sessions/{id}", h.GetSession)
	r.Get("/api/sessions/{id}/audio", h.ServeAudio)
	r.Post("/api/jobs/{id}/retry", h.RetryJob)
}

func (h *QueryHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 50)
	list, err := h.Sessions.List(r.Context(), limit, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 附带每个 session 最新 job 状态，前端展示处理进度
	type row struct {
		repo.AudioSession
		JobStatus string `json:"job_status,omitempty"`
		JobStage  string `json:"job_stage,omitempty"`
	}
	out := make([]row, len(list))
	for i, s := range list {
		out[i] = row{AudioSession: s}
		if s.JobID != nil {
			if j, err := h.Jobs.Get(r.Context(), *s.JobID); err == nil {
				out[i].JobStatus, out[i].JobStage = j.Status, j.Stage
			}
		}
	}
	writeJSON(w, map[string]any{"sessions": out})
}

type segmentView struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

func (h *QueryHandler) GetSession(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	s, err := h.Sessions.Get(r.Context(), sid)
	if err != nil {
		http.Error(w, "session 不存在", http.StatusNotFound)
		return
	}
	resp := map[string]any{"session": s}
	if tr, err := h.Transcripts.GetBySession(r.Context(), sid); err == nil {
		segs, _ := h.Transcripts.ListSegments(r.Context(), tr.ID)
		views := make([]segmentView, len(segs))
		for i, sg := range segs {
			views[i] = segmentView{
				Speaker: speakerLabelName(sg.SpeakerLabel), Text: sg.Text,
				StartMS: sg.StartMS, EndMS: sg.EndMS,
			}
		}
		resp["transcript"] = tr
		resp["segments"] = views
	}
	// Sprint 2：详情附带 memory/todo 卡片（repo 为空则跳过，兼容旧装配）
	if h.Memories != nil {
		if mems, err := h.Memories.ListBySession(r.Context(), sid); err == nil {
			resp["memories"] = mems
		}
	}
	if h.Todos != nil {
		if todos, err := h.Todos.ListBySession(r.Context(), sid); err == nil {
			resp["todos"] = todos
		}
	}
	if s.JobID != nil {
		if j, err := h.Jobs.Get(r.Context(), *s.JobID); err == nil {
			resp["job"] = j
		}
	}
	writeJSON(w, resp)
}

func (h *QueryHandler) RetryJob(w http.ResponseWriter, r *http.Request) {
	jid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	j, err := h.Jobs.Get(r.Context(), jid)
	if err != nil {
		http.Error(w, "job 不存在", http.StatusNotFound)
		return
	}
	if j.Status != "failed" {
		http.Error(w, "仅 failed 状态可重跑", http.StatusConflict)
		return
	}
	j.Status = "pending"
	j.Attempt = 0
	j.LastError = nil
	if err := h.Jobs.Save(r.Context(), j); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"job": j})
}

// ServeAudio 流式返回会话的原始音频文件（时间线播放用）。
// StoragePath 是服务端落盘路径，不通过 JSON 外泄；此处用 http.ServeFile
// 按扩展名推断 Content-Type，支持 Range 请求（拖动进度条）。
func (h *QueryHandler) ServeAudio(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	s, err := h.Sessions.Get(r.Context(), sid)
	if err != nil {
		http.Error(w, "session 不存在", http.StatusNotFound)
		return
	}
	if s.Mime != "" {
		w.Header().Set("Content-Type", s.Mime)
	}
	http.ServeFile(w, r, s.StoragePath)
}

// speakerLabelName "1" -> "说话人 1"；空标签 -> "未知说话人"。
func speakerLabelName(label string) string {
	if label == "" {
		return "未知说话人"
	}
	return "说话人 " + label
}

func intQuery(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 200 {
		return def
	}
	return n
}

// intOffset 解析 offset 查询参数，非法或负数归零。
func intOffset(r *http.Request) int {
	v := r.URL.Query().Get("offset")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
