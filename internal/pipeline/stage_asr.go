// stage_asr 实现 asr（转码+识别+落库）与 segment（聚合+全文）两个 stage。
package pipeline

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/entity"
	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/profile"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// StageDeps 是 stage 的依赖集合（接口化便于测试注入）。
type StageDeps struct {
	Sessions    *repo.SessionRepo
	Transcripts *repo.TranscriptRepo
	ASR         provider.ASRProvider
	DataDir     string // 转码输出目录

	// ---- ASR 前降噪（DeepFilterNet3，2026-09-02）----
	// AsrSettings 每用户降噪配置（开关+强度）；nil=无设置能力 → 不降噪。
	AsrSettings *repo.AsrSettingsRepo
	// Denoise 降噪实现（子进程 DeepFilterNet3）；nil=环境未装配 → 不降噪。
	Denoise Denoiser

	// ---- Sprint 2：extract stage ----
	DB            *sqlx.DB // 开启 commit 事务用
	Memories      *repo.MemoryRepo
	Todos         *repo.TodoRepo
	Topics        *repo.TopicRepo
	MemoryTopics  *repo.MemoryTopicRepo // 多对多关联表 DAO（commit 写关联 + 重链）
	TodoTopics    *repo.TodoTopicRepo   // 多对多关联表 DAO（commit 写关联 + 重链）
	LLM           provider.LLMProvider
	LLMModel      string            // Tier 1 flash 模型名
	Prompt        string            // prompts/extraction_v2.md 内容（system prompt）
	PromptVersion string            // prompt 文件名版本，如 extraction_v2（写 trace 用）
	ExtractWindow int               // 窗口切分大小（块数），0 = 用默认
	Gate          memory.GateConfig // 质量闸门阈值

	// ---- speaker stage ----
	Voiceprint          voiceprint.Client
	Speakers            *repo.SpeakerRepo
	VoiceprintThreshold float64 // ZW_VOICEPRINT_THRESHOLD，0 表示用默认 0.8
	// VoiceprintCorrectMargin 幽灵历史声纹纠正的领先幅度门槛（ZW_VOICEPRINT_CORRECT_MARGIN）。
	// 0 表示用默认 0.06。仅 speaker stage 的纠正 pass 使用。
	VoiceprintCorrectMargin float64
	// VoiceprintFragmentMS 「碎片组」判定阈值（ZW_VOICEPRINT_FRAGMENT_MS，毫秒，0=默认 10000）：
	// 组内段总时长小于此值的 ASR 标签组视为 diarization 碎片——其库内命中/未命中都不可信，
	// 优先并入「在场且更主要」的说话人（见 stage_speaker.go 碎片在场优先两机制）。
	VoiceprintFragmentMS int64
	// VoiceprintInSessionMin 碎片在场归并的最低相似度（ZW_VOICEPRINT_INSESSION_MIN，0=默认
	// voiceprint.SoftMin 0.72）：碎片组段向量对在场说话人样本的 segMax 相似度达到此值才并入。
	VoiceprintInSessionMin float64
	// SpeakerEmbeddings 多条声纹样本 repo（自动登记时落样本行；nil = 兼容旧装配/测试，跳过）
	SpeakerEmbeddings *repo.SpeakerEmbeddingRepo

	// ---- speakername stage（名字推断）----
	NameInferPrompt       string                         // prompts/speaker_naming_v1.md 内容（system prompt）
	SpeakerNameCandidates *repo.SpeakerNameCandidateRepo // 候选名存取（nil = no-op，兼容旧装配）
	NameInferWindowMin    int                            // 上下文回看窗口（分钟），0 = 默认 10
	NameInferMaxSegments  int                            // 上下文段数上限，0 = 默认 400

	// ---- correct stage（ASR 实体纠错）----
	EntityKB       *repo.EntityKBRepo       // 实体知识库（manual 条目）；nil = no-op（兼容旧装配/测试）
	EntitySettings *repo.EntitySettingsRepo // 纠错配置（每用户开关/阈值/auto_sources）；nil = no-op
	EntitySeed     entity.SeedDeps          // 实时聚合依赖（person/pet/speaker/topic 等来源 repo）
	EntityDisabled *repo.EntityDisabledRepo // 禁用名单（持久停用的自动实体名）；nil = 无禁用
	CorrectPrompt  string                   // prompts/asr_correction_v1.md 内容（system prompt）
	// CorrectPromptVersion prompt 文件版本名（如 asr_correction_v1），写 trace 用。
	CorrectPromptVersion string
	// CorrectEnabled 总开关（ZW_ENTITY_CORRECT_ENABLED）。stage 常驻 stagesList、
	// 开关在此生效（false → no-op）——中段 stage 不能按开关从 stagesList 移除，
	// 否则恰停在该 stage 的在途 job 会因 Flow.Next 无后继直接判完成、静默丢后续。
	CorrectEnabled bool // false = stage no-op（流水线照常推进）
	// CorrectConcurrency 逐段 LLM 调用的并发数（ZW_ENTITY_CORRECT_CONCURRENCY，0=默认 6）。
	// 段间判定相互独立（上下文读原始文本、应用只写 DB 不回写内存），并行不改语义；
	// 1 = 退回串行。见 stage_correct.go defaultCorrectConcurrency 注释。
	CorrectConcurrency int
	CorrectWindow      int     // 上下文前后段数，0 = 默认 2
	CorrectTopK        int     // 召回 Top-K，0 = 默认 5
	CorrectMinSim      float64 // 召回相似度下限，0 = 默认 0.6
	// CorrectMaxLLMCalls 逐段 LLM 调用的会话级上限（成本护栏），0 = 默认 500。
	CorrectMaxLLMCalls int

	// ---- profile stage（用户画像 P1）----
	Profile *profile.Service // 画像编排服务（ExtractSession / 手动 CRUD / 确认队列）

	// ---- audioscene stage（P1 音频场景与情绪）----
	AudioInsight         provider.AudioInsightProvider // nil = no-op（开关/未装配时跳过）
	SpeakerStates        *repo.SpeakerSessionStateRepo
	AudioInsightEnabled  bool
	AudioInsightChunkSec int // 0 = 默认 600（>此长度分块识别）

	// ---- emotionprofile stage（P2 人物情绪汇总）----
	PersonMetrics         *repo.PersonMetricRepo
	Persons               *repo.PersonRepo
	EmotionProfileEnabled bool
}

// BuildStages 返回 stage 名 -> handler 的映射，供 Pool 装配。
func BuildStages(d StageDeps) map[string]Handler {
	return map[string]Handler{
		"asr":            stageASR(d),
		"correct":        stageCorrect(d),
		"segment":        stageSegment(d),
		"speaker":        stageSpeaker(d),
		"speakername":    stageSpeakerName(d),
		"audioscene":     stageAudioScene(d),
		"emotionprofile": stageEmotionProfile(d),
		"extract":        stageExtract(d),
		"profile":        stageProfile(d),
	}
}

// stageASR：ffmpeg 统一转 wav16k →（可选）DeepFilterNet3 降噪 → ASR → transcript + segments 落库。
// 降噪只作用于送 ASR 的音频；声纹切片（speaker stage）仍用原始 transcoded wav——
// 既有声纹库从原始音频登记，降噪版会改变嵌入分布、破坏跨 session 相似度可比性。
func stageASR(d StageDeps) Handler {
	return func(ctx context.Context, j *repo.Job, sessionID ids.ID) error {
		s, err := d.Sessions.Get(ctx, 1, sessionID) // 阶段1：后台流水线无请求上下文，暂 user-1
		if err != nil {
			return fmt.Errorf("读取 session: %w", err)
		}
		wavPath, err := transcodeToWAV(d.DataDir, sessionID, s.StoragePath)
		if err != nil {
			return fmt.Errorf("转码: %w", err)
		}
		if asrPath, ok := denoiseForASR(ctx, d, j, sessionID, s.UserID, wavPath); ok {
			wavPath = asrPath
		}
		pieces, err := d.ASR.Transcribe(ctx, wavPath)
		if err != nil {
			return fmt.Errorf("asr: %w", err)
		}
		tr := &repo.Transcript{SessionID: sessionID, Language: "zh-CN"}
		if err := d.Transcripts.Create(ctx, tr); err != nil {
			return fmt.Errorf("写 transcript: %w", err)
		}
		segs := make([]repo.TranscriptSegment, len(pieces))
		for i, p := range pieces {
			conf := p.Confidence
			segs[i] = repo.TranscriptSegment{
				TranscriptID: tr.ID, SequenceNo: i + 1,
				SpeakerLabel: p.SpeakerLabel, Text: p.Text,
				StartMS: p.StartMS, EndMS: p.EndMS, Confidence: &conf,
			}
		}
		if err := d.Transcripts.InsertSegments(ctx, segs); err != nil {
			return fmt.Errorf("写 segments: %w", err)
		}
		return nil
	}
}

// stageSegment：把 segments 汇总成全文（Sprint 2 的 extract 将以
// 「连续同说话人聚合块」为输入；本 stage 先做全文字段并完成流水线）。
func stageSegment(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读取 transcript: %w", err)
		}
		segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
		if err != nil {
			return fmt.Errorf("读取 segments: %w", err)
		}
		var sb strings.Builder
		var sumConf, n float64
		for _, s := range segs {
			if s.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			label := s.SpeakerLabel
			if label == "" {
				fmt.Fprintf(&sb, "[未知] %s", s.Text)
			} else {
				fmt.Fprintf(&sb, "[%s] %s", label, s.Text)
			}
			if s.Confidence != nil {
				sumConf += *s.Confidence
				n++
			}
		}
		conf := 0.0
		if n > 0 {
			conf = sumConf / n
		}
		return d.Transcripts.SetFullText(ctx, tr.ID, sb.String(), conf)
	}
}

// transcodeToWAV 任意输入转 16k mono s16 wav，输出到 data/transcoded/。
func transcodeToWAV(dataDir string, sessionID ids.ID, src string) (string, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "transcoded"), 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dataDir, "transcoded", sessionID.String()+".wav")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil // 幂等：转码产物已存在直接复用
	}
	cmd := exec.Command("ffmpeg", "-y", "-i", src,
		"-ar", "16000", "-ac", "1", "-sample_fmt", "s16", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("ffmpeg 输出: %s", out)
		return "", err
	}
	return dst, nil
}
