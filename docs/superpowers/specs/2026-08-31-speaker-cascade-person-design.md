# 声纹删/合并 → 关联人物级联处理 设计

- 日期：2026-08-31
- 分支/worktree：`feat/speaker-cascade-person`
- 范围：说话人（声纹）被**删除**或**合并**时，处理其自动登记的人物——未编辑过则静默级联删除（dismiss），编辑过则返回提示由用户确认。
- 关联规格：person-profile spec §4.8（审计）、agent-chatbot spec §4（声纹↔人物绑定）。

## 1. 目标与决策（已与用户确认）

| 项 | 决策 |
|----|------|
| 触发场景 | 说话人被**删除**（`SpeakerHandler.Delete`）或被**合并**（`SpeakerHandler.Merge`，源说话人被并入目标后删除） |
| 未编辑过的人物 | **静默级联删除**（dismiss，软删） |
| 编辑过的人物 | **不删**，API 返回需确认的人物列表，由前端提示用户决定 |
| 「编辑过」口径 | **宽（B）**：人物本身被手工改（改名/摘要）**或**名下任何属性/事件/关系被手工增删改，均算编辑过。判定依据 `person_change_log.changed_by='user'`。 |
| owner 人物 | **永级联删除**（`is_owner=true` 保护） |

## 2. 现状（调研实证，file:line）

- **删声纹** `internal/api/speaker.go:482` `SpeakerHandler.Delete`：删 sidecar 向量 + DB 行 + 清 `transcript_segment.speaker_id` + 清样本/候选。**当前只解绑 person.speaker_id（`4b46c87` 已加），不删 person**。本设计在此追加级联。
- **合并声纹** `internal/api/speaker.go:525` `SpeakerHandler.Merge`：源段改指目标 → 删源行。**当前不处理源的人物**。本设计在合并后处理每个源声纹的人物。
- **人物删除语义** = **dismiss**（软删）：`internal/api/person.go:465` `h.Service.ManualSetPersonStatus(uid, id, "dismissed")`；恢复视图 `?dismissed=1`（`person.go:117`）。级联删除对齐此语义（dismiss），**不做硬删**。
- **编辑痕迹**：`person_change_log`（`internal/repo/person_change_log.go`）`ChangedBy string // user|llm`；`ListByPerson(personID, entityKind, attrKey)`（`:64`）。人物被手工编辑 ⟺ 存在 `changed_by='user'` 的 change_log（create 行也可能是 user 建的，需区分——见 §4 判定）。
- **声纹→人物绑定**：`person.speaker_id`（`internal/repo/person.go:21`）；`PersonRepo.GetBySpeaker(speakerID)`（`:149`）按声纹查人物。
- **SpeakerHandler 已注入**：`Persons *repo.PersonRepo`、`Service *profile.Service`（nil=降级兼容）。

## 3. 级联处理逻辑（核心）

新增 `internal/api/speaker_cascade.go`：

```go
// cascadePersonOnSpeakerRemoval 处理「声纹被删/合并移除」后其关联人物的去向。
//   - 无关联人物 / owner 人物：跳过。
//   - 人物未被手工编辑（无 changed_by='user' 变更）：静默 dismiss。
//   - 人物被手工编辑：不删，加入返回列表由前端提示。
// 返回需用户确认的人物摘要列表。
func cascadePersonOnSpeakerRemoval(ctx context.Context, h *SpeakerHandler, uid int64, speakerID ids.ID, autoDismiss bool) []personCascadePrompt {
	if h.Persons == nil || h.Service == nil {
		return nil
	}
	p, err := h.Persons.GetBySpeaker(ctx, speakerID)
	if err != nil || p == nil || p.IsOwner {
		return nil // 无关联 / owner：跳过
	}
	if personEditedByUser(ctx, h, p.ID) {
		return []personCascadePrompt{{PersonID: p.ID, Name: p.DisplayName, Reason: "该人物被手工编辑过"}}
	}
	if autoDismiss {
		_ = h.Service.ManualSetPersonStatus(ctx, uid, p.ID, "dismissed")
	}
	return nil
}

// personEditedByUser 人物是否被手工编辑（changed_by='user' 且非仅 create）。
func personEditedByUser(ctx context.Context, h *SpeakerHandler, personID ids.ID) bool {
	logs, err := h.ChangeLogs.ListByPerson(ctx, personID, "", "")
	if err != nil {
		return true // 查询失败时保守视为「已编辑」，不静默删
	}
	for _, l := range logs {
		if l.ChangedBy == "user" && l.ChangeType != "create" {
			return true
		}
	}
	return false
}
```
> `autoDismiss` 参数：删除流程传 true（未编辑静默删）；合并流程按产品语义——合并是纠错，源人物通常也自动建档，传 true 静默删（编辑过的仍提示）。

### 挂载点

**Delete**（`speaker.go:482`）：在现有解绑 person.speaker_id 之后（`4b46c87` 处），调用 `cascadePersonOnSpeakerRemoval(ctx, h, uid, id, autoDismiss=true)`。返回的需确认人物列表 → 若非空，在 204 响应体里带 `cascade_prompts`（或改为 200 带提示）。**权衡**：删除声纹是破坏性操作，若人物被编辑过需用户确认——是「先删声纹、人物留待确认」还是「有人物待确认就不删声纹」？**决策：先完成声纹删除（破坏小、可恢复性低风险在声纹侧），人物编辑过的在响应里带 prompts，由前端弹确认后用户再决定删不删人物**（人物 dismiss 是可恢复软删，风险低）。

**Merge**（`speaker.go:525`）：`Speakers.MergeInto` 删源行之后，对每个 `srcID` 调用 `cascadePersonOnSpeakerRemoval(ctx, h, uid, srcID, autoDismiss=true)`，收集所有需确认人物，合并进响应 `cascade_prompts`。

### SpeakerHandler 新增字段
```go
ChangeLogs *repo.PersonChangeLogRepo // 判定人物编辑痕迹（nil=降级：视为未编辑，静默删）
```
main.go 装配 `ChangeLogs: personLogs`（personLogs 已存在于 main.go）。

## 4. 需你定夺（先于实现）

1. **删除声纹时，若人物被编辑过需确认，是「先删声纹再提示人物」还是「不删声纹、整体回滚待确认」？** 推荐前者（声纹删除风险低，人物 dismiss 可恢复）。
2. **Merge 的源人物**：未编辑静默删（autoDismiss=true）是否符合预期？合并是纠错场景，源人物一般也是自动建档的。
3. **响应形式**：需确认的人物列表放 204 响应体（`cascade_prompts`）还是改 200？前端是否需要「稍后处理这些待确认人物」的持久化，还是仅在当次响应里提示？

## 5. 数据模型

**无新迁移**。复用 `person.speaker_id` + `person_change_log` + `ManualSetPersonStatus` dismiss 语义。

## 6. 前端

- 删除声纹响应带 `cascade_prompts` 时，前端 `confirm` 或内联提示：「声纹已删除。其关联人物「X」曾被手工编辑，是否一并删除？」→ 是则调人物 dismiss 端点。
- 合并声纹响应同样处理。

## 7. 测试

- repo 层：`personEditedByUser` 判定（有 user 非create→true；仅 llm→false；仅 user create→false；查询失败→true）。
- handler 层：删声纹→未编辑人物被 dismiss、人物仍在表内但 status=dismissed；编辑过人物不被删且在响应 prompts；owner 不删；无关联人物不报错。
- 合并：源人物同样级联。

## 8. 风险

- **误删风险**：编辑判定依赖 change_log 完整性——若历史编辑未记审计会漏判。查失败时保守「视为已编辑」不静默删。
- **owner 保护**：is_owner 必须硬保护，绝级联删除。
- **跨人物关联**：人物可能被属性/事件关联到多处，dismiss 是可恢复软删，风险可控。
```
