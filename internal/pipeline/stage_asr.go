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

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
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

	// ---- speakername stage（名字推断）----
	NameInferPrompt       string                         // prompts/speaker_naming_v1.md 内容（system prompt）
	SpeakerNameCandidates *repo.SpeakerNameCandidateRepo // 候选名存取（nil = no-op，兼容旧装配）
	NameInferWindowMin    int                            // 上下文回看窗口（分钟），0 = 默认 10
	NameInferMaxSegments  int                            // 上下文段数上限，0 = 默认 400
}

// BuildStages 返回 stage 名 -> handler 的映射，供 Pool 装配。
func BuildStages(d StageDeps) map[string]Handler {
	return map[string]Handler{
		"asr":         stageASR(d),
		"segment":     stageSegment(d),
		"speaker":     stageSpeaker(d),
		"speakername": stageSpeakerName(d),
		"extract":     stageExtract(d),
	}
}

// stageASR：ffmpeg 统一转 wav16k → ASR → transcript + segments 落库。
func stageASR(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
		s, err := d.Sessions.Get(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读取 session: %w", err)
		}
		wavPath, err := transcodeToWAV(d.DataDir, sessionID, s.StoragePath)
		if err != nil {
			return fmt.Errorf("转码: %w", err)
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
