package repo

import (
	"context"
	"testing"

	"zhiwei/internal/repotest"
)

func TestAsrSettingsRepo(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &AsrSettingsRepo{DB: db}
	const uid int64 = 2
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM asr_settings WHERE user_id = ?", uid) })
	_, _ = db.Exec("DELETE FROM asr_settings WHERE user_id = ?", uid)

	// 1) 无行 → 默认值（关 + 21dB）——新用户不开启，行为与历史一致。
	s, err := r.Get(ctx, uid)
	if err != nil {
		t.Fatalf("Get 默认: %v", err)
	}
	if s.DenoiseEnabled || s.DenoiseAttenLim != 21 {
		t.Fatalf("默认值应为 关+21dB: %+v", s)
	}

	// 2) Upsert 后读回。
	if err := r.Upsert(ctx, uid, true, 35.5); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	s, _ = r.Get(ctx, uid)
	if !s.DenoiseEnabled || s.DenoiseAttenLim != 35.5 {
		t.Fatalf("读回不符: %+v", s)
	}
	// 3) 再 Upsert 覆盖（单行更新而非新行）。
	if err := r.Upsert(ctx, uid, false, 12); err != nil {
		t.Fatalf("Upsert 覆盖: %v", err)
	}
	var n int
	_ = db.Get(&n, "SELECT COUNT(*) FROM asr_settings WHERE user_id = ?", uid)
	s, _ = r.Get(ctx, uid)
	if n != 1 || s.DenoiseEnabled || s.DenoiseAttenLim != 12 {
		t.Fatalf("覆盖后应单行且值为 关+12: n=%d %+v", n, s)
	}
	// 4) 强度越界被拒绝。
	if err := r.Upsert(ctx, uid, true, -1); err == nil {
		t.Fatal("强度 -1 应被拒绝")
	}
	if err := r.Upsert(ctx, uid, true, 101); err == nil {
		t.Fatal("强度 101 应被拒绝")
	}
}
