package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestReextractDedupSkipBug 复现「重新提取失效」bug（TDD 红灯，修复 dedup 语义后转绿）：
//
// 背景：用户改了 ASR 后点「重新提取」（同一 session 重跑 profile）。memory 走「删旧重插」
// 所以能反映新 ASR；但 pet/cycle 走「session 级自然键去重」——同 session 同自然键命中即
// DecisionSkip，绕过了「有变化则更新」逻辑，于是出现「记忆改成母猫了、宠物还是公猫」的不一致。
//
// 真实数据：session 2093242790510071808，ASR「他是母的」，pet 泡泡首次被抽成公；
// 重新提取 job 的 profile trace 为「facts=1 … 跳过=1」，pet 纹丝不动。
//
// 本测试断言：重新提取（同 session）payload 有变化时应能更新。当前代码会 skip，
// 故断言失败（红）。修复后：pet 更新为母（高置信自动 merge），cycle 产生 pending 更新行。
func TestReextractDedupSkipBug(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_pet WHERE person_id = ?`, oid.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_cycle WHERE person_id = ?`, oid.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ?`, oid.Int64())
	})

	// ===== pet：重新提取修正性别（公→母）应更新 active =====
	sessPet := ids.New()
	petFact := func(gender string) Fact {
		return Fact{Plane: "pet", Subject: Subject{Kind: "self"}, PetName: "泡泡", Species: "猫",
			Gender: gender, AgeText: "2026年5月出生",
			Confidence: 0.95, EpistemicType: "observed", SegmentIDs: []ids.ID{1}}
	}
	// 首次提取：ASR 未识别出「母」，LLM 默认填「公」→ active 公。
	if _, err := svc.ApplyFacts(ctx, sessPet, 1, []Fact{petFact("公")}); err != nil {
		t.Fatal(err)
	}
	// 用户改 ASR 补上「母」→ 重新提取（同一 session），期望 active 更新为「母」。
	if _, err := svc.ApplyFacts(ctx, sessPet, 1, []Fact{petFact("母")}); err != nil {
		t.Fatal(err)
	}
	act, err := svc.Pets.FindActiveByNameExt(ctx, svc.DB, oid, "泡泡")
	if err != nil {
		t.Fatal(err)
	}
	if act == nil || act.Gender == nil || *act.Gender != "母" {
		t.Fatalf("[pet] 重新提取应把性别更新为母（当前被 session dedup skip 卡在公）: act=%+v", act)
	}
	// 幂等守护：同 session 再次传入与现值一致的「母」应 skip，绝不重复落库（修复不得破坏幂等）。
	stIdem, err := svc.ApplyFacts(ctx, sessPet, 1, []Fact{petFact("母")})
	if err != nil {
		t.Fatal(err)
	}
	if stIdem.Skipped != 1 {
		t.Fatalf("[pet] 无变化重跑应 skip（幂等回归守护）: %+v", stIdem)
	}

	// ===== cycle：重新提取改剂量应产生 pending 更新行（敏感数据保守：不静默覆盖 active）=====
	sessCycle := ids.New()
	cycleFact := func(dosage string) Fact {
		return Fact{Plane: "cycle", Subject: Subject{Kind: "self"},
			CycleType: "medication", CycleLabel: "降压药",
			Dosage: dosage, Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{2}}
	}
	// 首次提取：每日一片 → active。
	if _, err := svc.ApplyFacts(ctx, sessCycle, 1, []Fact{cycleFact("每日一片")}); err != nil {
		t.Fatal(err)
	}
	// 重新提取（同一 session）改剂量「每日两片」→ 期望产生 pending 更新行（supersedes 现值）。
	stCyc, err := svc.ApplyFacts(ctx, sessCycle, 1, []Fact{cycleFact("每日两片")})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Cycles.ListByPerson(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	var pend *repo.PersonCycle
	for i := range rows {
		if rows[i].Status == "pending" {
			pend = &rows[i]
		}
	}
	if pend == nil || pend.Dosage == nil || *pend.Dosage != "每日两片" {
		t.Fatalf("[cycle] 重新提取改剂量应产生 pending 更新行（当前被 skip）: st=%+v rows=%+v", stCyc, rows)
	}
	if pend.SupersedesID == nil {
		t.Fatalf("[cycle] pending 更新行应 supersedes 现值: %+v", pend)
	}
}
