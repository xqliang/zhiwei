package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/auth"
	"zhiwei/internal/entity"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/search"
)

// AgentHandler 提供对话 REST（本期非流式；WS 流式见 P2c）。
type AgentHandler struct {
	Orch          *Orchestrator
	Conversations *repo.AgentConversationRepo
	Messages      *repo.AgentMessageRepo
	Configs       *repo.AgentConfigRepo                                            // 人设配置（identity/soul，全局单份）；nil 时人设端点返回空/不可写
	SystemPrompt  string                                                           // 进程级 system prompt（DSH_SYSTEM_PROMPT/persona，只读展示用）
	Ctx           *ProfileContext                                                  // 与 orchestrator 同一份：getConfig 据此算 owner 画像头（动态注入预览）
	Hub           *turnHub                                                         // 每会话轮次广播器（nil 时由 RegisterAgent 惰性初始化）
	Gen           func(ctx context.Context, uid int64, cid ids.ID) (string, error) // 手动生成标题（nil 时端点 503）
	// MCPServers 全局 MCP 服务清单（设置页管理）；nil 时 MCP 端点返回 503（管理面未装配的降级）。
	MCPServers *repo.MCPServerRepo
	// OnMCPChange 在任一 MCP 写操作成功后调用一次（重生成 cordis + 对在用运行时热插拔下发）；
	// nil 时只落库不生效（下次进程重启才读新配置）。
	OnMCPChange    func(ctx context.Context)
	Skills         *repo.AgentSkillRepo     // 已装技能元数据；nil 时技能端点 503
	SkillInst      *SkillInstaller          // 安装器（tarball/搜索代理 + 磁盘根）；nil 时技能端点 503
	EntityKB       *repo.EntityKBRepo       // 实体知识库（设置页「专有名词」manual 条目）；nil 时实体端点 503
	EntitySettings *repo.EntitySettingsRepo // 实体纠错配置；nil 时 503
	EntitySeed     entity.SeedDeps          // 实时聚合依赖（person/pet/speaker/topic 来源 repo）；auto 实体列表用
	EntityDisabled *repo.EntityDisabledRepo // 禁用名单（自动实体持久停用）；nil 时 auto 无禁用态
}

// RegisterAgent 挂载 /api/agent 路由。
func RegisterAgent(r chi.Router, h *AgentHandler) {
	if h.Hub == nil {
		h.Hub = newTurnHub() // 生产/测试都经此入口，main.go 用结构体字面量构造无需感知内部 hub 类型
	}
	r.Get("/api/agent/config", h.getConfig) // 查看人设（identity/soul + 组装预览）
	r.Put("/api/agent/config", h.putConfig) // 保存人设（每轮注入，下一条消息即时生效，不重启 dsh）
	r.Post("/api/agent/conversations", h.createConversation)
	r.Get("/api/agent/conversations", h.listConversations)
	r.Get("/api/agent/conversations/{cid}", h.getConversation)
	r.Patch("/api/agent/conversations/{cid}", h.patchConversation)
	r.Delete("/api/agent/conversations/{cid}", h.deleteConversation)
	r.Post("/api/agent/conversations/{cid}/title/generate", h.generateTitle)
	r.Post("/api/agent/conversations/{cid}/messages", h.postMessage)
	r.Get("/api/agent/conversations/{cid}/ws", h.handleWS) // WS 流式（上行发消息 + 下行流式帧）
	r.Get("/api/agent/mcp", h.listMCP)                     // MCP 服务清单（全局，设置页管理）
	r.Post("/api/agent/mcp", h.createMCP)                  // 新增（手动添加；触发生效）
	r.Put("/api/agent/mcp/{id}", h.updateMCP)              // 编辑（内置行仅 display_name）
	r.Patch("/api/agent/mcp/{id}", h.patchMCP)             // 启/禁（内置禁用被拒）
	r.Delete("/api/agent/mcp/{id}", h.deleteMCP)           // 删除（内置拒删）
	r.Get("/api/agent/skills", h.listSkills)               // 技能清单（已装）
	r.Get("/api/agent/skills/search", h.searchSkills)      // skills.sh 搜索代理（在 /{id} 前注册）
	r.Post("/api/agent/skills/install", h.installSkill)    // 安装（owner/repo/skill）
	r.Get("/api/agent/skills/{id}", h.getSkill)
	r.Patch("/api/agent/skills/{id}", h.patchSkill) // 启禁（目录 rename 热生效）
	r.Delete("/api/agent/skills/{id}", h.deleteSkill)
	registerEntityRoutes(r, h) // 专有名词：纠错配置 + 手动实体 CRUD（设置页）
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// reqUserID 从请求 ctx 取已鉴权用户 id（authGate 中间件注入）。未注入返回 ok=false，调用方据此
// 401——这些端点生产中都在 authGate 保护内、理论必有 uid；防御性显式取，杜绝「未鉴权却按某个
// 写死用户操作」的越权（2B-B：多用户隔离，一切读写都须绑定当前登录用户）。
func reqUserID(r *http.Request) (int64, bool) {
	id, ok := auth.UserID(r.Context())
	return id.Int64(), ok
}

// getConfig 返回全局人设（identity/soul）+ 组装预览，以及只读的整体 prompt 组成：
// system_prompt（进程级 persona，不可编辑）、datetime_head（每轮无条件注入的「当前日期+时区」，
// 动态）、owner_head（每轮注入的 owner 画像头，动态）。
// 注意：整体 prompt 预览须与 orchestrator.runTurn 的实际注入保持一致——今后任何注入内容/顺序调整，
// 都要同步更新这里的字段与前端 agentCfgFullPrompt 的拼装。
func (h *AgentHandler) getConfig(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	resp := map[string]any{"identity": "", "soul": "", "preview": "", "system_prompt": h.SystemPrompt, "search_engine": "auto", "search_api_key": ""}
	if h.Configs != nil {
		c, err := h.Configs.Get(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		resp["identity"], resp["soul"] = c.Identity, c.Soul
		resp["preview"] = AssemblePersona(c.Identity, c.Soul)
		resp["updated_at"] = c.UpdatedAt
		resp["search_engine"] = c.SearchEngine
		if resp["search_engine"] == "" {
			resp["search_engine"] = "auto"
		}
		resp["search_api_key"] = c.SearchKey()
	}
	// 当前日期 + 时区：每轮无条件注入（不依赖 owner），动态计算——预览也每次取当前值。
	resp["datetime_head"] = DateTimeHead(time.Now())
	// owner 画像头（每轮动态注入的背景；无 Ctx/owner/数据时为空串）——供「整体 prompt」只读预览。
	if h.Ctx != nil {
		resp["owner_head"] = h.Ctx.Head(r.Context(), uid, time.Now())
	}
	writeJSON(w, http.StatusOK, resp)
}

// putConfig 保存全局配置。Phase 2 起为指针合并语义：body 里未传（缺省/为 null）的字段
// 保持原值，传了的字段才覆盖——设置页「人设」与「联网搜索」两张卡各自只传自己的字段。
// 每轮注入，下一条消息即时生效（不重启 dsh）。
func (h *AgentHandler) putConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := reqUserID(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.Configs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "人设配置不可用"})
		return
	}
	var body struct {
		Identity     *string `json:"identity"`
		Soul         *string `json:"soul"`
		SearchEngine *string `json:"search_engine"`
		SearchAPIKey *string `json:"search_api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	// 读现值做合并基底（无行时零值：identity/soul 空、engine 由 repo 归一 auto）。
	cur, err := h.Configs.Get(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cfg := repo.AgentConfig{Identity: cur.Identity, Soul: cur.Soul, SearchAPIKey: cur.SearchAPIKey}
	if body.Identity != nil {
		cfg.Identity = *body.Identity
	}
	if body.Soul != nil {
		cfg.Soul = *body.Soul
	}
	if body.SearchEngine != nil {
		e := strings.TrimSpace(*body.SearchEngine)
		if !search.ValidEngine(e) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法搜索引擎: " + e})
			return
		}
		cfg.SearchEngine = e
	}
	if body.SearchAPIKey != nil {
		k := strings.TrimSpace(*body.SearchAPIKey)
		if k == "" {
			cfg.SearchAPIKey = nil // 清空 key 存 NULL
		} else {
			cfg.SearchAPIKey = &k
		}
	}
	if err := h.Configs.Upsert(r.Context(), cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"identity": cfg.Identity, "soul": cfg.Soul,
		"preview":        AssemblePersona(cfg.Identity, cfg.Soul),
		"search_engine":  cfg.SearchEngine,
		"search_api_key": cfg.SearchKey(),
	})
}

func (h *AgentHandler) createConversation(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	c := &repo.AgentConversation{Title: body.Title, UserID: uid}
	if err := h.Conversations.Create(r.Context(), c); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// I2：Create 不回填 DB 默认列（status/created_at/last_active_at），直接返回 c 会带出
	// 空 status 和零值时间戳。读回完整行再响应，保证前端拿到 active + 真实时间。
	if full, err := h.Conversations.Get(r.Context(), uid, c.ID); err == nil {
		c = full
	}
	writeJSON(w, 200, c)
}

func (h *AgentHandler) listConversations(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	list, err := h.Conversations.List(r.Context(), uid)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, list)
}

func (h *AgentHandler) getConversation(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid cid"})
		return
	}
	msgs, err := h.Messages.ListByConversation(r.Context(), uid, cid)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"conversation_id": cid, "messages": msgs})
}

// patchConversation 手动改标题：写 title_source=manual。越权/不存在 → 404。
func (h *AgentHandler) patchConversation(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cid"})
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title required"})
		return
	}
	if err := h.Conversations.UpdateTitle(r.Context(), uid, cid, title, titleSourceManual); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	full, err := h.Conversations.Get(r.Context(), uid, cid)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	writeJSON(w, http.StatusOK, full)
}

// deleteConversation 软删除：status→archived。幂等（已归档也 204）。越权/不存在 → 404。
func (h *AgentHandler) deleteConversation(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cid"})
		return
	}
	// 先确认存在且归属当前用户（Archive 幂等不报错，越权需显式 404）。
	if _, err := h.Conversations.Get(r.Context(), uid, cid); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if err := h.Conversations.Archive(r.Context(), uid, cid); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// generateTitle 手动触发一次自动生成（兜底）。装配了 Gen 才可用，否则 503。
func (h *AgentHandler) generateTitle(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cid"})
		return
	}
	if h.Gen == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "标题生成不可用"})
		return
	}
	title, err := h.Gen(r.Context(), uid, cid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"title": title, "title_source": titleSourceAuto})
}

func (h *AgentHandler) postMessage(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid cid"})
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "text required"})
		return
	}
	conv, err := h.Conversations.Get(r.Context(), uid, cid)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "conversation not found"})
		return
	}
	final, err := h.Orch.RunTurn(r.Context(), conv, body.Text)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "assistant": final})
		return
	}
	writeJSON(w, 200, final)
}
