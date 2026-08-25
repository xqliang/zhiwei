// 知微 Web 前端（Vue 3 CDN，无构建）。
// 标签页：时间线 / 录音 / Topics / 待办（问知微、今日留待后续 Sprint）。
const { createApp, ref, reactive, computed, onUnmounted } = Vue;

// memory 类型 → 中文标签与颜色（卡片徽标用）
const TYPE_META = {
  event:      { label: '事件', color: '#6366f1' },
  fact:       { label: '事实', color: '#0891b2' },
  decision:   { label: '决定', color: '#7c3aed' },
  idea:       { label: '想法', color: '#d97706' },
  problem:    { label: '问题', color: '#dc2626' },
  preference: { label: '偏好', color: '#059669' },
};

const app = createApp({
  setup() {
    const tab = ref('timeline');
    const toast = ref('');

    // ---------- 通用 ----------
    function fmtTime(iso) { return iso ? new Date(iso).toLocaleString('zh-CN') : ''; }
    function fmtDue(iso) {
      if (!iso) return '';
      const d = new Date(iso);
      const s = d.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' });
      return d < new Date() ? s + ' · 已过期' : s;
    }
    async function api(method, url, body) {
      const opt = { method };
      if (body instanceof FormData) {
        // FormData（录入说话人上传语音样本）：不手动设 Content-Type，
        // 交给浏览器自动带上含 boundary 的 multipart/form-data 头，否则后端解析不到文件。
        opt.body = body;
      } else if (body !== undefined) {
        opt.headers = { 'Content-Type': 'application/json' };
        opt.body = JSON.stringify(body);
      }
      const r = await fetch(url, opt);
      const text = await r.text();
      if (!r.ok) {
        let msg = '请求失败';
        try { msg = (text ? JSON.parse(text).error : '') || msg; } catch (e) {}
        throw new Error(msg);
      }
      // 部分写操作（关联/删除 topic）返回 200/204 空体，r.json() 会抛
      // "Unexpected end of JSON input"——空体直接返回 null，调用方按需判断。
      return text ? JSON.parse(text) : null;
    }
    function showError(e) {
      toast.value = (e && e.message) || String(e);
      setTimeout(() => { toast.value = ''; }, 3000);
    }
    function typeMeta(t) { return TYPE_META[t] || { label: t, color: '#6b7280' }; }
    function statusText(status, stage) {
      if (status === 'done' || status === 'completed') return '已完成';
      if (status === 'failed') return '失败';
      if (status === 'running') return '处理中 · ' + (stage || '');
      return '排队中';
    }
    // 待办状态 → 中文标签（模板多处复用，集中一处避免散落的三元）
    function todoStatusText(status) {
      if (status === 'suggested') return '待确认';
      if (status === 'confirmed') return '已确认';
      if (status === 'done') return '已完成';
      return '已忽略';
    }
    function spClass(speaker) {
      const n = (speaker || '').replace(/\D/g, '') || '1';
      return 'sp' + Math.min(Number(n), 3);
    }

    // ---------- 时间线 ----------
    const sessions = ref([]);
    const detail = ref(null);
    const expandedId = ref(null); // 当前就地展开的 session id

    async function loadSessions() {
      try {
        // limit=200：时间线客户端筛选/搜索需要更大窗口（单用户 MVP 足够）
        const d = await api('GET', '/api/sessions?limit=200');
        sessions.value = d.sessions || [];
      } catch (e) { showError(e); }
    }
    // 点击会话卡片：已展开则收起；否则拉取详情就地展开（内联在当前卡片下方）
    async function toggleSession(id) {
      if (expandedId.value === id) {
        expandedId.value = null;
        detail.value = null;
        editingMem.value = null;       // T1 已有
        editingTodo.value = null;      // 新增
        deletingTodoId.value = null;  // 新增
        deletingSessionId.value = null;  // T6: 折叠时清理删除态
        return;
      }
      try {
        detail.value = await api('GET', '/api/sessions/' + id);
        expandedId.value = id;
      } catch (e) { showError(e); }
    }
    // 重拉已展开的会话详情但不收起：memory/todo 关联操作后用，避免 toggle 的收起-再展开抖动
    async function reloadSession(id) {
      try { detail.value = await api('GET', '/api/sessions/' + id); }
      catch (e) { showError(e); }
    }
    // ---------- session 删除（硬删级联，2 步确认） ----------
    const deletingSessionId = ref(null);
    function askDeleteSession(s) { deletingSessionId.value = s.id; }
    function cancelDeleteSession() { deletingSessionId.value = null; }
    async function confirmDeleteSession(s) {
      try {
        await api('DELETE', '/api/sessions/' + s.id);
        deletingSessionId.value = null;
        // 删的是当前展开的 session→清展开态 + 其上残留的编辑/删除态（与 toggleSession 折叠清理对齐）
        if (expandedId.value === s.id) {
          expandedId.value = null; detail.value = null;
          editingMem.value = null; editingTodo.value = null; deletingTodoId.value = null;
        }
        await loadSessions();
      } catch (e) { showError(e); }
    }
    // 原始音频流式地址（ ServeAudio 端点）
    function audioUrl(id) { return '/api/sessions/' + id + '/audio'; }
    // ---------- 记忆忽略（2 步确认，时间线/主题内页/记忆 tab 共用） ----------
    // dismissingMemId = 待确认忽略的记忆 id。确认后 PATCH status=dismissed，再按当前视图 reload。
    const dismissingMemId = ref(null);
    function askDismissMem(m) { editingMem.value = null; dismissingMemId.value = m.id; }
    function cancelDismissMem() { dismissingMemId.value = null; }
    async function confirmDismissMem(m, reload) {
      try {
        await api('PATCH', '/api/memories/' + m.id, { status: 'dismissed' });
        dismissingMemId.value = null;
        if (reload) await reload();
      } catch (e) { showError(e); }
    }
    // ---------- 记忆 inplace 编辑（复用 PATCH /api/memories/{id}） ----------
    // editingMem = {id, title, content}；点 title/content 进入编辑、保存 PATCH、取消还原。
    const editingMem = ref(null);
    function startEditMemory(m) { editingMem.value = { id: m.id, title: m.title, content: m.content }; }
    function cancelEditMemory() { editingMem.value = null; }
    async function saveEditMemory(reload) {
      const e = editingMem.value;
      if (!e || !e.title.trim()) return; // 空 title 不发
      try {
        await api('PATCH', '/api/memories/' + e.id, { title: e.title.trim(), content: e.content });
        editingMem.value = null;
        if (reload) await reload();
      } catch (e2) { showError(e2); }
    }
    async function retryJob(id) {
      try {
        await api('POST', '/api/jobs/' + id + '/retry');
        await toggleSession(detail.value.session.id);
      } catch (e) { showError(e); }
    }

    // ---------- 录音 ----------
    const recording = ref(false);
    const recSeconds = ref(0);
    const uploadInfo = ref(null);
    const lastAudioFile = ref(null);   // 最近一次录音/拖拽的音频文件（试匹配声纹用）
    const matchInfo = ref(null);       // 试匹配结果：{speaker_name, similarity, threshold, matched, has_library}
    const voiceprintMatching = ref(false);
    let recorder = null, recTimer = null, pollTimer = null;

    async function startRec() {
      try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        recorder = new MediaRecorder(stream, { mimeType: 'audio/webm;codecs=opus' });
        const chunks = [];
        recorder.ondataavailable = e => chunks.push(e.data);
        recorder.onstop = () => {
          stream.getTracks().forEach(t => t.stop());
          const f = new File(chunks, 'record-' + Date.now() + '.webm', { type: 'audio/webm' });
          lastAudioFile.value = f; matchInfo.value = null; // 供「试匹配声纹」按钮用
          upload(f, 'web_record');
        };
        recorder.start();
        recording.value = true; recSeconds.value = 0;
        recTimer = setInterval(() => recSeconds.value++, 1000);
      } catch (e) { showError(e); }
    }
    function stopRec() {
      recorder.stop(); recording.value = false;
      clearInterval(recTimer);
    }
    function onDrop(e) {
      const f = e.dataTransfer.files[0];
      if (f) { lastAudioFile.value = f; matchInfo.value = null; upload(f, 'web_upload'); }
    }
    async function upload(file, source) {
      const fd = new FormData();
      fd.append('file', file); fd.append('source', source);
      uploadInfo.value = { filename: file.name, status: 'pending', text: '上传中…' };
      try {
        const r = await fetch('/api/audio', { method: 'POST', body: fd });
        const d = await r.json();
        if (!r.ok) throw new Error(d.error || '上传失败');
        uploadInfo.value = { filename: file.name, status: 'running', text: '已上传，处理中…' };
        pollTimer = setInterval(async () => {
          try {
            const rr = await fetch('/api/sessions/' + d.session_id);
            const dd = await rr.json();
            const st = dd.job ? dd.job.status : dd.session.status;
            if (st === 'done' || st === 'completed') {
              clearInterval(pollTimer);
              uploadInfo.value = { filename: file.name, status: 'done', text: '处理完成 ✓' };
              loadSessions();
            } else if (st === 'failed') {
              clearInterval(pollTimer);
              uploadInfo.value = { filename: file.name, status: 'failed', text: '处理失败，可在时间线重跑' };
            }
          } catch (e) { /* 轮询失败静默重试 */ }
        }, 2000);
      } catch (e) {
        uploadInfo.value = { filename: file.name, status: 'failed', text: e.message };
      }
    }

    // 试匹配声纹库（录音页「这段像谁」）：把当前录音/拖拽的音频 POST /api/voiceprint/match
    // → 后端转码+提向+1:N → 返回最匹配说话人 + 相似度 + 阈值。纯只读预览，不登记。
    async function tryMatchVoiceprint() {
      const f = lastAudioFile.value;
      if (!f) return;
      voiceprintMatching.value = true;
      const fd = new FormData(); fd.append('file', f);
      try {
        const d = await api('POST', '/api/voiceprint/match', fd);
        matchInfo.value = d;
      } catch (e) { showError(e); }
      finally { voiceprintMatching.value = false; }
    }

    // ---------- Topics ----------
    const topics = ref([]);
    const topicDetail = ref(null);
    const showNewTopic = ref(false);
    const newTopic = ref({ name: '', description: '' });
    const creating = ref(false); // 创建中：按钮 loading + 禁用防重复提交
    // 新建主题表单：打开/关闭与清空。关闭时还原输入与 creating 态，避免残留。
    function cancelNewTopic() { showNewTopic.value = false; newTopic.value = { name: '', description: '' }; creating.value = false; }
    function toggleNewTopic() { if (showNewTopic.value) cancelNewTopic(); else showNewTopic.value = true; }
    const renaming = ref(null); // { id, name }

    async function loadTopics() {
      try {
        const d = await api('GET', '/api/topics');
        topics.value = d.topics || [];
      } catch (e) { showError(e); }
    }
    // 已忽略主题（status=dismissed）折叠区：单独 GET ?dismissed=1，与活跃列表分离。
    const dismissedTopics = ref([]);
    const dismissedCollapsed = ref(true); // 默认收起
    async function loadDismissedTopics() {
      try {
        const d = await api('GET', '/api/topics?dismissed=1');
        dismissedTopics.value = d.topics || [];
      } catch (e) { showError(e); }
    }
    async function openTopic(id) {
      try {
        topicDetail.value = await api('GET', '/api/topics/' + id);
      } catch (e) { showError(e); }
    }
    async function confirmTopic(t) {
      deletingTopicId.value = null; dismissingTopicId.value = null; // topic 状态变 active，清理删除/忽略确认态
      try {
        await api('PATCH', '/api/topics/' + t.id, { status: 'active' });
        await loadTopics();
      } catch (e) { showError(e); }
    }
    // ---------- topic 忽略/删除（均 2 步确认，互斥）+ 恢复 ----------
    const dismissingTopicId = ref(null);
    const deletingTopicId = ref(null);
    function askDismissTopic(t) { deletingTopicId.value = null; dismissingTopicId.value = t.id; }
    function cancelDismissTopic() { dismissingTopicId.value = null; }
    async function confirmDismissTopic(t) { // 忽略=软隐藏（status=dismissed，行保留可恢复）
      try {
        await api('PATCH', '/api/topics/' + t.id, { status: 'dismissed' });
        dismissingTopicId.value = null;
        await loadTopics();
        await loadDismissedTopics();
      } catch (e) { showError(e); }
    }
    function askDeleteTopic(t) { dismissingTopicId.value = null; deletingTopicId.value = t.id; }
    function cancelDeleteTopic() { deletingTopicId.value = null; }
    async function confirmDeleteTopic(t) {
      try {
        await api('DELETE', '/api/topics/' + t.id);
        deletingTopicId.value = null;
        await loadTopics();
        await loadDismissedTopics();
        if (topicDetail.value && topicDetail.value.topic.id === t.id) closeTopicDetail();
      } catch (e) { showError(e); }
    }
    // 恢复已忽略主题（PATCH status=active）
    async function restoreTopic(t) {
      try {
        await api('PATCH', '/api/topics/' + t.id, { status: 'active' });
        await loadTopics();
        await loadDismissedTopics();
      } catch (e) { showError(e); }
    }
    // 关闭主题详情：同时清空重命名状态，避免残留的输入框污染下次打开
    function closeTopicDetail() {
      topicDetail.value = null;
      renaming.value = null;
      editingMem.value = null;
      deletingTopicId.value = null; dismissingTopicId.value = null; // 清理删除/忽略确认态
    }
    function startRename(t) { renaming.value = { id: t.id, name: t.name }; }
    async function commitRename() {
      const rn = renaming.value;
      renaming.value = null;
      if (!rn || !rn.name.trim()) return;
      try {
        await api('PATCH', '/api/topics/' + rn.id, { name: rn.name.trim() });
        await loadTopics();
        if (topicDetail.value && topicDetail.value.topic.id === rn.id) {
          await openTopic(rn.id);
        }
      } catch (e) {
        renaming.value = rn; // 保存失败时恢复编辑状态，避免用户输入丢失
        showError(e);
      }
    }
    async function createTopic() {
      if (creating.value) return; // 防重复提交
      if (!newTopic.value.name.trim()) {
        toast.value = '请输入主题名称'; setTimeout(() => { toast.value = ''; }, 2000);
        return;
      }
      creating.value = true; // 即时反馈：按钮变「创建中…」+ spinner
      try {
        // 提交前对名称与描述做 trim，避免首尾空白进入库
        await api('POST', '/api/topics', {
          name: newTopic.value.name.trim(),
          description: newTopic.value.description.trim(),
        });
        newTopic.value = { name: '', description: '' };
        showNewTopic.value = false;
        await loadTopics();
        toast.value = '主题已创建'; setTimeout(() => { toast.value = ''; }, 2000);
      } catch (e) { showError(e); }
      finally { creating.value = false; }
    }

    // ---------- Topics 相似度启发（疑似可合并提示，纯前端） ----------
    // 归一化标题：小写 + 仅保留字母/数字（与后端 NormalizeTitle 同思路，前端独立实现）。
    function normTitle(s) { return (s || '').toLowerCase().replace(/[^\p{L}\p{N}]/gu, ''); }
    // 疑似可合并：与列表中任一其他 topic 归一化后「互为包含」或 Levenshtein 相似比 > 0.85，
    // 返回疑似对象的名称（供卡片挂「疑似可合并: X」徽标）；无则 null。只标记不自动合并——
    // 字面近重复能抓（如「SDPC俱乐部活动」与「…活动准备」），语义相近的交给手动智能合并。
    function suspectOf(t, all) {
      const a = normTitle(t.name);
      if (!a) return null;
      for (const o of all) {
        if (o.id === t.id) continue;
        const b = normTitle(o.name);
        if (!b) continue;
        if (a.includes(b) || b.includes(a)) return o.name;
        if (a.length > 3 && b.length > 3 && similarRatio(a, b) > 0.85) return o.name;
      }
      return null;
    }
    // similarRatio：Levenshtein 编辑距离转相似比（0~1），1 表示完全相同。
    function similarRatio(a, b) {
      const m = a.length, n = b.length;
      const dp = Array.from({ length: m + 1 }, (_, i) => [i, ...Array(n).fill(0)]);
      for (let j = 0; j <= n; j++) dp[0][j] = j;
      for (let i = 1; i <= m; i++) for (let j = 1; j <= n; j++)
        dp[i][j] = a[i - 1] === b[j - 1] ? dp[i - 1][j - 1] : 1 + Math.min(dp[i - 1][j], dp[i][j - 1], dp[i - 1][j - 1]);
      return 1 - dp[m][n] / Math.max(m, n);
    }

    // ---------- 记忆整理（D2 LLM 提议 → 用户编辑确认 → 应用） ----------
    // 仿 T8 topic 智能合并：member 用 {id,name,checked} 对齐。canonical_id 是 memory id，
    // 用 <select> 选 member（非文本输入）。点按钮先 loadMemories（GET /api/memories?limit=200，
    // 取非 dismissed 记忆标题，上限 200；>200 条时超出部分 titleOf 回退为原始 id 字符串）。
    const memories = ref([]);
    async function loadMemories() {
      try {
        const d = await api('GET', '/api/memories?limit=200');
        memories.value = d.memories || [];
      } catch (e) { showError(e); }
    }
    const memoryDraft = ref(null); // {merges:[{canonical_id, members:[{id,name,checked}]}], adjustments:[{memory_id,title,kind,reason,evidence_ids,checked}]}
    async function startMemoryConsolidate() {
      if (memConsolidating.value) return; // 防重复点击
      memConsolidating.value = true;      // 立即反馈：按钮 loading
      try {
        await loadMemories();
        const d = await api('POST', '/api/memories/consolidate', {});
        const titleOf = id => { const m = memories.value.find(x => x.id === id); return m ? m.title : id; };
        const merges = (d.merges || []).map(g => ({
          canonical_id: g.canonical_id || '',
          members: (g.member_ids || []).map(id => ({ id, name: titleOf(id), checked: true })),
        }));
        const adjustments = (d.adjustments || []).map(a => ({
          memory_id: a.memory_id, title: titleOf(a.memory_id),
          kind: a.kind, reason: a.reason,
          evidence_ids: a.evidence_ids || [],
          checked: true,
        }));
        if (!merges.length && !adjustments.length) {
          // 无可整理项：只 toast 提示（3s 自动消失），不弹「确认整理」面板
          toast.value = '暂无需要整理的记忆';
          setTimeout(() => { toast.value = ''; }, 3000);
          return;
        }
        memoryDraft.value = { merges, adjustments };
      } catch (e) { showError(e); }
      finally { memConsolidating.value = false; }
    }
    function toggleMemoryMember(g, id) {
      const m = g.members.find(x => x.id === id);
      if (m) m.checked = !m.checked;
    }
    function toggleMemoryAdjustment(a) { a.checked = !a.checked; }
    // 只提交「canonical 非空 + 勾选 ≥2 member」的合并组 + 勾选的调整项
    async function applyMemoryConsolidation() {
      const d = memoryDraft.value || {};
      const merges = (d.merges || [])
        .map(g => ({ canonical_id: g.canonical_id, member_ids: g.members.filter(m => m.checked).map(m => m.id) }))
        .filter(g => g.canonical_id && g.member_ids.length >= 2);
      const adjustments = (d.adjustments || []).filter(a => a.checked)
        .map(a => ({ memory_id: a.memory_id, kind: a.kind, reason: a.reason, evidence_ids: a.evidence_ids }));
      if (!merges.length && !adjustments.length) { memoryDraft.value = null; return; }
      try {
        await api('POST', '/api/memories/merge', { merges, adjustments });
        memoryDraft.value = null;
        await reloadSession(detail.value.session.id);
        await loadMemories();
      } catch (e) { showError(e); }
    }

    // ---------- Topics 智能合并（LLM 提议 → 用户编辑确认 → 应用） ----------
    // mergeDraft：合并提议草稿，每组 {canonical_name, members:[{id,name,checked}]}。
    // 用 {id,name,checked} 成员对象而非裸 id 数组，便于模板正确渲染 checkbox（修掉
    // 计划初版里 g.member_ids[gi] 的索引错位）。
    const mergeDraft = ref(null);
    // startConsolidate：调后端 consolidate（LLM 按该用户主题列表给合并组提议），
    // 落进草稿供用户编辑 canonical 名与勾选成员；不改库。
    async function startConsolidate() {
      if (consolidating.value) return; // 防重复点击
      consolidating.value = true;      // 立即反馈：按钮 loading + 骨架卡片
      try {
        const d = await api('POST', '/api/topics/consolidate', {});
        const groups = (d.groups || []).map(g => ({
          canonical_name: g.canonical_name || '',
          members: (g.member_ids || []).map(id => {
            const m = topics.value.find(t => t.id === id);
            return { id, name: m ? m.name : id, checked: true };
          }),
        }));
        if (!groups.length) {
          // 无可合并组：只 toast 提示（3s 自动消失），不弹「确认合并」面板
          toast.value = '暂无需要合并的主题';
          setTimeout(() => { toast.value = ''; }, 3000);
          return;
        }
        mergeDraft.value = groups;
      } catch (e) { showError(e); }
      finally { consolidating.value = false; }
    }
    // toggleMergeMember：勾选/取消某组成员（直接翻转 m.checked，Vue 深响应式自动重渲染）。
    function toggleMergeMember(g, id) {
      const m = g.members.find(x => x.id === id);
      if (m) m.checked = !m.checked;
    }
    // applyMerge：只提交「规范名非空 + 仍勾选 ≥2 成员」的组；后端单事务把各 member 的
    // memory_topic/todo_topic 关联 INSERT IGNORE 迁到 canonical、member 置 dismissed。
    async function applyMerge() {
      const groups = (mergeDraft.value || [])
        .map(g => ({ canonical_name: g.canonical_name.trim(), member_ids: g.members.filter(m => m.checked).map(m => m.id) }))
        .filter(g => g.canonical_name && g.member_ids.length >= 2);
      if (!groups.length) { mergeDraft.value = null; return; }
      try {
        await api('POST', '/api/topics/merge', { groups });
        mergeDraft.value = null;
        await loadTopics();
      } catch (e) { showError(e); }
    }

    // ---------- 手动合并 topic（选多个→输新名→复用 /api/topics/merge） ----------
    // manualMergeMode=选择模式；manualSelected=勾选 id；manualConfirming=已点开始合并→输名。
    const manualMergeMode = ref(false);
    const manualSelected = ref([]);
    const manualMergeName = ref('');
    const manualConfirming = ref(false);
    function startManualMerge() {
      manualMergeMode.value = true; manualSelected.value = []; manualConfirming.value = false; manualMergeName.value = '';
    }
    function cancelManualMerge() {
      manualMergeMode.value = false; manualSelected.value = []; manualConfirming.value = false; manualMergeName.value = '';
    }
    function toggleManualSelect(t) {
      const i = manualSelected.value.indexOf(t.id);
      if (i >= 0) manualSelected.value.splice(i, 1); else manualSelected.value.push(t.id);
    }
    async function applyManualMerge() {
      const ids = manualSelected.value.slice();
      if (ids.length < 2) { toast.value = '至少选 2 个主题'; return; }
      const name = manualMergeName.value.trim();
      if (!name) { toast.value = '请输入规范名'; return; }
      try {
        await api('POST', '/api/topics/merge', { groups: [{ canonical_name: name, member_ids: ids }] });
        cancelManualMerge();
        await loadTopics();
      } catch (e) { showError(e); }
    }
    // startManualConfirm：从「开始合并」进入输名阶段，默认填首个选中 topic 的名。
    function startManualConfirm() {
      manualConfirming.value = true;
      const first = topics.value.find(t => t.id === manualSelected.value[0]);
      manualMergeName.value = (first && first.name) || '';
    }

    // ---------- 待办 ----------
    const todos = ref([]);
    const doneCollapsed = ref(true);
    const suggestedTodos = computed(() => todos.value.filter(t => t.status === 'suggested'));
    const activeTodos = computed(() => todos.value.filter(t => t.status === 'confirmed'));
    const doneTodos = computed(() => todos.value.filter(t => t.status === 'done'));

    async function loadTodos() {
      try {
        const d = await api('GET', '/api/todos');
        todos.value = d.todos || [];
      } catch (e) { showError(e); }
    }
    // 已忽略待办（dismissed 终态，仅供查看+硬删）。?dismissed=1 走后端 ListDismissed。
    const dismissedTodos = ref([]);
    const dismissedTodoCollapsed = ref(true); // 默认收起（区别于主题页的 dismissedCollapsed）
    async function loadDismissedTodos() {
      try {
        const d = await api('GET', '/api/todos?dismissed=1');
        dismissedTodos.value = d.todos || [];
      } catch (e) { showError(e); }
    }
    async function reloadAllTodos() { await loadTodos(); await loadDismissedTodos(); }
    // ---------- 待办/记忆 ↔ topic 多对多手动关联 ----------
    // 取条目身上的 topic 徽标数组（统一从 topics[] 读取，兼容空值）
    function topicChips(item) { return (item && item.topics) || []; }
    // 下拉选项：排除已忽略（dismissed）的 topic
    const availableTopics = computed(() => topics.value.filter(t => t.status !== 'dismissed'));
    async function addTodoTopic(t, topicId) {
      try { await api('POST', '/api/todos/' + t.id + '/topics', { topic_id: topicId }); await loadTodos(); }
      catch (e) { showError(e); }
    }
    async function removeTodoTopic(t, topicId) {
      try { await api('DELETE', '/api/todos/' + t.id + '/topics/' + topicId); await loadTodos(); }
      catch (e) { showError(e); }
    }
    // 关联变更后按当前视图刷新：记忆 tab 刷 memories，时间线详情刷 session。
    // 原先硬编码 reloadSession(detail.session.id)——在记忆 tab（无展开 session）会崩。
    async function reloadAfterMemoryTopic() {
      if (tab.value === 'memories') await loadMemories();
      else if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
    }
    async function addMemoryTopic(m, topicId) {
      try { await api('POST', '/api/memories/' + m.id + '/topics', { topic_id: topicId }); await reloadAfterMemoryTopic(); }
      catch (e) { showError(e); }
    }
    async function removeMemoryTopic(m, topicId) {
      try { await api('DELETE', '/api/memories/' + m.id + '/topics/' + topicId); await reloadAfterMemoryTopic(); }
      catch (e) { showError(e); }
    }
    async function setTodoStatus(t, status) {
      editingTodo.value = null; deletingTodoId.value = null; dismissingTodoId.value = null; // todo 即将换组，清理编辑/删除/忽略态
      try {
        await api('PATCH', '/api/todos/' + t.id, { status });
        await reloadAllTodos();
      } catch (e) { showError(e); }
    }
    // ---------- 待办 inplace 编辑 + 删除 ----------
    const editingTodo = ref(null);
    function startEditTodo(t) { deletingTodoId.value = null; editingTodo.value = { id: t.id, title: t.title }; }
    function cancelEditTodo() { editingTodo.value = null; }
    async function saveEditTodo(reload) {
      const e = editingTodo.value;
      if (!e || !e.title.trim()) return;
      try { await api('PATCH', '/api/todos/' + e.id, { title: e.title.trim() }); editingTodo.value = null; if (reload) await reload(); }
      catch (e2) { showError(e2); }
    }
    // 2 步行内删除确认：deletingTodoId 存正待确认删除的 todo id
    const deletingTodoId = ref(null);
    function askDeleteTodo(t) { editingTodo.value = null; deletingTodoId.value = t.id; }
    function cancelDeleteTodo() { deletingTodoId.value = null; }
    async function confirmDeleteTodo(t, reload) {
      try { await api('DELETE', '/api/todos/' + t.id); deletingTodoId.value = null; if (reload) await reload(); }
      catch (e) { showError(e); }
    }
    // 2 步行内忽略确认：dismissingTodoId 存正待确认忽略的 todo id（与删除态互斥）
    const dismissingTodoId = ref(null);
    function askDismissTodo(t) { editingTodo.value = null; deletingTodoId.value = null; dismissingTodoId.value = t.id; }
    function cancelDismissTodo() { dismissingTodoId.value = null; }
    async function confirmDismissTodo(t) {
      try { await setTodoStatus(t, 'dismissed'); }
      catch (e) { showError(e); }
      finally { dismissingTodoId.value = null; }
    }
    async function jumpToSession(sessionId) {
      switchTab('timeline');
      // 从待办页跳来时强制展开（不因已展开而收起）
      try {
        detail.value = await api('GET', '/api/sessions/' + sessionId);
        expandedId.value = sessionId;
      } catch (e) { showError(e); }
    }

    // ---------- 时间线筛选 / 按天分组（纯前端） ----------
    // 搜索 + 日期范围 + 按天分组均为客户端计算：数据量 ≤200，O(n) 过滤可忽略。
    const tlSearch = ref('');
    const tlDateFrom = ref(''); // YYYY-MM-DD
    const tlDateTo = ref('');
    const tlPreset = ref(''); // 当前激活的时间快捷方式（空=无）
    function clearTlFilter() { tlSearch.value = ''; tlDateFrom.value = ''; tlDateTo.value = ''; tlPreset.value = ''; }
    // fmtDate：Date -> 'YYYY-MM-DD'（date input 值格式）
    function fmtDate(d) {
      const y = d.getFullYear();
      const m = String(d.getMonth() + 1).padStart(2, '0');
      const day = String(d.getDate()).padStart(2, '0');
      return `${y}-${m}-${day}`;
    }
    // applyPreset：点快捷方式即设置 from/to（复用既有日期过滤），并标记激活态。
    // 手动改任一日期输入会清空 tlPreset（视为自定义范围）。
    function applyPreset(name) {
      const today = new Date(); today.setHours(0, 0, 0, 0);
      const daysAgo = (n) => { const d = new Date(today); d.setDate(d.getDate() - n); return d; };
      // 周一=0..周日=6（JS getDay 周日=0，转换）
      const mondayOffset = (today.getDay() + 6) % 7;
      let from, to;
      switch (name) {
        case '1d': from = daysAgo(0); to = today; break;                  // 最近1天=今天
        case '2d': from = daysAgo(1); to = today; break;
        case '3d': from = daysAgo(2); to = today; break;
        case '7d': from = daysAgo(6); to = today; break;
        case '30d': from = daysAgo(29); to = today; break;
        case 'week': from = daysAgo(mondayOffset); to = daysAgo(mondayOffset - 6); break;      // 本周一~周日
        case 'lastweek': from = daysAgo(mondayOffset + 7); to = daysAgo(mondayOffset + 1); break; // 上周一~周日
        case 'month': from = new Date(today.getFullYear(), today.getMonth(), 1); to = new Date(today.getFullYear(), today.getMonth() + 1, 0); break; // 本月1~月末
        default: clearTlFilter(); return;
      }
      tlDateFrom.value = fmtDate(from);
      tlDateTo.value = fmtDate(to);
      tlPreset.value = name;
    }
    function dayKey(iso) { return (iso || '').slice(0, 10); } // 取 YYYY-MM-DD
    const WEEK_ZH = ['日', '一', '二', '三', '四', '五', '六'];
    function dayLabel(key) {
      if (!key) return '';
      const d = new Date(key + 'T00:00:00');
      if (isNaN(d.getTime())) return key;
      return `${d.getMonth() + 1}月${d.getDate()}日 · 周${WEEK_ZH[d.getDay()]}`;
    }
    // filteredSessions：搜索(asr_preview+filename 子串) + 日期范围
    const filteredSessions = computed(() => {
      const q = tlSearch.value.trim().toLowerCase();
      const from = tlDateFrom.value, to = tlDateTo.value;
      return sessions.value.filter(s => {
        if (q) {
          const hay = ((s.asr_preview || '') + ' ' + (s.filename || '')).toLowerCase();
          if (!hay.includes(q)) return false;
        }
        const k = dayKey(s.created_at);
        if (from && k < from) return false;
        if (to && k > to) return false;
        return true;
      });
    });
    // sessionsByDay：按日期分组并倒序，保留 filteredSessions 内的原始顺序
    const sessionsByDay = computed(() => {
      const map = {}, order = [];
      for (const s of filteredSessions.value) {
        const k = dayKey(s.created_at) || '未知';
        if (!map[k]) { map[k] = []; order.push(k); }
        map[k].push(s);
      }
      order.sort((a, b) => (a < b ? 1 : a > b ? -1 : 0));
      return order.map(k => ({ day: k, label: dayLabel(k), items: map[k] }));
    });

    // ---------- ASR 转写就地编辑 ----------
    // segDraft = { [segId]: text }：键存在即该段处于编辑态（点击进入、保存/取消清键）。
    const segDraft = ref({});
    function segEditing(sg) { return !!(sg && sg.id !== undefined && segDraft.value[sg.id] !== undefined); }
    function startEditSeg(sg) { segDraft.value[sg.id] = sg.text; }
    function cancelEditSeg(sg) { delete segDraft.value[sg.id]; }
    const segDirty = computed(() => Object.keys(segDraft.value).length > 0);
    // 原始 ASR 视图开关：true 时转写段以只读方式展示 ASR 原始 spk 标签 + 毫秒时间戳 + 文本，
    // 便于排查「同人被拆成 spk0/spk1」类 diarization 问题（speaker stage 会用声纹聚类兜底合并）。
    const rawAsrView = ref(false);
    // 切换函数（与 toggleSpeakerFilter/toggleEnrollForm 一致用函数式，避免内联赋值在某些 Vue 编译路径下不触发响应）
    function toggleRawAsr() { rawAsrView.value = !rawAsrView.value; }

    // ---------- timeline 转写段：逐段播放 + 多段合并(归到同一说话人) ----------
    // 逐段播放复用详情区顶部那个 <audio :src="audioUrl(s.id)">（ref=sessionAudioEl）：
    // 点某段 ▶ → seek 到 start_ms 播放，timeupdate 到 end_ms 暂停（同声纹 tab playSpeakerSegment 思路）。
    const sessionAudioEl = ref(null);   // 详情区 <audio> 元素引用（在 v-for 内，Vue 收集成数组，见 tlAudio）
    const tlPlayingSegId = ref(null);   // 正在播放的段 id（▶→⏸ 高亮）
    // 取详情区 <audio>：ref 在 v-for(会话列表) 内会被 Vue 收集成数组，取首元素（同声纹 tab voiceAudio）。
    function tlAudio() { const r = sessionAudioEl.value; return Array.isArray(r) ? r[0] : r; }
    // 切换某段播放：正在播此段且未暂停→暂停；否则从该段 start_ms 起播。
    function toggleTimelineSegPlay(sg) {
      const a = tlAudio();
      if (tlPlayingSegId.value === sg.id && a && !a.paused) { a.pause(); tlPlayingSegId.value = null; return; }
      playTimelineSeg(sg);
    }
    function playTimelineSeg(sg) {
      const a = tlAudio(); if (!a) return;
      const startSec = (sg.start_ms || 0) / 1000, endSec = (sg.end_ms || 0) / 1000;
      const go = () => {
        a.currentTime = startSec;
        a.dataset.end = String(endSec); // timeupdate 到此秒暂停 → 只播这一段
        a.play().catch(() => {});
        tlPlayingSegId.value = sg.id;
      };
      // preload="none"：首次未加载 → 先 play 触发加载，等 loadedmetadata 再 seek（切源后立即 seek 会被忽略）
      if (a.readyState >= 1) go();
      else { a.addEventListener('loadedmetadata', go, { once: true }); a.play().catch(() => {}); }
      // 分段播完或文件播完时，复位播放头到 0，确保下次点整段播放从头开始
      const resetHead = () => { a.currentTime = 0; };
      a.addEventListener('ended', () => { tlPlayingSegId.value = null; delete a.dataset.end; resetHead(); }, { once: true });
    }
    // <audio> 的 timeupdate：播到当前段 end_ms 即暂停并复位高亮 + 播放头归零。
    function onTimelineAudioTimeUpdate() {
      const a = tlAudio();
      if (!a || !a.dataset.end) return;
      if (a.currentTime >= parseFloat(a.dataset.end)) {
        a.pause();
        tlPlayingSegId.value = null;
        delete a.dataset.end;
        a.currentTime = 0; // 复位播放头，确保下次点整段播放从头开始
      }
    }

    // 合并模式：选多段「拆开」的转写 → 一起 PATCH 归到同一说话人（同人统一，纠正 ASR 把一人拆成多 spk）。
    // 复用既有 PATCH /api/sessions/{id}/segments/{segId}/speaker（逐段循环，无需新端点）。
    const mergeMode = ref(false);
    const mergeSelected = reactive({});  // { [segId]: true }
    const mergeCount = computed(() => Object.keys(mergeSelected).filter(k => mergeSelected[k]).length);
    const mergeTarget = ref(null);      // 确认合并时归到的目标 speaker_id
    function enterMergeMode() { rawAsrView.value = false; mergeMode.value = true; for (const k in mergeSelected) delete mergeSelected[k]; mergeTarget.value = null; }
    function cancelMerge() { mergeMode.value = false; for (const k in mergeSelected) delete mergeSelected[k]; mergeTarget.value = null; }
    function toggleMergeSelect(sg) { if (mergeSelected[sg.id]) delete mergeSelected[sg.id]; else mergeSelected[sg.id] = true; }
    async function confirmMerge(s) {
      const ids = Object.keys(mergeSelected).filter(k => mergeSelected[k]);
      if (ids.length < 2 || !mergeTarget.value) return;
      try {
        // 段合并成一条（text 拼接+[min,max]时间+目标说话人，删其余段），非批量改判
        await api('POST', '/api/sessions/' + s.id + '/segments/merge', { segment_ids: ids, speaker_id: mergeTarget.value });
        cancelMerge();
        await reloadSession(s.id);
        toast.value = '已合并 ' + ids.length + ' 段为一条'; setTimeout(() => { toast.value = ''; }, 2000);
      } catch (e) { showError(e); }
    }

    // ---------- 从转写段音频录入声纹（timeline「用此段录音纹」） ----------
    // 用某段对应时间段的音频算声纹录入新说话人（后端切 transcoded wav 切片提向，受最小时长约束）。
    // 前端最小时长与后端默认 3000ms 对齐（后端为权威校验，前端仅做禁用提示）。
    const MIN_ENROLL_MS = 3000;
    const segEnrollId = ref(null);   // 正在录入的段 id
    const segEnrollName = ref('');  // 录入名输入
    function segDurMs(sg) { return (sg.end_ms || 0) - (sg.start_ms || 0); }
    function canEnrollSeg(sg) { return segDurMs(sg) >= MIN_ENROLL_MS; }
    function startSegEnroll(sg) { segEnrollId.value = sg.id; segEnrollName.value = ''; }
    function cancelSegEnroll() { segEnrollId.value = null; segEnrollName.value = ''; }
    async function confirmSegEnroll(s, sg) {
      if (!segEnrollName.value.trim()) return;
      try {
        await api('POST', '/api/sessions/' + s.id + '/segments/' + sg.id + '/enroll', { name: segEnrollName.value.trim() });
        cancelSegEnroll();
        await loadAllSpeakers(); // 新说话人立即可在换人下拉选到
        await reloadSession(s.id);
        toast.value = '已从该段录入说话人'; setTimeout(() => { toast.value = ''; }, 2000);
      } catch (e) { showError(e); }
    }

    async function saveTranscript(s) {
      const draft = segDraft.value;
      const segs = Object.keys(draft).map(id => ({ id, text: draft[id] }));
      if (!segs.length) return;
      try {
        await api('PATCH', '/api/sessions/' + s.id + '/transcript', { segments: segs });
        segDraft.value = {};
        await reloadSession(s.id);
        toast.value = '转写已保存'; setTimeout(() => { toast.value = ''; }, 2000);
      } catch (e) { showError(e); }
    }

    // ---------- 说话人面板（过滤 / 改名 / 删除 / 换人 / 录入） ----------
    // detail.speakers 来自 GetSession（本会话出现过的说话人：speaker_id/name/source/segment_count/color_index）；
    // allSpeakers 是全量已登记说话人（换人下拉数据源，GET /api/speakers，字段用 id/name）。
    const speakerFilter = ref(null);      // 非空=只显示该 speaker_id 的转写段
    const renamingSpeaker = ref(null);    // {id,name}：改名进行中（就地一行输入）
    const enrollOpen = ref(false);        // 录入表单是否展开
    const enrollForm = reactive({ name: '', file: null }); // 录入表单：名称 + 语音样本文件
    const enrolling = ref(false);         // 录入提交中（按钮 loading + 禁用防重复提交）
    const allSpeakers = ref([]);          // 全量已登记说话人（换人下拉选项）

    // 说话人配色板：面板 chip 与转写段徽标都按「在 detail.speakers 里的下标」取色，
    // 保证同一个人在面板和转写里颜色一致（8 色循环，超出取模复用）。
    const SPEAKER_PALETTE = [
      { bg: '#4338ca', fg: '#fff' }, { bg: '#0e7490', fg: '#fff' }, { bg: '#b45309', fg: '#fff' },
      { bg: '#6d28d9', fg: '#fff' }, { bg: '#047857', fg: '#fff' }, { bg: '#be123c', fg: '#fff' },
      { bg: '#1e40af', fg: '#fff' }, { bg: '#9d174d', fg: '#fff' },
    ];
    function speakerColor(i) { return SPEAKER_PALETTE[i % SPEAKER_PALETTE.length]; }
    // segSpeakerBg：按转写段 speaker_id 在 detail.speakers 里的下标取背景色；
    // 未解析（无 speaker_id 或在面板列表里找不到）→ 灰（与 --muted 同色）。
    function segSpeakerBg(sg) {
      if (sg.speaker_id && detail.value && detail.value.speakers) {
        const idx = detail.value.speakers.findIndex(s => s.speaker_id === sg.speaker_id);
        if (idx >= 0) return SPEAKER_PALETTE[idx % SPEAKER_PALETTE.length].bg;
      }
      return '#9a9388'; // 未解析 → 灰
    }
    // 点面板 chip：切换成只看该说话人的转写段；再点同一个 = 取消过滤（回到全部）。
    function toggleSpeakerFilter(id) { speakerFilter.value = speakerFilter.value === id ? null : id; }

    // 打开录入表单并清空（每次打开都是干净状态）
    function openEnroll() { enrollOpen.value = true; enrollForm.name = ''; enrollForm.file = null; }
    // 拖拽落文件到录入区（与录音页 onDrop 同思路，取第一个文件）
    function onEnrollDrop(e) { if (e.dataTransfer.files.length) enrollForm.file = e.dataTransfer.files[0]; }
    // 声纹录入：麦克风直录（独立 recorder，不与录音页的 recorder 冲突）。
    // 录完产 webm File → enrollForm.file，与拖拽文件同路径（后端 ffmpeg 转码 wav16k 后提声纹）。
    const enrollRecording = ref(false);
    const enrollRecSeconds = ref(0);
    let enrollRec = null, enrollRecTimer = null;
    // 提示用户照着念的样本文字（约 15s 自然语速，录到足够时长的稳定人声做声纹更准）。
    const enrollPromptText = '你好，我正在录入声纹样本。今天天气不错，晚点打算出去散散步。记得给团队发一封邮件，确认下周的会议时间。如果有任何问题，随时联系我，谢谢。';
    async function startEnrollRec() {
      try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        enrollRec = new MediaRecorder(stream, { mimeType: 'audio/webm;codecs=opus' });
        const chunks = [];
        enrollRec.ondataavailable = e => chunks.push(e.data);
        enrollRec.onstop = () => {
          stream.getTracks().forEach(t => t.stop());
          enrollForm.file = new File(chunks, 'enroll-' + Date.now() + '.webm', { type: 'audio/webm' });
        };
        enrollRec.start();
        enrollRecording.value = true; enrollRecSeconds.value = 0;
        enrollRecTimer = setInterval(() => enrollRecSeconds.value++, 1000);
      } catch (e) { showError(e); }
    }
    function stopEnrollRec() {
      if (enrollRec) { enrollRec.stop(); enrollRecording.value = false; clearInterval(enrollRecTimer); }
    }
    // 拉全量已登记说话人（换人下拉的数据源）；失败静默——它只是下拉选项，不该打断主流程。
    async function loadAllSpeakers() {
      try { const d = await api('GET', '/api/speakers'); allSpeakers.value = d.speakers || []; }
      catch (e) { /* 下拉数据源，失败静默 */ }
    }
    // 录入新说话人：multipart 上传（file + name）。成功后刷新 allSpeakers（新人立即可在换人下拉选到）并 toast。
    // 不重拉当前会话——新登记的人此刻在本会话还没有任何转写段，面板（按段计数）本就不会显示他。
    async function submitEnroll() {
      if (enrolling.value) return; // 防重复提交
      const fd = new FormData();
      fd.append('file', enrollForm.file);
      fd.append('name', enrollForm.name);
      enrolling.value = true;
      try {
        await api('POST', '/api/speakers', fd);
        await loadAllSpeakers();
        enrollOpen.value = false;     // 时间线面板的录入表单
        showEnrollForm.value = false; // 声纹 tab 的录入表单（两处共用 submitEnroll，成功后一并收起）
        toast.value = '已录入说话人'; setTimeout(() => { toast.value = ''; }, 2000);
      } catch (e) { showError(e); }
      finally { enrolling.value = false; }
    }
    // 改名：点 chip/名字的 ✎ 进入改名行；保存 PATCH /api/speakers/{id}，成功后重拉详情（面板名同步）+ 全量列表。
    // 说话人对象有两种来源：时间线面板的 detail.speakers 用 speaker_id 字段；声纹 tab 的 allSpeakers 用 id 字段。
    // 用 `sp.speaker_id || sp.id` 兼容两者（两者都是同一个 speaker 主键，只是响应结构里的字段名不同）。
    function startRenameSpeaker(sp) { renamingSpeaker.value = { id: sp.speaker_id || sp.id, name: sp.name }; }
    async function commitRenameSpeaker() {
      if (!renamingSpeaker.value || !renamingSpeaker.value.name.trim()) return; // 空名不发
      try {
        await api('PATCH', '/api/speakers/' + renamingSpeaker.value.id, { name: renamingSpeaker.value.name.trim() });
        renamingSpeaker.value = null;
        // 时间线面板内改名 → 重拉当前会话，同步转写段徽标名；声纹 tab 无展开会话（detail 为空），跳过重拉。
        if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
        await loadAllSpeakers();
      } catch (e) { showError(e); }
    }
    // 删除说话人：用原生 confirm 做二次确认（chip 空间紧凑，不适合像列表那样铺开行内两步确认；
    // 且原生对话框是成熟方案）。后端 DELETE 会把关联段的 speaker_id 置空 → 这些段变「未解析」。
    async function askDeleteSpeaker(sp) {
      if (!confirm('删除说话人「' + sp.name + '」？关联的转写段将变为未解析。')) return;
      try {
        await api('DELETE', '/api/speakers/' + (sp.speaker_id || sp.id));
        await loadAllSpeakers();
        // 时间线面板删除 → 重拉当前会话让相关段变「未解析」；声纹 tab 无展开会话（detail 为空），跳过。
        if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
      } catch (e) { showError(e); }
    }
    // 换人：把某转写段改判为另一说话人（修正识别错误）。选「+ 新加…」则转去打开录入表单。
    async function reassignSegment(sg, val) {
      if (val === '__new') { openEnroll(); return; }
      try {
        await api('PATCH', '/api/sessions/' + detail.value.session.id + '/segments/' + sg.id + '/speaker', { speaker_id: val });
        await reloadSession(detail.value.session.id);
      } catch (e) { showError(e); }
    }

    // ---------- 建议名字（speakername stage 的 LLM 候选：名称+置信度数值，用户点选确认） ----------
    // 与后端 stage_speaker_name.go 的 autoNamePattern 保持一致：
    // 只有名字仍是自动随机名（说话人+5位[a-z0-9]）的说话人才展示建议区——
    // 用户改过名（含采纳过候选）后不再打扰。
    const AUTO_NAME_RE = /^说话人[a-z0-9]{5}$/;
    function hasNameCandidates(sp) {
      return AUTO_NAME_RE.test(sp.name) && (sp.name_candidates || []).length > 0;
    }
    // 采纳候选：把候选名写为说话人正式名。复用改名 PATCH（后端改名成功即清空全部候选）。
    // sp 兼容两种来源：时间线面板 detail.speakers（speaker_id 字段）/ 声纹 tab allSpeakers（id 字段）。
    async function acceptNameCandidate(sp, cand) {
      const id = sp.speaker_id || sp.id;
      try {
        await api('PATCH', '/api/speakers/' + id, { name: cand.name });
        if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
        await loadAllSpeakers();
      } catch (e) { showError(e); }
    }
    // 忽略单个候选：删该行（后端幂等）。成功后刷新两处列表。
    async function dismissNameCandidate(sp, cand) {
      const id = sp.speaker_id || sp.id;
      try {
        await api('DELETE', '/api/speakers/' + id + '/name-candidates?name=' + encodeURIComponent(cand.name));
        if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
        await loadAllSpeakers();
      } catch (e) { showError(e); }
    }

    // ---------- 人物 tab（用户画像：名册 / 详情 / 确认队列 / 回填） ----------
    // 后端契约见 internal/api/person.go：读直连 repo 响应、变更走 Service（审计+事务）。
    const persons = ref([]);            // 名册（GET /api/persons → {persons:[PersonWithPending]}）
    const personDetail = ref(null);     // 当前详情（GET /api/persons/{id}，Task 2 用）
    const showNewPerson = ref(false);   // 新建人物表单开合
    const newPerson = ref({ display_name: '', speaker_id: '', summary: '' });
    const creatingPerson = ref(false);  // 防重复提交
    const renamingPerson = ref(null);   // { id, name } 详情改名（Task 2 用）

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
    // 工具栏「＋ 新建/收起」切换：收起时走 cancelNewPerson 一并清草稿（对齐 toggleNewTopic，
    // 避免 inline `showNewPerson = !showNewPerson` 收起不重置、重开残留旧输入的不对称）。
    function toggleNewPerson() { if (showNewPerson.value) cancelNewPerson(); else showNewPerson.value = true; }
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
    // 点名册卡片：已展开收起；否则拉详情（Task 2 渲染详情卡；本任务先实现数据拉取与切换）
    async function togglePerson(id) {
      if (personDetail.value && personDetail.value.person.id === id) { closePersonDetail(); return; }
      closePersonDetail(); // 切换前清旧人物的临时态（历史抽屉/加属性草稿/编辑删除态），防跨人泄漏
      try { personDetail.value = await api('GET', '/api/persons/' + id); }
      catch (e) { showError(e); }
    }
    function closePersonDetail() {
      personDetail.value = null;
      renamingPerson.value = null;
      archivingPersonId.value = null; // 切换详情/收起时一并清归档确认态（对齐 toggleSession 折叠清 deletingSessionId）
      // 属性手动管理临时态（Task 3）：加属性表单(含草稿) / 就地改值 / 删除确认 / 历史抽屉一并清空
      editingAttr.value = null; deletingAttrId.value = null; attrHistory.value = null;
      showAddAttr.value = false; addAttrForm.attr_key = ''; addAttrForm.value = '';
      // Task 4 将在此追加：showAddRel 等关系相关临时态清理
    }
    async function reloadPersonDetail() {
      if (!personDetail.value) return;
      try { personDetail.value = await api('GET', '/api/persons/' + personDetail.value.person.id); }
      catch (e) { showError(e); }
    }
    // 人物改名（详情内就地编辑，Task 2 渲染；本任务先定义）
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
    // epistemic_type（认知来源）→ 中文标签，属性徽标用。
    // 后端枚举见 internal/profile/fact.go：observed（对话直陈）/inferred（可推断）/
    // predicted（预测）/suggested（建议）。未知值原样返回，避免出现空徽标。
    function epiText(t) {
      return { observed: '直述', inferred: '推断', predicted: '预测', suggested: '建议' }[t] || t;
    }
    // 关系对端人物名：从已加载的名册缓存（persons.value，GET /api/persons）里按 id 查显示名。
    // 查不到就回退显示「未知联系人」——对端可能已被忽略（dismissed）或还没建卡，此时名册里
    // 没有它；长雪花 id 直接展示不友好，故用友好占位文案而非原始 id。
    function personNameOf(id) {
      const p = persons.value.find(x => x.id === id);
      return p ? p.display_name : '未知联系人';
    }

    // ---------- 人物属性手动管理（加 / 改(留痕) / 删 / 修改历史抽屉） ----------
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
    // 加属性表单开合切换：收起时一并清草稿（对齐 toggleNewPerson，防重开残留旧输入的不对称）。
    // 底部「＋ 加属性」与表单 ✕ 均走此函数，保证「关闭 ⇒ 清草稿」在所有路径对称。
    function toggleAddAttr() {
      showAddAttr.value = !showAddAttr.value;
      if (!showAddAttr.value) { addAttrForm.attr_key = ''; addAttrForm.value = ''; }
    }
    // 改值 = PATCH（后端 supersede 旧行留痕；body 必须带行自身的 attr_key，与目标行不一致会 400）
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

    // ---------- 声纹 tab（名册管理：列表 / 录入 / 改名 / 删除 + 点开看关联录音并按时间段播放） ----------
    // 复用说话人面板既有能力：allSpeakers / enrollForm / enrolling / submitEnroll / onEnrollDrop、
    // renamingSpeaker / startRenameSpeaker / commitRenameSpeaker、askDeleteSpeaker、loadAllSpeakers、speakerColor。
    // 本 tab 独有的临时态集中在此：录入表单开合、当前展开的说话人、其片段列表与播放态。
    const showEnrollForm = ref(false);    // 声纹 tab 的录入表单开合（与时间线面板的 enrollOpen 相互独立，避免跨 tab 串状态）
    const expandedSpeakerId = ref(null);  // 当前展开「关联录音」的说话人 id（对应 allSpeakers 里的 sp.id）
    const speakerSegments = ref([]);      // 当前展开说话人的跨 session 片段列表（GET /api/speakers/{id}/segments）
    const speakerSegLoading = ref(false); // 片段拉取中（展开按钮上显 spinner）
    const playingSegId = ref(null);       // 正在播放的 segment_id（对应「播放」按钮高亮）
    const voiceAudioEl = ref(null);       // 展开区里的 <audio> 元素引用（在 v-for 内，取用见 voiceAudio()）

    // 录入表单开合：已开则收起；打开时清空表单（每次都是干净状态，复用 enrollForm）。
    // 只翻 showEnrollForm、不动 enrollOpen——否则切回时间线并展开会话时，录入表单会莫名展开。
    function toggleEnrollForm() {
      if (showEnrollForm.value) { showEnrollForm.value = false; return; }
      enrollForm.name = ''; enrollForm.file = null;
      showEnrollForm.value = true;
    }

    // Vue3 里放在 v-for 内的模板 ref 会被收集成「数组」；本区同一时刻只展开一个说话人（只挂一个 <audio>），
    // 取数组首元素即可；同时兼容非数组（万一将来把 <audio> 挪出 v-for）。
    function voiceAudio() {
      const r = voiceAudioEl.value;
      return Array.isArray(r) ? r[0] : r;
    }

    // 点开/收起某说话人的「关联录音」。展开时拉 GET /api/speakers/{id}/segments
    //（后端已按录音时间倒序、段序升序返回）；再点同一个 = 收起并清空片段与播放态。
    async function toggleSpeakerSegments(id) {
      if (expandedSpeakerId.value === id) {
        expandedSpeakerId.value = null; speakerSegments.value = []; playingSegId.value = null;
        return;
      }
      expandedSpeakerId.value = id;
      speakerSegLoading.value = true;
      speakerSegments.value = [];
      playingSegId.value = null;
      try {
        const d = await api('GET', '/api/speakers/' + id + '/segments');
        speakerSegments.value = d.segments || [];
      } catch (e) { showError(e); }
      finally { speakerSegLoading.value = false; }
    }

    // 把片段按录音(session)分组，每组 {label, items}。后端已按 created_at DESC 排好，
    // 这里保持「首次出现顺序」即为录音时间倒序；label = 文件名 · 录音时间。
    const speakerSegmentsBySession = computed(() => {
      const map = {}, order = [];
      for (const seg of speakerSegments.value) {
        const k = seg.session_id;
        if (!map[k]) { map[k] = { label: (seg.filename || '录音') + ' · ' + fmtTime(seg.created_at), items: [] }; order.push(k); }
        map[k].items.push(seg);
      }
      return order.map(k => map[k]);
    });

    // 播放某片段：共用展开区里的 <audio>（原生 HTMLAudioElement，成熟方案）。
    // 同一 session 已加载 → 直接 seek 到 start_ms 播放；换 session 需先设 src，等 loadedmetadata
    // 后再 seek+play（切源后立即 seek 会被浏览器忽略）。播到 end_ms 由 onVoiceAudioTimeUpdate 暂停。
    function playSpeakerSegment(seg) {
      const a = voiceAudio();
      if (!a) return;
      playingSegId.value = seg.segment_id;
      const startSec = (seg.start_ms || 0) / 1000, endSec = (seg.end_ms || 0) / 1000;
      const seekAndPlay = () => {
        a.currentTime = startSec;
        a.dataset.end = String(endSec); // timeupdate 到此秒即暂停，实现「只播这一段」
        a.play().catch(() => {});       // 自动播放被拦时静默（用户已手动点，通常不会拦）
      };
      if (a.dataset.sid === String(seg.session_id)) {
        seekAndPlay();
      } else {
        a.src = '/api/sessions/' + seg.session_id + '/audio';
        a.dataset.sid = String(seg.session_id);
        a.addEventListener('loadedmetadata', seekAndPlay, { once: true });
        a.play().catch(() => {}); // 在用户手势内 play：既启动加载、又解锁自动播放策略——
        // 否则 loadedmetadata 回调(异步、非手势)里的 play 会被拦(.catch 吞)，导致需点第2次才放
      }
      // 自然播放到文件结尾也复位高亮（正常会被 timeupdate 先在 end_ms 处 pause）
      a.addEventListener('ended', () => { playingSegId.value = null; }, { once: true });
    }

    // <audio> 的 timeupdate 回调：播到当前片段 end_ms 即暂停并复位高亮。
    function onVoiceAudioTimeUpdate() {
      const a = voiceAudio();
      if (!a || !a.dataset.end) return;
      if (a.currentTime >= parseFloat(a.dataset.end)) { a.pause(); playingSegId.value = null; }
    }

    // 毫秒 → 时间标签：≥1 分钟显 "m:ss.sss"，不足 1 分钟显 "s.sss s"，用于展示片段起止时间。
    function fmtSec(ms) {
      const s = (ms || 0) / 1000;
      const m = Math.floor(s / 60);
      const r = (s - m * 60).toFixed(3);
      return m > 0 ? `${m}:${r.padStart(6, '0')}` : `${r}s`;
    }

    // 声纹 tab 手动合并说话人（参考主题页手动合并：选择模式 + 底部确认条）。
    // 选多个说话人 → 选一个作目标(保留) → 其余源的段改指目标、源删除。
    // 纠正 ASR 把同人拆成多个说话人；对应后端 POST /api/speakers/merge。
    const spMergeMode = ref(false);
    const spMergeSelected = ref([]);    // 勾选的 speaker id
    const spMergeConfirming = ref(false); // 已点开始合并→选目标阶段
    const spMergeTarget = ref(null);     // 选作目标(保留)的 speaker id
    function startSpMerge() { spMergeMode.value = true; spMergeSelected.value = []; spMergeConfirming.value = false; spMergeTarget.value = null; }
    function cancelSpMerge() { spMergeMode.value = false; spMergeSelected.value = []; spMergeConfirming.value = false; spMergeTarget.value = null; }
    function toggleSpSelect(sp) {
      const i = spMergeSelected.value.indexOf(sp.id);
      if (i >= 0) { spMergeSelected.value.splice(i, 1); if (spMergeTarget.value === sp.id) spMergeTarget.value = null; }
      else spMergeSelected.value.push(sp.id);
    }
    // 开始合并：进入选目标阶段，默认目标=首个选中
    function startSpConfirm() {
      spMergeConfirming.value = true;
      spMergeTarget.value = spMergeSelected.value[0] || null;
    }
    async function applySpMerge() {
      if (spMergeSelected.value.length < 2) { toast.value = '至少选 2 个说话人'; return; }
      if (!spMergeTarget.value) { toast.value = '请选择保留的目标说话人'; return; }
      const sources = spMergeSelected.value.filter(id => id !== spMergeTarget.value);
      if (!sources.length) { toast.value = '目标之外还需至少 1 个源'; return; }
      try {
        await api('POST', '/api/speakers/merge', { source_ids: sources, target_id: spMergeTarget.value });
        cancelSpMerge();
        await loadAllSpeakers();
        // 合并影响当前展开会话的说话人/段 → 重拉同步（声纹 tab 无展开会话则 detail 为空，跳过）
        if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
        toast.value = '已合并 ' + sources.length + ' 个说话人到目标'; setTimeout(() => { toast.value = ''; }, 2000);
      } catch (e) { showError(e); }
    }

    // ---------- 重新提取（基于最新 ASR 重跑 segment→extract） ----------
    // 点卡片「重新提取」→ 2 步确认 → 若有未保存转写先存盘 → POST reextract 建任务
    // → 轮询 job 状态 → 完成后刷新列表+详情。2 步确认提示会覆盖旧记忆/待办。
    const reextractingId = ref(null);   // 正在重新提取的 session id（卡片显 loading）
    const reextractConfirmId = ref(null); // 待确认重新提取的 session id
    let reextractPollTimer = null;
    function askReextract(s) { deletingSessionId.value = null; reextractConfirmId.value = s.id; }
    function cancelReextract() { reextractConfirmId.value = null; }
    async function confirmReextract(s) { reextractConfirmId.value = null; await reextractSession(s); }
    async function reextractSession(s) {
      if (reextractingId.value) return;
      // 当前展开且有未保存转写修改 → 先存盘，确保用最新 ASR 提取
      if (expandedId.value === s.id && segDirty.value) {
        await saveTranscript(s);
      }
      reextractingId.value = s.id;
      try {
        await api('POST', '/api/sessions/' + s.id + '/reextract', {});
        toast.value = '正在重新提取…';
        const poll = async () => {
          let st = '', err = '';
          try {
            const r = await api('GET', '/api/sessions/' + s.id);
            st = r.job ? r.job.status : (r.session && r.session.status) || '';
            err = r.job ? (r.job.last_error || '') : '';
          } catch (e) { /* 轮询失败静默重试 */ }
          if (st === 'done' || st === 'completed') {
            reextractingId.value = null;
            toast.value = '重新提取完成'; setTimeout(() => { toast.value = ''; }, 2500);
            await loadSessions();
            if (expandedId.value === s.id) await reloadSession(s.id);
          } else if (st === 'failed') {
            reextractingId.value = null;
            toast.value = '重新提取失败' + (err ? '：' + err : '');
            setTimeout(() => { toast.value = ''; }, 4000);
          } else {
            reextractPollTimer = setTimeout(poll, 2000);
          }
        };
        poll();
      } catch (e) {
        reextractingId.value = null;
        showError(e);
      }
    }

    // ---------- 重新识别说话人（清空说话人归属 + 重跑 speaker stage 用最新声纹库 1:N） ----------
    // 区别于重新提取（speaker 幂等跳过、不改已有归属）；重新识别会覆盖手动换人，故二次确认。
    const reidentifyingId = ref(null);     // 正在重新识别的 session id（卡片显 loading）
    const reidentifyConfirmId = ref(null); // 待确认重新识别的 session id
    let reidentifyPollTimer = null;
    function askReidentify(s) { deletingSessionId.value = null; reextractConfirmId.value = null; reidentifyConfirmId.value = s.id; }
    function cancelReidentify() { reidentifyConfirmId.value = null; }
    async function confirmReidentify(s) { reidentifyConfirmId.value = null; await reidentifySession(s); }
    async function reidentifySession(s) {
      if (reidentifyingId.value) return;
      reidentifyingId.value = s.id;
      try {
        await api('POST', '/api/sessions/' + s.id + '/reidentify', {});
        toast.value = '正在重新识别说话人…';
        const poll = async () => {
          let st = '', err = '';
          try {
            const r = await api('GET', '/api/sessions/' + s.id);
            st = r.job ? r.job.status : (r.session && r.session.status) || '';
            err = r.job ? (r.job.last_error || '') : '';
          } catch (e) { /* 轮询失败静默重试 */ }
          if (st === 'done' || st === 'completed') {
            reidentifyingId.value = null;
            toast.value = '重新识别完成'; setTimeout(() => { toast.value = ''; }, 2500);
            await loadSessions();
            if (expandedId.value === s.id) await reloadSession(s.id);
          } else if (st === 'failed') {
            reidentifyingId.value = null;
            toast.value = '重新识别失败' + (err ? '：' + err : '');
            setTimeout(() => { toast.value = ''; }, 4000);
          } else {
            reidentifyPollTimer = setTimeout(poll, 2000);
          }
        };
        poll();
      } catch (e) {
        reidentifyingId.value = null;
        showError(e);
      }
    }

    // ---------- 智能合并 / 记忆整理：即时反馈标志 ----------
    const consolidating = ref(false);
    const memConsolidating = ref(false);

    // ---------- 记忆 tab ----------
    // 复用 memories（loadMemories 拉 ?limit=200）。客户端按置信度过滤 + 搜索 + 时间倒序。
    const memSearch = ref('');
    const memConfMin = ref('0');
    const filteredMemories = computed(() => {
      const q = memSearch.value.trim().toLowerCase();
      const min = Number(memConfMin.value) || 0;
      return memories.value
        .filter(m => (m.confidence ?? 0) >= min)
        .filter(m => !q || ((m.title || '') + ' ' + (m.content || '')).toLowerCase().includes(q))
        .sort((a, b) => {
          // event_at 为空时回退 created_at，保证有值的不被排到末尾
          const ta = new Date(a.event_at || a.created_at).getTime();
          const tb = new Date(b.event_at || b.created_at).getTime();
          return (tb || 0) - (ta || 0);
        });
    });
    // ---------- 标签页切换 ----------
    function switchTab(name) {
      tab.value = name;
      if (name === 'timeline') { deletingSessionId.value = null; reextractConfirmId.value = null; segDraft.value = {}; loadSessions(); loadAllSpeakers(); }
      if (name === 'memories') { memSearch.value = ''; loadMemories(); }
      if (name === 'topics') { topicDetail.value = null; renaming.value = null; deletingTopicId.value = null; dismissingTopicId.value = null; cancelManualMerge(); loadDismissedTopics(); loadTopics(); }
      if (name === 'todos') { editingTodo.value = null; deletingTodoId.value = null; dismissingTodoId.value = null; loadTopics(); loadTodos(); loadDismissedTodos(); }
      // 声纹 tab：进入时复位本 tab 的临时态（收起录入表单/展开项/改名/播放）并拉全量名册。
      if (name === 'voiceprint') { showEnrollForm.value = false; expandedSpeakerId.value = null; speakerSegments.value = []; renamingSpeaker.value = null; playingSegId.value = null; loadAllSpeakers(); }
      // 人物 tab：进入时复位详情/归档确认态并拉名册（loadPending 是 Task 5 的，先不引用）。
      if (name === 'persons') { closePersonDetail(); archivingPersonId.value = null; loadPersons(); }
    }
    loadSessions();
    // 首屏 timeline 的「+ 关联」topic 下拉依赖 topics.value，而 loadTopics()
    // 原先只在 switchTab('topics'/'todos') 触发——首屏 timeline 下拉为空。
    // mount 时一并拉一次，保证首屏就有可选项（评审 M1）。
    loadTopics();
    // 换人下拉（转写段 <select>）的数据源，首屏 timeline 即可用。
    loadAllSpeakers();

    onUnmounted(() => { clearInterval(recTimer); clearInterval(pollTimer); clearTimeout(reextractPollTimer); });

    return {
      tab, toast, switchTab,
      fmtTime, fmtDue, typeMeta, statusText, todoStatusText, spClass,
      sessions, detail, expandedId, loadSessions, toggleSession, reloadSession, audioUrl, dismissingMemId, askDismissMem, cancelDismissMem, confirmDismissMem, retryJob, editingMem, startEditMemory, cancelEditMemory, saveEditMemory, deletingSessionId, askDeleteSession, cancelDeleteSession, confirmDeleteSession,
      tlSearch, tlDateFrom, tlDateTo, tlPreset, clearTlFilter, applyPreset, filteredSessions, sessionsByDay,
      segDraft, segEditing, startEditSeg, cancelEditSeg, segDirty, saveTranscript, rawAsrView, toggleRawAsr,
      sessionAudioEl, tlPlayingSegId, toggleTimelineSegPlay, onTimelineAudioTimeUpdate,
      mergeMode, mergeSelected, mergeCount, mergeTarget, enterMergeMode, cancelMerge, toggleMergeSelect, confirmMerge,
      MIN_ENROLL_MS, segEnrollId, segEnrollName, segDurMs, canEnrollSeg, startSegEnroll, cancelSegEnroll, confirmSegEnroll,
      speakerFilter, renamingSpeaker, enrollOpen, enrollForm, enrolling, allSpeakers,
      speakerColor, segSpeakerBg, toggleSpeakerFilter, openEnroll, onEnrollDrop, submitEnroll, loadAllSpeakers,
      startEnrollRec, stopEnrollRec, enrollRecording, enrollRecSeconds, enrollPromptText,
      startRenameSpeaker, commitRenameSpeaker, askDeleteSpeaker, reassignSegment,
      hasNameCandidates, acceptNameCandidate, dismissNameCandidate,
      showEnrollForm, toggleEnrollForm, expandedSpeakerId, speakerSegments, speakerSegLoading, playingSegId, voiceAudioEl, toggleSpeakerSegments, speakerSegmentsBySession, playSpeakerSegment, onVoiceAudioTimeUpdate, fmtSec,
      spMergeMode, spMergeSelected, spMergeConfirming, spMergeTarget, startSpMerge, cancelSpMerge, toggleSpSelect, startSpConfirm, applySpMerge,
      reextractingId, reextractConfirmId, askReextract, cancelReextract, confirmReextract,
      reidentifyingId, reidentifyConfirmId, askReidentify, cancelReidentify, confirmReidentify,
      recording, recSeconds, uploadInfo, startRec, stopRec, onDrop,
      lastAudioFile, matchInfo, voiceprintMatching, tryMatchVoiceprint,
      topics, topicDetail, showNewTopic, newTopic, creating, toggleNewTopic, cancelNewTopic, renaming,
      loadTopics, openTopic, closeTopicDetail, confirmTopic, startRename, commitRename, createTopic, suspectOf, mergeDraft, startConsolidate, consolidating, toggleMergeMember, applyMerge, deletingTopicId, askDeleteTopic, cancelDeleteTopic, confirmDeleteTopic, dismissingTopicId, askDismissTopic, cancelDismissTopic, confirmDismissTopic, restoreTopic, dismissedTopics, dismissedCollapsed, loadDismissedTopics,
      manualMergeMode, manualSelected, manualMergeName, manualConfirming, startManualMerge, cancelManualMerge, toggleManualSelect, applyManualMerge, startManualConfirm,
      memories, loadMemories, memoryDraft, startMemoryConsolidate, memConsolidating, toggleMemoryMember, toggleMemoryAdjustment, applyMemoryConsolidation,
      memSearch, memConfMin, filteredMemories,
      todos, doneCollapsed, suggestedTodos, activeTodos, doneTodos, dismissedTodos, dismissedTodoCollapsed, loadDismissedTodos,
      loadTodos, setTodoStatus, jumpToSession,
      editingTodo, startEditTodo, cancelEditTodo, saveEditTodo, deletingTodoId, askDeleteTodo, cancelDeleteTodo, confirmDeleteTodo, dismissingTodoId, askDismissTodo, cancelDismissTodo, confirmDismissTodo,
      topicChips, availableTopics, addTodoTopic, removeTodoTopic, addMemoryTopic, removeMemoryTopic,
      persons, personDetail, showNewPerson, newPerson, creatingPerson, loadPersons, cancelNewPerson, toggleNewPerson, createPerson, togglePerson, closePersonDetail, reloadPersonDetail, renamingPerson, startRenamePerson, commitRenamePerson, archivingPersonId, askArchivePerson, cancelArchivePerson, confirmArchivePerson,
      epiText, personNameOf,
      ATTR_KEYS, showAddAttr, addAttrForm, addingAttr, submitAddAttr, toggleAddAttr, editingAttr, startEditAttr, commitEditAttr, deletingAttrId, askDeleteAttr, confirmDeleteAttr, attrHistory, attrHistoryLoading, showAttrHistory, changeText, snapText,
    };
  }
});
// v-focus：表单展开时自动聚焦输入框（v-if 挂载即触发 mounted）
app.directive('focus', { mounted: el => el.focus() });
app.mount('#app');
