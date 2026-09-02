// trace.go 提供 job.trace 追加辅助。handler 原地修改 j.Trace，
// 由 pool 在 handler 返回后统一持久化（无读写竞争）。
package pipeline

import (
	"encoding/json"
	"sync"
	"time"

	"zhiwei/internal/repo"
)

// traceMu 保护 appendTrace 的读-改-写（反序列化→append→再序列化）。
// 旧假设「handler 内单线程记 trace」被 correct stage 的并行段级 LLM 调用打破：
// 并发调用会互相覆盖丢条目甚至撞坏 j.Trace，故在包级加锁（其余 stage 串行记
// trace，锁无竞争开销可忽略）。
var traceMu sync.Mutex

// appendTrace 向 job.Trace 追加一条执行记录（并发安全，见 traceMu）。
// 注意 Job.Trace 是 *json.RawMessage（可能为 nil），首次写入时创建。
// 反序列化/序列化错误被刻意忽略：trace 是尽力而为的可观测性数据，
// 已损坏即整段重置，绝不能因它让 handler 失败重试。
func appendTrace(j *repo.Job, e repo.TraceEntry) {
	traceMu.Lock()
	defer traceMu.Unlock()
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
