package ids

import (
	"encoding/json"
	"testing"
)

type payload struct {
	ID ID `json:"id"`
}

func TestMarshalAsString(t *testing.T) {
	b, err := json.Marshal(payload{ID: 1234567890123456789})
	if err != nil {
		t.Fatal(err)
	}
	// 雪花 ID 超过 JS Number.MAX_SAFE_INTEGER，必须序列化为字符串
	if string(b) != `{"id":"1234567890123456789"}` {
		t.Fatalf("got %s", b)
	}
}

func TestUnmarshalFromString(t *testing.T) {
	var p payload
	if err := json.Unmarshal([]byte(`{"id":"1234567890123456789"}`), &p); err != nil {
		t.Fatal(err)
	}
	if int64(p.ID) != 1234567890123456789 {
		t.Fatalf("got %d", int64(p.ID))
	}
}

func TestNewUnique(t *testing.T) {
	if err := Init(1); err != nil {
		t.Fatal(err)
	}
	a, b := New(), New()
	if a == b {
		t.Fatal("生成的 ID 不应重复")
	}
}
