// stage_audioscene 实现 audioscene stage（P1 音频场景与情绪理解，spec §4）。
// 在 speakername 之后跑：把整段录音（≤10min 单次 / >10min 分块）送 stepaudio-2.5-chat，
// 产出会话级声学场景 + 每位说话人情绪并落库。全程降级：任何失败只记日志、return nil，
// 绝不阻断流水线（转写与其余 stage 照常完成，富化信号缺失即空）。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// stageAudioScene 返回 audioscene handler。
func stageAudioScene(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
		if !d.AudioInsightEnabled || d.AudioInsight == nil {
			return nil // 开关关闭或未装配：no-op
		}
		s, err := d.Sessions.Get(ctx, 1, sessionID)
		if err != nil {
			log.Printf("[audioscene] 读 session 失败(降级): %v", err)
			return nil
		}
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			log.Printf("[audioscene] 无 transcript(降级): %v", err)
			return nil
		}
		wav, err := transcodeToWAV(d.DataDir, sessionID, s.StoragePath)
		if err != nil {
			log.Printf("[audioscene] 转码失败(降级): %v", err)
			return nil
		}
		segs, _ := d.Transcripts.ListSegments(ctx, tr.ID)
		labels, labelToSpeaker := collectSpeakerLabels(segs)

		chunkSec := d.AudioInsightChunkSec
		if chunkSec <= 0 {
			chunkSec = 600
		}
		durMS := s.DurationMS
		if durMS <= 0 && len(segs) > 0 {
			durMS = segs[len(segs)-1].EndMS
		}
		plans := planChunks(durMS, chunkSec)

		var results []provider.AudioInsight
		if len(plans) <= 1 {
			ins, err := d.AudioInsight.Analyze(ctx, wav, labels)
			if err != nil {
				log.Printf("[audioscene] 分析失败(降级): %v", err)
				return nil
			}
			results = []provider.AudioInsight{ins}
		} else {
			for i, pl := range plans {
				clip, err := sliceChunkWAV(d.DataDir, sessionID, i, wav, pl, i > 0)
				if err != nil {
					log.Printf("[audioscene] 切片 %d 失败(跳过该块): %v", i, err)
					continue
				}
				ins, err := d.AudioInsight.Analyze(ctx, clip, labels)
				_ = os.Remove(clip) // 用后即删临时切片
				if err != nil {
					log.Printf("[audioscene] 块 %d 分析失败(跳过): %v", i, err)
					continue
				}
				results = append(results, ins)
			}
			if len(results) == 0 {
				log.Printf("[audioscene] 所有块失败(降级)")
				return nil
			}
		}
		merged := mergeInsights(results)

		// 落会话级环境
		var bg *json.RawMessage
		if len(merged.BackgroundSounds) > 0 {
			if raw, err := json.Marshal(merged.BackgroundSounds); err == nil {
				rm := json.RawMessage(raw)
				bg = &rm
			}
		}
		if err := d.Transcripts.SetAcoustic(ctx, tr.ID, clipRunes(merged.AcousticScene, 32), bg, clipRunes(merged.WeatherCues, 32), clipRunes(merged.OverallMood, 128)); err != nil {
			log.Printf("[audioscene] 写环境失败: %v", err)
		}
		// 落每人情绪（speaker_id 按 label 映射回填）
		var rows []repo.SpeakerSessionState
		for _, sp := range merged.Speakers {
			row := repo.SpeakerSessionState{
				UserID: 1, TranscriptID: tr.ID, SessionID: sessionID,
				SpeakerLabel: sp.Label, Emotion: clipRunes(sp.Emotion, 32),
				MicroEmotion: clipRunes(sp.MicroEmotion, 64), MentalState: clipRunes(sp.MentalState, 64), Confidence: sp.Confidence,
			}
			if sid, ok := labelToSpeaker[sp.Label]; ok {
				row.SpeakerID = &sid
			}
			rows = append(rows, row)
		}
		if err := d.SpeakerStates.InsertBatch(ctx, rows); err != nil {
			log.Printf("[audioscene] 写说话人情绪失败: %v", err)
		}
		return nil
	}
}

// collectSpeakerLabels 从段收集去重 label（保序）+ label→speaker_id 映射（首个非空 speaker_id）。
func collectSpeakerLabels(segs []repo.TranscriptSegment) ([]string, map[string]ids.ID) {
	var labels []string
	seen := map[string]bool{}
	m := map[string]ids.ID{}
	for _, sg := range segs {
		if sg.SpeakerLabel != "" && !seen[sg.SpeakerLabel] {
			seen[sg.SpeakerLabel] = true
			labels = append(labels, sg.SpeakerLabel)
		}
		if sg.SpeakerID != nil {
			if _, ok := m[sg.SpeakerLabel]; !ok {
				m[sg.SpeakerLabel] = *sg.SpeakerID
			}
		}
	}
	return labels, m
}

// clipRunes 按 rune 截断到 max（DB 列宽保护，中文按字符不按字节）。
func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}

// chunkOverlapMS 相邻块重叠时长（spec §5：无静音可切时下块前移 2s，保边界不丢字/丢情绪）。
const chunkOverlapMS = 2000

// sliceChunkWAV 用 ffmpeg 切出一块。非首块起点前移 chunkOverlapMS（2s 重叠）——静音切点精修
// （silencedetect 在边界 ±1min 找最近静音）作为增强，本版先落固定切点+2s 重叠的可用版。
func sliceChunkWAV(dataDir string, sessionID ids.ID, idx int, srcWAV string, pl chunkPlan, hasPrev bool) (string, error) {
	start := pl.StartMS
	if hasPrev {
		start -= chunkOverlapMS
		if start < 0 {
			start = 0
		}
	}
	dur := pl.EndMS - start
	dst := filepath.Join(dataDir, "transcoded", sessionID.String()+"_chunk"+strconv.Itoa(idx)+".wav")
	cmd := exec.Command("ffmpeg", "-y", "-ss", msToSec(start), "-t", msToSec(dur),
		"-i", srcWAV, "-ar", "16000", "-ac", "1", "-sample_fmt", "s16", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[audioscene] ffmpeg 切片: %s", out)
		return "", err
	}
	return dst, nil
}

// msToSec 毫秒 → 秒字符串（ffmpeg -ss/-t 用，保留 3 位小数）。
func msToSec(ms int64) string { return strconv.FormatFloat(float64(ms)/1000.0, 'f', 3, 64) }

// refineCutBySilence（增强，spec §5）：在 boundaryMS 的 ±silenceWinMS 窗口内用 ffmpeg silencedetect
// 找最近静音区间中点作为切点，避免切断正在说的话。找不到静音返回 boundaryMS（调用方回退固定+重叠）。
// 本版先提供纯解析 helper，silencedetect 的集成作为后续增强。
func parseSilenceBounds(ffmpegLog string) []silenceRange {
	var out []silenceRange
	// ffmpeg silencedetect 输出形如：[silencedetect @ ...] silence_start: 12.34 / silence_end: 13.56
	lines := strings.Split(ffmpegLog, "\n")
	var curStart *float64
	for _, ln := range lines {
		var v float64
		if n, _ := fmt.Sscanf(afterColon(ln, "silence_start"), "%f", &v); n == 1 {
			curStart = &v
		} else if n, _ := fmt.Sscanf(afterColon(ln, "silence_end"), "%f", &v); n == 1 && curStart != nil {
			out = append(out, silenceRange{startSec: *curStart, endSec: v})
			curStart = nil
		}
	}
	return out
}

// silenceRange 是一段静音的时间范围（秒）。
type silenceRange struct{ startSec, endSec float64 }

// afterColon 取 "key: rest" 中匹配 key 的行里冒号后的子串（trimmed）。
func afterColon(line, key string) string {
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	rest = strings.TrimPrefix(rest, ":")
	return strings.TrimSpace(rest)
}
