package memory

import (
	"sort"
	"strings"

	"zhiwei/internal/ids"
)

// NaturalKey 是 memory/todo 跨 extract 重跑的稳定标识：来源块的 segment id 集合 + 标题。
// segment 来自 asr/segment stage，extract 重跑不动 segment → 跨重跑稳定。
// 用于重跑时按自然键快照与重链 source='user' 的手动 topic 关联（spec §6）。
// 排序保证 segment 顺序无关；\x1f 分隔避免 segment 与 title 串扰。
func NaturalKey(segmentIDs []ids.ID, title string) string {
	tmp := make([]string, len(segmentIDs))
	for i, id := range segmentIDs {
		tmp[i] = id.String()
	}
	sort.Strings(tmp)
	return strings.Join(tmp, ",") + "\x1f" + title
}
