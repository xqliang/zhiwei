package retrieve

import (
	"encoding/binary"
	"math"
)

// EncodeF32 把向量编码成小端 float32 字节（与 internal/api/speaker.go 声纹布局一致）。
func EncodeF32(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

// DecodeF32 解码小端 float32 字节；长度非 4 倍数（脏数据）返回 nil。
func DecodeF32(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
