// spike: 验证 StepFun 异步文件 ASR 端到端（TOS 上传→submit→轮询 query→解析 utterances）。
//
// 用法:
//
//	STEPFUN_ASR_FILE_API_KEY=.. TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/asr testdata/speech.wav
//	ZW_STEPFUN_ASR_BASE 可选，默认 https://api.c.ibasemind.com/v1；生产设 https://api.stepfun.com/v1
//
// 目的: 端到端验证 §2.1 的接口契约（result[].utterances[].{text,start_time,end_time,speaker.id}），
//
//	确认 parseFileASRResult（internal/provider/asr.go）的解析字段与真实响应一致。
//
// 自包含：内联 TOS 上传/presign/删除（用 Task 1 spike 验证过的真实 SDK API）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: STEPFUN_ASR_FILE_API_KEY=.. TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/asr <wav>")
		os.Exit(1)
	}
	apiKey := os.Getenv("STEPFUN_ASR_FILE_API_KEY")
	baseURL := os.Getenv("ZW_STEPFUN_ASR_BASE")
	if baseURL == "" {
		baseURL = "https://api.c.ibasemind.com/v1"
	}
	ak, sk := os.Getenv("TOS_ACCESS_KEY"), os.Getenv("TOS_SECRET_KEY")
	if apiKey == "" || ak == "" || sk == "" {
		fmt.Println("缺 STEPFUN_ASR_FILE_API_KEY / TOS_ACCESS_KEY / TOS_SECRET_KEY")
		os.Exit(1)
	}

	ctx := context.Background()
	tosURL := uploadAndPresign(ctx, ak, sk, os.Args[1])
	fmt.Println("presigned:", tosURL)
	defer cleanupTOS(ctx, ak, sk, tosKey) // tosKey 由 uploadAndPresign 设置（见下）

	taskID, err := submit(ctx, apiKey, baseURL, tosURL)
	if err != nil {
		panic(err)
	}
	fmt.Println("task:", taskID)

	raw, err := poll(ctx, apiKey, baseURL, taskID)
	if err != nil {
		panic(err)
	}
	fmt.Println("query raw:", string(raw))
}

var tosKey string

func uploadAndPresign(ctx context.Context, ak, sk, localPath string) string {
	endpoint, region, bucket := "tos-cn-shanghai.volces.com", "cn-shanghai", "user-growth"
	tosKey = "zhiwei/spike-asr-" + fmt.Sprint(time.Now().UnixNano()) + ".wav"
	client, err := tos.NewClientV2(endpoint, tos.WithRegion(region),
		tos.WithCredentialsProvider(tos.NewStaticCredentialsProvider(ak, sk, "")))
	if err != nil {
		panic(err)
	}
	if _, err := client.PutObjectFromFile(ctx, &tos.PutObjectFromFileInput{
		PutObjectBasicInput: tos.PutObjectBasicInput{Bucket: bucket, Key: tosKey, ContentType: "audio/wav"},
		FilePath:            localPath,
	}); err != nil {
		panic(fmt.Errorf("上传: %w", err))
	}
	out, err := client.PreSignedURL(&tos.PreSignedURLInput{
		Bucket: bucket, Key: tosKey, HTTPMethod: enum.HttpMethodGet, Expires: 3600,
	})
	if err != nil {
		panic(fmt.Errorf("presign: %w", err))
	}
	return out.SignedUrl
}

func cleanupTOS(ctx context.Context, ak, sk, _ string) {
	endpoint, region, bucket := "tos-cn-shanghai.volces.com", "cn-shanghai", "user-growth"
	client, _ := tos.NewClientV2(endpoint, tos.WithRegion(region),
		tos.WithCredentialsProvider(tos.NewStaticCredentialsProvider(ak, sk, "")))
	if client != nil {
		_, _ = client.DeleteObjectV2(ctx, &tos.DeleteObjectV2Input{Bucket: bucket, Key: tosKey})
	}
}

func submit(ctx context.Context, apiKey, baseURL, audioURL string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"audio":   map[string]any{"format": "wav", "channel": 1, "rate": 16000, "url": audioURL},
		"request": map[string]any{"model_name": "stepaudio-2.5-asr", "show_utterances": true, "enable_speaker_info": true},
	})
	raw, err := post(ctx, apiKey, baseURL+"/audio/asr/file/submit", body)
	if err != nil {
		return "", err
	}
	var r struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.TaskID == "" {
		return "", fmt.Errorf("submit 响应非法: %s", raw)
	}
	return r.TaskID, nil
}

func poll(ctx context.Context, apiKey, baseURL, taskID string) ([]byte, error) {
	for i := 0; i < 150; i++ { // 2s × 150 ≈ 5min
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		body, _ := json.Marshal(map[string]string{"task_id": taskID})
		raw, err := post(ctx, apiKey, baseURL+"/audio/asr/file/query", body)
		if err != nil {
			continue
		}
		var r struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		fmt.Println("status:", r.Status)
		if r.Status == "FAILED" {
			return nil, fmt.Errorf("asr failed: %s", raw)
		}
		if r.Status != "PENDING" && r.Status != "RUNNING" {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("超时（task=%s）", taskID)
}

func post(ctx context.Context, apiKey, url string, body []byte) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
