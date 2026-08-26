# 向量检索（doubao-embedding-vision）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: 用 superpowers:subagent-driven-development 逐任务执行；步骤用 `- [ ]` 复选框跟踪。

**Goal:** 给知微记忆接入语义向量检索——`search_memory` 与对话上下文头都用「向量+关键词+时新+重要度」混合召回，替代当前纯 `LIKE`。

**Architecture:** 新增多模态 embedder provider（Ark `doubao-embedding-vision`）→ 记忆 `embedding` 列存 f32 blob（后台 backfill 回填）→ 纯 Go 暴力 cosine + 混合打分的 `internal/retrieve` 服务 → 接到 `search_memory`（有向量则混合、否则退回关键词）与上下文头 top-k 种子（`ZW_AGENT_RETRIEVE_TOPK`）。embedder 未配置时全链路优雅降级到既有关键词行为。

**Tech Stack:** Go(chi/sqlx/MySQL8)、Ark OpenAI 兼容 multimodal embeddings、现有 `provider.EmbeddingProvider` 接口。

---

## ⚠️ 已实测确认的 embedder 契约（2026-08-26 真 key 探针，非文档推测）

- Key：`ARK_AUDIO_API_KEY`（≠ `ARK_API_KEY`；已在仓库根 `.env`）。标准 `…/api/v3` 用此 key **401**。
- baseURL：`https://ark.cn-beijing.volces.com/api/plan/v3`（就是这个，别改成 `/api/v3`）。
- 端点：`POST {base}/embeddings/multimodal`；model `doubao-embedding-vision`（解析为 `-251215`）。
- 请求体：`{"model":"doubao-embedding-vision","input":[{"type":"text","text":"…"}]}`（input 是类型化对象数组）。
- 响应：`{…,"object":"list","data":{"embedding":[…2048…]}}` —— `data` 是**单个对象**（一次一个向量），**非数组**；**无服务端批量**，N 条 = N 次调用。维度 **2048**。
- 现有 `internal/provider/embed.go` 的 `ArkEmbed`（文本 `/embeddings`、字符串 input、`data[].embedding`、16 批）**不兼容**，需新 provider。
- `memory.embedding` 已是 `LONGBLOB NULL`（migration 000001），**无需迁移**；存 f32 小端 2048×4=8192B。f32-LE 布局镜像 `internal/api/speaker.go:708 decodeEmbedding`。

## 混合打分（spec §10；无向量版是 `0.5*kw+0.3*recency+0.2*importance`，本期加向量项）

`score = 0.5*sim + 0.2*kw + 0.15*recency + 0.15*importance`（命名常量，注释标「可调」）。
- `sim` = query 与记忆向量 cosine（[-1,1]，clamp 到 [0,1]）。
- `kw` = query 文本（trim+lower）是否为 `title+content`（lower）子串 → 1/0（粗粒度，中文无分词；向量担语义，kw 仅兜底关键词命中）。
- `recency` = `clamp(1 - ageDays/365, 0, 1)`，age 用 `event_at`（NULL 回退 `created_at`）。
- `importance` = `clamp(m.Importance, 0, 1)`。

## File Structure

- 新建 `internal/provider/embed_vision.go`(+test)：`ArkVisionEmbed` multimodal provider。
- 新建 `internal/retrieve/codec.go`(+test)：f32-LE 编解码。
- 新建 `internal/retrieve/retriever.go`(+test)：打分 + `Search` + `Backfill`。
- 改 `internal/repo/memory.go`(+test)：`ListEmbeddedCandidates`/`ListForEmbedding`/`SetEmbeddingExt`。
- 改 `internal/agent/mcp_tools.go`(+test)：`MCPDeps.Retrieve`；`search_memory` 混合优先、关键词兜底。
- 改 `internal/agent/context.go`+`orchestrator.go`(+test)：上下文头 top-k 种子。
- 改 `internal/config/config.go`(+test)：`EmbedAPIKey`/`EmbedBaseURL`，`EmbedModel` 默认改 `doubao-embedding-vision`。
- 改 `cmd/zhiwei-server/main.go`：装配 provider/retriever + backfill sweep（启动一次 + 定时）。

**执行约束（并行安全）**：各任务只改各自包、只 `go test` 自己包，不碰 `main.go`/`web/*`、不跑 git；隔离库 `zhiwei_agentchat_test`（`TEST_MYSQL_DSN`，已到 000010）+ `t.Cleanup` 自清理。协调者（主）统一集成 + 提交。中文详细注释。

---

## 任务 1 — 多模态 embedder provider

**Files:** Create `internal/provider/embed_vision.go`, `internal/provider/embed_vision_test.go`

- [ ] **Step 1: 写失败测试**（httptest 模拟 multimodal 单对象响应）

```go
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArkVisionEmbedShape(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		// 校验请求体是类型化 input
		var req struct {
			Model string `json:"model"`
			Input []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) != 1 || req.Input[0].Type != "text" {
			t.Errorf("请求体 input 形状错: %+v", req.Input)
		}
		// 单对象 data（每文本一维度=3 的向量，值随文本变化便于断言）
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   map[string]any{"embedding": []float32{float32(len(req.Input[0].Text)), 1, 2}},
		})
	}))
	defer srv.Close()

	p := NewArkVisionEmbed(srv.URL, "k", "doubao-embedding-vision")
	out, err := p.Embed(context.Background(), []string{"aa", "bbbb"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 2 || len(out[0]) != 3 {
		t.Fatalf("应回 2 个向量、每个 3 维: %+v", out)
	}
	if out[0][0] != 2 || out[1][0] != 4 { // len("aa")=2, len("bbbb")=4 → 逐条单调用
		t.Errorf("应逐条调用 multimodal 端点: %+v", out)
	}
	for _, p := range gotPaths {
		if p != "/embeddings/multimodal" {
			t.Errorf("端点应为 /embeddings/multimodal, got %q", p)
		}
	}
}

func TestArkVisionEmbedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
	}))
	defer srv.Close()
	p := NewArkVisionEmbed(srv.URL, "k", "m")
	if _, err := p.Embed(context.Background(), []string{"x"}); err == nil {
		t.Error("error 响应应返回 err")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**：`go test ./internal/provider/ -run ArkVisionEmbed`，Expected: 编译失败（`NewArkVisionEmbed` 未定义）。

- [ ] **Step 3: 实现 provider**（实现同一 `EmbeddingProvider` 接口；逐条单调用、有界并发）

```go
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ArkVisionEmbed 走 Ark 多模态向量端点 /embeddings/multimodal（doubao-embedding-vision）。
// 与文本版 ArkEmbed 的关键差异（实测 2026-08-26）：input 是类型化对象数组、响应 data 是
// 单个对象（一次一个向量、无服务端批量），故 Embed 对 texts 逐条单调用（有界并发）。
type ArkVisionEmbed struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func NewArkVisionEmbed(baseURL, apiKey, model string) *ArkVisionEmbed {
	return &ArkVisionEmbed{baseURL: baseURL, apiKey: apiKey, model: model,
		client: &http.Client{Timeout: 30 * time.Second}}
}

type visionEmbedResp struct {
	Data struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed 逐条向量化（multimodal 端点一次一个向量）。有界并发 4，保序返回。
// 任一条失败即整体失败（调用方 backfill 会跳过该批、下轮重试）。
func (p *ArkVisionEmbed) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	sem := make(chan struct{}, 4)
	errCh := make(chan error, len(texts))
	done := make(chan struct{}, len(texts))
	for i, t := range texts {
		sem <- struct{}{}
		go func(i int, t string) {
			defer func() { <-sem; done <- struct{}{} }()
			v, err := p.embedOne(ctx, t)
			if err != nil {
				errCh <- err
				return
			}
			out[i] = v
		}(i, t)
	}
	for range texts {
		<-done
	}
	select {
	case err := <-errCh:
		return nil, err
	default:
		return out, nil
	}
}

func (p *ArkVisionEmbed) embedOne(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{
		"model": p.model,
		"input": []map[string]any{{"type": "text", "text": text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/embeddings/multimodal", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var er visionEmbedResp
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("ark vision embed 响应解析失败 (http %d): %s", resp.StatusCode, truncate(raw))
	}
	if er.Error != nil {
		return nil, fmt.Errorf("ark vision embed 错误: %s", er.Error.Message)
	}
	if len(er.Data.Embedding) == 0 {
		return nil, fmt.Errorf("ark vision embed 空向量 (http %d)", resp.StatusCode)
	}
	return er.Data.Embedding, nil
}
```

- [ ] **Step 4: 跑测试确认通过**：`go test ./internal/provider/ -run ArkVisionEmbed -v`，Expected: PASS。（`truncate` 已在 embed.go 同包存在，复用。）

---

## 任务 2 — f32 编解码 + memory embedding 存取

**Files:** Create `internal/retrieve/codec.go`,`internal/retrieve/codec_test.go`；Modify `internal/repo/memory.go`；Create `internal/repo/memory_embed_test.go`

- [ ] **Step 1: codec 失败测试**

```go
package retrieve

import "testing"

func TestF32RoundTrip(t *testing.T) {
	v := []float32{0.5, -1.25, 3.0, 0}
	b := EncodeF32(v)
	if len(b) != 16 {
		t.Fatalf("4 个 f32 应 16 字节, got %d", len(b))
	}
	got := DecodeF32(b)
	if len(got) != len(v) {
		t.Fatalf("维度不符: %d", len(got))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Errorf("第 %d 个: got %v want %v", i, got[i], v[i])
		}
	}
	if DecodeF32([]byte{1, 2, 3}) != nil { // 非 4 倍数 → nil（防脏数据）
		t.Error("非 4 倍数字节应回 nil")
	}
}
```

- [ ] **Step 2: 确认失败**：`go test ./internal/retrieve/ -run F32`，Expected: 编译失败。

- [ ] **Step 3: 实现 codec**（小端 f32，镜像 speaker.go 布局）

```go
package retrieve

import (
	"encoding/binary"
	"math"
)

// EncodeF32 把向量编码成小端 float32 字节（与 internal/api/speaker.go 声纹布局一致）。
func EncodeF32(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

// DecodeF32 解码小端 float32 字节；长度非 4 倍数（脏数据）返回 nil。
func DecodeF32(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
```

- [ ] **Step 4: repo 失败测试**（隔离库；seed 记忆、写向量、读回候选/待嵌）

```go
package repo

import (
	"testing"

	"zhiwei/internal/ids"
)

func TestMemoryEmbeddingStore(t *testing.T) {
	db := testDB(t) // 见同包既有测试的 DSN 助手；若无则仿 orchDSN 用 TEST_MYSQL_DSN
	r := &MemoryRepo{DB: db}
	ctx := t.Context()
	m := &Memory{Type: "fact", Title: "向量存取T", Content: "内容", EpistemicType: "observed",
		Confidence: 0.8, Importance: 0.5, Status: "active", TranscriptSegmentIDs: ids.List{}}
	if err := r.InsertExt(ctx, r.DB, []*Memory{m}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.DB.Exec("DELETE FROM memory WHERE id=?", m.ID.Int64()) })

	// 初始应出现在「待嵌」列表（embedding NULL）
	need, err := r.ListForEmbedding(ctx, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !containsID(need, m.ID) {
		t.Fatal("新记忆应在待嵌列表")
	}
	// 写入向量
	if err := r.SetEmbeddingExt(ctx, r.DB, m.ID, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	// 现应出现在「已嵌候选」、消失于「待嵌」
	cand, err := r.ListEmbeddedCandidates(ctx, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !containsMemID(cand, m.ID) {
		t.Fatal("写向量后应在已嵌候选")
	}
	need2, _ := r.ListForEmbedding(ctx, 1, 500)
	if containsID(need2, m.ID) {
		t.Error("写向量后不应再在待嵌列表")
	}
}

func containsID(rows []Memory, id ids.ID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
func containsMemID(rows []Memory, id ids.ID) bool { return containsID(rows, id) }
```
（注：若同包已有 DB 助手名不同，实现者按既有命名调整；不新增 DSN 逻辑。）

- [ ] **Step 5: 确认失败** → **Step 6: 实现 repo 三方法**

```go
// ListForEmbedding 返回该用户「active 且尚无 embedding」的记忆（backfill 待嵌队列）。
// 只取 id/title/content（够拼嵌入文本），按 id 倒序、limit 上限 500。
func (r *MemoryRepo) ListForEmbedding(ctx context.Context, userID int64, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var rows []Memory
	err := r.DB.SelectContext(ctx, &rows, `
SELECT id, user_id, type, title, content, epistemic_type, importance, confidence,
  session_id, conversation_id, transcript_segment_ids, event_at, status, embedding, version, created_at, updated_at
FROM memory WHERE user_id = ? AND status = 'active' AND embedding IS NULL
ORDER BY id DESC LIMIT ?`, userID, limit)
	return rows, err
}

// ListEmbeddedCandidates 返回该用户「active 且已有 embedding」的记忆（含 embedding blob），
// 供检索时在 Go 侧暴力 cosine。按 event_at 倒序取最近 limit 条（MVP 语料量下够用；
// 大语料再上 ANN）。limit 默认/上限 2000。
func (r *MemoryRepo) ListEmbeddedCandidates(ctx context.Context, userID int64, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	var rows []Memory
	err := r.DB.SelectContext(ctx, &rows, `
SELECT * FROM memory WHERE user_id = ? AND status = 'active' AND embedding IS NOT NULL
ORDER BY event_at DESC, id DESC LIMIT ?`, userID, limit)
	return rows, err
}

// SetEmbeddingExt 写入某记忆的向量 blob（ext 传 tx 即入事务；backfill 用 r.DB 逐条提交）。
func (r *MemoryRepo) SetEmbeddingExt(ctx context.Context, ext ExecerContext, id ids.ID, blob []byte) error {
	_, err := ext.ExecContext(ctx, `UPDATE memory SET embedding = ? WHERE id = ?`, blob, id.Int64())
	return err
}
```

- [ ] **Step 7: 跑测试确认通过**：`TEST_MYSQL_DSN=… go test ./internal/repo/ -run MemoryEmbeddingStore -count=1`。

---

## 任务 3 — 打分（纯函数，无 DB）

**Files:** Create `internal/retrieve/score.go`,`internal/retrieve/score_test.go`

- [ ] **Step 1: 失败测试**（cosine / recency / blend 排序）

```go
package retrieve

import (
	"testing"
	"time"
)

func TestCosine(t *testing.T) {
	if v := cosine([]float32{1, 0}, []float32{1, 0}); v < 0.999 {
		t.Errorf("同向应≈1, got %v", v)
	}
	if v := cosine([]float32{1, 0}, []float32{0, 1}); v > 0.001 {
		t.Errorf("正交应≈0, got %v", v)
	}
	if v := cosine([]float32{1, 0}, nil); v != 0 {
		t.Errorf("维度不符/空应 0, got %v", v)
	}
}

func TestRecency(t *testing.T) {
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	today := now
	old := now.AddDate(-1, 0, 0)
	if recencyScore(&today, now) < 0.99 {
		t.Error("当天应≈1")
	}
	if r := recencyScore(&old, now); r > 0.05 {
		t.Errorf("一年前应≈0, got %v", r)
	}
	if recencyScore(nil, now) != 0 {
		t.Error("无时间应 0")
	}
}

func TestBlendRanksSemanticFirst(t *testing.T) {
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	// 高相似但旧 vs 低相似但新+重要：sim 权重 0.5 应让高相似胜出
	a := blend(0.9 /*sim*/, 0 /*kw*/, 0.0 /*recency*/, 0.0 /*imp*/)
	b := blend(0.1, 0, 1.0, 1.0)
	if a <= b {
		t.Errorf("高相似(%.3f) 应 > 低相似高时新(%.3f)", a, b)
	}
	_ = now
}
```

- [ ] **Step 2: 确认失败** → **Step 3: 实现打分**

```go
package retrieve

import (
	"math"
	"time"
)

// 混合打分权重（spec §10；无向量版是 0.5kw+0.3recency+0.2imp，本期加向量项，可调）。
const (
	wSim        = 0.5
	wKw         = 0.2
	wRecency    = 0.15
	wImportance = 0.15
)

// cosine 余弦相似度；维度不符/空/零范数 → 0。
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// recencyScore 线性时新度：clamp(1 - ageDays/365, 0, 1)。at 为 nil → 0。
func recencyScore(at *time.Time, now time.Time) float64 {
	if at == nil {
		return 0
	}
	ageDays := now.Sub(*at).Hours() / 24
	return clamp01(1 - ageDays/365)
}

// blend 混合四项分数（sim 已 clamp 到 [0,1]）。
func blend(sim, kw, recency, importance float64) float64 {
	return wSim*clamp01(sim) + wKw*clamp01(kw) + wRecency*clamp01(recency) + wImportance*clamp01(importance)
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
```

- [ ] **Step 4: 确认通过**：`go test ./internal/retrieve/ -run 'Cosine|Recency|Blend' -v`。

---

## 任务 4 — Retriever：Search + Backfill

**Files:** Create `internal/retrieve/retriever.go`,`internal/retrieve/retriever_test.go`

- [ ] **Step 1: 失败测试**（fake embedder：把文本映射到确定向量；断言语义排序 + backfill 回填）

```go
package retrieve

import (
	"context"
	"strings"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// fakeEmbedder：含关键词则某维为 1，做可控语义。实现 provider.EmbeddingProvider。
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		cat := 0.0
		dog := 0.0
		if strings.Contains(t, "猫") {
			cat = 1
		}
		if strings.Contains(t, "狗") {
			dog = 1
		}
		out[i] = []float32{float32(cat), float32(dog)}
	}
	return out, nil
}

func TestRetrieverBackfillAndSearch(t *testing.T) {
	db := retrieveTestDB(t) // 本包助手：读 TEST_MYSQL_DSN + repo.NewDB，未设则 Skip（见下）
	mem := &repo.MemoryRepo{DB: db}
	ctx := t.Context()
	seed := func(title string) ids.ID {
		m := &repo.Memory{Type: "fact", Title: title, Content: title, EpistemicType: "observed",
			Confidence: 0.8, Importance: 0.5, Status: "active", TranscriptSegmentIDs: ids.List{}}
		_ = mem.InsertExt(ctx, mem.DB, []*repo.Memory{m})
		t.Cleanup(func() { _, _ = mem.DB.Exec("DELETE FROM memory WHERE id=?", m.ID.Int64()) })
		return m.ID
	}
	catID := seed("我养了一只猫RVX")
	_ = seed("邻居有条狗RVX")

	r := &Retriever{Memories: mem, Embedder: fakeEmbedder{}, TopK: 5}
	// backfill 回填两条向量
	n, err := r.Backfill(ctx, 1, 500)
	if err != nil || n < 2 {
		t.Fatalf("backfill n=%d err=%v", n, err)
	}
	// 语义检索「猫」应把 catID 排第一
	got, err := r.Search(ctx, 1, "猫", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].ID != catID {
		t.Fatalf("检索「猫」应 catID 居首, got %+v", got)
	}
}
```

- [ ] **Step 2: 确认失败** → **Step 3: 实现 Retriever**

本包 DSN 助手（放 `retriever_test.go` 顶部；镜像 agent 包 `orchDSN`）：

```go
func retrieveTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	db, err := repo.NewDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}
```
（test import：`os`、`github.com/jmoiron/sqlx`、`zhiwei/internal/repo`。）

```go
package retrieve

import (
	"context"
	"sort"
	"strings"
	"time"

	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// timeNow 便于单测覆盖（recency 用）。
var timeNow = time.Now


// Retriever 语义检索服务：embed query → 已嵌候选暴力 cosine → 混合打分 → topK。
// Embedder 为 nil / query 空 / 无已嵌候选 → Search 返回 nil（调用方退回关键词）。
type Retriever struct {
	Memories *repo.MemoryRepo
	Embedder provider.EmbeddingProvider
	TopK     int // 默认种子条数（context 头用）；Search 的 limit 显式传入
}

// Search 混合召回该用户 active 记忆。typ 非空按 type 过滤。limit<=0 用 TopK。
func (r *Retriever) Search(ctx context.Context, userID int64, query, typ string, limit int) ([]repo.Memory, error) {
	if r == nil || r.Embedder == nil || query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = r.TopK
	}
	qv, err := r.Embedder.Embed(ctx, []string{query})
	if err != nil || len(qv) == 0 {
		return nil, err // 嵌入失败：调用方退回关键词
	}
	cands, err := r.Memories.ListEmbeddedCandidates(ctx, userID, 2000)
	if err != nil {
		return nil, err
	}
	nowT := timeNow()
	type scored struct {
		m repo.Memory
		s float64
	}
	var out []scored
	for _, m := range cands {
		if typ != "" && m.Type != typ {
			continue
		}
		v := DecodeF32(m.Embedding)
		if v == nil {
			continue
		}
		sim := cosine(qv[0], v)
		kw := keywordScore(query, m.Title, m.Content)
		at := m.EventAt
		if at == nil {
			at = &m.CreatedAt
		}
		out = append(out, scored{m, blend(sim, kw, recencyScore(at, nowT), m.Importance)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].s > out[j].s })
	if len(out) > limit {
		out = out[:limit]
	}
	res := make([]repo.Memory, len(out))
	for i := range out {
		res[i] = out[i].m
	}
	return res, nil
}

// Backfill 给该用户 active 且未嵌的记忆回填向量（title+content 拼嵌入文本），逐条 UPDATE。
// 返回成功回填条数。embedder 为 nil → (0,nil)。
func (r *Retriever) Backfill(ctx context.Context, userID int64, limit int) (int, error) {
	if r == nil || r.Embedder == nil {
		return 0, nil
	}
	rows, err := r.Memories.ListForEmbedding(ctx, userID, limit)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	texts := make([]string, len(rows))
	for i, m := range rows {
		texts[i] = embedText(m.Title, m.Content)
	}
	vecs, err := r.Embedder.Embed(ctx, texts)
	if err != nil {
		return 0, err
	}
	n := 0
	for i, m := range rows {
		if i >= len(vecs) || len(vecs[i]) == 0 {
			continue
		}
		if err := r.Memories.SetEmbeddingExt(ctx, r.Memories.DB, m.ID, EncodeF32(vecs[i])); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func embedText(title, content string) string {
	if content == "" {
		return title
	}
	return title + "。" + content
}

// keywordScore 粗粒度关键词命中：query（trim+lower）是否为 title+content（lower）子串 → 1/0。
func keywordScore(query, title, content string) float64 {
	q := normalize(query)
	if q == "" {
		return 0
	}
	if strings.Contains(normalize(title+content), q) {
		return 1
	}
	return 0
}

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
```

`timeNow`（`var timeNow = time.Now`，已在上面 import 块下定义）便于单测覆盖；本任务测试不强依赖时间。`strings`/`time` import 已在块首列出。

- [ ] **Step 4: 确认通过**：`TEST_MYSQL_DSN=… go test ./internal/retrieve/ -count=1 -v`。

---

## 任务 5 — 接入 search_memory（混合优先，关键词兜底）

**Files:** Modify `internal/agent/mcp_server.go`(MCPDeps 定义处)、`internal/agent/mcp_tools.go`；Create/extend `internal/agent/mcp_tools_test.go`

- [ ] **Step 1:** 在 `MCPDeps` 加字段（找到 struct 定义，通常在 `mcp_server.go`）：

```go
// Retrieve 语义检索（可选）：非 nil 且 query 非空时 search_memory 走「向量+关键词」混合，
// 否则退回 Memory.Search 关键词。装配见 main.go（ARK_AUDIO_API_KEY 未配则 nil，降级）。
Retrieve *retrieve.Retriever
```
（`import "zhiwei/internal/retrieve"`。）

- [ ] **Step 2: 失败测试**（fake retriever 命中 → 返回向量序；nil → 关键词序不变）

```go
// 在 mcp_tools_test.go：构造带 fake embedder 的 Retriever，seed+backfill 两条记忆，
// searchMemoryHandler(md) 查询语义词 → 断言首条是语义最相关那条 id。
// 再构造 md.Retrieve=nil → 断言仍走 Memory.Search（关键词命中）不报错。
```
（实现者按 mcp_tools 既有测试风格补；断言 `memoryOut[0].ID` 命中。）

- [ ] **Step 3: 改 searchMemoryHandler**（混合优先，兜底关键词）

```go
func searchMemoryHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, searchMemoryArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a searchMemoryArgs) (*mcp.CallToolResult, any, error) {
		limit := a.Limit
		if limit <= 0 {
			limit = 20
		}
		var ms []repo.Memory
		// 有向量检索且给了 query：优先「向量+关键词」混合；结果为空则退回关键词 LIKE。
		if d.Retrieve != nil && a.Query != "" {
			hit, err := d.Retrieve.Search(ctx, toolUserID, a.Query, a.Type, limit)
			if err == nil && len(hit) > 0 {
				ms = hit
			}
		}
		if ms == nil {
			var err error
			ms, err = d.Memory.Search(ctx, toolUserID, a.Query, a.Type, limit)
			if err != nil {
				return nil, nil, err
			}
		}
		out := make([]memoryOut, 0, len(ms))
		for _, m := range ms {
			out = append(out, memoryOut{ID: m.ID, Type: m.Type, Title: m.Title, Content: m.Content, EventAt: m.EventAt, Importance: m.Importance})
		}
		return jsonResult(out)
	}
}
```
（`import "zhiwei/internal/repo"` 若未有。工具描述串可从「按关键词检索」改为「按语义/关键词检索」。）

- [ ] **Step 4: 确认通过**：`TEST_MYSQL_DSN=… go test ./internal/agent/ -run SearchMemory -count=1`。

---

## 任务 6 — 上下文头 top-k 记忆种子

**Files:** Modify `internal/agent/context.go`、`internal/agent/orchestrator.go`；extend `internal/agent/orchestrator_test.go`

- [ ] **Step 1:** `ProfileContext` 加可选检索依赖（不改既有 `Head` 签名，新增 `Seeds`）：

```go
type ProfileContext struct {
	Persons    *repo.PersonRepo
	Attributes *repo.PersonAttributeRepo
	// Retrieve 可选：非 nil 时按本轮 query 注入 top-k 相关记忆「种子」到上下文头（spec §10）。
	Retrieve *retrieve.Retriever
}

// Seeds 按本轮 query 召回 top-k 相关记忆，拼成上下文头的「相关记忆」块；
// 无 Retrieve / query 空 / 无命中 → ""。每轮一次 query 向量化（成本已知，未配 embedder 时不触发）。
func (pc *ProfileContext) Seeds(ctx context.Context, query string) string {
	if pc == nil || pc.Retrieve == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	ms, err := pc.Retrieve.Search(ctx, toolUserID, query, "", 0) // limit=0 → Retriever.TopK
	if err != nil || len(ms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("可能相关的我的记忆（供参考，不必逐条复述）：")
	for _, m := range ms {
		b.WriteString("\n- " + m.Title)
	}
	return b.String()
}
```
（`import "zhiwei/internal/retrieve"`。）

- [ ] **Step 2:** orchestrator `runTurn` 组装发送文本时追加种子（仍不改落库）：

```go
sent := userText
if o.Ctx != nil {
	var blocks []string
	if h := o.Ctx.Head(ctx, time.Now()); h != "" {
		blocks = append(blocks, h)
	}
	if s := o.Ctx.Seeds(ctx, userText); s != "" {
		blocks = append(blocks, s)
	}
	if len(blocks) > 0 {
		sent = strings.Join(blocks, "\n\n") + "\n\n" + userText
	}
}
```
（`import "strings"`；落库 `um` 与 user 帧仍用原始 `userText`，D2 不变。）

- [ ] **Step 3: 测试**（extend `TestOrchestratorContextInjection` 或新增）：Ctx 装 fake-embedder retriever + seed 记忆 → 断言 `fake.LastText` 含种子标题 + 原始问题；落库 user 消息仍 == 原始。Retrieve=nil → 行为同现状（种子不出现）。

- [ ] **Step 4: 确认通过**：`TEST_MYSQL_DSN=… go test ./internal/agent/ -run 'ContextInjection|Seeds' -count=1`。

---

## 任务 7 — config + main 装配 + backfill sweep

**Files:** Modify `internal/config/config.go`、`internal/config/config_test.go`、`cmd/zhiwei-server/main.go`

- [ ] **Step 1: config 失败测试**（默认值断言）

```go
// config_test.go 追加：默认 EmbedModel=="doubao-embedding-vision"；EmbedBaseURL 默认 /api/plan/v3；
// EmbedAPIKey 读 ARK_AUDIO_API_KEY（未设为空，不报错）。
```

- [ ] **Step 2: config 实现**

```go
// 结构体加：
EmbedAPIKey  string // ARK_AUDIO_API_KEY：向量端点专用 key（≠ ARK_API_KEY；未设→不启用向量）
EmbedBaseURL string // ZW_EMBED_BASE_URL：向量 base，默认 https://ark.cn-beijing.volces.com/api/plan/v3
// Load() 里：
EmbedAPIKey:  os.Getenv("ARK_AUDIO_API_KEY"),
EmbedBaseURL: getenv("ZW_EMBED_BASE_URL", "https://ark.cn-beijing.volces.com/api/plan/v3"),
// 并把 EmbedModel 默认改：
EmbedModel:   getenv("ZW_EMBED_MODEL", "doubao-embedding-vision"),
```

- [ ] **Step 3: main 装配**（在 agent 装配处附近）

```go
// 向量检索（可选）：仅当 ARK_AUDIO_API_KEY 配置时启用；否则 retriever=nil，search_memory/上下文头降级到关键词/无种子。
var retriever *retrieve.Retriever
if cfg.EmbedAPIKey != "" {
	embedder := provider.NewArkVisionEmbed(cfg.EmbedBaseURL, cfg.EmbedAPIKey, cfg.EmbedModel)
	retriever = &retrieve.Retriever{Memories: memoryRepo, Embedder: embedder, TopK: cfg.AgentRetrieveTopK}
}
// 注入：
mcpDeps.Retrieve = retriever            // search_memory 混合
if orch != nil && orch.Ctx != nil {     // 上下文头种子
	orch.Ctx.Retrieve = retriever
}
// backfill sweep：启动后台 goroutine，先立即跑一次、之后每 5 分钟一次（把新记忆补向量）。
if retriever != nil {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			if n, err := retriever.Backfill(context.Background(), 1, 500); err != nil {
				log.Printf("[retrieve] backfill 失败: %v", err)
			} else if n > 0 {
				log.Printf("[retrieve] backfill 回填 %d 条记忆向量", n)
			}
			select {
			case <-rootCtx.Done(): // main 的退出 context（signal.NotifyContext，main.go:208 已有）
				return
			case <-ticker.C:
			}
		}
	}()
}
```
（实现者按 main.go 既有命名接线：`memoryRepo` 换成既有 `*repo.MemoryRepo` 实例名；`rootCtx` 换成 main 里 `signal.NotifyContext` 返回的 ctx 名。）

- [ ] **Step 4: 验证**：`go build ./...` + `go vet ./...` 全绿；`config` 包测试通过。

---

## 验收 & 自检

- **降级安全**：`ARK_AUDIO_API_KEY` 未配 → retriever=nil → `search_memory` 走关键词、上下文头无种子、无 backfill；全链路不报错（每个消费点都判 nil）。
- **不改落库语义**：上下文头种子只进「发给 dsh 的文本」，落库 user 消息/回显仍原始（D2）。
- **apply/写路径无侵入**：向量只在 backfill(UPDATE embedding) 与检索(只读)出现，不进抽取事务、不改 InsertExt/InsertConversationExt。
- **无迁移**：`memory.embedding` 已是 LONGBLOB。
- **契约一致**：provider 实现 `EmbeddingProvider`；`Retriever.Search` 空结果 → 调用方兜底；维度不符/脏 blob → DecodeF32 返回 nil 被跳过。
- **类型一致性自查**：`ListEmbeddedCandidates/ListForEmbedding` 返回 `[]repo.Memory`；`Search` 返回 `[]repo.Memory`；`memoryOut` 映射不变；`Retriever.TopK` ← `cfg.AgentRetrieveTopK`。
- **真连通（集成，非 CI）**：装配后可用仓库根 `.env` 跑一次真 backfill + `search_memory`（隔离库），确认 2048 维写入 + 语义命中。
- 对齐 spec §10（混合检索 + 上下文头种子）、§配置（`ZW_AGENT_RETRIEVE_TOPK` 接上）。
