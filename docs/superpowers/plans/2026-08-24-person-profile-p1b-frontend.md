# 用户画像 P1b（人物前端 tab）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 web 单页（Vue3 CDN 无构建）新增「人物」tab：名册 / 人物详情（分组属性+关系+最近互动）/ 属性与关系手动管理 / 修改历史 / 跨平面确认队列（确认/放弃）/ 从历史回填抽取——消费 P1a 已上线的 15 条 REST 路由，**不改后端**。

**Architecture:** 遵循 app.js 既有模式：setup() 组合式 API、refs+函数、`api()` helper、switchTab 按需加载、2 步行内确认、inplace 编辑。人物 tab 数据流：名册(GET /api/persons) → 点卡片展开详情(GET /api/persons/{id}) → 手动操作走 Service 语义端点 → 操作后局部刷新。确认队列独立于详情（tab 顶部全局区）。

**Tech Stack:** Vue 3 CDN（web/vendor）、原生 fetch、现有 CSS 类（card/kv/btn/chip/muted/txt/field-label/empty/seg）。

**明确不做（本期边界）：**
- 「共同 Topic / 相关 Todo」详情区——spec §7 提到但依赖记忆↔人物的关联查询，P1a 后端未提供；延到后续批次（届时后端加查询+前端加区块）。
- 人物合并（status=merged 的 UI）、大事记/曲线（P2+ 平面）。

**工作目录：** worktree `.worktrees/person-frontend`（分支 `feat/person-frontend`），`cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.worktrees/person-frontend`

**验证约定（无 JS 测试框架）：**
- 每任务：`node --check web/app.js`（语法）+ 浏览器行为由 controller 终验
- 后端契约不变；涉及 API 的调用须与 `internal/api/person.go` 的请求/响应字段逐一对齐（写代码前先读它）
- 最终冒烟（Task 7）：起服务 + curl 全路由 + 浏览器手动清单

**前端硬编码目录（第 4 份拷贝，已知取舍）：** 加属性表单的 key 建议用 datalist 硬编码 47 个 key（与 `internal/profile/catalog.go` 一致；漂移风险记录在 spec §13 已知限制）。**不要**为它加后端端点（YAGNI）。

---

### Task 1: 人物 tab 骨架——导航/状态/名册/新建人物

**Files:**
- Modify: `web/app.js`（新增「---------- 人物 ----------」区块 + switchTab 接线 + return 导出）
- Modify: `web/index.html`（tabs 加按钮 + 名册 wrap）

- [ ] **Step 1: app.js 加人物状态与方法**

在 `// ---------- 声纹 tab ...` 区块之前（约 934 行前）插入：

```js
    // ---------- 人物 tab（用户画像：名册 / 详情 / 确认队列 / 回填） ----------
    // 后端契约见 internal/api/person.go：读直连 repo 响应、变更走 Service（审计+事务）。
    const persons = ref([]);            // 名册（GET /api/persons → {persons:[PersonWithPending]}）
    const personDetail = ref(null);     // 当前详情（GET /api/persons/{id} → {person,groups,relationships,recent_session_ids,pending_count}）
    const showNewPerson = ref(false);   // 新建人物表单开合
    const newPerson = ref({ display_name: '', speaker_id: '', summary: '' });
    const creatingPerson = ref(false);  // 防重复提交
    // 改名/编辑进行中的详情（就地一行输入；保存走 PATCH display_name）
    const renamingPerson = ref(null);   // { id, name }

    async function loadPersons() {
      try {
        const d = await api('GET', '/api/persons');
        persons.value = d.persons || [];
      } catch (e) { showError(e); }
    }
    function cancelNewPerson() {
      showNewPerson.value = false;
      newPerson.value = { display_name: '', speaker_id: '', summary: '' };
      creatingPerson.value = false;
    }
    async function createPerson() {
      if (creatingPerson.value) return;
      const name = newPerson.value.display_name.trim();
      if (!name) { toast.value = '请输入姓名'; setTimeout(() => { toast.value = ''; }, 2000); return; }
      creatingPerson.value = true;
      try {
        // speaker_id 可空（只被提到、没录音的人也能建档）；后端校验声纹冲突返回 409
        const body = { display_name: name };
        if (newPerson.value.speaker_id.trim()) body.speaker_id = newPerson.value.speaker_id.trim();
        if (newPerson.value.summary.trim()) body.summary = newPerson.value.summary.trim();
        await api('POST', '/api/persons', body);
        cancelNewPerson();
        await loadPersons();
        toast.value = '人物已创建'; setTimeout(() => { toast.value = ''; }, 2000);
      } catch (e) { showError(e); }
      finally { creatingPerson.value = false; }
    }
    // 点名册卡片：已展开收起；否则拉详情（内联展开，同 topics tab 的 topicDetail 模式但就地）
    async function togglePerson(id) {
      if (personDetail.value && personDetail.value.person.id === id) { closePersonDetail(); return; }
      try { personDetail.value = await api('GET', '/api/persons/' + id); }
      catch (e) { showError(e); }
    }
    function closePersonDetail() {
      personDetail.value = null;
      renamingPerson.value = null;
      attrHistory.value = null;       // 收起历史抽屉
      showAddAttr.value = false;      // 收起加属性表单
      showAddRel.value = false;       // 收起加关系表单
    }
    async function reloadPersonDetail() {
      if (!personDetail.value) return;
      try { personDetail.value = await api('GET', '/api/persons/' + personDetail.value.person.id); }
      catch (e) { showError(e); }
    }
    // 人物改名（就地编辑，PATCH display_name；不传 speaker_id/summary 保持不变——后端 nil=不改语义）
    function startRenamePerson() {
      renamingPerson.value = { id: personDetail.value.person.id, name: personDetail.value.person.display_name };
    }
    async function commitRenamePerson() {
      const rn = renamingPerson.value;
      renamingPerson.value = null;
      if (!rn || !rn.name.trim()) return;
      try {
        await api('PATCH', '/api/persons/' + rn.id, { display_name: rn.name.trim() });
        await reloadPersonDetail();
        await loadPersons();
      } catch (e) {
        renamingPerson.value = rn; // 失败恢复编辑态防输入丢失
        showError(e);
      }
    }
    // 人物归档（2 步确认；DELETE = status=dismissed 软删）
    const archivingPersonId = ref(null);
    function askArchivePerson(p) { archivingPersonId.value = p.id; }
    function cancelArchivePerson() { archivingPersonId.value = null; }
    async function confirmArchivePerson(p) {
      try {
        await api('DELETE', '/api/persons/' + p.id);
        archivingPersonId.value = null;
        if (personDetail.value && personDetail.value.person.id === p.id) closePersonDetail();
        await loadPersons();
      } catch (e) { showError(e); }
    }
```

switchTab 函数里追加一行（voiceprint 行之后）：

```js
      if (name === 'persons') { closePersonDetail(); archivingPersonId.value = null; loadPersons(); loadPending(); }
```

return 对象末尾追加导出：

```js
      persons, personDetail, showNewPerson, newPerson, creatingPerson, loadPersons, cancelNewPerson, createPerson, togglePerson, closePersonDetail, reloadPersonDetail, renamingPerson, startRenamePerson, commitRenamePerson, archivingPersonId, askArchivePerson, cancelArchivePerson, confirmArchivePerson,
```

（`loadPending`/`attrHistory`/`showAddAttr`/`showAddRel` 在后续任务定义——**本任务先在 Task 2/3/4 定义后再接线 switchTab 那行**；为避免编译期引用未定义，本任务的 switchTab 行先只写 `loadPersons()`，后续任务再补。修正：本任务 switchTab 行为 `if (name === 'persons') { closePersonDetail(); archivingPersonId.value = null; loadPersons(); }`，其中 closePersonDetail 内对未定义 ref 的引用改为安全写法——本任务先不引用 attrHistory/showAddAttr/showAddRel（它们 Task 3/4 才有），closePersonDetail 本任务版本只清 personDetail/renamingPerson，后续任务再补两行。）

- [ ] **Step 2: index.html 加 tab 按钮 + 名册区**

tabs 导航（声纹按钮后）加：

```html
    <button :class="{active: tab==='persons'}" @click="switchTab('persons')">人物</button>
```

在 topics wrap（`<div class="wrap" v-if="tab==='topics'">`）之前插入：

```html
  <!-- 人物 tab：名册 + 详情（就地展开）+ 确认队列 + 回填 -->
  <div class="wrap" v-if="tab==='persons'">
    <div class="kv" style="margin-bottom:12px">
      <b style="font-size:var(--fs-lg)">人物</b>
      <div style="display:flex; gap:6px">
        <button class="btn mini" @click="runBackfill" :disabled="backfilling">
          <span v-if="backfilling" class="spinner"></span>{{ backfilling ? '抽取中…' : '从历史抽取画像' }}
        </button>
        <button class="btn primary" style="padding:7px 16px" @click="showNewPerson = !showNewPerson">{{ showNewPerson ? '收起' : '＋ 新建' }}</button>
      </div>
    </div>

    <!-- 新建人物 -->
    <div class="card" v-if="showNewPerson">
      <div class="kv" style="margin-bottom:12px"><b>新建人物</b><button class="btn-link" @click="cancelNewPerson">✕</button></div>
      <label class="field-label">姓名 *</label>
      <input class="txt" v-model="newPerson.display_name" placeholder="如：张三" style="margin-bottom:8px">
      <label class="field-label">关联声纹 speaker_id（可空；只被提到的人可不绑）</label>
      <input class="txt" v-model="newPerson.speaker_id" placeholder="从声纹 tab 复制 id，可留空" style="margin-bottom:8px">
      <label class="field-label">备注</label>
      <input class="txt" v-model="newPerson.summary" placeholder="一句话备注（可空）" style="margin-bottom:8px">
      <div class="edit-actions">
        <button class="btn primary" :disabled="creatingPerson" @click="createPerson">
          <span v-if="creatingPerson" class="spinner"></span>{{ creatingPerson ? '创建中…' : '创建' }}
        </button>
        <button class="btn mini" @click="cancelNewPerson">取消</button>
      </div>
    </div>

    <!-- 名册卡片 -->
    <div v-if="!persons.length" class="card empty">还没有人物。点「新建」或「从历史抽取画像」自动生成。</div>
    <div class="card" v-for="p in persons" :key="p.id" style="cursor:pointer" @click="togglePerson(p.id)">
      <div class="kv">
        <div style="display:flex; align-items:center; gap:8px; min-width:0; flex-wrap:wrap">
          <b>{{ p.display_name }}</b>
          <span v-if="p.is_owner" class="chip" style="cursor:default; background:var(--accent-soft); color:var(--accent)">我</span>
          <span v-if="p.speaker_id" class="muted" title="已关联声纹">🎙</span>
          <span v-if="p.source === 'llm'" class="muted">自动识别</span>
        </div>
        <div style="display:flex; gap:6px; align-items:center; flex-shrink:0" @click.stop>
          <span v-if="p.pending_count" class="chip" style="cursor:default; background:var(--warn-soft); color:var(--warn)" title="待确认的属性/关系数">{{ p.pending_count }} 待确认</span>
          <template v-if="archivingPersonId === p.id">
            <span class="muted">归档？</span>
            <button class="btn mini danger" @click="confirmArchivePerson(p)">确认</button>
            <button class="btn mini" @click="cancelArchivePerson()">取消</button>
          </template>
          <button v-else class="btn mini" @click="askArchivePerson(p)">归档</button>
        </div>
      </div>
    </div>
  </div>
```

- [ ] **Step 3: 语法验证 + 提交**

```bash
node --check web/app.js && echo JS_OK
git add web/app.js web/index.html
git commit -m "feat(web): 人物 tab 骨架——名册/新建/归档"
```

（`runBackfill`/`backfilling` 在 Task 6 定义——**本任务模板里的回填按钮先不加**，Task 6 再加。名册模板顶部按钮区本任务只放「＋ 新建」。）

---

### Task 2: 人物详情——分组属性区 / 关系区 / 最近互动

**Files:**
- Modify: `web/app.js`（详情渲染辅助：epistemic/来源徽标）
- Modify: `web/index.html`（详情卡模板，插在名册 v-for 卡片之后）

- [ ] **Step 1: app.js 加徽标辅助函数**

人物区块内追加：

```js
    // epistemic → 中文标签（属性徽标；与后端 observed|inferred|predicted|suggested 对齐）
    function epiText(t) {
      return { observed: '直述', inferred: '推断', predicted: '预测', suggested: '建议' }[t] || t;
    }
    // 属性/关系行来源徽标：manual=人工（蓝）、llm=AI（紫）
    function srcClass(s) { return s === 'llm' ? 'llm-src' : 'manual-src'; }
```

- [ ] **Step 2: index.html 详情卡**

在名册 `v-for` 卡片之后、人物 wrap 结束前插入：

```html
    <!-- 人物详情（就地展开在名册之后；收起按钮在卡头） -->
    <div class="card" v-if="personDetail" style="border-color:var(--accent)">
      <div class="kv" style="margin-bottom:12px">
        <div style="display:flex; align-items:center; gap:8px; min-width:0; flex-wrap:wrap">
          <template v-if="!renamingPerson">
            <b style="font-size:var(--fs-md)">{{ personDetail.person.display_name }}</b>
            <span v-if="personDetail.person.is_owner" class="chip" style="cursor:default; background:var(--accent-soft); color:var(--accent)">我</span>
            <span v-if="personDetail.person.speaker_id" class="muted">🎙 {{ personDetail.person.speaker_id }}</span>
            <button class="btn-link" @click="startRenamePerson" title="改名">✎</button>
          </template>
          <template v-else>
            <input class="txt inline" v-model="renamingPerson.name" @keyup.enter="commitRenamePerson" @keyup.esc="renamingPerson=null">
            <button class="btn primary" style="padding:6px 12px" @click="commitRenamePerson">保存</button>
            <button class="btn mini" @click="renamingPerson=null">取消</button>
          </template>
        </div>
        <button class="btn mini" @click="closePersonDetail">收起 ✕</button>
      </div>
      <div v-if="personDetail.person.summary" class="muted" style="margin-bottom:8px">{{ personDetail.person.summary }}</div>

      <!-- 最近互动（溯源 session，点击跳时间线展开） -->
      <div v-if="personDetail.recent_session_ids && personDetail.recent_session_ids.length" style="margin-bottom:10px">
        <span class="muted" style="font-size:var(--fs-xs)">最近互动</span>
        <span v-for="sid in personDetail.recent_session_ids" :key="sid" class="chip" style="cursor:pointer; margin-left:4px"
              @click="jumpToSession(sid)" title="在时间线打开该录音">🎤</span>
      </div>

      <!-- 分组属性区 -->
      <div v-for="g in personDetail.groups" :key="g.group" style="margin-bottom:12px">
        <div class="muted" style="font-weight:600; margin-bottom:4px">{{ g.group }}</div>
        <div v-for="a in g.attrs" :key="a.id" class="seg">
          <div class="seg-text">
            <div>
              {{ a.attr_key }}：<b>{{ a.value_text }}</b>
              <span class="chip mini-chip" :class="srcClass(a.source)" style="cursor:default">{{ a.source === 'llm' ? 'AI' : '人工' }}</span>
              <span class="chip mini-chip" style="cursor:default" :title="'置信度 ' + a.confidence">{{ (a.confidence * 100).toFixed(0) }}%</span>
              <span class="chip mini-chip" style="cursor:default; background:var(--surface-sunken); color:var(--text-2)">{{ epiText(a.epistemic_type) }}</span>
              <span v-if="a.status === 'pending'" class="chip mini-chip" style="cursor:default; background:var(--warn-soft); color:var(--warn)">待确认</span>
            </div>
          </div>
          <div style="display:flex; gap:4px; flex-shrink:0">
            <button class="btn mini" @click="startEditAttr(a)" title="改值（旧行留痕）">✎</button>
            <button class="btn mini" @click="showAttrHistory(a)" title="修改历史">⟲</button>
            <button class="btn mini danger" @click="askDeleteAttr(a)" title="删除">🗑</button>
          </div>
        </div>
      </div>
      <div v-if="!personDetail.groups || !personDetail.groups.length" class="muted" style="margin-bottom:12px">暂无属性。</div>

      <!-- 关系区 -->
      <div v-if="personDetail.relationships && personDetail.relationships.length" style="margin-bottom:12px">
        <div class="muted" style="font-weight:600; margin-bottom:4px">关系</div>
        <div v-for="rel in personDetail.relationships" :key="rel.id" class="seg">
          <div class="seg-text">
            <div>
              <b>{{ rel.relation_type }}</b>
              <span v-if="rel.related_person_id" class="muted"> → {{ personNameOf(rel.related_person_id) }}</span>
              <span v-if="rel.org_name" class="muted"> · {{ rel.org_name }}</span>
              <span v-if="rel.label" class="muted"> · {{ rel.label }}</span>
              <span v-if="rel.direction" class="muted"> · {{ rel.direction }}</span>
              <span v-if="rel.status === 'pending'" class="chip mini-chip" style="cursor:default; background:var(--warn-soft); color:var(--warn)">待确认</span>
            </div>
          </div>
          <div style="display:flex; gap:4px; flex-shrink:0">
            <button class="btn mini danger" @click="askDeleteRel(rel)" title="删除关系">🗑</button>
          </div>
        </div>
      </div>

      <!-- 属性编辑（改值） / 历史抽屉 / 加属性 / 加关系：Task 3/4 填充 -->
      <div id="person-detail-forms-slot"></div>

      <div class="edit-actions">
        <button class="btn mini" @click="showAddAttr = !showAddAttr">{{ showAddAttr ? '收起加属性' : '＋ 加属性' }}</button>
        <button class="btn mini" @click="showAddRel = !showAddRel">{{ showAddRel ? '收起加关系' : '＋ 加关系' }}</button>
      </div>
    </div>
```

（`personNameOf`/`startEditAttr`/`showAttrHistory`/`askDeleteAttr`/`askDeleteRel`/`showAddAttr`/`showAddRel` 由 Task 3/4 提供；本任务模板里这些按钮/引用对应函数未定义会运行时报错——**本任务先不渲染这些按钮**：属性行操作按钮、关系删除按钮、底部两个加号按钮本任务全部去掉（只留分组展示与关系展示），Task 3/4 再逐个补回。`personNameOf` 本任务就要（关系区展示对端名），定义见下。）

- [ ] **Step 3: app.js 加 personNameOf**

人物区块内追加（关系对端名：从名册缓存查；详情页也常从这查）：

```js
    // 关系对端人物名：名册里查（GET /api/persons 的缓存；dismissed 的查不到→显示 id）
    function personNameOf(id) {
      const p = persons.value.find(x => x.id === id);
      return p ? p.display_name : id;
    }
```

- [ ] **Step 4: 语法验证 + 提交**

```bash
node --check web/app.js && echo JS_OK
git add web/app.js web/index.html
git commit -m "feat(web): 人物详情——分组属性/关系/最近互动展示"
```

---

### Task 3: 属性手动管理——加/改/删/历史

**Files:**
- Modify: `web/app.js`
- Modify: `web/index.html`（替换 Task 2 留的占位/补回操作按钮）

- [ ] **Step 1: app.js 属性管理状态与方法**

人物区块内追加（ATTR_KEYS 为 datalist 建议项，硬编码与后端 catalog 一致）：

```js
    // 常用属性 key 建议（datalist；与 internal/profile/catalog.go 的 47 键一致，可自由输入目录外 key）
    const ATTR_KEYS = ['aliases','birthday','gender','zodiac','mbti','education','school','city','address','phone',
      'occupation','industry','office_location','work_start_time','work_end_time','commute_mode','often_travel','current_projects',
      'meal_time','cuisine','eats_spicy','eats_numbing','smokes','drinks','wears_makeup','perfume',
      'hobbies','skills','reading_now','books_read','movies_watched','music_listened','games_played','fav_celebrities','fav_anime','fav_movie_genres','catchphrases','invests_stocks',
      'cities_visited','places_traveled','has_car','car_brand','phone_brand',
      'recent_concerns','attention_topics','personality','chronic_diseases'];

    const showAddAttr = ref(false);          // 加属性表单开合
    const addAttrForm = reactive({ attr_key: '', value: '' });
    const addingAttr = ref(false);
    const editingAttr = ref(null);           // { id, attr_key, value }：就地改值
    const deletingAttrId = ref(null);        // 2 步删除确认
    const attrHistory = ref(null);           // { attr_key, items }：历史抽屉
    const attrHistoryLoading = ref(false);

    async function submitAddAttr() {
      if (addingAttr.value) return;
      const key = addAttrForm.attr_key.trim(), val = addAttrForm.value.trim();
      if (!key || !val) { toast.value = 'key 与值必填'; setTimeout(() => { toast.value = ''; }, 2000); return; }
      addingAttr.value = true;
      try {
        await api('POST', '/api/persons/' + personDetail.value.person.id + '/attributes', { attr_key: key, value: val });
        showAddAttr.value = false; addAttrForm.attr_key = ''; addAttrForm.value = '';
        await reloadPersonDetail(); await loadPersons(); // 名册 pending 计数可能变化
      } catch (e) { showError(e); }
      finally { addingAttr.value = false; }
    }
    // 改值 = PATCH（后端 supersede 旧行留痕；body 必须带行自身的 attr_key）
    function startEditAttr(a) { deletingAttrId.value = null; editingAttr.value = { id: a.id, attr_key: a.attr_key, value: a.value_text }; }
    async function commitEditAttr() {
      const e = editingAttr.value;
      if (!e || !e.value.trim()) return;
      try {
        await api('PATCH', '/api/persons/' + personDetail.value.person.id + '/attributes/' + e.id,
          { attr_key: e.attr_key, value: e.value.trim() });
        editingAttr.value = null;
        await reloadPersonDetail();
      } catch (e2) { showError(e2); }
    }
    function askDeleteAttr(a) { editingAttr.value = null; deletingAttrId.value = a.id; }
    async function confirmDeleteAttr() {
      const id = deletingAttrId.value;
      if (!id) return;
      try {
        await api('DELETE', '/api/persons/' + personDetail.value.person.id + '/attributes/' + id);
        deletingAttrId.value = null;
        await reloadPersonDetail(); await loadPersons();
      } catch (e) { showError(e); }
    }
    // 修改历史抽屉：GET /api/persons/{id}/history?entity_kind=attribute&attr_key=X
    async function showAttrHistory(a) {
      attrHistory.value = { attr_key: a.attr_key, items: [] };
      attrHistoryLoading.value = true;
      try {
        const d = await api('GET', '/api/persons/' + personDetail.value.person.id +
          '/history?entity_kind=attribute&attr_key=' + encodeURIComponent(a.attr_key));
        attrHistory.value = { attr_key: a.attr_key, items: d.history || [] };
      } catch (e) { showError(e); attrHistory.value = null; }
      finally { attrHistoryLoading.value = false; }
    }
    // change_log 变更类型 → 中文（历史抽屉行徽标）
    function changeText(t) {
      return { create: '新建', update: '修改', confirm: '确认', dismiss: '放弃', supersede: '替换', delete: '删除', reaffirm: '佐证' }[t] || t;
    }
    // 历史 old/new_value 是 JSON 快照文本（如 "医生"），剥引号展示
    function snapText(v) { if (v == null) return ''; try { return JSON.parse(v); } catch (e) { return v; } }
```

closePersonDetail 补两行（Task 1 已建函数体内追加）：

```js
      editingAttr.value = null; deletingAttrId.value = null; attrHistory.value = null; showAddAttr.value = false;
```

return 导出追加：

```js
      ATTR_KEYS, showAddAttr, addAttrForm, addingAttr, submitAddAttr, editingAttr, startEditAttr, commitEditAttr, deletingAttrId, askDeleteAttr, confirmDeleteAttr, attrHistory, attrHistoryLoading, showAttrHistory, changeText, snapText, epiText, srcClass,
```

- [ ] **Step 2: index.html 补属性操作按钮 + 表单区**

属性行（Task 2 的 `v-for="a in g.attrs"` 块）操作列替换为：

```html
          <div style="display:flex; gap:4px; flex-shrink:0">
            <template v-if="editingAttr && editingAttr.id === a.id">
              <input class="txt inline" v-model="editingAttr.value" @keyup.enter="commitEditAttr" @keyup.esc="editingAttr=null">
              <button class="btn primary" style="padding:6px 12px" @click="commitEditAttr">存</button>
              <button class="btn mini" @click="editingAttr=null">取消</button>
            </template>
            <template v-else-if="deletingAttrId === a.id">
              <span class="muted">删除？</span>
              <button class="btn mini danger" @click="confirmDeleteAttr">确认</button>
              <button class="btn mini" @click="deletingAttrId=null">取消</button>
            </template>
            <template v-else>
              <button class="btn mini" @click="startEditAttr(a)" title="改值（旧行留痕）">✎</button>
              <button class="btn mini" @click="showAttrHistory(a)" title="修改历史">⟲</button>
              <button class="btn mini danger" @click="askDeleteAttr(a)" title="删除">🗑</button>
            </template>
          </div>
```

详情卡末尾「加属性」表单（替换 Task 2 的 forms-slot 占位，加关系表单 Task 4 一并替换此 slot）：

```html
      <!-- 加属性表单 -->
      <div class="card sunken" v-if="showAddAttr" style="margin-bottom:10px">
        <div class="kv" style="margin-bottom:8px"><b>加属性</b><button class="btn-link" @click="showAddAttr=false">✕</button></div>
        <div style="display:flex; gap:8px; flex-wrap:wrap">
          <input class="txt" list="attr-keys" v-model="addAttrForm.attr_key" placeholder="属性 key（如 occupation）" style="flex:1; min-width:140px">
          <datalist id="attr-keys">
            <option v-for="k in ATTR_KEYS" :key="k" :value="k"></option>
          </datalist>
          <input class="txt" v-model="addAttrForm.value" placeholder="值（如 后端工程师）" style="flex:1; min-width:140px">
          <button class="btn primary" :disabled="addingAttr" @click="submitAddAttr">{{ addingAttr ? '保存中…' : '保存' }}</button>
        </div>
        <div class="muted" style="font-size:var(--fs-xs); margin-top:4px">手动添加立即生效（来源=人工 · 置信度 100%）；单值属性重复添加 = 改值，旧值留痕可查历史。</div>
      </div>

      <!-- 修改历史抽屉 -->
      <div class="card sunken" v-if="attrHistory" style="margin-bottom:10px">
        <div class="kv" style="margin-bottom:8px">
          <b>「{{ attrHistory.attr_key }}」修改历史</b>
          <button class="btn-link" @click="attrHistory=null">✕</button>
        </div>
        <div v-if="attrHistoryLoading" class="muted">加载中…</div>
        <template v-else>
          <div v-for="h in attrHistory.items" :key="h.id" class="seg">
            <span class="chip mini-chip" style="cursor:default"
                  :style="{ background: h.changed_by === 'llm' ? 'var(--accent-soft)' : 'var(--ok-soft)', color: h.changed_by === 'llm' ? 'var(--accent)' : 'var(--ok)' }">
              {{ h.changed_by === 'llm' ? 'AI' : '人工' }} · {{ changeText(h.change_type) }}
            </span>
            <div class="seg-text">
              <div>
                <template v-if="h.old_value && h.new_value">{{ snapText(h.old_value) }} → <b>{{ snapText(h.new_value) }}</b></template>
                <template v-else-if="h.new_value"><b>{{ snapText(h.new_value) }}</b></template>
                <template v-else-if="h.old_value">删除了 {{ snapText(h.old_value) }}</template>
              </div>
              <div class="muted" style="font-size:var(--fs-xs)">{{ fmtTime(h.created_at) }}<span v-if="h.note"> · {{ h.note }}</span></div>
            </div>
          </div>
          <div v-if="!attrHistory.items.length" class="muted">无历史记录。</div>
        </template>
      </div>
```

- [ ] **Step 3: 语法验证 + 提交**

```bash
node --check web/app.js && echo JS_OK
git add web/app.js web/index.html
git commit -m "feat(web): 人物属性手动管理——加/改(留痕)/删/历史抽屉"
```

---

### Task 4: 关系管理——加/删关系

**Files:**
- Modify: `web/app.js`
- Modify: `web/index.html`（关系行删除按钮 + 加关系表单，替换 forms-slot）

- [ ] **Step 1: app.js 关系管理**

人物区块内追加：

```js
    // 关系类型枚举（与后端 ValidRelations 一致）
    const RELATION_TYPES = ['配偶','子女','父母','兄弟姐妹','亲戚','朋友','同事','领导','下属','客户','供应商','合作方','组织','其他'];
    const DIRECTIONS = ['upstream','downstream','peer'];

    const showAddRel = ref(false);
    const addRelForm = reactive({ relation_type: '', related_person_id: '', label: '', direction: '', org_name: '' });
    const addingRel = ref(false);
    const deletingRelId = ref(null);  // 2 步删除确认

    async function submitAddRel() {
      if (addingRel.value) return;
      const rt = addRelForm.relation_type;
      if (!rt) { toast.value = '请选择关系类型'; setTimeout(() => { toast.value = ''; }, 2000); return; }
      addingRel.value = true;
      try {
        const body = { relation_type: rt };
        if (addRelForm.related_person_id.trim()) body.related_person_id = addRelForm.related_person_id.trim();
        if (addRelForm.label.trim()) body.label = addRelForm.label.trim();
        if (addRelForm.direction) body.direction = addRelForm.direction;
        if (addRelForm.org_name.trim()) body.org_name = addRelForm.org_name.trim();
        await api('POST', '/api/persons/' + personDetail.value.person.id + '/relationships', body);
        showAddRel.value = false;
        addRelForm.relation_type = ''; addRelForm.related_person_id = ''; addRelForm.label = ''; addRelForm.direction = ''; addRelForm.org_name = '';
        await reloadPersonDetail();
      } catch (e) { showError(e); }
      finally { addingRel.value = false; }
    }
    function askDeleteRel(rel) { deletingRelId.value = rel.id; }
    async function confirmDeleteRel() {
      const id = deletingRelId.value;
      if (!id) return;
      try {
        await api('DELETE', '/api/persons/' + personDetail.value.person.id + '/relationships/' + id);
        deletingRelId.value = null;
        await reloadPersonDetail();
      } catch (e) { showError(e); }
    }
```

closePersonDetail 补一行：`showAddRel.value = false; deletingRelId.value = null;`

return 导出追加：

```js
      RELATION_TYPES, DIRECTIONS, showAddRel, addRelForm, addingRel, submitAddRel, deletingRelId, askDeleteRel, confirmDeleteRel, personNameOf,
```

- [ ] **Step 2: index.html 关系行删除按钮 + 加关系表单**

关系行操作列（Task 2 关系 v-for 内）替换为：

```html
          <div style="display:flex; gap:4px; flex-shrink:0">
            <template v-if="deletingRelId === rel.id">
              <span class="muted">删除？</span>
              <button class="btn mini danger" @click="confirmDeleteRel">确认</button>
              <button class="btn mini" @click="deletingRelId=null">取消</button>
            </template>
            <button v-else class="btn mini danger" @click="askDeleteRel(rel)" title="删除关系">🗑</button>
          </div>
```

加关系表单（与加属性表单并列，替换 forms-slot 区域追加）：

```html
      <!-- 加关系表单 -->
      <div class="card sunken" v-if="showAddRel" style="margin-bottom:10px">
        <div class="kv" style="margin-bottom:8px"><b>加关系</b><button class="btn-link" @click="showAddRel=false">✕</button></div>
        <div style="display:flex; gap:8px; flex-wrap:wrap; align-items:center">
          <select class="txt" v-model="addRelForm.relation_type" style="min-width:100px">
            <option value="" disabled>关系类型</option>
            <option v-for="t in RELATION_TYPES" :key="t" :value="t">{{ t }}</option>
          </select>
          <span class="muted">→</span>
          <select class="txt" v-model="addRelForm.related_person_id" style="min-width:140px">
            <option value="">（对端人物，可空）</option>
            <option v-for="p in persons.filter(x => x.id !== personDetail.person.id)" :key="p.id" :value="p.id">{{ p.display_name }}</option>
          </select>
          <input class="txt" v-model="addRelForm.label" placeholder="称呼（如 老婆/大儿子，可空）" style="flex:1; min-width:120px">
          <select class="txt" v-model="addRelForm.direction" style="min-width:100px">
            <option value="">（上下游，可空）</option>
            <option v-for="d in DIRECTIONS" :key="d" :value="d">{{ d }}</option>
          </select>
          <input class="txt" v-model="addRelForm.org_name" placeholder="组织名（组织关系用，可空）" style="flex:1; min-width:120px">
          <button class="btn primary" :disabled="addingRel" @click="submitAddRel">{{ addingRel ? '保存中…' : '保存' }}</button>
        </div>
      </div>
```

- [ ] **Step 3: 语法验证 + 提交**

```bash
node --check web/app.js && echo JS_OK
git add web/app.js web/index.html
git commit -m "feat(web): 人物关系管理——加/删（14 类型枚举+对端选择）"
```

---

### Task 5: 确认队列——全局待确认区（确认/放弃）

**Files:**
- Modify: `web/app.js`
- Modify: `web/index.html`（人物 wrap 顶部，名册之前）

- [ ] **Step 1: app.js 确认队列**

人物区块内追加：

```js
    // ---------- 确认队列（跨平面 pending 并集；与名册/详情独立刷新） ----------
    const pendingItems = ref([]);
    const pendingLoading = ref(false);
    async function loadPending() {
      pendingLoading.value = true;
      try {
        const d = await api('GET', '/api/profile/pending');
        pendingItems.value = d.items || [];
      } catch (e) { showError(e); }
      finally { pendingLoading.value = false; }
    }
    // 确认/放弃后三处联动刷新：队列本身 + 名册（pending 计数）+ 当前详情（若相关）
    async function refreshAfterQueue() {
      await loadPending();
      await loadPersons();
      if (personDetail.value) await reloadPersonDetail();
    }
    async function confirmPendingItem(it) {
      try {
        await api('POST', '/api/profile/pending/' + it.kind + '/' + it.id + '/confirm');
        await refreshAfterQueue();
      } catch (e) { showError(e); }
    }
    async function dismissPendingItem(it) {
      try {
        await api('POST', '/api/profile/pending/' + it.kind + '/' + it.id + '/dismiss');
        await refreshAfterQueue();
      } catch (e) { showError(e); }
    }
    // 队列条目摘要（kind 不同字段不同：attribute=建议值，relationship=类型，person=名字）
    function pendingSummary(it) {
      if (it.kind === 'attribute') return (it.attr_key || '') + '：' + (it.value || '');
      if (it.kind === 'relationship') return (it.relation_type || '') + (it.label ? '（' + it.label + '）' : '');
      return it.value || it.person_name; // person：名字
    }
    function pendingKindText(k) {
      return { attribute: '属性', relationship: '关系', person: '新人物' }[k] || k;
    }
```

switchTab 的 persons 行补 `loadPending()`（Task 1 建的行改为）：

```js
      if (name === 'persons') { closePersonDetail(); archivingPersonId.value = null; loadPersons(); loadPending(); }
```

return 导出追加：

```js
      pendingItems, pendingLoading, loadPending, confirmPendingItem, dismissPendingItem, pendingSummary, pendingKindText,
```

- [ ] **Step 2: index.html 队列区（人物 wrap 顶部、新建表单之前）**

```html
    <!-- 确认队列：低置信/冲突的 AI 建议（全局，与人物详情无关） -->
    <div class="card" v-if="pendingItems.length" style="border-color:var(--warn)">
      <div class="kv" style="margin-bottom:8px">
        <b style="color:var(--warn)">待确认 <span class="chip" style="cursor:default; background:var(--warn-soft); color:var(--warn)">{{ pendingItems.length }}</span></b>
        <span class="muted" style="font-size:var(--fs-xs)">AI 从对话中抽取的低置信/冲突信息，确认后生效</span>
      </div>
      <div v-for="it in pendingItems" :key="it.kind + '-' + it.id" class="seg" style="align-items:flex-start">
        <span class="chip mini-chip" style="cursor:default; background:var(--surface-sunken); color:var(--text-2)">{{ pendingKindText(it.kind) }}</span>
        <div class="seg-text">
          <div>
            <b>{{ it.person_name }}</b> · {{ pendingSummary(it) }}
            <span v-if="it.current_value" class="muted">（现值：{{ it.current_value }} → 建议改为 <b>{{ it.value }}</b>）</span>
          </div>
          <div class="muted" style="font-size:var(--fs-xs)">
            置信度 {{ (it.confidence * 100).toFixed(0) }}%
            <span v-if="it.session_id" style="cursor:pointer; text-decoration:underline" @click="jumpToSession(it.session_id)">来源录音</span>
          </div>
        </div>
        <div style="display:flex; gap:4px; flex-shrink:0">
          <button class="btn mini" style="background:var(--ok-soft); color:var(--ok); border-color:var(--ok)" @click="confirmPendingItem(it)">✓ 确认</button>
          <button class="btn mini danger" @click="dismissPendingItem(it)">✕ 放弃</button>
        </div>
      </div>
    </div>
    <div v-else-if="pendingLoading" class="card empty">加载待确认项…</div>
```

- [ ] **Step 3: 语法验证 + 提交**

```bash
node --check web/app.js && echo JS_OK
git add web/app.js web/index.html
git commit -m "feat(web): 确认队列——跨平面待确认区（冲突并排/来源跳转/确认放弃）"
```

---

### Task 6: 回填抽取——「从历史抽取画像」按钮

**Files:**
- Modify: `web/app.js`
- Modify: `web/index.html`（人物 wrap 顶部按钮区加回填按钮，Task 1 模板预留位）

- [ ] **Step 1: app.js 回填**

人物区块内追加：

```js
    // ---------- 从历史回填抽取（POST /api/profile/extract：不带 session_id = 最近 50 个 completed，同步） ----------
    const backfilling = ref(false);
    const backfillInfo = ref(null); // { processed, active, pending, skipped, errors }
    async function runBackfill() {
      if (backfilling.value) return;
      backfilling.value = true;
      backfillInfo.value = null;
      try {
        const d = await api('POST', '/api/profile/extract', {});
        const rs = d.results || [];
        backfillInfo.value = {
          processed: d.processed || rs.length,
          active: rs.reduce((s, r) => s + (r.active || 0), 0),
          pending: rs.reduce((s, r) => s + (r.pending || 0), 0),
          skipped: rs.reduce((s, r) => s + (r.skipped || 0), 0),
          errors: rs.filter(r => r.error).length,
        };
        await refreshAfterQueue(); // 新 pending 可能进队列
      } catch (e) { showError(e); }
      finally { backfilling.value = false; }
    }
```

return 导出追加：`backfilling, backfillInfo, runBackfill,`

- [ ] **Step 2: index.html 回填按钮 + 结果摘要**

人物 wrap 顶部 kv 按钮区（Task 1 预留）加：

```html
        <button class="btn mini" @click="runBackfill" :disabled="backfilling">
          <span v-if="backfilling" class="spinner"></span>{{ backfilling ? '抽取中…' : '从历史抽取画像' }}
        </button>
```

队列区之上加结果摘要条：

```html
    <div class="card sunken" v-if="backfillInfo" style="margin-bottom:12px">
      <div class="kv">
        <span class="muted">已回填 {{ backfillInfo.processed }} 段录音：{{ backfillInfo.active }} 条生效 · {{ backfillInfo.pending }} 条待确认 · {{ backfillInfo.skipped }} 条跳过<span v-if="backfillInfo.errors"> · {{ backfillInfo.errors }} 段失败</span></span>
        <button class="btn-link" @click="backfillInfo=null">✕</button>
      </div>
    </div>
```

- [ ] **Step 3: 语法验证 + 提交**

```bash
node --check web/app.js && echo JS_OK
git add web/app.js web/index.html
git commit -m "feat(web): 从历史回填抽取画像（最近 50 录音同步重放+摘要）"
```

---

### Task 7: 联调冒烟 + README + 收尾

**Files:**
- Modify: `README.md`（tab 说明一句话）

- [ ] **Step 1: 起服务**

```bash
set -a; source .env; set +a
make dev-start   # 或 make dev（前台）
```

- [ ] **Step 2: API 契约冒烟（curl，确认后端在）**

```bash
curl -s localhost:8080/api/persons | head -c 400
curl -s localhost:8080/api/profile/pending | head -c 400
```
Expected: JSON（persons 至少含 owner「我」；items 数组）。

- [ ] **Step 3: 浏览器手动清单（controller/用户执行）**

打开 http://localhost:8080 → 人物 tab：
1. 名册显示 owner「我」卡片（含「我」徽标）
2. ＋ 新建：建「测试人物」→ 出现在名册；空名提交被拦
3. 点开「我」详情：分组属性区 + 底部「＋ 加属性」
4. 加属性 occupation=工程师 → 出现（人工 · 100%）
5. 再加 occupation=教师 → 值变为教师（旧值留痕：点 ⟲ 看「工程师 → 教师」历史）
6. 删属性 → 2 步确认 → 消失；历史里出现「删除了 教师」
7. 加关系：朋友 → 测试人物 → 关系区出现
8. 「从历史抽取画像」→ 摘要条显示统计；若有 pending，队列区出现可确认/放弃
9. 队列确认一条 → 名册角标数变化
10. 「最近互动」🎤 chip 点击 → 跳时间线并展开对应 session

- [ ] **Step 4: README 更新**

快速开始一节的页面描述（`打开 http://localhost:8080 —— 时间线 / 录音 两个标签页`）改为：
`打开 http://localhost:8080 —— 时间线 / 录音 / 声纹 / 人物 / 主题 / 记忆 / 待办 标签页`

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs(web): README 人物 tab 说明"
```

---

## 计划自检

1. **覆盖**：spec §8 的 P1b 范围全落位（名册/详情/属性/关系/历史/确认队列/回填）；「共同 Topic/相关 Todo」明确声明延后。
2. **无占位**：所有代码完整；Task 间引用顺序已注明（Task 1 的 switchTab/closePersonDetail 分两步补全的约束写明）。
3. **类型一致**：前端字段名与 internal/api/person.go 响应逐一对齐（person.id/display_name/is_owner/speaker_id/source/status、groups[].group/attrs[]、pending_count、recent_session_ids、pendingItem 的 kind/id/person_name/attr_key/value/current_value/relation_type/label/confidence/session_id、extractResult 的 processed/results[].active/pending/skipped/error）。

## 执行交接

计划保存至 `docs/superpowers/plans/2026-08-24-person-profile-p1b-frontend.md`。沿用 Subagent-Driven 方式执行（每任务实现 + spec/质量双审；前端任务审查以代码走读 + `node --check` + 契约比对为主，Task 7 由用户做浏览器终验）。
