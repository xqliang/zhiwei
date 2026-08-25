package review

import (
	"context"
	"encoding/json"
	"errors"
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

// TestGetTopicStatusToolReadsLatest 验证 M3：get_topic_status 工具「读优先」——
// 首次无快照时生成一条；此后再调应直接返回最新快照，不重算(不调用 LLM)、不插新行。
// 需要独立库（无 DSN 自动跳过）。
func TestGetTopicStatusToolReadsLatest(t *testing.T) {
	f := &fakeLLM{Reply: `{"summary":"s","progress":0.3,"risks":[],"blockers":[]}`}
	g := newGenWithFake(t, f)
	ctx := context.Background()
	tp := &repo.Topic{Name: "读优先话题", Status: "active", CreatedBy: "user"}
	if err := g.Topics.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = g.Topics.Delete(context.Background(), tp.ID)
		_, _ = g.TopicStatuses.DB.ExecContext(context.Background(), `DELETE FROM topic_status WHERE topic_id = ?`, tp.ID.Int64())
	})

	h := getTopicStatusHandler(g)
	// 首次：无快照 → 生成一条
	if _, _, err := h(ctx, nil, getTopicStatusArgs{TopicID: tp.ID.String()}); err != nil {
		t.Fatal(err)
	}
	// 冻结 LLM：若第二次仍重算就会命中此错误，证明没有二次生成
	g.LLM = &fakeLLM{Err: errors.New("读优先不应再调用 LLM")}
	if _, _, err := h(ctx, nil, getTopicStatusArgs{TopicID: tp.ID.String()}); err != nil {
		t.Fatalf("第二次应读最新快照、不触发 LLM: %v", err)
	}
	// topic_status 仍只有 1 行（没有二次 INSERT）
	var cnt int
	if err := g.TopicStatuses.DB.GetContext(ctx, &cnt,
		`SELECT COUNT(*) FROM topic_status WHERE topic_id = ?`, tp.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("读优先不应插新行，期望 1 行 got %d", cnt)
	}
}
