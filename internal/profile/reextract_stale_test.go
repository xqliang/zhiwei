package profile

import (
	"context"
	"strings"
	"testing"

	"zhiwei/internal/ids"
)

// TestReextractRemovesStaleProfileRows 复现「改 ASR 重新提取后，旧文本的画像产物残留」
// bug（2026-08-31 用户实录：ASR 把「划船」错识成「化妆」，改文本重提取后 activity
// 「化妆」行仍在，与「划船」并存）。TDD 红灯，ApplyFacts 补残留清理后转绿。
//
// 语义（方案 A''——精准删除，兼容既有 refine 语义）：
//   - 同 session 重跑时，落新事实后，**未被新事实命中/复用**的本 session 旧行
//     （= 旧文本才有的「已消失事实」）连同其 change_log 一并删除；
//   - 同键同值 skip / 同键变值 refine（reextract_dedup_test 的契约）不受影响。
//
// 直接走 ApplyFacts（不经过 ExtractSession/LLM），聚焦落库语义。
func TestReextractRemovesStaleProfileRows(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)
	sess := ids.New()

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_activity WHERE person_id = ?`, oid.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ?`, oid.Int64())
	})

	mkActivity := func(name string) Fact {
		return Fact{Plane: "activity", Subject: Subject{Kind: "self"},
			ActivityText: name, Confidence: 0.95, EpistemicType: "observed", SegmentIDs: []ids.ID{1}}
	}

	// 第一次提取（错误文本）：产出 activity「化妆」+「写代码」两条 active。
	if _, err := svc.ApplyFacts(ctx, sess, 1, []Fact{mkActivity("化妆"), mkActivity("写代码")}); err != nil {
		t.Fatal(err)
	}
	acts, err := svc.Activities.ListByPerson(ctx, oid, nil, nil)
	if err != nil || len(acts) != 2 {
		t.Fatalf("首次提取应 2 条 activity: %v %+v", err, acts)
	}

	// 用户修正 ASR「化妆→划船」后重新提取（同 session）：新事实只有「划船」「写代码」
	//（「化妆」在新文本下已不存在）。「写代码」同键同值 → skip 复用旧行；「划船」新键 → 新建；
	//「化妆」无人命中 → 应作为残留被删除。
	st, err := svc.ApplyFacts(ctx, sess, 1, []Fact{mkActivity("划船"), mkActivity("写代码")})
	if err != nil {
		t.Fatal(err)
	}
	_ = st
	acts2, err := svc.Activities.ListByPerson(ctx, oid, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, a := range acts2 {
		names[a.Activity] = true
	}
	if len(acts2) != 2 || !names["划船"] || !names["写代码"] || names["化妆"] {
		t.Fatalf("重提取后应只剩 划船+写代码（化妆已消失、写代码复用旧行）: %v", names)
	}

	// change_log 同步：被删「化妆」行的 create 日志应一并删除（entity_id 关联级联），
	//「写代码」的日志保留（行复用未删）、「划船」的新日志在。
	logs, err := svc.ChangeLogs.ListByPerson(ctx, oid, "activity", "")
	if err != nil {
		t.Fatal(err)
	}
	logVals := map[string]int{}
	for _, l := range logs {
		if l.ChangeType == "create" && l.NewValue != nil {
			// snap=json.Marshal：字符串值带双引号（"划船"），剥掉再比对。
			v := strings.Trim(*l.NewValue, `"`)
			logVals[v]++ // activity 的 create 日志把活动名记在 new_value（见 createActivityLog）
		}
	}
	if logVals["化妆"] != 0 {
		t.Fatalf("被删行的 change_log 应级联清理: %+v", logVals)
	}
	if logVals["划船"] != 1 || logVals["写代码"] != 1 {
		t.Fatalf("划船新日志与写代码保留日志应在: %+v", logVals)
	}
}
