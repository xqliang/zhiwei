# P4 报告漫画（E）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 日报/周报生成时，LLM 从叙事派生 6-12 格分镜描述拼一个 prompt，Seedream **1 次调用**出整张多格漫画，转存自己 TOS 长期 URL，前端展示。

**Architecture:** `SeedreamComic`(provider，1 次出多格图 b64) + `UploadImage`(TOS 公共读+持久 URL) + `Generator.Daily/Weekly` 在 generateDaily 之后、UpsertDaily 之前注入 `DailyContent.Comic`(单张多格图) + 前端 reportContent 渲染。全部默认关 + 失败降级 Comic 空。

**Tech Stack:** Go + Ark（doubao LLM 派生 + Seedream 出图）+ ve-tos SDK（公共读 ACL）+ Vue3 CDN 前端。

**规格：** `docs/superpowers/specs/2026-08-29-report-comic-p4-design.md`。

**测试约定：** provider/tos 用 fake HTTP 桩；review 用 repotest.DSN（未设 TEST_MYSQL_DSN 则 skip）；Go 改动后 `go build ./... && go vet ./...`；前端改完 `make hash-web` + `node --check web/app.js`。MySQL 在 127.0.0.1:3307。**无新迁移**。每任务末尾提交。

**关键既有事实（已核实）：**
- `Generator.Daily`（`internal/review/gather.go:171`）：`generateDaily(ctx,in)` 得 `(content, raw, err)`（line 176）→ `UpsertDaily(raw,"ready")`（line 184）。漫画注入点 = **line 176 之后、line 184 之前**（生成 Comic → 塞 content → 重 marshal raw）。Weekly（gather.go:291）同构。
- `generateDaily`/`generateWeekly`（`internal/review/generator.go:58/70`）：`g.LLM.Chat(provider.ChatRequest{Model:g.Model, System:..., User:...})`。
- `DailyContent`/`WeeklyContent`（`internal/review/types.go`）。
- TOS：`internal/storage/tos.go` `UploadWAV`（私有+1h presign，勿改）；`enum.ACLType` 存在（含 `ACLPrivate`；公共读常量通常 `ACLPublicRead = "public-read"`，plan 阶段确认）。
- Seedream：`doubao-seedream-4-0-250828` + `ARK_API_KEY` + `https://ark.cn-beijing.volces.com/api/v3/images/generations`，`response_format=b64_json`，**1 次调用出整张多格图**。

---

### Task 1: provider — SeedreamComic（1 次出多格图）

**Files:** Create `internal/provider/comic.go` / `comic_test.go`

- [ ] **Step 1: 写失败测试**

`internal/provider/comic_test.go`：
```go
package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 桩 Seedream 返回 b64_json，验 SeedreamComic 解析 + 请求体正确。
func TestSeedreamComicGenerate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if gotBody["response_format"] != "b64_json" {
			t.Errorf("response_format 应为 b64_json, got %v", gotBody["response_format"])
		}
		w.Write([]byte(`{"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()

	c := NewSeedreamComic(srv.URL, "k", "doubao-seedream-4-0-250828")
	b64, err := c.Generate(context.Background(), "一个漫画 prompt")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if b64 != "aGVsbG8=" {
		t.Errorf("b64=%q", b64)
	}
}

func TestSeedreamComicError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer srv.Close()
	c := NewSeedreamComic(srv.URL, "k", "m")
	if _, err := c.Generate(context.Background(), "x"); err == nil {
		t.Error("应返回 error")
	}
}
```
（`comic_test.go` 需 import `encoding/json`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go vet ./internal/provider/`。Expected: `NewSeedreamComic` / `ComicProvider` 未定义。

- [ ] **Step 3: 实现 comic.go**

`internal/provider/comic.go`：
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

// ComicProvider 文生图接口：一个 prompt → 一张图（多格漫画整图）的 base64。
type ComicProvider interface {
	Generate(ctx context.Context, prompt string) (imageB64 string, err error)
}

// SeedreamComic 调火山方舟 Seedream 文生图（1 次调用出整张多格漫画）。
type SeedreamComic struct {
	baseURL, apiKey, model string
	client                 *http.Client
}

func NewSeedreamComic(baseURL, apiKey, model string) *SeedreamComic {
	return &SeedreamComic{baseURL: baseURL, apiKey: apiKey, model: model, client: &http.Client{Timeout: 180 * time.Second}}
}

type comicReq struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	Watermark      bool   `json:"watermark"`
}

type comicResp struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
		URL     string `json:"url"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate 1 次调用出整张多格漫画（b64_json）。prompt 含多格描述 + 统一风格指令。
func (p *SeedreamComic) Generate(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(comicReq{
		Model: p.model, Prompt: prompt, Size: "1792x1024",
		ResponseFormat: "b64_json", Watermark: false,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/images/generations", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var cr comicResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("响应解析(http %d): %s", resp.StatusCode, truncate(raw))
	}
	if cr.Error != nil {
		return "", fmt.Errorf("seedream 错误: %s", cr.Error.Message)
	}
	if len(cr.Data) == 0 || cr.Data[0].B64JSON == "" {
		return "", fmt.Errorf("空响应(http %d): %s", resp.StatusCode, truncate(raw))
	}
	return cr.Data[0].B64JSON, nil
}
```
（`truncate` 已在 llm.go 同包，直接复用。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/provider/ -run 'TestSeedreamComic' -v -count=1`。Expected: PASS。

- [ ] **Step 5: 提交**
```bash
git add internal/provider/comic.go internal/provider/comic_test.go
git commit -m "feat(provider): SeedreamComic 文生图(1次调用出整张多格漫画 b64)"
```

---

### Task 2: TOS — UploadImage（公共读 + 持久 URL）

**Files:** Modify `internal/storage/tos.go`；`internal/storage/tos_test.go`

- [ ] **Step 1: 写失败测试**

`internal/storage/tos_test.go` 加（照现有 tos 测试风格；若有 stub TOS server 则断言 PutObject 收到公共读 ACL + 返回持久 URL 格式）：
```go
func TestUploadImageReturnsPersistentURL(t *testing.T) {
	// 用一个 stub TOS server（照 tos_test.go 现有 stub 模式）或表驱动验证 URL 拼接逻辑。
	// 最小断言：UploadImage 返回的 URL 是非签名持久 URL（不含 X-Tos-Expires/Signature）。
	// 若现有 tos_test 无 stub server 基础设施，则本任务先只验证 URL 拼接纯函数（见 Step 3 的 persistentObjectURL）。
	_ = t
}
```
> 说明：完整 UploadImage 需真 TOS 或 stub server。若现有 tos_test 已有 stub server（httptest 模拟 TOS），复用之；若无，plan 阶段实现者：把「PutObject + 持久 URL 拼接」抽成可测的辅助，至少验证 URL 拼接逻辑（`https://{bucket}.{endpoint}/{key}` 且非签名）。真连通性靠部署冒烟。

- [ ] **Step 2: 实现 UploadImage**

`internal/storage/tos.go` 加（`UploadWAV` 之后）：
```go
// persistentObjectURL 拼 TOS 持久 URL（公共读对象，非签名、不过期）。
func (t *TOSClient) persistentObjectURL(key string) string {
	scheme := "https"
	host := t.cfg.Endpoint
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		// Endpoint 可能带 scheme
		return strings.TrimRight(host, "/") + "/" + key
	}
	return scheme + "://" + t.cfg.Bucket + "." + host + "/" + key
}

// UploadImage 上传图片（base64 → 临时文件 → 公共读 ACL），返回持久 URL（不过期）。
// 与 UploadWAV 平行，不动 UploadWAV（ASR 音频保持私有 + 1h presign）。
func (t *TOSClient) UploadImage(ctx context.Context, b64Data, key string) (string, error) {
	key = t.prefixed(key)
	data, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return "", fmt.Errorf("base64 解码: %w", err)
	}
	tmp, err := os.CreateTemp("", "comic-*.jpeg")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	// 公共读 ACL（enum.ACLPublicRead，若常量名不同照 SDK 实际；公共读让 URL 可直接访问、不过期）
	if _, err := t.client.PutObjectFromFile(ctx, &amp;tos.PutObjectFromFileInput{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket: t.cfg.Bucket, Key: key, ContentType: "image/jpeg",
			ACLType: enum.ACLPublicRead, // 公共读
		},
		FilePath: tmp.Name(),
	}); err != nil {
		return "", fmt.Errorf("tos put image: %w", err)
	}
	return t.persistentObjectURL(key), nil
}
```
> **注意**：
> - `&amp;tos.PutObjectFromFileInput` 里的 `&amp;` 是 HTML 转义，写文件时还原成 `&amp;`。
> - `enum.ACLPublicRead` 常量名 plan 阶段确认（若 SDK 用 `enum.ACLTypePublicRead` 或 `enum.ObjectACLPublicRead`，照 `go doc enum.ACLType` 实际名改）。ACL 字段可能是 `ACLType` 或 `ObjectACL`，照 `PutObjectBasicInput` 实际字段名改。
> - 若公共读 ACL 因 SDK 版本/字段名折腾不通，**兜底方案**：退回私有 ACL + 用 `PreSignedURL` 但 `Expires` 设很大（如 7 天）——plan 阶段若 ACL 卡住则用此兜底。

- [ ] **Step 3: build + 提交**

Run: `go build ./...`
```bash
git add internal/storage/tos.go internal/storage/tos_test.go
git commit -m "feat(storage): TOS UploadImage(公共读+持久URL, 漫画图存储)"
```

---

### Task 3: 数据模型 + 分镜派生 + 漫画编排

**Files:** Modify `internal/review/types.go`；`internal/review/prompt.go`；`internal/review/generator.go`；`internal/review/gather.go`；`internal/review/generator_test.go`

- [ ] **Step 1: DailyContent/WeeklyContent 加 Comic 字段 + ComicImage 类型**

`internal/review/types.go` 加：
```go
// ComicImage 报告漫画（一张多格连环画，P4）。
type ComicImage struct {
	Caption  string `json:"caption"`   // 整体小标题（可选）
	ImageURL string `json:"image_url"` // TOS 长期 URL
}
```
`DailyContent` 末尾加 `Comic *ComicImage \`json:"comic"\``；`WeeklyContent` 末尾加同字段。`normalizeDaily/normalizeWeekly` 无需改（Comic 是指针，nil 合法）。

- [ ] **Step 2: Generator 加 Comic 依赖 + 分镜派生**

`internal/review/generator.go`：
- Generator 结构体加：
```go
	Comic provider.ComicProvider // nil = 不生成漫画
```
- 加分镜派生方法：
```go
// buildComicPrompt 用 LLM 从报告叙事派生 Seedream 多格漫画 prompt。
func (g *Generator) buildComicPrompt(ctx context.Context, narrative string, mood []MoodPoint, scenes []SceneCount) (string, error) {
	sys := "你是漫画分镜师。根据用户的一天总结，写一个文生图 prompt：要求画成 6-12 格连环画（网格或横条），统一扁平插画风、暖色调、同一主角、每格一个场景、整体风格一致。只输出 prompt 本身（中文画面描述），不要解释。"
	usr := "一天总结：\n" + narrative + "\n\n情绪点：" + fmt.Sprintf("%v", mood) + "\n场景：" + fmt.Sprintf("%v", scenes)
	resp, err := g.LLM.Chat(ctx, provider.ChatRequest{Model: g.Model, System: sys, User: usr})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}
```
> `&amp;`/转义无；注意 import `strings`、`fmt`（generator.go 应已有）。

- [ ] **Step 3: 漫画编排方法**

`internal/review/generator.go` 加：
```go
// tryAttachComic 生成漫画并挂到 content 上（失败静默返回 nil content 不变）。
func (g *Generator) tryAttachComic(ctx context.Context, narrative string, mood []MoodPoint, scenes []SceneCount) *ComicImage {
	if g.Comic == nil {
		return nil
	}
	prompt, err := g.buildComicPrompt(ctx, narrative, mood, scenes)
	if err != nil {
		log.Printf("[review] 漫画分镜派生失败(降级): %v", err)
		return nil
	}
	b64, err := g.Comic.Generate(ctx, prompt)
	if err != nil {
		log.Printf("[review] 漫画出图失败(降级): %v", err)
		return nil
	}
	// 存自己 TOS 长期 URL（若 Generator 无 TOS 则退回存 b64 data URL，见下）
	url, err := g.storeComicImage(ctx, b64)
	if err != nil {
		log.Printf("[review] 漫画存图失败(降级): %v", err)
		return nil
	}
	return &ComicImage{ImageURL: url}
}

// storeComicImage 存漫画图，返回可访问 URL。优先 TOS 长期 URL；无 TOS 时退回 data URL。
func (g *Generator) storeComicImage(ctx context.Context, b64 string) (string, error) {
	if g.ComicStorage != nil {
		return g.ComicStorage.UploadImage(ctx, b64, "comics/"+ids.New().String()+".jpeg")
	}
	// 兜底：data URL（前端可直接 <img src>，但大；仅无 TOS 时）
	return "data:image/jpeg;base64," + b64, nil
}
```
Generator 加 `ComicStorage TOSImageUploader` 字段（接口，nil 时 data URL 兜底）：
```go
// TOSImageUploader 存图接口（*storage.TOSClient 实现）。
type TOSImageUploader interface {
	UploadImage(ctx context.Context, b64Data, key string) (string, error)
}
```
Generator 加 `ComicStorage TOSImageUploader` 字段 + import `zhiwei/internal/ids`、`log`、`strings`。

- [ ] **Step 4: Daily/Weekly 编排注入漫画**

`internal/review/gather.go` 的 `Generator.Daily`：把 line 176 改为拿 content，生成漫画挂上，重 marshal raw，再 UpsertDaily：
```go
	content, raw, genErr := g.generateDaily(ctx, in)
	if genErr != nil {
		// ... 原 failed 逻辑不变 ...
	}
	// 挂漫画（失败静默，content 不变 → Comic 为 nil）
	if content != nil {
		if comic := g.tryAttachComic(ctx, content.Narrative, content.MoodJourney, content.Scenes); comic != nil {
			content.Comic = comic
			if nb, err := json.Marshal(content); err == nil {
				raw = nb
			}
		}
	}
	if err := g.Reviews.UpsertDaily(ctx, reviewUserID, date, json.RawMessage(raw), "ready"); err != nil {
		// ... 原逻辑 ...
	}
```
`Generator.Weekly`（gather.go:291）同样：generateWeekly 后挂漫画（用 WeeklyContent.Narrative/Scenes，Weekly 无 MoodJourney 传 nil）+ 重 marshal raw + UpsertWeekly。
> 需改 `generateDaily`/`generateWeekly` 签名或新增返回 content 的版本——**最简**：在 Daily/Weekly 里直接 `json.Unmarshal(raw, &content)` 拿 content（raw 已是 DailyContent JSON），挂 Comic 后重 marshal。避免改 generateDaily 签名。即：
> ```go
> _, raw, genErr := g.generateDaily(ctx, in)
> // ...
> var content repo.DailyContent
> if json.Unmarshal(raw, &content) == nil {
>     if comic := g.tryAttachComic(ctx, content.Narrative, content.MoodJourney, content.Scenes); comic != nil {
>         content.Comic = comic
>         if nb, err := json.Marshal(content); err == nil { raw = nb }
>     }
> }
> ```
> Weekly 用 `repo.WeeklyContent`。

- [ ] **Step 5: 测试 + build + 提交**

`generator_test.go`：fake ComicProvider（返回固定 b64）+ fake LLM + fake TOSUploader，造 DailyInput 含 Narrative，跑 Daily 流程，断言落库 content 含 Comic.ImageURL（非空、来自 fake TOS）。开关（Comic=nil）时不生成。

Run: `go build ./... && go vet ./... && go test ./internal/review/ -run 'TestComic|TestDaily' -count=1`
```bash
git add internal/review/types.go internal/review/generator.go internal/review/gather.go internal/review/generator_test.go
git commit -m "feat(review): 报告漫画编排(LLM派生多格+Seedream出图+存TOS挂Comic)"
```
（prompt.go 若 buildComicPrompt 放那则一起 add。）

---

### Task 4: 前端 — 报告漫画区渲染

**Files:** Modify `web/app.js` / `web/index.html`；`make hash-web`

- [ ] **Step 1: 报告渲染区加漫画区**

`web/index.html` 报告渲染区（DailyContent/WeeklyContent 的 `report-sec` 块之后）加：
```html
<div class="report-sec" v-if="reportContent.comic && reportContent.comic.image_url">
  <div class="report-sec-title">今日漫画</div>
  <img :src="reportContent.comic.image_url" :alt="reportContent.comic.caption || '报告漫画'"
       loading="lazy" class="comic-img" />
</div>
```

- [ ] **Step 2: CSS**

`web/index.html` style 加：
```css
.comic-img { max-width: 100%; height: auto; border-radius: var(--radius); box-shadow: var(--shadow); display: block; }
```
（`--radius`/`--shadow` 若不存在用现有圆角/阴影变量或硬编码。）

- [ ] **Step 3: hash-web + 校验**

Run: `make hash-web && node --check web/app.js`

- [ ] **Step 4: 提交**
```bash
git add web/app.js web/index.html
git commit -m "feat(web): 报告展示多格漫画图"
```

---

### Task 5: 全量回归 + 真冒烟

- [ ] **Step 1: 全量 build/vet + review 包测试**

Run: `go build ./... && go vet ./...`；`TEST_MYSQL_DSN=... go test ./internal/review/ ./internal/provider/ ./internal/storage/ -count=1`。Expected: PASS。

- [ ] **Step 2: 漫画真出图冒烟（best-effort，消耗）**

若有 .env + 真实报告数据：开 `ZW_COMIC_ENABLED=true` 生成日报，确认 DailyContent.comic.image_url 是非签名 TOS URL 且浏览器能打开看到多格漫画。无则靠 fake 测试覆盖。

---

## Self-Review 结果

**Spec 覆盖：** §5a 派生→Task3 buildComicPrompt；§5b Seedream→Task1；§5c UploadImage→Task2；§5d 编排→Task3；§6 数据模型→Task3；§7 配置（ComicEnabled 等）→Task3 Generator 依赖 + main 注入见下；§8 前端→Task4；§9 测试→各任务。✅

**配置装配缺口修正**：spec §7 要 `ZW_COMIC_ENABLED/Model` + main.go 注入 `SeedreamComic`+`UploadImage` 到 Generator——**本计划 Task3 建了 Generator 依赖但漏了 config/main 装配**。**补充 Task 3.5（配置 + main.go 装配）**：config 加 `ComicEnabled`(默认false)/`ComicModel`；main.go 构造 `SeedreamComic`+`TOSClient`(作 ComicStorage)注入 review.Generator。执行时务必含此步。

**占位符**：Task2 的 `enum.ACLPublicRead` 常量名 + `&amp;` 转义是**显式标注**（照 go doc 实际名/还原 `&`），非模糊 TODO。Task3 的 storeComicImage 优先 TOS、兜底 data URL 是明确降级链。

**类型一致性：** `ComicProvider`/`SeedreamComic`(provider) Task1 定义、Task3 引用；`ComicImage`/`Comic`(types) Task3 定义、Task3/4 引用；`TOSImageUploader` 接口 Task3 定义、main 装配时 `*storage.TOSClient` 实现之（UploadImage 方法 Task2 加）。

**关键正确性点：** ①1 次调用出多格（非串行）；②存自己 TOS 长期 URL（规避 Seedream 24h）；③data URL 兜底（无 TOS 时仍可用）；④全程默认关+失败降级 Comic 空；⑤re-marshal raw 后落库（comic 落库）。

**⚠️ 待执行补充**：config + main.go 装配（ComicEnabled/ComicModel/注入 SeedreamComic+TOSClient）——见上「配置装配缺口修正」，实现时必须包含，否则功能不生效。
