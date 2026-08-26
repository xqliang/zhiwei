# 用户画像 P2b（大事记前端）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 人物 tab 消费 P2a 的 event 平面：详情卡「大事记」区（按年分组时间线 + 手动加/删事件）+ 确认队列 event 条目渲染。纯前端，不改后端。

**Architecture:** 沿用 P1b 模式：详情数据已在 `personDetail.events`（active+pending、occurred_at DESC）；队列条目 `kind==="event"` 已带 event_type/occurred_at。状态管理与清理全部对齐 P1b 的属性/关系模式（切换人物 closePersonDetail 清理、草稿对称、防双击 queueBusyIds 复用）。

**Tech Stack:** Vue 3 CDN 单文件（web/app.js + web/index.html），既有 CSS 类。

**契约（已核对 internal/api/person.go + internal/repo/person_event.go）：**
- 详情 `personDetail.events[]`：`id/event_type/title/description?/occurred_at?/end_at?/location?/related_person_ids/source(llm|manual)/status(active|pending)/confidence/session_id?`
- 队列 `pendingItem`（kind=event）：`value`=title、`event_type`、`occurred_at?`、`confidence`、`person_name`、`session_id?`
- `POST /api/persons/{id}/events`：`{event_type(9 枚举), title, description?, occurred_at?, end_at?, location?, related_person_id?}`（原始日期串，后端 parseEventAt 尽力解析；空 title 400、非法类型 400）
- `DELETE /api/persons/{id}/events/{eid}`（软删 dismissed）
- 队列确认/放弃：`POST /api/profile/pending/event/{id}/confirm|dismiss`（P1b 的 queueBusyIds 防双击已按 kind-id 键工作，天然覆盖 event）

**工作目录：** worktree `.worktrees/person-event-ui`（分支 `feat/person-event-ui`），`cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.worktrees/person-event-ui`

**验证约定：** `node --check web/app.js` + 契约比对；浏览器终验（Task 3 手动清单）。dev 端口 **8081**（.env ZW_PORT）。

---

### Task 1: 详情「大事记」区——按年分组时间线 + 手动加/删事件

**Files:** Modify `web/app.js` + `web/index.html`

**app.js**（人物区块、关系管理之后追加）：

```js
    // ---------- 大事记（event 平面，P2） ----------
    // 事件类型枚举与后端 ValidEventTypes 一致（9 类）
    const EVENT_TYPES = ['里程碑','聚会','会议','旅行','健康','成就','挫折','负面','其他'];

    const showAddEvent = ref(false);
    const addEventForm = reactive({ event_type: '', title: '', description: '', occurred_at: '', end_at: '', location: '', related_person_id: '' });
    const addingEvent = ref(false);
    const deletingEventId = ref(null);  // 2 步删除确认

    // 按年分组（时间倒序）：occurred_at 为空的事件归「时间未知」组排最后。
    // 后端 ListByPerson 已按 occurred_at DESC 排（NULL 沉底），这里只分桶保持顺序。
    const eventsByYear = computed(() => {
      if (!personDetail.value || !personDetail.value.events) return [];
      const map = {}, order = [];
      for (const e of personDetail.value.events) {
        const k = e.occurred_at ? String(e.occurred_at).slice(0, 4) : '时间未知';
        if (!map[k]) { map[k] = []; order.push(k); }
        map[k].push(e);
      }
      return order.map(k => ({ year: k, items: map[k] }));
    });
    // 事件日期展示：occurred_at ISO → 「M月D日」；空显「—」
    function fmtEventDate(iso) {
      if (!iso) return '—';
      const d = new Date(iso);
      return isNaN(d.getTime()) ? iso : `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`;
    }
    function resetAddEventForm() {
      addEventForm.event_type = ''; addEventForm.title = ''; addEventForm.description = '';
      addEventForm.occurred_at = ''; addEventForm.end_at = ''; addEventForm.location = ''; addEventForm.related_person_id = '';
    }
    // 开合切换：收起清草稿（对齐 toggleAddAttr/toggleAddRel 对称模式）
    function toggleAddEvent() {
      if (showAddEvent.value) { showAddEvent.value = false; resetAddEventForm(); return; }
      showAddEvent.value = true;
    }
    async function submitAddEvent() {
      if (addingEvent.value) return;
      const et = addEventForm.event_type;
      const title = addEventForm.title.trim();
      if (!et) { toast.value = '请选择事件类型'; setTimeout(() => { toast.value = ''; }, 2000); return; }
      if (!title) { toast.value = '请输入标题'; setTimeout(() => { toast.value = ''; }, 2000); return; }
      addingEvent.value = true;
      try {
        // 日期用 <input type="date"> 的 YYYY-MM-DD；可选字段仅非空才传（后端 parseEventAt 尽力解析）
        const body = { event_type: et, title };
        if (addEventForm.description.trim()) body.description = addEventForm.description.trim();
        if (addEventForm.occurred_at) body.occurred_at = addEventForm.occurred_at;
        if (addEventForm.end_at) body.end_at = addEventForm.end_at;
        if (addEventForm.location.trim()) body.location = addEventForm.location.trim();
        if (addEventForm.related_person_id) body.related_person_id = addEventForm.related_person_id;
        await api('POST', '/api/persons/' + personDetail.value.person.id + '/events', body);
        await reloadPersonDetail();
        showAddEvent.value = false;
        resetAddEventForm();
      } catch (e) { showError(e); }
      finally { addingEvent.value = false; }
    }
    function askDeleteEvent(ev) { deletingEventId.value = ev.id; }
    async function confirmDeleteEvent() {
      const id = deletingEventId.value;
      if (!id) return;
      try {
        await api('DELETE', '/api/persons/' + personDetail.value.person.id + '/events/' + id);
        deletingEventId.value = null;
        await reloadPersonDetail(); await loadPersons(); // pending 计数可能变化
      } catch (e) { showError(e); }
    }
```

closePersonDetail 追加：`showAddEvent.value = false; resetAddEventForm(); deletingEventId.value = null;`

return 导出追加：
```js
      EVENT_TYPES, showAddEvent, addEventForm, addingEvent, toggleAddEvent, submitAddEvent, eventsByYear, fmtEventDate, deletingEventId, askDeleteEvent, confirmDeleteEvent,
```

**index.html**（详情卡内、关系区之后、加属性表单之前插入）：

```html
      <!-- 大事记（按年分组，时间倒序；与属性/关系互补：属性记「有过的」，大事记记「某次发生的」） -->
      <div v-if="personDetail.events && personDetail.events.length" style="margin-bottom:12px">
        <div class="muted" style="font-weight:600; margin-bottom:4px">大事记</div>
        <div v-for="g in eventsByYear" :key="g.year" style="margin-bottom:8px">
          <div class="muted" style="font-size:var(--fs-xs); margin:4px 0">{{ g.year }}</div>
          <div v-for="ev in g.items" :key="ev.id" class="seg">
            <span class="chip" style="background:var(--surface-sunken); color:var(--text-2)">{{ ev.event_type }}</span>
            <div class="seg-text">
              <div>
                <b>{{ ev.title }}</b>
                <span class="muted"> · {{ fmtEventDate(ev.occurred_at) }}</span>
                <span v-if="ev.location" class="muted"> · {{ ev.location }}</span>
                <span v-if="ev.event_type === '旅行' && ev.end_at" class="muted"> 至 {{ fmtEventDate(ev.end_at) }}</span>
                <span class="chip" :style="ev.source === 'llm' ? { background:'var(--accent-soft)', color:'var(--accent)' } : { background:'var(--ok-soft)', color:'var(--ok)' }">{{ ev.source === 'llm' ? 'AI' : '人工' }}</span>
                <span v-if="ev.status === 'pending'" class="chip" style="background:var(--warn-soft); color:var(--warn)">待确认</span>
              </div>
              <div v-if="ev.description" class="muted" style="font-size:var(--fs-xs)">{{ ev.description }}</div>
            </div>
            <div style="display:flex; gap:4px; flex-shrink:0">
              <template v-if="deletingEventId === ev.id">
                <span class="muted">删除？</span>
                <button class="btn mini danger" @click="confirmDeleteEvent">确认</button>
                <button class="btn mini" @click="deletingEventId=null">取消</button>
              </template>
              <button v-else class="btn mini danger" @click="askDeleteEvent(ev)" title="删除事件">🗑</button>
            </div>
          </div>
        </div>
      </div>

      <!-- 加事件表单 -->
      <div class="card sunken" v-if="showAddEvent" style="margin-bottom:10px">
        <div class="kv" style="margin-bottom:8px"><b>加大事记</b><button class="btn-link" @click="toggleAddEvent">✕</button></div>
        <div style="display:flex; gap:8px; flex-wrap:wrap; align-items:center">
          <select class="txt" v-model="addEventForm.event_type" style="min-width:100px">
            <option value="" disabled>事件类型</option>
            <option v-for="t in EVENT_TYPES" :key="t" :value="t">{{ t }}</option>
          </select>
          <input class="txt" v-model="addEventForm.title" placeholder="标题（如 去云南旅游一周）*" style="flex:1; min-width:140px">
          <input class="txt" v-model="addEventForm.occurred_at" type="date" title="发生日期" style="min-width:130px">
          <input class="txt" v-model="addEventForm.end_at" type="date" title="结束日期（旅行/会议等跨天事件）" style="min-width:130px">
          <input class="txt" v-model="addEventForm.location" placeholder="地点（可空）" style="flex:1; min-width:100px">
          <select class="txt" v-model="addEventForm.related_person_id" style="min-width:120px" title="同场主要人物">
            <option value="">（同行人物，可空）</option>
            <option v-for="p in persons.filter(x => x.id !== personDetail.person.id)" :key="p.id" :value="p.id">{{ p.display_name }}</option>
          </select>
          <button class="btn primary" :disabled="addingEvent" @click="submitAddEvent">{{ addingEvent ? '保存中…' : '保存' }}</button>
        </div>
        <input class="txt" v-model="addEventForm.description" placeholder="细节（可空）" style="width:100%; margin-top:8px">
        <div class="muted" style="font-size:var(--fs-xs); margin-top:4px">手动添加立即生效（来源=人工 · 置信度 100%）；日期可选，只知道大概时间可留空。</div>
      </div>
```

底部操作按钮行（加属性/加关系旁）加：`<button class="btn mini" @click="toggleAddEvent">{{ showAddEvent ? '收起加大事记' : '＋ 加大事记' }}</button>`

验证：`node --check web/app.js`。Commit: `feat(web): 人物大事记——按年分组时间线+手动加删事件`

### Task 2: 确认队列 event 条目渲染

**Files:** Modify `web/app.js` + `web/index.html`

**app.js：**
- `pendingKindText` 加 `event: '大事记'`
- `pendingSummary` 加分支：`if (it.kind === 'event') return (it.event_type || '') + '：' + (it.value || '');`

**index.html** 队列条目行：
- 摘要行已由 pendingSummary 覆盖（event_type：title）
- 置信度行补日期显示：`<span v-if="it.occurred_at"> · {{ fmtEventDate(it.occurred_at) }}</span>`（加在置信度 span 之后）
- event 条目的置信度行保留（事件有真实抽取置信度，与 person kind 不同——不用 v-if 排除）

验证 + Commit: `feat(web): 确认队列渲染大事记条目`

### Task 3: hash 重建 + 冒烟 + 手动清单

**Files:** Modify `README.md`（如需）+ 新建手动清单文档

1. `bash scripts/hash-web.sh`（重建 hash 副本）+ `git add web/index.html`（src 变更须提交，对齐 main 自洽惯例）
2. `make dev-restart`（注意端口 8081）+ curl 冒烟：`POST /api/persons/{owner}/events`（造一条旅行事件）→ `GET /api/persons/{owner}`（详情含 events）→ `DELETE`（清理测试数据）
3. 手动清单追加到 `docs/superpowers/plans/2026-08-24-person-p1b-manual-checklist.md` 末尾：

```markdown
## P2b 大事记验收（追加）

14. 详情「大事记」区：手动加「旅行·去云南旅游一周」（选日期）→ 按年分组出现（人工徽标）
15. 加低置信事件路径：从历史抽取画像 → 队列出现「大事记」条目（类型：标题+日期+置信度）→ 确认 → 详情事件转正
16. 事件删除 → 2 步确认 → 消失；名册 pending 角标联动
17. 切换人物再切回：加事件草稿不残留（对称清理）
```

4. Commit: `docs(web): P2b 大事记手动验收清单 + hash 同步`

---

## 计划自检

1. **覆盖**：P2b 范围（详情大事记/队列 event/手动加删）全落位；契约逐字段核对过。
2. **模式一致性**：全部对齐 P1b 既有模式（toggleAddRel 对称草稿/deletingRelId 2 步确认/closePersonDetail 清理/queueBusyIds 天然复用）。
3. **明确不做**：事件编辑（改值留痕——事件无冲突路径，编辑语义未定义，后续需要再做）；大事记独立 tab（留在人物详情内，spec §8 未要求独立页）。
