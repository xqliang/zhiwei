# P4 报告漫画（E）实现设计

- 日期：2026-08-29
- 总纲：`docs/superpowers/specs/2026-08-29-conversation-insight-roadmap-design.md`（本文件是其 **P4 / E 子项** 的实现 spec）
- 前置：P1/P2/P3 已合入 main（报告有 Narrative/MoodJourney/Patterns/Scenes 深度字段）
- 分支/worktree：`feat/report-comic`

## 1. 目标与范围

在日报/周报里生成并展示 **6-12 格漫画**——基于 P3 报告的叙事（Narrative）+ 情绪走向（MoodJourney）+ 场景（Scenes），用 Seedream 生成场景插图，插入报告渲染。

**核心决策（已与用户确认）**：
- **TOS 长期 URL**：漫画图存自己 TOS（公共读 + 持久 URL 不过期），非 Seedream 的临时 URL，非 base64 存库。
- **报告生成时同步生成**：报告生成流程里生成漫画（与报告原子产出）。
- **单张多格漫画（1 次调用）**：LLM 派生 6-12 格分镜描述拼进一个 prompt，Seedream **1 次调用出整张多格漫画**（非串行 N 次单格）——快、且风格天然统一。

## 2. 可行性结论（已 spike 实测，2026-08-29）

- **Seedream 出图**：`doubao-seedream-4-0-250828`（`ARK_API_KEY` + `https://ark.cn-beijing.volces.com/api/v3/images/generations`）实测出图成功（memory [[audio-understanding-and-image-gen-config]]）。
- **✅ 支持单张图内多格连环画（关键）**：实测给「横向 4 格连环画」「3x3 共 9 格漫画网格」提示词，Seedream **1 次调用出 1 张图，图内多格清晰、风格高度统一（同一主角、同插画风）**。故 6-12 格 = **1 次调用**（非串行）。
- **⚠️ 不支持 `n` batch**：传 `n=3` 只返回 1 张——但那正是我们要的（1 张多格图）。
- **返回格式**：`response_format=url` 返回 **临时签名 URL**（实测 `X-Tos-Expires=86400`，**24h 过期**，不可直接存库）；`response_format=b64_json` 返回 base64（多格大图 ~600KB-1MB）。
- **结论**：Seedream **1 次调用**出整张多格漫画（b64）→ **转存自己 TOS 长期 URL**（b64 免下载临时 URL）。

## 3. 现状（调研实证，file:line）

- **报告叙事**：`DailyContent.Narrative` / `MoodJourney` / `Patterns` / `Scenes`（P3 已加，`internal/review/types.go`）。漫画分镜基于这些。
- **报告生成**：`review.Generator.Daily/Weekly`（`internal/review/generator.go`）→ gather → prompt → Ark LLM → DailyContent。漫画在此流程里加。
- **TOS 存储**：`internal/storage/tos.go` `UploadWAV`（私有 ACL + 1h presigned，ASR 音频用，**不能改**）。需**新增** `UploadImage`（公共读 ACL + 持久 URL）。
- **前端报告渲染**：`web/app.js` 的 `reportContent` computed + `report-sec`/`report-list`/`report-text` 组件。漫画插这。
- **TOS SDK**：`enum.ACLType` 存在（含 `ACLPrivate`）；公共读常量（通常 `ACLPublicRead = "public-read"`）plan 阶段确认。

## 4. 数据流

```
报告 Narrative/MoodJourney/Scenes（P3 已有）
        ↓ 派生多格分镜（LLM：从叙事/情绪点/场景拆 6-12 格，拼进【一个】prompt，
        │   每格一句画面描述 + 统一风格指令，如「6-12 格连环画/网格、统一扁平插画风、暖色调、同一主角」）
一个完整漫画 prompt（含全部 6-12 格描述）
        ↓ Seedream 【1 次调用】（response_format=b64_json，出一整张多格漫画图）
漫画大图 b64（~600KB-1MB）
        ↓ 转存自己 TOS（UploadImage：公共读 ACL + 持久 URL）
漫画长期 URL（1 个）
        ↓ 存 DailyContent.comic{ image_url, caption(整体小标题) }
前端报告渲染漫画区（展示 1 张多格漫画大图）
```

## 5. 组件设计

**(a) 多格分镜派生（LLM，拼一个 prompt）**

用 Ark LLM（doubao）从 Narrative/MoodJourney/Scenes 派生 6-12 格画面描述，拼进**一个** Seedream prompt：每格一句画面 + 统一风格指令（如「6-12 格连环画、统一扁平插画风、暖色调、同一主角、每格留白分隔」）。

- 产物：一个完整漫画 prompt 字符串（LLM 直接输出可直接喂 Seedream 的 prompt，或输出结构化分镜后模板拼装）。

**(b) Seedream 出图 provider**

`internal/provider/comic.go`：`ComicProvider` 接口 + `SeedreamComic` 实现。
```go
type ComicProvider interface {
	Generate(ctx context.Context, prompt string) (imageB64 string, err error)
}
```
`SeedreamComic.Generate`：**1 次调用** Seedream `/images/generations`（`response_format=b64_json`，size 用横向大图如 `1792x1024` 适配多格），返回整张多格漫画的 base64。

**(c) 存图 TOS UploadImage**

`internal/storage/tos.go` 新增 `UploadImage(ctx, b64Data, key)`：
- base64 解码 → 写临时文件 → `PutObject`（**公共读 ACL** + `ContentType: image/jpeg`）→ 返回**持久 URL**（`https://{bucket}.{endpoint}/{key}`，非签名、不过期）。
- 与 `UploadWAV` 平行，**不改动 UploadWAV**（ASR 音频保持私有）。

**(d) 漫画编排（Generator）**

`review.Generator.Daily/Weekly`：产出 DailyContent 后，若启用漫画：
- 派生多格 prompt（LLM，6-12 格）；
- Seedream **1 次**出整张多格漫画（b64）；
- 转存自己 TOS（长期 URL）；
- 填 `DailyContent.Comic`（1 张多格漫画图）。

**开关 + 降级**：`ZW_COMIC_ENABLED`（默认可关）；派生或出图或存图任一失败则 `Comic` 为空（报告照常，只没漫画），只记日志。

## 6. 数据模型

**DailyContent / WeeklyContent 增列**：
```go
type ComicImage struct {
	Caption  string `json:"caption"`   // 整体小标题（可选）
	ImageURL string `json:"image_url"` // TOS 长期 URL（一张多格漫画大图）
}
// DailyContent / WeeklyContent 加：
	Comic *ComicImage `json:"comic"` // 报告漫画（一张 6-12 格连环画，可空）
```
**无新迁移**（DailyContent/WeeklyContent 是 JSON 落库，加字段即可）。

## 7. 配置

- `ComicEnabled bool`（`ZW_COMIC_ENABLED`，默认 false——文生图贵 + 慢，默认关，用户主动开）。
- `ComicModel string`（`ZW_COMIC_MODEL`，默认 `doubao-seedream-4-0-250828`）。
- `ComicPanelCount int`（`ZW_COMIC_PANELS`，默认 8，6-12 范围）。
- `ComicAPIKey` 复用 `ARK_API_KEY`（Seedream 与 LLM 同 Ark 账号）。
- main.go：构造 `SeedreamComic` + `UploadImage` 依赖注入 Generator。

## 8. 前端渲染

**报告渲染区**（`web/app.js` 的 reportContent）增列漫画区（展示一张多格漫画大图）：
```html
<div class="report-sec" v-if="reportContent.comic && reportContent.comic.image_url">
  <div class="report-sec-title">今日漫画</div>
  <img :src="reportContent.comic.image_url" :alt="reportContent.comic.caption"
       loading="lazy" class="comic-img" />
</div>
```
+ CSS（`.comic-img` 自适应宽度、圆角阴影）。

## 9. 测试

- **provider**（`comic_test.go`）：fake Seedream HTTP 桩，验 b64 返回解析、错误路径。
- **UploadImage**（`tos_test.go`）：base64 → 上传 → 返回持久 URL（格式对、非签名）。
- **漫画派生 + 编排**（review 测试）：fake ComicProvider + fake LLM，验多格 prompt 派生 → 1 次出图 → 存 TOS → DailyContent.Comic 填充、开关关闭跳过、失败降级 Comic 空。
- **前端**：`node --check web/app.js` + 漫画区渲染（冒烟）。

## 10. 已定决策（不留 TBD）

| 项 | 决策 |
|----|------|
| 图片存储 | 自己 TOS 公共读 + 长期 URL（规避 Seedream 24h 临时 URL） |
| 生成时机 | 报告生成时同步生成（1 次调用） |
| 出图方式 | Seedream **1 次调用出整张 6-12 格多格漫画**（单图内多格，非串行单格；spike 实测可行） |
| 拿图 | response_format=b64_json（免下载临时 URL） |
| 分镜 | LLM 从 Narrative/MoodJourney/Scenes 派生 6-12 格，拼进一个 prompt |
| 开关 | ZW_COMIC_ENABLED 默认 false（文生图贵，主动开） |
| 降级 | 派生/出图/存图任一失败 Comic 空、报告照常 |
| 落库 | DailyContent/WeeklyContent.Comic（单张多格图，JSON，无新迁移） |

## 11. 待 plan 阶段决定（非阻塞）

- UploadImage 的公共读 ACL 常量名（enum.ACLPublicRead 待确认）+ 持久 URL 拼接格式。
- 分镜 prompt 措辞（格数范围、风格统一、单图内多格排版描述如连环画/网格）。
- 单图尺寸（如 1792x1024 横版适配多格；格数越多每格越小，6 vs 12 格可读性权衡）。
- 漫画前端展示细节（自适应宽度）。

## 12. 风险

- **成本**：文生图贵——默认关缓解，主动开才产生；且仅 1 次调用（非 N 次）。
- **时延**：单次 Seedream 出大图约数秒~十几秒——报告低频可接受。
- **TOS 公共读**：漫画图公共可访问——非敏感内容可接受；若需私有则改回 presigned + 长期签名。
- **多格可读性**：12 格在单图内每格较小——用大图尺寸 + 格数上限权衡；实测 9 格仍清晰。
- **LLM 分镜质量**：派生多格 prompt 质量影响出图——靠 prompt 工程 + 人工抽验。
