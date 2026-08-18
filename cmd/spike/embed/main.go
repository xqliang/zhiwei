// spike/embed 用真实 key 验证 Ark Embedding 连通性（手动运行）。
// 注意：当前账号 embedding 模型未开通（403），需到火山控制台开通后重跑。
package main

import (
	"context"
	"fmt"
	"os"

	"zhiwei/internal/config"
	"zhiwei/internal/provider"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}
	p := provider.NewArkEmbed(cfg.ARKBaseURL, cfg.ARKAPIKey, cfg.EmbedModel)
	vecs, err := p.Embed(context.Background(), []string{"今天和朋友讨论了 Rust 学习计划"})
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Printf("OK dim=%d first3=%v\n", len(vecs[0]), vecs[0][:3])
}
