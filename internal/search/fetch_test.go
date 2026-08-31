package search

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testClient 绕过 SSRF 安全拨号的普通 client（httptest 监听 127.0.0.1；仅测试用，
// 生产装配用 NewFetcher/NewSearcher 的安全 client）。
func testClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

// TestBlockedIP 锁定 SSRF 根防线的 IP 判定表：环回/私网/链路本地/组播/未指定/ULA 全拒，公网放行。
func TestBlockedIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.1.1", "0.0.0.0", "::1", "fe80::1", "fd00::1", "224.0.0.1"} {
		if !blockedIP(net.ParseIP(s)) {
			t.Errorf("%s 应被拦截", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		if blockedIP(net.ParseIP(s)) {
			t.Errorf("%s 不应被拦截", s)
		}
	}
}

// TestFetchHTMLToText：抓 HTML 页提取 <title> 与正文（剥 script/style、折叠空白）。
func TestFetchHTMLToText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>示例页</title><style>.x{color:red}</style></head>
<body><script>evil()</script><h1>你好</h1><p>第一段   多余空白</p><p>第二段</p></body></html>`))
	}))
	defer srv.Close()
	f := &Fetcher{HTTP: testClient()}
	p, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "示例页" {
		t.Errorf("title=%q", p.Title)
	}
	for _, want := range []string{"你好", "第一段 多余空白", "第二段"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("正文应含 %q: %q", want, p.Text)
		}
	}
	if strings.Contains(p.Text, "evil") || strings.Contains(p.Text, "color:red") {
		t.Errorf("正文不应含 script/style 内容: %q", p.Text)
	}
}

// TestFetchRejects：非 2xx / 非 HTML Content-Type / 空 URL / 非 http 协议 各自报错。
func TestFetchRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/404":
			w.WriteHeader(404)
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>ok</body></html>`))
		}
	}))
	defer srv.Close()
	f := &Fetcher{HTTP: testClient()}
	for _, tc := range []struct{ name, url string }{
		{"404", srv.URL + "/404"},
		{"json", srv.URL + "/json"},
		{"空", "  "},
		{"ftp", "ftp://example.com/x"},
	} {
		if _, err := f.Fetch(context.Background(), tc.url); err == nil {
			t.Errorf("%s 应报错", tc.name)
		}
	}
}

// TestFetchSSRFBlocked：安全 client（NewFetcher 默认）请求 127.0.0.1 的 httptest → 拨号被拒。
func TestFetchSSRFBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should never reach"))
	}))
	defer srv.Close()
	f := NewFetcher()
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("SSRF 防护应拦截 127.0.0.1 拨号")
	}
}

// TestFetchTruncates：正文超 fetchMaxText 按 rune 截断（带省略标记），不截出半个汉字。
func TestFetchTruncates(t *testing.T) {
	long := strings.Repeat("字", fetchMaxText+50)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>" + long + "</p></body></html>"))
	}))
	defer srv.Close()
	p, err := (&Fetcher{HTTP: testClient()}).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(p.Text)); got > fetchMaxText+10 { // 截断标记「…（已截断）」额外几个字符
		t.Errorf("截断后长度 %d 超限", got)
	}
	if !strings.Contains(p.Text, "已截断") {
		t.Errorf("截断应带省略标记: %q", p.Text[:20])
	}
}
