package agent

// 技能安装器：从 GitHub（codeload tarball）拉取 skills/<name>/ 整目录落盘到
// <skillRoot>/enabled/<name>/（dsh skills 插件 chokidar 监听该根，落盘即热生效）；
// Search 代理 skills.sh 检索 API（后端代理避免前端跨域）。规格见 skill-management spec §6。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"zhiwei/internal/repo"
)

// skillNameRe dsh 强制的技能名格式（kebab-case，dsh-skill isSkillName）。
var skillNameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// maxTarBytes 单次 tarball 拉取/解包上限（防恶意大仓库）。
const maxTarBytes = 64 << 20 // 64MB

// SkillSearchHit 是 skills.sh /api/search 的单条结果（原样透传给前端）。
type SkillSearchHit struct {
	ID       string `json:"id"` // 'owner/repo/skill'
	Name     string `json:"name"`
	Installs int64  `json:"installs"`
	Source   string `json:"source"`
}

// SkillInstaller 拉取/落盘技能。codeloadBase 与 searchBase 生产是官方地址，测试注入 httptest。
type SkillInstaller struct {
	codeloadBase string
	searchBase   string
	skillRoot    string
	httpClient   *http.Client
}

// NewSkillInstaller 构造。skillRoot 为 AgentSkillRoot（enabled/ 与 disabled/ 的父目录）。
func NewSkillInstaller(codeloadBase, searchBase, skillRoot string) *SkillInstaller {
	return &SkillInstaller{
		codeloadBase: strings.TrimRight(codeloadBase, "/"),
		searchBase:   strings.TrimRight(searchBase, "/"),
		skillRoot:    skillRoot,
		httpClient:   &http.Client{Timeout: 60 * time.Second},
	}
}

// EnabledDir / DisabledDir 返回启/禁子目录路径。
func (si *SkillInstaller) EnabledDir() string  { return filepath.Join(si.skillRoot, "enabled") }
func (si *SkillInstaller) DisabledDir() string { return filepath.Join(si.skillRoot, "disabled") }

// Install 从 source（'owner/repo/skill'）拉取并安装。原子性：先解到 .tmp-<rand>/ 校验，
// 通过后 rename 进 enabled/<name>/；任何失败清理暂存。返回待落库的元数据（调用方负责 Create DB 行）。
func (si *SkillInstaller) Install(ctx context.Context, source string) (*repo.AgentSkill, error) {
	owner, repoName, skillName, err := parseSource(source)
	if err != nil {
		return nil, err
	}
	if !skillNameRe.MatchString(skillName) {
		return nil, fmt.Errorf("技能名 %q 须为 kebab-case（小写字母/数字/连字符）", skillName)
	}
	for _, dir := range []string{si.EnabledDir(), si.DisabledDir()} {
		if _, err := os.Stat(filepath.Join(dir, skillName)); err == nil {
			return nil, fmt.Errorf("技能 %s 已安装（请先删除再重装）", skillName)
		}
	}

	body, err := si.fetchTarball(ctx, owner, repoName)
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp(si.skillRoot, ".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	skillMeta, err := si.extractSkill(body, skillName, tmp)
	if err != nil {
		return nil, err
	}
	skillMeta.Source = source

	dst := filepath.Join(si.EnabledDir(), skillName)
	if err := os.MkdirAll(si.EnabledDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(tmp, skillName), dst); err != nil {
		return nil, fmt.Errorf("落位技能目录: %w", err)
	}
	return skillMeta, nil
}

// parseSource 解析 'owner/repo/skill' 三段。
func parseSource(s string) (owner, repoName, skill string, err error) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("source 须为 owner/repo/skill 形式，got %q", s)
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, " .:@\\") {
			return "", "", "", fmt.Errorf("source 段非法: %q", s)
		}
	}
	return parts[0], parts[1], parts[2], nil
}

// fetchTarball 拉 repo tarball：默认分支 main，404 回退 master。
func (si *SkillInstaller) fetchTarball(ctx context.Context, owner, repoName string) ([]byte, error) {
	for _, branch := range []string{"main", "master"} {
		u := fmt.Sprintf("%s/%s/%s/tar.gz/refs/heads/%s", si.codeloadBase, owner, repoName, branch)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := si.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("拉取 %s/%s: %w", owner, repoName, err)
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("拉取 %s/%s: HTTP %d", owner, repoName, resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxTarBytes+1))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > maxTarBytes {
			return nil, fmt.Errorf("tarball 超过 %dMB 上限", maxTarBytes>>20)
		}
		return body, nil
	}
	return nil, fmt.Errorf("仓库 %s/%s 的 main/master 分支均不可达", owner, repoName)
}

// extractSkill 流式解 tar，取 */skills/<skillName>/ 子树写入 tmp/<skillName>/ 并校验 SKILL.md。
// 防穿越：在「原始」条目名（未 Clean，否则 ../ 会被折叠掉）上匹配 skills/<name>/ 前缀，取其后的
// 相对路径 rel；若 Clean(rel) 逃逸（以 .. 开头）则整包拒绝——不是静默跳过——以让穿越型 tarball
// 明确安装失败并清理暂存。symlink/hardlink 条目跳过。
func (si *SkillInstaller) extractSkill(tarball []byte, skillName, tmp string) (*repo.AgentSkill, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, fmt.Errorf("解 gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	dstBase := filepath.Join(tmp, skillName)
	marker := "skills/" + skillName + "/"
	var prefix string // 仓库根前缀（首条命中固定，防跨根混入）
	var prefixSet bool
	var totalWritten int64
	var skillMD []byte

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读 tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			continue
		}
		// 在原始名上定位 skills/<name>/ 前缀（保留 ../ 以便下面识别越界）。
		raw := hdr.Name
		i := strings.Index(raw, marker)
		if i < 0 {
			continue
		}
		root := raw[:i]
		if !prefixSet {
			prefix = root
			prefixSet = true
		} else if root != prefix {
			continue
		}
		rel := raw[i+len(marker):]
		cleanRel := filepath.Clean(rel)
		// rel 非空且 Clean 后逃逸（== ".." 或以 "../" 开头）→ 穿越，整包拒绝。
		if rel != "" && (cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator))) {
			return nil, fmt.Errorf("tar 条目路径越界: %s", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(filepath.Join(dstBase, cleanRel), 0o755); err != nil {
				return nil, err
			}
			continue
		}
		totalWritten += hdr.Size
		if totalWritten > maxTarBytes {
			return nil, fmt.Errorf("技能解包超 %dMB 上限", maxTarBytes>>20)
		}
		out := filepath.Join(dstBase, cleanRel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxTarBytes+1))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return nil, err
		}
		if cleanRel == "SKILL.md" {
			skillMD = data
		}
	}
	if skillMD == nil {
		return nil, fmt.Errorf("仓库里没有 skills/%s/SKILL.md", skillName)
	}
	fmName, fmDesc, err := parseFrontmatter(string(skillMD))
	if err != nil {
		return nil, fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	if fmName != skillName {
		return nil, fmt.Errorf("frontmatter name %q 与目录名 %q 不一致", fmName, skillName)
	}
	return &repo.AgentSkill{
		Name: fmName, DisplayName: fmName, Description: fmDesc,
		Content: string(skillMD), Enabled: true,
	}, nil
}

// parseFrontmatter 简易解析 SKILL.md 头部 YAML（只需 name/description 两行，兼容引号包裹）。
func parseFrontmatter(md string) (name, description string, err error) {
	s := strings.TrimSpace(md)
	if !strings.HasPrefix(s, "---") {
		return "", "", fmt.Errorf("缺 frontmatter 头 ---")
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("frontmatter 未闭合")
	}
	block := rest[:end]
	for _, line := range strings.Split(block, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "name":
			name = v
		case "description":
			description = v
		}
	}
	if name == "" || description == "" {
		return "", "", fmt.Errorf("frontmatter 缺 name 或 description")
	}
	return name, description, nil
}

// Search 代理 skills.sh /api/search?q=<kw>（10s 超时）。
func (si *SkillInstaller) Search(ctx context.Context, q string) ([]SkillSearchHit, error) {
	u := si.searchBase + "/api/search?q=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("搜索 skills.sh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索 skills.sh: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Skills []SkillSearchHit `json:"skills"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Skills, nil
}
