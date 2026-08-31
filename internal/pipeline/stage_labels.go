package pipeline

// stageLabels 是 stage key → 用户可读中文名的唯一事实源。
// 新增 stage 时：在 BuildStages（stage_asr.go）注册 handler 的同时必须在这里配中文名，
// 否则 TestStageLabelsCoverBuildStages 会红——前端时间线 badge 直接展示后端标签
// （/api/sessions 的 job_stage_label），不再维护前端自己的字典（曾因此漏配 correct）。
var stageLabels = map[string]string{
	"asr":            "语音转写",
	"correct":        "实体纠错",
	"segment":        "全文汇总",
	"speaker":        "声纹识别",
	"speakername":    "名字推断",
	"audioscene":     "场景情绪",
	"emotionprofile": "情绪汇总",
	"extract":        "记忆抽取",
	"profile":        "画像抽取",
}

// StageLabel 返回 stage 的中文显示名；未注册的 key 原样返回（前端 fallback 用）。
func StageLabel(stage string) string {
	if l, ok := stageLabels[stage]; ok {
		return l
	}
	return stage
}
