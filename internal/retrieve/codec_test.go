package retrieve

import "testing"

func TestF32RoundTrip(t *testing.T) {
	v := []float32{0.5, -1.25, 3.0, 0}
	b := EncodeF32(v)
	if len(b) != 16 {
		t.Fatalf("4 个 f32 应 16 字节, got %d", len(b))
	}
	got := DecodeF32(b)
	if len(got) != len(v) {
		t.Fatalf("维度不符: %d", len(got))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Errorf("第 %d 个: got %v want %v", i, got[i], v[i])
		}
	}
	if DecodeF32([]byte{1, 2, 3}) != nil {
		t.Error("非 4 倍数字节应回 nil")
	}
}
