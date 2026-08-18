// spike/llm 用真实 ARK_API_KEY 验证 Ark LLM 连通性（手动运行，不进 CI）。
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
	p := provider.NewArkLLM(cfg.ARKBaseURL, cfg.ARKAPIKey)
	resp, err := p.Chat(context.Background(), provider.ChatRequest{
		Model:  cfg.LLMFastModel,
		System: "你只能回复 JSON。",
		User:   `用 JSON 回答：{"hello":"world"} 的键是什么？`,
	})
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Printf("OK model=%s tokens=%d content=%s\n", cfg.LLMFastModel, resp.TotalTokens, resp.Content)
}
