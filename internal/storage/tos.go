// Package storage 封装火山引擎 TOS 对象存储：上传私有 wav + 生成 presigned GET URL + 删除。
// 复用 xy/web/tools/tos-upload.mjs 同账号/桶（user-growth/cn-shanghai），key 前缀 zhiwei/。
// TOSClient 的方法签名与 provider.TOSUploader 接口一致（UploadWAV/Delete），
// 由 main 装配处做 compile-time 接口符合性校验；本包不 import provider（避免低层→高层依赖）。
package storage

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"
)

// TOSConfig TOS 连接配置。凭证由 main 从环境变量读入后注入（见 config）。
type TOSConfig struct {
	AccessKey string
	SecretKey string
	Region    string // cn-shanghai
	Bucket    string // user-growth
	Endpoint  string // tos-cn-shanghai.volces.com
	KeyPrefix string // zhiwei/
}

type TOSClient struct {
	cfg    TOSConfig
	client *tos.ClientV2
}

func NewTOSClient(cfg TOSConfig) (*TOSClient, error) {
	c, err := tos.NewClientV2(cfg.Endpoint, tos.WithRegion(cfg.Region),
		tos.WithCredentialsProvider(tos.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	if err != nil {
		return nil, err
	}
	return &TOSClient{cfg: cfg, client: c}, nil
}

// UploadWAV 上传本地 wav（私有 ACL），返回 presigned GET URL（1h）。
// key 若未带 KeyPrefix 会自动补前缀。
func (t *TOSClient) UploadWAV(ctx context.Context, localPath, key string) (string, error) {
	key = t.prefixed(key)
	if _, err := t.client.PutObjectFromFile(ctx, &tos.PutObjectFromFileInput{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket: t.cfg.Bucket, Key: key, ContentType: "audio/wav",
		},
		FilePath: localPath,
	}); err != nil {
		return "", fmt.Errorf("tos put: %w", err)
	}
	out, err := t.client.PreSignedURL(&tos.PreSignedURLInput{
		Bucket: t.cfg.Bucket, Key: key, HTTPMethod: enum.HttpMethodGet, Expires: 3600,
	})
	if err != nil {
		return "", fmt.Errorf("tos presign: %w", err)
	}
	return out.SignedUrl, nil
}

// persistentObjectURL 拼 TOS 持久 URL（公共读对象，非签名、不过期）。
// Endpoint 可能带 scheme 也可能只是 host；Bucket 做子域（TOS 标准 virtual-hosted 风格）。
func (t *TOSClient) persistentObjectURL(key string) string {
	host := t.cfg.Endpoint
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimRight(host, "/") + "/" + key
	}
	return "https://" + t.cfg.Bucket + "." + host + "/" + key
}

// UploadImage 上传图片（base64 → 临时文件 → 公共读 ACL），返回持久 URL（不过期）。
// 与 UploadWAV 平行，不动 UploadWAV（ASR 音频保持私有 + 1h presign）。
func (t *TOSClient) UploadImage(ctx context.Context, b64Data, key string) (string, error) {
	key = t.prefixed(key)
	data, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return "", fmt.Errorf("base64 解码: %w", err)
	}
	tmp, err := os.CreateTemp("", "comic-*.jpeg")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	if _, err := t.client.PutObjectFromFile(ctx, &tos.PutObjectFromFileInput{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket: t.cfg.Bucket, Key: key, ContentType: "image/jpeg",
			ACL: enum.ACLPublicRead,
		},
		FilePath: tmp.Name(),
	}); err != nil {
		return "", fmt.Errorf("tos put image: %w", err)
	}
	return t.persistentObjectURL(key), nil
}

// Delete 删除对象（识别完成后清理，best-effort）。key 未带前缀会自动补。
func (t *TOSClient) Delete(ctx context.Context, key string) error {
	key = t.prefixed(key)
	_, err := t.client.DeleteObjectV2(ctx, &tos.DeleteObjectV2Input{Bucket: t.cfg.Bucket, Key: key})
	return err
}

// prefixed 若 key 未以 KeyPrefix 开头则补前缀。
func (t *TOSClient) prefixed(key string) string {
	if t.cfg.KeyPrefix != "" && !strings.HasPrefix(key, t.cfg.KeyPrefix) {
		return t.cfg.KeyPrefix + key
	}
	return key
}
