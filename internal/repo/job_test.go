package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestJobLifecycle(t *testing.T) {
	db, _ := NewDB(repotest.DSN(t))
	sr := &SessionRepo{DB: db}
	jr := &JobRepo{DB: db}
	ctx := context.Background()

	// F6 后本测试库 zhiwei_test_repo 由 repo 包测试二进制独占，pipeline_job 无并发写入方；
	// 清掉历史遗留 job（如上一次「脏库连跑」留下的 pending）。否则 ClaimNext 按 id 恒先领到
	// 更老的遗留 job，下面「外任务」分支把它重置→continue，重试循环永远空转（本次 review 阻断项）。
	// 结束时再清一次，让同一测试库对「不 re-init 连续跑」保持幂等。
	if _, err := db.ExecContext(ctx, "DELETE FROM pipeline_job"); err != nil {
		t.Fatalf("清理 pipeline_job: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), "DELETE FROM pipeline_job"); err != nil {
			t.Logf("cleanup 清理 pipeline_job: %v", err)
		}
	})

	sid := ids.New()
	if err := sr.Create(ctx, newTestSession(sid)); err != nil {
		t.Fatal(err)
	}

	j := &Job{SessionID: sid, Stage: "asr", Status: "pending"}
	if err := jr.Create(ctx, j); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// ClaimNext 领取「最老的 pending 任务」——共享测试库被并行测试二进制
	// （如 pipeline 包的 pool 测试）使用时，可能先抢到别人的任务。
	// 重试领取：抢到外任务就还原为 pending（不破坏对方测试），直到领到本任务的。
	var claimed *Job
	deadline := time.Now().Add(10 * time.Second)
	for claimed == nil {
		c, ok, err := jr.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("ClaimNext: %v", err)
		}
		if !ok {
			if time.Now().After(deadline) {
				t.Fatal("10s 内未能领取到本测试创建的任务")
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if c.ID != j.ID {
			// 纵深防御：对齐上面 !ok 分支的 10s 闸门。开头已 DELETE，正常不该领到外任务；
			// 万一（并发/残留）仍反复领到，10s 后判死而非空转，避免再退化成死循环。
			if time.Now().After(deadline) {
				t.Fatal("10s 内一直领到非本测试创建的 job，疑似脏库残留")
			}
			c.Status = "pending"
			if err := jr.Save(ctx, c); err != nil {
				t.Fatalf("还原外任务: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		claimed = c
	}
	if claimed.Status != "running" {
		t.Fatalf("claimed %+v", claimed)
	}

	claimed.Stage = "segment"
	claimed.Status = "pending"
	claimed.Attempt = 0
	if err := jr.Save(ctx, claimed); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _ := jr.Get(ctx, j.ID)
	if got.Stage != "segment" {
		t.Fatalf("want segment, got %s", got.Stage)
	}
}

// 重启恢复：running 的任务要能被重置回 pending
func TestResetRunning(t *testing.T) {
	db, _ := NewDB(repotest.DSN(t))
	jr := &JobRepo{DB: db}
	ctx := context.Background()
	n, err := jr.ResetRunning(ctx)
	if err != nil {
		t.Fatalf("ResetRunning: %v", err)
	}
	_ = n // 只要不出错即可
}
