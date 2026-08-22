package storage

import (
	"context"
	"testing"
)

// TOSClient 真实调用需云凭证，不进单测。这里用匿名接口做 compile-time 签名符合性校验
// （等价于 provider.TOSUploader 的方法集；真正接口符合性在 main 装配时校验）。
func TestTOSClientSignatureMatchesUploader(t *testing.T) {
	var _ interface {
		UploadWAV(ctx context.Context, localPath, key string) (string, error)
		Delete(ctx context.Context, key string) error
	} = (*TOSClient)(nil)
}

func TestPrefixed(t *testing.T) {
	c := &TOSClient{cfg: TOSConfig{KeyPrefix: "zhiwei/"}}
	if got := c.prefixed("a.wav"); got != "zhiwei/a.wav" {
		t.Fatalf("got %q", got)
	}
	if got := c.prefixed("zhiwei/a.wav"); got != "zhiwei/a.wav" {
		t.Fatalf("got %q", got)
	}
	empty := &TOSClient{cfg: TOSConfig{}}
	if got := empty.prefixed("a.wav"); got != "a.wav" {
		t.Fatalf("got %q", got)
	}
}
