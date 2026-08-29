// 知微 Web 前端（Vue 3 CDN，无构建）。
// 标签页：问知微 / 时间线 / 录音 / 声纹 / 主题 / 记忆 / 待办。
const { createApp, ref, reactive, computed, watch, onUnmounted, nextTick } = Vue;

// memory 类型 → 中文标签与颜色（卡片徽标用）
const TYPE_META = {
  event:      { label: '事件', color: '#6366f1' },
  fact:       { label: '事实', color: '#0891b2' },
  decision:   { label: '决定', color: '#7c3aed' },
  idea:       { label: '想法', color: '#d97706' },
  problem:    { label: '问题', color: '#dc2626' },
  preference: { label: '偏好', color: '#059669' },
};

// profile 平面元信息（变更事件流的平面标签；对齐 TYPE_META）。entity_kind 覆盖 8 平面。
const PROFILE_PLANE_META = {
  person:       { label: '人物', icon: '👤', color: '#6b7280' },
  attribute:    { label: '属性', icon: '🏷️', color: '#6366f1' },
  relationship: { label: '关系', icon: '🔗', color: '#7c3aed' },
  event:        { label: '大事记', icon: '📌', color: '#d97706' },
  metric:       { label: '指标', icon: '📈', color: '#059669' },
  cycle:        { label: '周期', icon: '💊', color: '#dc2626' },
  activity:     { label: '轨迹', icon: '🏃', color: '#0284c7' },
  pet:          { label: '宠物', icon: '🐱', color: '#0891b2' },
};

const app = createApp({
  setup() {
    const tab = ref('timeline');
    const toast = ref('');
    // notify(msg, ms) 轻量瞬时提示。合并对账兜底：main 侧代码统一调 notify() 出提示，feat 侧用单
    // toast ref + 行内 setTimeout；此处把 notify 实现为「写 toast ref + 定时清空」，让两侧调用都喂
    // 同一个 <div v-if="toast"> 渲染（index.html），避免 notify 未定义导致运行时 ReferenceError。
    // 单 toast 语义：后到的提示顶掉先到的（清旧定时器再置新），与既有 toast.value 直写行为一致。
    let toastTimer = null;
    function notify(msg, ms = 3000) {
      toast.value = msg;
      if (toastTimer) clearTimeout(toastTimer);
      toastTimer = setTimeout(() => { toast.value = ''; }, ms);
    }

    // ---------- 登录门（cookie + session 鉴权）状态 ----------
    // authed 三态：null=校验登录态中（初始，显示加载占位）/ false=显示登录页 / true=显示主界面。
    // 由启动时 checkAuth()（GET /api/auth/me）与 api() 的 401 拦截共同驱动。
    const authed = ref(null);
    const currentUser = ref(null);                  // 当前登录用户 {id, username, display_name}
    const loginForm = reactive({ username: '', password: '' });
    const loginError = ref('');                     // 登录失败提示（凭据错等，行内展示）
    const loggingIn = ref(false);                   // 登录请求中（按钮 loading + 禁用防重复提交）

    // ---------- 通用 ----------
    function fmtTime(iso) { return iso ? new Date(iso).toLocaleString('zh-CN') : ''; }
    // envSounds：把详情里的 background_sounds（可能是 JSON 字符串或数组）归一成 string[]，过滤"无"/空。
    function envSounds(v) {
      let arr = v;
      if (typeof v === 'string') { try { arr = JSON.parse(v); } catch (e) { arr = []; } }
      if (!Array.isArray(arr)) return [];
      return arr.filter(s => s && s !== '无');
    }
    function fmtDue(iso) {
      if (!iso) return '';
      const d = new Date(iso);
      const s = d.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' });
      return d < new Date() ? s + ' · 已过期' : s;
    }
    async function api(method, url, body) {
      // credentials:'same-origin' —— 同源请求自动携带会话 cookie（zw_session）做鉴权。
      // 现代浏览器 fetch 同源默认即 same-origin，这里显式声明，语义清晰且防默认值变动。
      const opt = { method, credentials: 'same-origin' };
      if (body instanceof FormData) {
        // FormData（录入说话人上传语音样本）：不手动设 Content-Type，
        // 交给浏览器自动带上含 boundary 的 multipart/form-data 头，否则后端解析不到文件。
        opt.body = body;
      } else if (body !== undefined) {
        opt.headers = { 'Content-Type': 'application/json' };
        opt.body = JSON.stringify(body);
      }
      const r = await fetch(url, opt);
      // 401 拦截：未登录或会话过期 → 踢回登录页（authed=false），并抛错中断调用链，
      // 避免调用方拿空数据继续渲染。这样任何 /api/* 在 session 失效后都会自动回到登录门。
      if (r.status === 401) {
        authed.value = false;
        throw new Error('未登录或会话已过期');
      }
      const text = await r.text();
      if (!r.ok) {
        // 后端错误体有两种形态，都要透传给用户，不能一律吞成通用「请求失败」：
        //   1) Go 的 http.Error 直接回「纯文本」错误串（如「display_name 不能为空」、
        //      「该声纹已绑定人物「张三」」）——它不是 JSON，JSON.parse 会抛异常。
        //   2) 业务 JSON API 回 {"error":"..."} 结构，取其 .error 字段。
        // 策略：先按 JSON 解析取 .error；解析失败（形态 1）则回退用响应纯文本，
        //   trim 去首尾空白后 slice(0,200) 截断，防后端偶发返回超长 HTML 错误页刷屏。
        //   两种都拿不到内容时，才回落到「请求失败 + 状态码」这一最后兜底。
        let msg = '';
        try { msg = (text ? JSON.parse(text).error : '') || ''; } catch (e) {}
        // 纯文本兜底：疑似 HTML（反代 502/504 错误页等，以 '<' 开头）不透传原始标签，
        // 直接走最后兜底——本栈后端错误全是纯文本中文，命中 '<' 开头必非业务错误。
        if (!msg && text && text.trim()[0] !== '<') { msg = text.trim().slice(0, 200); }
        throw new Error(msg || '请求失败 ' + r.status);
      }
      // 部分写操作（关联/删除 topic）返回 200/204 空体，r.json() 会抛
      // "Unexpected end of JSON input"——空体直接返回 null，调用方按需判断。
      return text ? JSON.parse(text) : null;
    }
    function showError(e) {
      notify((e && e.message) || String(e),3000);
    }

    // ---------- 登录门（cookie + session 鉴权）逻辑 ----------
    // 首屏加载主界面数据（与原 mount 时的一次性加载对齐）：sessions + topics（「+关联」下拉）+ speakers（换人下拉）。
    function bootMainData() {
      loadSessions();
      loadTopics();       // 首屏 timeline 的「+ 关联」topic 下拉依赖 topics.value（评审 M1）
      loadAllSpeakers();  // 换人下拉（转写段 <select>）数据源，首屏 timeline 即可用
      // 恢复上次所在 tab 与问知微会话（刷新后回到现场）：先置 agentConvId 再 switchTab，
      // 让 switchTab('agent') 分支能重拉历史 + 重连 WS。
      try {
        const savedConv = localStorage.getItem(LS_AGENT_CONV);
        if (savedConv) agentConvId.value = savedConv;
        const savedTab = localStorage.getItem(LS_TAB);
        if (savedTab && savedTab !== tab.value) switchTab(savedTab);
      } catch (e) {}
    }
    // 启动/登录成功后调用：GET /api/auth/me 判断登录态。
    // 200 → 记住当前用户、authed=true、加载主界面首屏数据；401 → api() 已置 authed=false（登录页）。
    async function checkAuth() {
      try {
        const d = await api('GET', '/api/auth/me');
        currentUser.value = (d && d.user) || null;
        authed.value = true;
        bootMainData();
      } catch (e) {
        authed.value = false; // 401（api 已置）或网络错误：统一停在登录页，用户可重试登录
      }
    }
    // 登录表单提交：POST /api/auth/login → 成功则重新走「me→主界面」加载流程；失败提示凭据错误。
    async function submitLogin() {
      if (loggingIn.value) return; // 防重复提交
      const username = loginForm.username.trim();
      const password = loginForm.password;
      if (!username || !password) { loginError.value = '请输入用户名和密码'; return; }
      loggingIn.value = true; loginError.value = '';
      try {
        await api('POST', '/api/auth/login', { username, password });
        loginForm.password = '';  // 不在内存里留存明文密码
        await checkAuth();        // 成功 → 重新走 me→主界面 流程（Set-Cookie 已由浏览器保存）
      } catch (e) {
        loginError.value = '用户名或密码错误';
      } finally {
        loggingIn.value = false;
      }
    }
    // 登出：POST /api/auth/logout（清 cookie）→ 无论成败都回登录页；顺带断开可能存在的问知微 WS。
    async function logout() {
      try { await api('POST', '/api/auth/logout'); }
      catch (e) { /* 登出失败也回登录页：本地态清掉即可，后端 cookie 到期自然失效 */ }
      closeAgentWS();           // 断开后台常驻的问知微 WS（函数声明，已提升）
      currentUser.value = null;
      authed.value = false;
    }

    function typeMeta(t) { return TYPE_META[t] || { label: t, color: '#6b7280' }; }
    // ---------- profile 平面变更（转写详情 timeline）----------
    function profilePlaneMeta(kind) { return PROFILE_PLANE_META[kind] || { label: kind, icon: '•', color: '#6b7280' }; }
    // 变更动作归一：change_type+note → {label, color}。新增(note空)/更新(含「合并更新」)/佐证(reaffirm)/待确认(含 conflict/待人工确认)。
    function profileChangeAction(log) {
      const note = log.note || '';
      if (log.change_type === 'reaffirm' || note.includes('佐证')) return { label: '佐证', color: '#059669' };
      if (note.includes('合并更新')) return { label: '更新', color: '#d97706' };
      if (note.includes('conflict') || note.includes('待人工确认')) return { label: '待确认', color: '#dc2626' };
      return { label: '新增', color: '#6366f1' };
    }
    // 实体摘要：new_value 是带引号的 JSON 字符串，parse 去引号；失败原样显示。
    function fmtChangeSummary(log) {
      if (!log.new_value) return profilePlaneMeta(log.entity_kind).label + '变更';
      try { return JSON.parse(log.new_value); } catch (e) { return log.new_value; }
    }
    // 按 entity_kind 分组（组内后端已按 id 正序）。
    const profileChangeGroups = computed(() => {
      const g = {};
      for (const log of (detail.value && detail.value.profile_changes) || []) {
        (g[log.entity_kind] = g[log.entity_kind] || []).push(log);
      }
      return g;
    });
    // 跳转「人物」tab 的确认队列处理待确认项（确认队列在 persons tab，见 index.html 确认队列区）。
    function goProfilePending() { switchTab('persons'); }
    function statusText(status, stage) {
      if (status === 'done' || status === 'completed') return '已完成';
      if (status === 'failed') return '失败';
      // 处理中标注当前阶段（中文）：pipeline 各 stage 的用户可读名
      const stageNames = {
        asr: '语音转写', segment: '全文汇总', speaker: '声纹识别',
        speakername: '名字推断', extract: '记忆抽取',
      };
      if (status === 'running') return '处理中 · ' + (stageNames[stage] || stage || '');
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
        const r = await fetch('/api/audio', { method: 'POST', body: fd, credentials: 'same-origin' });
        const d = await r.json();
        if (!r.ok) throw new Error(d.error || '上传失败');
        uploadInfo.value = { filename: file.name, status: 'running', text: '已上传，处理中…' };
        // 记录上次轮询到的 job.stage：阶段每推进一步就刷新一次时间线列表。
        // 这样 ASR 落库后（stage 由 asr → segment）卡片标题 asr_preview 立即可见，
        // 不用等整条流水线（说话人/提取/画像…）跑完；badge 的阶段文案也同步更新。
        let lastStage = '';
        pollTimer = setInterval(async () => {
          try {
            const rr = await fetch('/api/sessions/' + d.session_id, { credentials: 'same-origin' });
            const dd = await rr.json();
            const job = dd.job;
            const st = job ? job.status : dd.session.status;
            const stage = job ? (job.stage || '') : '';
            if (stage && stage !== lastStage) { lastStage = stage; loadSessions(); }
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
    // 主题详情页的统一刷新：重开当前详情 + 刷主题列表（记忆/待办增删或关联变化会影响列表计数）。
    // 供主题页里记忆忽略、待办编辑/删除、关联变更等操作的 reload 回调复用。
    async function reloadTopicDetail() {
      if (topicDetail.value && topicDetail.value.topic) await openTopic(topicDetail.value.topic.id);
      await loadTopics();
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
        notify('请输入主题名称', 2000);
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
        notify('主题已创建', 2000);
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
          notify('暂无需要整理的记忆',3000);
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
          notify('暂无需要合并的主题',3000);
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
      if (ids.length < 2) { notify('至少选 2 个主题'); return; }
      const name = manualMergeName.value.trim();
      if (!name) { notify('请输入规范名'); return; }
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
    // 搜索框关键词（客户端过滤标题，与记忆 tab 同模式）；三个分组共用一个搜索词。
    const todoSearch = ref('');
    const todoMatch = t => {
      const q = todoSearch.value.trim().toLowerCase();
      return !q || (t.title || '').toLowerCase().includes(q);
    };
    const suggestedTodos = computed(() => todos.value.filter(t => t.status === 'suggested' && todoMatch(t)));
    const activeTodos = computed(() => todos.value.filter(t => t.status === 'confirmed' && todoMatch(t)));
    const doneTodos = computed(() => todos.value.filter(t => t.status === 'done' && todoMatch(t)));

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
      try { await api('POST', '/api/todos/' + t.id + '/topics', { topic_id: topicId }); await reloadAfterTodoTopic(); }
      catch (e) { showError(e); }
    }
    async function removeTodoTopic(t, topicId) {
      try { await api('DELETE', '/api/todos/' + t.id + '/topics/' + topicId); await reloadAfterTodoTopic(); }
      catch (e) { showError(e); }
    }
    // 关联变更后按当前视图刷新：待办 tab 刷 todos，主题详情页重开当前主题
    // （原先硬编码 loadTodos()——在主题页改关联时详情列表不刷新）。
    async function reloadAfterTodoTopic() {
      if (tab.value === 'topics' && topicDetail.value) await reloadTopicDetail();
      else await loadTodos();
    }
    // 关联变更后按当前视图刷新：记忆 tab 刷 memories，时间线详情刷 session，
    // 主题详情页重开当前主题（原先硬编码 reloadSession——在主题页改关联时详情列表不刷新）。
    async function reloadAfterMemoryTopic() {
      if (tab.value === 'memories') await loadMemories();
      else if (tab.value === 'topics' && topicDetail.value) await reloadTopicDetail();
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

    // ---------- 待办批量操作（待确认/进行中/已完成 分组各自勾选、统一处理） ----------
    // todoSel：勾选中的 todo id 集合（跨组共用，批量按钮只作用于本组勾选项）；
    // todoBatchAsk：破坏性批量操作（忽略/删除）的 2 步确认态 {act, group}，与单条操作的行内确认同风格。
    // todoMultiMode：多选模式开关。默认关闭——此时不显示行级勾选框、分组「全选」与批量条，
    //   待办标题自然左对齐；点击工具栏「多选」进入后，三类勾选控件与批量条才出现。
    const todoSel = reactive(new Set());
    const todoBatchAsk = ref(null);
    const todoMultiMode = ref(false);
    function toggleTodoSel(id) { todoSel.has(id) ? todoSel.delete(id) : todoSel.add(id); }
    // 组内已勾选条目
    function todoGroupSel(list) { return list.filter(td => todoSel.has(td.id)); }
    // 组内全选/全不选切换（已全选 → 清空；否则全选）
    function toggleTodoGroupSel(list) {
      if (list.length && todoGroupSel(list).length === list.length) list.forEach(td => todoSel.delete(td.id));
      else list.forEach(td => todoSel.add(td.id));
    }
    // 进入/退出多选模式：退出时清空勾选与批量确认态，避免残留批量条
    function toggleTodoMultiMode() {
      todoMultiMode.value = !todoMultiMode.value;
      if (!todoMultiMode.value) { todoSel.clear(); todoBatchAsk.value = null; }
    }
    // 批量改状态：逐条 PATCH（单用户量级足够；状态机已放行 suggested→done 与 done→confirmed）。
    // 失败逐条计数（不刷屏），最后统一刷新列表并清掉已处理项的勾选。
    async function batchTodoStatus(items, status) {
      todoBatchAsk.value = null;
      let failed = 0;
      await Promise.all(items.map(td =>
        api('PATCH', '/api/todos/' + td.id, { status }).catch(() => { failed++; })));
      items.forEach(td => todoSel.delete(td.id));
      await reloadAllTodos();
      notify(failed ? `批量操作完成，${failed} 条失败` : '批量操作完成');
    }
    // 批量删除：逐条 DELETE（幂等，404 视为已删不失败）
    async function batchTodoDelete(items) {
      todoBatchAsk.value = null;
      let failed = 0;
      await Promise.all(items.map(td =>
        api('DELETE', '/api/todos/' + td.id).catch(() => { failed++; })));
      items.forEach(td => todoSel.delete(td.id));
      await reloadAllTodos();
      notify(failed ? `批量删除完成，${failed} 条失败` : '批量删除完成');
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

    // 详情区是否有「提取的记忆 / 待办 / 整理草稿」——决定转写与洞察是否分双栏：
    // 有洞察时（宽屏）右侧栏展示记忆/待办卡片，转写列表在左；无则单栏全宽给转写。
    const detailInsights = computed(() => {
      const d = detail.value;
      return !!(d && ((d.memories && d.memories.length) ||
                      (d.todos && d.todos.length) || memoryDraft.value));
    });

    // ---------- ASR 转写就地编辑 ----------
    // segDraft = { [segId]: text }：键存在即该段处于编辑态（点击进入、保存/取消清键）。
    const segDraft = ref({});
    function segEditing(sg) { return !!(sg && sg.id !== undefined && segDraft.value[sg.id] !== undefined); }
    function startEditSeg(sg) { segDraft.value[sg.id] = sg.text; }
    function cancelEditSeg(sg) { delete segDraft.value[sg.id]; }
    // 单段保存：只提交该段草稿，其余段的草稿保持编辑态互不影响
    // （顶部「保存转写修改」是批量路径，本函数是段编辑框旁边的逐段路径）。
    async function saveSegEdit(s, sg) {
      const text = segDraft.value[sg.id];
      if (text === undefined) return;
      try {
        await api('PATCH', '/api/sessions/' + s.id + '/transcript', { segments: [{ id: sg.id, text }] });
        delete segDraft.value[sg.id];
        await reloadSession(s.id);
        notify('转写已保存', 2000);
      } catch (e) { showError(e); }
    }
    const segDirty = computed(() => Object.keys(segDraft.value).length > 0);
    // 原始 ASR 视图开关：true 时转写段以只读方式展示 ASR 原始 spk 标签 + 毫秒时间戳 + 文本，
    // 便于排查「同人被拆成 spk0/spk1」类 diarization 问题（跨录音同人靠声纹 1:N 识别归并）。
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
        notify('已合并 ' + ids.length + ' 段为一条', 2000);
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
        notify('已从该段录入说话人', 2000);
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
        notify('转写已保存', 2000);
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
        notify('已录入说话人', 2000);
      } catch (e) { showError(e); }
      finally { enrolling.value = false; }
    }
    // ---------- 声纹名册的人物关联管理（关联/换绑/解绑/新建并关联） ----------
    // 编辑态 { speakerId, personId, boundPersonId }：personId ''=解绑（须已有绑定）、
    // '__new__'=新建人物并关联（人物名取当前声纹名）、其余=换绑到该人物。
    const spPersonEdit = ref(null);
    const spPersonSaving = ref(false);  // 防重复提交
    // 可关联的人物：名册里未绑声纹的（已绑的走解绑后才能换人；后端唯一键兜底）。
    const unboundPersons = computed(() => persons.value.filter(p => !p.speaker_id));
    function startSpPersonEdit(sp) {
      loadPersons(); // 声纹 tab 开着时人物 tab 可能没拉过名册，懒加载（已加载则缓存跳过）
      spPersonEdit.value = { speakerId: sp.id, personId: sp.person_id || '', boundPersonId: sp.person_id || '' };
    }
    async function commitSpPersonEdit() {
      const d = spPersonEdit.value;
      if (!d || spPersonSaving.value) return;
      spPersonSaving.value = true;
      spPersonEdit.value = null;
      try {
        if (d.personId === '__new__') {
          // 新建人物（暂不绑声纹）→ 经声纹侧转移端点绑上：两步而非一步，是因 POST /api/persons
          // 对「已被人占用的声纹」回 409（占用保护），转移语义只在 speakers/{id}/person 上有。
          const sp = allSpeakers.value.find(s => s.id === d.speakerId);
          const np = await api('POST', '/api/persons', { display_name: sp ? sp.name : '未命名' });
          await api('PATCH', '/api/speakers/' + d.speakerId + '/person', { person_id: np.id });
        } else {
          // ''=解绑 / 其余=转移到该人物：后端单事务清原持有人+绑目标+声纹名同步为人物名
          await api('PATCH', '/api/speakers/' + d.speakerId + '/person', { person_id: d.personId });
        }
        await loadAllSpeakers(); // 名册刷新：sp.name 已同步为人物名、person_id/person_name 更新
        await loadPersons();     // 人物名册的占用关系同步
        notify('人物关联已更新', 2000);
      } catch (e) { showError(e); } // 409（目标人物已被并发绑定其他声纹）等纯文本透传
      finally { spPersonSaving.value = false; }
    }

    // 改名：点 chip/名字的 ✎ 进入改名行；保存 PATCH /api/speakers/{id}，成功后重拉详情（面板名同步）+ 全量列表。
    // 说话人对象有两种来源：时间线面板的 detail.speakers 用 speaker_id 字段；声纹 tab 的 allSpeakers 用 id 字段。
    // 用 `sp.speaker_id || sp.id` 兼容两者（两者都是同一个 speaker 主键，只是响应结构里的字段名不同）。
    function startRenameSpeaker(sp) { renamingSpeaker.value = { id: sp.speaker_id || sp.id, name: sp.name }; }

    // ---------- 多条声纹样本（一个人可录多条；合并说话人时累加不覆盖） ----------
    const addEmbNotes = reactive({});       // speakerId → 追加备注草稿（可空）
    const addEmbTargetId = ref(null);       // 点「＋ 追加声纹」的目标说话人
    const addingEmbId = ref(null);          // 正在上传追加样本的说话人（按钮显 loading）
    const editingEmbNote = ref(null);       // { id, note }：正在改备注的样本
    const deletingEmbId = ref(null);        // 待确认删除的样本 id
    const embFileInput = ref(null);         // 隐藏的文件选择器（共用，点按目标唤起）
    function embSourceText(s) {
      return { manual: '手动录', auto: '自动登记', merge: '合并迁入' }[s] || s;
    }
    // 追加声纹：点按钮 → 记目标说话人 → 唤起文件选择；选完即上传（multipart file+note）。
    // 后端聚合重算代表向量（全部样本均值）并更新 FAISS，成功后重拉名册刷新样本列表。
    function triggerAddEmb(sp) {
      if (addingEmbId.value) return; // 上传中防重复
      addEmbTargetId.value = sp.id;
      if (embFileInput.value) { embFileInput.value.value = ''; embFileInput.value.click(); }
    }
    async function onAddEmbFile(ev) {
      const file = ev.target.files && ev.target.files[0];
      const spId = addEmbTargetId.value;
      if (!file || !spId) return;
      addingEmbId.value = spId;
      const fd = new FormData();
      fd.append('file', file);
      const note = (addEmbNotes[spId] || '').trim();
      if (note) fd.append('note', note);
      try {
        await api('POST', '/api/speakers/' + spId + '/embeddings', fd);
        addEmbNotes[spId] = '';
        await loadAllSpeakers();
        notify('声纹已追加，代表向量已重算', 2000);
      } catch (e) { showError(e); }
      finally { addingEmbId.value = null; }
    }
    function startEditEmbNote(e) { editingEmbNote.value = { id: e.id, note: e.note || '' }; }
    async function commitEmbNote(sp, e) {
      const ed = editingEmbNote.value;
      editingEmbNote.value = null;
      if (!ed || !sp || !e) return;
      try {
        await api('PATCH', '/api/speakers/' + sp.id + '/embeddings/' + e.id, { note: ed.note });
        await loadAllSpeakers();
      } catch (err) { showError(err); }
    }
    async function confirmDeleteEmb(sp, e) {
      deletingEmbId.value = null;
      if (!sp || !e) return;
      try {
        await api('DELETE', '/api/speakers/' + sp.id + '/embeddings/' + e.id);
        await loadAllSpeakers();
        notify('已删除该条声纹，代表向量已重算', 2000);
      } catch (err) { showError(err); }
    }

    // ---------- 切换声纹（识别错时的一键批量改判） ----------
    // 把本会话内某说话人的全部段一键改判给目标声纹（后端按 transcript 作用域批量 UPDATE，
    // 单语句原子写）。只改段归属，不动名册/声纹——错误登记的说话人可另行删除或手动合并。
    const switchingSpeaker = ref(null); // { id, name }：待切换的源说话人
    const switchTarget = ref(null);     // 目标声纹 speaker id
    // 待切换段数 = 本会话转写里归属源说话人的段数（行内提示 + 用户确认依据）
    const switchSegCount = computed(() => {
      if (!switchingSpeaker.value || !detail.value) return 0;
      return (detail.value.segments || []).filter(sg => sg.speaker_id === switchingSpeaker.value.id).length;
    });
    function startSwitchSpeaker(sp) {
      switchingSpeaker.value = { id: sp.speaker_id || sp.id, name: sp.name };
      switchTarget.value = null;
    }
    function cancelSwitchSpeaker() { switchingSpeaker.value = null; switchTarget.value = null; }
    async function commitSwitchSpeaker() {
      if (!switchingSpeaker.value || !switchTarget.value || !detail.value.session) return;
      try {
        await api('POST', '/api/sessions/' + detail.value.session.id + '/speakers/reassign',
          { from_speaker_id: switchingSpeaker.value.id, to_speaker_id: switchTarget.value });
        cancelSwitchSpeaker();
        await reloadSession(detail.value.session.id); // 刷新段归属与说话人面板
        await loadAllSpeakers();                      // 下拉数据源同步（保险）
      } catch (e) { showError(e); }
    }

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
    // 新建人物「关联声纹」下拉的数据源 + 懒加载缓存标志（详见 loadNewPersonSpeakers）。
    const newPersonSpeakers = ref([]);          // GET /api/speakers 的说话人列表（{id,name,...}）
    const newPersonSpeakersLoaded = ref(false); // 简单缓存：已加载则跳过重复拉取（新建表单期间名册少变化）
    const creatingPerson = ref(false);  // 防重复提交
    const renamingPerson = ref(null);   // { id, name } 详情改名（Task 2 用）

    async function loadPersons() {
      try {
        const d = await api('GET', '/api/persons');
        persons.value = d.persons || [];
      } catch (e) { showError(e); }
    }
    // 新建人物「关联声纹」下拉的数据源：GET /api/speakers（响应结构同 loadAllSpeakers，
    // {speakers:[{id,name,...}]}）。带 newPersonSpeakersLoaded 简单缓存——已加载则跳过
    // （新建表单期间名册极少变化，无需每次重拉）。失败走 showError 提示但不阻塞表单：
    // 下拉此时为空，用户仍可「不关联声纹」留空提交，不影响建档主流程。
    async function loadNewPersonSpeakers() {
      if (newPersonSpeakersLoaded.value) return; // 已加载：跳过（简单缓存标志）
      try {
        const d = await api('GET', '/api/speakers');
        newPersonSpeakers.value = d.speakers || [];
        newPersonSpeakersLoaded.value = true;
      } catch (e) { showError(e); } // 仅提示不阻塞：下拉空 → 只能留空，但仍可提交
    }
    function cancelNewPerson() {
      showNewPerson.value = false;
      newPerson.value = { display_name: '', speaker_id: '', summary: '' };
      creatingPerson.value = false;
      // 注意：不清 newPersonSpeakers/newPersonSpeakersLoaded——纯只读列表，缓存无泄漏风险，
      // 保留可让下次打开表单即时显示下拉（免二次拉取）。
    }
    // 工具栏「＋ 新建/收起」切换：收起时走 cancelNewPerson 一并清草稿（对齐 toggleNewTopic，
    // 避免 inline `showNewPerson = !showNewPerson` 收起不重置、重开残留旧输入的不对称）。
    // 打开时懒加载声纹下拉数据源（不 await：表单立即展开，下拉数据返回后再填充，不卡展开）。
    function toggleNewPerson() {
      if (showNewPerson.value) { cancelNewPerson(); return; }
      showNewPerson.value = true;
      loadNewPersonSpeakers();
    }
    async function createPerson() {
      if (creatingPerson.value) return;
      const name = newPerson.value.display_name.trim();
      if (!name) { notify('请输入姓名', 2000); return; }
      creatingPerson.value = true;
      try {
        // speaker_id 可空（只被提到、没录音的人也能建档）；后端校验声纹冲突返回 409。
        // 现在 speaker_id 来自下拉 <select>（值=声纹 id 或空串「不关联」），无首尾空格，
        // 故 .trim() 纯为兼容旧自由输入、此处无害。选到已被占用的声纹提交时，后端回 409
        // 纯文本「该声纹已绑定人物「张三」」——经 Task 1 的 api() 纯文本透传，showError 能显示原文。
        const body = { display_name: name };
        if (newPerson.value.speaker_id.trim()) body.speaker_id = newPerson.value.speaker_id.trim();
        if (newPerson.value.summary.trim()) body.summary = newPerson.value.summary.trim();
        await api('POST', '/api/persons', body);
        cancelNewPerson();
        await loadPersons();
        notify('人物已创建', 2000);
      } catch (e) { showError(e); }
      finally { creatingPerson.value = false; }
    }
    // 点名册卡片：已展开收起；否则拉详情（Task 2 渲染详情卡；本任务先实现数据拉取与切换）
    async function togglePerson(id) {
      if (personDetail.value && personDetail.value.person.id === id) { closePersonDetail(); return; }
      closePersonDetail(); // 切换前清旧人物的临时态（历史抽屉/加属性草稿/编辑删除态），防跨人泄漏
      try {
        personDetail.value = await api('GET', '/api/persons/' + id);
        // 指标(metric)已内嵌在人物详情响应里(personDetail.metrics 分组)，无需单独拉取；此处补拉活动时间线。
        await loadActivities(); // 同步加载「生活轨迹」活动时间线（对齐 loadPending 的挂接方式）
        await loadPets(); // 人物详情的「宠物」分区（对齐 loadActivities 的挂接方式）
      }
      catch (e) { showError(e); }
    }
    // 从 timeline「涉及的画像变更」跳人物详情：先切 persons tab（其分支会 closePersonDetail+
    // loadPersons，名册数据就绪），再展开该人物详情。detail 抽屉保持打开——返回 timeline 时
    // expandedId 仍在，用户可继续看原录音（切换 tab 不清 detail，符合既有行为）。
    async function jumpToPerson(id) {
      switchTab('persons');
      await loadPersons();
      await togglePerson(id);
    }
    function closePersonDetail() {
      personDetail.value = null;
      renamingPerson.value = null;
      bindingSpeaker.value = null; // 声纹换绑编辑态一并清（对齐 renamingPerson）
      deletingPersonId.value = null; // 切换详情/收起时一并清删除确认态（对齐 toggleSession 折叠清 deletingSessionId）
      // 属性手动管理临时态（Task 3）：加属性表单(含草稿) / 就地改值 / 删除确认 / 历史抽屉一并清空
      editingAttr.value = null; deletingAttrId.value = null; attrHistory.value = null;
      showAddAttr.value = false; addAttrForm.attr_key = ''; addAttrForm.value = '';
      // 关系手动管理临时态（Task 4）：加关系表单(含草稿) / 删除确认一并清空
      showAddRel.value = false; resetAddRelForm(); deletingRelId.value = null;
      // 大事记临时态（P2b Task 1）：加事件表单(含草稿) / 删除确认一并清空
      showAddEvent.value = false; resetAddEventForm(); deletingEventId.value = null;
      // 指标临时态（P2 metric）：加指标表单(含草稿) / 删除确认一并清空
      showAddMetric.value = false; resetAddMetricForm(); deletingMetricId.value = null;
      // 健康周期临时态（P3b，敏感区）：折叠+清列表/免责文案 / 加周期表单(含草稿) / 删除确认一并清空
      healthOpen.value = false; cycles.value = []; cyclesNote.value = ''; showAddCycle.value = false; resetAddCycleForm(); deletingCycleId.value = null;
      // 生活轨迹临时态（P4）：清列表/加载态 / 加活动表单(含草稿) / 提交态 / 删除确认一并清空
      activities.value = []; activityLoading.value = false; showAddActivity.value = false; resetAddActivityForm(); addingActivity.value = false; deletingActivityId.value = null;
      // 宠物临时态（pet 平面）：清列表/加载态 / 加编辑表单(含草稿+回填 id) / 保存态 / 删除确认一并清空
      pets.value = []; petLoading.value = false; showAddPet.value = false; resetAddPetForm(); savingPet.value = false; deletingPetId.value = null;
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
    // 声纹名册「👤 人物」跳转（模式照 jumpToSession）：切到人物 tab 并强制展开目标详情。
    // switchTab('persons') 已做 closePersonDetail + loadPersons（重拉名册，绑定态最新），
    // 这里随后直接拉详情——不调 togglePerson，避免「已展开则收起」的切换语义吃掉跳转。
    async function jumpToPerson(pid) {
      switchTab('persons');
      try {
        personDetail.value = await api('GET', '/api/persons/' + pid);
        await loadActivities(); // 人物详情的「生活轨迹」活动时间线（对齐 togglePerson）
        await loadPets(); // 人物详情的「宠物」分区（对齐 loadActivities 的挂接方式）
      } catch (e) { showError(e); }
    }

    // ---------- 声纹关联（详情头部就地换绑/解绑，模式对齐 renamingPerson 就地改名） ----------
    // 编辑态草稿 { speaker_id }：''=解绑；非空=换绑到该声纹。null=非编辑态。
    const bindingSpeaker = ref(null);
    const bindingSaving = ref(false);   // 防重复提交
    // 可绑声纹列表：GET /api/speakers 全量 − 已被「其他人物」占用的（占用关系来自名册缓存
    // persons[].speaker_id；本人已绑的保留展示）。冲突兜底在后端——并发抢占时 PATCH 回 409
    // 纯文本「该声纹已绑定人物「XX」」，经 api() 透传 showError 显示原文。
    const bindableSpeakers = computed(() => {
      if (!personDetail.value) return [];
      const mySid = personDetail.value.person.speaker_id || '';
      const taken = new Set(persons.value
        .filter(x => x.speaker_id && x.id !== personDetail.value.person.id)
        .map(x => x.speaker_id));
      return newPersonSpeakers.value
        .filter(s => s.status !== 'dismissed')
        .filter(s => !taken.has(s.id) || s.id === mySid);
    });
    function startBindingSpeaker() {
      // 数据源复用新建弹窗的懒加载缓存（loadNewPersonSpeakers 已加载则直接命中）；
      // 未加载则现拉，失败仅 showError 不阻塞——下拉空时仍可「解绑」提交。
      loadNewPersonSpeakers();
      bindingSpeaker.value = { speaker_id: personDetail.value.person.speaker_id || '' };
    }
    async function commitBindingSpeaker() {
      const draft = bindingSpeaker.value;
      if (!draft || bindingSaving.value) return;
      bindingSaving.value = true;
      bindingSpeaker.value = null; // 先收编辑态：成功即完成；失败 showError（草稿丢弃，
      // 与 commitRenamePerson 的「失败恢复」不同——select 无自由输入，重选成本低）
      try {
        // 后端三态语义：speaker_id 必传——''=解绑，非空=换绑（person.go Patch）。
        await api('PATCH', '/api/persons/' + personDetail.value.person.id, { speaker_id: draft.speaker_id });
        await reloadPersonDetail(); // 刷新 speaker_name 徽标
        await loadPersons();        // 名册占用关系同步（供其他人物详情下拉过滤）
        notify('声纹关联已更新', 2000);
      } catch (e) { showError(e); }
      finally { bindingSaving.value = false; }
    }

    // 人物删除（原「归档」，2 步确认；DELETE = status=dismissed 软删，六平面级联 dismiss——
    // 行 dismiss 前状态记 pre_dismiss_status，恢复时可级联回滚，手动删过的行不受影响）
    const deletingPersonId = ref(null);
    function askDeletePerson(p) { deletingPersonId.value = p.id; }
    function cancelDeletePerson() { deletingPersonId.value = null; }
    async function confirmDeletePerson(p) {
      try {
        await api('DELETE', '/api/persons/' + p.id);
        deletingPersonId.value = null;
        if (personDetail.value && personDetail.value.person.id === p.id) closePersonDetail();
        await loadPersons();
        await loadDeletedPersons(); // 已删除列表同步（该人物移入折叠区）
      } catch (e) { showError(e); }
    }
    // 已删除人物（status=dismissed 软删行）：底部折叠区查看 + 恢复入口（对齐已忽略主题的交互）。
    const deletedPersons = ref([]);
    const deletedCollapsed = ref(true); // 默认收起
    async function loadDeletedPersons() {
      try {
        const d = await api('GET', '/api/persons?dismissed=1');
        deletedPersons.value = d.persons || [];
      } catch (e) { showError(e); }
    }
    // 恢复已删除人物（PATCH status=active）：后端在同一事务级联恢复删除时被连带清掉的
    // 六平面行（手动删过的行保持 dismissed，不会误恢复）
    async function restorePerson(p) {
      try {
        await api('PATCH', '/api/persons/' + p.id, { status: 'active' });
        await loadPersons();
        await loadDeletedPersons();
        notify('人物已恢复', 2000);
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
    // 属性目录（F4 前端配套）：GET /api/profile/catalog → [{key,label,group,value_type,enum_options,cardinality}]。
    // 懒加载缓存，模式照 loadNewPersonSpeakers：人物 tab 首次进入拉一次（静态数据，无需重拉）。
    // 加属性/改值表单据此把值输入按 value_type 切成受控控件（enum→select / bool→是否 / date→日期选择器）——
    // Task 1 写入端上闸后，受控输入保证提交值天然合法（enum 精确命中、bool 只认 true/false、date 可解析），
    // 免去用户手输脏值被 400。目录同时作 attr_key 输入的 datalist 数据源（key + 中文 label 建议）。
    const attrCatalog = ref([]);
    const attrCatalogLoaded = ref(false);
    async function loadAttrCatalog() {
      if (attrCatalogLoaded.value) return; // 已加载：跳过（静态目录，缓存标志）
      try {
        const d = await api('GET', '/api/profile/catalog');
        attrCatalog.value = d.catalog || [];
        attrCatalogLoaded.value = true;
      } catch (e) { showError(e); } // 失败不阻塞：目录空 → 值输入回退全部自由文本、datalist 无建议，仍可手输
    }
    // attrDefOf：按 key 查目录定义；目录外 key（自造）或目录未加载时返回 null → 前端回退自由文本输入。
    function attrDefOf(key) { return attrCatalog.value.find(d => d.key === key) || null; }
    // attrLabel：属性 key → 中文 label（目录含则用其中文 label，目录外自造 key/目录未加载时回退原 key）。
    function attrLabel(key) { const d = attrDefOf(key); return d && d.label ? d.label : key; }

    const showAddAttr = ref(false);          // 加属性表单开合
    const addAttrForm = reactive({ attr_key: '', value: '' });
    const addingAttr = ref(false);
    const editingAttr = ref(null);           // { id, attr_key, value }：就地改值
    const deletingAttrId = ref(null);        // 2 步删除确认
    const attrHistory = ref(null);           // { attr_key, items }：历史抽屉
    const attrHistoryLoading = ref(false);

    // 加属性表单 / 就地改值当前 key 的目录定义（受控输入据其 value_type 切控件）。
    // addAttrForm.attr_key 是 reactive 字段、editingAttr 是 ref，computed 会自动追踪其变化。
    const addAttrDef = computed(() => attrDefOf(addAttrForm.attr_key));
    const editAttrDef = computed(() => (editingAttr.value ? attrDefOf(editingAttr.value.attr_key) : null));
    // 加属性表单 key 变更（datalist 选中/失焦触发 change）时清空已填值：旧值可能不符合新 key 的
    // 值类型（如已填的文本值留在切到 bool/enum 的受控控件里，select 无匹配项却仍在 model 里 → 提交非法值被 400）。
    function onAddAttrKeyChange() { addAttrForm.value = ''; }

    async function submitAddAttr() {
      if (addingAttr.value) return;
      const key = addAttrForm.attr_key.trim(), val = addAttrForm.value.trim();
      if (!key || !val) { notify('key 与值必填', 2000); return; }
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

    // ---------- 关系管理 ----------
    // 关系类型枚举（与后端 ValidRelations 14 项一致）
    const RELATION_TYPES = ['配偶','子女','父母','兄弟姐妹','亲戚','朋友','同事','领导','下属','客户','供应商','合作方','组织','其他'];
    const DIRECTIONS = ['upstream','downstream','peer'];

    const showAddRel = ref(false);
    const addRelForm = reactive({ relation_type: '', related_person_id: '', label: '', direction: '', org_name: '' });
    const addingRel = ref(false);
    const deletingRelId = ref(null);  // 2 步删除确认

    async function submitAddRel() {
      if (addingRel.value) return;
      const rt = addRelForm.relation_type;
      if (!rt) { notify('请选择关系类型', 2000); return; }
      addingRel.value = true;
      try {
        const body = { relation_type: rt };
        if (addRelForm.related_person_id) body.related_person_id = addRelForm.related_person_id;
        if (addRelForm.label.trim()) body.label = addRelForm.label.trim();
        if (addRelForm.direction) body.direction = addRelForm.direction;
        if (addRelForm.org_name.trim()) body.org_name = addRelForm.org_name.trim();
        await api('POST', '/api/persons/' + personDetail.value.person.id + '/relationships', body);
        await reloadPersonDetail();
        // 手动加=active 不产生 pending，无需刷名册（删 pending 关系才需要，见 confirmDeleteRel）
        showAddRel.value = false;
        resetAddRelForm();
      } catch (e) { showError(e); }
      finally { addingRel.value = false; }
    }
    function resetAddRelForm() {
      addRelForm.relation_type = ''; addRelForm.related_person_id = ''; addRelForm.label = ''; addRelForm.direction = ''; addRelForm.org_name = '';
    }
    // 开合切换：收起时清草稿（对齐 toggleAddAttr 的对称清理模式）
    function toggleAddRel() {
      if (showAddRel.value) { showAddRel.value = false; resetAddRelForm(); return; }
      showAddRel.value = true;
    }
    function askDeleteRel(rel) { deletingRelId.value = rel.id; }
    async function confirmDeleteRel() {
      const id = deletingRelId.value;
      if (!id) return;
      try {
        await api('DELETE', '/api/persons/' + personDetail.value.person.id + '/relationships/' + id);
        deletingRelId.value = null;
        await reloadPersonDetail(); await loadPersons(); // 删 pending 关系会改名册 pending 计数，一并刷（对齐 confirmDeleteAttr）
      } catch (e) { showError(e); }
    }

    // ---------- 大事记（event 平面，P2） ----------
    // 事件类型枚举与后端 ValidEventTypes 一致（9 类）
    const EVENT_TYPES = ['里程碑','聚会','会议','旅行','健康','成就','挫折','负面','其他'];

    // 重要度三档（P7）：语义档位（人生分量）比数字输入更贴近用户直觉，映射后端 clamp 到 (0,1] 的值。
    // 默认「一般」(0.5)；手动录入恒发所选档（见 submitAddEvent），与后端「类型默认」路径(LLM/缺省)互不干扰。
    const EVENT_IMPORTANCE_LEVELS = [
      { label: '重大', value: 0.9 },
      { label: '一般', value: 0.5 },
      { label: '日常', value: 0.3 },
    ];
    // 大事记行重要度视觉分层（P7）：不引新色，用「左侧 accent 竖条 + 透明度」映射人生分量三档——
    // 重大(≥0.8) 显示 accent 左竖条(视觉加重)、一般(0.6~0.8) 常规、日常(<0.6) 淡显。所有行恒留 3px
    // 左竖条槽位(默认透明)保持左对齐，只切换颜色不产生位移；纯视觉不暴露数值(数值走 hover title)。
    // 对齐 dataviz「数据是唯一主角」：importance 是权重信号，用视觉重量表达而非再塞一个数字。
    function eventRowStyle(ev) {
      const imp = ev && typeof ev.importance === 'number' ? ev.importance : 0.5;
      const style = {
        paddingLeft: '8px',
        borderLeft: '3px solid ' + (imp >= 0.8 ? 'var(--accent)' : 'transparent'),
      };
      // 淡显阈值 <0.45（非 <0.6）：三档按钮 一般=0.5 须保持常规亮度——若阈值取 0.6 会把
      // 「一般」也淡显，与「日常」(0.3) 渲染无差别（final review Low 修正）。类型默认 0.4
      // （其他）落淡显档、0.5/0.8 常规、0.9 重亮——各档可辨。
      if (imp < 0.45) style.opacity = '0.55';
      return style;
    }

    const showAddEvent = ref(false);
    const addEventForm = reactive({ event_type: '', title: '', description: '', occurred_at: '', end_at: '', location: '', related_person_ids: [], importance: 0.5 });
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
    // 事件日期展示：occurred_at ISO → 「M月D日」（已按年分组，组内不再重复年）；
    // withYear=true 输出含年「YYYY年M月D日」，供无年上下文的「时间未知」组用；空显「—」。
    function fmtEventDate(iso, withYear) {
      if (!iso) return '—';
      const d = new Date(iso);
      if (isNaN(d.getTime())) return iso;
      const md = `${d.getMonth() + 1}月${d.getDate()}日`;
      return withYear ? `${d.getFullYear()}年${md}` : md;
    }
    function resetAddEventForm() {
      addEventForm.event_type = ''; addEventForm.title = ''; addEventForm.description = '';
      addEventForm.occurred_at = ''; addEventForm.end_at = ''; addEventForm.location = '';
      addEventForm.related_person_ids = []; addEventForm.importance = 0.5;  // 数组与档位对称清理（默认「一般」）
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
      if (!et) { notify('请选择事件类型', 2000); return; }
      if (!title) { notify('请输入标题', 2000); return; }
      addingEvent.value = true;
      try {
        // 日期用 <input type="date"> 的 YYYY-MM-DD；可选字段仅非空才传（后端 parseEventAt 尽力解析）
        const body = { event_type: et, title };
        if (addEventForm.description.trim()) body.description = addEventForm.description.trim();
        if (addEventForm.occurred_at) body.occurred_at = addEventForm.occurred_at;
        if (addEventForm.end_at) body.end_at = addEventForm.end_at;
        if (addEventForm.location.trim()) body.location = addEventForm.location.trim();
        // 同行人物多选（P7）：数组非空才发 related_person_ids（后端 []string，逐个 ParseID）
        if (addEventForm.related_person_ids.length) body.related_person_ids = addEventForm.related_person_ids;
        // 重要度恒发所选档（P7）：手动录入 WYSIWYG——用户所见档位(默认「一般」0.5)即其意图，
        // 恒发避免「看着是一般的里程碑被后端类型默认悄悄提成重大」的意外；后端 clamp 到 (0,1]。
        body.importance = addEventForm.importance;
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

    // ---------- 指标（metric 平面，第 5 平面 person_metric，P2） ----------
    // 前端指标目录：与后端 6 键一致（profile.MetricDefOf / catalog）。numeric=true 的指标可画趋势曲线。
    const METRIC_CATALOG = [
      { key: 'emotion', label: '情绪', unit: '', numeric: true },
      { key: 'weight', label: '体重', unit: 'kg', numeric: true },
      { key: 'sleep', label: '睡眠时长', unit: 'h', numeric: true },
      { key: 'mood_energy', label: '精力', unit: '', numeric: true },
      { key: 'diet', label: '饮食', unit: '', numeric: false },
      { key: 'health', label: '健康', unit: '', numeric: false },
    ];
    // metric_key → 中文标签（确认队列摘要用；目录外键原样返回，不出空标签）
    function metricLabel(key) { const d = METRIC_CATALOG.find(m => m.key === key); return d ? d.label : (key || ''); }

    const showAddMetric = ref(false);
    // value_num 用字符串承载 <input type=number> 的值，提交时按所选指标 numeric 决定是否 Number()。
    const addMetricForm = reactive({ metric_key: '', value_num: '', value_text: '', unit: '', measured_at: '' });
    const addingMetric = ref(false);
    const deletingMetricId = ref(null);  // 2 步删除确认（测点 id）

    // 当前所选指标键的目录定义（决定表单填 value_num 还是 value_text、默认单位）
    const addMetricDef = computed(() => METRIC_CATALOG.find(m => m.key === addMetricForm.metric_key) || null);

    // 各数值型指标组的趋势曲线几何：只对 numeric 组且「≥2 个含 value_num 的测点」预算好坐标
    //（模板只读，不在模板里算）。返回 { [metric_key]: chartGeom 结果 }；1 点或类别组不产出，
    // 模板据此决定是否画线（对齐周报 weeklyCharts 的 chartGeom 用法）。points 已按 measured_at 升序。
    const metricCharts = computed(() => {
      const out = {};
      if (!personDetail.value || !personDetail.value.metrics) return out;
      for (const g of personDetail.value.metrics) {
        if (!g.numeric) continue;
        const pts = (g.points || []).filter(p => p.value_num != null);
        if (pts.length < 2) continue; // 单点不画线，仅列表展示
        out[g.key] = chartGeom(pts.map(p => p.value_num), pts.map(p => p.measured_at));
      }
      return out;
    });
    // 测点值展示串：数值型取 value_num（带单位，如 70kg / 0.8），类别型取 value_text（如 火锅）
    function metricPointValue(g, p) {
      if (p.value_num != null) return fmtNum(p.value_num) + (g && g.unit ? g.unit : '');
      return p.value_text || '';
    }
    function resetAddMetricForm() {
      addMetricForm.metric_key = ''; addMetricForm.value_num = ''; addMetricForm.value_text = '';
      addMetricForm.unit = ''; addMetricForm.measured_at = '';
    }
    // 选指标键后自动带出目录默认单位（用户仍可改），并清掉不适用的那个值输入，避免误传另一类型的值。
    function onPickMetricKey() {
      const d = addMetricDef.value;
      addMetricForm.unit = d ? d.unit : '';
      if (d && d.numeric) addMetricForm.value_text = '';
      else addMetricForm.value_num = '';
    }
    // 开合切换：收起清草稿（对齐 toggleAddEvent/toggleAddAttr 的对称清理模式）
    function toggleAddMetric() {
      if (showAddMetric.value) { showAddMetric.value = false; resetAddMetricForm(); return; }
      showAddMetric.value = true;
    }
    async function submitAddMetric() {
      if (addingMetric.value) return;
      const key = addMetricForm.metric_key;
      const def = addMetricDef.value;
      if (!key || !def) { toast.value = '请选择指标'; setTimeout(() => { toast.value = ''; }, 2000); return; }
      // 数值型必须给 value_num、类别型必须给 value_text（与后端 ManualAddMetric 硬约束一致，前端先拦一道）
      if (def.numeric && String(addMetricForm.value_num).trim() === '') {
        toast.value = '数值型指标需填数值'; setTimeout(() => { toast.value = ''; }, 2000); return;
      }
      if (!def.numeric && !addMetricForm.value_text.trim()) {
        toast.value = '类别型指标需填描述'; setTimeout(() => { toast.value = ''; }, 2000); return;
      }
      addingMetric.value = true;
      try {
        const body = { metric_key: key };
        if (def.numeric) body.value_num = Number(addMetricForm.value_num);
        else body.value_text = addMetricForm.value_text.trim();
        if (addMetricForm.unit.trim()) body.unit = addMetricForm.unit.trim();
        if (addMetricForm.measured_at) body.measured_at = addMetricForm.measured_at; // 留空 → 后端用当前时间
        await api('POST', '/api/persons/' + personDetail.value.person.id + '/metrics', body);
        await reloadPersonDetail();
        showAddMetric.value = false;
        resetAddMetricForm();
      } catch (e) { showError(e); }
      finally { addingMetric.value = false; }
    }
    // 删除测点（2 步确认，DELETE /api/persons/{id}/metrics/{mid}）。测点 p 需含 id 才可删；
    // 详情页 points 目前不下发 id 时删除按钮不渲染（见 index.html 的 v-if="p.id != null"）。
    function askDeleteMetric(p) { deletingMetricId.value = p.id; }
    async function confirmDeleteMetric() {
      const id = deletingMetricId.value;
      if (!id) return;
      try {
        await api('DELETE', '/api/persons/' + personDetail.value.person.id + '/metrics/' + id);
        deletingMetricId.value = null;
        await reloadPersonDetail(); await loadPersons(); // pending 计数可能变化
      } catch (e) { showError(e); }
    }

    // ---------- 健康周期（cycle 平面，P3，敏感：默认折叠 + 免责直显） ----------
    const CYCLE_TYPES = [
      { key: 'menstrual', label: '生理期' },
      { key: 'medication', label: '用药' },
      { key: 'injection', label: '注射' },
      { key: 'followup', label: '随访' },
    ];
    const healthOpen = ref(false);        // 敏感区默认折叠
    const cycles = ref([]);
    const cyclesNote = ref('');           // 后端免责文案（响应 note 字段直显）
    const cycleLoading = ref(false);
    const showAddCycle = ref(false);
    const addCycleForm = reactive({ cycle_type: '', label: '', anchor_date: '', period_days: '', duration_days: '', dosage: '', frequency: '' });
    const addingCycle = ref(false);
    const deletingCycleId = ref(null);

    function cycleTypeLabel(k) { const t = CYCLE_TYPES.find(x => x.key === k); return t ? t.label : k; }
    // 日期仅显示 YYYY-MM-DD（DATE 列的 ISO 串截取）
    function fmtDateOnly(iso) { return iso ? String(iso).slice(0, 10) : '—'; }
    // 取周期数据的唯一入口（toggleHealth/reloadCycles 共用，避免两处内联重复）。
    // 敏感数据：await 前记下当前人物 id，回来时若已切人/已收起则丢弃响应，
    // 防止「A 的在途响应晚于 B 返回」把 A 的生理期/用药渲染到 B 名下。
    async function fetchCyclesInto() {
      const pid = personDetail.value?.person?.id;
      const d = await api('GET', '/api/persons/' + pid + '/cycles');
      if (!healthOpen.value || personDetail.value?.person?.id !== pid) return;
      cycles.value = d.cycles || [];
      cyclesNote.value = d.note || '';
    }
    // 敏感区懒加载：首次展开才拉数据（含免责 note）；再点收起并清列表
    async function toggleHealth() {
      if (healthOpen.value) {
        healthOpen.value = false; cycles.value = []; cyclesNote.value = '';
        showAddCycle.value = false; resetAddCycleForm(); deletingCycleId.value = null;
        return;
      }
      healthOpen.value = true;
      cycleLoading.value = true;
      try { await fetchCyclesInto(); } catch (e) { showError(e); }
      finally { cycleLoading.value = false; }
    }
    async function reloadCycles() {
      if (!healthOpen.value) return;
      try { await fetchCyclesInto(); } catch (e) { showError(e); }
    }
    function resetAddCycleForm() {
      addCycleForm.cycle_type = ''; addCycleForm.label = ''; addCycleForm.anchor_date = '';
      addCycleForm.period_days = ''; addCycleForm.duration_days = ''; addCycleForm.dosage = ''; addCycleForm.frequency = '';
    }
    function toggleAddCycle() {
      if (showAddCycle.value) { showAddCycle.value = false; resetAddCycleForm(); return; }
      showAddCycle.value = true;
    }
    async function submitAddCycle() {
      if (addingCycle.value) return;
      const ct = addCycleForm.cycle_type;
      if (!ct) { notify('请选择周期类型', 2000); return; }
      addingCycle.value = true;
      try {
        const body = { cycle_type: ct };
        if (addCycleForm.label.trim()) body.label = addCycleForm.label.trim();
        if (addCycleForm.anchor_date) body.anchor_date = addCycleForm.anchor_date;
        if (addCycleForm.period_days) body.period_days = Number(addCycleForm.period_days);
        if (addCycleForm.duration_days) body.duration_days = Number(addCycleForm.duration_days);
        if (addCycleForm.dosage.trim()) body.dosage = addCycleForm.dosage.trim();
        if (addCycleForm.frequency.trim()) body.frequency = addCycleForm.frequency.trim();
        await api('POST', '/api/persons/' + personDetail.value.person.id + '/cycles', body);
        showAddCycle.value = false;
        resetAddCycleForm();
        await reloadCycles(); await loadPersons();
      } catch (e) { showError(e); }
      finally { addingCycle.value = false; }
    }
    function askDeleteCycle(c) { deletingCycleId.value = c.id; }
    async function confirmDeleteCycle() {
      const id = deletingCycleId.value;
      if (!id) return;
      try {
        await api('DELETE', '/api/persons/' + personDetail.value.person.id + '/cycles/' + id);
        deletingCycleId.value = null;
        await reloadCycles(); await loadPersons();
      } catch (e) { showError(e); }
    }

    // ---------- 生活轨迹（activity 平面，P4：什么时间 / 多长时间 / 什么工具 / 做什么 / 地点 / 通勤） ----------
    // 测点流语义（同 metric 平面）：追加式、无「当前值」、无冲突——每条 = 某时开始的一次活动。
    // 画成时间线列表（升序，从早到晚）而非曲线：活动是「类别身份」，画连续线是撒谎（dataviz 结论，P3b 已引用）。
    const activities = ref([]);              // 当前人物的全状态活动（后端升序返回）
    const activityLoading = ref(false);
    const showAddActivity = ref(false);
    const addActivityForm = reactive({ activity: '', tool: '', location: '', commute_mode: '', started_at: '', duration_min: '' });
    const addingActivity = ref(false);
    const deletingActivityId = ref(null);    // 2 步删除确认

    async function loadActivities() {
      if (!personDetail.value) return;
      // 记下请求时的人物 id，await 回来后校验——快速切人时晚到的旧响应直接丢弃，
      // 防止过期数据渲染且粘住（await 前记 pid，回来校验人物未变）。
      const pid = personDetail.value.person.id;
      activityLoading.value = true;
      try {
        const d = await api('GET', '/api/persons/' + pid + '/activities');
        if (personDetail.value?.person?.id !== pid) return;
        activities.value = d.activities || [];
      } catch (e) { showError(e); }
      finally { activityLoading.value = false; }
    }
    // 展示视图：排除 dismissed（软删不显示，与详情各平面过滤一致）。
    const activityRows = computed(() => activities.value.filter(a => a.status !== 'dismissed'));
    function resetAddActivityForm() {
      addActivityForm.activity = ''; addActivityForm.tool = ''; addActivityForm.location = '';
      addActivityForm.commute_mode = ''; addActivityForm.started_at = ''; addActivityForm.duration_min = '';
    }
    function toggleAddActivity() {
      if (showAddActivity.value) { showAddActivity.value = false; resetAddActivityForm(); return; }
      showAddActivity.value = true;
    }
    async function submitAddActivity() {
      if (addingActivity.value) return;
      const act = addActivityForm.activity.trim();
      if (!act) { notify('请输入活动', 2000); return; }
      addingActivity.value = true;
      try {
        // 可选字段非空才发（后端 trim 空→NULL）；duration_min 表单是字符串，Number() 转数字。
        const body = { activity: act };
        if (addActivityForm.tool.trim()) body.tool = addActivityForm.tool.trim();
        if (addActivityForm.location.trim()) body.location = addActivityForm.location.trim();
        if (addActivityForm.commute_mode.trim()) body.commute_mode = addActivityForm.commute_mode.trim();
        if (addActivityForm.started_at) body.started_at = addActivityForm.started_at;
        if (addActivityForm.duration_min) body.duration_min = Number(addActivityForm.duration_min);
        // POST 返回裸行不消费，直接 reload（对齐 submitAddMetric）；名册角标可能变故一并刷新。
        await api('POST', '/api/persons/' + personDetail.value.person.id + '/activities', body);
        showAddActivity.value = false;
        resetAddActivityForm();
        await loadActivities(); await loadPersons();
      } catch (e) { showError(e); }
      finally { addingActivity.value = false; }
    }
    function askDeleteActivity(a) { deletingActivityId.value = a.id; }
    async function confirmDeleteActivity() {
      const id = deletingActivityId.value;
      if (!id) return;
      try {
        await api('DELETE', '/api/persons/' + personDetail.value.person.id + '/activities/' + id);
        deletingActivityId.value = null;
        await loadActivities(); await loadPersons();
      } catch (e) { showError(e); }
    }

    // ---------- 宠物（pet 平面：多只；名/小名/类别/品种/性别/年龄/生日/喜好） ----------
    const PET_SPECIES = ['猫', '狗', '鸟', '鱼', '兔', '仓鼠', '爬行', '其他'];
    const pets = ref([]);                 // 当前人物全状态宠物（后端按 id 升序）
    const petLoading = ref(false);
    const showAddPet = ref(false);        // 加/编辑表单开合（编辑复用同一张表单）
    const editingPetId = ref(null);       // 非空=编辑模式（PATCH）；空=新增（POST）
    const addPetForm = reactive({ name: '', nickname: '', species: '猫', breed: '', gender: '', age_text: '', birthday: '', likes: '' });
    const savingPet = ref(false);
    const deletingPetId = ref(null);      // 2 步删除确认

    async function loadPets() {
      if (!personDetail.value) return;
      // 记下请求时的人物 id，await 回来后校验（快速切人丢弃过期响应，对齐 loadActivities）。
      const pid = personDetail.value.person.id;
      petLoading.value = true;
      try {
        const d = await api('GET', '/api/persons/' + pid + '/pets');
        if (personDetail.value?.person?.id !== pid) return;
        pets.value = d.pets || [];
      } catch (e) { showError(e); }
      finally { petLoading.value = false; }
    }
    // 展示视图：排除 dismissed（软删不显示，与详情各平面过滤一致）。
    const petRows = computed(() => pets.value.filter(p => p.status !== 'dismissed'));
    function resetAddPetForm() {
      addPetForm.name = ''; addPetForm.nickname = ''; addPetForm.species = '猫';
      addPetForm.breed = ''; addPetForm.gender = ''; addPetForm.age_text = '';
      addPetForm.birthday = ''; addPetForm.likes = '';
      editingPetId.value = null;
    }
    function toggleAddPet() {
      if (showAddPet.value) { showAddPet.value = false; resetAddPetForm(); return; }
      showAddPet.value = true;
    }
    // 编辑：回填现值后展开同一张表单（PATCH 即整只替换——未提到字段会被清空，回填保证全量）。
    function startEditPet(p) {
      editingPetId.value = p.id;
      addPetForm.name = p.name || '';
      addPetForm.nickname = p.nickname || '';
      addPetForm.species = p.species || '其他';
      addPetForm.breed = p.breed || '';
      addPetForm.gender = p.gender || '';
      addPetForm.age_text = p.age_text || '';
      addPetForm.birthday = p.birthday ? String(p.birthday).slice(0, 10) : '';
      addPetForm.likes = p.likes || '';
      showAddPet.value = true;
    }
    async function submitPet() {
      if (savingPet.value) return;
      if (!addPetForm.name.trim()) { notify('请输入宠物名字', 2000); return; }
      if (!addPetForm.birthday) { notify('生日必填（YYYY-MM-DD）', 2000); return; }
      savingPet.value = true;
      try {
        const pid = personDetail.value.person.id;
        const body = {
          name: addPetForm.name.trim(),
          species: addPetForm.species || '其他',
          birthday: addPetForm.birthday,
        };
        if (addPetForm.nickname.trim()) body.nickname = addPetForm.nickname.trim();
        if (addPetForm.breed.trim()) body.breed = addPetForm.breed.trim();
        if (addPetForm.gender) body.gender = addPetForm.gender;
        if (addPetForm.age_text.trim()) body.age_text = addPetForm.age_text.trim();
        if (addPetForm.likes.trim()) body.likes = addPetForm.likes.trim();
        if (editingPetId.value) {
          await api('PATCH', '/api/persons/' + pid + '/pets/' + editingPetId.value, body);
        } else {
          await api('POST', '/api/persons/' + pid + '/pets', body);
        }
        showAddPet.value = false;
        resetAddPetForm();
        await loadPets(); await loadPersons(); // pending 计数可能变化
      } catch (e) { showError(e); }
      finally { savingPet.value = false; }
    }
    function askDeletePet(p) { deletingPetId.value = p.id; }
    async function confirmDeletePet() {
      const id = deletingPetId.value;
      if (!id) return;
      try {
        await api('DELETE', '/api/persons/' + personDetail.value.person.id + '/pets/' + id);
        deletingPetId.value = null;
        await loadPets(); await loadPersons();
      } catch (e) { showError(e); }
    }

    // ---------- 确认队列（跨平面 pending 并集；与名册/详情独立刷新） ----------
    const pendingItems = ref([]);
    const pendingLoading = ref(false);
    const queueBusyIds = reactive({}); // 正在确认/放弃的条目 id（kind-id 键），防双击重放
    async function loadPending() {
      pendingLoading.value = true;
      try {
        const d = await api('GET', '/api/profile/pending');
        pendingItems.value = d.items || [];
      } catch (e) { showError(e); }
      finally { pendingLoading.value = false; }
    }
    // 确认/放弃后三处联动刷新：队列本身 + 名册（pending 计数）+ 当前详情（若开着）
    async function refreshAfterQueue() {
      await loadPending();
      await loadPersons();
      if (personDetail.value) await reloadPersonDetail();
    }
    async function confirmPendingItem(it) {
      const k = it.kind + '-' + it.id;
      if (queueBusyIds[k]) return; // 防双击重放（确认/放弃互斥共用同一键）
      queueBusyIds[k] = true;
      try {
        await api('POST', '/api/profile/pending/' + it.kind + '/' + it.id + '/confirm');
        await refreshAfterQueue();
      } catch (e) { showError(e); }
      finally { delete queueBusyIds[k]; }
    }
    async function dismissPendingItem(it) {
      const k = it.kind + '-' + it.id;
      if (queueBusyIds[k]) return; // 防双击重放（确认/放弃互斥共用同一键）
      queueBusyIds[k] = true;
      try {
        await api('POST', '/api/profile/pending/' + it.kind + '/' + it.id + '/dismiss');
        await refreshAfterQueue();
      } catch (e) { showError(e); }
      finally { delete queueBusyIds[k]; }
    }
    // 队列条目摘要（kind 不同字段不同：attribute=建议值，relationship=类型+称呼，event=类型+标题，metric=指标+值，person=名字）
    function pendingSummary(it) {
      if (it.kind === 'attribute') return (it.attr_key || '') + '：' + (it.value || '');
      if (it.kind === 'relationship') return (it.relation_type || '') + (it.label ? '（' + it.label + '）' : '');
      if (it.kind === 'event') return (it.event_type || '') + '：' + (it.value || ''); // event：event_type + title（value 后端映射为 title）
      if (it.kind === 'metric') return metricLabel(it.metric_key) + '：' + (it.value || ''); // metric：指标中文名 + 值（value 后端映射为带单位的读数）
      if (it.kind === 'cycle') {
        // 后端 value = type 或 type·label；type 是英文枚举，换成中文（cycleTypeLabel
        // 找不到时回退原 key），label 部分原样保留
        const v = it.value || '';
        return it.cycle_type ? v.replace(it.cycle_type, cycleTypeLabel(it.cycle_type)) : v;
      }
      if (it.kind === 'pet') return (it.value || '') + (it.label ? '（' + it.label + '）' : ''); // pet：value=名字，label=类别·品种·性别·年龄摘要
      if (it.kind === 'activity') return it.value || ''; // activity：value = activity 串（做什么）
      return it.value || it.person_name; // person：名字
    }
    function pendingKindText(k) {
      return { attribute: '属性', relationship: '关系', person: '新人物', event: '大事记', metric: '指标', cycle: '周期', activity: '活动', pet: '宠物' }[k] || k;
    }

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
      if (spMergeSelected.value.length < 2) { notify('至少选 2 个说话人'); return; }
      if (!spMergeTarget.value) { notify('请选择保留的目标说话人'); return; }
      const sources = spMergeSelected.value.filter(id => id !== spMergeTarget.value);
      if (!sources.length) { notify('目标之外还需至少 1 个源'); return; }
      try {
        await api('POST', '/api/speakers/merge', { source_ids: sources, target_id: spMergeTarget.value });
        cancelSpMerge();
        await loadAllSpeakers();
        // 合并影响当前展开会话的说话人/段 → 重拉同步（声纹 tab 无展开会话则 detail 为空，跳过）
        if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
        notify('已合并 ' + sources.length + ' 个说话人到目标', 2000);
      } catch (e) { showError(e); }
    }

    // ---------- 重新提取（基于最新 ASR 重跑 segment→extract） ----------
    // 点卡片「重新提取」→ 2 步确认 → 若有未保存转写先存盘 → POST reextract 建任务
    // → 轮询 job 状态 → 完成后刷新列表+详情。2 步确认提示会覆盖旧记忆/待办。
    const reextractingIds = reactive(new Set()); // 正在重新提取的 session id 集合（支持多卡片并行）
    const reextractConfirmId = ref(null);        // 待确认重新提取的 session id
    const reextractTimers = new Map();           // sessionId → 轮询 timer（并行时各自独立，互不覆盖）
    // jobInProgress 判断会话是否有任务在跑/排队（列表富化的 job_status；处理中禁再
    // 触发重新提取/重新识别——后端也会 409 拒，这里提前提示避免白点一趟确认按钮）。
    function jobInProgress(s) { return s.job_status === 'running' || s.job_status === 'pending'; }
    function askReextract(s) {
      if (jobInProgress(s)) { notify('该录音正在处理中，等当前任务完成后再重新提取（避免重复排队）'); return; }
      deletingSessionId.value = null; reextractConfirmId.value = s.id;
    }
    function cancelReextract() { reextractConfirmId.value = null; }
    async function confirmReextract(s) { reextractConfirmId.value = null; await reextractSession(s); }
    async function reextractSession(s) {
      if (reextractingIds.has(s.id)) return; // 只防**同一** session 重复提交；不同 session 允许并行（后端 pool 并发=2，多的排队）
      // 当前展开且有未保存转写修改 → 先存盘，确保用最新 ASR 提取
      if (expandedId.value === s.id && segDirty.value) {
        await saveTranscript(s);
      }
      reextractingIds.add(s.id);
      try {
        await api('POST', '/api/sessions/' + s.id + '/reextract', {});
        notify('正在重新提取…');
        const poll = async () => {
          let st = '', err = '';
          try {
            const r = await api('GET', '/api/sessions/' + s.id);
            st = r.job ? r.job.status : (r.session && r.session.status) || '';
            err = r.job ? (r.job.last_error || '') : '';
          } catch (e) { /* 轮询失败静默重试 */ }
          if (st === 'done' || st === 'completed') {
            reextractingIds.delete(s.id);
            reextractTimers.delete(s.id);
            notify('重新提取完成', 2500);
            await loadSessions();
            if (expandedId.value === s.id) await reloadSession(s.id);
          } else if (st === 'failed') {
            reextractingIds.delete(s.id);
            reextractTimers.delete(s.id);
            notify('重新提取失败' + (err ? '：' + err : ''),4000);
          } else {
            reextractTimers.set(s.id, setTimeout(poll, 2000));
          }
        };
        poll();
      } catch (e) {
        reextractingIds.delete(s.id);
        showError(e);
      }
    }

    // ---------- 重新识别说话人（清空说话人归属 + 重跑 speaker stage 用最新声纹库 1:N） ----------
    // 区别于重新提取（speaker 幂等跳过、不改已有归属）；重新识别会覆盖手动换人，故二次确认。
    const reidentifyingIds = reactive(new Set()); // 正在重新识别的 session id 集合（支持多卡片并行）
    const reidentifyConfirmId = ref(null);        // 待确认重新识别的 session id
    const reidentifyTimers = new Map();           // sessionId → 轮询 timer（并行时各自独立）
    function askReidentify(s) {
      if (jobInProgress(s)) { notify('该录音正在处理中，等当前任务完成后再重新识别（避免重复排队）'); return; }
      deletingSessionId.value = null; reextractConfirmId.value = null; reidentifyConfirmId.value = s.id;
    }
    function cancelReidentify() { reidentifyConfirmId.value = null; }
    async function confirmReidentify(s) { reidentifyConfirmId.value = null; await reidentifySession(s); }
    async function reidentifySession(s) {
      if (reidentifyingIds.has(s.id)) return; // 只防**同一** session 重复提交；不同 session 允许并行（后端 pool 并发=2，多的排队）
      reidentifyingIds.add(s.id);
      try {
        await api('POST', '/api/sessions/' + s.id + '/reidentify', {});
        notify('正在重新识别说话人…');
        const poll = async () => {
          let st = '', err = '';
          try {
            const r = await api('GET', '/api/sessions/' + s.id);
            st = r.job ? r.job.status : (r.session && r.session.status) || '';
            err = r.job ? (r.job.last_error || '') : '';
          } catch (e) { /* 轮询失败静默重试 */ }
          if (st === 'done' || st === 'completed') {
            reidentifyingIds.delete(s.id);
            reidentifyTimers.delete(s.id);
            notify('重新识别完成', 2500);
            await loadSessions();
            if (expandedId.value === s.id) await reloadSession(s.id);
          } else if (st === 'failed') {
            reidentifyingIds.delete(s.id);
            reidentifyTimers.delete(s.id);
            notify('重新识别失败' + (err ? '：' + err : ''),4000);
          } else {
            reidentifyTimers.set(s.id, setTimeout(poll, 2000));
          }
        };
        poll();
      } catch (e) {
        reidentifyingIds.delete(s.id);
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
    // ---------- 问知微（流式对话 / WS + MCP 工具卡） ----------
    // 后端契约：REST 见 internal/agent/handlers.go；WS 见 internal/agent/ws.go。
    //   会话列表  GET  /api/agent/conversations            → [conversation]（直接数组）
    //   新建会话  POST /api/agent/conversations {title?}    → conversation
    //   会话历史  GET  /api/agent/conversations/{cid}       → {conversation_id, messages:[{id,role,kind,content,tool_payload,created_at}]}
    //   流式会话  WS   /api/agent/conversations/{cid}/ws     上行 {"text":"…"}；下行 StreamFrame
    // StreamFrame.type ∈ user|assistant|tool_call|tool_result|turn_end：
    //   一条用户消息 → user → (assistant | tool_call | tool_result)* → turn_end（turn_end.error 非空=失败）。
    // 工具结果配对：tool_result 帧/历史都【带 call_id】（见 orchestrator.go / event.go），
    //   故优先按 call_id 精确配对到对应 tool_call 卡；仅当无 call_id 或未命中（旧数据/异常）时
    //   才退回 FIFO（填最早一个未填充的卡）。
    const agentConversations = ref([]);   // 左侧会话列表
    const agentConvId = ref(null);        // 当前选中的会话 id（string）
    const agentEditConvId = ref(null);    // 正在编辑标题的会话 id（行内 input 态）
    const agentEditTitle = ref('');       // 行内编辑的标题临时值
    const deletingConvId = ref(null);     // 正待确认删除的会话 id（2 步行内确认，与编辑态互斥）
    const agentMessages = ref([]);        // 当前会话的展示项流（见下 makeToolItem / 文本项结构）
    const agentInput = ref('');           // 输入框内容
    const agentConnected = ref(false);    // WS 是否已连接（连接指示灯）
    const agentTyping = ref(false);       // 是否有轮次进行中（「正在思考…」+ 禁发，遵守后端单轮次约束）
    const agentTurnError = ref('');       // 最近一轮的模型侧错误（turn_end.error）
    const agentLoading = ref(false);      // 拉历史中
    const agentStreamEl = ref(null);      // 消息流容器（滚动到底用）
    // WS 与重连句柄用普通变量（与录音页 recorder/pollTimer 同风格，非响应式）。
    let agentWS = null, agentReconnectTimer = null;
    let agentWantConnect = false;         // 是否「希望保持连接」（切走/切换会话时置 false，抑制重连）

    // ---- 刷新恢复：把「当前 tab」与「当前会话 id」持久化到 localStorage，刷新后回到现场 ----
    const LS_TAB = 'zw_tab';              // 上次所在 tab
    const LS_AGENT_CONV = 'zw_agent_conv'; // 上次选中的问知微会话 id
    function persistAgentConv(id) { try { id ? localStorage.setItem(LS_AGENT_CONV, id) : localStorage.removeItem(LS_AGENT_CONV); } catch (e) {} }
    // 已渲染消息 id 集合（非响应式）：刷新/重连时历史(loadAgentHistory)与广播器重放(replay)会重叠，
    // 按 msg_id 去重，避免同一条消息渲染两次。每次载入历史时重建。
    let agentSeen = new Set();

    // ---- 思考计时：nowTick 每秒推进（仅在有轮次进行中时走），用于「思考中 mm:ss」实时刷新 ----
    const nowTick = ref(Date.now());
    let agentTimer = null;
    watch(agentTyping, (running) => {
      if (running) { nowTick.value = Date.now(); if (!agentTimer) agentTimer = setInterval(() => { nowTick.value = Date.now(); }, 1000); }
      else if (agentTimer) { clearInterval(agentTimer); agentTimer = null; }
    });

    // ---- 轮次分组（视图层）：把平铺 agentMessages 折成一轮轮 ----
    // 轮次边界：user 文本项开始，直到下一个 user（或结尾）。每轮：
    //   userText  该轮用户输入；thinking[] 过程项（tool / reasoning）；answers[] 助手答复文本；
    //   startAt/answerAt/lastAt 计时用（首个 user / 首条答复 / 最后一项 的时间戳，毫秒）。
    // 分组只在视图层做，handleAgentFrame / mapAgentHistory 仍维护平铺 agentMessages（live 与历史复用同一分组）。
    const agentTurns = computed(() => {
      const turns = [];
      let cur = null;
      const open = (userText, ts) => { cur = { userText: userText || '', thinking: [], answers: [], proposals: [], startAt: ts || null, answerAt: null, lastAt: ts || null }; turns.push(cur); };
      for (const m of agentMessages.value) {
        if (m.kind === 'text' && m.role === 'user') { open(m.content, m.ts); continue; }
        if (!cur) open('', m.ts); // 无前导 user（历史开头/异常）：起一个匿名轮兜底
        if (m.kind === 'text' && m.role === 'assistant') { cur.answers.push(m); if (cur.answerAt == null) cur.answerAt = m.ts || null; }
        else if (m.kind === 'tool' && m.proposal) cur.proposals.push(m); // 写-提议确认卡：移出思考块，放答复区显眼处（需用户操作）
        else cur.thinking.push(m); // reasoning + 读工具 + 提议骨架（结果未到、proposal 未定）
        if (m.ts) cur.lastAt = m.ts;
      }
      return turns;
    });
    // 该轮是否「进行中」：最后一轮 + agentTyping + 尚无答复。
    function turnRunning(ti) { const t = agentTurns.value; return ti === t.length - 1 && agentTyping.value && !!t[ti] && t[ti].answers.length === 0; }
    // agentTyping 为真但当前没有可展示的「进行中思考块」（用户帧尚未回显 / 上一轮已答完的空档）→ 显示兜底「正在思考」。
    const agentThinkingGap = computed(() => { if (!agentTyping.value) return false; const t = agentTurns.value; const last = t.length - 1; return !(last >= 0 && t[last].answers.length === 0); });
    // 思考耗时（秒）：进行中用 nowTick 实时；已结束用「首条答复 / 最后一项」时间减 startAt。
    function turnDuration(ti) {
      const t = agentTurns.value[ti];
      if (!t || !t.startAt) return 0;
      const end = turnRunning(ti) ? nowTick.value : (t.answerAt || t.lastAt || t.startAt);
      return Math.max(0, Math.round((end - t.startAt) / 1000));
    }
    function fmtDur(sec) { return sec < 60 ? sec + ' 秒' : Math.floor(sec / 60) + ' 分 ' + (sec % 60) + ' 秒'; }
    // 折叠状态：按轮次序号存显式布尔；未设置时默认「进行中展开、已结束折叠」。
    const agentTurnExpanded = reactive({});
    // 流式草稿：当前进行中一轮的「边想边现」缓冲——reasoning_delta / answer_delta 增量帧实时累积到此，
    // 最终权威帧(reasoning/assistant)到达时清空并由落库项接管。仅当前活轮有效。
    const streamDraft = reactive({ reasoning: '', answer: '' });
    function resetStreamDraft() { streamDraft.reasoning = ''; streamDraft.answer = ''; }
    function isTurnOpen(ti) { return (ti in agentTurnExpanded) ? agentTurnExpanded[ti] : turnRunning(ti); }
    function toggleTurn(ti) { agentTurnExpanded[ti] = !isTurnOpen(ti); }
    function resetAgentView() { for (const k of Object.keys(agentTurnExpanded)) delete agentTurnExpanded[k]; resetStreamDraft(); }


    // ---- XSS 安全的极简 Markdown（先转义所有 HTML，再只注入自己生成的白名单标签）----
    // 助手文本、工具结果、记忆/转写内容均为【不可信】（可能含注入的 HTML）。任何要作为
    // HTML 插入的文本必须先转义；仅助手气泡用 v-html（渲染 Markdown），工具卡一律走
    // Vue 文本插值（自动转义）。不引入外部 md 库。
    function escapeHtml(s) {
      return String(s == null ? '' : s)
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }
    // 行内格式（作用于【已转义】文本）：`代码` / **粗** / *斜* / [文字](链接)。
    // 链接 href 仅放行 http/https/mailto，其余降级为 #，防 javascript: 等注入。
    function inlineMd(s) {
      const codes = [];
      // 先抽出行内代码，避免其中的 * / [ ] 被后续规则误伤
      s = s.replace(/`([^`]+)`/g, (m, c) => '\uE000C' + (codes.push('<code>' + c + '</code>') - 1) + '\uE000');
      s = s.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, txt, url) => {
        const safe = /^(https?:|mailto:)/i.test(url) ? url : '#'; // url 已转义，可安全放进属性
        return '<a href="' + safe + '" target="_blank" rel="noopener noreferrer">' + txt + '</a>';
      });
      s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
      s = s.replace(/\*([^*\n]+)\*/g, '<em>$1</em>');
      s = s.replace(/\uE000C(\d+)\uE000/g, (m, i) => codes[Number(i)] || '');
      return s;
    }
    // 块级：围栏代码块（```）、有序/无序列表、段落（连续文本行用 <br> 连接）。
    function renderMarkdown(src) {
      if (src == null || src === '') return '';
      const blocks = [];
      // 1) 先摘出围栏代码块（内容整体转义、原样保留），用占位符替换，避免被行内规则处理
      let text = String(src).replace(/```(?:[^\n`]*)\n?([\s\S]*?)```/g, (m, code) =>
        '\uE000B' + (blocks.push('<pre class="chat-code"><code>' + escapeHtml(code.replace(/\s+$/, '')) + '</code></pre>') - 1) + '\uE000');
      // 2) 转义其余全部文本（占位符不含特殊字符，安全穿过）
      text = escapeHtml(text);
      const lines = text.split('\n');
      let html = '', listType = null, para = [];
      const flushPara = () => { if (para.length) { html += '<p>' + para.join('<br>') + '</p>'; para = []; } };
      const closeList = () => { if (listType) { html += '</' + listType + '>'; listType = null; } };
      for (const line of lines) {
        const t = line.trim();
        if (/^\uE000B\d+\uE000$/.test(t)) { flushPara(); closeList(); html += t; continue; } // 独立成行的代码块占位符
        // ATX 标题：行首 1-6 个 # + 空格 → <h1>~<h6>（class=chat-h 控样式），标题文本走行内格式；
        // 结尾可选的收尾 #（如「## 标题 ##」）一并去掉。须放在列表/段落判定之前。
        const hd = t.match(/^(#{1,6})\s+(.*?)\s*#*$/);
        if (hd) { flushPara(); closeList(); const lv = hd[1].length; html += '<h' + lv + ' class="chat-h">' + inlineMd(hd[2]) + '</h' + lv + '>'; continue; }
        const ul = line.match(/^\s*[-*+]\s+(.*)$/);
        const ol = line.match(/^\s*\d+\.\s+(.*)$/);
        if (ul) { flushPara(); if (listType !== 'ul') { closeList(); html += '<ul>'; listType = 'ul'; } html += '<li>' + inlineMd(ul[1]) + '</li>'; }
        else if (ol) { flushPara(); if (listType !== 'ol') { closeList(); html += '<ol>'; listType = 'ol'; } html += '<li>' + inlineMd(ol[1]) + '</li>'; }
        else if (t === '') { flushPara(); }                     // 空行=段落分隔；注意「不断列表」——列表项之间的空行只是排版间距，若在此 closeList 会把一个 <ol>/<ul> 拆成多个单项列表，有序列表序号便全部重置为 1（表现为每项都显示 1.）。列表改由其后首个非列表行（普通段落/标题/代码块/异型列表）或文末统一收尾。
        else { closeList(); para.push(inlineMd(line)); }         // 真正的非列表、非空行：先收尾可能开着的列表，再并入段落
      }
      flushPara(); closeList();
      // 3) 回填代码块
      return html.replace(/\uE000B(\d+)\uE000/g, (m, i) => blocks[Number(i)] || '');
    }

    // ---- 工具名/参数/结果解析 ----
    // dsh 侧 MCP 工具名形如 mcp__zhiwei__get_todos，取 __ 分隔的末段作基名。
    function toolBase(name) { const p = String(name || '').split('__'); return p[p.length - 1]; }
    const TOOL_LABELS = {
      search_memory: '检索记忆', get_timeline: '查看时间线', get_topics: '查看主题',
      get_todos: '查看待办', get_topic_status: '话题状态', generate_report: '生成报告',
      zhiwei_ping: '连通性检查',
      // 画像读工具（P2）
      get_profile: '读取画像', get_person: '查看人物',
      // 写-提议工具（propose_*）：出结果后会翻成「确认卡」，这里只是进行中骨架的友好名。
      propose_memory_edit: '修改记忆', propose_memory_dismiss: '忽略记忆',
      propose_topic_rename: '话题改名', propose_topic_confirm: '确认话题', propose_topic_dismiss: '忽略话题',
      propose_todo_create: '新建待办', propose_todo_status: '待办状态',
      propose_profile_attr: '更新画像属性', propose_profile_event: '记录大事记',
      propose_profile_relationship: '新增人物关系', propose_profile_metric: '记录指标',
      propose_profile_cycle: '记录健康周期', propose_profile_activity: '记录活动',
    };
    function toolLabel(name) { return TOOL_LABELS[toolBase(name)] || (name || '工具'); }
    // 参数摘要：把 args(JSON 字符串) 折成 "k=v · k=v"，解析失败原样显示
    function toolArgsSummary(args) {
      if (!args) return '';
      try {
        const o = JSON.parse(args);
        return Object.entries(o).filter(([, v]) => v !== '' && v != null)
          .map(([k, v]) => k + '=' + (typeof v === 'string' ? v : JSON.stringify(v))).join(' · ');
      } catch (e) { return String(args); }
    }
    function safeParse(t) { try { return JSON.parse(t); } catch (e) { return null; } }
    function coerceStr(v) { return typeof v === 'string' ? v : JSON.stringify(v); }
    // 报告类工具（generate_report / get_topic_status）结果是包装行 {..., content:{…}}；
    // 抽出 content 的 headline/summary 作标题，已知数组字段作分节列表，风险/按话题做特殊拼接。
    function reportSections(parsed) {
      const c = parsed && parsed.content ? parsed.content : parsed;
      if (!c || typeof c !== 'object') return null;
      const title = c.headline || c.summary || '报告';
      const defs = [['要点', 'highlights'], ['决定', 'decisions'], ['洞察', 'insights'], ['明日', 'tomorrow'],
        ['里程碑', 'milestones'], ['未完成待办', 'open_todos'], ['阻塞', 'blockers'], ['下周', 'next_week']];
      const sections = [];
      for (const [label, key] of defs) {
        const arr = c[key];
        if (Array.isArray(arr) && arr.length) sections.push({ label, items: arr.map(coerceStr) });
      }
      if (Array.isArray(c.risks) && c.risks.length) {
        sections.push({ label: '风险', items: c.risks.map(r => typeof r === 'string' ? r : ((r.desc || '') + (r.severity ? '（' + r.severity + '）' : ''))) });
      }
      if (Array.isArray(c.by_topic) && c.by_topic.length) {
        sections.push({ label: '按话题', items: c.by_topic.map(t => (t.topic || '') + (t.progress != null ? ' · ' + Math.round(t.progress * 100) + '%' : '')) });
      }
      return { title, progress: (typeof c.progress === 'number' ? c.progress : null), sections };
    }
    function prettyJSON(text) { try { return JSON.stringify(JSON.parse(text), null, 2); } catch (e) { return String(text == null ? '' : text); } }

    // ---- 写操作提议（propose_* 工具结果）→ 确认卡 ----
    // 后端 propose_* 工具不直接改库，只落一条 pending agent_proposal 并把它作为 tool_result 回传
    //（JSON：{id,kind,target_kind,target_id,payload:{old?,new?},rationale,status}）。前端据此渲染
    // 「确认卡」：old→new diff + 理由 + [确认]/[放弃]。确认/放弃走 /api/agent/proposals/{id}/{action}
    //（幂等）。所有展示值一律走 Vue {{ }} 自动转义，不用 v-html（rationale/old/new 均为不可信文本）。
    const PROPOSAL_KINDS = ['memory_update', 'memory_dismiss', 'topic_rename', 'topic_confirm', 'topic_dismiss', 'todo_create', 'todo_status', 'profile_attr', 'profile_event', 'profile_relationship', 'profile_metric', 'profile_cycle', 'profile_activity'];
    const PROPOSAL_TITLES = {
      memory_update: '修改记忆', memory_dismiss: '忽略记忆',
      topic_rename: '话题改名', topic_confirm: '确认话题', topic_dismiss: '忽略话题',
      todo_create: '新建待办', todo_status: '待办状态',
      profile_attr: '更新画像属性', profile_event: '记录大事记', profile_relationship: '新增人物关系', profile_metric: '记录指标',
      profile_cycle: '记录健康周期', profile_activity: '记录活动',
    };
    // 字段名 → 中文标签（diff 行左侧）
    const PROPOSAL_FIELD_LABELS = { title: '标题', content: '内容', name: '名称', status: '状态', due_at: '截止', topic_id: '关联话题', type: '类型', kind: '类型', attr_key: '属性', value: '值', event_type: '事件类型', occurred_at: '发生时间', relation_type: '关系类型', related_person_name: '关联人', org_name: '组织', direction: '方向', label: '称呼', metric_key: '指标', value_num: '数值', value_text: '描述', unit: '单位', measured_at: '时间', cycle_type: '周期类型', anchor_date: '上次开始', period_days: '周期天数', duration_days: '持续天数', dosage: '剂量', frequency: '频次', activity: '活动', tool: '工具', location: '地点', commute_mode: '通勤', started_at: '开始时间', duration_min: '时长(分)' };
    // 状态枚举 → 中文（memory/topic/todo 状态并集）
    const PROPOSAL_STATUS_LABELS = { suggested: '待确认', pending: '待处理', confirmed: '已确认', active: '活跃', done: '已完成', dismissed: '已忽略', applied: '已应用', expired: '已过期' };
    // 单字段值格式化：status 走中文枚举，*_at 走本地时间，其余按类型稳妥转字符串。
    function fmtProposalField(k, v) {
      if (v == null || v === '') return '';
      if (k === 'status') return PROPOSAL_STATUS_LABELS[v] || String(v);
      if (/_at$/.test(k)) return fmtTime(v);
      if (typeof v === 'string') return v;
      if (typeof v === 'boolean') return v ? '是' : '否';
      if (typeof v === 'number') return String(v);
      try { return JSON.stringify(v); } catch (e) { return String(v); }
    }
    // 检测 tool_result 是否为「提议」形状：对象(非数组) + 有 id/status + (已知 kind 或 payload.old/new)。
    // 读工具结果（数组 / 报告对象 {content} / ping）都不满足此组合，故不会误判为提议。
    function asProposal(parsed) {
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
      if (!parsed.id || !parsed.status) return null;
      const kindOK = parsed.kind && PROPOSAL_KINDS.indexOf(parsed.kind) >= 0;
      const pl = parsed.payload;
      const payloadOK = pl && typeof pl === 'object' && (pl.old !== undefined || pl.new !== undefined);
      return (kindOK || payloadOK) ? parsed : null;
    }
    // proposalView：把提议预计算成模板就绪视图（标题 / 目标身份 / 效果句 / diff 行）。
    // diff 只遍历 new 的键（真正被改的字段），old[k] 作改前值——避免把「old 有而 new 没有」的
    // 上下文字段（如被忽略记忆的标题）误显示成「被清空」。忽略/确认类给一句话效果，不铺 diff。
    function proposalView(p) {
      if (!p) return null;
      const kind = p.kind;
      const payload = (p.payload && typeof p.payload === 'object') ? p.payload : {};
      const old = (payload.old && typeof payload.old === 'object') ? payload.old : null;
      const nw = (payload.new && typeof payload.new === 'object') ? payload.new : null;
      const contextLabel = old ? (old.title || old.name || '') : '';
      // 效果类（忽略/确认）：改前后差异不重要，直接给一句话说明将发生什么。
      const EFFECTS = {
        memory_dismiss: contextLabel ? ('将忽略记忆「' + contextLabel + '」') : '将忽略这条记忆',
        topic_confirm: contextLabel ? ('将确认话题「' + contextLabel + '」为正式话题') : '将确认这个话题',
        topic_dismiss: contextLabel ? ('将忽略话题「' + contextLabel + '」') : '将忽略这个话题',
      };
      const isEffect = !!EFFECTS[kind];
      const rows = [];
      if (nw) {
        for (const k of Object.keys(nw)) {
          const ns = fmtProposalField(k, nw[k]);
          const hasOld = !!(old && old[k] !== undefined && old[k] !== null && old[k] !== '');
          const os = hasOld ? fmtProposalField(k, old[k]) : '';
          if (ns === '' && os === '') continue;
          rows.push({ field: PROPOSAL_FIELD_LABELS[k] || k, old: os, new: ns, hasOld });
        }
      }
      // 变更字段本身含 title/name 时，diff 行已展示身份，无需再单列目标身份行（去重）。
      const changingIdentity = !!(nw && (('title' in nw) || ('name' in nw)));
      const showContext = !!contextLabel && !isEffect && kind !== 'todo_create' && !changingIdentity;
      return { title: PROPOSAL_TITLES[kind] || '提议', kind, isEffect, effect: EFFECTS[kind] || '', contextLabel, showContext, rows };
    }
    // 确认/放弃：POST /api/agent/proposals/{id}/{action}，回传更新后的提议 → 就地更新 status
    //（applied→「已确认」/ dismissed→「已放弃」徽标，按钮消失）。端点幂等，重复确认返回当前状态。
    // 出错保留按钮 + 行内错误提示，可重试。proposalBusy 防重复点击。
    async function resolveProposal(it, action) {
      const p = it && it.proposal;
      if (!p || !p.id || it.proposalBusy) return;
      it.proposalBusy = true; it.proposalError = '';
      try {
        const updated = await api('POST', '/api/agent/proposals/' + p.id + '/' + action);
        // 端点回传整条更新后的提议；取其 status，兜底按动作推断（confirm→applied / dismiss→dismissed）。
        const st = (updated && updated.status) ? updated.status : (action === 'confirm' ? 'applied' : 'dismissed');
        it.proposal.status = st;
      } catch (e) {
        it.proposalError = (e && e.message) || String(e);
      } finally {
        it.proposalBusy = false;
      }
    }
    function confirmProposal(it) { return resolveProposal(it, 'confirm'); }
    function dismissProposal(it) { return resolveProposal(it, 'dismiss'); }

    // 工具展示项工厂：tool_call → 骨架卡（result=null）；tool_result → 填充。
    // proposal/pview/proposalBusy/proposalError 为写-提议确认卡预留（读工具恒为 null/false，无害）。
    function makeToolItem(callId, name, args, ts) {
      return { kind: 'tool', call_id: callId, name, base: toolBase(name), label: toolLabel(name),
        args, argsSummary: toolArgsSummary(args), result: null, parsed: null, report: null,
        proposal: null, pview: null, proposalBusy: false, proposalError: '', ts: ts || Date.now() };
    }
    function fillTool(it, text, isErr) {
      it.result = { text: text || '', is_error: !!isErr };
      it.parsed = safeParse(text);
      if (it.base === 'generate_report' || it.base === 'get_topic_status') it.report = reportSections(it.parsed);
      // 写-提议工具的成功结果 → 确认卡（错误结果不当提议，走通用错误样式）。
      if (!isErr) {
        it.proposal = asProposal(it.parsed);
        it.pview = it.proposal ? proposalView(it.proposal) : null;
      }
    }
    // 把 tool_result 填到 arr 里【最早一个未填充】的工具卡（FIFO 配对）。无匹配返回 false。
    function fillNextToolIn(arr, text, isErr) {
      for (const it of arr) { if (it.kind === 'tool' && it.result === null) { fillTool(it, text, isErr); return true; } }
      return false;
    }
    // 把 tool_result 填到对应工具卡：优先按 call_id 精确命中未填充的卡；无 call_id 或未命中
    // （旧数据/异常）则退回 FIFO（fillNextToolIn）。无任何匹配返回 false。
    function fillToolResult(arr, callId, text, isErr) {
      if (callId) {
        for (const it of arr) {
          if (it.kind === 'tool' && it.call_id === callId && it.result === null) { fillTool(it, text, isErr); return true; }
        }
      }
      return fillNextToolIn(arr, text, isErr);
    }
    // 历史消息（GET）→ 展示项：tool_payload 已是对象（Go 内联 JSON）。tool_result 带 call_id，按 id 配对（未命中退回 FIFO）。
    function mapAgentHistory(messages) {
      const items = [];
      for (const m of (messages || [])) {
        const ts = m.created_at ? new Date(m.created_at).getTime() : Date.now();
        if (m.kind === 'tool_call') {
          const tp = m.tool_payload || {};
          items.push(makeToolItem(tp.call_id, tp.name, tp.arguments, ts));
        } else if (m.kind === 'tool_result') {
          const tp = m.tool_payload || {};
          if (!fillToolResult(items, tp.call_id, tp.text, tp.is_error)) {
            // 落单的结果（历史异常）：作独立错误/结果卡兜底显示
            const it = makeToolItem('', '', '', ts); fillTool(it, tp.text, tp.is_error); items.push(it);
          }
        } else if (m.kind === 'reasoning') {
          // 思考内容（reasoning 落库为独立 kind）：进折叠块显示
          if (m.content) items.push({ kind: 'reasoning', role: 'assistant', content: m.content, msg_id: m.id, ts });
        } else if (m.content) {
          items.push({ kind: 'text', role: m.role, content: m.content, msg_id: m.id, ts });
        }
      }
      return items;
    }

    // 距底判定：容器内容已滚到接近底部（60px 容差）时返回 true。容器未就绪按 true（默认跟随）。
    function agentNearBottom() {
      const el = agentStreamEl.value;
      if (!el) return true;
      return el.scrollHeight - el.scrollTop - el.clientHeight <= 60;
    }
    // 滚动跟随：force=true 强制到底（新载入历史 / 用户刚发消息）；否则仅当用户当前贴底时才跟随，
    // 用户上滚查看历史/思考过程时锁定其位置、不被新内容拽到底。
    // 关键时序：须在改动消息数据后、Vue 刷新 DOM 前【同步】调用——此刻读到的是旧内容的滚动量，
    // 正确反映「用户在新内容出现前是否贴底」；再于 nextTick（DOM 更新后）按此决定是否滚到底。
    function scrollAgentBottom(force) {
      const stick = force || agentNearBottom();
      nextTick(() => { const el = agentStreamEl.value; if (el && stick) el.scrollTop = el.scrollHeight; });
    }
    async function loadAgentConversations() {
      try { const d = await api('GET', '/api/agent/conversations'); agentConversations.value = d || []; }
      catch (e) { showError(e); }
    }
    async function loadAgentHistory(cid) {
      agentLoading.value = true;
      try {
        const d = await api('GET', '/api/agent/conversations/' + cid);
        const msgs = d.messages || [];
        // 重建去重集合：历史里每条消息 id 都算「已渲染」，广播器重放同 id 的帧会被跳过。
        agentSeen = new Set();
        for (const m of msgs) if (m.id) agentSeen.add(m.id);
        resetStreamDraft(); // 重连/重载：清空流式草稿，靠重放/历史重建
        agentMessages.value = mapAgentHistory(msgs);
        scrollAgentBottom(true); // 新载入会话历史：强制到底
      } catch (e) { showError(e); }
      finally { agentLoading.value = false; }
    }
    // 同源 WS 地址：跟随页面协议（https→wss）
    function agentWSURL(cid) {
      return (location.protocol === 'https:' ? 'wss:' : 'ws:') + '//' + location.host + '/api/agent/conversations/' + cid + '/ws';
    }
    function handleAgentFrame(ev) {
      let f; try { f = JSON.parse(ev.data); } catch (e) { return; }
      // 去重：带 msg_id 的帧若已渲染过（历史与广播器重放重叠）则跳过；turn_active/turn_end 无 id、幂等。
      if (f.msg_id && agentSeen.has(f.msg_id)) return;
      if (f.msg_id) agentSeen.add(f.msg_id);
      const now = Date.now();
      switch (f.type) {
        case 'turn_active': agentTyping.value = true; break; // 重连到进行中的一轮：恢复「思考中」态
        case 'reasoning_delta': agentTyping.value = true; streamDraft.reasoning += (f.content || ''); break; // 思考逐字流
        case 'answer_delta': agentTyping.value = true; streamDraft.answer += (f.content || ''); break;         // 答复逐字流
        case 'user': agentMessages.value.push({ kind: 'text', role: 'user', content: f.content, msg_id: f.msg_id, ts: now }); break;
        case 'assistant': agentMessages.value.push({ kind: 'text', role: 'assistant', content: f.content, msg_id: f.msg_id, ts: now }); streamDraft.answer = ''; break;       // 权威答复到达 → 清答复草稿
        case 'reasoning': agentMessages.value.push({ kind: 'reasoning', role: 'assistant', content: f.content, msg_id: f.msg_id, ts: now }); streamDraft.reasoning = ''; break; // 权威思考到达 → 清思考草稿
        case 'tool_call': agentMessages.value.push(makeToolItem(f.call_id, f.name, f.args, now)); break;
        case 'tool_result': fillToolResult(agentMessages.value, f.call_id, f.content, f.is_error); break;
        case 'turn_end': agentTyping.value = false; resetStreamDraft(); if (f.error) agentTurnError.value = f.error; break;
      }
      // 用户自己发的消息回显 → 强制到底（用户刚发言，期望看到自己的气泡）；其余帧（思考/答复流、
      // 工具卡等）仅当用户贴底时跟随，用户上滚查看时不打扰。
      scrollAgentBottom(f.type === 'user');
    }
    // 打开/重开 WS（先关旧连接）。断线时若仍希望连接且仍停留在本会话/本 tab，则 1.5s 后重连。
    function openAgentWS(cid) {
      closeAgentWS();
      agentWantConnect = true;
      let ws;
      try { ws = new WebSocket(agentWSURL(cid)); }
      catch (e) { showError(e); return; }
      agentWS = ws;
      ws.onopen = () => { if (agentWS === ws) agentConnected.value = true; };
      ws.onmessage = handleAgentFrame;
      ws.onclose = () => {
        if (agentWS !== ws) return; // 已被 closeAgentWS 换掉：忽略旧连接的回调
        agentConnected.value = false;
        agentTyping.value = false;  // 断线时停掉「思考中」，避免发送按钮永久禁用（重发会自纠正）
        agentWS = null;
        if (agentWantConnect && tab.value === 'agent' && agentConvId.value === cid) {
          // 重连时重拉历史（从 DB 权威状态补回断线期间漏掉的帧），再开新连接
          agentReconnectTimer = setTimeout(() => {
            if (agentWantConnect && agentConvId.value === cid) { loadAgentHistory(cid); openAgentWS(cid); }
          }, 1500);
        }
      };
      ws.onerror = () => { /* 错误后紧跟 onclose，统一在那里处理重连 */ };
    }
    function closeAgentWS() {
      agentWantConnect = false;
      if (agentReconnectTimer) { clearTimeout(agentReconnectTimer); agentReconnectTimer = null; }
      if (agentWS) {
        const ws = agentWS; agentWS = null;
        try { ws.onclose = null; ws.onmessage = null; ws.onerror = null; ws.onopen = null; ws.close(); } catch (e) {}
      }
      agentConnected.value = false;
    }
    // 选中某会话：切换前关旧 WS、清流，拉历史后开新 WS。
    async function selectAgentConversation(c) {
      if (agentConvId.value === c.id) return;
      deletingConvId.value = null; // 切换会话清掉他项残留的删除确认态
      closeAgentWS();
      agentConvId.value = c.id;
      persistAgentConv(c.id);
      agentMessages.value = [];
      agentTurnError.value = '';
      agentTyping.value = false;
      resetAgentView();
      await loadAgentHistory(c.id);
      openAgentWS(c.id);
    }
    // 进入行内编辑：记下目标会话 + 临时标题；不触发选中（按钮已 @click.stop）。清删除确认态（互斥）。
    function startEditConv(c) {
      deletingConvId.value = null;
      agentEditConvId.value = c.id;
      agentEditTitle.value = c.title || '';
    }
    function cancelEditConv() {
      agentEditConvId.value = null;
      agentEditTitle.value = '';
    }
    // 失焦/回车保存：PATCH 改标题(manual)，成功重拉列表。空标题或未变则取消。
    // 防重复：blur 与 enter 可能连发——先判 agentEditConvId 再置 null，只生效一次。
    async function saveAgentTitle(c) {
      if (agentEditConvId.value !== c.id) return; // 已取消/已保存（重复 blur/enter）
      agentEditConvId.value = null;
      const title = agentEditTitle.value.trim();
      if (!title || title === (c.title || '')) return; // 空标题或未改动：不发请求
      try {
        await api('PATCH', '/api/agent/conversations/' + c.id, { title });
        await loadAgentConversations();
      } catch (e) {
        notify(e.message || '保存标题失败', 4000);
      }
    }
    // 2 步行内删除确认（对齐 todo/topic 等的 ask/confirm 模式，不弹原生 confirm）：
    // 点🗑 → askDeleteConv 进入确认态（列表项就地显示「确认删除?/取消」）；清编辑态（互斥）。
    function askDeleteConv(c) {
      agentEditConvId.value = null;
      deletingConvId.value = c.id;
    }
    function cancelDeleteConv() {
      deletingConvId.value = null;
    }
    // 软删除：DELETE 会话（后端置 archived）。若删的是当前会话则关 WS + 清空主区。成功后重拉列表。
    async function deleteAgentConversation(c) {
      try {
        await api('DELETE', '/api/agent/conversations/' + c.id);
        deletingConvId.value = null;
        if (agentConvId.value === c.id) {
          closeAgentWS();
          agentConvId.value = null;
          agentMessages.value = [];
        }
        await loadAgentConversations();
        notify('会话已删除');
      } catch (e) {
        showError(e);
      }
    }
    // 新对话：POST 建会话 → 刷新列表 → 选中并连 WS。
    async function newAgentConversation() {
      try {
        const c = await api('POST', '/api/agent/conversations', {});
        await loadAgentConversations();
        closeAgentWS();
        agentConvId.value = c.id;
        persistAgentConv(c.id);
        agentMessages.value = [];
        agentSeen = new Set();
        agentTurnError.value = '';
        agentTyping.value = false;
        resetAgentView();
        openAgentWS(c.id);
      } catch (e) { showError(e); }
    }
    // 发送：Enter 触发。遵守后端单轮次约束——进行中(agentTyping)不再发。
    function sendAgentMessage() {
      const text = agentInput.value.trim();
      if (!text || !agentConvId.value || agentTyping.value) return;
      if (!agentWS || agentWS.readyState !== WebSocket.OPEN) { showError(new Error('连接未就绪，请稍候')); return; }
      try {
        agentWS.send(JSON.stringify({ text }));
        agentInput.value = '';
        agentTurnError.value = '';
        agentTyping.value = true; // user 帧会由服务端回显渲染气泡；这里只置「思考中」
      } catch (e) { showError(e); }
    }
    // 停止：轮次进行中点「停止」→ WS 发 {stop:true}，请求后端优雅中止本轮（dsh abort）。
    // 不在本地立刻清 agentTyping——以服务端为准：真正收尾的 turn_end 帧回来时由 handleAgentFrame
    // 置 agentTyping=false 再恢复「发送」，避免「已点停止但后端仍在收尾」的中间态错乱。
    // WS 未就绪或本就没有进行中的轮次 → 忽略（无轮可停）。
    function stopAgentMessage() {
      if (!agentTyping.value) return;
      if (!agentWS || agentWS.readyState !== WebSocket.OPEN) return;
      try { agentWS.send(JSON.stringify({ stop: true })); } catch (e) { /* 发送失败无害：轮次仍会靠 idle/超时收尾 */ }
    }

    // ---------- 设置：知微人设（identity/soul，全局单份；每轮注入，保存后下条消息即时生效，不重启 dsh） ----------
    // 后端契约：GET /api/agent/config → {identity, soul, preview}；PUT /api/agent/config {identity, soul}。
    const agentCfgIdentity = ref('');
    const agentCfgSoul = ref('');
    const agentCfgSaving = ref(false);
    const agentCfgSaved = ref(false);
    const agentCfgSystemPrompt = ref(''); // 进程级 persona（只读）
    const agentCfgDatetimeHead = ref(''); // 每轮无条件注入的「当前日期+时区」（只读，动态）
    const agentCfgOwnerHead = ref('');    // 每轮注入的 owner 画像头（只读，动态）
    // 注入预览：与后端 AssemblePersona 同格式，随输入实时更新（保存后即后端每轮注入的内容）。
    const agentCfgPreview = computed(() => {
      const id = (agentCfgIdentity.value || '').trim();
      const soul = (agentCfgSoul.value || '').trim();
      const b = [];
      if (id) b.push('你的身份设定：\n' + id);
      if (soul) b.push('你的性格与说话风格：\n' + soul);
      return b.join('\n\n');
    });
    // 整体 prompt 组装（只读预览）：system(进程级) + 每轮注入前缀(人设+当前日期时区+owner画像) + 动态检索说明 + 你的问题。
    // 顺序须与后端 orchestrator.runTurn 一致：人设 → 当前日期+时区 → owner 画像头 →（动态检索种子）→ 你的问题。
    const agentCfgFullPrompt = computed(() => {
      const seg = [];
      seg.push('【System · 进程级人设（不可编辑）】\n' + (agentCfgSystemPrompt.value || '（未设置）'));
      const inject = [];
      if (agentCfgPreview.value) inject.push(agentCfgPreview.value);
      if (agentCfgDatetimeHead.value) inject.push(agentCfgDatetimeHead.value);
      if (agentCfgOwnerHead.value) inject.push(agentCfgOwnerHead.value);
      seg.push('【每轮注入到你消息之前的背景】\n' + (inject.length ? inject.join('\n\n') : '（无额外注入）'));
      seg.push('【每轮还会按你的提问动态检索相关记忆/时间线作为背景（此处不预览）】');
      seg.push('【最后拼上你的实际问题】');
      return seg.join('\n\n──────────\n\n');
    });
    async function loadAgentConfig() {
      try {
        const d = await api('GET', '/api/agent/config');
        agentCfgIdentity.value = (d && d.identity) || '';
        agentCfgSoul.value = (d && d.soul) || '';
        agentCfgSystemPrompt.value = (d && d.system_prompt) || '';
        agentCfgDatetimeHead.value = (d && d.datetime_head) || '';
        agentCfgOwnerHead.value = (d && d.owner_head) || '';
        agentCfgSaved.value = false;
      } catch (e) { showError(e); }
    }
    async function saveAgentConfig() {
      if (agentCfgSaving.value) return;
      agentCfgSaving.value = true; agentCfgSaved.value = false;
      try {
        await api('PUT', '/api/agent/config', { identity: agentCfgIdentity.value, soul: agentCfgSoul.value });
        agentCfgSaved.value = true;
      } catch (e) { showError(e); }
      finally { agentCfgSaving.value = false; }
    }

    // ---------- 设置：MCP 服务（全局，手动管理；增删启禁经 /api/agent/mcp，保存后即时生效） ----------
    // 后端契约：GET /api/agent/mcp → {servers:[{id,server_key,display_name,transport,url,command,args,enabled,builtin}]}；
    // POST {server_key,display_name,transport,url|command+args,enabled}；PATCH /{id} {enabled}；DELETE /{id}。
    // 启用/禁用/增删后端即时生效：重生成 cordis + 对在用 dsh 热插拔（无需重启）。
    const mcpServers = ref([]);
    const mcpForm = ref({ server_key: '', display_name: '', transport: 'streamable-http', url: '', command: '', args: '' });
    const mcpErr = ref('');
    async function loadMCP() {
      try { const d = await api('GET', '/api/agent/mcp'); mcpServers.value = (d && d.servers) || []; }
      catch (e) { showError(e); }
    }
    async function addMCP() {
      mcpErr.value = '';
      const f = mcpForm.value;
      const key = (f.server_key || '').trim();
      if (!/^[A-Za-z0-9_]{1,64}$/.test(key)) { mcpErr.value = 'server_key 需为 1-64 位字母/数字/下划线'; return; }
      const body = { server_key: key, display_name: (f.display_name || '').trim() || key, transport: f.transport, enabled: true };
      if (f.transport === 'streamable-http') body.url = (f.url || '').trim();
      else {
        body.command = (f.command || '').trim();
        body.args = (f.args || '').trim() ? f.args.trim().split(/\s+/) : [];
      }
      try {
        await api('POST', '/api/agent/mcp', body);
        mcpForm.value = { server_key: '', display_name: '', transport: 'streamable-http', url: '', command: '', args: '' };
        await loadMCP();
      } catch (e) { mcpErr.value = (e && e.message) || String(e); }
    }
    async function toggleMCP(m) {
      try { await api('PATCH', '/api/agent/mcp/' + m.id, { enabled: !m.enabled }); await loadMCP(); }
      catch (e) { showError(e); }
    }
    async function deleteMCP(m) {
      if (m.builtin) return;
      if (!confirm('删除 MCP 服务「' + (m.display_name || m.server_key) + '」？')) return;
      try { await api('DELETE', '/api/agent/mcp/' + m.id); await loadMCP(); }
      catch (e) { showError(e); }
    }

    // ---------- 设置：技能 Skills（全局；从 skills.sh 搜索/手动 GitHub 路径安装，落盘热生效） ----------
    // 后端契约：GET /api/agent/skills → {skills:[AgentSkill]}；GET /skills/search?q= → {skills:[{id,name,installs,source}]}；
    // POST /skills/install {source:'owner/repo/skill'}；PATCH /skills/{id} {enabled}；DELETE /skills/{id}。
    // 安装/启禁/删除都是磁盘操作，dsh skills 插件热加载，下一轮对话即生效。
    const agentSkills = ref([]);
    const skillSearchQ = ref('');
    const skillResults = ref([]);
    const skillSearching = ref(false);
    const skillManual = ref('');
    const skillErr = ref('');
    const skillView = ref(null); // 展开查看的技能（含 content）
    async function loadSkills() {
      try { const d = await api('GET', '/api/agent/skills'); agentSkills.value = (d && d.skills) || []; }
      catch (e) { showError(e); }
    }
    async function searchSkills() {
      const q = (skillSearchQ.value || '').trim();
      if (!q) return;
      skillSearching.value = true; skillErr.value = '';
      try { const d = await api('GET', '/api/agent/skills/search?q=' + encodeURIComponent(q)); skillResults.value = (d && d.skills) || []; }
      catch (e) { skillErr.value = (e && e.message) || String(e); }
      finally { skillSearching.value = false; }
    }
    async function installSkill(source) {
      skillErr.value = '';
      try { await api('POST', '/api/agent/skills/install', { source }); await loadSkills(); }
      catch (e) { skillErr.value = (e && e.message) || String(e); }
    }
    async function toggleSkill(s) {
      try { await api('PATCH', '/api/agent/skills/' + s.id, { enabled: !s.enabled }); await loadSkills(); }
      catch (e) { showError(e); }
    }
    async function deleteSkill(s) {
      if (!confirm('删除技能「' + s.name + '」？')) return;
      try { await api('DELETE', '/api/agent/skills/' + s.id); skillView.value = null; await loadSkills(); }
      catch (e) { showError(e); }
    }

    // ---------- 报告（日报/周报 + 话题状态；后端 internal/api/review.go + internal/review/types.go） ----------
    // 契约（读源确认）：
    //   日报  GET  /api/reviews/daily?date=YYYY-MM-DD       → DailyReview 行 {content, status, review_date, created_at}
    //         POST /api/reviews/daily/generate {date}       强制重生成
    //   周报  GET  /api/reviews/weekly?week_start=YYYY-MM-DD → WeeklyReview 行 {content, status, week_start, week_end, created_at}
    //         POST /api/reviews/weekly/generate {week_start} 强制重生成
    //   话题状态 GET /api/topics/{id}/status[?refresh=1]      → TopicStatus 行 {content, generated_at}
    // content 为 *json.RawMessage（omitempty）：有则是对象，无则整个字段缺失（JS 里 undefined）。
    // DailyContent:  headline / highlights[] / decisions[] / todos{new,done,open} / insights[] / tomorrow[] / topic_distribution[{topic,count}]
    // WeeklyContent: headline / by_topic[{topic,progress,key_events[],open_todos[],risks[]}] / trends[{metric,labels?,series[]}] / risks[] / next_week[]
    // TopicStatus:   summary / progress / milestones[] / decisions[] / open_todos[] / risks[{desc,severity}] / blockers[]
    // 失败语义：LLM/解析失败 handler 返回 502（api() 抛错→reportError）；日报/周报另落一行 status='failed'。
    // 故 UI 双保险：抛错、或 status==='failed'、或 content 为空 → 友好「生成失败，请重试」。所有数组字段兜底 []。
    const reportKind = ref('daily');                    // 'daily' | 'weekly'
    const reportDate = ref(fmtDate(new Date()));        // 日报日期 YYYY-MM-DD
    const reportWeekStart = ref(fmtDate(thisMonday())); // 周报周起始（默认本周一）
    const reportRow = ref(null);                        // 当前报告行（含 content/status）
    const reportLoading = ref(false);
    const reportError = ref('');                        // 502/网络等抛错信息

    // 本周一 00:00（周报默认周起始；与后端 mondayOf 一致：周一为周首）。函数声明，供上面 ref 初始化时提升引用。
    function thisMonday() {
      const d = new Date(); d.setHours(0, 0, 0, 0);
      d.setDate(d.getDate() - ((d.getDay() + 6) % 7));
      return d;
    }
    // content 为对象或 undefined → 统一成对象或 null
    const reportContent = computed(() => (reportRow.value && reportRow.value.content) || null);
    // 失败判定：加载中不算失败；有抛错 / 状态 failed / 有行但无 content → 失败
    const reportFailed = computed(() => {
      if (reportLoading.value) return false;
      if (reportError.value) return true;
      const r = reportRow.value;
      if (!r) return false;
      return r.status === 'failed' || !r.content;
    });
    async function loadReport() {
      // 先清 reportRow：切类型/刷新时先落到骨架，避免用旧类型 content 渲染新类型模板
      reportLoading.value = true; reportError.value = ''; reportRow.value = null;
      try {
        const url = reportKind.value === 'daily'
          ? '/api/reviews/daily?date=' + encodeURIComponent(reportDate.value)
          : '/api/reviews/weekly?week_start=' + encodeURIComponent(reportWeekStart.value);
        reportRow.value = await api('GET', url);
      } catch (e) { reportError.value = (e && e.message) || String(e); }
      finally { reportLoading.value = false; }
    }
    async function regenReport() {
      if (reportLoading.value) return;
      reportLoading.value = true; reportError.value = ''; reportRow.value = null;
      try {
        const [url, body] = reportKind.value === 'daily'
          ? ['/api/reviews/daily/generate', { date: reportDate.value }]
          : ['/api/reviews/weekly/generate', { week_start: reportWeekStart.value }];
        reportRow.value = await api('POST', url, body);
        toast.value = '报告已重新生成'; setTimeout(() => { toast.value = ''; }, 2000);
      } catch (e) { reportError.value = (e && e.message) || String(e); }
      finally { reportLoading.value = false; }
    }
    // 日报/周报切换：切类型即重载（loadReport 内部先清 reportRow）
    function switchReportKind(k) { if (reportKind.value === k) return; reportKind.value = k; loadReport(); }

    // 周报趋势图：把每条 trend {metric, labels?, series[]} 预计算成 SVG 就绪坐标（模板只读，不重复计算）。
    const weeklyCharts = computed(() => {
      const c = reportContent.value;
      if (!c || !Array.isArray(c.trends)) return [];
      return c.trends.map(tr => ({ metric: tr.metric, geom: chartGeom(tr.series, tr.labels) }));
    });
    // chartGeom：等距 x、0..max 线性 y 的折线图坐标。viewBox 固定坐标系，svg 宽 100% 自适应容器。
    function chartGeom(series, labels) {
      const vals = (Array.isArray(series) ? series : []).map(v => Number(v) || 0);
      const n = vals.length;
      const W = 520, H = 150, padL = 30, padR = 14, padT = 14, padB = 26;
      const innerW = W - padL - padR, innerH = H - padT - padB;
      // 有符号值域：下界 min(0,…)、上界 max(0,…)，使 0 始终在域内——情绪 valence(−1..1) 等
      // 负值指标不会掉出画布/基线下（评审 I1）。非负数据下 lo=0，映射与旧版一致（0 线在底部）。
      const lo = Math.min(0, ...(n ? vals : [0]));
      const hi = Math.max(0, ...(n ? vals : [0]));
      let span = hi - lo; if (span === 0) span = 1;        // 避免除 0（全 0 数据）
      const baselineY = padT + innerH;                     // 画布底边（x 轴标签基线）
      const xAt = i => n <= 1 ? padL + innerW / 2 : padL + innerW * i / (n - 1);
      const yAt = v => padT + innerH * (hi - v) / span;    // v=hi→顶, v=lo→底
      const zeroY = +yAt(0).toFixed(1);                    // 值 0 的 y（0 参考线画这里）
      const pts = vals.map((v, i) => ({
        x: +xAt(i).toFixed(1), y: +yAt(v).toFixed(1), v,
        label: shortLabel(labels && labels[i] != null ? String(labels[i]) : String(i + 1)),
      }));
      return {
        W, H, padL, padT, padB, baselineY, zeroY, max: hi, min: lo,
        maxLabel: fmtNum(hi), minLabel: fmtNum(lo),
        pts, polyline: pts.map(p => p.x + ',' + p.y).join(' '),
      };
    }
    // 数值格式化：整数原样，小数保留 1 位（趋势值 / y 轴标注）
    function fmtNum(v) { const n = Number(v) || 0; return Number.isInteger(n) ? String(n) : n.toFixed(1); }
    // x 轴标签精简：YYYY-MM-DD → MM-DD，其余原样（7 点日期串防过挤）
    function shortLabel(s) { return /^\d{4}-\d{2}-\d{2}/.test(s) ? s.slice(5, 10) : String(s); }
    // 0..1 → 百分比整数（进度条宽度 + 文字）
    function pct(x) { return Math.round((Number(x) || 0) * 100); }
    // 迷你条形缩放基准：一组 {count} 里的最大计数（至少 1，避免除 0）
    function maxCount(arr) { return Math.max(1, ...(arr || []).map(t => Number(t && t.count) || 0)); }
    // 风险严重度 → 中文标签 + 徽标类（枚举 low|medium|high，其余原样降级到 low 样式）
    function sevMeta(sev) {
      return ({
        high: { label: '高', cls: 'sev-high' },
        medium: { label: '中', cls: 'sev-medium' },
        low: { label: '低', cls: 'sev-low' },
      })[sev] || { label: sev || '—', cls: 'sev-low' };
    }

    // 话题状态：选主题 → GET /api/topics/{id}/status（refresh=1 强制重算）。topic_status 行无 status 列，只按 content 判空。
    const statusTopicId = ref('');
    const topicStatusRow = ref(null);
    const topicStatusLoading = ref(false);
    const topicStatusError = ref('');
    const topicStatusContent = computed(() => (topicStatusRow.value && topicStatusRow.value.content) || null);
    const topicStatusFailed = computed(() => {
      if (topicStatusLoading.value) return false;
      if (topicStatusError.value) return true;
      const r = topicStatusRow.value;
      return !!(r && !r.content);
    });
    async function loadTopicStatus(refresh) {
      if (!statusTopicId.value) { topicStatusRow.value = null; return; }
      topicStatusLoading.value = true; topicStatusError.value = ''; topicStatusRow.value = null;
      try {
        const url = '/api/topics/' + statusTopicId.value + '/status' + (refresh ? '?refresh=1' : '');
        topicStatusRow.value = await api('GET', url);
      } catch (e) { topicStatusError.value = (e && e.message) || String(e); }
      finally { topicStatusLoading.value = false; }
    }
    // 选主题：清旧结果并按最新快照加载（无快照则后端现算）
    function onPickStatusTopic() { topicStatusRow.value = null; topicStatusError.value = ''; loadTopicStatus(false); }

    // ---------- 主题/人物/声纹 搜索（客户端过滤，与记忆 tab 同模式） ----------
    // 主题：名称 + 描述；人物：显示名；声纹：说话人名称。
    const topicSearch = ref('');
    const filteredTopics = computed(() => {
      const q = topicSearch.value.trim().toLowerCase();
      return topics.value.filter(t => !q || ((t.name || '') + ' ' + (t.description || '')).toLowerCase().includes(q));
    });
    const personSearch = ref('');
    const filteredPersons = computed(() => {
      const q = personSearch.value.trim().toLowerCase();
      return persons.value.filter(p => !q || (p.display_name || '').toLowerCase().includes(q));
    });
    const speakerSearch = ref('');
    const filteredSpeakers = computed(() => {
      const q = speakerSearch.value.trim().toLowerCase();
      return allSpeakers.value.filter(sp => !q || (sp.name || '').toLowerCase().includes(q));
    });
    // ---------- 标签页切换 ----------
    function switchTab(name) {
      const prev = tab.value;
      tab.value = name;
      try { localStorage.setItem(LS_TAB, name); } catch (e) {} // 记住所在 tab，刷新后回到现场
      // 离开问知微：断开 WS（含抑制重连），避免后台常驻连接
      if (prev === 'agent' && name !== 'agent') closeAgentWS();
      if (name === 'timeline') { deletingSessionId.value = null; reextractConfirmId.value = null; segDraft.value = {}; loadSessions(); loadAllSpeakers(); }
      if (name === 'memories') { memSearch.value = ''; loadMemories(); }
      if (name === 'topics') { topicDetail.value = null; renaming.value = null; deletingTopicId.value = null; dismissingTopicId.value = null; topicSearch.value = ''; cancelManualMerge(); loadDismissedTopics(); loadTopics(); }
      if (name === 'todos') { editingTodo.value = null; deletingTodoId.value = null; dismissingTodoId.value = null; todoSel.clear(); todoBatchAsk.value = null; todoMultiMode.value = false; todoSearch.value = ''; loadTopics(); loadTodos(); loadDismissedTodos(); }
      // 声纹 tab：进入时复位本 tab 的临时态（收起录入表单/展开项/改名/播放）并拉全量名册。
      if (name === 'voiceprint') { showEnrollForm.value = false; expandedSpeakerId.value = null; speakerSegments.value = []; renamingSpeaker.value = null; playingSegId.value = null; speakerSearch.value = ''; loadAllSpeakers(); }
      // 问知微 tab：拉会话列表；若已有选中会话，重拉历史 + 重连 WS（切回时恢复现场）。
      if (name === 'agent') { loadAgentConversations(); if (agentConvId.value) { const cid = agentConvId.value; loadAgentHistory(cid); openAgentWS(cid); } }
      // 设置 tab：拉当前人设（identity/soul）到表单。
      if (name === 'settings') { loadAgentConfig(); loadMCP(); loadSkills(); }
      // 报告 tab：拉主题列表（话题状态选择器数据源）+ 按当前日报/周报类型加载报告。
      if (name === 'reports') { loadTopics(); loadReport(); }
      // 人物 tab：进入时复位详情/删除确认态，拉名册 + 已删除列表 + 确认队列（跨平面 pending 并集，独立刷新）+ 属性目录（受控输入元数据，懒加载缓存）。
      if (name === 'persons') { closePersonDetail(); deletingPersonId.value = null; personSearch.value = ''; loadPersons(); loadDeletedPersons(); loadPending(); loadAttrCatalog(); }
    }
    // 启动先校验登录态（登录门）：GET /api/auth/me → 200 进主界面并加载首屏数据（bootMainData）；
    // 401 → api() 已置 authed=false，显示登录页。未登录时不再盲发 sessions/topics/speakers 等请求。
    checkAuth();

    onUnmounted(() => { clearInterval(recTimer); clearInterval(pollTimer); clearTimeout(reextractPollTimer); if (agentTimer) clearInterval(agentTimer); closeAgentWS(); });

    return {
      tab, toast, switchTab,
      // 登录门（cookie + session 鉴权）
      authed, currentUser, loginForm, loginError, loggingIn, submitLogin, logout,
      fmtTime, fmtDue, typeMeta, statusText, todoStatusText, spClass, envSounds,
      profilePlaneMeta, profileChangeAction, fmtChangeSummary, profileChangeGroups, goProfilePending,
      sessions, detail, expandedId, loadSessions, toggleSession, reloadSession, audioUrl, dismissingMemId, askDismissMem, cancelDismissMem, confirmDismissMem, retryJob, editingMem, startEditMemory, cancelEditMemory, saveEditMemory, deletingSessionId, askDeleteSession, cancelDeleteSession, confirmDeleteSession,
      tlSearch, tlDateFrom, tlDateTo, tlPreset, clearTlFilter, applyPreset, filteredSessions, sessionsByDay, detailInsights,
      segDraft, segEditing, startEditSeg, cancelEditSeg, saveSegEdit, segDirty, saveTranscript, rawAsrView, toggleRawAsr,
      sessionAudioEl, tlPlayingSegId, toggleTimelineSegPlay, onTimelineAudioTimeUpdate,
      mergeMode, mergeSelected, mergeCount, mergeTarget, enterMergeMode, cancelMerge, toggleMergeSelect, confirmMerge,
      MIN_ENROLL_MS, segEnrollId, segEnrollName, segDurMs, canEnrollSeg, startSegEnroll, cancelSegEnroll, confirmSegEnroll,
      speakerFilter, renamingSpeaker, enrollOpen, enrollForm, enrolling, allSpeakers,
      speakerColor, segSpeakerBg, toggleSpeakerFilter, openEnroll, onEnrollDrop, submitEnroll, loadAllSpeakers,
      startEnrollRec, stopEnrollRec, enrollRecording, enrollRecSeconds, enrollPromptText,
      startRenameSpeaker, commitRenameSpeaker, askDeleteSpeaker, reassignSegment,
      addEmbNotes, addingEmbId, editingEmbNote, deletingEmbId, embFileInput, embSourceText,
      triggerAddEmb, onAddEmbFile, startEditEmbNote, commitEmbNote, confirmDeleteEmb,
      switchingSpeaker, switchTarget, switchSegCount, startSwitchSpeaker, cancelSwitchSpeaker, commitSwitchSpeaker,
      hasNameCandidates, acceptNameCandidate, dismissNameCandidate,
      showEnrollForm, toggleEnrollForm, expandedSpeakerId, speakerSegments, speakerSegLoading, playingSegId, voiceAudioEl, toggleSpeakerSegments, speakerSegmentsBySession, playSpeakerSegment, onVoiceAudioTimeUpdate, fmtSec,
      spMergeMode, spMergeSelected, spMergeConfirming, spMergeTarget, startSpMerge, cancelSpMerge, toggleSpSelect, startSpConfirm, applySpMerge,
      reextractingIds, reextractConfirmId, askReextract, cancelReextract, confirmReextract,
      reidentifyingIds, reidentifyConfirmId, askReidentify, cancelReidentify, confirmReidentify,
      recording, recSeconds, uploadInfo, startRec, stopRec, onDrop,
      lastAudioFile, matchInfo, voiceprintMatching, tryMatchVoiceprint,
      topics, topicDetail, showNewTopic, newTopic, creating, toggleNewTopic, cancelNewTopic, renaming,
      loadTopics, openTopic, reloadTopicDetail, closeTopicDetail, confirmTopic, startRename, commitRename, createTopic, suspectOf, mergeDraft, startConsolidate, consolidating, toggleMergeMember, applyMerge, deletingTopicId, askDeleteTopic, cancelDeleteTopic, confirmDeleteTopic, dismissingTopicId, askDismissTopic, cancelDismissTopic, confirmDismissTopic, restoreTopic, dismissedTopics, dismissedCollapsed, loadDismissedTopics,
      manualMergeMode, manualSelected, manualMergeName, manualConfirming, startManualMerge, cancelManualMerge, toggleManualSelect, applyManualMerge, startManualConfirm,
      memories, loadMemories, memoryDraft, startMemoryConsolidate, memConsolidating, toggleMemoryMember, toggleMemoryAdjustment, applyMemoryConsolidation,
      memSearch, memConfMin, filteredMemories,
      todoSearch, topicSearch, filteredTopics, personSearch, filteredPersons, speakerSearch, filteredSpeakers,
      todos, doneCollapsed, suggestedTodos, activeTodos, doneTodos, dismissedTodos, dismissedTodoCollapsed, loadDismissedTodos,
      loadTodos, setTodoStatus, jumpToSession,
      todoSel, todoBatchAsk, toggleTodoSel, todoGroupSel, toggleTodoGroupSel, batchTodoStatus, batchTodoDelete,
      todoMultiMode, toggleTodoMultiMode,
      editingTodo, startEditTodo, cancelEditTodo, saveEditTodo, deletingTodoId, askDeleteTodo, cancelDeleteTodo, confirmDeleteTodo, dismissingTodoId, askDismissTodo, cancelDismissTodo, confirmDismissTodo,
      topicChips, availableTopics, addTodoTopic, removeTodoTopic, addMemoryTopic, removeMemoryTopic,
      // 问知微（流式对话）
      agentConversations, agentConvId, agentEditConvId, agentEditTitle, deletingConvId, agentMessages, agentInput, agentConnected, agentTyping, agentTurnError, agentLoading, agentStreamEl,
      agentTurns, turnRunning, agentThinkingGap, turnDuration, fmtDur, isTurnOpen, toggleTurn, streamDraft,
      loadAgentConversations, newAgentConversation, selectAgentConversation, startEditConv, cancelEditConv, saveAgentTitle, askDeleteConv, cancelDeleteConv, deleteAgentConversation, sendAgentMessage, stopAgentMessage,
      agentCfgIdentity, agentCfgSoul, agentCfgSaving, agentCfgSaved, agentCfgPreview, agentCfgSystemPrompt, agentCfgOwnerHead, agentCfgFullPrompt, loadAgentConfig, saveAgentConfig,
      mcpServers, mcpForm, mcpErr, loadMCP, addMCP, toggleMCP, deleteMCP,
      agentSkills, skillSearchQ, skillResults, skillSearching, skillManual, skillErr, skillView, loadSkills, searchSkills, installSkill, toggleSkill, deleteSkill,
      confirmProposal, dismissProposal,
      renderMarkdown, reportSections, prettyJSON,
      // 报告（日报/周报 + 话题状态）
      reportKind, reportDate, reportWeekStart, reportRow, reportLoading, reportError, reportContent, reportFailed,
      loadReport, regenReport, switchReportKind, weeklyCharts, fmtNum, pct, maxCount, sevMeta,
      statusTopicId, topicStatusRow, topicStatusLoading, topicStatusError, topicStatusContent, topicStatusFailed,
      loadTopicStatus, onPickStatusTopic,
      // 人物 / 画像
      persons, personDetail, showNewPerson, newPerson, newPersonSpeakers, creatingPerson, loadPersons, cancelNewPerson, toggleNewPerson, createPerson, togglePerson, closePersonDetail, jumpToPerson, reloadPersonDetail, renamingPerson, startRenamePerson, commitRenamePerson, deletingPersonId, askDeletePerson, cancelDeletePerson, confirmDeletePerson, deletedPersons, deletedCollapsed, loadDeletedPersons, restorePerson,
      bindingSpeaker, bindingSaving, bindableSpeakers, startBindingSpeaker, commitBindingSpeaker,
      jumpToPerson, spPersonEdit, spPersonSaving, unboundPersons, startSpPersonEdit, commitSpPersonEdit,
      epiText, personNameOf,
      attrCatalog, attrDefOf, attrLabel, addAttrDef, editAttrDef, onAddAttrKeyChange, showAddAttr, addAttrForm, addingAttr, submitAddAttr, toggleAddAttr, editingAttr, startEditAttr, commitEditAttr, deletingAttrId, askDeleteAttr, confirmDeleteAttr, attrHistory, attrHistoryLoading, showAttrHistory, changeText, snapText,
      RELATION_TYPES, DIRECTIONS, showAddRel, addRelForm, addingRel, submitAddRel, toggleAddRel, resetAddRelForm, deletingRelId, askDeleteRel, confirmDeleteRel,
      EVENT_TYPES, EVENT_IMPORTANCE_LEVELS, eventRowStyle, showAddEvent, addEventForm, addingEvent, toggleAddEvent, submitAddEvent, eventsByYear, fmtEventDate, deletingEventId, askDeleteEvent, confirmDeleteEvent,
      METRIC_CATALOG, showAddMetric, addMetricForm, addingMetric, addMetricDef, onPickMetricKey, toggleAddMetric, submitAddMetric, metricCharts, metricPointValue, deletingMetricId, askDeleteMetric, confirmDeleteMetric,
      CYCLE_TYPES, healthOpen, cycles, cyclesNote, cycleLoading, toggleHealth, showAddCycle, addCycleForm, addingCycle, toggleAddCycle, submitAddCycle, deletingCycleId, askDeleteCycle, confirmDeleteCycle, cycleTypeLabel, fmtDateOnly,
      activities, activityRows, activityLoading, showAddActivity, addActivityForm, addingActivity, toggleAddActivity, submitAddActivity, deletingActivityId, askDeleteActivity, confirmDeleteActivity,
      PET_SPECIES, pets, petRows, petLoading, showAddPet, editingPetId, addPetForm, savingPet, toggleAddPet, startEditPet, submitPet, deletingPetId, askDeletePet, confirmDeletePet,
      pendingItems, pendingLoading, queueBusyIds, loadPending, refreshAfterQueue, confirmPendingItem, dismissPendingItem, pendingSummary, pendingKindText,
      backfilling, backfillInfo, runBackfill,
    };
  }
});
// v-focus：表单展开时自动聚焦输入框（v-if 挂载即触发 mounted）
app.directive('focus', { mounted: el => el.focus() });
// report-list：报告分节列表（小标题 + 字符串数组；空数组回退「（无）」）。
// 每项只用 Vue 文本插值 {{ it }} 渲染（自动转义，LLM 文本安全），复用报告页 CSS 类。
// 用于日报/周报/话题状态里多处「字符串数组」字段，避免模板重复。
app.component('report-list', {
  props: { label: { type: String, default: '' }, items: { type: Array, default: () => [] } },
  template: `<div class="report-sec">
    <div class="report-sec-title">{{ label }}</div>
    <ul v-if="items && items.length" class="report-ul"><li v-for="(it, i) in items" :key="i">{{ it }}</li></ul>
    <div v-else class="muted">（无）</div>
  </div>`,
});
// report-text：报告分节单段文本（小标题 + 一段叙事文字；空文本回退「（无）」）。
// 与 report-list 配套，用于 narrative 这类「单段字符串」深度字段（report-list 只接数组，不适用）。
app.component('report-text', {
  props: { label: { type: String, default: '' }, text: { type: String, default: '' } },
  template: `<div class="report-sec"><div class="report-sec-title">{{ label }}</div><p v-if="text" style="margin:0; line-height:1.7">{{ text }}</p><div v-else class="muted">（无）</div></div>`,
});
app.mount('#app');
