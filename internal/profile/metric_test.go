package profile

import "testing"

// TestMetricCatalog 锁定指标目录：命中/不命中判定，以及数值型/类别型的 Numeric 标志。
func TestMetricCatalog(t *testing.T) {
	// 命中：目录内键
	for _, k := range []string{"emotion", "weight", "sleep", "mood_energy", "diet", "health"} {
		if !ValidMetricKey(k) {
			t.Errorf("目录内键应命中: %s", k)
		}
	}
	// 不命中：目录外键
	for _, k := range []string{"", "bogus", "blood_pressure", "Weight"} {
		if ValidMetricKey(k) {
			t.Errorf("目录外键不应命中: %q", k)
		}
	}

	// 数值型：weight.Numeric=true，带单位 kg
	w := MetricDefOf("weight")
	if !w.Numeric || w.Unit != "kg" || w.Label != "体重" {
		t.Fatalf("weight 定义错误: %+v", w)
	}
	// 类别型：diet.Numeric=false，无单位
	d := MetricDefOf("diet")
	if d.Numeric || d.Unit != "" || d.Label != "饮食" {
		t.Fatalf("diet 定义错误: %+v", d)
	}
	// 无量纲数值型：emotion 数值(valence)但无单位
	e := MetricDefOf("emotion")
	if !e.Numeric || e.Unit != "" {
		t.Fatalf("emotion 定义错误: %+v", e)
	}
	// 未命中键返回零值 MetricDef（Numeric=false、空串），不 panic
	if z := MetricDefOf("bogus"); z.Key != "" || z.Numeric {
		t.Fatalf("未知键应返回零值 MetricDef: %+v", z)
	}
}

// TestDecideMetric 锁定 metric 闸门：只 active/pending（无冲突/现值分支），
// 且沿用 observed/inferred 自动写入白名单 + >=阈值语义。
func TestDecideMetric(t *testing.T) {
	cfg := GateConfig{AutoConf: 0.75}
	// 高置信 observed → active
	if s := DecideMetric(0.9, "observed", cfg); s != "active" {
		t.Fatalf("高置信 observed 应 active: %s", s)
	}
	// 低置信 → pending
	if s := DecideMetric(0.6, "observed", cfg); s != "pending" {
		t.Fatalf("低置信应 pending: %s", s)
	}
	// 高置信但 suggested → pending（不在自动写入白名单）
	if s := DecideMetric(0.9, "suggested", cfg); s != "pending" {
		t.Fatalf("suggested 应 pending: %s", s)
	}
	// 高置信 inferred → active（与 observed 并列白名单）
	if s := DecideMetric(0.9, "inferred", cfg); s != "active" {
		t.Fatalf("inferred 高置信应 active: %s", s)
	}
	// 高置信 predicted → pending
	if s := DecideMetric(0.9, "predicted", cfg); s != "pending" {
		t.Fatalf("predicted 应 pending: %s", s)
	}
	// 边界：恰好等于阈值 → active（>= 语义）
	if s := DecideMetric(0.75, "observed", cfg); s != "active" {
		t.Fatalf("confidence==阈值应 active: %s", s)
	}
	// 阈值兜底：AutoConf<=0 用默认 0.75
	if s := DecideMetric(0.9, "observed", GateConfig{}); s != "active" {
		t.Fatalf("默认阈值 0.75，0.9 应 active: %s", s)
	}
}

// TestParseFactsMetric 锁定 metric 事实解析：合法条目通过、字段正确；非法 key /
// Numeric 键缺 value_num / 类别键缺 value_text 的条目被丢弃（宁少勿错）。
func TestParseFactsMetric(t *testing.T) {
	raw := `{"facts":[
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"emotion","value_num":-0.6,"value_text":" 焦虑 ","confidence":0.8,"block_index":1},
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"weight","value_num":70,"unit":" kg ","measured_at":" 2026-08-01 ","confidence":0.9},
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"diet","value_text":"火锅","confidence":0.7},
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"bogus_key","value_num":1,"confidence":0.9},
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"weight","unit":"kg","confidence":0.9},
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"diet","confidence":0.9}
	]}`
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 保留前 3 条；后 3 条分别因 非法key / Numeric缺value_num / 类别缺value_text 被丢弃。
	if len(facts) != 3 {
		t.Fatalf("应保留 3 条 metric 事实: %d (%+v)", len(facts), facts)
	}
	// ① emotion：数值 + 文本兼有；value_text 前后空格被 trim
	e := facts[0]
	if e.Plane != "metric" || e.MetricKey != "emotion" || e.ValueNum == nil || *e.ValueNum != -0.6 ||
		e.MetricValueText != "焦虑" {
		t.Fatalf("emotion 解析错误: %+v", e)
	}
	// ② weight：unit / measured_at 前后空格被 trim
	w := facts[1]
	if w.MetricKey != "weight" || w.ValueNum == nil || *w.ValueNum != 70 || w.Unit != "kg" || w.MeasuredAt != "2026-08-01" {
		t.Fatalf("weight 解析错误: %+v", w)
	}
	// ③ diet：类别型，仅 value_text，value_num 为 nil
	d := facts[2]
	if d.MetricKey != "diet" || d.ValueNum != nil || d.MetricValueText != "火锅" {
		t.Fatalf("diet 解析错误: %+v", d)
	}
}
