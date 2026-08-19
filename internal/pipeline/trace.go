// trace.go 提供 job.trace 追加辅助。handler 原地修改 j.Trace，
// 由 pool 在 handler 返回后统一持久化（无读写竞争）。
package pipeline

import (
	"encoding/json"
	"time"

	"zhiwei/internal/repo"
)

// appendTrace 向 job.Trace 追加一条执行记录。
// 注意 Job.Trace 是 *json.RawMessage（可能为 nil），首次写入时创建。
// 反序列化/序列化错误被刻意忽略：trace 是尽力而为的可观测性数据，
// 已损坏即整段重置，绝不能因它让 handler 失败重试。
func appendTrace(j *repo.Job, e repo.TraceEntry) {
	var entries []repo.TraceEntry
	if j.Trace != nil && len(*j.Trace) > 0 {
		_ = json.Unmarshal(*j.Trace, &entries)
	}
	e.At = time.Now()
	entries = append(entries, e)
	b, err := json.Marshal(entries)
	if err == nil {
		raw := json.RawMessage(b)
		j.Trace = &raw
	}
}
