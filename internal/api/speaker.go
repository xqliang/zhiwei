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
	"sort"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// SpeakerHandler 说话人名册 + 录入 + 换人。
type SpeakerHandler struct {
	Speakers            *repo.SpeakerRepo
	Transcripts         *repo.TranscriptRepo
	Voiceprint          voiceprint.Client
	DataDir             string
	EnrollMinDurationMS int64   // 从转写段音频录入声纹的最小时长（ms，0→兜底 3000）
	VoiceprintThreshold float64 // 1:N 余弦匹配阈值（match 预览判定+展示用，0→兜底 0.5）
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
	r.Post("/api/sessions/{sid}/segments/{seg}/enroll", h.EnrollFromSegment) // timeline「用此段录音纹」：从转写段音频录入新说话人
	r.Post("/api/sessions/{sid}/segments/merge", h.MergeSegments)            // timeline「合并连续同人段成一条」
	r.Post("/api/voiceprint/match", h.MatchPreview)                          // 录音页「试匹配」预览：上传音频→提向→1:N→返回相似度+阈值（只读不登记）
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

// EnrollFromSegment timeline「用此段录音纹」：用某转写段对应时间段的音频录入新说话人。
// 切 transcoded/{sid}.wav 的 [start_ms,end_ms] → sidecar /embed → 登记(enrolled) + /add。
// 时长 < EnrollMinDurationMS 拒绝（声纹需足够时长才稳，WeSpeaker LM 对 >3s 更准）。
// 只创建说话人、不改判段——改判可能误拆已聚类说话人，留给下拉/手动合并；返回新 speaker。
func (h *SpeakerHandler) EnrollFromSegment(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "sid"))
	if err != nil {
		http.Error(w, "invalid sid", http.StatusBadRequest)
		return
	}
	segID, err := ids.ParseID(chi.URLParam(r, "seg"))
	if err != nil {
		http.Error(w, "invalid seg id", http.StatusBadRequest)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "缺少 name", http.StatusBadRequest)
		return
	}
	// 校验段属于该会话的 transcript（防跨会话误用）
	tr, err := h.Transcripts.GetBySession(r.Context(), sid)
	if err != nil {
		http.Error(w, "无转写", http.StatusNotFound)
		return
	}
	seg, err := h.Transcripts.GetSegment(r.Context(), segID)
	if err != nil {
		http.Error(w, "转写段不存在", http.StatusNotFound)
		return
	}
	if seg.TranscriptID != tr.ID {
		http.Error(w, "段不属于该会话", http.StatusBadRequest)
		return
	}
	min := h.EnrollMinDurationMS
	if min == 0 {
		min = 3000 // 兜底，与 config 默认一致
	}
	if dur := seg.EndMS - seg.StartMS; dur < min {
		http.Error(w, fmt.Sprintf("时长 %.1fs 不足，录入声纹至少需 %.0fs", float64(dur)/1000, float64(min)/1000),
			http.StatusBadRequest)
		return
	}
	wavPath := filepath.Join(h.DataDir, "transcoded", sid.String()+".wav")
	if _, err := os.Stat(wavPath); err != nil {
		http.Error(w, "转码音频未找到（需先完成 ASR）", http.StatusNotFound)
		return
	}
	tmpDir := filepath.Join(h.DataDir, "enroll")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		http.Error(w, "存储目录创建失败", http.StatusInternalServerError)
		return
	}
	slice := filepath.Join(tmpDir, segID.String()+"-seg.wav")
	defer os.Remove(slice)
	if err := sliceWavForEnroll(wavPath, slice, seg.StartMS, seg.EndMS); err != nil {
		http.Error(w, "切片失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	vec, err := h.Voiceprint.Embed(r.Context(), slice)
	if err != nil || len(vec) != 256 {
		http.Error(w, "声纹提取失败", http.StatusInternalServerError)
		return
	}
	sp := &repo.Speaker{Name: req.Name, Source: "enrolled", Embedding: float32BlobAPI(vec), SampleCount: 1}
	if err := h.Speakers.Create(r.Context(), sp); err != nil {
		http.Error(w, "登记失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 写 FAISS 索引；失败回滚 speaker 行，避免孤儿（与 Enroll 一致）
	if err := h.Voiceprint.Add(r.Context(), vec, sp.ID); err != nil {
		_ = h.Speakers.Delete(r.Context(), sp.ID)
		http.Error(w, "声纹索引写入失败，请重试", http.StatusInternalServerError)
		return
	}
	writeJSON(w, sp)
}

// sliceWavForEnroll 从 transcoded 16k mono wav 按 [start_ms,end_ms] 切片段（录入声纹用）。
// 内联而非 import pipeline.sliceAudio（避免 api→pipeline 反向依赖；-c copy 包级精度对声纹无影响）。
func sliceWavForEnroll(src, dst string, startMS, endMS int64) error {
	out, err := exec.Command("ffmpeg", "-y",
		"-ss", fmt.Sprintf("%.3f", float64(startMS)/1000),
		"-to", fmt.Sprintf("%.3f", float64(endMS)/1000),
		"-i", src, "-c", "copy", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", out)
	}
	return nil
}

// MergeSegments timeline「合并连续同人段成一条」：把选中段合并成一条——text 按 sequence_no
// 顺序拼接、时间 [min start, max end]、speaker_id=target，其余段删除 + 重算全文。
// 用于纠正 ASR 把同人连续发言拆成多段。后端按 ListSegments(已按 sequence_no 排序) 顺序合并，
// 不强制连续性校验（前端引导选连续同人段；非连续合并会得跨时段，用户自负）。
func (h *SpeakerHandler) MergeSegments(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "sid"))
	if err != nil {
		http.Error(w, "invalid sid", http.StatusBadRequest)
		return
	}
	var req struct {
		SegmentIDs []string `json:"segment_ids"`
		SpeakerID  string   `json:"speaker_id"`
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
	if len(req.SegmentIDs) < 2 {
		http.Error(w, "至少选 2 段", http.StatusBadRequest)
		return
	}
	tr, err := h.Transcripts.GetBySession(r.Context(), sid)
	if err != nil {
		http.Error(w, "无转写", http.StatusNotFound)
		return
	}
	all, err := h.Transcripts.ListSegments(r.Context(), tr.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	idSet := make(map[ids.ID]bool, len(req.SegmentIDs))
	for _, s := range req.SegmentIDs {
		id, err := ids.ParseID(s)
		if err != nil {
			http.Error(w, "invalid segment_id: "+s, http.StatusBadRequest)
			return
		}
		idSet[id] = true
	}
	// all 已按 sequence_no 排序，过滤选中即得合并顺序
	picked := make([]repo.TranscriptSegment, 0, len(req.SegmentIDs))
	for _, s := range all {
		if idSet[s.ID] {
			picked = append(picked, s)
		}
	}
	if len(picked) != len(req.SegmentIDs) {
		http.Error(w, "部分段不属于该会话", http.StatusBadRequest)
		return
	}
	// 拼接文本 + [min,max] 时间
	text := ""
	startMS, endMS := picked[0].StartMS, picked[0].EndMS
	for _, s := range picked {
		text += s.Text
		if s.StartMS < startMS {
			startMS = s.StartMS
		}
		if s.EndMS > endMS {
			endMS = s.EndMS
		}
	}
	keeper := picked[0]
	others := make([]ids.ID, 0, len(picked)-1)
	for _, s := range picked[1:] {
		others = append(others, s.ID)
	}
	if err := h.Transcripts.MergeSegments(r.Context(), tr.ID, keeper.ID, others, text, startMS, endMS, spID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = h.Transcripts.RecomputeFullText(r.Context(), tr.ID)
	writeJSON(w, map[string]any{"ok": true, "merged_count": len(picked), "keeper_id": keeper.ID.String()})
}

// MatchPreview 试匹配预览（录音页「这段像谁」）：上传音频 → 转码 wav16k → sidecar /embed →
// /search → 返回最匹配说话人 + 余弦相似度 + 阈值 + 是否命中。纯只读（不登记、不入库），
// 便于用户在录入前看「这段音频和已有声纹库的匹配度」。低于阈值也回最接近的候选 + 相似度。
func (h *SpeakerHandler) MatchPreview(w http.ResponseWriter, r *http.Request) {
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
	dir := filepath.Join(h.DataDir, "enroll")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "存储目录创建失败", http.StatusInternalServerError)
		return
	}
	tmpID := ids.New()
	src := filepath.Join(dir, tmpID.String()+"-match")
	wav16 := src + ".wav"
	defer os.Remove(src)
	defer os.Remove(wav16)
	out, err := os.Create(src)
	if err != nil {
		http.Error(w, "文件创建失败", http.StatusInternalServerError)
		return
	}
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
	threshold := h.VoiceprintThreshold
	if threshold == 0 {
		threshold = 0.5
	}
	// 列全部 active 说话人，用其灾备 BLOB(与 FAISS 同向量) 逐个算余弦匹配度——
	// 返回全库按相似度降序（不止 top-1），便于看「这段像库里的每一个谁、各多像」。
	list, err := h.Speakers.List(r.Context())
	if err != nil {
		http.Error(w, "声纹库读取失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type matchItem struct {
		SpeakerID  string  `json:"speaker_id"`
		Name       string  `json:"name"`
		Similarity float64 `json:"similarity"`
	}
	items := make([]matchItem, 0, len(list))
	for _, sp := range list {
		emb, ok := decodeEmbedding(sp.Embedding)
		if !ok || len(emb) != 256 {
			continue // 无灾备向量或维度异常的跳过
		}
		items = append(items, matchItem{SpeakerID: sp.ID.String(), Name: sp.Name, Similarity: cosine(vec, emb)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Similarity > items[j].Similarity })
	matched := len(items) > 0 && items[0].Similarity >= threshold
	resp := map[string]any{
		"matches":     items, // 全库按相似度降序
		"threshold":   threshold,
		"matched":     matched,
		"has_library": len(items) > 0,
	}
	if matched {
		resp["speaker_id"] = items[0].SpeakerID
		resp["speaker_name"] = items[0].Name
		resp["similarity"] = items[0].Similarity
	}
	writeJSON(w, resp)
}

// decodeEmbedding 解码 speaker.embedding 灾备 BLOB(256×float32 LE) → []float32。
// 长度非 4 倍数→脏数据，返回 ok=false 跳过。与 float32BlobAPI 互逆。
func decodeEmbedding(blob []byte) ([]float32, bool) {
	if len(blob) == 0 || len(blob)%4 != 0 {
		return nil, false
	}
	v := make([]float32, len(blob)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return v, true
}

// cosine 两个 L2 归一化向量的余弦相似度（= 内积）。用于 match 预览对全库算匹配度。
// 与 sidecar FAISS IndexFlatIP(内积) 等价——BLOB 与索引同向量，结果一致。
func cosine(a, b []float32) float64 {
	var s float64
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s
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
