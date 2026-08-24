# 用户画像 / 人物系统 · 设计规格（总纲）

- 日期：2026-08-24
- 状态：设计定稿待评审
- 关联：产品文档「十六、人物系统」「十七、Unknown Speaker」「二十、事实等级」；声纹特性 `2026-08-22-speaker-voiceprint-design.md`；记忆抽取 `2026-08-19-zhiwei-sprint2-design.md`
- 交付方式：**本总纲一次画清全量数据模型，分 4 期（P1→P4）实现**，每期独立 plan、可单独上线

---

## 1. 目标与范围

围绕「人物（person）」建立结构化、可溯源、带置信度与修改历史的画像系统。既能**手动**增删改查，也能由 LLM 基于 timeline 对话历史**自动抽取/更新**，低置信或冲突的改动进入**人工确认队列**，绝不静默覆盖。画像可选关联声纹（speaker）。

覆盖的信息维度（用户原始需求，全部映射见 §14）：

- **静态属性**：姓名/别名/生日/性别/星座/MBTI/学历/学校/城市/住址/手机号；职业/行业/办公地点/上下班时间/通勤方式/是否出差/在做的项目；生活习惯（吃饭时间/菜系/吃辣/吃麻/吸烟/喝酒/化妆/香水）；兴趣（爱好/技能/在读的书/书单/影视/明星/动漫/电影类型/口头禅/是否炒股）；出行物品（去过的城市/旅游地/是否有车/车品牌/手机品牌）；关注领域/性格；慢性病清单
- **关系**：家庭/社会/上下游/组织关系
- **事件**（有日期）：重大事件（结婚/毕业/升学/生子/亲人过世/项目成败/出国/恋爱/生病/受伤/学会技能/中奖/被骗/被拐…）、聚会、会议、旅行
- **时间序列指标**：情绪、状态、体重、饮食、健康、是否熬夜
- **周期/日程（敏感）**：生理期、用药/打针（剂量·频次·周期）、慢病随访
- **活动流**：什么时间/多长时间/什么工具/做什么/地点/通勤 → 生活轨迹曲线

**非目标**：不做医疗诊断；「下次周期预测」仅为基于历史的估算（带免责）；不做多租户权限（沿用单用户 `user_id=1` MVP）。

## 2. 关键决策（brainstorm 结论）

1. **画像对象 = 每个人物**（owner「我」+ 他人），可选关联 0/1 个声纹；不强制——只被提到、从未录音的人（配偶/孩子）也能建档。
2. **属性建模 = 目录 + 类型化属性表 + 统一历史**（否决了单 JSON 文档、复用 memory 表两种方案，理由见 §3.1）。
3. **范围 = 一次做全**（含 LLM 抽取），但按 §12 分期落地。
4. 默认值（评审可调）：
   - 抽取触发 = 自动 `profile` 流水线阶段 + 按需回填端点（非纯手动）
   - 提到的人自动建档 = 允许，但自动新建的人物走 `pending` 确认
   - 自动写入置信阈值 = `0.75`，env 可配（`ZW_PROFILE_AUTO_CONFIDENCE`）
   - 「关注领域」（政治/军事/体育/三农…）建模为一个 list 属性，非 4 个 bool

## 3. 概念模型

一个中心实体 `person`，挂 6 个**数据平面** + 1 个统一审计：

| 平面 | 表 | 语义 | 期 |
|---|---|---|---|
| 属性 | `person_attribute` | 当前态事实（单值/列表） | P1 |
| 关系 | `person_relationship` | 人与人/组织的边 | P1 |
| 事件 | `person_event` | 有日期的一次性事件 | P2 |
| 指标 | `person_metric` | 时间序列（时间戳+值） | P3 |
| 周期 | `person_cycle` | 周期/日程（敏感） | P3 |
| 活动 | `person_activity` | 活动流（轨迹曲线源） | P4 |
| 审计 | `person_change_log` | 跨平面统一修改历史 | P1 |

**跨平面统一的横切字段**（除 `person`/`change_log` 外，6 个平面表都有）：
`source`(manual|llm)、`confidence` DECIMAL(5,4)、`epistemic_type`(observed|inferred|predicted|suggested)、`status`(active|pending|superseded|dismissed)、`session_id`/`memory_id`/`transcript_segment_ids`(溯源)、`supersedes_id`(冲突改动指向被替换行)、`version`、时间戳。

**确认队列不单独建表** = 跨 6 平面查 `status='pending'` 的并集。**修改历史不每平面各建表** = 统一进 `person_change_log`（`entity_kind + entity_id`）。这是相对上一版设计的两处关键收敛。

### 3.1 为何选「目录 + 类型化属性表」

- 单 JSON 文档：每字段的置信度/来源/确认/历史塞进 JSON 极别扭，「谁吃辣」类查询难，整文档写入并发不安全。
- 复用 memory 表：memory 是叙事/事件导向（自由文本 title/content + event_at），拿不到类型化枚举/列表和干净的「当前值」视图，表单难做，且会把画像事实混进记忆流。
- 目录 + 类型化属性表：唯一能同时满足「类型化值 + 每字段置信度 + 每字段确认 + 每字段历史含人/LLM+溯源」且可扩展、表单友好。epistemic 词汇从 memory 借用，保持全局一致。

## 4. 数据模型

沿用现有约定：雪花 ID BIGINT 主键、无 AUTO_INCREMENT、`utf8mb4`、`DATETIME(3)`、JSON 列存 id 数组（`ids.List`）。迁移 `000005_person`（P1）建 person/attribute/relationship/change_log 四表；`000006`（P2）建 event；`000007`（P3）建 metric/cycle；`000008`（P4）建 activity。分期建表避免一次性大迁移。

### 4.1 person（P1）

```sql
CREATE TABLE person (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,          -- 画像归属的 owner（单用户 MVP=1）
  display_name VARCHAR(128) NOT NULL,              -- 主显示名
  speaker_id   BIGINT NULL,                        -- 可选关联声纹；至多 1 个（唯一）
  is_owner     TINYINT(1) NOT NULL DEFAULT 0,      -- 是否是「我」本人（全局至多一个）
  summary      TEXT NULL,                          -- 自由备注/一句话画像
  source       VARCHAR(8) NOT NULL DEFAULT 'manual', -- manual|llm（llm=抽取时新建的人）
  status       VARCHAR(16) NOT NULL DEFAULT 'active', -- active|pending|merged|dismissed
  created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_speaker (speaker_id),              -- 一个声纹至多绑一个人（NULL 不占唯一）
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- `speaker_id` 唯一：MySQL 允许多行 NULL，不影响「未关联」的人。
- `is_owner`：迁移回填时若无 owner 则建一个（`display_name='我'`）。
- 自动新建的人（`source='llm'`）以 `status='pending'` 落库，确认后转 `active`；避免抽取噪声污染名册。

### 4.2 person_attribute（P1）

```sql
CREATE TABLE person_attribute (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  person_id     BIGINT NOT NULL,
  attr_key      VARCHAR(64) NOT NULL,              -- 目录 key 或自由 key
  value_text    TEXT NOT NULL,                     -- 规范字符串形态（bool 存 'true'/'false'，date 存 ISO）
  value_type    VARCHAR(16) NOT NULL DEFAULT 'text', -- text|enum|bool|date|number|list_item
  confidence    DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed',
  source        VARCHAR(8)  NOT NULL DEFAULT 'manual', -- manual|llm
  status        VARCHAR(16) NOT NULL DEFAULT 'active', -- active|pending|superseded|dismissed
  session_id    BIGINT NULL,                       -- 溯源：来自哪个会话
  memory_id     BIGINT NULL,                       -- 溯源：来自哪条记忆
  transcript_segment_ids JSON NULL,                -- 溯源：哪些转写段
  supersedes_id BIGINT NULL,                       -- pending 冲突改动指向当前 active 行
  version       INT NOT NULL DEFAULT 1,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_key_status (person_id, attr_key, status),
  KEY idx_user_status (user_id, status),           -- 确认队列扫描
  KEY idx_session (session_id)                      -- 抽取幂等 dedup 扫描
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- **列表型属性**（爱好/书单/菜系…）= 同 `attr_key` 多行 active，每元素独立置信度/来源/溯源/确认。单值型 = 同 key 至多一行 active。单值/列表由 §4.8 属性目录声明。
- **冲突改动**：新值 pending 行 `supersedes_id` 指向当前 active 行；确认后旧行转 `superseded`、新行转 `active`。

### 4.3 person_relationship（P1）

```sql
CREATE TABLE person_relationship (
  id                BIGINT PRIMARY KEY,
  user_id           BIGINT NOT NULL DEFAULT 1,
  person_id         BIGINT NOT NULL,               -- 主体
  related_person_id BIGINT NULL,                   -- 关系对端（可空：只有称呼没建档时）
  relation_type     VARCHAR(24) NOT NULL,          -- 配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他
  direction         VARCHAR(8) NULL,               -- upstream|downstream|peer（上下游用）
  org_name          VARCHAR(128) NULL,             -- 组织关系的组织名
  label             VARCHAR(128) NULL,             -- 自由称呼（「大儿子」「张总」）
  -- 横切字段（source/confidence/epistemic_type/status/溯源/supersedes_id/version/时间戳）同上
  ...
  KEY idx_person (person_id, status),
  KEY idx_related (related_person_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- 「老婆做什么的」= 给配偶那个 person 记 `occupation` 属性 + 一条 `配偶` 关系边。
- 「几个孩子/几岁/生日」= N 条 `子女` 边 → N 个（可 pending 的）子女 person，各自 `age`/`birthday` 属性。统一走属性机器，不特殊化。

### 4.4 person_event（P2）

```sql
CREATE TABLE person_event (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  person_id     BIGINT NOT NULL,
  event_type    VARCHAR(32) NOT NULL,              -- 里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他（开放）
  title         VARCHAR(512) NOT NULL,
  description   TEXT NULL,
  occurred_at   DATETIME(3) NULL,                  -- 事件发生时间（可能只精确到日）
  end_at        DATETIME(3) NULL,                  -- 跨天事件（旅行/会议）
  location      VARCHAR(256) NULL,
  related_person_ids JSON NULL,                    -- 同场人物（聚会/会议）
  importance    DECIMAL(5,4) NOT NULL DEFAULT 0.5,
  -- 横切字段同上
  ...
  KEY idx_person_time (person_id, occurred_at),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- 与 memory 的去重约定：memory 是「理解层」叙事，person_event 是「人物大事记」结构化条目；抽取时二者可由同一段对话各自产出，靠 `memory_id` 溯源关联，不强制一致。
- 旅行既落 event（有日期的一次），也可回填 `旅游地` list 属性（速览）；避免重复见 §6.4。

### 4.5 person_metric（P3）

```sql
CREATE TABLE person_metric (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  person_id    BIGINT NOT NULL,
  metric_key   VARCHAR(32) NOT NULL,               -- emotion|state|weight|sleep_late|diet|health
  value_num    DECIMAL(10,3) NULL,                 -- 数值型（体重 kg、情绪 valence -1..1、熬夜 0/1）
  value_text   VARCHAR(256) NULL,                  -- 类别/描述（情绪='焦虑'、饮食='火锅'）
  unit         VARCHAR(16) NULL,
  measured_at  DATETIME(3) NOT NULL,               -- 该测点时间
  -- 横切字段同上
  ...
  KEY idx_person_metric_time (person_id, metric_key, measured_at),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- 「独处的时间」= 派生指标：由 activity/event 中「无同场人物」的时段聚合计算，不单独存原始行（P4 计算，可选物化）。

### 4.6 person_cycle（P3，敏感）

```sql
CREATE TABLE person_cycle (
  id              BIGINT PRIMARY KEY,
  user_id         BIGINT NOT NULL DEFAULT 1,
  person_id       BIGINT NOT NULL,
  cycle_type      VARCHAR(16) NOT NULL,            -- menstrual|medication|injection|followup
  label           VARCHAR(128) NULL,               -- 药名/针名/'生理期'
  anchor_date     DATE NULL,                       -- 上次起始（预测锚点）
  period_days     INT NULL,                        -- 周期天数
  duration_days   INT NULL,                        -- 单次持续
  dosage          VARCHAR(64) NULL,                -- 剂量
  frequency_text  VARCHAR(64) NULL,                -- 频次（'每日两次'）
  next_predicted_at DATE NULL,                     -- 估算下次（= anchor + period；非医疗建议）
  -- 横切字段同上（含 status，用于停用/确认）
  ...
  KEY idx_person_type (person_id, cycle_type),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

敏感处理见 §9。慢性病本身落 `person_attribute`（健康组 list）+ 可关联确诊 `person_event` + 用药 `person_cycle`。

### 4.7 person_activity（P4）

```sql
CREATE TABLE person_activity (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  person_id    BIGINT NOT NULL,
  activity     VARCHAR(256) NOT NULL,              -- 做什么
  tool         VARCHAR(128) NULL,                  -- 什么工具（手机/电脑/健身房/汽车…）
  location     VARCHAR(256) NULL,
  commute_mode VARCHAR(24) NULL,                   -- 通勤方式（复用属性枚举）
  started_at   DATETIME(3) NOT NULL,
  duration_min INT NULL,                           -- 多长时间
  -- 横切字段同上
  ...
  KEY idx_person_time (person_id, started_at),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

生活轨迹曲线 = 前端对 activity（+event/metric）的可视化，实现时走 `dataviz` skill。

### 4.8 person_change_log（P1，统一审计）

```sql
CREATE TABLE person_change_log (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  person_id     BIGINT NOT NULL,
  entity_kind   VARCHAR(16) NOT NULL,              -- person|attribute|relationship|event|metric|cycle|activity
  entity_id     BIGINT NULL,                       -- 目标行 id（删除后仍留历史）
  attr_key      VARCHAR(64) NULL,                  -- attribute 平面冗余，便于按字段查历史
  change_type   VARCHAR(16) NOT NULL,              -- create|update|confirm|dismiss|supersede|delete|reaffirm
  changed_by    VARCHAR(8) NOT NULL,               -- user|llm
  old_value     JSON NULL,                         -- 变更前快照
  new_value     JSON NULL,                         -- 变更后快照
  confidence    DECIMAL(5,4) NULL,
  epistemic_type VARCHAR(16) NULL,
  session_id    BIGINT NULL,                       -- 关联的 timeline 会话
  memory_id     BIGINT NULL,                       -- 关联的事件/记忆
  transcript_segment_ids JSON NULL,
  note          TEXT NULL,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_person_kind_time (person_id, entity_kind, created_at),
  KEY idx_entity (entity_kind, entity_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

满足用户硬需求：「同一条信息被谁（`changed_by`）、何时（`created_at`）、从什么（`old_value`）改成什么（`new_value`）、关联哪条 timeline/事件（`session_id`/`memory_id`/`segment_ids`）」。**只追加**，永不 update/delete。

### 4.9 属性目录（代码级，非表）

`internal/profile/catalog.go` 定义已知属性：`key → {Label(中文), Group, ValueType, EnumOptions[], Cardinality(single|list)}`。目录外的 key 落「其他」组、`value_type=text`、`single`，仍可扩展。分组与枚举示例：

- **基本**：aliases(list)、birthday(date)、gender(enum:男/女/其他)、zodiac(enum，可由 birthday 推)、mbti(enum16)、education(enum)、school(list)、city、address、phone
- **工作**：occupation、industry、office_location、work_start_time、work_end_time、commute_mode(enum:步行/自行车/地铁/公交/开车/打车/高铁…)、often_travel(bool)、current_projects(list)
- **生活习惯**：meal_time、cuisine(list:川/粤/湘…)、eats_spicy(bool)、eats_numbing(bool)、smokes(bool)、drinks(bool)、wears_makeup(bool)、perfume
- **兴趣**：hobbies(list)、skills(list)、reading_now(list)、books_read(list)、movies_watched(list)、music_listened(list)、games_played(list)、fav_celebrities(list)、fav_anime(list)、fav_movie_genres(list)、catchphrases(list)、invests_stocks(bool)
- **出行物品**：cities_visited(list)、places_traveled(list)、has_car(bool)、car_brand、phone_brand
- **关注/性格/健康**：recent_concerns(list)、attention_topics(list:政治/军事/体育/三农…)、personality、chronic_diseases(list)

## 5. 置信度与确认闸门（跨平面统一规则）

对任一平面的一条**候选**（来自 LLM 抽取或手动录入）：

1. **手动录入/编辑**：立即 `status='active'`、`source='manual'`、`confidence=1.0`；写 change_log(`user`,`create|update`)。冲突时旧行转 `superseded`。
2. **LLM 候选，该 key 无现值**（单值型；列表型视元素是否已存在）：
   - `confidence ≥ ZW_PROFILE_AUTO_CONFIDENCE` 且 `epistemic∈{observed,inferred}` → 直接 `active`，change_log(`llm`,`create`)。
   - 否则 → `status='pending'`（进确认队列），change_log(`llm`,`create`)。
3. **LLM 候选，有现值且新值==旧值**：仅提升现值 `confidence`（+0.05 封顶 0.99）、补溯源；change_log(`llm`,`reaffirm`)。幂等见 §6.3（同 session 不重复 bump）。
4. **LLM 候选，有现值且新值≠旧值（冲突）**：**绝不静默覆盖**。建 `pending` 行、`supersedes_id` 指向现值；change_log(`llm`,`create`+note='conflict')。
5. **确认队列操作**：
   - 确认：pending→active；若有 `supersedes_id`，旧行→superseded；change_log(`user`,`confirm`/`supersede`)。
   - 放弃：pending→dismissed；change_log(`user`,`dismiss`)。

阈值 `ZW_PROFILE_AUTO_CONFIDENCE` 默认 0.75，需用真实对话 benchmark 调（沿用 `ZW_VOICEPRINT_THRESHOLD` 的调参文化）。

## 6. LLM 抽取

### 6.1 profile 流水线阶段

在 `Flow.Stages` 末尾加 `"profile"`：`asr → segment → speaker → extract → profile`。`stageProfile(d StageDeps) Handler` 模仿 `stage_extract.go`：

1. 读本 session 的 transcript/segments（把 `speaker_id` 换成人名，同 extract）+ 本 session 刚抽出的 memories（`memory_id` 供溯源）。
2. 调 `profile.Extractor{LLM, Model, Prompt(prompts/profile_extraction_v1.md), Window}` → 返回一批 **typed facts**：每条含 `plane`(attribute|relationship|event|metric|cycle|activity)、`subject`(人物指代)、`key/type/value`、`confidence`、`epistemic`、`segment_ids`。
3. **人物归属**（§6.2）→ **归类落库**（§5 闸门）在单事务内提交。
4. 写 `job.trace`（tokens/windows/prompt 版本），沿用 extract 的 trace 风格。

### 6.2 人物归属

- 段落 `speaker_id` → 已绑定的 person；第一人称（"我…"）→ owner person。
- 提到的第三方（"我老婆 Alice 是医生"）→ 按 姓名/别名 匹配现有 person；无则新建 `source='llm'`、`status='pending'` 的无声纹 person（走确认，防噪声）。
- LLM 在 typed fact 里给出 `subject` 线索（说话人 / "我" / 具名他人 / 关系指代如"我老婆"），Go 侧做确定性解析与匹配。

### 6.3 幂等（关键，区别于 extract）

extract 阶段靠「删本 session 旧 memory 再重插」保证幂等；**profile 不能删——属性跨 session 累积且带用户确认状态**。改用 **dedup key**：

- 每条候选算自然键 `(session_id, person_id, plane, key, normalize(value))`。
- 落库前在事务内查该自然键是否已有行（任意 status）；已有则**跳过**（不重复建 pending、不重复 bump）。
- 自动新建人物按 `(user_id, normalize(display_name), source='llm')` 去重。
- 效果：重跑同一 session 不产生重复建议，且用户此前的 confirm/dismiss 决定被保留。

### 6.4 去重约定

- 列表属性叠加、单值属性遵 §5 冲突规则。
- 旅行：event 记「某次带日期的旅行」；`places_traveled` list 属性记「去过的地方」速览。抽取时二者可同出，靠自然键各自去重，互不覆盖。

### 6.5 按需回填端点

`POST /api/profile/extract`（可带 `session_id`；不带则遍历历史 session）触发对存量 timeline 的抽取，复用同一 `profile.Extractor` 与闸门逻辑（异步入 job 或同步小批，实现期定）。

## 7. API

沿用 `api.RegisterXxx(r, &Handler{...})` 风格：

```
GET/POST           /api/persons                       名册 / 新建人物
GET/PATCH/DELETE   /api/persons/{id}                  详情 / 改名·关联声纹·设owner·状态 / 归档
POST               /api/persons/{id}/attributes       手动加属性
PATCH/DELETE       /api/persons/{id}/attributes/{aid} 手动改/删（自动记 change_log + supersede）
GET                /api/persons/{id}/history          该人物全平面修改历史（可 ?entity_kind=&attr_key= 过滤）
POST               /api/persons/{id}/relationships    加关系边
GET                /api/profile/pending               确认队列（跨平面 status='pending' 并集；可 ?person_id=）
POST               /api/profile/pending/{kind}/{id}/confirm|dismiss  确认/放弃一条候选
POST               /api/profile/extract               触发抽取/回填（可带 session_id）
# P2+：/api/persons/{id}/events、/metrics、/cycles、/activities（同风格）
```

详情返回：分组 active 属性 + 关系 + 最近互动（该人物出现过的 session，对齐产品§16）+ 共同 Topic + 相关 Todo + 待确认计数。

## 8. 前端

`web/app.js` 新增「人物」tab（复用现有卡片/徽标/tab 样式，无构建）：

- **名册**：人物卡（名字、声纹徽标、待确认数角标、is_owner 标记）。
- **详情页**（对齐产品§16）：分组属性区（每条显示 值 + 置信度 + 来源[人工/LLM] + epistemic 徽标；行内改/删；「查看历史」抽屉）；关系区；大事记时间线（P2）；状态/体重曲线（P3，dataviz）；健康区（慢病+生理期+用药，可折叠/私密，P3）；生活轨迹曲线（P4，dataviz）；最近互动 / 共同 Topic / 相关 Todo。
- **待确认区**：pending 建议列，展示 建议值 vs 现值（冲突时并排）、置信度、来源段落链接（点击跳 timeline）、[确认]/[放弃]。
- **操作**：新建人物、关联声纹（从声纹名册选）、从历史抽取画像。

## 9. 敏感数据处理（生理期/慢病/用药）

- 单用户本地 MVP，数据存本地库；不外泄（同声纹 embedding「不外泄」约定）。
- `next_predicted_at` 仅为「anchor + period」估算，UI 明确标注**非医疗建议**。
- 健康/生理平面在详情页**默认折叠**，提供「隐藏敏感信息」开关；不做诊断逻辑。
- change_log 同样记录这些平面的改动（可审计）。

## 10. 配置项（`internal/config`）

- `ZW_PROFILE_AUTO_CONFIDENCE`（默认 0.75）：自动写入阈值。
- `ZW_PROFILE_EXTRACT_ENABLED`（默认 true）：是否启用 profile 流水线阶段（关掉则仅手动）。
- `ZW_PROFILE_EXTRACT_WINDOW`：抽取窗口（沿用 extract 的窗口切分思路）。
- profile 抽取 prompt 走版本化文件 `prompts/profile_extraction_v1.md`，版本号进 trace。

## 11. 测试策略

沿用 `make test`（mock provider，无 MySQL）+ `make test-integration`（真连 MySQL）：

- **纯逻辑单测**（`internal/profile`）：闸门规则（无现值/命中/冲突/阈值/列表叠加）、人物归属解析、dedup 自然键、周期预测算式。
- **repo 单测**（integration）：各表 CRUD、跨平面 pending 并集查询、change_log 追加、supersede 事务。
- **stage 单测**：mock LLM 返回 typed facts，验证归属+闸门+幂等（重跑不重复）。
- **api 单测**：handler 路由、确认/放弃、历史查询。
- 阈值/prompt 的真实 LLM 验证走 `make spike-*` 手动，不进 CI（避免烧钱，沿用现有约定）。

## 12. 分期实施

| 期 | 交付物 | 迁移 | 验收 |
|---|---|---|---|
| **P1 基础画像** | person + attribute + relationship + change_log；闸门+确认队列；`internal/profile`（catalog/extractor/gate/resolve）；profile stage（属性·关系抽取）；`/api/persons*`、`/api/profile/*`；人物 tab（属性/关系/确认/最近互动/Topic/Todo） | 000005 | 手动 CRUD + 抽取产出 pending + 确认落 active + 历史可查；重跑 session 不重复 |
| **P2 大事记 & 媒体史** | person_event；书/音乐/影视/游戏 list 属性；大事记时间线 UI；抽取扩展 event | 000006 | 抽取生成带日期事件；时间线展示；去重不与属性打架 |
| **P3 状态 & 健康** | person_metric + person_cycle；情绪/状态/体重/饮食/熬夜曲线；生理期/用药/慢病；敏感处理 | 000007 | 指标时序图表；周期预测（带免责）；敏感折叠 |
| **P4 生活轨迹** | person_activity；轨迹曲线可视化（dataviz）；独处时间等派生指标 | 000008 | 活动流录入/抽取；轨迹曲线；派生指标计算 |

全量模型在本总纲一次画清，plan 按期出。**先做 P1**。

## 13. 已知限制与后续

- 人物归属对「同名不同人」「代词指代」依赖 LLM+规则，边界情形进 pending 由人确认。
- 回填端点对大量历史 session 的批处理性能，P1 先小批/单 session，规模化留后续。
- 「独处时间」等派生指标依赖 activity/event 覆盖度，P4 前数据稀疏时不展示。
- 曲线可视化在无构建的 `web/app.js` 里用轻量方案（内联 SVG 或单文件图表库 vendor），实现期定，走 dataviz skill。

## 14. 字段映射（确保用户列举项全部落位）

| 用户原始表述 | 落位 |
|---|---|
| 姓名 | person.display_name |
| 别名 | attribute:aliases(list) |
| 生日/性别/星座/MBTI/学历/学校/城市/住址/手机号 | attribute 基本组 |
| 工作·职业/所属行业/办公地点/上班·下班时间/通勤方式/是否经常出差/正在进行的项目 | attribute 工作组 |
| 家庭关系/老婆或老公做什么的/有几个孩子·几岁·孩子生日/社会关系/上下游/组织关系 | relationship（+ 对端 person 的 occupation/age/birthday 属性） |
| 吃饭时间/喜欢的菜系/是否吃辣/是否吃麻/是否吸烟/是否喝酒/是否化妆/香水 | attribute 生活习惯组 |
| 爱好/学的技能/正在看的书/看过的书/看过的电影电视剧/听过的音乐/玩过的游戏/喜欢的明星/动漫/电影类型/口头禅/是否炒股 | attribute 兴趣组（多为 list） |
| 去过的城市/旅游过的地方/是否有车/车品牌/用什么手机 | attribute 出行物品组 |
| 最近关心的事情/关心政治·军事·体育·三农/性格 | attribute 关注·性格组（attention_topics 为 list） |
| 重大事件（出生·上学·毕业·升学·结婚·恋爱·生子·亲人过世·项目成败·旅游·出国·生病·受伤·学会·中奖·被骗·被拐） | person_event |
| 参加过的聚会/讨论过的会议/旅游过的地方（有日期的一次） | person_event |
| 情绪历史/状态历史/体重/饮食/健康/是否熬夜 | person_metric |
| 独处的时间 | 派生指标（activity/event 聚合） |
| 女生生理日期/得了什么慢性病/吃什么药·打什么针（周期） | person_cycle（慢病诊断名亦落 attribute 健康组） |
| 生活轨迹曲线（什么时间·多长时间·什么工具·做什么·通勤） | person_activity + 前端曲线 |
| 修改历史（人工/LLM·何时·从什么到什么·关联事件） | person_change_log |
| 置信度不高提示确认 | status='pending' + 确认队列 |
| 关联声纹 | person.speaker_id |
```
