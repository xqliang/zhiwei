package pipeline

import "testing"

// TestStageLabelsCoverBuildStages 守护测试：BuildStages 注册的每个 stage 都必须有
// 中文名（stageLabels 非空且非原样回退）。新增 stage 忘配中文时此测试直接红，
// 防止「新功能上线、时间线 badge 显示英文 key」再次发生（correct 曾漏配）。
func TestStageLabelsCoverBuildStages(t *testing.T) {
	stages := BuildStages(StageDeps{})
	if len(stages) == 0 {
		t.Fatal("BuildStages 返回空映射，流水线装配异常")
	}
	for stage := range stages {
		got := StageLabel(stage)
		if got == "" {
			t.Errorf("stage %q 未配置中文显示名（stageLabels 缺条目）", stage)
			continue
		}
		// 标签等于 key 本身 = 没查到映射（StageLabel 的回退行为），视同漏配。
		if got == stage {
			t.Errorf("stage %q 的中文显示名缺失（StageLabel 原样回退）", stage)
		}
		// 粗查确实中文化了：至少含一个 CJK 字符。
		if !hasCJK(got) {
			t.Errorf("stage %q 的显示名 %q 不含中文，疑似漏配", stage, got)
		}
	}
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
