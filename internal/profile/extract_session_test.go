package profile

import (
	"context"
	"errors"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// mkSession 给 ExtractSession 造最小 session+transcript+segments 夹具。
func mkSession(t *testing.T, svc *Service, texts []string) ids.ID {
	t.Helper()
	ctx := context.Background()
	// audio_session.id 非自增，须由调用方赋雪花 ID：SessionRepo.Create 不自动生成
	// ID（与 transcript/segment 的 repo 不同），生产 api/audio.go 也是 ID: sid 显式赋值。
	// 不赋值会插入 id=0，多次造夹具即 PRIMARY 冲突。
	sess := &repo.AudioSession{ID: ids.New(), Source: "web_upload", Filename: "t.wav", StoragePath: "/tmp/t.wav", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sess.ID, Language: "zh-CN"}
	if err := svc.Transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	segs := make([]repo.TranscriptSegment, len(texts))
	for i, txt := range texts {
		segs[i] = repo.TranscriptSegment{
			TranscriptID: tr.ID, SequenceNo: i + 1, SpeakerLabel: "我", Text: txt,
			StartMS: int64(i * 4000), EndMS: int64(i*4000 + 3000),
		}
	}
	if err := svc.Transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

// newExtractService 构造带全部 repo + fakeLLM 的画像 Service 并跑 bootstrap，供
// ExtractSession 相关用例复用。resps 是 fakeLLM 按序返回的响应（每次 Chat 弹一条）；
// 不触达 LLM 的用例（如边界路径）可不传。
func newExtractService(t *testing.T, resps ...string) *Service {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		DB:       db,
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Speakers: &repo.SpeakerRepo{DB: db},
		Persons: &repo.PersonRepo{DB: db}, Attributes: &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db}, ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
		LLM:   &fakeLLM{resps: resps},
		Model: "test", Prompt: "sys", Window: 10, Gate: GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(context.Background(), svc.Persons, svc.Speakers); err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestExtractSession(t *testing.T) {
	ctx := context.Background()
	// 同一份 LLM 响应备两条：本用例调用 ExtractSession 两次（第二次验幂等重跑），
	// 每次抽取跑 1 个窗口 = 1 次 Chat，而 fakeLLM 每次 Chat 会弹掉一条响应
	//（见 extractor_test.go）。只给一条时第二次会拿到空串 → 解析失败，故按调用
	// 次数备足；两次响应相同，模拟真实 LLM 对同一转写重复产出同样的事实。
	resp := `{"facts":[
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"occupation","value":"后端开发工程师",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"relationship","subject":{"kind":"self"},"related":{"kind":"mentioned","name":"Alice"},
		 "relation_type":"配偶","label":"老婆","confidence":0.85,"epistemic_type":"observed","block_index":1}
	]}`
	svc := newExtractService(t, resp, resp)
	// 本用例把 occupation/配偶/Alice 写到共享的 owner（user_id=1）上，而这些 key 与
	// service_test.go 的 TestApplyFactsGatePaths 重叠；本包所有测试共用同一 zhiwei_test
	// 库且不逐个重置（靠各用例使用不相交的 key/人名共存，见 confirm_test.go）。故收尾
	// 删掉这三类行 + owner 的属性/关系审计，恢复 owner 干净基线，不污染后续同 owner 用例。
	// 提前注册（用 t.Cleanup），保证任一断言 t.Fatal 提前退出时也会清理。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person WHERE user_id = 1 AND display_name = 'Alice'`)
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			oid := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ? AND attr_key = 'occupation'`, oid)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_relationship WHERE person_id = ? AND relation_type = '配偶'`, oid)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ?`, oid)
		}
	})

	sid := mkSession(t, svc, []string{"我在互联网公司做后端开发，我老婆 Alice 是医生"})
	res, err := svc.ExtractSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if res.Windows != 1 || res.Tokens != 42 {
		t.Fatalf("stats 错误: %+v", res)
	}
	if res.Apply.Active != 2 {
		t.Fatalf("应 2 条 active(职业+配偶关系): %+v", res.Apply)
	}
	oid := ownerID(t, svc)
	oa, _ := svc.Attributes.FindActiveByKey(ctx, oid, "occupation")
	if oa == nil || oa.ValueText != "后端开发工程师" {
		t.Fatalf("owner 职业未落库: %+v", oa)
	}
	alice, _ := svc.Persons.FindByName(ctx, 1, "Alice")
	if alice == nil || alice.Status != "pending" {
		t.Fatalf("Alice 应为 pending 人物: %+v", alice)
	}

	// 幂等：重跑同 session 全部 skip
	res2, err := svc.ExtractSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Apply.Skipped != 2 || res2.Apply.Active != 0 {
		t.Fatalf("重跑应全部 skip: %+v", res2.Apply)
	}
}

// TestExtractSessionEdgePaths 覆盖 ExtractSession 的两条边界路径——这两条是
// Task 16 回填端点重放历史 session 时依赖的语义（不存在的 id 记入批次结果、缺转写
// 的 session 直接跳过），单独立用例保证不被回归。两条路径都在读 LLM 之前返回，
// 故 Service 无需备 LLM 响应，也不写 owner 数据、无需清理。
func TestExtractSessionEdgePaths(t *testing.T) {
	svc := newExtractService(t)
	ctx := context.Background()

	// ① 不存在的 session → ErrNotFound（Sessions.Get 返回 sql.ErrNoRows，映射为 ErrNotFound）
	if _, err := svc.ExtractSession(ctx, ids.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的 session 应返回 ErrNotFound: %v", err)
	}

	// ② session 存在但无 transcript → (零值, nil) 优雅跳过
	sess := &repo.AudioSession{ID: ids.New(), Source: "web_upload", Filename: "t.wav", StoragePath: "/tmp/t.wav", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ExtractSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("无 transcript 应优雅跳过: %v", err)
	}
	if res.Apply.Total != 0 || res.Windows != 0 {
		t.Fatalf("跳过应零值: %+v", res)
	}
}
