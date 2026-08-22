package api

import (
	"encoding/json"
	"net/http"
	"os"
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
	Memories    *repo.MemoryRepo  // Sprint 2：详情附带 memory 卡片
	Todos       *repo.TodoRepo    // Sprint 2：详情附带 todo 卡片
	Speakers    *repo.SpeakerRepo // speaker stage：详情附带段说话人 + speakers 列表
}

// RegisterQuery 挂载查询路由。
func RegisterQuery(r chi.Router, h *QueryHandler) {
	r.Get("/api/sessions", h.ListSessions)
	r.Get("/api/sessions/{id}", h.GetSession)
	r.Patch("/api/sessions/{id}/transcript", h.PatchTranscript)
	r.Post("/api/sessions/{id}/reextract", h.Reextract)
	r.Delete("/api/sessions/{id}", h.DeleteSession)
	r.Get("/api/sessions/{id}/audio", h.ServeAudio)
	r.Post("/api/jobs/{id}/retry", h.RetryJob)
}

// ListSessions 列出会话，每行富化 asr_preview（转写前 120 字）+ memory_count +
// todo_count（单 SQL 相关子查询，避免 N+1），并附最新 job 状态（处理进度）。
// asr_full 不外泄（json:"-"），仅截断后以 asr_preview 输出。
func (h *QueryHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 50)
	type row struct {
		repo.AudioSession
		JobStatus   string `json:"job_status,omitempty"`
		JobStage    string `json:"job_stage,omitempty"`
		MemoryCount int    `db:"memory_count" json:"memory_count"`
		TodoCount   int    `db:"todo_count" json:"todo_count"`
		AsrFull     string `db:"asr_full" json:"-"` // GROUP_CONCAT 全文，截断后给 AsrPreview
		AsrPreview  string `db:"-" json:"asr_preview"`
	}
	var rows []row
	err := h.Sessions.DB.SelectContext(r.Context(), &rows, `
SELECT s.*,
  (SELECT COUNT(*) FROM memory WHERE session_id = s.id AND status = 'active') AS memory_count,
  (SELECT COUNT(*) FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = s.id) AND status != 'dismissed') AS todo_count,
  (SELECT IFNULL(GROUP_CONCAT(seg.text ORDER BY seg.start_ms SEPARATOR ''), '')
     FROM transcript_segment seg JOIN transcript tr ON tr.id = seg.transcript_id
     WHERE tr.session_id = s.id) AS asr_full
FROM audio_session s ORDER BY s.id DESC LIMIT ?`, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]row, len(rows))
	for i, s := range rows {
		out[i] = s
		// asr_full 截 120 runes（够卡片预览；GROUP_CONCAT 默认上限 1024 够取前 120）
		if rs := []rune(s.AsrFull); len(rs) > 120 {
			out[i].AsrPreview = string(rs[:120]) + "…"
		} else {
			out[i].AsrPreview = s.AsrFull
		}
		if s.JobID != nil {
			if j, err := h.Jobs.Get(r.Context(), *s.JobID); err == nil {
				out[i].JobStatus, out[i].JobStage = j.Status, j.Stage
			}
		}
	}
	writeJSON(w, map[string]any{"sessions": out})
}

type segmentView struct {
	ID        string `json:"id"`
	Speaker   string `json:"speaker"`              // 显示名：解析到用登记名，否则 "说话人 N"
	SpeakerID string `json:"speaker_id,omitempty"` // 解析到的已登记说话人 id（未解析则空）
	Text      string `json:"text"`
	StartMS   int64  `json:"start_ms"`
	EndMS     int64  `json:"end_ms"`
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
		// 已解析说话人 id→name 映射，避免逐段 N+1 查 speaker；sis 同时是面板用的说话人列表。
		sis, _ := h.Transcripts.ListSpeakersForTranscript(r.Context(), tr.ID)
		spMap := make(map[ids.ID]string, len(sis))
		for i := range sis {
			sis[i].ColorIndex = i // 按转写出现序号着色
			spMap[sis[i].SpeakerID] = sis[i].Name
		}
		views := make([]segmentView, len(segs))
		for i, sg := range segs {
			views[i] = segmentView{
				ID: sg.ID.String(), Text: sg.Text, StartMS: sg.StartMS, EndMS: sg.EndMS,
			}
			if sg.SpeakerID != nil {
				views[i].SpeakerID = sg.SpeakerID.String()
				if name, ok := spMap[*sg.SpeakerID]; ok {
					views[i].Speaker = name // 解析到登记名
				} else {
					views[i].Speaker = speakerLabelName(sg.SpeakerLabel) // speaker_id 已设但名册缺失→回退
				}
			} else {
				views[i].Speaker = speakerLabelName(sg.SpeakerLabel) // 未解析→"说话人 N"
			}
		}
		resp["transcript"] = tr
		resp["segments"] = views
		resp["speakers"] = sis
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

// PatchTranscript 就地修正 ASR 转写段文本：body {segments:[{id,text}]}。
// 校验 session 存在 → 取 transcript → 逐段 UpdateSegmentText（带 transcript_id 作用域，
// 跨会话 id 静默忽略）→ RecomputeFullText 同步 full_text/confidence。
// 只改转写文本，不触发抽取；前端保存后可单独点「重新提取」走 Reextract。
func (h *QueryHandler) PatchTranscript(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if _, err := h.Sessions.Get(r.Context(), sid); err != nil {
		http.Error(w, "session 不存在", http.StatusNotFound)
		return
	}
	tr, err := h.Transcripts.GetBySession(r.Context(), sid)
	if err != nil {
		http.Error(w, "transcript 不存在", http.StatusNotFound)
		return
	}
	var req struct {
		Segments []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"segments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	for _, sg := range req.Segments {
		segID, err := ids.ParseID(sg.ID)
		if err != nil {
			http.Error(w, "非法 segment id: "+sg.ID, http.StatusBadRequest)
			return
		}
		// 直接写用户输入（含清空），作用域到本 transcript 防跨会话误写
		if err := h.Transcripts.UpdateSegmentText(r.Context(), tr.ID, segID, sg.Text); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := h.Transcripts.RecomputeFullText(r.Context(), tr.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// Reextract 基于当前（可能已编辑的）ASR 重新抽取记忆/待办：
// 在 segment stage 建一个 pending job，pool 领取后重算 full_text（segment）→
// speaker（幂等：段已解析则 no-op，不覆盖手动换人、不依赖 sidecar）→
// 重新抽取（extract，对本 session 幂等：删旧 memory/todo 再重插）→ done。
// SetJobID 把 session 指向新 job，前端轮询 GET /api/sessions/{id} 的 job.status 可见进度。
// 必须已有 transcript（无转写的 session 无法跑 segment→speaker→extract）。
func (h *QueryHandler) Reextract(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if _, err := h.Sessions.Get(r.Context(), sid); err != nil {
		http.Error(w, "session 不存在", http.StatusNotFound)
		return
	}
	if _, err := h.Transcripts.GetBySession(r.Context(), sid); err != nil {
		http.Error(w, "该会话暂无转写，无法重新提取", http.StatusConflict)
		return
	}
	j := &repo.Job{SessionID: sid, Stage: "segment", Status: "pending"}
	if err := h.Jobs.Create(r.Context(), j); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = h.Sessions.SetJobID(r.Context(), sid, j.ID)
	writeJSON(w, map[string]any{"job_id": j.ID})
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

// DeleteSession 硬删除 session + 派生数据（级联单事务）+ 音频文件 best-effort。
// 2 步确认由前端；后端：Get 不存在→404，删成功→204。音频文件库外删，失败仅 log 不阻断
// （DB 已删，文件残留可接受；区别于 DB 事务的强一致）。StoragePath 是 json:"-" 不外泄，
// 此处仅服务端读用于删文件。
func (h *QueryHandler) DeleteSession(w http.ResponseWriter, r *http.Request) {
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
	if err := h.Sessions.Delete(r.Context(), sid, s.JobID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.StoragePath != "" {
		_ = os.Remove(s.StoragePath) // best-effort：失败不阻断（DB 已删）
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
