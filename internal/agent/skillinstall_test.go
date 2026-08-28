package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTarGz 构造 codeload 风格 tarball（根目录 repo-main/，内含 skills/<name>/...）。
func buildTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for path, content := range entries {
		hdr := &tar.Header{Name: "repo-main/" + path, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const goodSKILL = "---\nname: test-skill\ndescription: 测试技能说明\n---\n\n# 使用说明\n正文。"

func newSkillInstaller(t *testing.T, tarball []byte, statusMain int) (*SkillInstaller, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/refs/heads/main") && statusMain != 0 {
			w.WriteHeader(statusMain)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	inst := NewSkillInstaller(srv.URL, srv.URL, root)
	return inst, root
}

func TestInstallSkillHappyPath(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"skills/test-skill/SKILL.md":     goodSKILL,
		"skills/test-skill/reference.md": "# 参考",
		"skills/other-skill/SKILL.md":    "---\nname: other-skill\ndescription: 别的\n---\n正文",
		"README.md":                      "# repo",
	})
	inst, root := newSkillInstaller(t, tarball, 0)
	s, err := inst.Install(context.Background(), "acme/repo/test-skill")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if s.Name != "test-skill" || s.Description != "测试技能说明" || !s.Enabled {
		t.Errorf("安装结果异常: %+v", s)
	}
	for _, f := range []string{"enabled/test-skill/SKILL.md", "enabled/test-skill/reference.md"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("缺文件 %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "enabled/other-skill")); err == nil {
		t.Error("不应落其它技能目录")
	}
	// 暂存目录已清理（rename 后 tmp 空目录由 defer RemoveAll 清）
	m, _ := filepath.Glob(filepath.Join(root, ".tmp-*"))
	if len(m) > 0 {
		t.Errorf("暂存目录应清理: %v", m)
	}
	// 重复安装被拒
	if _, err := inst.Install(context.Background(), "acme/repo/test-skill"); err == nil {
		t.Error("重复安装应报错")
	}
}

func TestInstallSkillBadFrontmatter(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"skills/test-skill/SKILL.md": "---\nname: WrongName\ndescription: x\n---\n正文",
	})
	inst, _ := newSkillInstaller(t, tarball, 0)
	if _, err := inst.Install(context.Background(), "acme/repo/test-skill"); err == nil {
		t.Error("name 非 kebab-case 应报错")
	}
}

func TestInstallSkillRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct{ name, body string }{
		{"repo-main/skills/test-skill/SKILL.md", goodSKILL},
		{"repo-main/skills/test-skill/../../evil.sh", "#!/bin/sh"},
	} {
		_ = tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))})
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	inst, root := newSkillInstaller(t, buf.Bytes(), 0)
	if _, err := inst.Install(context.Background(), "acme/repo/test-skill"); err == nil {
		t.Error("路径穿越条目应报错")
	}
	if _, err := os.Stat(filepath.Join(root, "evil.sh")); err == nil {
		t.Error("穿越文件不应落盘")
	}
	m, _ := filepath.Glob(filepath.Join(root, ".tmp-*"))
	if len(m) > 0 {
		t.Errorf("暂存应清理: %v", m)
	}
}

func TestInstallSkillMainFallbackMaster(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{"skills/test-skill/SKILL.md": goodSKILL})
	inst, _ := newSkillInstaller(t, tarball, http.StatusNotFound)
	if _, err := inst.Install(context.Background(), "acme/repo/test-skill"); err != nil {
		t.Fatalf("master 回退安装: %v", err)
	}
}

func TestSearchSkillsProxies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "git" {
			t.Errorf("应透传 q: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"skills":[{"id":"a/b/c","name":"c","installs":1,"source":"a/b"}]}`))
	}))
	t.Cleanup(srv.Close)
	inst := NewSkillInstaller("http://x", srv.URL, t.TempDir())
	res, err := inst.Search(context.Background(), "git")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ID != "a/b/c" {
		t.Errorf("搜索结果异常: %+v", res)
	}
}

func TestParseFrontmatter(t *testing.T) {
	name, desc, err := parseFrontmatter("---\nname: abc\ndescription: plain text\n---\nbody")
	if err != nil || name != "abc" || desc != "plain text" {
		t.Errorf("plain: %q %q %v", name, desc, err)
	}
	_, desc, _ = parseFrontmatter("---\nname: abc\ndescription: 'quoted'\n---\n")
	if desc != "quoted" {
		t.Errorf("single-quoted: %q", desc)
	}
	if _, _, err := parseFrontmatter("---\ndescription: x\n---\n"); err == nil {
		t.Error("缺 name 应报错")
	}
}
