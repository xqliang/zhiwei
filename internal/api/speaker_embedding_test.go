package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// setupSpeakerEmbAPI 构造带多条声纹 repo 的说话人 handler（fake Voiceprint：Embed 返回
// one-hot e0，供聚合断言）。
func setupSpeakerEmbAPI(t *testing.T) (http.Handler, *repo.SpeakerRepo, *repo.SpeakerEmbeddingRepo) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.InitForTest(); err != nil {
		t.Fatal(err)
	}
	speakers := &repo.SpeakerRepo{DB: db}
	embs := &repo.SpeakerEmbeddingRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		SpeakerEmbeddings: embs,
	})
	return r, speakers, embs
}

// oneHot 构造 256 维 one-hot 向量（helper，测试内联用）。
func oneHot(i int) []float32 {
	v := make([]float32, 256)
	v[i] = 1
	return v
}

// TestSpeakerMultiEmbeddings 覆盖多条声纹核心链路（2026-08-26 需求）：
// ① 启动回填：既有 speaker.embedding 物化成首条样本（幂等——再跑不重复插）；
// ② 追加样本：POST embeddings → 聚合重算代表（两向量均值）；
// ③ 改备注 PATCH；④ 删单条 DELETE → 代表重算回剩余样本；
// ⑤ 合并：源样本**迁入**目标（不丢弃），目标代表 = 目标+迁入全部样本聚合。
func TestSpeakerMultiEmbeddings(t *testing.T) {
	r, speakers, embs := setupSpeakerEmbAPI(t)
	ctx := context.Background()

	// 造两个说话人：甲（e0 向量）、乙（e1 向量），模拟单向量时代的存量数据
	jia := &repo.Speaker{Name: "甲", Source: "enrolled", Embedding: float32BlobAPI(oneHot(0)), SampleCount: 1}
	yi := &repo.Speaker{Name: "乙", Source: "auto", Embedding: float32BlobAPI(oneHot(1)), SampleCount: 2}
	for _, sp := range []*repo.Speaker{jia, yi} {
		if err := speakers.Create(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, sp := range []*repo.Speaker{jia, yi} {
			cctx := context.Background()
			_ = embs.DeleteBySpeaker(cctx, sp.ID)
			_ = speakers.Delete(cctx, sp.ID)
		}
	})

	// ① 回填：各有 embedding 但无样本行 → 各补 1 条；再跑一遍不重复
	n, err := embs.EnsureSpeakerEmbeddingBootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("回填至少 2 条（甲/乙），实际 %d", n)
	}
	if n2, _ := embs.EnsureSpeakerEmbeddingBootstrap(ctx); n2 != 0 {
		t.Fatalf("二次回填应 0（幂等），实际 %d", n2)
	}
	es, _ := embs.ListBySpeaker(ctx, jia.ID)
	if len(es) != 1 || es[0].Source != "manual" {
		t.Fatalf("甲应回填 1 条 manual 样本: %+v", es)
	}
	if e2, _ := embs.ListBySpeaker(ctx, yi.ID); len(e2) != 1 || e2[0].Source != "auto" {
		t.Fatalf("乙应回填 1 条 auto 样本（source 沿用）: %+v", e2)
	}

	// ② 合并：乙并入甲 → 乙的样本迁入甲（source=merge），甲代表 = e0+e1 均值归一
	body := `{"source_ids":["` + yi.ID.String() + `"],"target_id":"` + jia.ID.String() + `"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/speakers/merge", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := speakers.Get(ctx, yi.ID); err == nil {
		t.Fatal("乙应已被删（并入甲）")
	}
	es2, _ := embs.ListBySpeaker(ctx, jia.ID)
	if len(es2) != 2 {
		t.Fatalf("合并后甲应有 2 条样本（累加不覆盖），实际 %d: %+v", len(es2), es2)
	}
	merged := false
	for _, e := range es2 {
		if e.Source == "merge" {
			merged = true
		}
	}
	if !merged {
		t.Fatalf("迁入样本 source 应为 merge: %+v", es2)
	}
	jiaRow, _ := speakers.Get(ctx, jia.ID)
	got := mustDecodeEmb(t, jiaRow.Embedding)
	// e0+e1 均值 L2 归一 → 下标 0/1 各 ≈0.7071
	if math.Abs(float64(got[0])-0.7071) > 1e-3 || math.Abs(float64(got[1])-0.7071) > 1e-3 {
		t.Fatalf("合并后甲代表应为 e0+e1 均值归一（0.707/0.707），实际 %v/%v", got[0], got[1])
	}
	if jiaRow.SampleCount != 3 { // 甲 1 + 乙 2
		t.Fatalf("sample_count 应 3（1+2），实际 %d", jiaRow.SampleCount)
	}

	// ③ 追加一条（fake Embed 返回全 0.1 向量）→ 代表重算。走 multipart（真实 wav 测试文件）。
	wavBytes, err := os.ReadFile("../../testdata/speech.wav")
	if err != nil {
		t.Skip("testdata/speech.wav 缺失")
	}
	rec2 := httptest.NewRecorder()
	multi := "--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"note\"\r\n\r\n" +
		"追加样本备注\r\n" +
		"--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n" +
		"Content-Type: audio/wav\r\n\r\n" +
		string(wavBytes) + "\r\n" +
		"--BOUNDARY--\r\n"
	req := httptest.NewRequest(http.MethodPost, "/api/speakers/"+jia.ID.String()+"/embeddings", strings.NewReader(multi))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=BOUNDARY")
	r.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("add embedding: %d %s", rec2.Code, rec2.Body.String())
	}
	es3, _ := embs.ListBySpeaker(ctx, jia.ID)
	if len(es3) != 3 {
		t.Fatalf("追加后应有 3 条样本，实际 %d", len(es3))
	}

	// ④ 列表富化：GET /api/speakers 应带 embeddings 元数据（含备注）
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/speakers", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec3.Code, rec3.Body.String())
	}
	var listResp struct {
		Speakers []struct {
			ID         string `json:"id"`
			Embeddings []struct {
				Note      string `json:"note"`
				Source    string `json:"source"`
				CreatedAt string `json:"created_at"`
			} `json:"embeddings"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal(rec3.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	var jiaView *struct {
		ID         string `json:"id"`
		Embeddings []struct {
			Note      string `json:"note"`
			Source    string `json:"source"`
			CreatedAt string `json:"created_at"`
		} `json:"embeddings"`
	}
	for i := range listResp.Speakers {
		if listResp.Speakers[i].ID == jia.ID.String() {
			jiaView = &listResp.Speakers[i]
		}
	}
	if jiaView == nil || len(jiaView.Embeddings) != 3 {
		t.Fatalf("名册应附甲的 3 条样本元数据: %+v", jiaView)
	}
	foundNote := false
	for _, e := range jiaView.Embeddings {
		if e.Note == "追加样本备注" {
			foundNote = true
		}
		if e.CreatedAt == "" {
			t.Fatalf("样本应带 created_at: %+v", e)
		}
	}
	if !foundNote {
		t.Fatalf("名册样本元数据缺备注: %+v", jiaView.Embeddings)
	}

	// ⑤ 改备注 PATCH + 删单条 DELETE → 剩 2 条、代表重算
	var addEmbID string
	for _, e := range es3 {
		if e.Note != nil && *e.Note == "追加样本备注" {
			addEmbID = e.ID.String()
		}
	}
	rec4 := httptest.NewRecorder()
	r.ServeHTTP(rec4, httptest.NewRequest(http.MethodPatch,
		"/api/speakers/"+jia.ID.String()+"/embeddings/"+addEmbID,
		strings.NewReader(`{"note":"改过的备注"}`)))
	if rec4.Code != http.StatusOK {
		t.Fatalf("patch note: %d %s", rec4.Code, rec4.Body.String())
	}
	gotEmb, _ := embs.Get(ctx, func() ids.ID {
		id, _ := ids.ParseID(addEmbID)
		return id
	}())
	if gotEmb == nil || gotEmb.Note == nil || *gotEmb.Note != "改过的备注" {
		t.Fatalf("备注应已更新: %+v", gotEmb)
	}
	rec5 := httptest.NewRecorder()
	r.ServeHTTP(rec5, httptest.NewRequest(http.MethodDelete,
		"/api/speakers/"+jia.ID.String()+"/embeddings/"+addEmbID, nil))
	if rec5.Code != http.StatusNoContent {
		t.Fatalf("delete embedding: %d %s", rec5.Code, rec5.Body.String())
	}
	es4, _ := embs.ListBySpeaker(ctx, jia.ID)
	if len(es4) != 2 {
		t.Fatalf("删单条后应剩 2 条，实际 %d", len(es4))
	}
	// 删掉的是全 0.1 向量，剩 e0+e1（迁移）+e0（回填）→ 代表应只含下标 0/1 分量
	jiaRow2, _ := speakers.Get(ctx, jia.ID)
	got2 := mustDecodeEmb(t, jiaRow2.Embedding)
	if math.Abs(float64(got2[2])) > 1e-6 {
		t.Fatalf("删后代表不应含下标 2 分量，实际 %v", got2[2])
	}
}

// mustDecodeEmb 测试内联的 BLOB→向量（1024B）解码（断言失败即 Fatal）。
func mustDecodeEmb(t *testing.T, blob []byte) []float32 {
	t.Helper()
	v, ok := decodeEmbedding(blob)
	if !ok || len(v) != 256 {
		t.Fatalf("BLOB 解码失败或维度异常: len=%d ok=%v", len(blob), ok)
	}
	return v
}
