package profile

import "strings"

// 人物名校验/归一（LLM 抽取的兜底防线，2026-08-29）：
// prompt（profile_extraction_v4.md「人物名字规则」）已要求 LLM 只给单人名，但模型偶发
// 仍会输出「老保一家」这类口语粘连——这里做硬校验：剥离家庭/集体后缀、拒绝过长/代词名。
// 全链路：fact.trimSubject（TrimSpace）→ resolveOrCreateByName → NormalizePersonName。

// familySuffixes 家庭/集体词后缀：命中则剥离后取核心名（「老保一家」→「老保」）。
// 只收「X+Y」构词（Y 是集体量词），不收单字称谓（「张老师」「李医生」是正常称呼）。
var familySuffixes = []string{"一家人", "一家子", "一家", "全家", "两口子", "老两口"}

// pronounNames 代词/非人名：整体或剥离后缀后是这些 → 拒绝（「他们」/「他们全家」→「他们」）。
var pronounNames = map[string]bool{
	"他们": true, "她们": true, "大家": true, "有人": true, "所有人": true,
	"别人": true, "人家": true, "这俩": true, "那两个": true, "夫妻": true,
	"一对夫妻": true, "夫妇": true, "小两口": true,
}

// maxPersonNameRunes 人物名长度上限（中文名 2-4 字，含少数民族/外文音译留到 8）。
const maxPersonNameRunes = 8

// NormalizePersonName 归一/校验 LLM 给出的人物名：
//   - TrimSpace + 剥离家庭/集体后缀（「老保一家」→「老保」）
//   - 空名 / 剥离后为代词或非人名 / 超长（>8 rune）→ 返回 "" 表示拒绝（调用方不建人物）
//   - 其余原样返回（已 TrimSpace）
func NormalizePersonName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, suf := range familySuffixes {
		if strings.HasSuffix(name, suf) {
			name = strings.TrimSpace(strings.TrimSuffix(name, suf))
			break
		}
	}
	if name == "" || pronounNames[name] {
		return ""
	}
	if len([]rune(name)) > maxPersonNameRunes {
		return ""
	}
	return name
}
