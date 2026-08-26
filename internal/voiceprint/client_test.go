package voiceprint

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"zhiwei/internal/ids"
)

// TestMain 统一初始化雪花 ID 节点：TestClientAddRemove 会调用 ids.New()，
// 而 ids.New() 依赖 ids.Init() 先设置好 snowflake 节点，否则 node 为 nil 会 panic。
// 与 internal/pipeline、internal/repo 的测试初始化保持一致。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}

func TestClientSearchMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			_, _ = w.Write([]byte(`{"speaker_id":42,"distance":0.81,"second_distance":0.12,"matched":true}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	res, err := c.Search(context.Background(), make([]float32, 256))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Matched || res.SpeakerID.Int64() != 42 || res.Distance != 0.81 || res.SecondDistance != 0.12 {
		t.Fatalf("%+v", res)
	}
}

func TestClientSearchMatchNoSecondField(t *testing.T) {
	// 旧版 sidecar 响应无 second_distance 字段 → 解析为 0（gap 规则退化为仅看 top1），不报错
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			_, _ = w.Write([]byte(`{"speaker_id":42,"distance":0.81,"matched":true}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	res, err := c.Search(context.Background(), make([]float32, 256))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Matched || res.SpeakerID.Int64() != 42 || res.SecondDistance != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestClientSearchEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"matched":false}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	res, err := c.Search(context.Background(), make([]float32, 256))
	if err != nil || res.Matched {
		t.Fatalf("%v %v", res.Matched, err)
	}
}

func TestClientAddRemove(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	if err := c.Add(context.Background(), make([]float32, 256), ids.New()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/add" {
		t.Fatalf("%s", gotPath)
	}
	if err := c.Remove(context.Background(), ids.ID(42)); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/remove" {
		t.Fatalf("%s", gotPath)
	}
}

func TestClientEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vec := make([]float32, 256)
		b, _ := json.Marshal(map[string]any{"vector": vec})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	v, err := c.Embed(context.Background(), "x.wav")
	if err != nil || len(v) != 256 {
		t.Fatalf("%v %d", err, len(v))
	}
}
