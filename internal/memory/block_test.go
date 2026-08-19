package memory

import (
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func seg(id int64, speaker string, text string, start, end int64) repo.TranscriptSegment {
	return repo.TranscriptSegment{ID: ids.ID(id), SpeakerLabel: speaker,
		Text: text, StartMS: start, EndMS: end}
}

func TestAggregateBlocks(t *testing.T) {
	segs := []repo.TranscriptSegment{
		seg(1, "1", "明天记得", 0, 1000),
		seg(2, "1", "给 Tom 发邮件", 1100, 2000), // 同说话人、间隔 100ms → 合并
		seg(3, "2", "好的", 2100, 2500),        // 换说话人 → 新块
		seg(4, "1", "另外一件事", 3000, 3500),     // 换回来 → 新块
		seg(5, "1", "隔了很久的话", 40000, 41000),  // 同说话人但间隔 >30s → 强制切块
		seg(6, "1", "", 42000, 43000),        // 空文本 → 跳过
	}
	blocks := AggregateBlocks(segs, 30000)
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4: %+v", len(blocks), blocks)
	}
	b0 := blocks[0]
	if b0.Text != "明天记得给 Tom 发邮件" || b0.SpeakerLabel != "1" {
		t.Fatalf("b0 = %+v", b0)
	}
	if len(b0.SegmentIDs) != 2 || b0.SegmentIDs[0] != 1 || b0.SegmentIDs[1] != 2 {
		t.Fatalf("b0.SegmentIDs = %v", b0.SegmentIDs)
	}
	if b0.StartMS != 0 || b0.EndMS != 2000 {
		t.Fatalf("b0 时间 = %d-%d", b0.StartMS, b0.EndMS)
	}
	if blocks[3].Text != "隔了很久的话" || len(blocks[3].SegmentIDs) != 1 {
		t.Fatalf("b3 = %+v", blocks[3])
	}
}

func TestAggregateBlocksEmpty(t *testing.T) {
	if got := AggregateBlocks(nil, 30000); got != nil {
		t.Fatalf("nil in -> %v", got)
	}
	if got := AggregateBlocks([]repo.TranscriptSegment{seg(1, "1", "", 0, 100)}, 30000); len(got) != 0 {
		t.Fatalf("全空文本 -> %v", got)
	}
}

// 边界：同说话人、间隔恰好等于 gapMS（<=）→ 仍应合并
func TestAggregateBlocksGapBoundary(t *testing.T) {
	segs := []repo.TranscriptSegment{
		seg(1, "1", "前一句", 0, 1000),
		seg(2, "1", "恰好间隔三十秒", 31000, 32000), // 31000-1000 == 30000 → 合并
	}
	blocks := AggregateBlocks(segs, 30000)
	if len(blocks) != 1 || blocks[0].Text != "前一句恰好间隔三十秒" || len(blocks[0].SegmentIDs) != 2 {
		t.Fatalf("边界间隔应合并: %+v", blocks)
	}
}

func TestSplitWindows(t *testing.T) {
	mk := func(n int) []Block {
		bs := make([]Block, n)
		for i := range bs {
			bs[i] = Block{Text: "b"}
		}
		return bs
	}
	// 不超过窗口大小：单窗口
	if w := SplitWindows(mk(10), 10); len(w) != 1 || len(w[0]) != 10 {
		t.Fatalf("10/10 -> %v", winLens(w))
	}
	// 超过：整窗 + 末尾残窗
	w := SplitWindows(mk(25), 10)
	if len(w) != 3 || len(w[0]) != 10 || len(w[2]) != 5 {
		t.Fatalf("25/10 -> %v", winLens(w))
	}
	// 空输入
	if w := SplitWindows(nil, 10); w != nil {
		t.Fatalf("nil -> %v", winLens(w))
	}
	// 窗口参数非法时回退默认 10
	if w := SplitWindows(mk(11), 0); len(w) != 2 {
		t.Fatalf("11/0 -> %v", winLens(w))
	}
}

func winLens(ws [][]Block) []int {
	out := make([]int, len(ws))
	for i, w := range ws {
		out[i] = len(w)
	}
	return out
}
