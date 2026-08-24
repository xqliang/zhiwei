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
		if d.Profile == nil {
			return fmt.Errorf("stage profile: service 未装配")
		}
		begin := time.Now()
		res, err := d.Profile.ExtractSession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("profile: %w", err)
		}
		appendTrace(j, repo.TraceEntry{
			Stage: "profile", MS: msSince(begin),
			Model: d.Profile.Model, PromptVersion: d.Profile.PromptVersion,
			Tokens: res.Tokens, Windows: res.Windows,
			Error: fmt.Sprintf("facts=%d active=%d pending=%d 冲突=%d 佐证=%d 跳过=%d",
				res.Apply.Total, res.Apply.Active, res.Apply.Pending,
				res.Apply.Conflicts, res.Apply.Reaffirmed, res.Apply.Skipped),
		})
		log.Printf("[profile] session=%s facts=%d active=%d pending=%d windows=%d tokens=%d",
			sessionID, res.Apply.Total, res.Apply.Active, res.Apply.Pending, res.Windows, res.Tokens)
		return nil
	}
}
