package api

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// SpeakerHandler 说话人名册 + 录入 + 换人。
type SpeakerHandler struct {
	Speakers    *repo.SpeakerRepo
	Transcripts *repo.TranscriptRepo
	Voiceprint  voiceprint.Client
	DataDir     string
}

// RegisterSpeaker 挂载说话人相关路由。
func RegisterSpeaker(r chi.Router, h *SpeakerHandler) {
	r.Get("/api/speakers", h.List)
	r.Post("/api/speakers", h.Enroll)
	r.Patch("/api/speakers/{id}", h.Rename)
	r.Delete("/api/speakers/{id}", h.Delete)
	r.Post("/api/speakers/merge", h.Merge)           // 声纹页「手动合并」：多说话人并入一个目标
	r.Get("/api/speakers/{id}/segments", h.Segments) // 该说话人跨 session 出现的片段（声纹 tab 点开看关联录音）
	r.Get("/api/sessions/{sid}/speakers", h.SessionSpeakers)
	r.Patch("/api/sessions/{sid}/segments/{seg}/speaker", h.ReassignSegment)
}

// List 全部 active 说话人（管理页/换人下拉用）。
func (h *SpeakerHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.Speakers.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"speakers": list})
}

// Segments 该说话人出现的所有片段（跨 session，声纹 tab「点开看关联录音」用）。
// 每条含 session_id/filename/created_at（音频经 GET /api/sessions/{session_id}/audio 播放）
// + 段文本与 start_ms/end_ms（前端按时间段定位播放）。
func (h *SpeakerHandler) Segments(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	list, err := h.Transcripts.ListSegmentsBySpeaker(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"segments": list})
}

// Enroll 录入：收音频样本 + 名 → 转码 wav16k → sidecar /embed → 登记(enrolled) + /add。
func (h *SpeakerHandler) Enroll(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少 file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "缺少 name", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(h.DataDir, "enroll")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "存储目录创建失败", http.StatusInternalServerError)
		return
	}
	sid := ids.New()
	src := filepath.Join(dir, sid.String()+".wav")
	wav16 := filepath.Join(dir, sid.String()+"-16k.wav")
	defer os.Remove(src) // 临时文件：成功/失败都清理（data/enroll 不残留）
	defer os.Remove(wav16)
	out, err := os.Create(src)
	if err != nil {
		http.Error(w, "文件创建失败", http.StatusInternalServerError)
		return
	}
	// 用 io.Copy 直接把上传流写入落盘文件（成熟 stdlib 方案，避免自造缓冲拷贝）。
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		http.Error(w, "文件写入失败", http.StatusInternalServerError)
		return
	}
	out.Close()
	if err := transcodeEnroll(src, wav16); err != nil {
		http.Error(w, "转码失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	vec, err := h.Voiceprint.Embed(r.Context(), wav16)
	if err != nil || len(vec) != 256 {
		http.Error(w, "声纹提取失败", http.StatusInternalServerError)
		return
	}
	sp := &repo.Speaker{Name: name, Source: "enrolled", Embedding: float32BlobAPI(vec), SampleCount: 1}
	if err := h.Speakers.Create(r.Context(), sp); err != nil {
		http.Error(w, "登记失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 写 FAISS 索引；失败则回滚刚建的 speaker 行，避免"名册有但索引无"的孤儿
	// （1:N 永不命中，用户以为录入成功实则无效）。与 stage 的自动登记路径保持一致（那里 Add 失败即报错）。
	if err := h.Voiceprint.Add(r.Context(), vec, sp.ID); err != nil {
		_ = h.Speakers.Delete(r.Context(), sp.ID)
		http.Error(w, "声纹索引写入失败，请重试", http.StatusInternalServerError)
		return
	}
	writeJSON(w, sp)
}

func transcodeEnroll(src, dst string) error {
	cmd := exec.Command("ffmpeg", "-y", "-i", src, "-ar", "16000", "-ac", "1", "-sample_fmt", "s16", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", out)
	}
	return nil
}

// Rename 改名（自动登记名就地改）。
func (h *SpeakerHandler) Rename(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "缺少 name", http.StatusBadRequest)
		return
	}
	if err := h.Speakers.UpdateName(r.Context(), id, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// Delete 删 sidecar 向量 + DB 行 + 清悬空引用（段 speaker_id 置 NULL）。
func (h *SpeakerHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	_ = h.Voiceprint.Remove(r.Context(), id)
	if err := h.Speakers.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = h.Speakers.DB.ExecContext(r.Context(),
		`UPDATE transcript_segment SET speaker_id = NULL WHERE speaker_id = ?`, id.Int64())
	w.WriteHeader(http.StatusNoContent)
}

// Merge 把多个说话人并入一个目标（声纹页「手动合并」：纠正 ASR 把同人拆成多个说话人）。
// 源说话人的全部转写段（跨所有 session）改指目标 → 删源行 + sidecar 移除源向量。
// 目标向量不动（MVP 不重算，沿用其既有声纹；与 stage「已命中不增量更新」一致）。
func (h *SpeakerHandler) Merge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceIDs []string `json:"source_ids"`
		TargetID  string   `json:"target_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	targetID, err := ids.ParseID(req.TargetID)
	if err != nil {
		http.Error(w, "invalid target_id", http.StatusBadRequest)
		return
	}
	srcIDs := make([]ids.ID, 0, len(req.SourceIDs))
	for _, s := range req.SourceIDs {
		id, err := ids.ParseID(s)
		if err != nil {
			http.Error(w, "invalid source_id: "+s, http.StatusBadRequest)
			return
		}
		if id == targetID {
			continue // 源含目标则跳过（目标自身不能并入自己）
		}
		srcIDs = append(srcIDs, id)
	}
	if len(srcIDs) == 0 {
		http.Error(w, "无可合并的源（source_ids 不能为空或只含 target）", http.StatusBadRequest)
		return
	}
	// 先 sidecar 移除源向量（段此时还没改指，移除不影响读名）；DB 段改指 + 删源事务。
	for _, sid := range srcIDs {
		_ = h.Voiceprint.Remove(r.Context(), sid)
	}
	merged, err := h.Speakers.MergeInto(r.Context(), targetID, srcIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "merged_segments": merged, "removed_speakers": len(srcIDs)})
}

// SessionSpeakers 本 session 解析到的说话人（面板用）。color_index 按序号填。
func (h *SpeakerHandler) SessionSpeakers(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "sid"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	tr, err := h.Transcripts.GetBySession(r.Context(), sid)
	if err != nil {
		http.Error(w, "无转写", http.StatusNotFound)
		return
	}
	list, err := h.Transcripts.ListSpeakersForTranscript(r.Context(), tr.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range list {
		list[i].ColorIndex = i
	}
	writeJSON(w, map[string]any{"speakers": list})
}

// ReassignSegment 单段换人（前端"换人"下拉）。带 transcript 作用域防跨会话。
func (h *SpeakerHandler) ReassignSegment(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "sid"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	segID, err := ids.ParseID(chi.URLParam(r, "seg"))
	if err != nil {
		http.Error(w, "invalid seg id", http.StatusBadRequest)
		return
	}
	tr, err := h.Transcripts.GetBySession(r.Context(), sid)
	if err != nil {
		http.Error(w, "无转写", http.StatusNotFound)
		return
	}
	var req struct {
		SpeakerID string `json:"speaker_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	spID, err := ids.ParseID(req.SpeakerID)
	if err != nil {
		http.Error(w, "invalid speaker_id", http.StatusBadRequest)
		return
	}
	if err := h.Transcripts.SetSegmentSpeakerByID(r.Context(), tr.ID, segID, spID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// float32BlobAPI 256×float32 → []byte（Little-Endian），存 speaker.embedding 灾备 BLOB。
// 内联而非 import pipeline（避免 api→pipeline 反向依赖；同 repo.RecomputeFullText 模式）。
func float32BlobAPI(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}
