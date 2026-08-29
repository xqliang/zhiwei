package profile

import (
	"math"
	"testing"
)

func TestEmotionToValence(t *testing.T) {
	// 正价情绪 > 0
	if v := EmotionToValence("喜悦"); v <= 0 { t.Errorf("喜悦 应 >0, got %v", v) }
	if v := EmotionToValence(" 开心 "); v <= 0 { t.Errorf("去空格后 开心 应 >0, got %v", v) }
	// 负价情绪 < 0
	if v := EmotionToValence("愤怒"); v >= 0 { t.Errorf("愤怒 应 <0, got %v", v) }
	if v := EmotionToValence("焦虑"); v >= 0 { t.Errorf("焦虑 应 <0, got %v", v) }
	// 中性
	if v := EmotionToValence("平静"); math.Abs(v) > 0.5 { t.Errorf("平静 应接近中性, got %v", v) }
	// 未收录 → 0（中性回落）
	if v := EmotionToValence("某种未知情绪"); v != 0 { t.Errorf("未收录应回落 0, got %v", v) }
	if v := EmotionToValence(""); v != 0 { t.Errorf("空应回落 0, got %v", v) }
}
