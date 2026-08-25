package review

import (
	"encoding/json"
	"fmt"
	"strings"
)

// stripToJSON 截取首个 '{' 到末个 '}'，剥掉模型可能输出的代码围栏/前后废话。
// 与 memory.ParseCandidates 的容错同构（那里注释「宁粗勿丢」）。
func stripToJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}

// ParseDaily 解析日报 JSON；非法 JSON 返回 error（调用方置 status=failed）。
func ParseDaily(raw string) (*DailyContent, error) {
	var c DailyContent
	if err := json.Unmarshal([]byte(stripToJSON(raw)), &c); err != nil {
		return nil, fmt.Errorf("日报 JSON 解析失败: %w", err)
	}
	return &c, nil
}

// ParseWeekly 解析周报 JSON。
func ParseWeekly(raw string) (*WeeklyContent, error) {
	var c WeeklyContent
	if err := json.Unmarshal([]byte(stripToJSON(raw)), &c); err != nil {
		return nil, fmt.Errorf("周报 JSON 解析失败: %w", err)
	}
	return &c, nil
}

// ParseTopicStatus 解析话题状态 JSON。
func ParseTopicStatus(raw string) (*TopicStatusContent, error) {
	var c TopicStatusContent
	if err := json.Unmarshal([]byte(stripToJSON(raw)), &c); err != nil {
		return nil, fmt.Errorf("话题状态 JSON 解析失败: %w", err)
	}
	return &c, nil
}
