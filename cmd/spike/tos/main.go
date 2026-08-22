// spike: 验证火山引擎 TOS Go SDK 的上传/presign/删除三件套。
// 用法: TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/tos <本地wav>
// 目的: 拿到可用的 NewClientV2 / PutObjectFromFile / PreSignedURL / DeleteObjectV2 调用，
//       确认输入结构体字段名，固化到 internal/storage/tos.go（后续任务）。
//
// ===== 经 `go doc` 核对（SDK v2.9.9，本地 module cache）后确认的真实 API =====
// （下面标 ⚠️ 的三处，是计划里按 pkg.go.dev 索引猜测、但实际编译不过、已修正的差异）
//
// 建客户端：
//   func tos.NewClientV2(endpoint string, options ...tos.ClientOption) (*tos.ClientV2, error)
//   选项：  tos.WithRegion(region string) tos.ClientOption
//           tos.WithCredentialsProvider(p tos.CredentialsProvider) tos.ClientOption
//   凭证：  tos.NewStaticCredentialsProvider(ak, sk, securityToken string) tos.CredentialsProvider
//
// 上传：  func (*tos.ClientV2) PutObjectFromFile(ctx, *tos.PutObjectFromFileInput) (*tos.PutObjectFromFileOutput, error)  // 带 ctx
//   ⚠️ PutObjectFromFileInput 内嵌 PutObjectBasicInput（Bucket/Key/ContentType 等在里面），
//      Go 复合字面量【不能平铺赋值内嵌字段】，必须显式写 PutObjectBasicInput: tos.PutObjectBasicInput{...}；
//      只有 FilePath 才是 PutObjectFromFileInput 自身的直接字段。
//
// 预签名：func (*tos.ClientV2) PreSignedURL(*tos.PreSignedURLInput) (*tos.PreSignedURLOutput, error)
//   ⚠️ 该方法【不接收 ctx】（计划里的 client.PreSignedURL(ctx, ...) 编译不过，已去掉 ctx）。
//   ⚠️ PreSignedURLInput.HTTPMethod 类型是 enum.HttpMethodType；GET 常量是 enum.HttpMethodGet
//      （tos.HTTPMethodGet 这个符号不存在），需 import ".../tos/enum"。
//      字段：Bucket string、Key string、HTTPMethod enum.HttpMethodType、
//            Expires int64（秒，默认 3600，范围 [1, 604800]）。
//      输出：PreSignedURLOutput.SignedUrl string 即预签名 URL。
//
// 删除：  func (*tos.ClientV2) DeleteObjectV2(ctx, *tos.DeleteObjectV2Input) (*tos.DeleteObjectV2Output, error)  // 带 ctx
//      DeleteObjectV2Input.Bucket / .Key 是直接字段（非内嵌），可平铺赋值。
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"
)

func main() {
	ak, sk := os.Getenv("TOS_ACCESS_KEY"), os.Getenv("TOS_SECRET_KEY")
	if ak == "" || sk == "" || len(os.Args) < 2 {
		fmt.Println("用法: TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/tos <wav>")
		os.Exit(1)
	}
	localPath := os.Args[1]
	endpoint := "tos-cn-shanghai.volces.com"
	region := "cn-shanghai"
	bucket := "user-growth"
	key := "zhiwei/spike-" + fmt.Sprint(time.Now().Unix()) + ".wav"

	client, err := tos.NewClientV2(endpoint, tos.WithRegion(region),
		tos.WithCredentialsProvider(tos.NewStaticCredentialsProvider(ak, sk, "")))
	if err != nil {
		panic(err)
	}
	ctx := context.Background()

	// 上传（私有，默认 ACL）。Bucket/Key/ContentType 在内嵌的 PutObjectBasicInput 里，须显式嵌套赋值。
	_, err = client.PutObjectFromFile(ctx, &tos.PutObjectFromFileInput{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket: bucket, Key: key, ContentType: "audio/wav",
		},
		FilePath: localPath,
	})
	if err != nil {
		panic(fmt.Errorf("上传: %w", err))
	}
	fmt.Println("uploaded:", key)

	// presigned GET URL（1h）。注意：PreSignedURL 不接收 ctx；GET 用 enum.HttpMethodGet。
	out, err := client.PreSignedURL(&tos.PreSignedURLInput{
		Bucket: bucket, Key: key, HTTPMethod: enum.HttpMethodGet, Expires: 3600,
	})
	if err != nil {
		panic(fmt.Errorf("presign: %w", err))
	}
	fmt.Println("presigned:", out.SignedUrl)

	// 验证 URL 可拉取（打印 HEAD 状态码）
	resp, err := http.Head(out.SignedUrl)
	if err == nil {
		fmt.Println("fetch status:", resp.StatusCode)
		resp.Body.Close()
	}

	// 删除
	_, err = client.DeleteObjectV2(ctx, &tos.DeleteObjectV2Input{Bucket: bucket, Key: key})
	if err != nil {
		fmt.Println("delete err:", err)
	}
	fmt.Println("done")
}
