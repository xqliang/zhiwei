package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArkEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]},{"embedding":[0.4,0.5,0.6]}]}`))
	}))
	defer srv.Close()

	p := NewArkEmbed(srv.URL, "test-key", "test-model")
	vecs, err := p.Embed(context.Background(), []string{"你好", "世界"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 {
		t.Fatalf("vecs = %v", vecs)
	}
}
