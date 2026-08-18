// state.go 是 pipeline 的纯逻辑：stage 推进与重试决策。
// 不碰 DB / 网络，保证可以完全单元测试。
package pipeline

// StageDone 表示整条流水线完成。
const StageDone = "done"

// JobState 是 pool 与 DAO 之间传递的任务快照（repo.Job 的子集，
// 拆出来是为了让状态机不依赖 sqlx）。
type JobState struct {
	ID      int64
	Stage   string
	Status  string
	Attempt int
}

// Flow 描述一条 stage 流水线及其重试策略。
type Flow struct {
	Stages     []string // 按执行顺序
	MaxAttempt int      // 每 stage 最大尝试次数（含首次）
}

// Next 返回下一 stage；已是最后一个则返回 StageDone。
func (f Flow) Next(stage string) string {
	for i, s := range f.Stages {
		if s == stage {
			if i+1 < len(f.Stages) {
				return f.Stages[i+1]
			}
			return StageDone
		}
	}
	return StageDone
}

// Apply 把一次执行结果应用到任务状态上（原地修改）。
// err == nil：推进到下一 stage（或 done），attempt 清零。
// err != nil：attempt+1；未超上限回 pending 重试，超了进 failed。
func (f Flow) Apply(j *JobState, err error) error {
	max := f.MaxAttempt
	if max <= 0 {
		max = 3
	}
	if err == nil {
		j.Stage = f.Next(j.Stage)
		j.Attempt = 0
		if j.Stage == StageDone {
			j.Status = "done"
		} else {
			j.Status = "pending"
		}
		return nil
	}
	j.Attempt++
	if j.Attempt >= max {
		j.Status = "failed"
	} else {
		j.Status = "pending"
	}
	return nil
}
