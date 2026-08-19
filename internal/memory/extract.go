package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// ExtractStats 是一次 Extract 的调用统计（写 job.trace 用）。
type ExtractStats struct {
	Windows int // LLM 调用次数（窗口数）
	Tokens  int // 累计 token 用量
}

// Extractor 用 LLM 从对话块抽取记忆候选：按窗口逐次调用、合并去重、
// 填充 provenance（SegmentIDs）与近似事件时间（EventAt）。
type Extractor struct {
	LLM    provider.LLMProvider
	Model  string // 模型名（Tier 1 flash）
	Prompt string // 系统指令（prompts/extraction_v1.md 内容，含版本说明）
	Window int    // 窗口大小（块数），<=0 时 SplitWindows 内部回退默认

	// stats 记录最近一次 Extract 的调用统计。
	// 并发说明：每个 stage 调用各自 new 一个 Extractor（handler 内构造），
	// 单实例不跨 goroutine 共享，无需加锁。
	stats ExtractStats
}

// Stats 返回最近一次 Extract 的统计。
func (e *Extractor) Stats() ExtractStats {
	return e.stats
}

// Extract 抽取全部对话块。baseTime 是会话基准时间（session.created_at），
// EventAt = baseTime + 块 start_ms 偏移。
// 跨窗口同 title+content 的候选视为重复，保留置信度高者。
func (e *Extractor) Extract(ctx context.Context, blocks []Block, topics []repo.Topic, baseTime time.Time) ([]Candidate, error) {
	// 统计在 Extract 开始时重置，反映「最近一次」调用的窗口数与 token 用量。
	e.stats = ExtractStats{}
	var all []Candidate
	seen := map[string]int{} // title\x00content -> 在 all 中的下标
	for winIdx, win := range SplitWindows(blocks, e.Window) {
		resp, err := e.LLM.Chat(ctx, provider.ChatRequest{
			Model:  e.Model,
			System: e.Prompt,
			User:   buildUserMessage(win, topics),
		})
		if err != nil {
			return nil, fmt.Errorf("LLM 调用: %w", err)
		}
		// 每个窗口一次 LLM 调用；token 用量按窗口累加（resp.TotalTokens
		// 是本次调用的总量，由 provider 从 usage.total_tokens 解析）。
		e.stats.Windows++
		e.stats.Tokens += resp.TotalTokens
		cands, err := ParseCandidates(resp.Content)
		if err != nil {
			return nil, fmt.Errorf("第 %d 窗口解析失败: %w", winIdx+1, err)
		}
		for _, c := range cands {
			c.SegmentIDs, c.EventAt = blockProvenance(win, c.BlockIndex, baseTime)
			key := c.Title + "\x00" + c.Content
			if idx, ok := seen[key]; ok {
				if c.Confidence > all[idx].Confidence {
					all[idx] = c
				}
				continue
			}
			seen[key] = len(all)
			all = append(all, c)
		}
	}
	return all, nil
}

// blockProvenance 由 block_index 定位来源块；越界时用整个窗口的
// segment 并集与窗口起点兜底（宁粗勿丢）。
func blockProvenance(win []Block, idx int, base time.Time) ([]ids.ID, time.Time) {
	if idx >= 1 && idx <= len(win) {
		b := win[idx-1]
		return b.SegmentIDs, base.Add(time.Duration(b.StartMS) * time.Millisecond)
	}
	var segs []ids.ID
	start := int64(0)
	if len(win) > 0 {
		start = win[0].StartMS
		for _, b := range win {
			segs = append(segs, b.SegmentIDs...)
		}
	}
	return segs, base.Add(time.Duration(start) * time.Millisecond)
}

// buildUserMessage 组装用户消息：对话块列表 + 已有主题列表。
func buildUserMessage(win []Block, topics []repo.Topic) string {
	var sb strings.Builder
	sb.WriteString("对话块列表（格式：序号|说话人|时间偏移|文本）：\n")
	for i, b := range win {
		speaker := b.SpeakerLabel
		if speaker == "" {
			speaker = "未知"
		}
		fmt.Fprintf(&sb, "%d|%s|%s|%s\n", i+1, speaker, fmtOffset(b.StartMS), b.Text)
	}
	sb.WriteString("\n已有主题列表（格式：topic_id|名称），请优先归入已有主题：\n")
	if len(topics) == 0 {
		sb.WriteString("（暂无）\n")
	}
	for _, tp := range topics {
		fmt.Fprintf(&sb, "%s|%s\n", tp.ID.String(), tp.Name)
	}
	return sb.String()
}

// fmtOffset 毫秒 → HH:MM:SS
func fmtOffset(ms int64) string {
	s := ms / 1000
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}
