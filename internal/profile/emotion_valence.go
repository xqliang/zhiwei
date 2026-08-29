package profile

import "strings"

// EmotionValence 类别情绪 → 效价 −1..1（PersonMetric.emotion 的 value_num，spec §4）。
// 覆盖 audioscene 常见输出；值为经验设定，未收录回落 0（中性），不报错。
var EmotionValence = map[string]float64{
	"喜悦": 0.8, "开心": 0.8, "兴奋": 0.9, "满足": 0.6, "平静": 0.2, "中性": 0.0,
	"疲惫": -0.4, "焦虑": -0.6, "紧张": -0.5, "愤怒": -0.9, "悲伤": -0.8,
	"沮丧": -0.7, "无聊": -0.2, "困惑": -0.3,
}

// EmotionToValence 类别情绪 → 效价；去首尾空格，未收录回落 0（中性）。
func EmotionToValence(emotion string) float64 {
	if v, ok := EmotionValence[strings.TrimSpace(emotion)]; ok {
		return v
	}
	return 0.0
}
