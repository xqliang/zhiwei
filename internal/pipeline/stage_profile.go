package pipeline

import (
	"context"
	"fmt"
	"log"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// stageProfile 是画像抽取 stage 的薄包装：完整逻辑在 profile.Service.ExtractSession
// （与 API 回填端点共用），这里只做调用与 trace 记录。
// stage 顺序：asr → segment → speaker → extract → profile（main.go 装配）。
func stageProfile(d StageDeps) Handler {
	return func(ctx context.Context, j *repo.Job, sessionID ids.ID) error {
		// 装配错误（Profile 未注入）保持致命：这是部署/接线错误，不是运行时抖动，
		// 应尽早暴露而非静默放行。与下面 ExtractSession 的运行时错误（非致命）区别对待。
		if d.Profile == nil {
			return fmt.Errorf("stage profile: service 未装配")
		}
		begin := time.Now()
		res, err := d.Profile.ExtractSession(ctx, sessionID)
		if err != nil {
			// F3（spec §13）：非致命化——profile 是 pipeline 末段，transcript/memory 已落库
			// 且完好，画像只是增强数据；ExtractSession 失败（如 LLM 超时/抖动）不应把整个
			// session 置 failed。记 trace（Error 字段）+ 日志后 return nil 放行；后续「从历史
			// 回填」端点可重跑该 session 的画像（ApplyFacts 幂等，重跑安全，不会重复落库）。
			appendTrace(j, repo.TraceEntry{
				Stage: "profile", MS: msSince(begin),
				Model: d.Profile.Model, PromptVersion: d.Profile.PromptVersion,
				Error: fmt.Sprintf("画像抽取失败（非致命，可回填重跑）: %v", err),
			})
			log.Printf("[profile] session=%s 抽取失败（非致命）: %v", sessionID, err)
			return nil
		}
		appendTrace(j, repo.TraceEntry{
			Stage: "profile", MS: msSince(begin),
			Model: d.Profile.Model, PromptVersion: d.Profile.PromptVersion,
			Tokens: res.Tokens, Windows: res.Windows,
			Error: fmt.Sprintf("facts=%d active=%d pending=%d 冲突=%d 佐证=%d 跳过=%d 提及人名=%d 新建人物=%d",
				res.Apply.Total, res.Apply.Active, res.Apply.Pending,
				res.Apply.Conflicts, res.Apply.Reaffirmed, res.Apply.Skipped,
				res.Mentioned, res.PersonsNew),
		})
		log.Printf("[profile] session=%s facts=%d active=%d pending=%d 提及人名=%d 新建人物=%d windows=%d tokens=%d",
			sessionID, res.Apply.Total, res.Apply.Active, res.Apply.Pending,
			res.Mentioned, res.PersonsNew, res.Windows, res.Tokens)
		return nil
	}
}
