# P4 报告漫画（E）实现设计

- 日期：2026-08-29
- 总纲：`docs/superpowers/specs/2026-08-29-conversation-insight-roadmap-design.md`（本文件是其 **P4 / E 子项** 的实现 spec）
- 前置：P1/P2/P3 已合入 main（报告有 Narrative/MoodJourney/Patterns/Scenes 深度字段）
- 分支/worktree：`feat/report-comic`

## 1. 目标与范围

在日报/周报里生成并展示 **5-8 格漫画**——基于 P3 报告的叙事（Narrative）+ 情绪走向（MoodJourney）+ 场景（Scenes），用 Seedream 生成场景插图，插入报告渲染。

**核心决策（已与用户确认）**：
- **TOS 长期 URL**：漫画图存自己 TOS（公共读 + 持久 URL 不过期），非 Seedream 的临时 URL，非 base64 存库。
- **报告生成时同步生成**：报告生成流程里串行生成漫画（与报告原子产出）。

## 2. 可行性结论（已 spike 实测，2026-08-29）

- **Seedream 出图**：`doubao-seedream-4-0-250828`（`ARK_API_KEY` + `https://ark.cn-beijing.volces.com/api/v3/images/generations`）实测出图成功（memory [[audio-understanding-and-image-gen-config]]）。
- **⚠️ 不支持 batch**：传 `n=3` 只返回 1 张——Seedream 4.0 **每次调用只出 1 图**。故 5-8 格 = **串行 5-8 次调用**，每次 1 张（慢，5-8 格可能 1-2 分钟）。
- **返回格式**：`response_format=url` 返回 **临时签名 URL**（实测 `X-Tos-Expires=86400`，**24h 过期**，不可直接存库）；`response_format=b64_json` 返回 base64（实测单张 ~273KB）。
- **结论**：Seedream 出图 → 拿图（b64 或下载临时 URL）→ **转存自己 TOS 长期 URL**。b64 最直接（免下载临时 URL）。

## 3. 现状（调研实证，file:line）

- **报告叙事**：`DailyContent.Narrative` / `MoodJourney` / `Patterns` / `Scenes`（P3 已加，`internal/review/types.go`）。漫画分镜基于这些。
- **报告生成**：`review.Generator.Daily/Weekly`（`internal/review/generator.go`）→ gather → prompt → Ark LLM → DailyContent。漫画在此流程里加。
- **TOS 存储**：`internal/storage/tos.go` `UploadWAV`（私有 ACL + 1h presigned，ASR 音频用，**不能改**）。需**新增** `UploadImage`（公共读 ACL + 持久 URL）。
- **前端报告渲染**：`web/app.js` 的 `reportContent` computed + `report-sec`/`report-list`/`report-text` 组件。漫画插这。
- **TOS SDK**：`enum.ACLType` 存在（含 `ACLPrivate`）；公共读常量（通常 `ACLPublicRead = "public-read"`）plan 阶段确认。

## 4. 数据流

```
报告 Narrative/MoodJourney/Scenes（P3 已有）
        ↓ 派生分镜（LLM 或模板：从叙事/情绪点/场景拆 5-8 格，每格一句画面描述）
ComicPanel{prompt, caption} × 5-8
        ↓ 串行 Seedream（每次 1 张，拿 b64）
ComicPanel{image_b64} × 5-8
        ↓ 每张 b64 → 存自己 TOS（UploadImage，公共读 + 持久 URL）
ComicPanel{image_url} × 5-8
        ↓ 存 DailyContent.comic[{caption, image_url}]
前端报告渲染漫画区（网格展示）
```

## 5. 组件设计

**(a) 分镜派生（LLM）**

新增 prompt 或复用：让 Ark LLM（doubao）从 Narrative/MoodJourney/Scenes 派生 5-8 格漫画分镜，每格 `{prompt: 画面描述(英文/中文，适合 Seedream), caption: 中文小标题}`。风格统一指令（如"统一扁平插画风、暖色调"）。

- 派生结果 `[]ComicPanel{{Prompt, Caption}}`。

**(b) Seedream 出图 provider**

`internal/provider/comic.go`：`ComicProvider` 接口 + `SeedreamComic` 实现。
```go
type ComicProvider interface {
	Generate(ctx context.Context, prompt string) (imageB64 string, err error)
}
```
`SeedreamComic.Generate`：调 Seedream `/images/generations`（`response_format=b64_json`），返回 base64。

**(c) 存图 TOS UploadImage**

`internal/storage/tos.go` 新增 `UploadImage(ctx, b64Data, key)`：
- base64 解码 → 写临时文件 → `PutObject`（**公共读 ACL** + `ContentType: image/jpeg`）→ 返回**持久 URL**（`https://{bucket}.{endpoint}/{key}`，非签名、不过期）。
- 与 `UploadWAV` 平行，**不改动 UploadWAV**（ASR 音频保持私有）。

**(d) 漫画编排（Generator）**

`review.Generator.Daily/Weekly`：产出 DailyContent 后，若启用漫画：
- 派生分镜（LLM，5-8 格）；
- 串行 Seedream 出图（每格 b64）；
- 每张存 TOS（长期 URL）；
- 填 `DailyContent.Comic []ComicPanel{{Caption, ImageURL}}`。

**开关 + 降级**：`ZW_COMIC_ENABLED`（默认可关）；任一格失败跳过该格（其余照常）；整体失败则 `Comic` 为空（报告照常，只没漫画）。

## 6. 数据模型

**DailyContent / WeeklyContent 增列**：
```go
type ComicPanel struct {
	Caption  string `json:"caption"`   // 中文小标题
	ImageURL string `json:"image_url"` // TOS 长期 URL
}
// DailyContent / WeeklyContent 加：
	Comic []ComicPanel `json:"comic"` // 报告漫画 5-8 格（可空）
```
**无新迁移**（DailyContent/WeeklyContent 是 JSON 落库，加字段即可）。

## 7. 配置

- `ComicEnabled bool`（`ZW_COMIC_ENABLED`，默认 false——文生图贵 + 慢，默认关，用户主动开）。
- `ComicModel string`（`ZW_COMIC_MODEL`，默认 `doubao-seedream-4-0-250828`）。
- `ComicPanelCount int`（`ZW_COMIC_PANELS`，默认 6，5-8 范围）。
- `ComicAPIKey` 复用 `ARK_API_KEY`（Seedream 与 LLM 同 Ark 账号）。
- main.go：构造 `SeedreamComic` + `UploadImage` 依赖注入 Generator。

## 8. 前端渲染

**报告渲染区**（`web/app.js` 的 reportContent）增列漫画区：
```html
<div class="report-sec" v-if="reportContent.comic && reportContent.comic.length">
  <div class="report-sec-title">今日漫画</div>
  <div class="comic-grid">
    <figure v-for="(p, i) in reportContent.comic" :key="i" class="comic-panel">
      <img :src="p.image_url" :alt="p.caption" loading="lazy" />
      <figcaption>{{ p.caption }}</figcaption>
    </figure>
  </div>
</div>
```
+ CSS（`.comic-grid` 网格布局、`.comic-panel` 圆角阴影）。

## 9. 测试

- **provider**（`comic_test.go`）：fake Seedream HTTP 桩，验 b64 返回解析、错误路径。
- **UploadImage**（`tos_test.go`）：base64 → 上传 → 返回持久 URL（格式对、非签名）。
- **漫画派生 + 编排**（review 测试）：fake ComicProvider + fake LLM，验分镜派生 → 串行出图 → 存 TOS → DailyContent.Comic 填充、开关关闭跳过、单格失败跳过。
- **前端**：`node --check web/app.js` + 漫画区渲染（冒烟）。

## 10. 已定决策（不留 TBD）

| 项 | 决策 |
|----|------|
| 图片存储 | 自己 TOS 公共读 + 长期 URL（规避 Seedream 24h URL + base64 过大） |
| 生成时机 | 报告生成时同步串行生成 |
| 批量 | Seedream 不支持 batch → 串行 N 次 × 1 张 |
| 拿图 | response_format=b64_json（免下载临时 URL） |
| 分镜 | LLM 从 Narrative/MoodJourney/Scenes 派生 5-8 格 |
| 开关 | ZW_COMIC_ENABLED 默认 false（贵+慢，主动开） |
| 降级 | 单格失败跳过、整体失败 Comic 空、报告照常 |
| 落库 | DailyContent/WeeklyContent.Comic[]（JSON，无新迁移） |

## 11. 待 plan 阶段决定（非阻塞）

- UploadImage 的公共读 ACL 常量名（enum.ACLPublicRead 待确认）+ 持久 URL 拼接格式。
- 分镜派生的 prompt 措辞（风格统一、画面描述中英）。
- 串行 5-8 次 Seedream 的时延（可能 1-2min）——可接受（报告低频）或后续优化并发。
- 漫画区前端网格布局细节。

## 12. 风险

- **成本**：文生图贵 + 5-8 次调用——默认关缓解，主动开才产生。
- **时延**：串行 5-8 次可能 1-2min——报告低频可接受，或后续并发优化。
- **TOS 公共读**：漫画图公共可访问——非敏感内容可接受；若需私有则改回 presigned + 长期签名。
- **风格一致性**：Seedream 每格独立出图，跨格人物/风格可能不一致——靠分镜 prompt 统一风格描述缓解（Seedream 4 无多格一致性保证）。
- **LLM 分镜质量**：派生分镜的 prompt 质量影响出图——靠 prompt 工程 + 人工抽验。
