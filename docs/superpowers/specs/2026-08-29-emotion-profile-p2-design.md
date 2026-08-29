# P2 人物情绪汇总（C）实现设计

- 日期：2026-08-29
- 总纲：`docs/superpowers/specs/2026-08-29-conversation-insight-roadmap-design.md`（本文件是其 **P2 / C 子项** 的实现 spec）
- 前置：P1（A+B）已合入 main（`speaker_session_state` 表 + audioscene stage 已落地）
- 分支/worktree：`feat/emotion-profile`（从 main 最新分出）

## 1. 目标与范围

把 P1 落库的**逐说话人情绪**（`speaker_session_state`，每会话每已识别说话人一条）聚合成**人物级情绪**，写入现有 `PersonMetric`（metric_key=emotion）时序平面，让人物画像有「整体情绪/精神状态」曲线与记录。

**核心决策（已与用户确认）**：
- **情绪映射到 valence 数值**：类别情绪（平静/喜悦/焦虑/疲惫/愤怒…）映射到 −1..1 效价，写 `value_num`（进情绪曲线）；`value_text` 存原始类别描述。
- **每会话每人一条**：每次 audioscene 跑完后，把该会话里每个已识别说话人（speaker_id→person）的情绪写成一条 PersonMetric，`measured_at` = 会话创建时间（时序最细、直接衔接情绪曲线）。
- **跳过未识别说话人**：speaker_id 为空（未关联 person）或经 `GetBySpeaker` 找不到 person 的，跳过、留待识别后再补（不造占位 person）。

## 2. 现状（调研实证，file:line）

- **P1 落库** `repo.SpeakerSessionState`（`internal/repo/speaker_session_state.go`）：字段 `{id,user_id,transcript_id,session_id,speaker_label,speaker_id(可空),emotion,micro_emotion,mental_state,confidence,created_at}`。audioscene stage 已把 `speaker_id` 按 label 归因回填（`internal/pipeline/stage_audioscene.go:106`）。
- **speaker→person 关联**：`repo.PersonRepo.GetBySpeaker(speakerID)`（`internal/repo/person.go:149`，`SELECT * FROM person WHERE speaker_id=?`）——person.speaker_id 指向 speaker。
- **情绪 metric 目录**：`profile.MetricCatalog["emotion"]`（`internal/profile/metric.go:22`）= `{emotion, 情绪, "", true}`，`value_num` 为情绪效价 valence，取值 −1..1（目录已注释，但**无现成映射表**，P2 需新建类别→valence 映射）。
- **PersonMetric 写入**：`repo.PersonMetricRepo.Create/CreateExt`（`internal/repo/person_metric.go:59/97`）。字段 `{person_id,metric_key,value_num(*float64),value_text(*string),unit,measured_at,confidence,epistemic_type,source,status,transcript_segment_ids}`。
- **管线顺序**：`asr → segment → speaker → speakername → audioscene → extract [→ profile]`（`cmd/zhiwei-server/main.go:242`）。P2 新 stage 插在 `audioscene` 之后、`extract` 之前。
- **会话时间**：`repo.SessionRepo.Get` 返回 `CreatedAt`（`internal/repo/session.go:22`）——measured_at 用它。

## 3. 数据流

```
audioscene stage 落 speaker_session_state（每人情绪，speaker_id 已归因）
        ↓
emotionprofile stage（新）
  1. 读本会话 speaker_session_state（SpeakerStates.ListBySession）
  2. 取 session.created_at 作 measured_at
  3. 逐条：speaker_id 非空 → PersonRepo.GetBySpeaker(speaker_id) 找 person
        · 找到 → 类别情绪映射 valence → 写 PersonMetric(emotion)
        · 找不到 / speaker_id 空 → 跳过
  4. 幂等：同 session+speaker 已写过则跳过（防 stage 重跑重复）
        ↓
  PersonMetric 情绪曲线 + 人物画像「精神状态」记录
```

## 4. 情绪→valence 映射（新建）

在 `internal/profile/`（或 `internal/agent`？放 profile，因 PersonMetric 属 profile）新建映射表 + 函数：
```go
// EmotionValence 类别情绪 → 效价 −1..1（PersonMetric.emotion 的 value_num）。
// 覆盖 audioscene 常见输出；未收录返回 0（中性）。
var EmotionValence = map[string]float64{
	"喜悦": 0.8, "开心": 0.8, "兴奋": 0.9, "满足": 0.6, "平静": 0.2, "中性": 0.0,
	"疲惫": -0.4, "焦虑": -0.6, "紧张": -0.5, "愤怒": -0.9, "悲伤": -0.8, "沮丧": -0.7,
	"无聊": -0.2, "困惑": -0.3,
}
func EmotionToValence(emotion string) float64 {
	if v, ok := EmotionValence[strings.TrimSpace(emotion)]; ok { return v }
	return 0.0 // 未收录→中性
}
```
（映射值为经验设定，可后续调；未收录回落 0 而非报错。）

## 5. Stage `emotionprofile`

**位置**：`asr → segment → speaker → speakername → audioscene → emotionprofile(新) → extract [→ profile]`。

**逻辑**（`stageEmotionProfile(d)`，`internal/pipeline/stage_emotionprofile.go`）：
1. 开关：`d.EmotionProfileEnabled` false 或 `d.PersonMetrics==nil` 或 `d.Persons==nil` → return nil（no-op）。
2. 读本会话 `speaker_session_state`（`SpeakerStates.ListBySession(ctx, 1, sessionID)`）。
3. 空 → return nil。
4. 取 `session.created_at`（`Sessions.Get`）作 measuredAt。
5. 逐条：
   - `speaker_id` 空 → 跳过。
   - `Persons.GetBySpeaker(speakerID)` → 找不到/ErrNoRows → 跳过。
   - **幂等**：查是否已存在 (person_id, metric_key=emotion, measured_at=本会话created_at, source=auto) 的 PersonMetric → 存在则跳过（防 stage 重跑重复）。
   - 映射 valence，写 `PersonMetric{PersonID, MetricKey:"emotion", ValueNum:&valence, ValueText:&emotion, MeasuredAt:createdAt, Confidence:原confidence, EpistemicType:"observed", Source:"auto", Status:"active", TranscriptSegmentIDs:[]}`。
6. user_id：后台流水线暂 user-1（对齐现有 stage）。
7. 失败：单条失败记日志 continue，不阻断；整体读失败降级 return nil。

**幂等重点**：stage 可能因 job 重试重跑，须防同 session+speaker 重复写。用 `PersonMetricRepo.FindByPointExt`(自然键 measured_at+值)或新增按 (person,metric,measured_at,source=auto) 的存在性查询。plan 阶段定具体查询。

**StageDeps 新字段**：
```go
	// ---- emotionprofile stage（P2 人物情绪汇总）----
	PersonMetrics         *repo.PersonMetricRepo
	Persons               *repo.PersonRepo
	EmotionProfileEnabled bool
```

## 6. 配置 + main.go 装配

- config 加 `EmotionProfileEnabled bool`（`ZW_EMOTION_PROFILE_ENABLED`，默认 true，可全局关）。
- main.go：构造 `PersonMetrics`/`Persons` repo 注入 StageDeps；`emotionprofile` 加入 stagesList（`audioscene` 后、`extract` 前）。
- （沿用 P1 的代理 key 无需再配——P2 不调 LLM，是确定性聚合。）

## 7. 前端（人物画像显示情绪）

- 人物详情已有 PersonMetric 情绪曲线（`emotion` value_num）——P2 写入后自动出现在现有指标曲线区块，无需新前端（除非要做「精神状态」专门展示，留作可选）。
- 最小改动：确认情绪曲线能渲染 P2 写入的 auto 来源数据。若现有曲线已按 metric_key 展示，则零前端改动。

## 8. 测试

- **valence 映射**（`emotion_valence_test.go`）：`EmotionToValence("喜悦")>0`、`("愤怒")<0`、未收录→0、去空格。
- **stage**（`stage_emotionprofile_test.go`）：fake 依赖注入——验证写 PersonMetric（valence 映射对、value_text=原类别、measured_at=会话时间、source=auto）、跳过未识别 speaker_id 空、跳过找不到 person、幂等（重跑不重复写）、开关关闭跳过。用 `repotest.DSN`。
- **repo**：如新增幂等存在性查询，测之。

## 9. 已定决策（不留 TBD）

| 项 | 决策 |
|----|------|
| 情绪映射 | 类别→valence(−1..1)写 value_num，value_text 存原类别 |
| 粒度/时机 | 每会话每人一条，measured_at=会话 created_at |
| 未识别说话人 | speaker_id 空或找不到 person → 跳过 |
| stage 位置 | audioscene 之后、extract 之前 |
| 写入 | PersonMetric(emotion, source=**auto**, status=active, epistemic=observed) |
| 幂等 | 同 session+speaker 不重复写（防 stage 重跑） |
| 开关 | ZW_EMOTION_PROFILE_ENABLED 默认开可关 |
| 不调 LLM | 确定性聚合，无 provider 依赖 |

## 10. 待 plan 阶段决定（非阻塞）

- 幂等的具体查询（FindByPointExt 复用 vs 新增按 person+metric+measured_at+source 查询）。
- valence 映射表的落位包（profile vs pipeline）与可配置化（后续可外提成配置/让 LLM 动态判）。
- 前端是否加「精神状态」专门展示（当前假定复用现有情绪曲线，零改动）。

## 11. 风险

- **映射主观性**：valence 值是经验设定，可能不精确——可接受（趋势/概览用，非精确测量），后续可调或让 LLM 判。
- **人物关联缺失**：大量说话人未绑定 person 时，P2 写入少——符合预期（未识别不硬造），随识别率提升自然增多。
- **迁移**：P2 不加迁移（复用 PersonMetric + speaker_session_state 现有表），无撞号风险。
- **与 LLM 抽取的 emotion 共存**：profile service 的 LLM 抽取也写 emotion metric，但用 `Source: "extract"`（`applyMetricFact`，`internal/profile/service.go:457`）。P2 用 `Source: "auto"` 天然区分，二者不冲突。幂等存在性查询须带 `source='auto'` 条件，绝不覆盖/跳过 LLM 的 `extract` 行。
