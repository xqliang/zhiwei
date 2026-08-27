# 宠物平面（pet plane）设计

日期：2026-08-27
状态：已评审通过（用户确认方案 A + 各节设计）

## 背景与目标

画像系统现有六平面（attribute/relationship/event/metric/cycle/activity），均无宠物概念。
用户需要记录养的宠物：支持多只，每只含 名称、小名、性别、年龄、喜好、类别（狗/猫…）、品种 等基本信息。

需求决策（已与用户确认）：

- **录入方式**：LLM 抽取 + 手动录入（与其他平面一致）
- **归属主体**：任意人物都可挂宠物（Subject 解析后挂对应 person，可记「我的猫」也可记「我老婆的猫」）
- **年龄**：存自由文本 `age_text`（「3岁」「8个月」）；LLM 抽取时据文本估算 `birthday`（DATE 可空）；手动录入必填正确 `birthday`（YYYY-MM-DD）
- **类别**：开放枚举（狗|猫|鸟|鱼|兔|仓鼠|爬行|其他）；品种纯自由文本
- **更新粒度**：整只替换（旧行 supersede、新行全量、版本链），不做字段级确认

## 方案选型

- **A（采纳）：新增 pet 平面** —— 复刻 event/cycle/activity 平面的增量模式（迁移表 + repo + Fact 字段 + prompt 契约 + service 落库 + API + Web 分区 + pending 确认流）。
- B（否决）：pets 列表属性 + JSON 值 —— JSON 塞文本列无法查询、无字段类型、确认流退化，违背类型化属性设计。
- C（否决）：宠物建模为 person + relationship（relation_type=宠物）—— 宠物混入人物列表/说话人解析，语义污染严重。

## 1. 数据模型

迁移 `migrations/000017_person_pet.up.sql`，表 `person_pet`（一只宠物一行）：

| 列 | 类型/约束 | 说明 |
|---|---|---|
| `id` | ids.ID 主键 | |
| `user_id` | int64 NOT NULL | 多用户隔离 |
| `person_id` | ids.ID NOT NULL | 归属人物 |
| `name` | varchar NOT NULL | 宠物名。**自然键 = (person_id, name)**：同人同名视为同一只；nickname 不参与匹配 |
| `nickname` | varchar NULL | 小名 |
| `species` | varchar NOT NULL | 类别枚举：狗\|猫\|鸟\|鱼\|兔\|仓鼠\|爬行\|其他（解析层收敛，库不设 CHECK） |
| `breed` | varchar NULL | 品种自由文本（柯基/布偶猫） |
| `gender` | varchar NULL | 公\|母 |
| `age_text` | varchar NULL | 年龄原始表述 |
| `birthday` | DATE NULL | LLM 按年龄估算；手动录入必填 |
| `likes` | varchar NULL | 喜好自由文本 |
| 通用列 | | `confidence float / epistemic_type / source(manual\|llm) / status(active\|pending\|superseded\|dismissed) / pre_dismiss_status / session_id / memory_id / transcript_segment_ids / supersedes_id / version / created_at / updated_at`，与其他平面完全同构 |

索引：`(user_id, person_id, status)` 列表查询；`(person_id, name)` 匹配查询。

## 2. LLM 抽取契约

- `internal/profile/fact.go`：`Fact` 加 pet 平面字段 `PetName / PetNickname / Species / Breed / Gender / AgeText / Birthday / Likes`；`rawFact` 对应 json 键（`pet_name / pet_nickname / species / breed / gender / age_text / birthday / likes`）；`validPlanes` 加 `"pet"`；校验：`PetName` 非空才保留（宁少勿错）；species 缺省或非法值均收敛到「其他」（库列 NOT NULL 的兜底）。
- prompt 升版 `prompts/profile_extraction_v3.md` → **`profile_extraction_v4.md`**（契约变更按惯例升版，main.go 读取文件名派生版本号）：
  - 新增「宠物平面」段：对话提到宠物时输出 `plane:"pet"`；`subject` 指代宠物主人；name 必填；年龄同时给 `age_text` 原文与 `birthday` 估算（YYYY-MM-DD，未知可空）；只填听到的字段。
  - few-shot：「我家猫小花最近不吃鱼了」→ `{plane:"pet", subject:{kind:"self"}, pet_name:"小花", species:"猫", likes:"不吃鱼", age_text:…, birthday:…}`。
- v3 文件保留（历史版本不删）。

## 3. 闸门与落库（gate.go + service.go）

`DecidePet` + `applyPetFact`，语义对齐 attribute 单值模式：

- **新宠物**（同人下无同名 active/pending）：置信度达标（autoConf 闸门）→ 直接 active；不达标 → pending。
- **同名已存在** → **字段级合并、整行重写**：fact 提到的字段覆盖旧值，未提到字段从旧行沿用；生成新行 supersede 旧行；置信度不足 → pending 行挂 supersedes 指向 active（确认后替换）。
- **完全无变化** → 去重跳过（reaffirm 佐证，更新置信度）。
- 写 change_log 审计（对齐 attribute 的 create/update 日志写法）。
- 人物归档级联 dismiss 宠物行（`pre_dismiss_status` 记录），恢复人物时还原——接入现有 RestoreArchivedExt 机制。

## 4. API（internal/api/person.go）

- `GET /api/persons/{id}/pets` —— active+pending 列表
- `POST /api/persons/{id}/pets` —— 手动新增（source=manual、active；birthday 必填 YYYY-MM-DD 校验）
- `PATCH /api/persons/{id}/pets/{pid}` —— 手动编辑，整只替换（supersede）
- `DELETE /api/persons/{id}/pets/{pid}` —— dismiss
- 人物详情 GET 响应加 `pets` 数组
- pending 确认队列加 kind `pet`（ListPending / ConfirmPending / DismissPending 复用现有机制）

## 5. Web UI（web/app.js + index.html）

人物详情页新增「宠物」分区：每只宠物一张卡片（名称/小名/类别·品种/性别/年龄·生日/喜好），pending 卡带确认/忽略按钮（复用现有 pending 交互）；手动添加/编辑表单（对齐 activity 分区的交互模式）。

## 6. 测试

按既有样板，测试库用 repotest.DSN 按包隔离（可并行）：

- `repo/person_pet_test.go`：CRUD / supersede / 归档级联与恢复
- `profile/fact_test.go`：pet plane 解析（含非法丢弃、species 收敛）
- `profile/gate_test.go`：DecidePet 分支
- `profile/service_test.go`：applyPetFact 新键/合并/低置信 pending
- `api/person_test.go`：pets CRUD + pending kind=pet 确认流

## 7. 开发方式

worktree 新分支 `feat/pet-plane`；调试用独立临时库 `zhiwei_pet`（约定见 memory：worktree 不迁共享库，合并 main 后才动共享库迁移）。

## 非目标（YAGNI）

- 不做宠物照片/体重等时序指标（可后续挂 metric 平面）
- 不做宠物生日提醒/事件联动（birthday 字段已具备数据基础）
- 不做字段级确认流（整只替换已确认）
