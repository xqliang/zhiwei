package agent

import (
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
)

// RegisterExtract 挂载 POST /api/agent/conversations/{cid}/extract：
// 从一段「问知微」对话抽取候选记忆并落库（幂等：按 conversation_id 先删后插，见 P3b 计划）。
// deps 由主服务装配（复用现有 repo + Ark LLM）。
//
// 用一把进程级互斥串行化所有抽取：单用户 MVP 下足够，避免同会话并发的
// delete+insert 事务相互打架（C 计划 §7 的「同 cid 抽取应串行」的最简实现）。
func RegisterExtract(r chi.Router, deps memory.ConversationExtractDeps) {
	var mu sync.Mutex
	r.Post("/api/agent/conversations/{cid}/extract", func(w http.ResponseWriter, req *http.Request) {
		cid, err := ids.ParseID(chi.URLParam(req, "cid"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cid"})
			return
		}
		mu.Lock()
		res, err := memory.ExtractConversation(req.Context(), deps, cid)
		mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
}
