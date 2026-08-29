package provider

import "testing"

func TestParseAudioInsightJSON(t *testing.T) {
	// 裹 ```json 代码块也要能解析
	raw := "```json\n{\"acoustic_scene\":\"室内\",\"background_sounds\":[\"键盘\"],\"weather_cues\":\"无\",\"overall_mood\":\"专注\",\"speakers\":[{\"label\":\"1\",\"emotion\":\"平静\",\"micro_emotion\":\"专注\",\"mental_state\":\"投入\",\"confidence\":0.8}]}\n```"
	ins, err := parseAudioInsight(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ins.AcousticScene != "室内" || len(ins.Speakers) != 1 || ins.Speakers[0].Emotion != "平静" {
		t.Errorf("解析异常: %+v", ins)
	}
}

// 无代码块裸 JSON 也要能解析（覆盖常规路径）。
func TestParseAudioInsightBare(t *testing.T) {
	raw := `{"acoustic_scene":"车内","speakers":[]}`
	ins, err := parseAudioInsight(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ins.AcousticScene != "车内" {
		t.Errorf("scene=%q", ins.AcousticScene)
	}
}
