// Package memory 实现记忆抽取的纯逻辑：对话块聚合、窗口切分、候选解析、
// 质量闸门与 Topic 归属决策。全部不碰 DB 与网络，可完全单元测试。
// LLM 编排（Extractor）也在此包，但依赖以接口注入，测试用 fake。
package memory

import (
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// Block 是连续同说话人的段聚合成的对话块，抽取的最小输入单元。
type Block struct {
	SpeakerLabel string
	Text         string
	StartMS      int64
	EndMS        int64
	SegmentIDs   []ids.ID
}

// AggregateBlocks 把转写分段聚合为对话块：连续同说话人且间隔不超过 gapMS 的段
// 合并；换说话人或间隔超阈值则切块；空文本段跳过。
// 前置条件：假定 segs 按时间升序（上游 ListSegments 按 sequence_no 排序）；
// 乱序输入会产生负间隔被误合并。
func AggregateBlocks(segs []repo.TranscriptSegment, gapMS int64) []Block {
	var blocks []Block
	for _, s := range segs {
		if s.Text == "" {
			continue
		}
		if n := len(blocks); n > 0 {
			last := &blocks[n-1]
			if last.SpeakerLabel == s.SpeakerLabel && s.StartMS-last.EndMS <= gapMS {
				last.Text += s.Text
				last.EndMS = s.EndMS
				last.SegmentIDs = append(last.SegmentIDs, s.ID)
				continue
			}
		}
		blocks = append(blocks, Block{
			SpeakerLabel: s.SpeakerLabel, Text: s.Text,
			StartMS: s.StartMS, EndMS: s.EndMS, SegmentIDs: []ids.ID{s.ID},
		})
	}
	return blocks
}

// SplitWindows 按窗口大小切分对话块（每窗口一次 LLM 调用）。
// window <= 0 时用默认 10；空输入返回 nil。
func SplitWindows(blocks []Block, window int) [][]Block {
	if window <= 0 {
		window = 10
	}
	if len(blocks) == 0 {
		return nil
	}
	if len(blocks) <= window {
		return [][]Block{blocks}
	}
	var out [][]Block
	for i := 0; i < len(blocks); i += window {
		end := i + window
		if end > len(blocks) {
			end = len(blocks)
		}
		out = append(out, blocks[i:end])
	}
	return out
}
