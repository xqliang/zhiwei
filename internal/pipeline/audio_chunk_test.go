package pipeline

import (
	"testing"

	"zhiwei/internal/provider"
)

// mergeInsights：多块 → 会话级取众数/并集，每说话人按最高置信合并。
func TestMergeInsights(t *testing.T) {
	chunks := []provider.AudioInsight{
		{AcousticScene: "室内", BackgroundSounds: []string{"键盘"}, WeatherCues: "无", OverallMood: "专注",
			Speakers: []provider.SpeakerInsight{{Label: "1", Emotion: "平静", MicroEmotion: "专注", Confidence: 0.6}}},
		{AcousticScene: "室内", BackgroundSounds: []string{"车流"}, WeatherCues: "无", OverallMood: "疲惫",
			Speakers: []provider.SpeakerInsight{{Label: "1", Emotion: "焦虑", MicroEmotion: "急促", Confidence: 0.9}, {Label: "2", Emotion: "喜悦", Confidence: 0.7}}},
	}
	m := mergeInsights(chunks)
	if m.AcousticScene != "室内" {
		t.Errorf("scene=%q", m.AcousticScene)
	}
	if len(m.BackgroundSounds) != 2 {
		t.Errorf("bg 应并集去重=2, got %v", m.BackgroundSounds)
	}
	// 说话人1 取最高置信块(0.9)的 emotion=焦虑
	var s1 *provider.SpeakerInsight
	for i := range m.Speakers {
		if m.Speakers[i].Label == "1" {
			s1 = &m.Speakers[i]
		}
	}
	if s1 == nil || s1.Emotion != "焦虑" {
		t.Errorf("说话人1 应取最高置信情绪=焦虑, got %+v", s1)
	}
	if len(m.Speakers) != 2 {
		t.Errorf("应有 2 位说话人, got %d", len(m.Speakers))
	}
}

// planChunks：按 chunkSec 计算切点数（纯计算，不切文件）。
func TestPlanChunks(t *testing.T) {
	if n := len(planChunks(25*60*1000, 10*60)); n != 3 {
		t.Errorf("25min/10min 应 3 块, got %d", n)
	}
	if n := len(planChunks(8*60*1000, 10*60)); n != 1 {
		t.Errorf("8min 应 1 块, got %d", n)
	}
}
