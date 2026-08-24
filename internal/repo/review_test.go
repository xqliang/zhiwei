package repo

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReviewUpsertDailyIdempotent(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	rr := &ReviewRepo{DB: db}
	ctx := t.Context()
	day := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	if err := rr.UpsertDaily(ctx, 1, day, json.RawMessage(`{"headline":"v1"}`), "ready"); err != nil {
		t.Fatalf("UpsertDaily v1: %v", err)
	}
	// 同一天再写 → 覆盖，不重复行
	if err := rr.UpsertDaily(ctx, 1, day, json.RawMessage(`{"headline":"v2"}`), "ready"); err != nil {
		t.Fatalf("UpsertDaily v2: %v", err)
	}
	got, err := rr.GetDaily(ctx, 1, day)
	if err != nil {
		t.Fatalf("GetDaily: %v", err)
	}
	if got == nil || got.Content == nil {
		t.Fatal("GetDaily 返回空或 content 为 nil")
	}
	var body struct{ Headline string }
	if err := json.Unmarshal(*got.Content, &body); err != nil {
		t.Fatalf("content 非合法 JSON: %v", err)
	}
	if body.Headline != "v2" {
		t.Errorf("应覆盖为 v2, got %q", body.Headline)
	}
}

func TestReviewUpsertWeekly(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	rr := &ReviewRepo{DB: db}
	ctx := t.Context()
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 6)

	if err := rr.UpsertWeekly(ctx, 1, start, end, json.RawMessage(`{"headline":"周"}`), "ready"); err != nil {
		t.Fatalf("UpsertWeekly: %v", err)
	}
	got, err := rr.GetWeekly(ctx, 1, start)
	if err != nil {
		t.Fatalf("GetWeekly: %v", err)
	}
	if got == nil || got.Status != "ready" {
		t.Errorf("GetWeekly 异常: %+v", got)
	}
}

// TestReviewGetDailyMissing 验证不存在时返回 (nil, nil)。
func TestReviewGetDailyMissing(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	rr := &ReviewRepo{DB: db}
	ctx := t.Context()
	got, err := rr.GetDaily(ctx, 1, time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GetDaily missing: %v", err)
	}
	if got != nil {
		t.Errorf("不存在的日报应返回 nil, got %+v", got)
	}
}
