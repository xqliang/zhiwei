package profile

import (
	"context"
	"errors"
	"testing"

	"zhiwei/internal/ids"
)

// TestNormalizeAttrValue 全类型矩阵（F4 写入端校验/规范化单点闸）：合法值归一、非法值报错。
// 纯函数测试，不依赖 DB（与 gate_test.go 同风格）。
func TestNormalizeAttrValue(t *testing.T) {
	// number 型 catalog 里暂无对应 key，直接构造 AttrDef 覆盖（NormalizeAttrValue 只看 ValueType）。
	numDef := AttrDef{Key: "test_num", ValueType: ValueTypeNumber}
	// catalog 外 key：Def 回退 text，走 default 分支（仅 trim 透传，不校验值域）。
	unknownDef := Def("my_custom_key_不在目录里")

	cases := []struct {
		name    string
		d       AttrDef
		in      string
		want    string // 期望规范化结果（wantErr=false 时校验）
		wantErr bool
	}{
		// ---- enum（gender: 男/女/其他）：精确命中，不猜映射 ----
		{"enum 精确命中", Def("gender"), "男", "男", false},
		{"enum 带空格 trim 后命中", Def("gender"), "  男  ", "男", false},
		{"enum 语义近似不接受", Def("gender"), "男性", "", true},
		{"enum 空串", Def("gender"), "", "", true},
		{"enum 英文别名不接受", Def("gender"), "male", "", true},
		{"enum 另一目录 mbti 命中", Def("mbti"), "INTJ", "INTJ", false},
		{"enum mbti 小写不命中(大小写敏感)", Def("mbti"), "intj", "", true},

		// ---- bool（smokes）：只认 true/false，大小写不敏感，归一小写 ----
		{"bool true", Def("smokes"), "true", "true", false},
		{"bool True 归一小写", Def("smokes"), "True", "true", false},
		{"bool FALSE 归一小写", Def("smokes"), "FALSE", "false", false},
		{"bool 带空格", Def("smokes"), "  true  ", "true", false},
		{"bool 中文是不接受", Def("smokes"), "是", "", true},
		{"bool 1 不接受", Def("smokes"), "1", "", true},
		{"bool 空串", Def("smokes"), "", "", true},

		// ---- date（birthday）：parseEventAt 解析后重排 YYYY-MM-DD ----
		{"date 标准格式", Def("birthday"), "2026-08-03", "2026-08-03", false},
		{"date 斜杠格式重排", Def("birthday"), "2026/08/03", "2026-08-03", false},
		{"date 月份精度补日", Def("birthday"), "2026-08", "2026-08-01", false},
		{"date 带时刻时区取日历日", Def("birthday"), "2026-08-03T05:00:00+08:00", "2026-08-03", false},
		{"date 带空格", Def("birthday"), "  2026-08-03  ", "2026-08-03", false},
		{"date 自然语言报错", Def("birthday"), "八月三号", "", true},
		{"date 非日期报错", Def("birthday"), "not-a-date", "", true},
		{"date 空串", Def("birthday"), "", "", true},

		// ---- number：ParseFloat 可解析，归一 %g ----
		{"number 整数", numDef, "70", "70", false},
		{"number 小数", numDef, "72.5", "72.5", false},
		{"number 尾零归一", numDef, "72.50", "72.5", false},
		{"number 科学计数归一", numDef, "1e3", "1000", false},
		{"number 带空格", numDef, "  72.5  ", "72.5", false},
		{"number 负数", numDef, "-3.14", "-3.14", false},
		{"number 非数值报错", numDef, "abc", "", true},
		{"number 空串报错", numDef, "", "", true},

		// ---- text（occupation）：仅 trim 透传，不校验 ----
		{"text 透传", Def("occupation"), "工程师", "工程师", false},
		{"text trim", Def("occupation"), "  工程师  ", "工程师", false},
		{"text 空串透传不报错", Def("occupation"), "", "", false},

		// ---- catalog 外 key（回退 text）：不校验值域，日期样值也当文本透传 ----
		{"未知 key 文本透传", unknownDef, "随便什么值", "随便什么值", false},
		{"未知 key 日期样值不校验", unknownDef, "八月三号", "八月三号", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeAttrValue(c.d, c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 got=%q err=nil", got)
				}
				// 非法值必须包裹 ErrInvalidAttrValue 哨兵（API 层靠它回 400）。
				if !errors.Is(err, ErrInvalidAttrValue) {
					t.Fatalf("错误应包裹 ErrInvalidAttrValue: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != c.want {
				t.Fatalf("规范化结果错误: got=%q want=%q", got, c.want)
			}
		})
	}
}

// TestApplyAttributeFactF4 集成（LLM 路径）：脏值 skip 不落库；规范化后的值贯穿闸门并落库。
func TestApplyAttributeFactF4(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	// 收尾清掉本用例写到 owner 的属性与审计，恢复干净基线（模式参照 TestApplyFactsGatePaths）。
	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			pk := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ? AND attr_key IN ('gender','smokes','birthday','eats_spicy')`, pk)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind='attribute' AND attr_key IN ('gender','smokes','birthday','eats_spicy')`, pk)
		}
	})

	sess := ids.New()
	facts := []Fact{
		// 脏枚举：gender=「男性」（非目录取值）→ 校验失败 skip
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "gender",
			Value: "男性", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// 脏布尔：smokes=「是」→ skip
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "smokes",
			Value: "是", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// 合法但需重排：birthday=「2026/08/03」→ 归一「2026-08-03」落 active
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "birthday",
			Value: "2026/08/03", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// 合法布尔：eats_spicy=「True」→ 归一「true」落 active
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "eats_spicy",
			Value: "True", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	// 2 脏值 skip、2 合法 active
	if st.Active != 2 || st.Skipped != 2 || st.Pending != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	// 脏值未落库
	if a, _ := svc.Attributes.FindActiveByKey(ctx, oid, "gender"); a != nil {
		t.Fatalf("脏枚举 gender 不应落库: %+v", a)
	}
	if a, _ := svc.Attributes.FindActiveByKey(ctx, oid, "smokes"); a != nil {
		t.Fatalf("脏布尔 smokes 不应落库: %+v", a)
	}
	// 规范化后的值落库（birthday 重排、eats_spicy 归一小写）
	if a, _ := svc.Attributes.FindActiveByKey(ctx, oid, "birthday"); a == nil || a.ValueText != "2026-08-03" {
		t.Fatalf("birthday 应归一为 2026-08-03: %+v", a)
	}
	if a, _ := svc.Attributes.FindActiveByKey(ctx, oid, "eats_spicy"); a == nil || a.ValueText != "true" {
		t.Fatalf("eats_spicy 应归一为 true: %+v", a)
	}
}

// TestManualAddAttributeF4 集成（手动路径）：脏值报错（哨兵可命中，API 层据此回 400）；
// 合法值正常落库且值被规范化。
func TestManualAddAttributeF4(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			pk := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ? AND attr_key IN ('gender','birthday')`, pk)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind='attribute' AND attr_key IN ('gender','birthday')`, pk)
		}
	})

	// 脏枚举 → 报错（哨兵命中），且不落库
	_, err := svc.ManualAddAttribute(ctx, oid, "gender", "男性")
	if err == nil || !errors.Is(err, ErrInvalidAttrValue) {
		t.Fatalf("脏枚举应报 ErrInvalidAttrValue: %v", err)
	}
	if a, _ := svc.Attributes.FindActiveByKey(ctx, oid, "gender"); a != nil {
		t.Fatalf("脏值报错后不应落库: %+v", a)
	}

	// 脏日期 → 报错
	if _, err := svc.ManualAddAttribute(ctx, oid, "birthday", "八月三号"); err == nil || !errors.Is(err, ErrInvalidAttrValue) {
		t.Fatalf("脏日期应报 ErrInvalidAttrValue: %v", err)
	}

	// 合法枚举 → 落库
	row, err := svc.ManualAddAttribute(ctx, oid, "gender", "男")
	if err != nil {
		t.Fatalf("合法枚举不应报错: %v", err)
	}
	if row == nil || row.ValueText != "男" || row.Status != "active" {
		t.Fatalf("合法枚举落库错误: %+v", row)
	}

	// 合法但需重排的日期 → 落库时已规范化
	row2, err := svc.ManualAddAttribute(ctx, oid, "birthday", "2026/08/03")
	if err != nil {
		t.Fatalf("合法日期不应报错: %v", err)
	}
	if row2 == nil || row2.ValueText != "2026-08-03" {
		t.Fatalf("日期应规范化为 2026-08-03: %+v", row2)
	}
}
