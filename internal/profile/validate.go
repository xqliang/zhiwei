package profile

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidAttrValue 是属性值写入端校验失败的哨兵错误（F4）。
//
// 用途：手动路径（ManualAddAttribute）把 NormalizeAttrValue 的错误原样 return，
// API handler 用 errors.Is(err, ErrInvalidAttrValue) 命中后回 400（而非默认 500）——
// 校验错误的语义是「用户/上游给的值不对，请改」，属于客户端错误，理应 400；只有真正的
// DB / 事务故障才 500。这与既有 metric 枚举校验（AddMetric handler 里 ValidMetricKeys
// 不命中直接回 400）保持同一状态码口径。
//
// LLM 路径不消费这个哨兵——它对任何校验错误一律 skip（宁少勿错），不区分错误种类。
var ErrInvalidAttrValue = errors.New("属性值非法")

// NormalizeAttrValue 按 catalog 的 ValueType 校验并规范化属性值（F4：写入端单点闸）。
//
// 背景：catalog 的 ValueType/EnumOptions 此前对写入端纯建议性——LLM 出 gender=「男性」、
// smokes=「是」、birthday=「八月三号」都会原样落库，数据脏了以后按 enum/bool 前端渲染与
// 查询就会失配。本函数是「值域正确性」的唯一落库前闸门，LLM 路径与手动路径共用它，保证
// 「什么值合法、怎么归一」只定义一处。
//
// 返回：规范化后的值（合法时）；不合法时返回 "" 与包裹了 ErrInvalidAttrValue 的错误
// （错误信息带属性 key、期望形态与实际值，直接给用户看）。
//
// 各类型规则（对应 AttrDef.ValueType）：
//
//	enum  ：TrimSpace 后须**精确命中** EnumOptions 之一。不做任何模糊/同义映射——
//	        gender=「男性」直接报错，绝不猜它是「男」（猜错落库比不落更糟，宁少勿错）。
//	bool  ：只接受 true/false（大小写不敏感，归一为小写 "true"/"false"）。「是/否」
//	        「yes/no」「1/0」一律**不接受**——中文口语该由 LLM 在抽取阶段就转成 true/false，
//	        落库层不承担语义映射；接受歧义写法会让 bool 列混入不可判定值。
//	date  ：经 parseEventAt 可解析（支持 YYYY-MM-DD / YYYY/MM/DD / YYYY-MM / 带时刻或
//	        时区的 ISO 等），再重排为规范 YYYY-MM-DD。「八月三号」这类自然语言无法解析→报错。
//	        复用 parseEventAt 而非另写解析器：日期解析与时区归一逻辑单点，避免与事件平面漂移。
//	number：strconv.ParseFloat 可解析即合法，归一为 %g 形态（复用 formatMetricValue，与
//	        metric 的 value_text 同一格式化点，防 "72.50"/"72.5" 这类等价写法漂移）。
//	text  ：仅 TrimSpace 透传，不做任何值域校验。
//
// catalog 外的 key（Def 回退为 text/single/「其他」组）走 default 分支，等同 text——
// 自造 key 的值域未知，任何格式校验都可能误伤（比如用户自定义一个 key 存日期字符串却
// 被当 text 也无妨），故一律不校验。
func NormalizeAttrValue(d AttrDef, value string) (string, error) {
	v := strings.TrimSpace(value)
	switch d.ValueType {
	case ValueTypeEnum:
		// 精确命中 EnumOptions 之一才放行；否则报错（不猜映射）。
		for _, opt := range d.EnumOptions {
			if v == opt {
				return v, nil
			}
		}
		return "", fmt.Errorf("%w：属性「%s」期望枚举值之一 %v，得到「%s」",
			ErrInvalidAttrValue, d.Key, d.EnumOptions, value)
	case ValueTypeBool:
		// 只认 true/false（大小写不敏感），归一为小写。
		switch strings.ToLower(v) {
		case "true":
			return "true", nil
		case "false":
			return "false", nil
		}
		return "", fmt.Errorf("%w：属性「%s」期望布尔值 true/false，得到「%s」",
			ErrInvalidAttrValue, d.Key, value)
	case ValueTypeDate:
		// 经 parseEventAt 解析成功后重排为 YYYY-MM-DD（月份精度如 2026-08 会补成 2026-08-01）。
		if t, ok := parseEventAt(v); ok {
			return t.Format("2006-01-02"), nil
		}
		return "", fmt.Errorf("%w：属性「%s」期望日期（YYYY-MM-DD），得到「%s」",
			ErrInvalidAttrValue, d.Key, value)
	case ValueTypeNumber:
		// ParseFloat 成功即合法，归一为 %g（与 formatMetricValue 同法）。
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return formatMetricValue(n), nil
		}
		return "", fmt.Errorf("%w：属性「%s」期望数值，得到「%s」",
			ErrInvalidAttrValue, d.Key, value)
	default:
		// text 及 catalog 外 key：仅 trim 透传，不校验值域。
		return v, nil
	}
}
