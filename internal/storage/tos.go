// Package storage 封装火山引擎 TOS 对象存储：上传私有 wav + 生成 presigned GET URL + 删除。
// 复用 xy/web/tools/tos-upload.mjs 同账号/桶（user-growth/cn-shanghai），key 前缀 zhiwei/。
// TOSClient 的方法签名与 provider.TOSUploader 接口一致（UploadWAV/Delete），
// 由 main 装配处做 compile-time 接口符合性校验；本包不 import provider（避免低层→高层依赖）。
package storage

import (
	"context"
	"fmt"
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
