package storage

import (
	"context"
	"strings"
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

// TestPersistentObjectURL 验证持久 URL 拼接：公共读对象非签名、不过期，
// 不含 presign 的 X-Tos-Expires/Signature 参数（真实公共读连通性靠部署冒烟）。
func TestPersistentObjectURL(t *testing.T) {
	// Endpoint 只是 host：走 virtual-hosted 风格 https://{bucket}.{host}/{key}
	c := &TOSClient{cfg: TOSConfig{Bucket: "user-growth", Endpoint: "tos-cn-shanghai.volces.com"}}
	if got := c.persistentObjectURL("zhiwei/comic/1.jpeg"); got != "https://user-growth.tos-cn-shanghai.volces.com/zhiwei/comic/1.jpeg" {
		t.Fatalf("host-only got %q", got)
	}

	// Endpoint 带 scheme：直接拼在 endpoint 之后（去掉末尾多余斜杠）
	withScheme := &TOSClient{cfg: TOSConfig{Bucket: "user-growth", Endpoint: "https://user-growth.tos-cn-shanghai.volces.com/"}}
	if got := withScheme.persistentObjectURL("zhiwei/comic/1.jpeg"); got != "https://user-growth.tos-cn-shanghai.volces.com/zhiwei/comic/1.jpeg" {
		t.Fatalf("scheme got %q", got)
	}

	// 断言非签名：URL 不含 presign 参数
	got := c.persistentObjectURL("zhiwei/comic/1.jpeg")
	for _, bad := range []string{"X-Tos-Expires", "Signature", "X-Tos-Signature", "?"} {
		if strings.Contains(got, bad) {
			t.Fatalf("持久 URL 不应包含签名参数 %q，got %q", bad, got)
		}
	}
}
