// stage_correct 实现 correct stage（ASR 实体纠错）：拼音/音素召回候选白名单 →
// LLM 一程裁决（只改白名单内实体）→ 双重门控后局部替换并标记 corrected_reason='entity'。
// 设计见 docs/superpowers/specs/2026-08-29-asr-entity-correction-design.md。
// 本文件先落 prompt 契约与输出解析；stage 主体（runCorrectStage/stageCorrect）见后续提交。
package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
)

// correctionEdit LLM 输出的一条纠错建议（asr_correction_v1 契约）。
type correctionEdit struct {
	Orig       string  `json:"orig"`       // 段内原片段（门控：必须原样出现在段文本里）
	Corrected  string  `json:"corrected"`  // 替换目标（门控：必须逐字等于白名单 canonical）
	EntityID   string  `json:"entity_id"`  // 白名单条目 id（门控：必须在白名单内）
	Confidence float64 `json:"confidence"` // 0~1，clamp
	Reason     string  `json:"reason"`     // 简短依据（存 entity_edits 供前端 tooltip）
}

// correctionResult LLM 输出整体。
type correctionResult struct {
	Edits []correctionEdit `json:"edits"`
}

// ParseCorrectionEdits 解析 LLM 纠错输出（纯函数，可单测）。
// 容错同 ParseNameCandidates：截取首 { 到末 }，剥掉围栏与前后废话；
// 彻底非法 JSON 返回 error（调用方吞错跳过该段，不 fail session）。
// 清洗：置信度 clamp 到 [0,1]；空 orig/corrected 丢弃（无法应用）。
func ParseCorrectionEdits(raw string) ([]correctionEdit, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out correctionResult
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("纠错输出解析失败: %w", err)
	}
	edits := make([]correctionEdit, 0, len(out.Edits))
	for _, e := range out.Edits {
		if strings.TrimSpace(e.Orig) == "" || strings.TrimSpace(e.Corrected) == "" {
			continue
		}
		e.Confidence = clampConfidence(e.Confidence) // 复用 stage_speaker_name.go 的 clamp
		edits = append(edits, e)
	}
	return edits, nil
}
