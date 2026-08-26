# 用户画像 P3b（状态&健康前端）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 人物 tab 消费 P3a 的 metric/cycle 平面：详情卡「状态&健康」区——指标图表（数值型折线 / 类别型时间线）、手动录入/删除测点；「健康周期」区（**敏感默认折叠**，免责直显）——周期管理；确认队列 metric/cycle 条目。纯前端。

**Architecture:** 沿 P1b/P2b 模式。图表设计（dataviz skill 结论）：
- **数值型指标（weight）→ 单序列折线图**：内联 SVG（无构建约束下最轻）、2px 线圆角连接、端点值标签（唯一直标）、≥8px 数据点带 2px surface 环、hairline 实线网格（recessive）、**无图例**（单序列，标题即名）、文字用 text tokens 绝不用数据色、hover 最近点 tooltip（日期+值）。
- **类别型指标（emotion/state/diet/health/sleep_late）→ 时间线列表**（date + 值 chip），不是折线——类别身份画成连续线是撒谎。列表即诚实。
- **配色**：单序列用应用既有 `--accent`（#4338ca，全站主色）——无多序列分类调色板需求（一次只看一个 metric_key），不需跑 palette validator。
- 敏感周期区：默认折叠 + API note 免责文案直显（spec §9）。

**契约（已核对 person.go/repo）：**
- `GET /api/persons/{id}/metrics?metric_key=&from=&to=`（升序，`{metrics:[...]}`；行含 `id/metric_key/value_num?/value_text?/unit?/measured_at/source/status`）
- `POST /api/persons/{id}/metrics` `{metric_key, value, unit?, measured_at?}`；`DELETE .../metrics/{mid}`
- `GET /api/persons/{id}/cycles` → `{cycles:[...], note:"免责"}`（默认 active+pending；行含 `id/cycle_type/label?/anchor_date?/period_days?/duration_days?/dosage?/frequency_text?/next_predicted_at?/source/status`）
- `POST /api/persons/{id}/cycles` `{cycle_type, label?, anchor_date?, period_days?, duration_days?, dosage?, frequency?}`；`DELETE .../cycles/{cid}`
- 队列：kind=metric（value=值串/metric_key/occurred_at）、kind=cycle（value=type·label/cycle_type）

**工作目录：** worktree `.worktrees/person-metric-ui`（分支 `feat/person-metric-ui`）。dev 端口 **8081**。

**验证约定：** `node --check web/app.js` + 契约比对；Task 3 冒烟 + 手动清单。

---

### Task 1: 「状态&健康」区——指标图表 + 手动录入

**Files:** Modify `web/app.js` + `web/index.html`

**app.js**（人物区块、大事记之后追加）：

```js
    // ---------- 状态&健康（metric 平面，P3） ----------
    // 指标类型：数值型（weight）画折线；类别型（emotion 等）列时间线——类别画连续线是撒谎。
    const METRIC_KEYS = [
      { key: 'weight', label: '体重', numeric: true },
      { key: 'emotion', label: '情绪', numeric: false },
      { key: 'state', label: '状态', numeric: false },
      { key: 'diet', label: '饮食', numeric: false },
      { key: 'sleep_late', label: '熬夜', numeric: false },
      { key: 'health', label: '健康', numeric: false },
    ];
    const metricKey = ref('weight');          // 当前选中指标
    const metricRows = ref([]);               // 当前指标的全状态测点（升序）
    const metricLoading = ref(false);
    const showAddMetric = ref(false);
    const addMetricForm = reactive({ value: '', unit: '', measured_at: '' });
    const addingMetric = ref(false);
    const deletingMetricId = ref(null);
    const metricHover = ref(null);            // {x, y, label} 折线 hover 最近点

    function metricDef(k) { return METRIC_KEYS.find(m => m.key === k) || { key: k, label: k, numeric: false }; }
    const metricIsNumeric = computed(() => metricDef(metricKey.value).numeric);

    async function loadMetrics() {
      if (!personDetail.value) return;
      metricLoading.value = true;
      try {
        const d = await api('GET', '/api/persons/' + personDetail.value.person.id +
          '/metrics?metric_key=' + metricKey.value);
        metricRows.value = d.metrics || [];
        metricHover.value = null;
      } catch (e) { showError(e); }
      finally { metricLoading.value = false; }
    }
    function switchMetric(k) { metricKey.value = k; loadMetrics(); }
    function resetAddMetricForm() { addMetricForm.value = ''; addMetricForm.unit = ''; addMetricForm.measured_at = ''; }
    function toggleAddMetric() {
      if (showAddMetric.value) { showAddMetric.value = false; resetAddMetricForm(); return; }
      showAddMetric.value = true;
    }
    async function submitAddMetric() {
      if (addingMetric.value) return;
      const v = addMetricForm.value.trim();
      if (!v) { toast.value = '请输入数值'; setTimeout(() => { toast.value = ''; }, 2000); return; }
      addingMetric.value = true;
      try {
        const body = { metric_key: metricKey.value, value: v };
        if (addMetricForm.unit.trim()) body.unit = addMetricForm.unit.trim();
        if (addMetricForm.measured_at) body.measured_at = addMetricForm.measured_at;
        await api('POST', '/api/persons/' + personDetail.value.person.id + '/metrics', body);
        showAddMetric.value = false;
        resetAddMetricForm();
        await loadMetrics(); await loadPersons(); // 名册角标可能变
      } catch (e) { showError(e); }
      finally { addingMetric.value = false; }
    }
    function askDeleteMetric(m) { deletingMetricId.value = m.id; }
    async function confirmDeleteMetric() {
      const id = deletingMetricId.value;
      if (!id) return;
      try {
        await api('DELETE', '/api/persons/' + personDetail.value.person.id + '/metrics/' + id);
        deletingMetricId.value = null;
        await loadMetrics(); await loadPersons();
      } catch (e) { showError(e); }
    }

    // ---- 折线图几何（纯函数，无 SVG 依赖，可测）----
    // 数值型测点 → 折线 SVG 的坐标/路径。padding 留出轴与端点标签；y 轴 clean ticks。
    const CHART_W = 560, CHART_H = 160, CHART_PAD = { l: 34, r: 46, t: 12, b: 20 };
    const metricNumericRows = computed(() => metricRows.value.filter(m => m.value_num != null && m.status !== 'dismissed'));
    const metricChart = computed(() => {
      const rows = metricNumericRows.value;
      if (rows.length < 2) return null; // 少于 2 点不画线（单点/空 → 文案兜底）
      const vals = rows.map(r => r.value_num);
      let lo = Math.min(...vals), hi = Math.max(...vals);
      if (lo === hi) { lo -= 1; hi += 1; } // 平线也让 y 有量程
      const span = hi - lo;
      const iw = CHART_W - CHART_PAD.l - CHART_PAD.r, ih = CHART_H - CHART_PAD.t - CHART_PAD.b;
      const t0 = new Date(rows[0].measured_at).getTime();
      const t1 = new Date(rows[rows.length - 1].measured_at).getTime();
      const tSpan = (t1 - t0) || 1;
      const x = i => CHART_PAD.l + (new Date(rows[i].measured_at).getTime() - t0) / tSpan * iw;
      const y = v => CHART_PAD.t + (hi - v) / span * ih;
      const pts = rows.map((r, i) => ({ x: x(i), y: y(r.value_num), v: r.value_num, at: r.measured_at, id: r.id }));
      const path = pts.map((p, i) => (i ? 'L' : 'M') + p.x.toFixed(1) + ' ' + p.y.toFixed(1)).join(' ');
      // y 轴 3 档 clean ticks（粗粒度即可——具体值靠端点标签与 hover）
      const ticks = [0, 0.5, 1].map(f => ({ y: y(lo + f * span), label: Math.round((lo + f * span) * 10) / 10 }));
      return { pts, path, ticks, unit: rows[rows.length - 1].unit || '' };
    });
    // hover：mousemove 找最近点（x 距离），tooltip 显示日期+值
    function onMetricChartMove(e) {
      const c = metricChart.value;
      if (!c) return;
      const svg = e.currentTarget.getBoundingClientRect();
      const mx = (e.clientX - svg.left) * (CHART_W / svg.width);
      let best = null, bd = Infinity;
      for (const p of c.pts) {
        const d = Math.abs(p.x - mx);
        if (d < bd) { bd = d; best = p; }
      }
      if (best) metricHover.value = { x: best.x, y: best.y, text: fmtEventDate(best.at, true) + ' · ' + best.v + (c.unit || '') };
    }
```

closePersonDetail 追加：`showAddMetric.value = false; resetAddMetricForm(); deletingMetricId.value = null; metricHover.value = null;`
**loadMetrics 的挂接**：togglePerson 打开详情后调 `loadMetrics()`（togglePerson 里详情拉取成功后）；切人时 closePersonDetail 已清表单。switchTab 不用（详情内切换）。

return 导出：`METRIC_KEYS, metricKey, metricRows, metricLoading, metricIsNumeric, switchMetric, showAddMetric, addMetricForm, addingMetric, toggleAddMetric, submitAddMetric, deletingMetricId, askDeleteMetric, confirmDeleteMetric, metricChart, metricHover, onMetricChartMove, CHART_W, CHART_H, metricNumericRows,`

**index.html**（详情卡内、大事记区之后）：

```html
      <!-- 状态&健康：指标图表（数值型折线 / 类别型时间线） -->
      <div style="margin-bottom:12px">
        <div class="kv" style="margin-bottom:8px">
          <div class="muted" style="font-weight:600">状态&健康</div>
          <div style="display:flex; gap:4px; flex-wrap:wrap">
            <button v-for="mk in METRIC_KEYS" :key="mk.key" class="btn mini"
                    :style="metricKey === mk.key ? { background:'var(--accent)', color:'#fff', borderColor:'var(--accent)' } : {}"
                    @click="switchMetric(mk.key)">{{ mk.label }}</button>
          </div>
        </div>
        <div v-if="metricLoading" class="muted">加载中…</div>
        <template v-else>
          <!-- 数值型：折线图（单序列无图例；端点直标；hover 最近点） -->
          <div v-if="metricIsNumeric && metricChart" style="position:relative">
            <svg :viewBox="'0 0 ' + CHART_W + ' ' + CHART_H" style="width:100%; height:auto; display:block"
                 @mousemove="onMetricChartMove" @mouseleave="metricHover = null">
              <!-- hairline 网格（recessive） -->
              <line v-for="t in metricChart.ticks" :key="t.y" :x1="CHART_PAD_L" x2="560" :y1="t.y" :y2="t.y"
                    stroke="var(--border)" stroke-width="1"/>
              <text v-for="t in metricChart.ticks" :key="'l'+t.y" :x="CHART_PAD_L - 6" :y="t.y + 4"
                    text-anchor="end" font-size="11" fill="var(--muted)">{{ t.label }}</text>
              <!-- 2px 线圆角连接 -->
              <path :d="metricChart.path" fill="none" stroke="var(--accent)" stroke-width="2"
                    stroke-linejoin="round" stroke-linecap="round"/>
              <!-- 数据点：r=4 实心 + 2px surface 环 -->
              <circle v-for="p in metricChart.pts" :key="p.id" :cx="p.x" :cy="p.y" r="4"
                      fill="var(--accent)" stroke="var(--surface)" stroke-width="2"/>
              <!-- 端点值直标（唯一标签，text token 色） -->
              <text :x="metricChart.pts[metricChart.pts.length-1].x + 8"
                    :y="metricChart.pts[metricChart.pts.length-1].y + 4"
                    font-size="12" fill="var(--text-2)" font-weight="600">
                {{ metricChart.pts[metricChart.pts.length-1].v }}{{ metricChart.unit }}
              </text>
              <!-- hover 十字线 + 提示 -->
              <template v-if="metricHover">
                <line :x1="metricHover.x" :x2="metricHover.x" y1="12" y2="140" stroke="var(--border-strong)" stroke-width="1"/>
                <circle :cx="metricHover.x" :cy="metricHover.y" r="5" fill="var(--accent)" stroke="var(--surface)" stroke-width="2"/>
              </template>
            </svg>
            <div v-if="metricHover" class="chip" style="position:absolute; pointer-events:none; white-space:nowrap"
                 :style="{ left: (metricHover.x / 560 * 100) + '%', top: (metricHover.y - 34) + 'px' }">
              {{ metricHover.text }}
            </div>
            <div class="muted" style="font-size:var(--fs-xs)">{{ metricNumericRows.length }} 个测点（含待确认）</div>
          </div>
          <div v-else-if="metricIsNumeric" class="muted" style="font-size:var(--fs-xs)">数值型指标至少 2 个测点才画曲线。</div>
          <!-- 类别型：时间线列表 -->
          <div v-else>
            <div v-for="m in metricRows" :key="m.id" class="seg">
              <span class="chip" style="background:var(--surface-sunken); color:var(--text-2)">{{ m.value_text }}</span>
              <div class="seg-text">
                <div>
                  {{ m.value_text }}
                  <span class="chip" :style="m.source === 'llm' ? { background:'var(--accent-soft)', color:'var(--accent)' } : { background:'var(--ok-soft)', color:'var(--ok)' }">{{ m.source === 'llm' ? 'AI' : '人工' }}</span>
                  <span v-if="m.status === 'pending'" class="chip" style="background:var(--warn-soft); color:var(--warn)">待确认</span>
                </div>
                <div class="muted" style="font-size:var(--fs-xs)">{{ fmtEventDate(m.measured_at, true) }}</div>
              </div>
              <div style="display:flex; gap:4px; flex-shrink:0">
                <template v-if="deletingMetricId === m.id">
                  <span class="muted">删除？</span>
                  <button class="btn mini danger" @click="confirmDeleteMetric">确认</button>
                  <button class="btn mini" @click="deletingMetricId=null">取消</button>
                </template>
                <button v-else class="btn mini danger" @click="askDeleteMetric(m)">🗑</button>
              </div>
            </div>
            <div v-if="!metricRows.length" class="muted" style="font-size:var(--fs-xs)">暂无记录。从对话自动抽取或手动添加。</div>
          </div>
        </template>
        <!-- 手动录入（表单折叠） -->
        <div class="edit-actions" style="margin-top:6px">
          <button class="btn mini" @click="toggleAddMetric">{{ showAddMetric ? '收起录入' : '＋ 记一笔' }}</button>
        </div>
        <div class="card sunken" v-if="showAddMetric" style="margin-top:8px">
          <div style="display:flex; gap:8px; flex-wrap:wrap; align-items:center">
            <input class="txt" v-model="addMetricForm.value" :placeholder="metricIsNumeric ? '数值（如 72.5）' : '状态（如 焦虑）'" style="flex:1; min-width:120px">
            <input v-if="metricIsNumeric" class="txt" v-model="addMetricForm.unit" placeholder="单位（kg，可空）" style="width:auto; min-width:90px">
            <input class="txt" v-model="addMetricForm.measured_at" type="date" style="width:auto; min-width:130px">
            <button class="btn primary" :disabled="addingMetric" @click="submitAddMetric">{{ addingMetric ? '保存中…' : '保存' }}</button>
          </div>
          <div class="muted" style="font-size:var(--fs-xs); margin-top:4px">录入当前指标「{{ metricDef(metricKey).label }}」；日期可空（默认今天）。</div>
        </div>
      </div>
```

注意：模板里 `CHART_PAD_L` 须导出常量 `const CHART_PAD_L = 34`（或直接写 34——**用直接数字 34 简化**，删掉 CHART_PAD_L 引用）。类别型列表行首 chip 与行内重复显示 value——**去掉行首 chip**（seg-text 里已有）。数值型测点的删除入口：hover 太隐晦，**在「N 个测点」行尾加「管理」折叠切换**显示全部测点列表（复用类别型的 .seg 列表）——简化：数值型也显示测点列表（图表下方小字列表，含删除）？会太长。**决策**：数值型提供「管理测点」toggle 按钮展开列表（含删除），默认收起。

Commit: `feat(web): 状态&健康区——指标折线/时间线+手动录入`

### Task 2: 周期区（敏感折叠）+ 队列条目

**app.js：**
- 健康周期状态：`healthOpen = ref(false)`（默认折叠）、`cycles = ref([])`、`cyclesNote = ref('')`、`showAddCycle`/`addCycleForm`(reactive: cycle_type/label/anchor_date/period_days/duration_days/dosage/frequency)/`addingCycle`/`deletingCycleId`
- `toggleHealth()`: 开时拉 `GET cycles`（note 存 cyclesNote）；关时清
- `CYCLE_TYPES = [{key:'menstrual',label:'生理期'},{key:'medication',label:'用药'},{key:'injection',label:'注射'},{key:'followup',label:'随访'}]`；`fmtDateOnly(iso)`（YYYY-MM-DD）
- submitAddCycle/toggleAddCycle（对称草稿）/askDeleteCycle/confirmDeleteCycle（照事件模式）
- closePersonDetail 追加清周期态
- 队列：`pendingKindText` 加 `metric: '指标'`、`cycle: '周期'`；`pendingSummary` 加两分支（metric：`metric_key：value`——用 metricKeyLabel 映射中文；cycle：`event_type 似的 type·label`——`(it.cycle_type || '') + (it.value 不含 type 前缀？后端 value=type·label，直接用 it.value)`——**直接 return it.value || ''`**）
- togglePerson 打开详情后 loadMetrics 已挂；周期是折叠懒加载不加

**index.html：**
- 周期区（敏感）：折叠头「健康周期（敏感）」+ 折叠后内容——**免责 note 直显**（cyclesNote，muted 小字）、周期列表（.seg：类型 chip 中文 + label + anchor→next 预测 + dosage/frequency + 来源/pending 徽标 + 删除 2 步）、加周期表单（类型下拉 4 枚举/label/anchor date/period/duration 数字输入/dosage/frequency）
- 队列条目：metric/cycle 摘要已由 pendingSummary 覆盖；occurred_at 显示已有（metric 条目后端已带）

Commit: `feat(web): 健康周期区（敏感折叠+免责）+队列 metric/cycle 条目`

### Task 3: hash + 冒烟 + 手动清单

1. `bash scripts/hash-web.sh` + `git add web/index.html`（src 行提交）
2. 冒烟（curl 8081）：POST weight 测点×3 → GET metrics?metric_key=weight 断言 3 条升序 → 图表数据就绪；POST/GET/DELETE cycle（note 字段断言）→ 清理
3. 手动清单追加（P1b 清单文件末尾）：
```markdown
## P3b 状态&健康验收（追加）

19. 详情「状态&健康」：切「体重」录入 3 笔不同日期 → 折线出现（端点值直标；hover 显示日期+值）
20. 切「情绪」录入/抽取 → 时间线列表（值+日期+来源徽标）
21. 「记一笔」空值被拦；日期默认今天；删除测点 2 步确认
22. 「健康周期（敏感）」默认折叠 → 展开见免责文案 + 周期列表
23. 加周期（用药·降压药 anchor+period）→ 下次预测日期显示
24. 队列出现「指标」「周期」条目（含值/日期）→ 确认流转
25. 切换人物再切回：指标表单/周期表单草稿不残留
```
4. Commit: `docs(web): P3b 手动验收清单 + hash 同步`

---

## 计划自检

1. **覆盖**：P3b 范围（指标图表/录入/删除、周期敏感折叠+免责、队列条目）全落位；契约逐字段核对。
2. **图表合规**（dataviz skill）：单序列折线无图例/2px 圆角线/端点唯一直标/8px 点+2px surface 环/hairline 网格/text token 文字色/hover 最近点+tooltip；类别型用列表不用线。
3. **模式一致**：全部对齐 P1b/P2b（对称草稿/closePersonDetail 清理/2 步删除/懒加载）。
