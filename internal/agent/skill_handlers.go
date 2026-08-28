package agent

// 技能管理端点（/api/agent/skills*）：列表/详情只读 DB；安装走 SkillInstaller（tarball 落盘）；
// 启禁 = enabled↔disabled 目录 rename（dsh watcher 热生效）；删除 = 删目录 + 删行。
// 顺序约定「先磁盘后 DB」：rename 成功但 DB 更新失败时回滚 rename（管理操作低频，简单补偿）。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
)

func (h *AgentHandler) skillAvailable(w http.ResponseWriter) bool {
	if h.Skills == nil || h.SkillInst == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "技能管理不可用"})
		return false
	}
	return true
}

func (h *AgentHandler) listSkills(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	rows, err := h.Skills.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": rows})
}

func (h *AgentHandler) getSkill(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	s, err := h.Skills.Get(r.Context(), id)
	if err != nil {
		writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *AgentHandler) searchSkills(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q required"})
		return
	}
	hits, err := h.SkillInst.Search(r.Context(), q)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": hits})
}

func (h *AgentHandler) installSkill(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	var body struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if _, _, _, err := parseSource(body.Source); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s, err := h.SkillInst.Install(r.Context(), body.Source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := h.Skills.Create(r.Context(), s); err != nil {
		// DB 失败回滚磁盘（先磁盘后 DB 的补偿）
		_ = os.RemoveAll(filepath.Join(h.SkillInst.EnabledDir(), s.Name))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *AgentHandler) patchSkill(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	s, err := h.Skills.Get(r.Context(), id)
	if err != nil {
		writeSkillErr(w, err)
		return
	}
	if s.Enabled != body.Enabled {
		if err := h.SkillInst.renameSkill(s.Name, body.Enabled); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := h.Skills.SetEnabled(r.Context(), id, body.Enabled); err != nil {
			_ = h.SkillInst.renameSkill(s.Name, !body.Enabled)
			writeSkillErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AgentHandler) deleteSkill(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	s, err := h.Skills.Get(r.Context(), id)
	if err != nil {
		writeSkillErr(w, err)
		return
	}
	if err := h.SkillInst.removeSkill(s.Name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := h.Skills.Delete(r.Context(), id); err != nil {
		writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeSkillErr(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}
