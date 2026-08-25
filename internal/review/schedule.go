package review

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"
)

// parseCronHM 从 5 字段 cron（"m h * * *"）取分钟+小时；解析失败回退 22:00。
// 只支持纯数字的 m/h 字段（本期日/周报够用），不解析 */范围/列表。
func parseCronHM(expr string) (hour, min int) {
	hour, min = 22, 0 // 默认 22:00（对齐 cfg 默认 "0 22 * * *"）
	f := strings.Fields(expr)
	if len(f) >= 2 {
		if v, err := strconv.Atoi(f[0]); err == nil && v >= 0 && v < 60 {
			min = v
		}
		if v, err := strconv.Atoi(f[1]); err == nil && v >= 0 && v < 24 {
			hour = v
		}
	}
	return hour, min
}

// nextDaily 返回 now 之后最近的「每天 hh:mm」时刻（若今日该时刻已过则明天）。
func nextDaily(now time.Time, hour, min int) time.Time {
	y, m, d := now.Date()
	t := time.Date(y, m, d, hour, min, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// nextWeekly 返回 now 之后最近的「周一 hh:mm」时刻。
func nextWeekly(now time.Time, hour, min int) time.Time {
	t := nextDaily(now, hour, min)
	for t.Weekday() != time.Monday {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// Scheduler 是报告定时器：日报每天、周报每周一，均在 hh:mm 触发。
type Scheduler struct {
	Gen       *Generator
	Hour, Min int
}

// NewScheduler 从 cron 表达式取 hh:mm 构造调度器。
func NewScheduler(gen *Generator, cronExpr string) *Scheduler {
	h, m := parseCronHM(cronExpr)
	return &Scheduler{Gen: gen, Hour: h, Min: m}
}

// Start 启动日报 + 周报两个调度 goroutine；随 ctx 取消退出。
// 触发时对「当前日期/本周」生成（Daily/Weekly 幂等 upsert，重复触发安全）。
func (s *Scheduler) Start(ctx context.Context) {
	go s.loop(ctx, "daily", nextDaily, func(now time.Time) {
		if _, err := s.Gen.Daily(ctx, now); err != nil {
			log.Printf("[review] 日报定时生成失败: %v", err)
		}
	})
	go s.loop(ctx, "weekly", nextWeekly, func(now time.Time) {
		if _, err := s.Gen.Weekly(ctx, mondayOf(now)); err != nil {
			log.Printf("[review] 周报定时生成失败: %v", err)
		}
	})
}

// loop 通用调度循环：算下次触发 → Timer → 触发 fire → 重排；ctx 取消即退。
func (s *Scheduler) loop(ctx context.Context, name string,
	next func(time.Time, int, int) time.Time, fire func(time.Time)) {
	for {
		now := time.Now()
		at := next(now, s.Hour, s.Min)
		timer := time.NewTimer(at.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			log.Printf("[review] %s 定时触发 @ %s", name, at.Format(time.RFC3339))
			fire(time.Now())
		}
	}
}
