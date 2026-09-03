# 声纹合并相似度预检 + 二次确认 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 声纹页合并前，后端返回参与合并的声纹两两相似度，前端原地展开表格并要求二次确认后才真正发合并请求。

**Architecture:** 新增只读端点 `POST /api/speakers/similarities`，纯 Go 算两两 max 余弦（复用已有 `ListBySpeakers` 取样本，不走 FAISS sidecar）；相似度纯函数收敛进 `internal/voiceprint` 做单一事实源；前端在 sticky 确认卡片里多一态，第一次点「确认合并」出表、第二次点「⚠ 仍然合并」才发请求。低分只警示不拦截，预检失败放行。

**Tech Stack:** Go 1.x + chi + sqlx + MySQL（集成测试走 `repotest.DSN` 按包隔离库）；Vue 3 CDN 版（`web/app.js` + `web/index.html`，无构建步骤，改动后跑 `make hash-web` 重算指纹）。

**Spec:** `docs/superpowers/specs/2026-09-03-voiceprint-merge-similarity-confirm-design.md`

**工作目录：** `.worktrees/voiceprint-merge-sim`，分支 `feat/voiceprint-merge-similarity-confirm`

**跑测试的前置：** MySQL 需在跑（`zhiwei-mvp-mysql`，3307）。库被并行分支搞脏时先 `make init-testdb`。

---

## 文件结构

| 文件 | 责任 | 动作 |
|---|---|---|
| `internal/voiceprint/pair.go` | 相似度纯函数：`Cosine`（实现）+ `MaxCosine`（多样本取 max） | 新建 |
| `internal/voiceprint/pair_test.go` | 上述纯函数的单测 + benchmark | 新建 |
| `internal/api/speaker.go` | `Similarities` handler + 路由；本地 `cosine` 收敛为转调 `voiceprint.Cosine` | 修改 |
| `internal/api/speaker_test.go` | handler 集成测试 | 修改 |
| `web/app.js` | 合并流程三态 + 相似度预检调用 | 修改 |
| `web/index.html` | sticky 卡片里的相似度表 | 修改 |

**边界说明：** `Cosine` 放进 `internal/voiceprint` 而非留在 `internal/api`，是为了让相似度语义有单一事实源（spec §3）。既有 `internal/pipeline/stage_speaker.go` 的 `dotSim` 是第三份同实现拷贝，其注释自陈「与 api.cosine 同实现」——它属另一包、不参与本次改动，**本计划不动它**，避免范围蔓延。

---

## Task 1: 相似度纯函数 `Cosine` / `MaxCosine`

**Files:**
- Create: `internal/voiceprint/pair.go`
- Test: `internal/voiceprint/pair_test.go`

- [ ] **Step 1: 写失败的测试**

新建 `internal/voiceprint/pair_test.go`：

```go
package voiceprint

import (
	"math"
	"testing"
)

// TestMaxCosinePicksHighestAcrossSamples 多向量语义：与对方任意一条样本的最高分，
// 不是与聚合代表（质心）的分数——感冒/哑嗓变体不得稀释分数（与 Matched/MatchPreview 同口径）。
func TestMaxCosinePicksHighestAcrossSamples(t *testing.T) {
	// 3 维且全部 L2 归一化，点积即余弦。
	a := [][]float32{{1, 0, 0}, {0, 1, 0}}
	b := [][]float32{{0.6, 0.8, 0}, {0, 0, 1}, {0.8, 0.6, 0}}
	// 最高分两处同为 0.8：a1·b3 与 a2·b1；b2 与两者正交=0，不得成为最高分。
	if got := MaxCosine(a, b); math.Abs(got-0.8) > 1e-6 {
		t.Fatalf("MaxCosine = %v, want 0.8", got)
	}
}

// TestMaxCosineSymmetric 相似度必须对称（前端按 id 升序生成上三角对，顺序无关）。
func TestMaxCosineSymmetric(t *testing.T) {
	a := [][]float32{{1, 0, 0}, {0, 1, 0}}
	b := [][]float32{{0.6, 0.8, 0}, {0, 0, 1}}
	if fwd, rev := MaxCosine(a, b), MaxCosine(b, a); math.Abs(fwd-rev) > 1e-9 {
		t.Fatalf("不对称: fwd=%v rev=%v", fwd, rev)
	}
}

// TestMaxCosineEmptySideIsZero 任一方无样本返回 0——纯向量域无法表达「不可比」，
// handler 须用「样本表是否有该人」另行判定后以 null 呈现（spec §3/§7）。
func TestMaxCosineEmptySideIsZero(t *testing.T) {
	one := [][]float32{{1, 0, 0}}
	for _, tc := range []struct {
		name string
		a, b [][]float32
	}{
		{"a 空", nil, one},
		{"b 空", one, nil},
		{"双侧空", nil, nil},
	} {
		if got := MaxCosine(tc.a, tc.b); got != 0 {
			t.Fatalf("%s: want 0, got %v", tc.name, got)
		}
	}
}

// TestCosineTruncatesToShorterDim 维度不一致取较短者，与旧 api.cosine 行为一致
// （防御脏数据不报错；真实向量恒为 256 维，此分支仅兜底）。
func TestCosineTruncatesToShorterDim(t *testing.T) {
	if got := Cosine([]float32{1, 0, 0}, []float32{1, 0, 0, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("同向 want 1, got %v", got)
	}
	if got := Cosine([]float32{0, 1, 0}, []float32{1, 0, 0, 0}); math.Abs(got-0) > 1e-9 {
		t.Fatalf("正交 want 0, got %v", got)
	}
}

// BenchmarkMaxCosine 佐证成本可忽略（CLAUDE.md：性能须有数据）。
// 造 10×10 样本对（256 维，贴近单人样本量上限）——handler 端 N=10 时要跑 45 对。
func BenchmarkMaxCosine(b *testing.B) {
	a := make([][]float32, 10)
	c := make([][]float32, 10)
	for i := range a {
		va := make([]float32, 256)
		vc := make([]float32, 256)
		for j := range va {
			va[j] = float32((i*7 + j) % 13)
			vc[j] = float32((i*5 + j*3) % 11)
		}
		a[i], c[i] = va, vc
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MaxCosine(a, c)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/voiceprint/ -run 'TestMaxCosine|TestCosineTruncates' -v`
Expected: FAIL —— `undefined: MaxCosine` 和 `undefined: Cosine`

- [ ] **Step 3: 写最小实现**

新建 `internal/voiceprint/pair.go`：

```go
// pair.go：说话人两两相似度（纯函数，声纹页「手动合并」前的相似度预检用）。
package voiceprint

// Cosine 两个 L2 归一向量的余弦相似度（= 内积）。声纹向量由 sidecar 归一化，
// 与 sidecar FAISS IndexFlatIP(内积) 等价——BLOB 与索引同向量，结果一致。
//
// 相似度语义收敛到本包做单一事实源：原 internal/api/speaker.go 的 cosine 已改为
// 转调本函数（Task 2）。另有一份同实现拷贝 internal/pipeline/stage_speaker.go 的
// dotSim，属另一包且不参与本次改动，保持原样。
func Cosine(a, b []float32) float64 {
	var s float64
	n := len(a)
	if len(b) < n {
		n = len(b) // 维度不一致取较短者：防御脏数据，不报错（真实向量恒 256 维）
	}
	for i := 0; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// MaxCosine 两组向量间的最大余弦相似度（多向量语义：与对方任意一条样本的最高分）。
// 与 Matched / MatchPreview 的「多向量取 max」同口径——感冒/哑嗓变体不会稀释分数。
//
// 任一方无样本返回 0：「不可比」与「完全不同」在纯向量域无法区分，交由调用方（handler）
// 用「该说话人是否存在于样本表」另行判定后以 null 表达。
func MaxCosine(a, b [][]float32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var best float64
	for _, x := range a {
		for _, y := range b {
			if s := Cosine(x, y); s > best {
				best = s
			}
		}
	}
	return best
}
```

- [ ] **Step 4: 跑测试确认通过 + benchmark**

Run: `go test ./internal/voiceprint/ -v`
Expected: PASS（含 `TestCallerPkgDBName` 等既有用例）

Run: `go test ./internal/voiceprint/ -bench=BenchmarkMaxCosine -benchmem -run=^$`
Expected: 打印 `BenchmarkMaxCosine-...  xxx ns/op`，记录该数值——spec §3 要求性能有数据，提交信息里带上。

- [ ] **Step 5: 提交**

```bash
git add internal/voiceprint/pair.go internal/voiceprint/pair_test.go
git commit -m "$(cat <<'EOF'
feat(voiceprint): 说话人两两相似度纯函数——Cosine + MaxCosine

相似度语义收敛到 voiceprint 包做单一事实源（原 api.cosine 就地复制过两份）。

- Cosine：L2 归一向量的内积，与 sidecar IndexFlatIP 等价
- MaxCosine：多样本取 max（与 Matched/MatchPreview 同口径，变体不稀释分数）；
  任一方无样本返回 0，「不可比」由 handler 另行以 null 表达
- BenchmarkMaxCosine 佐证成本可忽略：10×10 样本对（256 维）xxx ns/op

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `Similarities` handler + 路由

**Files:**
- Modify: `internal/api/speaker.go:96-115`（路由注册区）、`:1230`、`:1366`（cosine 调用点）、`:1281-1290`（cosine 定义）、新增 handler
- Test: `internal/api/speaker_test.go`

**关键事实（动手前必读）：**
- 复用 `SpeakerEmbeddingRepo.ListBySpeakers`（`internal/repo/speaker_embedding.go:54`）——已存在，按说话人分组返回 `map[ids.ID][]SpeakerEmbedding`，缺失 id 自然不进 map。
- 复用 `decodeEmbedding`（`internal/api/speaker.go:1268`）：BLOB→`[]float32`，长度非 4 的倍数时返回 `false`。
- `SpeakerRepo.List`（`internal/repo/speaker.go:56`）返回全部 active 说话人、**不按 user 过滤**。既有 `Merge`/`Delete` 亦不按 user 过滤——本端点保持一致，不凭空造归属校验（spec §3/§7）。
- `ids.ID` 是 `type ID int64`，可直接比较；有 `.Int64()` / `.String()`。
- `internal/api/speaker.go` 已 import `sort` 与 `voiceprint`，无需改 import。

- [ ] **Step 1: 写失败的 handler 测试**

在 `internal/api/speaker_test.go` 末尾追加：

```go
// --- 声纹合并相似度预检（POST /api/speakers/similarities） ---

// simBlob 把 float32 切片编码成 DB 存的 LONGBLOB（小端），与 decodeEmbedding 互逆。
// 形参用 testing.TB 而非 *testing.T：Step 7 的 benchmark 也要复用它（*testing.B 满足 TB）。
func simBlob(tb testing.TB, v []float32) []byte {
	tb.Helper()
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// vec3 造 3 维 L2 归一化向量（点积即余弦），避开 256 维手写。
// a1={1,0,0} a2={0,1,0}；b1={0.6,0.8,0} b2={0,0,1} b3={0.8,0.6,0}。
// MaxCosine(a,b)=0.8（a1·b3 与 a2·b1 同为 0.8，b2 正交不得当选）。
func vec3(x, y, z float32) []float32 {
	n := float32(math.Sqrt(float64(x*x + y*y + z*z)))
	return []float32{x / n, y / n, z / n}
}

func setupSimilaritiesAPI(t *testing.T) (http.Handler, *repo.SpeakerRepo, *repo.SpeakerEmbeddingRepo) {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.InitForTest(); err != nil {
		t.Fatal(err)
	}
	speakers := &repo.SpeakerRepo{DB: db}
	embeddings := &repo.SpeakerEmbeddingRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{Speakers: speakers, Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(), SpeakerEmbeddings: embeddings})
	return r, speakers, embeddings
}

// newSimSpeaker 建说话人并写一条样本，返回 id。
func newSimSpeaker(t *testing.T, ctx context.Context, r *repo.SpeakerRepo, emb *repo.SpeakerEmbeddingRepo, name string, v []float32) ids.ID {
	t.Helper()
	sp := &repo.Speaker{Name: name, Source: "auto"}
	if err := r.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if err := emb.Create(ctx, &repo.SpeakerEmbedding{SpeakerID: sp.ID, Embedding: simBlob(t, v), SampleCount: 1, Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	return sp.ID
}

func postSimilarities(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/speakers/similarities", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSimilaritiesReturnsAllPairs(t *testing.T) {
	h, speakers, emb := setupSimilaritiesAPI(t)
	ctx := context.Background()
	a := newSimSpeaker(t, ctx, speakers, emb, "甲", vec3(1, 0, 0))
	b := newSimSpeaker(t, ctx, speakers, emb, "乙", vec3(0.6, 0.8, 0))
	c := newSimSpeaker(t, ctx, speakers, emb, "丙", vec3(0, 0, 1))

	rec := postSimilarities(t, h, fmt.Sprintf(`{"ids":["%s","%s","%s"]}`, a, b, c))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Pairs []struct {
			AID, BID             string   `json:"a_id"`
			AName, BName         string   `json:"a_name"`
			Similarity           *float64 `json:"similarity"`
		} `json:"pairs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// 3 个说话人 → C(3,2)=3 对，行数恒定不依赖谁是 target
	if len(resp.Pairs) != 3 {
		t.Fatalf("pairs=%d want 3: %s", len(resp.Pairs), rec.Body.String())
	}
	// 按 id 升序生成，响应确定
	if resp.Pairs[0].AID != a.String() || resp.Pairs[0].BID != b.String() {
		t.Fatalf("首对应为 甲×乙，实为 %s×%s", resp.Pairs[0].AID, resp.Pairs[0].BID)
	}
	if resp.Pairs[0].AName != "甲" || resp.Pairs[0].BName != "乙" {
		t.Fatalf("名字未带出: %+v", resp.Pairs[0])
	}
	// 甲×乙 = 0.8（多样本取 max 口径）
	if resp.Pairs[0].Similarity == nil || math.Abs(*resp.Pairs[0].Similarity-0.8) > 1e-6 {
		t.Fatalf("甲×乙 similarity=%v want 0.8", resp.Pairs[0].Similarity)
	}
	// 甲×丙 正交 = 0；乙×丙 = 0.6
	if resp.Pairs[1].Similarity == nil || math.Abs(*resp.Pairs[1].Similarity) > 1e-6 {
		t.Fatalf("甲×丙 similarity=%v want 0", resp.Pairs[1].Similarity)
	}
	if resp.Pairs[2].Similarity == nil || math.Abs(*resp.Pairs[2].Similarity-0.6) > 1e-6 {
		t.Fatalf("乙×丙 similarity=%v want 0.6", resp.Pairs[2].Similarity)
	}
}

func TestSimilaritiesDedupesIDs(t *testing.T) {
	h, speakers, emb := setupSimilaritiesAPI(t)
	ctx := context.Background()
	a := newSimSpeaker(t, ctx, speakers, emb, "甲", vec3(1, 0, 0))
	b := newSimSpeaker(t, ctx, speakers, emb, "乙", vec3(0, 1, 0))

	// 重复 id 必须去重，否则行数会翻倍
	rec := postSimilarities(t, h, fmt.Sprintf(`{"ids":["%s","%s","%s","%s"]}`, a, b, a, b))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Pairs []map[string]any `json:"pairs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Pairs) != 1 {
		t.Fatalf("去重后期望 1 对，实为 %d 对: %s", len(resp.Pairs), rec.Body.String())
	}
}

func TestSimilaritiesRejectsTooFewIDs(t *testing.T) {
	h, speakers, emb := setupSimilaritiesAPI(t)
	ctx := context.Background()
	a := newSimSpeaker(t, ctx, speakers, emb, "甲", vec3(1, 0, 0))

	for _, body := range []string{
		`{"ids":[]}`,
		fmt.Sprintf(`{"ids":["%s"]}`, a),
	} {
		rec := postSimilarities(t, h, body)
		if rec.Code != 400 {
			t.Fatalf("body=%s → code=%d want 400 (body=%s)", body, rec.Code, rec.Body.String())
		}
	}
	// 非法 id 字符串同样 400
	rec := postSimilarities(t, h, fmt.Sprintf(`{"ids":["%s","not-an-id"]}`, a))
	if rec.Code != 400 {
		t.Fatalf("非法 id → code=%d want 400", rec.Code)
	}
}

// TestSimilaritiesNullForSpeakerWithoutSamples 无样本的说话人：该对 similarity 为 null
// 而非 0——0 会被读成「完全不相似」，null 才表达「不可比」（spec §7）。
func TestSimilaritiesNullForSpeakerWithoutSamples(t *testing.T) {
	h, speakers, emb := setupSimilaritiesAPI(t)
	ctx := context.Background()
	a := newSimSpeaker(t, ctx, speakers, emb, "甲", vec3(1, 0, 0))
	// 乙：建了说话人但不写样本
	bare := &repo.Speaker{Name: "乙", Source: "auto"}
	if err := speakers.Create(ctx, bare); err != nil {
		t.Fatal(err)
	}

	rec := postSimilarities(t, h, fmt.Sprintf(`{"ids":["%s","%s"]}`, a, bare.ID))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Pairs []struct {
			Similarity *float64 `json:"similarity"`
		} `json:"pairs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Pairs) != 1 {
		t.Fatalf("期望 1 对，实为 %d", len(resp.Pairs))
	}
	if resp.Pairs[0].Similarity != nil {
		t.Fatalf("无样本对应为 null，实为 %v", *resp.Pairs[0].Similarity)
	}
	// 行数仍是 C(2,2)=1——「不可比」不剔除该说话人
}

// TestSimilaritiesMultiSampleTakesMax 多样本取 max：对方三条样本里最高的那个算分，
// 不是与聚合质心的分（变体不得稀释）。
func TestSimilaritiesMultiSampleTakesMax(t *testing.T) {
	h, speakers, emb := setupSimilaritiesAPI(t)
	ctx := context.Background()
	a := newSimSpeaker(t, ctx, speakers, emb, "甲", vec3(1, 0, 0))
	b := newSimSpeaker(t, ctx, speakers, emb, "乙", vec3(0, 1, 0))
	// 给乙补两条样本：一条与甲正交，一条与甲同向 → max 应取到 1.0
	if err := emb.Create(ctx, &repo.SpeakerEmbedding{SpeakerID: b, Embedding: simBlob(t, vec3(0, 0, 1)), SampleCount: 1, Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	if err := emb.Create(ctx, &repo.SpeakerEmbedding{SpeakerID: b, Embedding: simBlob(t, vec3(1, 0, 0)), SampleCount: 1, Source: "manual"}); err != nil {
		t.Fatal(err)
	}

	rec := postSimilarities(t, h, fmt.Sprintf(`{"ids":["%s","%s"]}`, a, b))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Pairs []struct {
			Similarity *float64 `json:"similarity"`
		} `json:"pairs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Pairs[0].Similarity == nil || math.Abs(*resp.Pairs[0].Similarity-1) > 1e-6 {
		t.Fatalf("多样本应取 max=1.0，实为 %v", resp.Pairs[0].Similarity)
	}
}

func TestSimilaritiesRequiresEmbeddingsRepo(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.InitForTest(); err != nil {
		t.Fatal(err)
	}
	// 未装配 SpeakerEmbeddings → 降级 501，与既有 ListEmbeddings 同语义
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{Speakers: &repo.SpeakerRepo{DB: db}, Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir()})
	rec := postSimilarities(t, r, `{"ids":["1","2"]}`)
	if rec.Code != 501 {
		t.Fatalf("未装配 → code=%d want 501", rec.Code)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run 'TestSimilarities' -v`
Expected: FAIL —— 编译错误 `undefined: (SpeakerHandler).Similarities`（其余既有测试不受影响）

- [ ] **Step 3: 实现 handler**

在 `internal/api/speaker.go` 的 `func (h *SpeakerHandler) List` 之后（`:72` 附近，`List` 函数体的结束位置）插入：

```go
// Similarities 声纹合并前的相似度预检：返回这组说话人两两之间的最大余弦相似度。
// 只读、不走 sidecar（向量本体在 DB BLOB，纯 Go 算），失败不阻断合并——前端预检
// 失败时放行，由用户自行确认（预检是辅助不是门禁）。
//
// 语义要点（spec §3/§7）：
//   - 范围仅限入参 ids 两两之间，不看全库第三人；
//   - 口径为多向量取 max（voiceprint.MaxCosine），与 Matched/MatchPreview 一致；
//   - 无样本的说话人不剔除，其全部相似度为 null（行数恒为 C(N_unique,2)）；
//   - 不做 user 维度过滤——既有 List/Merge/Delete 亦不过滤，保持一致。
func (h *SpeakerHandler) Similarities(w http.ResponseWriter, r *http.Request) {
	if h.SpeakerEmbeddings == nil {
		writeJSONError(w, "多条声纹功能未装配", http.StatusNotImplemented)
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "请求体非法", http.StatusBadRequest)
		return
	}
	uniq := map[ids.ID]struct{}{}
	for _, s := range req.IDs {
		id, err := ids.ParseID(s)
		if err != nil {
			writeJSONError(w, "invalid id: "+s, http.StatusBadRequest)
			return
		}
		uniq[id] = struct{}{}
	}
	if len(uniq) < 2 {
		writeJSONError(w, "至少 2 个说话人", http.StatusBadRequest)
		return
	}
	idList := make([]ids.ID, 0, len(uniq))
	for id := range uniq {
		idList = append(idList, id)
	}
	// 按 id 升序固定生成顺序 → 同一份输入响应稳定，前端可缓存比对
	sort.Slice(idList, func(i, j int) bool { return idList[i].Int64() < idList[j].Int64() })

	// 名字：一次 List 建映射，不做逐 id N+1。id 不在结果里（已 dismiss/删除）仍参与矩阵，
	// 名字留空、相似度为 null。List 失败不阻断：名字全空，相似度照算。
	nameByID := map[ids.ID]string{}
	if list, err := h.Speakers.List(r.Context()); err == nil {
		for _, sp := range list {
			nameByID[sp.ID] = sp.Name
		}
	}
	grouped, err := h.SpeakerEmbeddings.ListBySpeakers(r.Context(), idList)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 解码样本向量（一次），无有效样本的 id 不进入 vecs → 其全部相似度为 null
	vecs := make(map[ids.ID][][]float32, len(idList))
	for _, id := range idList {
		rows := grouped[id]
		if len(rows) == 0 {
			continue
		}
		vs := make([][]float32, 0, len(rows))
		for _, e := range rows {
			if v, ok := decodeEmbedding(e.Embedding); ok && len(v) == 256 {
				vs = append(vs, v)
			}
		}
		if len(vs) > 0 {
			vecs[id] = vs
		}
	}
	type simPair struct {
		AID        string   `json:"a_id"`
		BID        string   `json:"b_id"`
		AName      string   `json:"a_name"`
		BName      string   `json:"b_name"`
		Similarity *float64 `json:"similarity"` // null = 一方无样本，不可比
	}
	pairs := make([]simPair, 0, len(idList)*(len(idList)-1)/2)
	for i := 0; i < len(idList); i++ {
		for j := i + 1; j < len(idList); j++ {
			a, b := idList[i], idList[j]
			p := simPair{AID: a.String(), BID: b.String(), AName: nameByID[a], BName: nameByID[b]}
			if va, ok1 := vecs[a]; ok1 {
				if vb, ok2 := vecs[b]; ok2 {
					s := voiceprint.MaxCosine(va, vb)
					p.Similarity = &s
				}
			}
			pairs = append(pairs, p)
		}
	}
	writeJSON(w, map[string]any{"pairs": pairs})
}
```

- [ ] **Step 4: 注册路由**

`internal/api/speaker.go:103` 那行 `r.Post("/api/speakers/merge", h.Merge)` 之后加一行：

```go
	r.Post("/api/speakers/merge", h.Merge)                                       // 声纹页「手动合并」：多说话人并入一个目标（声纹样本累加）
	r.Post("/api/speakers/similarities", h.Similarities)                         // 合并前相似度预检：两两 max 余弦（只读，不走 sidecar）
```

- [ ] **Step 5: 把本地 cosine 收敛为转调（消除第三份拷贝）**

删除 `internal/api/speaker.go:1281-1290` 的整个 `cosine` 函数定义（含其上方注释），然后把两处调用点改为 `voiceprint.Cosine`：

`internal/api/speaker.go:1230`：
```go
			if s := cosine(vec, v); s > best {
```
改为：
```go
			if s := voiceprint.Cosine(vec, v); s > best {
```

`internal/api/speaker.go:1366`：同样改法。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/api/ -run 'TestSimilarities|TestSpeaker' -v`
Expected: PASS（6 个新测试 + 既有 `TestSpeaker*` 全绿，证明 cosine 收敛未破坏 MatchPreview）

Run: `go test ./internal/api/ ./internal/pipeline/ ./internal/voiceprint/ -p 2 -count=1`
Expected: 全 ok

- [ ] **Step 7: 补 benchmark 并记录数值**

在 `internal/api/speaker_test.go` 追加（用测试库，N=20 全量跑一遍）：

```go
// BenchmarkSimilaritiesHandler 佐证端点成本可忽略（CLAUDE.md：性能须有数据）。
// N=20 说话人 → C(20,2)=190 对，每对各 1 条样本；走测试库，含真实 BLOB 读写。
func BenchmarkSimilaritiesHandler(b *testing.B) {
	db, err := repo.NewDB(repotest.DSN(b))
	if err != nil {
		b.Fatal(err)
	}
	if err := ids.InitForTest(); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	speakers := &repo.SpeakerRepo{DB: db}
	emb := &repo.SpeakerEmbeddingRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{Speakers: speakers, Voiceprint: fakeVoiceprintAPI{}, DataDir: b.TempDir(), SpeakerEmbeddings: emb})
	raw := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		sp := &repo.Speaker{Name: fmt.Sprintf("s%d", i), Source: "auto"}
		if err := speakers.Create(ctx, sp); err != nil {
			b.Fatal(err)
		}
		if err := emb.Create(ctx, &repo.SpeakerEmbedding{SpeakerID: sp.ID, Embedding: simBlob(b, vec3(float32(i%3), float32((i+1)%3), float32((i+2)%3))), SampleCount: 1, Source: "manual"}); err != nil {
			b.Fatal(err)
		}
		raw = append(raw, sp.ID.String())
	}
	body := `{"ids":["` + strings.Join(raw, `","`) + `"]}`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/speakers/similarities", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != 200 {
			b.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}
```

注意 `simBlob` / `vec3` 的 `*testing.T` 形参要放宽为接口，否则 benchmark 传不进 `*testing.B`。改法：把两个辅助函数首参类型从 `*testing.T` 改为 `testing.TB`（`simBlob(t *testing.T, ...)` → `simBlob(tb testing.TB, ...)`），并把 Step 1 里所有 `simBlob(t, ...)` 调用保持不变（`*testing.T` 满足 `testing.TB`）。同时在 `internal/api/speaker_test.go` 的 import 块补 `"encoding/binary"` 与 `"math"`。

Run: `go test ./internal/api/ -bench=BenchmarkSimilaritiesHandler -benchmem -run=^$`
Expected: 打印 `BenchmarkSimilaritiesHandler-...  xxx ns/op`，记录数值。

同时确认 `internal/api/speaker_test.go` 的 import 块含 `"encoding/binary"` 与 `"math"`（Step 1 的辅助函数要用）；缺则补上。

- [ ] **Step 8: 提交**

```bash
git add internal/api/speaker.go internal/api/speaker_test.go
git commit -m "$(cat <<'EOF'
feat(api): 声纹合并相似度预检端点 POST /api/speakers/similarities

合并前先摊开「这几个人到底像不像」，把「手动 merge 前先看样本互似度」的
教训从靠人记得变成系统强制。

- 范围仅入参 ids 两两之间，不看全库第三人；口径多向量取 max（同 Matched）
- 复用已有 ListBySpeakers，不新增 repo 方法；纯 Go 算不走 sidecar
- 无样本的说话人不剔除、similarity 为 null（非 0）→ 行数恒 C(N_unique,2)
- 按 id 升序生成，响应确定；不做 user 过滤（与 List/Merge/Delete 一致）
- api.cosine 收敛为转调 voiceprint.Cosine，消除第三份同实现拷贝

BenchmarkSimilaritiesHandler（N=20，190 对）xxx ns/op，成本可忽略。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: 前端两阶段二次确认

**Files:**
- Modify: `web/app.js:2318-2356`（合并逻辑）
- Modify: `web/index.html:1644-1662`（sticky 确认卡片）

**关键事实：**
- 相似度分档**复用库里既有阈值，不引入新常量**：`≥0.8` 强像（`ZW_VOICEPRINT_THRESHOLD` 默认值）、`0.72~0.8` 弱像（`voiceprint.SoftMin`）、`<0.72` 标红警示。
- CSS 变量已存在：`--ok`/`--ok-soft`、`--danger`/`--danger-soft`、`--accent-2`、`--muted`、`--fs-xs`（`web/index.html:25-38`）。
- `.badge` 样式已存在（`web/index.html:115-118`），带 `.done`/`.failed` 等修饰类。
- 表格数字用 `.toFixed(3)`，与既有声纹匹配表一致（`web/index.html:1499`）。
- **不变量（必须写进注释）**：相似度矩阵只依赖勾选集合，与谁是 target 无关 → 改目标下拉不重算。
- 项目无前端测试基建，本任务靠手动验证。

- [ ] **Step 1: 加状态与预检函数**

`web/app.js` 中 `const spMergeTarget = ref(null);`（`:2320`）之后加两个 ref：

```javascript
    const spMergeSim = ref(null);        // 相似度预检结果：{pairs:[{a_id,b_id,a_name,b_name,similarity}]}（similarity=null 不可比）
    const spMergeSimLoaded = ref(false); // 本次勾选集合是否已拉到过相似度
```

紧接其下的注释块（`:2321` 那个「选中说话人按创建时间升序」注释上方）补一条不变量说明：

```javascript
    // 不变量：相似度矩阵只依赖「勾选集合」，与谁是 target 无关——用户在表格展开后改目标下拉
    // 时矩阵不重算，仅重标哪行是「源→目标」方向。故 startSpConfirm/cancelSpMerge/toggleSpSelect
    // 三处改集合时才重置 spMergeSim/spMergeSimLoaded；spMergeTarget 变化不重置。
```

改 `startSpMerge` / `cancelSpMerge` / `toggleSpSelect` 三个函数，让它们在**改变勾选集合**时重置预检状态（`startSpMerge` 与 `cancelSpMerge` 已重置全部 ref，只需补两个；`toggleSpSelect` 增删勾选时也要重置）：

```javascript
    function startSpMerge() { spMergeMode.value = true; spMergeSelected.value = []; spMergeConfirming.value = false; spMergeTarget.value = null; resetSpMergeSim(); }
    function cancelSpMerge() { spMergeMode.value = false; spMergeSelected.value = []; spMergeConfirming.value = false; spMergeTarget.value = null; resetSpMergeSim(); }
    function toggleSpSelect(sp) {
      const i = spMergeSelected.value.indexOf(sp.id);
      if (i >= 0) { spMergeSelected.value.splice(i, 1); if (spMergeTarget.value === sp.id) spMergeTarget.value = null; }
      else spMergeSelected.value.push(sp.id);
      resetSpMergeSim(); // 勾选集合变了 → 预检结果作废，下次确认时重拉
    }
    // 重置相似度预检（只在勾选集合变化时调；改 target 不调——矩阵与 target 无关）
    function resetSpMergeSim() { spMergeSim.value = null; spMergeSimLoaded.value = false; }
```

在 `startSpConfirm` 之后、`applySpMerge` 之前插入预检函数：

```javascript
    // 拉取勾选集合的两两相似度（合并前的预检）。失败不阻断——返回 false 让调用方放行，
    // 由用户自行确认（预检是辅助不是门禁，网络抖动不得卡住正常纠错流程）。
    async function loadSpMergeSim() {
      if (spMergeSimLoaded.value) return true;
      try {
        const d = await api('POST', '/api/speakers/similarities', { ids: spMergeSelected.value });
        spMergeSim.value = d;
        spMergeSimLoaded.value = true;
        return true;
      } catch (e) {
        notify('相似度预检失败，请自行确认后再合并', 3000);
        return false;
      }
    }
    // 分档徽标：复用库里既有阈值（0.8 强命中 / 0.72 弱命中下限），不引入新常量。
    function simTier(s) {
      if (s === null || s === undefined) return { cls: '', text: '无法比较（无样本）' };
      if (s >= 0.8) return { cls: 'done', text: '强像' };
      if (s >= 0.72) return { cls: 'active', text: '弱像' };
      return { cls: 'failed', text: '⚠ 不像，请确认' };
    }
```

- [ ] **Step 2: 挂到 Vue setup 的返回值（模板要用）**

`web/app.js:3440` 那行 return 里，`spMergeTarget` 之后插入新 ref 和 `simTier`：

```javascript
      spMergeMode, spMergeSelected, spMergeSelectedSorted, spMergeConfirming, spMergeTarget, spMergeSim, spMergeSimLoaded, simTier, startSpMerge, cancelSpMerge, toggleSpSelect, startSpConfirm, applySpMerge,
```

> **漏了这步模板会静默失效**——Vue 3 全局构建版只暴露 setup() 返回的键，`simTier`/`spMergeSim` 不进返回值，模板里就是 `undefined`，相似度表整块不渲染且无报错。

- [ ] **Step 3: 改合并为三态**

把 `applySpMerge` 整体替换为：

```javascript
    // 三态合并：①勾选→开始合并→选目标 → ②第一次点「确认合并」拉相似度并展开表格
    // → ③第二次点「⚠ 仍然合并」才真正发请求。
    async function applySpMerge() {
      if (spMergeSelected.value.length < 2) { notify('至少选 2 个说话人'); return; }
      if (!spMergeTarget.value) { notify('请选择保留的目标说话人'); return; }
      const sources = spMergeSelected.value.filter(id => id !== spMergeTarget.value);
      if (!sources.length) { notify('目标之外还需至少 1 个源'); return; }
      // 阶段②：还没看过相似度 → 先拉预检并展开表格，本次不真合并
      if (!spMergeSimLoaded.value) {
        await loadSpMergeSim();
        return; // 无论预检成功与否都停在这里：成功则展开表格待二次确认，失败则 toast 后放行重试
      }
      try {
        await api('POST', '/api/speakers/merge', { source_ids: sources, target_id: spMergeTarget.value });
        cancelSpMerge();
        await loadAllSpeakers();
        if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
        notify('已合并 ' + sources.length + ' 个说话人到目标', 2000);
      } catch (e) { showError(e); }
    }
```

- [ ] **Step 4: 加相似度表格到 sticky 卡片**

`web/index.html` 的合并确认条里，`保留目标：` 那行 `</div>`（`:1661` 附近，即 `</div>` 关闭 `.kv` 之前）**之前**插入表格区块：

```html
            <!-- 相似度预检表：第一次点「确认合并」后展开；改目标不重拉（矩阵只依赖勾选集合） -->
            <div v-if="spMergeSimLoaded && spMergeSim && (spMergeSim.pairs||[]).length" style="margin-top:10px; border-top:1px solid var(--line); padding-top:8px">
              <div class="muted" style="font-size:var(--fs-xs); margin-bottom:6px">
                两两相似度（{{ spMergeSim.pairs.length }} 对）：
                <span :style="{color:'var(--danger)'}">标红表示两人相似度偏低，请确认是否同一人</span>
              </div>
              <div style="display:flex; flex-direction:column; gap:3px; max-height:180px; overflow:auto">
                <div v-for="(p, pi) in spMergeSim.pairs" :key="pi"
                     style="display:flex; align-items:center; gap:8px; font-size:var(--fs-xs); flex-wrap:wrap">
                  <span :style="{color: p.similarity !== null && p.similarity !== undefined && p.similarity < 0.72 ? 'var(--danger)' : 'inherit', fontWeight: p.similarity !== null && p.similarity !== undefined && p.similarity < 0.72 ? 600 : 400}">
                    {{ p.a_name || p.a_id }} × {{ p.b_name || p.b_id }}
                  </span>
                  <span style="font-variant-numeric:tabular-nums; color:var(--muted); width:44px; text-align:right">
                    {{ p.similarity === null || p.similarity === undefined ? '—' : p.similarity.toFixed(3) }}
                  </span>
                  <span class="badge" :class="simTier(p.similarity).cls">{{ simTier(p.similarity).text }}</span>
                  <span v-if="p.a_id === spMergeTarget || p.b_id === spMergeTarget" class="muted">← 含保留目标</span>
                </div>
              </div>
            </div>
```

同时把按钮文案随阶段切换（`web/index.html:1660` 那行）：

```html
            <button class="btn primary" style="padding:7px 14px" @click="applySpMerge">
              {{ spMergeSimLoaded ? '⚠ 仍然合并' : '确认合并' }}
            </button>
```

- [ ] **Step 5: 重算前端指纹**

Run: `make hash-web`
Expected: 打印重算结果，`web/app.<hash>.js` 更新（hash 变化）；`git status` 显示 `web/app.js`、`web/index.html`、新增的 hash 副本、以及被改动的旧 hash 副本删除。

- [ ] **Step 6: 手动验证**

Run: `make dev-restart`（或按项目惯用方式重启 dev server，端口 8081）

逐项确认：
1. 声纹页 →「手动合并」→ 勾 3 个说话人 →「开始合并」→ 目标下拉默认最早创建者
2. 点「确认合并」→ 出现相似度表，3 对分数 + 分档徽标；按钮变「⚠ 仍然合并」
3. **改目标下拉 → 表格不重拉、不闪烁**（验证不变量）
4. 再点「⚠ 仍然合并」→ 才真正合并，toast「已合并 N 个说话人到目标」，列表刷新
5. 取消/重新勾选 → 表格消失，下次确认重新拉取
6. 勾选含 0 样本的说话人 → 该对显示「—」+「无法比较（无样本）」
7. **接口失败放行**：用 devtools 把 `/api/speakers/similarities` 断网/强制 500 → toast「相似度预检失败，请自行确认后再合并」，按钮可继续点完成合并

- [ ] **Step 7: 提交**

```bash
git add web/app.js web/index.html
git commit -m "$(cat <<'EOF'
feat(web): 声纹合并两阶段二次确认——先看两两相似度再合并

合并前一点「确认合并」就直接发请求，纠错场景极易合错（memory 记着「手动 merge
前先看样本互似度」的教训）。改为先摊开相似度、二次确认后才真合并。

- sticky 卡片原地展开相似度表，不新造模态；第一次点确认合并出表、第二次点
  「⚠ 仍然合并」才发请求
- 分档复用既有阈值 0.8/0.72，不引入新常量；<0.72 标红警示但绝不拦截
- 矩阵只依赖勾选集合、与谁是 target 无关 → 改目标不重拉（写进注释的不变量）
- 预检失败 toast 后放行：预检是辅助不是门禁，网络抖动不得卡住正常纠错

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: 全量验证

**Files:** 无（只跑命令）

- [ ] **Step 1: 全量构建 + vet**

Run: `go build ./... && go vet ./...`
Expected: 无输出（成功）

- [ ] **Step 2: 全量测试**

Run: `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 2 -count=1 ./...`
Expected: 全 ok

> 若报 `no migration found for version 30` 之类——是并行分支把 `zhiwei_test_<pkg>` 库建到了更高的迁移版本。确认无并发 `go test` 进程后跑 `make init-testdb` 清库重来。

- [ ] **Step 3: 确认 spec 全覆盖**

对照 spec 逐节核对：

| spec 要求 | 落地 |
|---|---|
| §3 端点 `POST /api/speakers/similarities`，C(N,2) 对 | Task 2 Step 3/4 |
| §3 多向量取 max | Task 1 Step 3 |
| §3 复用 `ListBySpeakers`，不新增 repo 方法 | Task 2 Step 3 |
| §3 不走 sidecar | Task 2（全程只读 DB） |
| §3 相似度语义单一事实源在 voiceprint | Task 1 + Task 2 Step 5 |
| §3 ≥2 校验 / 非法 id 400 | Task 2 Step 1（`TestSimilaritiesRejectsTooFewIDs`） |
| §3 无 user 过滤 | Task 2（无相关代码） |
| §3 响应顺序确定 | Task 2 Step 3（升序生成） |
| §3 性能 benchmark | Task 1 Step 4 + Task 2 Step 7 |
| §4 两阶段确认 | Task 3 Step 2 |
| §4 分档 0.8/0.72、低分标红 | Task 3 Step 1（`simTier`） |
| §4 改目标不重算 | Task 3 Step 1（`resetSpMergeSim` 仅在集合变化时调） |
| §4 失败放行 | Task 3 Step 1（`loadSpMergeSim` catch） |
| §5 0 样本 → null | Task 2 Step 1（`TestSimilaritiesNullForSpeakerWithoutSamples`） |
| §5 nil repo → 降级 | Task 2 Step 1（`TestSimilaritiesRequiresEmbeddingsRepo`） |
| §6 纯函数测试 / handler 测试 / repo 无需新测试 | Task 1 + Task 2 |
| §7 风险：行数恒 C(N_unique,2) | Task 2 Step 1（dedup + 不剔除） |

- [ ] **Step 4: 提交（如有遗漏修补）**

若 Step 3 发现缺口，补对应任务后单独提交；无缺口则本任务无提交。

---

## 执行交接

计划已存至 `docs/superpowers/plans/2026-09-03-voiceprint-merge-similarity-confirm.md`。两种执行方式：

**1. Subagent-Driven（推荐）** — 每个 Task 派一个新 subagent，任务间我来 review，迭代快、上下文干净

**2. Inline Execution** — 在当前会话里用 executing-plans 批量执行，带检查点 review

选哪种？
