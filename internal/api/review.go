package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/review"
)

// ReviewHandler 提供报告读取/生成端点（后端；报告前端页由协调者集成 web/*）。
type ReviewHandler struct {
	Gen *review.Generator
}

// RegisterReviews 挂载 /api/reviews/* 与 /api/topics/{id}/status（router.go 统一接线在协调者侧）。
func RegisterReviews(r chi.Router, gen *review.Generator) {
	h := &ReviewHandler{Gen: gen}
	r.Get("/api/reviews/daily", h.getDaily)
	r.Post("/api/reviews/daily/generate", h.generateDaily)
	r.Get("/api/reviews/weekly", h.getWeekly)
	r.Post("/api/reviews/weekly/generate", h.generateWeekly)
	r.Get("/api/topics/{id}/status", h.getTopicStatus)
}

// parseDateOrToday 解析 ?date=YYYY-MM-DD；空则用今天。第二返回值 ok=false 表示格式非法。
// 时区统一走 time.Local：time.Now() 本就是本地时区，显式日期也用 ParseInLocation(..., Local)
// 解析，二者落在同一 Location，避免「今天=本地 / 显式日期=UTC」导致 dayRange 切出错位的窗口
// （单用户场景按本地自然日语义）。scheduler / MCP 工具的日期默认与此一致。
func parseDateOrToday(s string) (time.Time, bool) {
	if s == "" {
		return time.Now(), true
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// mondayOf 返回 t 所在周的周一 00:00（周报周起点；api 侧本地实现，不跨包引用）。
func mondayOf(t time.Time) time.Time {
	y, m, d := t.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	offset := (int(start.Weekday()) + 6) % 7
	return start.AddDate(0, 0, -offset)
}

// getDaily：有 ready 日报则取，否则生成（latest-or-generate）。
func (h *ReviewHandler) getDaily(w http.ResponseWriter, r *http.Request) {
	date, ok := parseDateOrToday(r.URL.Query().Get("date"))
	if !ok {
		writeJSONError(w, "date 需 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	if row, err := h.Gen.Reviews.GetDaily(r.Context(), 1, date); err == nil && row != nil && row.Status == "ready" {
		writeJSON(w, row)
		return
	}
	row, err := h.Gen.Daily(r.Context(), date)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}

// generateDaily：强制重生成（POST，可选 body {date}）。
func (h *ReviewHandler) generateDaily(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Date string `json:"date"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	date, ok := parseDateOrToday(body.Date)
	if !ok {
		writeJSONError(w, "date 需 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	row, err := h.Gen.Daily(r.Context(), date)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}

// getWeekly：有 ready 周报则取，否则生成。
func (h *ReviewHandler) getWeekly(w http.ResponseWriter, r *http.Request) {
	base, ok := parseDateOrToday(r.URL.Query().Get("week_start"))
	if !ok {
		writeJSONError(w, "week_start 需 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	ws := mondayOf(base)
	if row, err := h.Gen.Reviews.GetWeekly(r.Context(), 1, ws); err == nil && row != nil && row.Status == "ready" {
		writeJSON(w, row)
		return
	}
	row, err := h.Gen.Weekly(r.Context(), ws)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}

// generateWeekly：强制重生成（POST，可选 body {week_start}）。
func (h *ReviewHandler) generateWeekly(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WeekStart string `json:"week_start"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	base, ok := parseDateOrToday(body.WeekStart)
	if !ok {
		writeJSONError(w, "week_start 需 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	row, err := h.Gen.Weekly(r.Context(), mondayOf(base))
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}

// getTopicStatus：有快照则取最新，否则生成；?refresh=1 强制重算。
func (h *ReviewHandler) getTopicStatus(w http.ResponseWriter, r *http.Request) {
	tid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSONError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("refresh") != "1" {
		if row, err := h.Gen.TopicStatuses.GetLatest(r.Context(), tid); err == nil && row != nil {
			writeJSON(w, row)
			return
		}
	}
	row, err := h.Gen.TopicStatus(r.Context(), tid)
	if err != nil {
		// 话题不存在 → 404（客户端给了错 id）；其余生成链路故障 → 502。
		if errors.Is(err, review.ErrTopicNotFound) {
			writeJSONError(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, row)
}
