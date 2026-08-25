package review

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/repo"
)

func TestGenerateReportTopicStatusTool(t *testing.T) {
	f := &fakeLLM{Reply: `{"summary":"s","progress":0.3,"risks":[],"blockers":[]}`}
	g := newGenWithFake(t, f)
	ctx := context.Background()
	tp := &repo.Topic{Name: "工具测试话题", Status: "active", CreatedBy: "user"}
	if err := g.Topics.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = g.Topics.Delete(context.Background(), tp.ID)
		_, _ = g.TopicStatuses.DB.ExecContext(context.Background(), `DELETE FROM topic_status WHERE topic_id = ?`, tp.ID.Int64())
	})

	res, _, err := generateReportHandler(g)(ctx, nil, generateReportArgs{Type: "topic_status", Target: tp.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	// 结果是 repo.TopicStatus 行的 JSON；断言可解回且带 content
	tc := res.Content[0].(*mcp.TextContent).Text
	var row repo.TopicStatus
	if err := json.Unmarshal([]byte(tc), &row); err != nil || row.Content == nil {
		t.Errorf("工具结果异常: %s (err=%v)", tc, err)
	}
}

func TestGenerateReportBadType(t *testing.T) {
	g := &Generator{LLM: &fakeLLM{}} // 不触 DB（bad type 提前返回）
	if _, _, err := generateReportHandler(g)(context.Background(), nil, generateReportArgs{Type: "xxx"}); err == nil {
		t.Error("未知类型应报错")
	}
}
