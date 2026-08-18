package pipeline

import (
	"context"
	"fmt"
	"log"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// Handler 执行一个 stage。sessionID 是流水线的处理对象。
// 返回 nil 即成功，状态机推进到下一 stage。
type Handler func(ctx context.Context, sessionID ids.ID) error

// Pool 是进程内 worker 池：轮询领取 pending 任务并执行。
type Pool struct {
	jobs        *repo.JobRepo
	flow        Flow
	handlers    map[string]Handler
	concurrency int
	onDone      func(ctx context.Context, sessionID ids.ID)
}

func NewPool(jobs *repo.JobRepo, flow Flow, handlers map[string]Handler) *Pool {
	return &Pool{jobs: jobs, flow: flow, handlers: handlers, concurrency: 2}
}

// OnDone 注册流水线完成回调（如把 session 置为 completed）。
func (p *Pool) OnDone(fn func(ctx context.Context, sessionID ids.ID)) { p.onDone = fn }

// Start 阻塞式启动：先恢复遗留 running 任务，再启动 worker 循环。
// 应在 goroutine 中调用。
func (p *Pool) Start(ctx context.Context) {
	if n, err := p.jobs.ResetRunning(ctx); err != nil {
		log.Printf("[pool] 恢复 running 任务失败: %v", err)
	} else if n > 0 {
		log.Printf("[pool] 恢复 %d 个中断任务", n)
	}
	for i := 0; i < p.concurrency; i++ {
		go p.loop(ctx)
	}
}

func (p *Pool) loop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.claimAndRun(ctx)
		}
	}
}

func (p *Pool) claimAndRun(ctx context.Context) {
	j, ok, err := p.jobs.ClaimNext(ctx)
	if err != nil {
		log.Printf("[pool] 领取任务失败: %v", err)
		return
	}
	if !ok {
		return
	}
	h, exists := p.handlers[j.Stage]
	st := JobState{ID: j.ID.Int64(), Stage: j.Stage, Status: j.Status, Attempt: j.Attempt}
	var runErr error
	if !exists {
		runErr = errNoHandler(j.Stage)
	} else {
		begin := time.Now()
		runErr = safeRun(ctx, h, j.SessionID)
		log.Printf("[pool] job=%d stage=%s took=%s err=%v", j.ID, j.Stage, time.Since(begin), runErr)
	}
	_ = p.flow.Apply(&st, runErr)
	persist(ctx, p.jobs, j, st, runErr)
	if st.Status == "done" && p.onDone != nil {
		p.onDone(ctx, j.SessionID)
	}
}

func persist(ctx context.Context, r *repo.JobRepo, j *repo.Job, st JobState, runErr error) {
	j.Stage, j.Status, j.Attempt = st.Stage, st.Status, st.Attempt
	if runErr != nil {
		msg := runErr.Error()
		j.LastError = &msg
	}
	if err := r.Save(ctx, j); err != nil {
		log.Printf("[pool] 保存任务状态失败 job=%d: %v", j.ID, err)
	}
}

func safeRun(ctx context.Context, h Handler, sid ids.ID) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, sid)
}
