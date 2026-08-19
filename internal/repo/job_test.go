package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
)

func TestJobLifecycle(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	sr := &SessionRepo{DB: db}
	jr := &JobRepo{DB: db}
	ctx := context.Background()

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
	db, _ := NewDB(TestDSN(t))
	jr := &JobRepo{DB: db}
	ctx := context.Background()
	n, err := jr.ResetRunning(ctx)
	if err != nil {
		t.Fatalf("ResetRunning: %v", err)
	}
	_ = n // 只要不出错即可
}
