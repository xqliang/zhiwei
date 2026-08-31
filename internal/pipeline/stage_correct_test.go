package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/entity"
	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// TestParseCorrectionEdits 解析 LLM 输出：正常/带围栏废话/空 edits/清洗/非法 JSON。
func TestParseCorrectionEdits(t *testing.T) {
	// 1) 正常输出。
	got, err := ParseCorrectionEdits(`{"edits":[{"orig":"常梦瑜","corrected":"张梦瑜","entity_id":"9527","confidence":0.9,"reason":"读音相近"}]}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 1 || got[0].Orig != "常梦瑜" || got[0].Corrected != "张梦瑜" || got[0].EntityID != "9527" {
		t.Fatalf("解析不符: %+v", got)
	}
	if got[0].Confidence != 0.9 {
		t.Fatalf("置信度不符: %v", got[0].Confidence)
	}

	// 2) 带代码围栏与前后废话（容错：截首{尾}，同 ParseNameCandidates）。
	got, err = ParseCorrectionEdits("好的，以下是纠错结果：\n```json\n{\"edits\":[]}\n```")
	if err != nil || len(got) != 0 {
		t.Fatalf("围栏容错失败: %v %+v", err, got)
	}

	// 3) 清洗：置信度越界 clamp、空 orig/corrected 丢弃。
	got, err = ParseCorrectionEdits(`{"edits":[{"orig":"","corrected":"张梦瑜","entity_id":"9527","confidence":1.5},{"orig":"阿黄","corrected":"阿皇","entity_id":"9528","confidence":0.8}]}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("空 orig 应丢弃: %+v", got)
	}
	if got[0].Confidence != 0.8 {
		t.Fatalf("置信度应保留 0.8: %+v", got)
	}
	// clamp 验证：单独一条越界的。
	got, err = ParseCorrectionEdits(`{"edits":[{"orig":"阿黄","corrected":"阿皇","entity_id":"9528","confidence":1.5}]}`)
	if err != nil || len(got) != 1 || got[0].Confidence != 1 {
		t.Fatalf("置信度应 clamp 到 1: %v %+v", err, got)
	}

	// 4) 彻底非法 JSON → error（调用方吞错跳过该段）。
	if _, err := ParseCorrectionEdits(`这不是JSON`); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// fakeCorrectLLM 可编程 LLM 桩：按序弹出预设响应；耗尽/err 返回错误。记录收到的 user 消息。
type fakeCorrectLLM struct {
	resps []string
	calls []string
	err   error
}

func (f *fakeCorrectLLM) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	f.calls = append(f.calls, req.User)
	if f.err != nil {
		return provider.ChatResponse{}, f.err
	}
	if len(f.resps) == 0 {
		return provider.ChatResponse{}, errors.New("no more canned responses")
	}
	r := f.resps[0]
	f.resps = f.resps[1:]
	return provider.ChatResponse{Content: r, TotalTokens: 88}, nil
}

// correctFixture 是 correct stage 集成测试的共享夹具（session + transcript + 2 段 + 1 实体）。
type correctFixture struct {
	db          *sqlx.DB
	sessions    *repo.SessionRepo
	transcripts *repo.TranscriptRepo
	entityKB    *repo.EntityKBRepo
	settings    *repo.EntitySettingsRepo
	sid         ids.ID
	tr          *repo.Transcript
	ent         *repo.Entity
}

// setupCorrectFixture 建：AudioSession(user1/completed) + Transcript + 2 段
// （seq1「我们出发吧」召回空；seq2「常梦瑜你看到我的邮件了吗」召回 张梦瑜）+
// 实体库一条 person 实体 张梦瑜（拼音 zhang meng yu）。settings 走默认（无行=启用+0.8）。
// 为测试隔离：seed 前清空 user1 的实体库与配置行，t.Cleanup 收尾再清。
// 注意 seq1 文本刻意选发音与 zhang-meng-yu 明显不同者——RecallCandidates 用 Jaro-Winkler
// 对短拼音串较宽松（如「明天开会」ming-tian-kai-hui 竟 0.65 越 0.6 下限而误召回），
// 「我们出发吧」实测召回为空，才能验证「无候选段不调 LLM」。
func setupCorrectFixture(t *testing.T) *correctFixture {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fx := &correctFixture{
		db:          db,
		sessions:    &repo.SessionRepo{DB: db},
		transcripts: &repo.TranscriptRepo{DB: db},
		entityKB:    &repo.EntityKBRepo{DB: db},
		settings:    &repo.EntitySettingsRepo{DB: db},
	}

	// 隔离：清 user1 的实体库与配置行（防上一轮/别用例残留污染召回与开关）。
	if _, err := db.ExecContext(ctx, `DELETE FROM entity_kb WHERE user_id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM entity_settings WHERE user_id = 1`); err != nil {
		t.Fatal(err)
	}

	fx.sid = ids.New()
	if err := fx.sessions.Create(ctx, &repo.AudioSession{
		ID: fx.sid, UserID: 1, Source: "web_upload", Filename: "mail.wav",
		StoragePath: "/tmp/mail.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	fx.tr = &repo.Transcript{SessionID: fx.sid, Language: "zh-CN"}
	if err := fx.transcripts.Create(ctx, fx.tr); err != nil {
		t.Fatal(err)
	}
	if err := fx.transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: fx.tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "我们出发吧", StartMS: 0, EndMS: 2000},
		{TranscriptID: fx.tr.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "常梦瑜你看到我的邮件了吗", StartMS: 2500, EndMS: 6000},
	}); err != nil {
		t.Fatal(err)
	}

	// 实体：person 张梦瑜，拼音键由 NormalizePinyin 算（= "zhang meng yu"）。
	py := entity.NormalizePinyin("张梦瑜")
	fx.ent = &repo.Entity{UserID: 1, Canonical: "张梦瑜", Kind: repo.EntityKindPerson, Pinyin: &py}
	if err := fx.entityKB.CreateManual(ctx, fx.ent); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.db.ExecContext(c, `DELETE FROM transcript_segment WHERE transcript_id = ?`, fx.tr.ID.Int64())
		_, _ = fx.db.ExecContext(c, `DELETE FROM transcript WHERE id = ?`, fx.tr.ID.Int64())
		_, _ = fx.db.ExecContext(c, `DELETE FROM audio_session WHERE id = ?`, fx.sid.Int64())
		_, _ = fx.db.ExecContext(c, `DELETE FROM entity_kb WHERE user_id = 1`)
		_, _ = fx.db.ExecContext(c, `DELETE FROM entity_settings WHERE user_id = 1`)
	})
	return fx
}

// newCorrectDeps 组 StageDeps（只填 correct stage 用到的字段）。
// EntitySeed 留零值 → RefreshAuto 因 KB=nil 直接 no-op（不动手动实体）。
func newCorrectDeps(fx *correctFixture, llm provider.LLMProvider) StageDeps {
	return StageDeps{
		Sessions: fx.sessions, Transcripts: fx.transcripts,
		EntityKB: fx.entityKB, EntitySettings: fx.settings,
		LLM: llm, LLMModel: "fake-model", CorrectPrompt: "测试纠错 prompt",
	}
}

// getSeg 按 sequence_no 取本 transcript 的段（读回校验用）。
func getSeg(t *testing.T, tr *repo.TranscriptRepo, trID ids.ID, seq int) repo.TranscriptSegment {
	t.Helper()
	segs, err := tr.ListSegments(context.Background(), trID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range segs {
		if s.SequenceNo == seq {
			return s
		}
	}
	t.Fatalf("未找到 seq=%d 的段", seq)
	return repo.TranscriptSegment{}
}

func TestStageCorrectHappyPath(t *testing.T) {
	fx := setupCorrectFixture(t)
	ctx := context.Background()
	// 首个响应：一条合法 edit（entity_id 用真实 id）。第二个响应用于验证幂等——
	// 第二轮不应再调 LLM（seg1 无召回、seg2 已 entity 纠正被跳过），故第二个响应永不弹出。
	llm := &fakeCorrectLLM{resps: []string{
		fmt.Sprintf(`{"edits":[{"orig":"常梦瑜","corrected":"张梦瑜","entity_id":"%s","confidence":0.92,"reason":"读音相近"}]}`, fx.ent.ID.String()),
		`{"edits":[{"orig":"张梦瑜","corrected":"张梦瑜","entity_id":"x","confidence":0.99}]}`,
	}}
	d := newCorrectDeps(fx, llm)
	j := &repo.Job{}

	if err := runCorrectStage(ctx, d, j, fx.sid); err != nil {
		t.Fatalf("stage: %v", err)
	}

	// 恰 1 次 LLM 调用（只有 seg2 有召回候选）。
	if len(llm.calls) != 1 {
		t.Fatalf("应恰好 1 次 LLM 调用（仅 seg2 有候选），实际 %d", len(llm.calls))
	}
	// user message 含白名单（canonical + entity id）+ 本段标记 + 段文本。
	user := llm.calls[0]
	for _, want := range []string{"张梦瑜", fx.ent.ID.String(), "本段", "常梦瑜你看到我的邮件了吗"} {
		if !strings.Contains(user, want) {
			t.Fatalf("user message 应含 %q，实际=\n%s", want, user)
		}
	}

	// seg2 已纠正。
	seg2 := getSeg(t, fx.transcripts, fx.tr.ID, 2)
	if seg2.Text != "张梦瑜你看到我的邮件了吗" {
		t.Fatalf("seg2 文本应被纠正，实际=%q", seg2.Text)
	}
	if seg2.CorrectedReason == nil || *seg2.CorrectedReason != "entity" {
		t.Fatalf("seg2 corrected_reason 应为 entity，实际=%v", seg2.CorrectedReason)
	}
	var edits []appliedEdit
	if err := json.Unmarshal(seg2.EntityEdits, &edits); err != nil {
		t.Fatalf("entity_edits 反序列化失败: %v (raw=%s)", err, seg2.EntityEdits)
	}
	if len(edits) != 1 || edits[0].Orig != "常梦瑜" || edits[0].Corrected != "张梦瑜" || edits[0].Confidence != 0.92 {
		t.Fatalf("entity_edits 明细不符: %+v", edits)
	}

	// seg1 未动。
	seg1 := getSeg(t, fx.transcripts, fx.tr.ID, 1)
	if seg1.CorrectedReason != nil {
		t.Fatalf("seg1 不应被纠正，corrected_reason=%v", seg1.CorrectedReason)
	}

	// full_text 已重算并含纠正后文本。
	tr, err := fx.transcripts.GetBySession(ctx, fx.sid)
	if err != nil {
		t.Fatal(err)
	}
	if tr.FullText == nil || !strings.Contains(*tr.FullText, "张梦瑜你看到我的邮件了吗") {
		t.Fatalf("full_text 应含纠正后文本，实际=%v", tr.FullText)
	}

	// trace 记了 correct:llm 条目。
	if j.Trace == nil || !strings.Contains(string(*j.Trace), "correct:llm") {
		t.Fatalf("job trace 应含 correct:llm 条目，实际=%v", j.Trace)
	}

	// 幂等：再跑一次 → 不再调 LLM（seg2 已 entity 跳过、seg1 无候选）。
	if err := runCorrectStage(ctx, d, j, fx.sid); err != nil {
		t.Fatalf("重跑 stage: %v", err)
	}
	if len(llm.calls) != 1 {
		t.Fatalf("幂等重跑不应新增 LLM 调用，实际累计 %d", len(llm.calls))
	}
}

func TestStageCorrectGate(t *testing.T) {
	// 每个子用例：门控应拦下 edit → seg2 文本/标记/明细均不变。
	cases := []struct {
		name   string
		resp   string
		llmErr error
		editFn func(entID string) string // 生成响应（需要真实 entity id 时）
	}{
		{name: "低置信度", editFn: func(id string) string {
			return fmt.Sprintf(`{"edits":[{"orig":"常梦瑜","corrected":"张梦瑜","entity_id":"%s","confidence":0.5}]}`, id)
		}},
		{name: "orig不在段内", editFn: func(id string) string {
			return fmt.Sprintf(`{"edits":[{"orig":"王大锤","corrected":"张梦瑜","entity_id":"%s","confidence":0.95}]}`, id)
		}},
		{name: "corrected不在白名单", editFn: func(id string) string {
			return fmt.Sprintf(`{"edits":[{"orig":"常梦瑜","corrected":"张梦宇","entity_id":"%s","confidence":0.95}]}`, id)
		}},
		{name: "entity_id不在白名单", editFn: func(id string) string {
			return `{"edits":[{"orig":"常梦瑜","corrected":"张梦瑜","entity_id":"999","confidence":0.95}]}`
		}},
		{name: "LLM失败不阻塞", llmErr: errors.New("模拟 LLM 超时")},
		{name: "解析失败不阻塞", resp: "这不是JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := setupCorrectFixture(t)
			ctx := context.Background()
			llm := &fakeCorrectLLM{err: c.llmErr}
			if c.editFn != nil {
				llm.resps = []string{c.editFn(fx.ent.ID.String())}
			} else if c.resp != "" {
				llm.resps = []string{c.resp}
			}
			d := newCorrectDeps(fx, llm)
			if err := runCorrectStage(ctx, d, &repo.Job{}, fx.sid); err != nil {
				t.Fatalf("门控用例应 best-effort 返回 nil，实际 err=%v", err)
			}
			seg2 := getSeg(t, fx.transcripts, fx.tr.ID, 2)
			if seg2.Text != "常梦瑜你看到我的邮件了吗" {
				t.Fatalf("门控拦截后 seg2 文本不应变，实际=%q", seg2.Text)
			}
			if seg2.CorrectedReason != nil {
				t.Fatalf("门控拦截后不应标记 corrected_reason，实际=%v", seg2.CorrectedReason)
			}
			if len(seg2.EntityEdits) != 0 {
				t.Fatalf("门控拦截后不应写 entity_edits，实际=%s", seg2.EntityEdits)
			}
		})
	}
}

func TestStageCorrectDisabled(t *testing.T) {
	fx := setupCorrectFixture(t)
	ctx := context.Background()
	// 关闭纠错开关。
	if err := fx.settings.Upsert(ctx, 1, false, 0.8, nil); err != nil {
		t.Fatal(err)
	}
	llm := &fakeCorrectLLM{resps: []string{
		fmt.Sprintf(`{"edits":[{"orig":"常梦瑜","corrected":"张梦瑜","entity_id":"%s","confidence":0.95}]}`, fx.ent.ID.String()),
	}}
	d := newCorrectDeps(fx, llm)
	if err := runCorrectStage(ctx, d, &repo.Job{}, fx.sid); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(llm.calls) != 0 {
		t.Fatalf("功能关闭时不应调 LLM，实际 %d 次", len(llm.calls))
	}
	seg2 := getSeg(t, fx.transcripts, fx.tr.ID, 2)
	if seg2.Text != "常梦瑜你看到我的邮件了吗" || seg2.CorrectedReason != nil {
		t.Fatalf("功能关闭时 seg2 不应变，text=%q reason=%v", seg2.Text, seg2.CorrectedReason)
	}
}

func TestStageCorrectNoDeps(t *testing.T) {
	fx := setupCorrectFixture(t)
	ctx := context.Background()
	llm := &fakeCorrectLLM{resps: []string{`{"edits":[]}`}}
	d := newCorrectDeps(fx, llm)
	d.EntityKB = nil // 依赖未装配 → no-op（兼容旧装配）。
	if err := runCorrectStage(ctx, d, &repo.Job{}, fx.sid); err != nil {
		t.Fatalf("依赖缺失应 no-op 返回 nil，实际 err=%v", err)
	}
	if len(llm.calls) != 0 {
		t.Fatalf("依赖缺失不应调 LLM，实际 %d 次", len(llm.calls))
	}
	seg2 := getSeg(t, fx.transcripts, fx.tr.ID, 2)
	if seg2.Text != "常梦瑜你看到我的邮件了吗" || seg2.CorrectedReason != nil {
		t.Fatalf("依赖缺失时 seg2 不应变，text=%q reason=%v", seg2.Text, seg2.CorrectedReason)
	}
}
