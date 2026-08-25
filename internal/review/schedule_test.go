package review

import (
	"testing"
	"time"
)

func TestParseCronHM(t *testing.T) {
	if h, m := parseCronHM("0 22 * * *"); h != 22 || m != 0 {
		t.Errorf("期望 22:00, got %d:%d", h, m)
	}
	if h, m := parseCronHM("30 9 * * *"); h != 9 || m != 30 {
		t.Errorf("期望 09:30, got %d:%d", h, m)
	}
	if h, m := parseCronHM("garbage"); h != 22 || m != 0 {
		t.Errorf("非法应回退 22:00, got %d:%d", h, m)
	}
}

func TestNextDaily(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC) // 周一 10:00
	// 今天 22:00 未过 → 今天 22:00
	if got := nextDaily(now, 22, 0); !got.Equal(time.Date(2026, 8, 24, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("got %s", got)
	}
	// 今天 09:00 已过 → 明天 09:00
	if got := nextDaily(now, 9, 0); !got.Equal(time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("got %s", got)
	}
}

func TestNextWeekly(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) // 周二
	got := nextWeekly(now, 22, 0)
	if got.Weekday() != time.Monday || !got.Equal(time.Date(2026, 8, 31, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("下个周一 22:00 应为 2026-08-31, got %s (%s)", got, got.Weekday())
	}
}
