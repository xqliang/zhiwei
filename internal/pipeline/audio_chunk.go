package pipeline

import (
	"strings"

	"zhiwei/internal/provider"
)

// chunkPlan 是一个待分析块的时间范围（毫秒）。
type chunkPlan struct{ StartMS, EndMS int64 }

// planChunks 把总时长按 chunkSec 切成若干块（固定切点；静音微调/重叠在 stage 切片时用 ffmpeg 处理）。
// 返回每块的粗略时间范围（供 stage 决定 ffmpeg -ss/-t 与静音搜索窗口）。
func planChunks(totalMS int64, chunkSec int) []chunkPlan {
	step := int64(chunkSec) * 1000
	if totalMS <= step {
		return []chunkPlan{{0, totalMS}}
	}
	var plans []chunkPlan
	for start := int64(0); start < totalMS; start += step {
		end := start + step
		if end > totalMS {
			end = totalMS
		}
		plans = append(plans, chunkPlan{start, end})
	}
	return plans
}

// mergeInsights 合并多块结果（spec §5）：会话级 scene/weather/mood 取众数（并列取先出现），
// background_sounds 并集去重；每说话人按 label 聚合、emotion/mental_state 取最高置信块、
// micro_emotion 并集去重、confidence 取均值。单块直接返回。
func mergeInsights(chunks []provider.AudioInsight) provider.AudioInsight {
	if len(chunks) == 0 {
		return provider.AudioInsight{}
	}
	if len(chunks) == 1 {
		return chunks[0]
	}
	out := provider.AudioInsight{
		AcousticScene: modeStr(pluckStr(chunks, func(a provider.AudioInsight) string { return a.AcousticScene })),
		WeatherCues:   modeStr(pluckStr(chunks, func(a provider.AudioInsight) string { return a.WeatherCues })),
		OverallMood:   modeStr(pluckStr(chunks, func(a provider.AudioInsight) string { return a.OverallMood })),
	}
	// background_sounds 并集去重（保序）
	seen := map[string]bool{}
	for _, c := range chunks {
		for _, b := range c.BackgroundSounds {
			if b != "" && b != "无" && !seen[b] {
				seen[b] = true
				out.BackgroundSounds = append(out.BackgroundSounds, b)
			}
		}
	}
	// 每说话人聚合
	type agg struct {
		best   provider.SpeakerInsight
		micros []string
		sum    float64
		n      int
	}
	byLabel := map[string]*agg{}
	var order []string
	for _, c := range chunks {
		for _, s := range c.Speakers {
			a := byLabel[s.Label]
			if a == nil {
				a = &agg{}
				byLabel[s.Label] = a
				order = append(order, s.Label)
			}
			if s.Confidence >= a.best.Confidence {
				a.best = s // 最高置信块的 emotion/mental_state
			}
			if s.MicroEmotion != "" {
				a.micros = appendUniqStr(a.micros, s.MicroEmotion)
			}
			a.sum += s.Confidence
			a.n++
		}
	}
	for _, label := range order {
		a := byLabel[label]
		conf := 0.0
		if a.n > 0 {
			conf = a.sum / float64(a.n)
		}
		out.Speakers = append(out.Speakers, provider.SpeakerInsight{
			Label: label, Emotion: a.best.Emotion, MentalState: a.best.MentalState,
			MicroEmotion: strings.Join(a.micros, "/"), Confidence: conf,
		})
	}
	return out
}

func pluckStr(cs []provider.AudioInsight, f func(provider.AudioInsight) string) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		if v := f(c); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// modeStr 取众数（并列取先出现者）；全空返回 ""。
func modeStr(xs []string) string {
	cnt := map[string]int{}
	best, bestN := "", 0
	for _, x := range xs {
		cnt[x]++
		if cnt[x] > bestN {
			best, bestN = x, cnt[x]
		}
	}
	return best
}

func appendUniqStr(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}
