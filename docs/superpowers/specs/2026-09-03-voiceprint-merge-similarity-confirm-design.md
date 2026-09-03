# 声纹合并相似度预检 + 二次确认 设计

- 日期：2026-09-03
- 分支/worktree：`feat/voiceprint-merge-similarity-confirm`（`.worktrees/voiceprint-merge-sim`）
- 基线：本地 `main` @ `431ba3b`（含未合 origin/main 的合并写侧级联改动；`431ba3b` 改了 `internal/api/speaker.go` 与 `internal/repo/speaker.go`，故基线必须含它，否则两条分支改同两文件 → 合 main 必冲突）
- 关联规格：`2026-08-31-speaker-cascade-person-design.md`（合并写侧级联）、`2026-08-28-per-segment-voiceprint-reattribution-design.md`（相似度语义）

## 1. 目标与已定决策

声纹页「手动合并」在真正发请求前，多一道**相似度预检 + 二次确认**：用户勾选 ≥2 个说话人、选好保留目标、点「确认合并」时，后端返回这 N 个声纹**两两之间**的相似度，前端原地展开成表，用户看清后再点一次才执行合并。

把 memory 里那条教训从「靠人记得」变成「系统强制摊开」：

> **教训：手动 merge 声纹前先看两声纹样本互似度；挤团库（铉晔/文生/杰辉 0.74~0.84）里 merge 极易引入他声。**

| 项 | 决策 |
|----|------|
| 相似度覆盖范围 | **仅参与本次合并的那些声纹两两之间**（不看全库第三人） |
| 相似度口径 | **多向量取 max 余弦**——与 `Matched`/`MatchPreview` 同口径 |
| 交互形态 | sticky 确认卡片**原地展开相似度表**（两阶段，不新造模态） |
| 低分处理 | **只警示不拦截**——标红+文案，但仍可继续合并 |
| 拉取失败 | **放行**——预检是辅助不是门禁，网络抖动不得卡住正常纠错 |

## 2. 现状（调研实证，file:line）

- **合并端点** `POST /api/speakers/merge`：路由 `internal/api/speaker.go:103`，handler `SpeakerHandler.Merge` `:579`，请求体 `{source_ids, target_id}`（`:580-583`），响应 `{ok, merged_segments, removed_speakers}`（`:634`）。
- **合并事务** `SpeakerRepo.MergeInto`（`internal/repo/speaker.go:102`）：段 `speaker_id` 改指目标 + `speaker_session_state` 改指（`431ba3b` 加）+ 删源行，单事务。
- **样本迁移** `SpeakerEmbeddingRepo.MigrateToSpeakerExt`（`internal/repo/speaker_embedding.go:113`）：源样本行 `speaker_id` 改指目标、`source='merge'`（样本累加，不丢弃）。
- **保留目标选择**：target 由前端传 `target_id`，后端不自动选；前端默认取创建最早者（`web/app.js:2322-2329` `spMergeSelectedSorted` 按 `created_at` 升序）。
- **⚠ 当前合并无任何二次确认**：`applySpMerge()`（`web/app.js:2343-2356`）一点「确认合并」直接发请求，确认条仅「已选 N 个说话人」+ 目标下拉（`web/index.html:1648-1660`）。
- **没有 speaker↔speaker 相似度 API**。已有 `POST /api/voiceprint/match`（`speaker.go:114`/handler `:1163`）是「上传音频 vs 全库」，需 sidecar 提向。
- **向量存储**：`speaker_embedding` 表（`migrations/000016`）一行=一条样本，`embedding LONGBLOB` 256×float32=1024B；Go struct `repo.SpeakerEmbedding`（`speaker_embedding.go:17-26`，`Embedding` 标 `json:"-"` 不外泄）。真实索引在 FAISS sidecar，DB BLOB 是备份/重建源。
- **可复用的相似度设施**：`decodeEmbedding`（`speaker.go:1268`，BLOB→[]float32，长度须 %4==0）、`cosine`（`speaker.go:1281`，L2 归一向量内积，与 sidecar `IndexFlatIP` 等价）。
- **可复用的批量取数**：`SpeakerEmbeddingRepo.ListBySpeakers(ctx, ids)`（`speaker_embedding.go:54`）——**已存在**，按说话人分组返回 `map[ids.ID][]SpeakerEmbedding`，缺失 id 自然不进 map。无需新 repo 方法。
- **阈值常量**（`internal/voiceprint/match.go:12-24`）：`SoftMin=0.72`（弱命中下限）、`GapMin=0.06`、`LooseMin=0.4`/`LooseGap=0.1`；强命中阈值 `ZW_VOICEPRINT_THRESHOLD` 默认 0.8（`internal/config/config.go:158`）。
- **现有相似度 UI 格式**：表格用 `.toFixed(3)` + 进度条（`web/index.html:1497-1499`），chip 用 `.toFixed(2)`（`web/index.html:675`）；≥0.72 高亮 `--accent-2`（`web/index.html:673`）。

## 3. 后端：一个只读端点

```
POST /api/speakers/similarities        body  {"ids":["...","..."]}
→ {"pairs":[{"a_id","b_id","a_name","b_name","similarity"}]}   // C(N,2) 对；similarity=null 表示一方无样本不可比
```

- **路由**：挂在 `RegisterSpeaker`（`speaker.go:96`）内，`speaker.go:103` 旁。handler `SpeakerHandler.Similarities`。
- **不走 sidecar**：向量本体在 DB BLOB，纯 Go 算，不引入 FAISS 往返、不产生 sidecar 依赖（sidecar 未起时也能用）。
- **handler 流程**：
  1. 解析 `ids`；≥2 个（否则 400）；去重；逐个 `ids.ParseID`（非法 400）。
  2. `h.Speakers.List(ctx)` 建 `id→name` 映射（**一次查询取全部 active 说话人**，不做逐 id N+1；注意它不过滤 user，与既有 `Merge`/`Delete` 同语义）。
  3. `h.SpeakerEmbeddings.ListBySpeakers(ctx, ids)` 取分组样本（nil repo → 400 降级，与现有「未装配条目功能降级」一致）。
  4. 逐个 id `decodeEmbedding` 解出样本向量组，两两喂 `voiceprint.MaxCosine`，组装 `pairs`。
- **名字缺失不剔除**：id 不在 `List` 结果里（已被 dismiss/删除）时，该 id **仍参与矩阵**（`a_name` 留空、其全部相似度为 `null`）。这样「响应对数恒为 C(N_unique,2)」无条件成立，前端无需处理行数变化；「不存在」与「0 样本」渲染一致（都不可比），无需额外区分。
- **相似度纯函数**：新增 `internal/voiceprint/pair.go`。

```go
// MaxCosine 两组向量间的最大余弦相似度（多向量语义：与对方任意一条样本的最高分）。
// 任一方无样本返回 0——「不可比」与「完全不同」在纯向量域无法区分，交由调用方（handler）
// 用「该说话人是否存在于样本表」另行判定后以 null 表达。
func MaxCosine(a, b [][]float32) float64
```

  与 `match.go` 同域（相似度语义单一事实源）、只吃向量不碰 DB、可独立单测；`internal/api` 已 import `internal/voiceprint`（`h.Voiceprint voiceprint.Client`），无环。
- **归属校验**：**不做 user 维度过滤**。speaker 域现有 `SpeakerRepo.List`（`speaker.go:56`）即「返回全部 active 说话人」，`Merge`/`Delete` 亦不按 user 过滤——凭空加一层会与全域不一致。与既有端点保持同样语义。
- **响应顺序**：按入参 `ids` 的字典序对生成（先固定 `ids` 升序，再 `for i<j`），保证同一份输入响应稳定（前端可缓存比对）。
- **无数据库迁移。**

### 性能

N=10、每人 10 样本 → C(10,2)=45 对 × 100 次 256 维点积 = 4500 次内积，亚毫秒级；BLOB 读取约 N×M×1KB。纯 Go、无 IO 往返。补一个 benchmark 佐证（CLAUDE.md 要求性能有数据）：

- `BenchmarkMaxCosine`：多样本组的耗时。
- `BenchmarkSimilaritiesHandler`：N=20 全端点耗时。

## 4. 前端：两阶段确认

现状 `applySpMerge()`（`web/app.js:2343`）一点即发。改为三态：

| 阶段 | 行为 |
|---|---|
| ① 勾选 →「开始合并」→ 选目标 | 现状不变（默认目标=创建最早者） |
| ② 第一次点「确认合并」 | 调 `POST /api/speakers/similarities` → 展开相似度表 |
| ③ 第二次点「⚠ 仍然合并」 | 才发 `POST /api/speakers/merge`（现状逻辑原样） |

- **新 ref**：`spMergeSim = ref(null)`，结构 `{loading, error, pairs}`；`spMergeSimLoaded = ref(false)`。
- **表格**（`web/index.html`，sticky 卡片内新增）：每行一对 `A × B` + 相似度 `.toFixed(3)` + 分档徽标。**分档复用库里既有阈值，不引入新常量**：
  - `≥0.8` → `--ok`，徽标「强像」
  - `0.72~0.8` → `--accent-2`，徽标「弱像」（与 `web/index.html:673` 一致）
  - `<0.72` → `--danger` 标红，徽标「⚠ 不像，请确认」
  - `null` → 徽标「无法比较（无样本）」
- **不变量（写进注释守住）**：相似度矩阵**只依赖勾选集合，与谁是 target 无关**。用户在表格展开后改目标下拉，矩阵不重算，仅重标哪行是「源→目标」方向。省一次请求，也少一次等待。
- **降级**：相似度接口失败（网络错误 / 400 / 404）→ `notify` 提示「相似度预检失败，请自行确认后再合并」并**放行**进入第 ③ 阶段。预检不得成为门禁。
- **成功后**：仍走 `cancelSpMerge()` → `loadAllSpeakers()` → 按需 `reloadSession()` → toast，现状不变。

## 5. 边界与降级汇总

| 情形 | 行为 |
|---|---|
| 某说话人 0 样本 | 该对 `similarity: null`，表格照常列出「无法比较（无样本）」 |
| 相似度接口失败 | toast 提示 + **放行**，允许直接合并 |
| `ids` <2 个 / 非法 id | 400，前端视为预检失败 → 放行（不阻断合并，与上同） |
| `SpeakerEmbeddings` 未装配（nil） | 400，同上放行 |
| 勾选集合变化 | 重置 `spMergeSim`/`spMergeSimLoaded`，下次确认时重拉 |
| 仅改 target（集合不变） | **不重拉**，复用已有矩阵 |

## 6. 测试

**Go（TDD，先红后绿）**

- `internal/voiceprint/pair_test.go`
  - 多样本取最大（对方三条样本分别 0.5/0.9/0.7 → 0.9）
  - 对称性 `MaxCosine(a,b) == MaxCosine(b,a)`
  - 任一方空 → 0
  - 维度不一的向量（cosine 现有实现取较短长度，记录该行为）
  - `BenchmarkMaxCosine`
- `internal/api/speaker_test.go`（handler，走测试库）
  - <2 个 id → 400
  - 重复 id 去重，响应对数 = C(N,2)
  - 非法 id 字符串 → 400
  - 某说话人 0 样本 → 对应对 `similarity: null`
  - 响应对同一入参稳定（顺序确定性）
  - `SpeakerEmbeddings` 为 nil → 400
  - `BenchmarkSimilaritiesHandler`
- repo 层**无需新测试**——`ListBySpeakers` 已有覆盖，本特性只复用它。

**前端**：项目无前端测试基建，手动验证（勾 3 个 → 确认合并 → 见 3 对分数 → 改目标不重拉 → 第二次点才真合并 → 断网/接口 500 时仍可合并）。

## 7. 风险

- **误判方向**：低分标红只警示不拦截——若用户养成「一路点过去」，预检形同虚设。缓解：标红行加警示文案明确「不像同一人」。
- **0 样本静默**：无样本（或该说话人已不存在）的 id 返回 `null` 而非 0，避免被读成「完全不相似」而误导。因「不存在」与「0 样本」渲染一致（均不可比），handler 无需区分二者，也不因名字缺失而剔除该 id——矩阵行数恒为 C(N_unique,2)。
- **挤团库天花板**：铉晔/文生/杰辉互似 0.74~0.84 本就在警示区间内，预检会频繁标红。这是**如实反映库卫生问题**，不是 bug——memory 已记该库需人工合并去重。
- **不做 user 维度过滤**：与 speaker 域既有端点一致，但意味着多用户场景下任何人可查任意 speaker 的相似度。当前应用为单用户语义（`user_id DEFAULT 1`），若将来做多用户须一并补全域归属校验。
