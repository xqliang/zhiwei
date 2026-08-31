package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSERP 起一个 httptest 假搜索引擎，按 path 分流：/bing 返回 Bing 形态 SERP，
// /empty 返回无结果页，/ddg 返回 DDG-lite 形态，/tavily 校验 Bearer key 后返回 JSON。
func fakeSERP(t *testing.T, key string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bing":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><ol><li class="b_algo"><h2><a href="https://ex.com/a">结果甲</a></h2><div class="b_caption"><p>摘要甲</p></div></li><li class="b_algo"><h2><a href="https://ex.com/b">结果乙</a></h2><p>摘要乙</p></li></ol></body></html>`)
		case "/empty":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><p>没有结果</p></body></html>`)
		case "/ddg":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><table>
<tr><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fex.com%2Fddg&amp;rut=abc" class="result-link">DDG结果</a></td></tr>
<tr><td class="result-snippet">DDG摘要</td></tr>
</table></body></html>`)
		case "/tavily":
			if got := r.Header.Get("Authorization"); got != "Bearer "+key {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":"bad key"}`))
				return
			}
			var body struct {
				Query      string `json:"query"`
				MaxResults int    `json:"max_results"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Query == "" || body.MaxResults <= 0 {
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"results":[{"title":"Tavily甲","url":"https://ex.com/t1","content":"内容甲"},{"title":"Tavily乙","url":"https://ex.com/t2","content":"内容乙"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

// testSearcher 指向假 SERP 的 Searcher（普通 client 绕过 SSRF 拨号，见 fetch_test.go 注释）。
func testSearcher(srv *httptest.Server) *Searcher {
	return &Searcher{
		HTTP:      &http.Client{Timeout: 5 * time.Second},
		BingURL:   srv.URL + "/bing",
		DDGURL:    srv.URL + "/ddg",
		TavilyURL: srv.URL + "/tavily",
	}
}

func TestSearchBingParse(t *testing.T) {
	srv := fakeSERP(t, "")
	defer srv.Close()
	rs, err := testSearcher(srv).Search(context.Background(), EngineBing, "", "测试", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Title != "结果甲" || rs[0].URL != "https://ex.com/a" || rs[0].Snippet != "摘要甲" {
		t.Fatalf("Bing 解析不符: %+v", rs)
	}
}

func TestSearchDDGParse(t *testing.T) {
	srv := fakeSERP(t, "")
	defer srv.Close()
	rs, err := testSearcher(srv).Search(context.Background(), EngineDDG, "", "测试", 5)
	if err != nil {
		t.Fatal(err)
	}
	// uddg 解包后的真实 URL + 摘要按文档序配对。
	if len(rs) != 1 || rs[0].Title != "DDG结果" || rs[0].URL != "https://ex.com/ddg" || rs[0].Snippet != "DDG摘要" {
		t.Fatalf("DDG 解析不符: %+v", rs)
	}
}

func TestSearchTavily(t *testing.T) {
	srv := fakeSERP(t, "tvly-x")
	defer srv.Close()
	s := testSearcher(srv)
	rs, err := s.Search(context.Background(), EngineTavily, "tvly-x", "测试", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Title != "Tavily甲" || rs[0].Snippet != "内容甲" {
		t.Fatalf("Tavily 解析不符: %+v", rs)
	}
	// key 缺失 / 错 key 都要报错。
	if _, err := s.Search(context.Background(), EngineTavily, "", "测试", 5); err == nil {
		t.Error("缺 key 应报错")
	}
	if _, err := s.Search(context.Background(), EngineTavily, "wrong", "测试", 5); err == nil {
		t.Error("错 key 应报错（401）")
	}
}

// TestSearchAutoChain：auto = Bing 失败（此处指到 /empty）→ 降级 DDG 成功。
func TestSearchAutoChain(t *testing.T) {
	srv := fakeSERP(t, "")
	defer srv.Close()
	s := testSearcher(srv)
	s.BingURL = srv.URL + "/empty" // 首选引擎解析不到结果
	rs, err := s.Search(context.Background(), EngineAuto, "", "测试", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Title != "DDG结果" {
		t.Fatalf("auto 链应降级到 DDG: %+v", rs)
	}
	// 全链失败且无 key → 明确报错（提示配 Tavily key）。
	s.DDGURL = srv.URL + "/empty"
	if _, err := s.Search(context.Background(), EngineAuto, "", "测试", 5); err == nil || !strings.Contains(err.Error(), "Tavily") {
		t.Fatalf("全链失败应报含 Tavily 提示的错误: %v", err)
	}
}

func TestSearchArgValidation(t *testing.T) {
	srv := fakeSERP(t, "")
	defer srv.Close()
	s := testSearcher(srv)
	if _, err := s.Search(context.Background(), EngineBing, "", "  ", 5); err == nil {
		t.Error("空 query 应报错")
	}
	if _, err := s.Search(context.Background(), "nope", "", "x", 5); err == nil {
		t.Error("未知引擎应报错")
	}
	if !ValidEngine(EngineAuto) || !ValidEngine(EngineBing) || !ValidEngine(EngineDDG) || !ValidEngine(EngineTavily) || ValidEngine("nope") {
		t.Error("ValidEngine 判定不符")
	}
}

// TestUnwrapDDGNoDoubleUnescape：uddg 参数只应解一层——目标 URL 含 + 或 % 序列时
// 不能被二次反转义（+ 变空格 / %XX 被吃掉），否则交给模型的链接是坏的。
func TestUnwrapDDGNoDoubleUnescape(t *testing.T) {
	// 目标 https://ex.com/c++q?a=1+2 单层编码进 uddg（%→%25、+→%2B）。
	wrapped := "//duckduckgo.com/l/?uddg=https%3A%2F%2Fex.com%2Fc%2B%2Bq%3Fa%3D1%2B2&rut=abc"
	if got := unwrapDDG(wrapped); got != "https://ex.com/c++q?a=1+2" {
		t.Fatalf("uddg 解包应单层解码: got %q", got)
	}
	// 非包装链接原样返回。
	if got := unwrapDDG("https://plain.example/x"); got != "https://plain.example/x" {
		t.Fatalf("非包装链接应原样返回: %q", got)
	}
}
