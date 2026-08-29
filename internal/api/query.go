package api

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/auth"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// QueryHandler 会话/任务查询。
type QueryHandler struct {
	Sessions    *repo.SessionRepo
	Jobs        *repo.JobRepo
	Transcripts *repo.TranscriptRepo
	Memories    *repo.MemoryRepo  // Sprint 2：详情附带 memory 卡片
	Todos       *repo.TodoRepo    // Sprint 2：详情附带 todo 卡片
	ChangeLogs  *repo.PersonChangeLogRepo // 详情附带该录音触发的 profile 平面变更（entity_kind 覆盖 8 平面）
	Speakers    *repo.SpeakerRepo // speaker stage：详情附带段说话人 + speakers 列表

	SpeakerStates *repo.SpeakerSessionStateRepo // 说话人情绪状态（audioscene stage 落库；nil=不返回该字段）

	SpeakerNameCandidates *repo.SpeakerNameCandidateRepo // speakername stage：详情 speakers 附候选名

	// VoiceprintThreshold 1:N 强命中阈值（timeline 列表「整段声纹」判定用；0→兜底 0.8）。
	VoiceprintThreshold float64

	// SpeakerEmbeddings 多条声纹样本 repo（多向量匹配：每人任意一条样本命中即命中；
	// nil = 未装配，回退聚合代表单向量，兼容旧装配/测试）
	SpeakerEmbeddings *repo.SpeakerEmbeddingRepo
}

// RegisterQuery 挂载查询路由。
func RegisterQuery(r chi.Router, h *QueryHandler) {
	r.Get("/api/sessions", h.ListSessions)
	r.Get("/api/sessions/{id}", h.GetSession)
	r.Patch("/api/sessions/{id}/transcript", h.PatchTranscript)
	r.Post("/api/sessions/{id}/reextract", h.Reextract)
	r.Post("/api/sessions/{id}/reidentify", h.Reidentify) // timeline「重新识别」：清空说话人归属+重跑 speaker stage
	r.Delete("/api/sessions/{id}", h.DeleteSession)
	r.Get("/api/sessions/{id}/audio", h.ServeAudio)
	r.Post("/api/jobs/{id}/retry", h.RetryJob)
}

// ListSessions 列出会话，每行富化 asr_preview（转写前 120 字）+ memory_count +
// todo_count（单 SQL 相关子查询，避免 N+1），并附最新 job 状态（处理进度）。
// 另富化 voice_top（「整段声纹」top-3 + 两级规则判定，2026-08-26 需求）：
//   - 整段只有 1 个 ASR 说话人标签 → 用全部段向量均值（≈整段代表声纹）与全库比对；
//   - 多人 → 用时长最长的一段（有向量的）比对——该段最能代表主要说话人；
//   - 判定与 speaker stage 同一套两级规则（voiceprint.Matched：≥阈值 或 ≥0.72 且领先 0.06）。
// 只读展示、不改实际归属；段向量缺失（存量会话）或声纹库为空时无此字段。
// asr_full 不外泄（json:"-"），仅截断后以 asr_preview 输出。
func (h *QueryHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	// 多租户隔离（评审 C2）：取登录用户，未登录 401。SQL 追加 WHERE s.user_id = ?，
	// 只列该用户自己的会话，杜绝全表返回他人录音。
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	limit := intQuery(r, "limit", 50)
	type row struct {
		repo.AudioSession
		JobStatus   string `json:"job_status,omitempty"`
		JobStage    string `json:"job_stage,omitempty"`
		MemoryCount int    `db:"memory_count" json:"memory_count"`
		TodoCount   int    `db:"todo_count" json:"todo_count"`
		AsrFull     string `db:"asr_full" json:"-"` // GROUP_CONCAT 全文，截断后给 AsrPreview
		AsrPreview  string `db:"-" json:"asr_preview"`
		VoiceTop    *voiceTopView `db:"-" json:"voice_top,omitempty"`
	}
	var rows []row
	err := h.Sessions.DB.SelectContext(r.Context(), &rows, `
SELECT s.*,
  (SELECT COUNT(*) FROM memory WHERE session_id = s.id AND status = 'active') AS memory_count,
  (SELECT COUNT(*) FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = s.id) AND status != 'dismissed') AS todo_count,
  (SELECT IFNULL(GROUP_CONCAT(seg.text ORDER BY seg.start_ms SEPARATOR ''), '')
     FROM transcript_segment seg JOIN transcript tr ON tr.id = seg.transcript_id
     WHERE tr.session_id = s.id) AS asr_full
FROM audio_session s WHERE s.user_id = ? ORDER BY s.id DESC LIMIT ?`, uid.Int64(), limit)
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
	// 「整段声纹」富化（声纹库未装配/读取失败则整列跳过，不阻断列表）
	if h.Speakers != nil {
		sids := make([]ids.ID, len(out))
		for i := range out {
			sids[i] = out[i].ID
		}
		tops := h.enrichVoiceTops(r.Context(), sids)
		for i := range out {
			out[i].VoiceTop = tops[out[i].ID]
		}
	}
	writeJSON(w, map[string]any{"sessions": out})
}

// voiceTopView timeline 卡片的「整段声纹」信息。
type voiceTopView struct {
	Basis string       `json:"basis"`           // whole=整段（单人）/ longest=最长段（多人）
	// Matches 与全库声纹的 top-3 余弦相似（降序；声纹库不足 3 人则更少）
	Matches []voiceMatch `json:"matches"`
	// Matched 两级规则（voiceprint.Matched）是否判定命中；Rule 给出命中依据
	// （strong=强命中过阈值 / gap=区分性弱命中），未命中为空
	Matched bool   `json:"matched"`
	Rule    string `json:"rule,omitempty"`
	// SpeakerID/SpeakerName 命中时的说话人（=Matches[0]）
	SpeakerID   string `json:"speaker_id,omitempty"`
	SpeakerName string `json:"speaker_name,omitempty"`
}

// voiceTopSegMeta 列表声纹判定所需的段元信息（不含 embedding——向量按需另取，控制列表开销）。
type voiceTopSegMeta struct {
	SessionID    ids.ID `db:"session_id"`
	ID           ids.ID `db:"id"`
	SpeakerLabel string `db:"speaker_label"`
	StartMS      int64  `db:"start_ms"`
	EndMS        int64  `db:"end_ms"`
	HasEmb       bool   `db:"has_emb"` // embedding 是否非空（决定单人聚合/最长段回退能否取到向量）
}

// enrichVoiceTops 整页计算各会话的「整段声纹」top-3 + 判定（session→voiceTopView，
// 无值（库空/段无向量）的会话不在返回 map 里）。
// 开销控制（列表接口，避免 N+1）：段元信息一次查全页、声纹库一次解码全页复用；
// 段向量按判定基准按需取——单人会话取其全部段、多人会话只取最长一段。
func (h *QueryHandler) enrichVoiceTops(ctx context.Context, sids []ids.ID) map[ids.ID]*voiceTopView {
	out := map[ids.ID]*voiceTopView{}
	if len(sids) == 0 {
		return out
	}
	// 声纹库：一次取全页复用（空库直接整体跳过）
	all, err := h.Speakers.List(ctx)
	if err != nil {
		log.Printf("[speaker] 列表声纹库读取失败: %v", err)
		return out
	}
	lib := libraryWithEntries(all, loadSpeakerEntries(ctx, h.SpeakerEmbeddings, all))
	if len(lib) == 0 {
		return out
	}
	// 段元信息：一次查全页（不含向量，见 listSegMetas 注释）
	metas, err := h.listSegMetas(ctx, sids)
	if err != nil {
		log.Printf("[speaker] 列表声纹段元信息读取失败: %v", err)
		return out
	}
	threshold := h.VoiceprintThreshold
	if threshold == 0 {
		threshold = 0.8
	}

	// 第一遍：逐会话定基准（whole/longest），收集需要取向量的段 id
	type plan struct {
		basis   string   // whole=整段（单人）/ longest=最长段（多人）
		segIDs  []ids.ID // 需要向量的段（whole=全部有向量的段；longest=选出的那一段）
	}
	plans := map[ids.ID]*plan{}
	var wantIDs []ids.ID
	for sid, segs := range metas {
		if len(segs) == 0 {
			continue
		}
		labels := map[string]bool{}
		for _, m := range segs {
			labels[m.SpeakerLabel] = true
		}
		p := &plan{}
		if len(labels) <= 1 {
			// 单人：整段代表声纹 = 全部段向量均值（只取向量非空的段）
			p.basis = "whole"
			for _, m := range segs {
				if m.HasEmb {
					p.segIDs = append(p.segIDs, m.ID)
				}
			}
			if len(p.segIDs) == 0 {
				continue // 存量会话（逐段向量落库前处理），无值
			}
		} else {
			// 多人：按时长降序取第一个有向量的段（最长段向量缺失时顺延次长）
			p.basis = "longest"
			ordered := append([]voiceTopSegMeta(nil), segs...)
			for i := 1; i < len(ordered); i++ { // 插入排序（段数少，无需 sort 包）
				for j := i; j > 0 && ordered[j].EndMS-ordered[j].StartMS > ordered[j-1].EndMS-ordered[j-1].StartMS; j-- {
					ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
				}
			}
			for _, m := range ordered {
				if m.HasEmb {
					p.segIDs = append(p.segIDs, m.ID)
					break
				}
			}
			if len(p.segIDs) == 0 {
				continue
			}
		}
		plans[sid] = p
		wantIDs = append(wantIDs, p.segIDs...)
	}
	// 第二遍：段向量一次批量取，再逐会话算 top-3 + 判定
	embs, err := h.loadSegEmbeddings(ctx, wantIDs)
	if err != nil {
		log.Printf("[speaker] 列表声纹段向量读取失败: %v", err)
		return out
	}
	for sid, p := range plans {
		var vec []float32
		if p.basis == "whole" {
			vecs := make([][]float32, 0, len(p.segIDs))
			for _, id := range p.segIDs {
				if b, ok := embs[id]; ok {
					if v, ok2 := decodeEmbedding(b); ok2 && len(v) == 256 {
						vecs = append(vecs, v)
					}
				}
			}
			if len(vecs) == 0 {
				continue
			}
			vec = aggregateVecsAPI(vecs)
		} else {
			if b, ok := embs[p.segIDs[0]]; ok {
				if v, ok2 := decodeEmbedding(b); ok2 && len(v) == 256 {
					vec = v
				}
			}
			if vec == nil {
				continue
			}
		}
		ms := topVoiceMatchesVec(lib, vec, 3)
		if len(ms) == 0 {
			continue
		}
		second := 0.0
		if len(ms) > 1 {
			second = ms[1].Similarity
		}
		vt := &voiceTopView{Basis: p.basis, Matches: ms}
		if voiceprint.Matched(ms[0].Similarity, second, threshold) {
			vt.Matched = true
			vt.SpeakerID, vt.SpeakerName = ms[0].SpeakerID, ms[0].Name
			if ms[0].Similarity >= threshold {
				vt.Rule = "strong" // 强命中：过阈值
			} else {
				vt.Rule = "gap" // 区分性弱命中：≥0.72 且领先第二名 ≥0.06
			}
		}
		out[sid] = vt
	}
	return out
}

// listSegMetas 取一批会话的段元信息（id/标签/起止/有无向量），按 session 分组。
// 只取元信息不取向量——embedding 每段 1KB，整页全取会把列表接口拖重；向量按
// sessionVoiceTop 的判定基准再按需取（单人取全部段、多人只取最长一段）。
func (h *QueryHandler) listSegMetas(ctx context.Context, sids []ids.ID) (map[ids.ID][]voiceTopSegMeta, error) {
	out := map[ids.ID][]voiceTopSegMeta{}
	if len(sids) == 0 {
		return out, nil
	}
	ph := make([]string, len(sids))
	args := make([]any, len(sids))
	for i, sid := range sids {
		ph[i] = "?"
		args[i] = sid.Int64()
	}
	var rows []voiceTopSegMeta
	q := `SELECT tr.session_id, seg.id, seg.speaker_label, seg.start_ms, seg.end_ms,
	             (seg.embedding IS NOT NULL) AS has_emb
	      FROM transcript_segment seg JOIN transcript tr ON tr.id = seg.transcript_id
	      WHERE tr.session_id IN (` + strings.Join(ph, ",") + `)`
	if err := h.Transcripts.DB.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, err
	}
	for _, m := range rows {
		out[m.SessionID] = append(out[m.SessionID], m)
	}
	return out, nil
}

// loadSegEmbeddings 按段 id 批量取向量 BLOB（id→blob；缺向量的段不在结果里）。
func (h *QueryHandler) loadSegEmbeddings(ctx context.Context, segIDs []ids.ID) (map[ids.ID][]byte, error) {
	out := map[ids.ID][]byte{}
	if len(segIDs) == 0 {
		return out, nil
	}
	ph := make([]string, len(segIDs))
	args := make([]any, len(segIDs))
	for i, sid := range segIDs {
		ph[i] = "?"
		args[i] = sid.Int64()
	}
	var rows []struct {
		ID        ids.ID `db:"id"`
		Embedding []byte `db:"embedding"`
	}
	q := `SELECT id, embedding FROM transcript_segment WHERE id IN (` + strings.Join(ph, ",") + `)`
	if err := h.Transcripts.DB.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r.Embedding
	}
	return out, nil
}

// aggregateVecsAPI 多段向量取均值再 L2 归一（整段代表声纹）。
// 内联而非 import pipeline（避免 api→pipeline 反向依赖；同 float32BlobAPI 模式），
// 算法与 pipeline.aggregateEmbeddings 完全一致（组代表声纹同一套路）。
func aggregateVecsAPI(vecs [][]float32) []float32 {
	rep := make([]float32, 256)
	for _, v := range vecs {
		for i := 0; i < 256 && i < len(v); i++ {
			rep[i] += v[i]
		}
	}
	n := float32(len(vecs))
	var sumSq float64
	for i := 0; i < 256; i++ {
		rep[i] /= n
		sumSq += float64(rep[i]) * float64(rep[i])
	}
	if sumSq > 0 {
		inv := float32(1.0 / math.Sqrt(sumSq))
		for i := 0; i < 256; i++ {
			rep[i] *= inv
		}
	}
	return rep
}

type segmentView struct {
	ID           string `json:"id"`
	Speaker      string `json:"speaker"`                 // 显示名：解析到用登记名，否则 "说话人 N"
	SpeakerID    string `json:"speaker_id,omitempty"`    // 解析到的已登记说话人 id（未解析则空）
	SpeakerLabel string `json:"speaker_label,omitempty"` // ASR 原始标签（spk0/spk1…，「原始 ASR」视图用）
	Text         string `json:"text"`
	StartMS      int64  `json:"start_ms"`
	EndMS        int64  `json:"end_ms"`
	// VoiceMatches 该段声纹与全库的 top-3 相似（speaker stage 逐段落库的向量算出；
	// 一句话可能混多人，段级 top-1 不是归属说话人即该段可能被切错/归错）。
	// 存量会话（逐段向量落库前处理）无值。json 无 omitempty：前端统一按空数组处理。
	VoiceMatches []voiceMatch `json:"voice_matches"`
	// CorrectedFrom 非空 = 该段被 speaker stage 的幽灵历史声纹纠正 pass 自动改判过；
	// 值为被顶掉的原历史说话人 id，CorrectedFromName 为其显示名（前端"已修改"徽章 + tooltip）。
	CorrectedFrom     string `json:"corrected_from,omitempty"`
	CorrectedFromName string `json:"corrected_from_name,omitempty"`
	// CorrectedReason 非空=该段被自动纠正；'phantom'=幽灵历史声纹改判(配 CorrectedFrom)；
	// 'short'=过短噪声段并入最近在场说话人(CorrectedFrom 为空)。前端据此渲染徽章 + tooltip。
	CorrectedReason string `json:"corrected_reason,omitempty"`
}

func (h *QueryHandler) GetSession(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	s, err := h.Sessions.Get(r.Context(), uid.Int64(), sid)
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
		// 预解码声纹库（一次），供逐段算 top-3 相似度；无库或失败降级为空（不阻断详情）
		var lib []libVoice
		if h.Speakers != nil {
			if all, err := h.Speakers.List(r.Context()); err != nil {
				log.Printf("[speaker] 段级声纹相似度富化失败: %v", err)
			} else {
				lib = libraryWithEntries(all, loadSpeakerEntries(r.Context(), h.SpeakerEmbeddings, all))
			}
		}
		ghostNames := make(map[ids.ID]string) // 纠正原说话人名兜底缓存：同一幽灵 id 只查一次库（避免 N+1）
		views := make([]segmentView, len(segs))
		for i, sg := range segs {
			views[i] = segmentView{
				ID: sg.ID.String(), Text: sg.Text, StartMS: sg.StartMS, EndMS: sg.EndMS,
				SpeakerLabel: sg.SpeakerLabel, VoiceMatches: []voiceMatch{},
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
			// 幽灵历史声纹纠正标记：原历史人可能已不在本会话 speaker 列表（spMap），从 Speakers 兜底解析名字（带缓存）。
			if sg.CorrectedFromSpeakerID != nil {
				gid := *sg.CorrectedFromSpeakerID
				views[i].CorrectedFrom = gid.String()
				if name, ok := spMap[gid]; ok {
					views[i].CorrectedFromName = name
				} else if h.Speakers != nil {
					name, cached := ghostNames[gid]
					if !cached {
						if sp, err := h.Speakers.Get(r.Context(), gid); err != nil {
							log.Printf("[speaker] 纠正原说话人名兜底解析失败 id=%s: %v", gid, err) // FIX 2：与本 handler 其他降级一致，不静默
						} else {
							name = sp.Name
						}
						ghostNames[gid] = name // 命中/失败都缓存，避免重复查库
					}
					views[i].CorrectedFromName = name
				}
			}
			if sg.CorrectedReason != nil {
				views[i].CorrectedReason = *sg.CorrectedReason
			}
			// 段级声纹 top-3（含归属者）：speaker stage 落库的逐段向量 vs 全库余弦
			if len(lib) > 0 && len(sg.Embedding) > 0 {
				if emb, ok := decodeEmbedding(sg.Embedding); ok {
					views[i].VoiceMatches = topVoiceMatchesVec(lib, emb, 3)
				}
			}
		}
		// sis 富化候选名（随机名说话人展示「建议名字」区）；repo 未装配则空候选
		type speakerWithCands struct {
			repo.SpeakerInSegment
			NameCandidates []NameCandidateView `json:"name_candidates"`
		}
		sisView := make([]speakerWithCands, len(sis))
		spIDs := make([]ids.ID, len(sis))
		for i := range sis {
			sisView[i] = speakerWithCands{SpeakerInSegment: sis[i], NameCandidates: []NameCandidateView{}}
			spIDs[i] = sis[i].SpeakerID
		}
		if h.SpeakerNameCandidates != nil {
			cands, err := h.SpeakerNameCandidates.ListBySpeakers(r.Context(), spIDs)
			if err != nil {
				log.Printf("[speaker] 候选名富化失败: %v", err) // 降级为空候选，不阻断详情
			} else {
				idx := make(map[ids.ID]int, len(sisView))
				for i := range sisView {
					idx[sisView[i].SpeakerID] = i
				}
				for _, c := range cands {
					if i, ok := idx[c.SpeakerID]; ok {
						sisView[i].NameCandidates = append(sisView[i].NameCandidates,
							NameCandidateView{Name: c.Name, Confidence: c.Confidence, Evidence: c.Evidence})
					}
				}
			}
		}
		resp["transcript"] = tr
		resp["segments"] = views
		resp["speakers"] = sisView
		// audioscene stage：会话级声学环境（4 字段来自 transcript） + 说话人整体情绪状态
		resp["acoustic_scene"] = tr.AcousticScene
		resp["background_sounds"] = tr.BackgroundSounds
		resp["weather_cues"] = tr.WeatherCues
		resp["overall_mood"] = tr.OverallMood
		if h.SpeakerStates != nil {
			// 行级 user_id 过滤（IDOR 防护）；未装配则不返回该字段
			states, _ := h.SpeakerStates.ListBySession(r.Context(), uid.Int64(), sid)
			resp["speaker_states"] = states
		}
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
	if h.ChangeLogs != nil {
		if changes, err := h.ChangeLogs.ListBySession(r.Context(), sid); err == nil {
			resp["profile_changes"] = changes
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if _, err := h.Sessions.Get(r.Context(), uid.Int64(), sid); err != nil {
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

// jobInProgress 判断该会话当前是否有正在跑/排队的任务（前端「重新提取/重新识别」的
// 防重入闸：处理中再建新 job 会和旧 job 抢同一 session 的数据——如 reidentify 清空
// speaker_id 的同时旧 speaker stage 正在回填）。以 session 当前指向的 job 状态为准。
func (h *QueryHandler) jobInProgress(ctx context.Context, s *repo.AudioSession) bool {
	if s.JobID == nil {
		return false
	}
	j, err := h.Jobs.Get(ctx, *s.JobID)
	if err != nil {
		return false // job 读不到（已删/脏数据）按不在处理中放行
	}
	return j.Status == "pending" || j.Status == "running"
}

// Reextract 基于当前（可能已编辑的）ASR 重新抽取记忆/待办：
// 在 segment stage 建一个 pending job，pool 领取后重算 full_text（segment）→
// speaker（幂等：段已解析则 no-op，不覆盖手动换人、不依赖 sidecar）→
// 重新抽取（extract，对本 session 幂等：删旧 memory/todo 再重插）→ done。
// SetJobID 把 session 指向新 job，前端轮询 GET /api/sessions/{id} 的 job.status 可见进度。
// 必须已有 transcript（无转写的 session 无法跑 segment→speaker→extract）。
// 当前有任务在跑/排队时 409 拒绝（避免重复排队、新旧 job 抢同一 session 数据）。
func (h *QueryHandler) Reextract(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	s, err := h.Sessions.Get(r.Context(), uid.Int64(), sid)
	if err != nil {
		http.Error(w, "session 不存在", http.StatusNotFound)
		return
	}
	if h.jobInProgress(r.Context(), s) {
		http.Error(w, "该录音正在处理中，请等当前任务完成后再操作", http.StatusConflict)
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

// Reidentify 重新识别说话人：清空本会话所有段的 speaker_id（含手动纠正）→ 建 job 从
// speaker stage 起跑（pool 跑 speaker→extract）。清空后 speaker stage 不再幂等跳过，会重新
// 切片提向 + 按最新声纹库 1:N 匹配（用户录入/合并/改名声纹后重算归属用）。
// 区别于 Reextract（segment→speaker→extract，speaker 幂等跳过、不改已有归属）。
// 注意：会覆盖手动换人，前端需二次确认。
// 当前有任务在跑/排队时 409 拒绝（清空 speaker_id 会和正在跑的 speaker stage 竞写）。
func (h *QueryHandler) Reidentify(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	s, err := h.Sessions.Get(r.Context(), uid.Int64(), sid)
	if err != nil {
		http.Error(w, "session 不存在", http.StatusNotFound)
		return
	}
	if h.jobInProgress(r.Context(), s) {
		http.Error(w, "该录音正在处理中，请等当前任务完成后再操作", http.StatusConflict)
		return
	}
	tr, err := h.Transcripts.GetBySession(r.Context(), sid)
	if err != nil {
		http.Error(w, "该会话暂无转写，无法重新识别", http.StatusConflict)
		return
	}
	if err := h.Transcripts.ClearSegmentSpeakers(r.Context(), tr.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	j := &repo.Job{SessionID: sid, Stage: "speaker", Status: "pending"}
	if err := h.Jobs.Create(r.Context(), j); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = h.Sessions.SetJobID(r.Context(), sid, j.ID)
	writeJSON(w, map[string]any{"job_id": j.ID})
}

func (h *QueryHandler) RetryJob(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	// 归属校验：job 无 user_id 列，经其 session 判归属——非本用户的 session 一律 404（不泄漏存在性）。
	if _, err := h.Sessions.Get(r.Context(), uid.Int64(), j.SessionID); err != nil {
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	s, err := h.Sessions.Get(r.Context(), uid.Int64(), sid)
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	s, err := h.Sessions.Get(r.Context(), uid.Int64(), sid)
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

// writeJSONError 返回 JSON 格式错误响应：{error: msg}，带 HTTP 状态码。
// 前端 api() helper 统一按 JSON 解析错误体；http.Error 返回纯文本会导致 JSON.parse 失败。
func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
