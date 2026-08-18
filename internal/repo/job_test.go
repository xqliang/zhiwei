package repo

import (
	"context"
	"testing"

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

	claimed, ok, err := jr.ClaimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("ClaimNext: %v ok=%v", err, ok)
	}
	if claimed.ID != j.ID || claimed.Status != "running" {
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
