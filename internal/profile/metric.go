package profile

// metric.go 是画像第 5 平面（person_metric，时间序列个人指标）的指标目录（catalog）。
// 仿属性目录（catalog.go）/ 事件类型枚举（ValidEventTypes）的做法，把「合法指标键」
// 与其展示元数据集中在一处，供抽取校验（fact.go）、落库（service.go）、手动 CRUD
// （service_manual.go）统一取用。

// MetricDef 描述一个画像指标键（仿属性目录的 AttrDef，但为时序测点精简）。
type MetricDef struct {
	Key     string // 指标键（英文 snake_case，落库 metric_key）
	Label   string // 中文名（表单/展示用）
	Unit    string // 单位，无量纲则 ""（如情绪/精力）
	Numeric bool   // true=数值指标（要求 value_num，曲线可画）；false=类别指标（要求 value_text）
}

// MetricCatalog 是合法指标键全集（spec §4.5）。
//
// 字段顺序即 MetricDef{Key, Label, Unit, Numeric}：
//   - Numeric=true 的键必须带 value_num（校验保证），曲线才能画（硬约束 6）；
//   - Numeric=false 的键为类别描述，落 value_text（如饮食='火锅'、健康='感冒'）。
var MetricCatalog = map[string]MetricDef{
	"emotion":     {"emotion", "情绪", "", true},     // value_num 为情绪效价 valence，取值 −1..1
	"weight":      {"weight", "体重", "kg", true},    // value_num kg
	"sleep":       {"sleep", "睡眠时长", "h", true},    // value_num 小时
	"mood_energy": {"mood_energy", "精力", "", true}, // value_num 0..1
	"diet":        {"diet", "饮食", "", false},       // value_text 类别（火锅/清淡…）
	"health":      {"health", "健康", "", false},     // value_text 类别（感冒/头痛…）
	// 人体测量（均数值型，value_num 为测量读数；单位见 Unit）
	"height":   {"height", "身高", "cm", true},   // value_num 厘米
	"waist":    {"waist", "腰围", "cm", true},    // value_num 厘米
	"chest":    {"chest", "胸围", "cm", true},    // value_num 厘米
	"hip":      {"hip", "臀围", "cm", true},      // value_num 厘米
	"body_fat": {"body_fat", "体脂率", "%", true}, // value_num 百分比
}

// ValidMetricKey 判定指标键是否在目录内（抽取校验用；目录外的键一律丢弃——
// 与属性目录「未知 key 归其他组仍可用」不同：时序曲线的键必须收敛，故 metric 只收目录内键）。
func ValidMetricKey(k string) bool { _, ok := MetricCatalog[k]; return ok }

// MetricDefOf 取指标定义（调用方应先经 ValidMetricKey 确认命中；未命中返回零值 MetricDef）。
func MetricDefOf(k string) MetricDef { return MetricCatalog[k] }
