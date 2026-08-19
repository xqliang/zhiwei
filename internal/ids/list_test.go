package ids

import (
	"encoding/json"
	"testing"
)

func TestListValueAndScan(t *testing.T) {
	l := List{1234567890123456789, 1234567890123456790}
	v, err := l.Value()
	if err != nil {
		t.Fatal(err)
	}
	// Valuer 输出 JSON 文本，可直接写入 MySQL JSON 列
	if s, ok := v.(string); !ok || s != `["1234567890123456789","1234567890123456790"]` {
		t.Fatalf("Value = %#v", v)
	}

	var out List
	if err := out.Scan([]byte(s0(t))); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0] != 1234567890123456789 {
		t.Fatalf("Scan out = %v", out)
	}
}

func TestListScanNilAndEmpty(t *testing.T) {
	var l List
	if err := l.Scan(nil); err != nil || l != nil {
		t.Fatalf("Scan(nil) -> %v %v", l, err)
	}
	if err := l.Scan([]byte("[]")); err != nil || len(l) != 0 {
		t.Fatalf("Scan([]) -> %v %v", l, err)
	}

	// nil 切片 Value() 直接写 NULL（不产生 "null" 文本）
	v, err := List(nil).Value()
	if err != nil || v != nil {
		t.Fatalf("List(nil).Value() -> %v %v", v, err)
	}
	// 空 but 非 nil 切片写 JSON 空数组，而不是 NULL
	v, err = List{}.Value()
	if err != nil || v != "[]" {
		t.Fatalf("List{}.Value() -> %v %v", v, err)
	}
}

func TestListJSONRoundTrip(t *testing.T) {
	b, err := json.Marshal(List{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `["1","2"]` {
		t.Fatalf("json = %s", b)
	}
	var l List
	if err := json.Unmarshal([]byte(`["1","2"]`), &l); err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 || l[1] != 2 {
		t.Fatalf("unmarshal = %v", l)
	}
	// nil 序列化为 null（对应 DB NULL 列）
	b, _ = json.Marshal(List(nil))
	if string(b) != "null" {
		t.Fatalf("nil json = %s", b)
	}
}

func s0(t *testing.T) string {
	t.Helper()
	l := List{1234567890123456789, 1234567890123456790}
	v, _ := l.Value()
	s, _ := v.(string)
	return s
}
