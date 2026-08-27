package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	VoiceprintThreshold float64 // 1:N 余弦匹配阈值（match 预览判定+展示用，0→兜底 0.8）

	SpeakerEmbeddings *repo.SpeakerEmbeddingRepo // 多条声纹样本（nil = 未装配，条目功能降级，兼容旧装配/测试）

	SpeakerNameCandidates *repo.SpeakerNameCandidateRepo // 名字候选 repo（nil = 不富化/不清理，兼容旧装配）
}

// NameCandidateView 前端展示的候选名：名称 + 置信度数值（硬性要求：用户确认时
// 必须能看到名称和置信度值）+ 依据。倒排。
type NameCandidateView struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence,omitempty"`
}

// speakerWithCandidates speaker + 候选名列表（名册/面板富化视图）。
type speakerWithCandidates struct {
	repo.Speaker
	NameCandidates []NameCandidateView `json:"name_candidates"`
}

// attachCandidates 为说话人列表批量附候选名（一次查询避免 N+1）。
// repo 未装配时返回全空候选；查询失败降级为空候选（富化仅影响建议展示，不阻断列表）。
func (h *SpeakerHandler) attachCandidates(ctx context.Context, list []repo.Speaker) []speakerWithCandidates {
	out := make([]speakerWithCandidates, len(list))
	spIDs := make([]ids.ID, len(list))
	idx := make(map[ids.ID]int, len(list))
	for i, sp := range list {
		out[i] = speakerWithCandidates{Speaker: sp, NameCandidates: []NameCandidateView{}}
		spIDs[i] = sp.ID
		idx[sp.ID] = i
	}
	if h.SpeakerNameCandidates == nil || len(list) == 0 {
		return out
	}
	cands, err := h.SpeakerNameCandidates.ListBySpeakers(ctx, spIDs)
	if err != nil {
		log.Printf("[speaker] 候选名富化失败: %v", err)
		return out // 降级：无候选展示
	}
	for _, c := range cands {
		if i, ok := idx[c.SpeakerID]; ok {
			out[i].NameCandidates = append(out[i].NameCandidates, NameCandidateView{
				Name: c.Name, Confidence: c.Confidence, Evidence: c.Evidence,
			})
		}
	}
	return out
}

// RegisterSpeaker 挂载说话人相关路由。
func RegisterSpeaker(r chi.Router, h *SpeakerHandler) {
	r.Get("/api/speakers", h.List)
	r.Post("/api/speakers", h.Enroll)
	r.Patch("/api/speakers/{id}", h.Rename)
	r.Delete("/api/speakers/{id}", h.Delete)
	r.Delete("/api/speakers/{id}/name-candidates", h.DeleteNameCandidate) // 忽略单个候选名（建议区 ✕）
	r.Post("/api/speakers/merge", h.Merge)                                // 声纹页「手动合并」：多说话人并入一个目标（声纹样本累加）
	r.Get("/api/speakers/{id}/segments", h.Segments)                      // 该说话人跨 session 出现的片段（声纹 tab 点开看关联录音）
	r.Get("/api/speakers/{id}/embeddings", h.ListEmbeddings)              // 多条声纹样本列表（备注/创建时间/来源）
	r.Post("/api/speakers/{id}/embeddings", h.AddEmbedding)               // 追加一条声纹样本（multipart file+note）
	r.Patch("/api/speakers/{id}/embeddings/{eid}", h.UpdateEmbeddingNote) // 改样本备注
	r.Delete("/api/speakers/{id}/embeddings/{eid}", h.DeleteEmbedding)    // 删单条样本（聚合代表重算）
	r.Get("/api/sessions/{sid}/speakers", h.SessionSpeakers)
	r.Patch("/api/sessions/{sid}/segments/{seg}/speaker", h.ReassignSegment) // 单段换人
	r.Post("/api/sessions/{sid}/speakers/reassign", h.ReassignSpeakerAll)   // 「切换声纹」：本会话某说话人全部段一键改判
	r.Post("/api/sessions/{sid}/segments/{seg}/enroll", h.EnrollFromSegment) // timeline「用此段录音纹」：从转写段音频录入新说话人
	r.Post("/api/sessions/{sid}/segments/merge", h.MergeSegments)            // timeline「合并连续同人段成一条」
	r.Post("/api/voiceprint/match", h.MatchPreview)                          // 录音页「试匹配」预览：上传音频→提向→1:N→返回相似度+阈值（只读不登记）
	r.Post("/api/voiceprint/resync", h.Resync)                               // 重建索引：逐说话人 Remove+逐条样本 Add（多向量迁移/运维修复）
}

// embeddingView 前端展示的声纹样本元数据（向量 BLOB 不外泄）。
type embeddingView struct {
	ID          string  `json:"id"`
	Note        string  `json:"note"`                 // 备注（可空串）
	Source      string  `json:"source"`               // manual|auto|merge
	SampleCount int     `json:"sample_count"`         // 该条聚合的段向量数
	CreatedAt   string  `json:"created_at"`           // RFC3339（前端 fmtTime 格式化）
}

// recomputeVoiceprint 重算某说话人的声纹：speaker.embedding 写「全部样本均值+L2 归一」
// 的聚合代表（展示/审计/无样本回退用）；FAISS 索引按**多向量**语义重建——该人的每条
// 样本向量各自 Add（sidecar /add 纯追加），1:N 匹配时与任意一条最高即命中（变体不稀释）。
// 样本删光时 embedding 置 NULL、Remove 索引（名字/段归属保留，不再被声纹命中）。
// 多条声纹模型的核心不变式：索引里的向量集合 ≡ 样本行集合；聚合代表只用于展示。
func (h *SpeakerHandler) recomputeVoiceprint(ctx context.Context, speakerID ids.ID) error {
	if h.SpeakerEmbeddings == nil {
		return nil // 未装配条目 repo（旧装配）：不动既有代表向量，调用方语义退化为覆盖式
	}
	entries, err := h.SpeakerEmbeddings.ListBySpeaker(ctx, speakerID)
	if err != nil {
		return err
	}
	// 索引先移除旧向量（无论还剩几条样本；后面逐条 Add 重建。Add 失败由调用方报错——
	// DB 样本行是完整真值，索引可由重算或 /api/voiceprint/resync 恢复）
	_ = h.Voiceprint.Remove(ctx, speakerID)
	if len(entries) == 0 {
		return h.Speakers.UpdateVoiceprint(ctx, speakerID, nil, 0)
	}
	vecs := make([][]float32, 0, len(entries))
	total := 0
	for _, e := range entries {
		if v, ok := decodeEmbedding(e.Embedding); ok && len(v) == 256 {
			vecs = append(vecs, v)
		}
		total += e.SampleCount
	}
	if len(vecs) == 0 {
		// 全是脏 BLOB：清空代表与索引，避免残留旧向量与样本不一致
		return h.Speakers.UpdateVoiceprint(ctx, speakerID, nil, 0)
	}
	// 聚合代表（展示/审计）= 均值 + L2 归一
	rep := aggregateVecsAPI(vecs)
	if err := h.Speakers.UpdateVoiceprint(ctx, speakerID, float32BlobAPI(rep), total); err != nil {
		return err
	}
	// 索引（匹配用）= 逐条样本向量（多向量：变体各自可命中）
	for _, v := range vecs {
		if err := h.Voiceprint.Add(ctx, v, speakerID); err != nil {
			return err
		}
	}
	return nil
}

// Resync 重建整个 FAISS 索引：对每个 active 说话人 Remove 后逐条 Add 其样本向量。
// 用途：①多向量语义上线后把存量索引（旧的单聚合向量）迁移为逐样本向量；
// ②索引与 DB 样本行失一致时（sidecar 数据丢失/中途失败）的运维修复。幂等。
func (h *SpeakerHandler) Resync(w http.ResponseWriter, r *http.Request) {
	if h.SpeakerEmbeddings == nil {
		http.Error(w, "多条声纹功能未装配", http.StatusNotImplemented)
		return
	}
	list, err := h.Speakers.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	spN, vecN := 0, 0
	for _, sp := range list {
		entries, err := h.SpeakerEmbeddings.ListBySpeaker(r.Context(), sp.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(entries) == 0 {
			continue // 无样本（无向量）的说话人不动索引
		}
		_ = h.Voiceprint.Remove(r.Context(), sp.ID)
		for _, e := range entries {
			if v, ok := decodeEmbedding(e.Embedding); ok && len(v) == 256 {
				if err := h.Voiceprint.Add(r.Context(), v, sp.ID); err != nil {
					http.Error(w, fmt.Sprintf("speaker %s 入索引失败: %v", sp.ID, err), http.StatusInternalServerError)
					return
				}
				vecN++
			}
		}
		spN++
	}
	writeJSON(w, map[string]any{"ok": true, "speakers": spN, "vectors": vecN})
}

// List 全部 active 说话人（管理页/换人下拉用）。随机名说话人附 LLM 推断的候选名（倒排）；
// 多条声纹装配时附每人的样本元数据（备注/创建时间，向量不外泄）。
func (h *SpeakerHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.Speakers.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	withCands := h.attachCandidates(r.Context(), list)
	if h.SpeakerEmbeddings != nil {
		writeJSON(w, map[string]any{"speakers": h.attachEmbeddings(r.Context(), withCands)})
		return
	}
	writeJSON(w, map[string]any{"speakers": withCands})
}

// speakerWithEmbeddings speaker + 候选名 + 声纹样本列表（名册富化视图）。
type speakerWithEmbeddings struct {
	speakerWithCandidates
	Embeddings []embeddingView `json:"embeddings"` // 无 omitempty：前端统一按空数组处理
}

// attachEmbeddings 为名册批量附声纹样本元数据（一次查询避免 N+1；失败降级空列表不阻断）。
func (h *SpeakerHandler) attachEmbeddings(ctx context.Context, list []speakerWithCandidates) []speakerWithEmbeddings {
	out := make([]speakerWithEmbeddings, len(list))
	spIDs := make([]ids.ID, len(list))
	for i, sp := range list {
		out[i] = speakerWithEmbeddings{speakerWithCandidates: sp, Embeddings: []embeddingView{}}
		spIDs[i] = sp.ID
	}
	entries, err := h.SpeakerEmbeddings.ListBySpeakers(ctx, spIDs)
	if err != nil {
		log.Printf("[speaker] 样本元数据富化失败: %v", err)
		return out
	}
	for i := range out {
		if es := entries[out[i].ID]; len(es) > 0 {
			out[i].Embeddings = toEmbeddingViews(es)
		}
	}
	return out
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
		msg := "声纹提取失败"
		if err != nil {
			msg += ": " + err.Error()
		}
		writeJSONError(w, msg, http.StatusInternalServerError)
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
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
	// 首条声纹样本落库（多条声纹模型；失败不回滚——speaker/FAISS 已就绪，样本行缺了
	// 只影响后续聚合重算的来源，可用重启 bootstrap 兜底补齐，非致命）
	if h.SpeakerEmbeddings != nil {
		e := &repo.SpeakerEmbedding{SpeakerID: sp.ID, Embedding: float32BlobAPI(vec), SampleCount: 1, Source: "manual"}
		if note != "" {
			e.Note = &note
		}
		if err := h.SpeakerEmbeddings.Create(r.Context(), e); err != nil {
			log.Printf("[speaker] 录入后样本行落库失败 speaker=%s: %v", sp.ID, err)
		}
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
	// 改名 = 用户已确认称呼（采纳候选或手动命名）：清空候选——名字不再是随机名，
	// 后续也不再重跑推断。清空失败不回滚改名（候选残留仅影响建议展示，前端对
	// 非随机名说话人本就不显示建议区），log 便于排查。
	if h.SpeakerNameCandidates != nil {
		if err := h.SpeakerNameCandidates.DeleteBySpeaker(r.Context(), id); err != nil {
			log.Printf("[speaker] 改名后清候选失败 speaker=%s: %v", id, err)
		}
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
	// 声纹样本行一并清（说话人没了，样本是孤儿；幂等 best-effort）
	if h.SpeakerEmbeddings != nil {
		if err := h.SpeakerEmbeddings.DeleteBySpeaker(r.Context(), id); err != nil {
			log.Printf("[speaker] 删说话人后清样本行失败 speaker=%s: %v", id, err)
		}
	}
	_, _ = h.Speakers.DB.ExecContext(r.Context(),
		`UPDATE transcript_segment SET speaker_id = NULL WHERE speaker_id = ?`, id.Int64())
	// 删说话人后清其候选：孤儿候选永不外显（说话人已没了）但会在表里累积，顺手清掉。
	// best-effort，失败仅 log 不阻断删除（与 Rename 清候选一致；候选残留无副作用）。
	if h.SpeakerNameCandidates != nil {
		if err := h.SpeakerNameCandidates.DeleteBySpeaker(r.Context(), id); err != nil {
			log.Printf("[speaker] 删说话人后清候选失败 speaker=%s: %v", id, err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Merge 把多个说话人并入一个目标（声纹页「手动合并」：纠正 ASR 把同人拆成多个说话人）。
// 源说话人的全部转写段（跨所有 session）改指目标 → 删源行 + sidecar 移除源向量。
// 声纹样本**累加不覆盖**（2026-08-26 需求）：源的样本行全部迁挂到目标（source=merge），
// 目标代表向量按「目标样本 + 迁入样本」聚合重算（均值+L2）后更新 FAISS——
// 合并前的两份声纹都保留，代表的区分度只增不减。
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
	// 样本行先迁挂到目标（在删源说话人**之前**——源行删了就找不到归属了）。失败即中止，
	// 不留「源删了但样本没迁」的半合并。
	if h.SpeakerEmbeddings != nil {
		if _, err := h.SpeakerEmbeddings.MigrateToSpeakerExt(r.Context(), h.Speakers.DB, targetID, srcIDs); err != nil {
			http.Error(w, "声纹样本迁移失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// sidecar 移除源向量（段此时还没改指，移除不影响读名）；DB 段改指 + 删源事务。
	for _, sid := range srcIDs {
		_ = h.Voiceprint.Remove(r.Context(), sid)
	}
	merged, err := h.Speakers.MergeInto(r.Context(), targetID, srcIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 目标代表向量按迁入后的全部样本聚合重算 + 更新 FAISS（Remove 在 recompute 里做）。
	// 失败仅报错不回滚合并（段已改指；代表向量可用再追加/重算修复，DB 样本行是完整真值）。
	if h.SpeakerEmbeddings != nil {
		if err := h.recomputeVoiceprint(r.Context(), targetID); err != nil {
			http.Error(w, "合并后聚合重算失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
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

// ReassignSpeakerAll timeline 说话人 chip「切换声纹」：把本会话内源说话人的全部段
// 一键改判给目标说话人（纠正声纹/识别错误——单段换人逐段点太繁琐）。
// 只改本 transcript 段的 speaker_id，不动说话人名册/声纹（错误登记的说话人
// 可用既有的删除/手动合并处理）。目标必须在名册中存在，防误写悬空 id。
func (h *SpeakerHandler) ReassignSpeakerAll(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "sid"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		FromSpeakerID string `json:"from_speaker_id"`
		ToSpeakerID   string `json:"to_speaker_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	fromID, err := ids.ParseID(req.FromSpeakerID)
	if err != nil {
		http.Error(w, "invalid from_speaker_id", http.StatusBadRequest)
		return
	}
	toID, err := ids.ParseID(req.ToSpeakerID)
	if err != nil {
		http.Error(w, "invalid to_speaker_id", http.StatusBadRequest)
		return
	}
	if fromID == toID {
		http.Error(w, "源与目标声纹相同", http.StatusBadRequest)
		return
	}
	tr, err := h.Transcripts.GetBySession(r.Context(), sid)
	if err != nil {
		http.Error(w, "无转写", http.StatusNotFound)
		return
	}
	// 目标必须在名册中存在（防把段指向已删除/不存在的声纹）
	if _, err := h.Speakers.Get(r.Context(), toID); err != nil {
		http.Error(w, "目标声纹不存在", http.StatusNotFound)
		return
	}
	updated, err := h.Transcripts.ReassignSpeakerSegments(r.Context(), tr.ID, fromID, toID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "updated": updated})
}

// EnrollFromSegment timeline「用此段录音纹」：用某转写段对应时间段的音频录入新说话人，
// 并把该段所属说话人在本会话的全部段一并改判到新说话人。
// 切 transcoded/{sid}.wav 的 [start_ms,end_ms] → sidecar /embed → 登记(enrolled) + /add → 批量改判。
// 时长 < EnrollMinDurationMS 拒绝（声纹需足够时长才稳，WeSpeaker LM 对 >3s 更准）。
// 改判口径「按当前显示的说话人」：该段已识别出说话人(speaker_id 非空)→ 改判本 transcript 内同一
// speaker 的所有段；尚未识别(为空)→ 退回按 ASR 说话人标签分组，回填本 transcript 内同标签的未解析段。
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
		msg := "声纹提取失败"
		if err != nil {
			msg += ": " + err.Error()
		}
		writeJSONError(w, msg, http.StatusInternalServerError)
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
	// 首条样本落库（备注标注来源段，便于在声纹页辨认这条是怎么来的；失败仅 log 非致命，同 Enroll）
	if h.SpeakerEmbeddings != nil {
		note := "来自转写段：" + fmt.Sprintf("%.1fs", float64(seg.EndMS-seg.StartMS)/1000)
		e := &repo.SpeakerEmbedding{SpeakerID: sp.ID, Embedding: float32BlobAPI(vec), SampleCount: 1, Source: "manual", Note: &note}
		if err := h.SpeakerEmbeddings.Create(r.Context(), e); err != nil {
			log.Printf("[speaker] 段录入后样本行落库失败 speaker=%s: %v", sp.ID, err)
		}
	}
	// 把该段所属说话人在本会话的全部段一并改判到新录入的说话人（口径见函数注释）。
	// 走到这里 speaker 已成功登记+入索引：改判失败只返回错误、不回滚新说话人（它是有效声纹，
	// 用户可用换人下拉补救；不留孤儿）。
	if seg.SpeakerID != nil {
		// 已识别：按当前 speaker_id 分组，改判本 transcript 内同一 speaker 的所有段。
		if _, err := h.Transcripts.ReassignSpeakerInTranscript(r.Context(), tr.ID, *seg.SpeakerID, sp.ID); err != nil {
			http.Error(w, "改判失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		// 未识别：退回按 ASR 说话人标签分组，回填本 transcript 内同标签的未解析段（含本段）。
		if err := h.Transcripts.SetSegmentSpeaker(r.Context(), tr.ID, seg.SpeakerLabel, sp.ID); err != nil {
			http.Error(w, "改判失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
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

// ---- 多条声纹样本（2026-08-26 需求：一个人可录多条，各带备注/创建时间） ----

// ListEmbeddings 某说话人的声纹样本列表（新录在前）。向量不外泄，仅元数据。
func (h *SpeakerHandler) ListEmbeddings(w http.ResponseWriter, r *http.Request) {
	if h.SpeakerEmbeddings == nil {
		http.Error(w, "多条声纹功能未装配", http.StatusNotImplemented)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	entries, err := h.SpeakerEmbeddings.ListBySpeaker(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"embeddings": toEmbeddingViews(entries)})
}

func toEmbeddingViews(entries []repo.SpeakerEmbedding) []embeddingView {
	out := make([]embeddingView, 0, len(entries))
	for _, e := range entries {
		note := ""
		if e.Note != nil {
			note = *e.Note
		}
		out = append(out, embeddingView{
			ID: e.ID.String(), Note: note, Source: e.Source,
			SampleCount: e.SampleCount, CreatedAt: e.CreatedAt.Format(time.RFC3339),
		})
	}
	return out
}

// AddEmbedding 给既有说话人**追加**一条声纹样本（multipart：file 必填、note 可选）。
// 追加后聚合重算代表向量（均值+L2）并更新 FAISS——多条声纹让代表更稳，而不是覆盖。
func (h *SpeakerHandler) AddEmbedding(w http.ResponseWriter, r *http.Request) {
	if h.SpeakerEmbeddings == nil {
		http.Error(w, "多条声纹功能未装配", http.StatusNotImplemented)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sp, err := h.Speakers.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "说话人不存在", http.StatusNotFound)
		return
	}
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
	note := strings.TrimSpace(r.FormValue("note"))
	vec, err := h.embedUploaded(r.Context(), file)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	e := &repo.SpeakerEmbedding{SpeakerID: sp.ID, Embedding: float32BlobAPI(vec),
		SampleCount: 1, Source: "manual"}
	if note != "" {
		e.Note = &note
	}
	if err := h.SpeakerEmbeddings.Create(r.Context(), e); err != nil {
		http.Error(w, "样本保存失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 聚合重算（含 FAISS 更新）；失败返回错误——样本行已落库，代表向量可用「再追加/重算」修复
	if err := h.recomputeVoiceprint(r.Context(), sp.ID); err != nil {
		http.Error(w, "聚合重算失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "embedding": toEmbeddingViews([]repo.SpeakerEmbedding{*e})[0]})
}

// embedUploaded 落盘上传流 → 转码 wav16k → 提向量（Enroll/AddEmbedding 共用）。
func (h *SpeakerHandler) embedUploaded(ctx context.Context, file io.Reader) ([]float32, error) {
	dir := filepath.Join(h.DataDir, "enroll")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("存储目录创建失败: %w", err)
	}
	sid := ids.New()
	src := filepath.Join(dir, sid.String()+"-add.wav")
	wav16 := filepath.Join(dir, sid.String()+"-add-16k.wav")
	defer os.Remove(src)
	defer os.Remove(wav16)
	out, err := os.Create(src)
	if err != nil {
		return nil, fmt.Errorf("文件创建失败: %w", err)
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		return nil, fmt.Errorf("文件写入失败: %w", err)
	}
	out.Close()
	if err := transcodeEnroll(src, wav16); err != nil {
		return nil, fmt.Errorf("转码失败: %w", err)
	}
	vec, err := h.Voiceprint.Embed(ctx, wav16)
	if err != nil || len(vec) != 256 {
		msg := "声纹提取失败"
		if err != nil {
			msg += ": " + err.Error()
		}
		return nil, errors.New(msg)
	}
	return vec, nil
}

// UpdateEmbeddingNote 改某条样本的备注。body {note}；空串 = 清空备注。
func (h *SpeakerHandler) UpdateEmbeddingNote(w http.ResponseWriter, r *http.Request) {
	if h.SpeakerEmbeddings == nil {
		http.Error(w, "多条声纹功能未装配", http.StatusNotImplemented)
		return
	}
	_, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	eid, err := ids.ParseID(chi.URLParam(r, "eid"))
	if err != nil {
		http.Error(w, "invalid embedding id", http.StatusBadRequest)
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	var note *string
	if s := strings.TrimSpace(req.Note); s != "" {
		note = &s
	}
	if err := h.SpeakerEmbeddings.UpdateNote(r.Context(), eid, note); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// DeleteEmbedding 删单条声纹样本 + 聚合重算代表。允许删到最后一条（此时说话人
// 不再有声纹、FAISS 移除——名字与段归属保留，可再追加新样本恢复）。
func (h *SpeakerHandler) DeleteEmbedding(w http.ResponseWriter, r *http.Request) {
	if h.SpeakerEmbeddings == nil {
		http.Error(w, "多条声纹功能未装配", http.StatusNotImplemented)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	eid, err := ids.ParseID(chi.URLParam(r, "eid"))
	if err != nil {
		http.Error(w, "invalid embedding id", http.StatusBadRequest)
		return
	}
	if err := h.SpeakerEmbeddings.Delete(r.Context(), eid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.recomputeVoiceprint(r.Context(), id); err != nil {
		http.Error(w, "聚合重算失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		msg := "声纹提取失败"
		if err != nil {
			msg += ": " + err.Error()
		}
		writeJSONError(w, msg, http.StatusInternalServerError)
		return
	}
	threshold := h.VoiceprintThreshold
	if threshold == 0 {
		threshold = 0.8
	}
	// 列全部 active 说话人，按**多向量**模型算匹配度：每人 = 与其任意一条声纹样本的
	// 最高余弦（感冒/哑嗓变体命中任一即命中），返回全库按相似度降序（不止 top-1），
	// 便于看「这段像库里的每一个谁、各多像」。
	list, err := h.Speakers.List(r.Context())
	if err != nil {
		http.Error(w, "声纹库读取失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	lib := libraryWithEntries(list, loadSpeakerEntries(r.Context(), h.SpeakerEmbeddings, list))
	type matchItem struct {
		SpeakerID  string  `json:"speaker_id"`
		Name       string  `json:"name"`
		Similarity float64 `json:"similarity"`
	}
	items := make([]matchItem, 0, len(lib))
	for _, lv := range lib {
		best := 0.0
		for _, v := range lv.vecs {
			if s := cosine(vec, v); s > best {
				best = s
			}
		}
		items = append(items, matchItem{SpeakerID: lv.id, Name: lv.name, Similarity: best})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Similarity > items[j].Similarity })
	// 命中判定与 speaker stage 同一套两级规则（voiceprint.Matched）：强命中 top1≥阈值；
	// 或区分性弱命中 top1≥0.72 且明显领先第二名（top1−top2≥0.06）。
	// 保证「试匹配」预览与实际识别结论一致，避免预览说未达阈值、实际处理却命中。
	second := 0.0
	if len(items) > 1 {
		second = items[1].Similarity
	}
	matched := len(items) > 0 && voiceprint.Matched(items[0].Similarity, second, threshold)
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
		resp["match_rule"] = map[string]any{ // 命中依据（区分性弱命中时前端可解释为何低于阈值仍命中）
			"top1":    items[0].Similarity,
			"top2":    second,
			"strong":  items[0].Similarity >= threshold, // true=强命中（过阈值）；false=区分性弱命中
			"soft_min": voiceprint.SoftMin,
			"gap_min":  voiceprint.GapMin,
		}
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

// voiceMatch 单条相似声纹（top-N 之一）。
type voiceMatch struct {
	SpeakerID  string  `json:"speaker_id"`
	Name       string  `json:"name"`
	Similarity float64 `json:"similarity"`
}

// libVoice 预解码的库内说话人（多向量模型，2026-08-26）：vecs = 该人全部声纹样本向量
// （感冒/哑嗓/变声等变体各自一条）；无样本行时回退聚合代表（speaker.embedding）单向量。
type libVoice struct {
	id   string
	name string
	vecs [][]float32
}

// libraryWithEntries 构建多向量检索库：每人展开其全部样本向量（与该人任意一条的最高
// 余弦即该人的匹配分——变体命中任一即命中）；样本表未装配/读取失败/该人无样本行时
// 回退聚合代表（旧数据兼容）。与 sidecar 多向量 /search 语义一致。
func libraryWithEntries(all []repo.Speaker, entries map[ids.ID][]repo.SpeakerEmbedding) []libVoice {
	lib := make([]libVoice, 0, len(all))
	for _, sp := range all {
		lv := libVoice{id: sp.ID.String(), name: sp.Name}
		if es := entries[sp.ID]; len(es) > 0 {
			for _, e := range es {
				if v, ok := decodeEmbedding(e.Embedding); ok && len(v) == 256 {
					lv.vecs = append(lv.vecs, v)
				}
			}
		}
		if len(lv.vecs) == 0 {
			// 回退：无样本行（旧数据/bootstrap 前）用聚合代表——保证库非空说话人不因
			// 样本缺失而从匹配里消失
			if v, ok := decodeEmbedding(sp.Embedding); ok && len(v) == 256 {
				lv.vecs = append(lv.vecs, v)
			}
		}
		if len(lv.vecs) > 0 {
			lib = append(lib, lv)
		}
	}
	return lib
}

// loadSpeakerEntries 批量取说话人的样本行（nil repo / 出错返回 nil，调用方走聚合回退）。
func loadSpeakerEntries(ctx context.Context, embs *repo.SpeakerEmbeddingRepo, all []repo.Speaker) map[ids.ID][]repo.SpeakerEmbedding {
	if embs == nil {
		return nil
	}
	spIDs := make([]ids.ID, len(all))
	for i := range all {
		spIDs[i] = all[i].ID
	}
	entries, err := embs.ListBySpeakers(ctx, spIDs)
	if err != nil {
		log.Printf("[speaker] 样本向量加载失败（回退聚合代表）: %v", err)
		return nil
	}
	return entries
}

// topVoiceMatchesVec 计算查询向量与库（多向量）的 top-N **说话人**，按
// 「与该人任意一条样本的最高余弦」降序——多向量模型的核心：感冒期录音撞上感冒期
// 样本即高分命中，不被其他变体（或聚合均值）稀释。
// 用途：timeline 详情段级 top-3 + timeline 列表「整段声纹」+ 试匹配预览（同一套语义）。
// vec 非 256 维返回 nil。
func topVoiceMatchesVec(lib []libVoice, vec []float32, n int) []voiceMatch {
	if len(vec) != 256 || len(lib) == 0 {
		return nil
	}
	ms := make([]voiceMatch, 0, len(lib))
	for _, lv := range lib {
		best := 0.0
		for _, v := range lv.vecs {
			if s := cosine(vec, v); s > best {
				best = s
			}
		}
		ms = append(ms, voiceMatch{SpeakerID: lv.id, Name: lv.name, Similarity: best})
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Similarity > ms[j].Similarity })
	if len(ms) > n {
		ms = ms[:n]
	}
	return ms
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

// DeleteNameCandidate 忽略单个候选名（前端建议区 ✕ 按钮）。
// ?name= 指定候选名；幂等（不存在也 204）。repo 未装配 501。
func (h *SpeakerHandler) DeleteNameCandidate(w http.ResponseWriter, r *http.Request) {
	if h.SpeakerNameCandidates == nil {
		http.Error(w, "候选名功能未装配", http.StatusNotImplemented)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "缺少 name", http.StatusBadRequest)
		return
	}
	if err := h.SpeakerNameCandidates.DeleteOne(r.Context(), id, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
