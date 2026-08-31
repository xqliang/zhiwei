package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zhiwei/internal/repo"
	"zhiwei/internal/search"
)

// webDeps 构造带假 SERP/假页面的 MCPDeps（Configs 指向真库以测引擎路由）。
func webDeps(t *testing.T) (MCPDeps, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/serp":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><ol><li class="b_algo"><h2><a href="https://ex.com/a">搜索结果甲</a></h2><p>摘要甲</p></li></ol></body></html>`)
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><title>页面标题</title></head><body><p>页面正文内容</p></body></html>`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfgRepo := &repo.AgentConfigRepo{DB: db}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_config WHERE id = 1") })
	md, _ := p2dDeps(t)
	md.Configs = cfgRepo
	md.Search = &search.Searcher{
		HTTP:      &http.Client{Timeout: 5 * time.Second},
		BingURL:   srv.URL + "/serp",
		DDGURL:    srv.URL + "/serp",
		TavilyURL: srv.URL + "/serp",
	}
	md.Fetch = &search.Fetcher{HTTP: &http.Client{Timeout: 5 * time.Second}}
	return md, srv
}

// TestWebSearchTool：默认（无配置行）auto 链命中假 SERP → 返回结构化结果；
// 配置引擎=bing → 只走 bing；Search=nil 时报「未启用」；空 query 报错。
func TestWebSearchTool(t *testing.T) {
	md, _ := webDeps(t)
	ctx := context.Background()
	res, _, err := webSearchHandler(md)(ctx, nil, webSearchArgs{Query: "测试", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	var out []webResultOut
	if err := json.Unmarshal([]byte(mcpText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Title != "搜索结果甲" || out[0].URL != "https://ex.com/a" {
		t.Fatalf("web_search 结果不符: %+v", out)
	}
	// 配置了引擎=bing → 只走 bing（同样命中假 SERP，读最新配置生效）。
	if err := md.Configs.Upsert(ctx, repo.AgentConfig{SearchEngine: "bing"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := webSearchHandler(md)(ctx, nil, webSearchArgs{Query: "测试"}); err != nil {
		t.Fatalf("指定 bing 引擎不应报错: %v", err)
	}
	// Search 未装配 → tool-error。
	md.Search = nil
	if _, _, err := webSearchHandler(md)(ctx, nil, webSearchArgs{Query: "测试"}); err == nil {
		t.Error("Search=nil 应报「未启用」")
	}
	// 空 query。
	md2, _ := webDeps(t)
	if _, _, err := webSearchHandler(md2)(ctx, nil, webSearchArgs{Query: " "}); err == nil {
		t.Error("空 query 应报错")
	}
}

// TestWebFetchTool：抓假页面返回 title+正文；空 URL 报错；Fetch=nil 报「未启用」。
func TestWebFetchTool(t *testing.T) {
	md, srv := webDeps(t)
	ctx := context.Background()
	res, _, err := webFetchHandler(md)(ctx, nil, webFetchArgs{URL: srv.URL + "/page"})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(mcpText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Title != "页面标题" || !strings.Contains(out.Text, "页面正文内容") {
		t.Fatalf("web_fetch 结果不符: %+v", out)
	}
	if _, _, err := webFetchHandler(md)(ctx, nil, webFetchArgs{URL: " "}); err == nil {
		t.Error("空 URL 应报错")
	}
	md.Fetch = nil
	if _, _, err := webFetchHandler(md)(ctx, nil, webFetchArgs{URL: srv.URL + "/page"}); err == nil {
		t.Error("Fetch=nil 应报「未启用」")
	}
}
