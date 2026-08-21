// 知微 Web 前端（Vue 3 CDN，无构建）。
// 标签页：时间线 / 录音 / Topics / 待办（问知微、今日留待后续 Sprint）。
const { createApp, ref, computed, onUnmounted } = Vue;

// memory 类型 → 中文标签与颜色（卡片徽标用）
const TYPE_META = {
  event:      { label: '事件', color: '#6366f1' },
  fact:       { label: '事实', color: '#0891b2' },
  decision:   { label: '决定', color: '#7c3aed' },
  idea:       { label: '想法', color: '#d97706' },
  problem:    { label: '问题', color: '#dc2626' },
  preference: { label: '偏好', color: '#059669' },
};

createApp({
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
      if (body !== undefined) {
        opt.headers = { 'Content-Type': 'application/json' };
        opt.body = JSON.stringify(body);
      }
      const r = await fetch(url, opt);
      if (!r.ok) {
        let msg = '请求失败';
        try { msg = (await r.json()).error || msg; } catch (e) {}
        throw new Error(msg);
      }
      return r.json();
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
        const d = await api('GET', '/api/sessions');
        sessions.value = d.sessions || [];
      } catch (e) { showError(e); }
    }
    // 点击会话卡片：已展开则收起；否则拉取详情就地展开（内联在当前卡片下方）
    async function toggleSession(id) {
      if (expandedId.value === id) {
        expandedId.value = null;
        detail.value = null;
        editingMem.value = null;
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
    // 原始音频流式地址（ ServeAudio 端点）
    function audioUrl(id) { return '/api/sessions/' + id + '/audio'; }
    async function dismissMemory(m) {
      editingMem.value = null;
      try {
        await api('PATCH', '/api/memories/' + m.id, { status: 'dismissed' });
        detail.value.memories = (detail.value.memories || []).filter(x => x.id !== m.id);
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
    let recorder = null, recTimer = null, pollTimer = null;

    async function startRec() {
      try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        recorder = new MediaRecorder(stream, { mimeType: 'audio/webm;codecs=opus' });
        const chunks = [];
        recorder.ondataavailable = e => chunks.push(e.data);
        recorder.onstop = () => {
          stream.getTracks().forEach(t => t.stop());
          upload(new File(chunks, 'record-' + Date.now() + '.webm', { type: 'audio/webm' }), 'web_record');
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
      if (f) upload(f, 'web_upload');
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

    // ---------- Topics ----------
    const topics = ref([]);
    const topicDetail = ref(null);
    const showNewTopic = ref(false);
    const newTopic = ref({ name: '', description: '' });
    const renaming = ref(null); // { id, name }

    async function loadTopics() {
      try {
        const d = await api('GET', '/api/topics');
        topics.value = d.topics || [];
      } catch (e) { showError(e); }
    }
    async function openTopic(id) {
      try {
        topicDetail.value = await api('GET', '/api/topics/' + id);
      } catch (e) { showError(e); }
    }
    async function confirmTopic(t) {
      try {
        await api('PATCH', '/api/topics/' + t.id, { status: 'active' });
        await loadTopics();
      } catch (e) { showError(e); }
    }
    async function dismissTopic(t) {
      try {
        await api('PATCH', '/api/topics/' + t.id, { status: 'dismissed' });
        await loadTopics();
      } catch (e) { showError(e); }
    }
    // 关闭主题详情：同时清空重命名状态，避免残留的输入框污染下次打开
    function closeTopicDetail() {
      topicDetail.value = null;
      renaming.value = null;
      editingMem.value = null;
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
      if (!newTopic.value.name.trim()) return;
      try {
        // 提交前对名称与描述做 trim，避免首尾空白进入库
        await api('POST', '/api/topics', {
          name: newTopic.value.name.trim(),
          description: newTopic.value.description.trim(),
        });
        newTopic.value = { name: '', description: '' };
        showNewTopic.value = false;
        await loadTopics();
      } catch (e) { showError(e); }
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

    // ---------- Topics 智能合并（LLM 提议 → 用户编辑确认 → 应用） ----------
    // mergeDraft：合并提议草稿，每组 {canonical_name, members:[{id,name,checked}]}。
    // 用 {id,name,checked} 成员对象而非裸 id 数组，便于模板正确渲染 checkbox（修掉
    // 计划初版里 g.member_ids[gi] 的索引错位）。
    const mergeDraft = ref(null);
    // startConsolidate：调后端 consolidate（LLM 按该用户主题列表给合并组提议），
    // 落进草稿供用户编辑 canonical 名与勾选成员；不改库。
    async function startConsolidate() {
      try {
        const d = await api('POST', '/api/topics/consolidate', {});
        mergeDraft.value = (d.groups || []).map(g => ({
          canonical_name: g.canonical_name || '',
          members: (g.member_ids || []).map(id => {
            const m = topics.value.find(t => t.id === id);
            return { id, name: m ? m.name : id, checked: true };
          }),
        }));
        if (!mergeDraft.value.length) toast.value = '暂无需要合并的主题';
      } catch (e) { showError(e); }
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
    async function addMemoryTopic(m, topicId) {
      try { await api('POST', '/api/memories/' + m.id + '/topics', { topic_id: topicId }); await reloadSession(detail.value.session.id); }
      catch (e) { showError(e); }
    }
    async function removeMemoryTopic(m, topicId) {
      try { await api('DELETE', '/api/memories/' + m.id + '/topics/' + topicId); await reloadSession(detail.value.session.id); }
      catch (e) { showError(e); }
    }
    async function setTodoStatus(t, status) {
      try {
        await api('PATCH', '/api/todos/' + t.id, { status });
        await loadTodos();
      } catch (e) { showError(e); }
    }
    async function jumpToSession(sessionId) {
      switchTab('timeline');
      // 从待办页跳来时强制展开（不因已展开而收起）
      try {
        detail.value = await api('GET', '/api/sessions/' + sessionId);
        expandedId.value = sessionId;
      } catch (e) { showError(e); }
    }

    // ---------- 标签页切换 ----------
    function switchTab(name) {
      tab.value = name;
      if (name === 'timeline') loadSessions();
      if (name === 'topics') { topicDetail.value = null; renaming.value = null; loadTopics(); }
      if (name === 'todos') { loadTopics(); loadTodos(); }
    }
    loadSessions();
    // 首屏 timeline 的「+ 关联」topic 下拉依赖 topics.value，而 loadTopics()
    // 原先只在 switchTab('topics'/'todos') 触发——首屏 timeline 下拉为空。
    // mount 时一并拉一次，保证首屏就有可选项（评审 M1）。
    loadTopics();

    onUnmounted(() => { clearInterval(recTimer); clearInterval(pollTimer); });

    return {
      tab, toast, switchTab,
      fmtTime, fmtDue, typeMeta, statusText, spClass,
      sessions, detail, expandedId, loadSessions, toggleSession, reloadSession, audioUrl, dismissMemory, retryJob, editingMem, startEditMemory, cancelEditMemory, saveEditMemory,
      recording, recSeconds, uploadInfo, startRec, stopRec, onDrop,
      topics, topicDetail, showNewTopic, newTopic, renaming,
      loadTopics, openTopic, closeTopicDetail, confirmTopic, dismissTopic, startRename, commitRename, createTopic, suspectOf, mergeDraft, startConsolidate, toggleMergeMember, applyMerge,
      todos, doneCollapsed, suggestedTodos, activeTodos, doneTodos,
      loadTodos, setTodoStatus, jumpToSession,
      topicChips, availableTopics, addTodoTopic, removeTodoTopic, addMemoryTopic, removeMemoryTopic,
    };
  }
}).mount('#app');
