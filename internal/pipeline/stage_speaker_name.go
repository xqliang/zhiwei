// stage_speaker_name 实现 speakername stage：对「名字仍是自动随机名」的说话人，
// 用 LLM 从跨录音墙钟窗口（当前录音全文 + 前 N 分钟）的对话转写推断真实称呼，
// 候选（名称+置信度+证据）写入 speaker_name_candidate。仅作建议，不改名。
// 设计见 docs/superpowers/specs/2026-08-24-speaker-name-inference-design.md。
package pipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// autoNamePattern 自动登记的默认名：说话人 + 5 位 [a-z0-9]（stage_speaker.go rand5 产物）。
// 用户改名后不再命中 → 不再重复推断（比 source=auto 判定准：source 不随改名变）。
// 注意与显示回退「说话人 N」（带空格）区分——那个从不落库为 speaker.name。
var autoNamePattern = regexp.MustCompile(`^说话人[a-z0-9]{5}$`)

// isAutoName 判断说话人名是否仍是自动登记的随机名（= 待识别）。
func isAutoName(name string) bool { return autoNamePattern.MatchString(name) }

// nameCandidate LLM 输出的一条候选名。
type nameCandidate struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
}

// nameInferResult LLM 输出整体：每个待识别人物（ref=占位符）一组候选。
type nameInferResult struct {
	Speakers []struct {
		Ref        string          `json:"ref"`
		Candidates []nameCandidate `json:"candidates"`
	} `json:"speakers"`
}

// ParseNameCandidates 解析 LLM 输出为 ref→候选列表（纯函数，可单测）。
// 容错同 memory.ParseCandidates：截取首 { 到末 }，剥掉围栏与前后废话；
// 彻底非法 JSON 返回 error（由 stage 走重试）。
// 候选内清洗：空名丢弃、置信度 clamp 到 [0,1]、按置信度倒排。
func ParseNameCandidates(raw string) (map[string][]nameCandidate, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out nameInferResult
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("名字推断结果解析失败: %w", err)
	}
	m := make(map[string][]nameCandidate, len(out.Speakers))
	for _, sp := range out.Speakers {
		cands := make([]nameCandidate, 0, len(sp.Candidates))
		for _, c := range sp.Candidates {
			if strings.TrimSpace(c.Name) == "" {
				continue // 无名候选丢弃
			}
			c.Name = strings.TrimSpace(c.Name)
			c.Confidence = clampConfidence(c.Confidence)
			cands = append(cands, c)
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].Confidence > cands[j].Confidence })
		m[sp.Ref] = cands
	}
	return m, nil
}

// clampConfidence 置信度越界归位 [0,1]。
func clampConfidence(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
