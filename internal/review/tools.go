package review

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
)

// RegisterReportTools 把报告工具注册到（已由 agent.NewMCPServer 建好的）MCP server。
// 协调者装配：s := agent.NewMCPServer(deps); review.RegisterReportTools(s, gen)。
func RegisterReportTools(s *mcp.Server, gen *Generator) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "generate_report",
		Description: "生成结构化报告。type=daily|weekly|topic_status；target 可选：daily/weekly 传日期(YYYY-MM-DD，空=今天/本周)，topic_status 传话题 id。返回报告对象。",
	}, generateReportHandler(gen))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_topic_status",
		Description: "取某话题(项目/主题)的整体状态快照(进展/里程碑/未完成待办/风险/阻塞)。现算并落库，返回最新快照。",
	}, getTopicStatusHandler(gen))
}

// jsonResult 把任意值 JSON 序列化为单个 TextContent 结果（对齐 agent/mcp_tools.go）。
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

// mondayOf 返回 t 所在自然周的周一 00:00（周报周起点）。
// 本函数在 package review 内共用（tools.go 与 schedule.go）。
func mondayOf(t time.Time) time.Time {
	start, _ := dayRange(t)
	// time.Weekday: Sunday=0..Saturday=6；换成距周一的偏移（周一=0）
	offset := (int(start.Weekday()) + 6) % 7
	return start.AddDate(0, 0, -offset)
}

type generateReportArgs struct {
	Type   string `json:"type" jsonschema:"报告类型: daily|weekly|topic_status"`
	Target string `json:"target,omitempty" jsonschema:"daily/weekly 传日期 YYYY-MM-DD(空=今天/本周)；topic_status 传话题 id"`
}

func generateReportHandler(gen *Generator) func(context.Context, *mcp.CallToolRequest, generateReportArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a generateReportArgs) (*mcp.CallToolResult, any, error) {
		switch a.Type {
		case "daily":
			day := time.Now()
			if a.Target != "" {
				d, err := time.Parse("2006-01-02", a.Target)
				if err != nil {
					return nil, nil, fmt.Errorf("target 日期非法(需 YYYY-MM-DD): %w", err)
				}
				day = d
			}
			row, err := gen.Daily(ctx, day)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(row)
		case "weekly":
			base := time.Now()
			if a.Target != "" {
				d, err := time.Parse("2006-01-02", a.Target)
				if err != nil {
					return nil, nil, fmt.Errorf("target 日期非法(需 YYYY-MM-DD): %w", err)
				}
				base = d
			}
			row, err := gen.Weekly(ctx, mondayOf(base))
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(row)
		case "topic_status":
			tid, err := ids.ParseID(a.Target)
			if err != nil {
				return nil, nil, fmt.Errorf("target 需为话题 id: %w", err)
			}
			row, err := gen.TopicStatus(ctx, tid)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(row)
		default:
			return nil, nil, fmt.Errorf("未知报告类型: %q", a.Type)
		}
	}
}

type getTopicStatusArgs struct {
	TopicID string `json:"topic_id" jsonschema:"话题 id"`
}

func getTopicStatusHandler(gen *Generator) func(context.Context, *mcp.CallToolRequest, getTopicStatusArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getTopicStatusArgs) (*mcp.CallToolResult, any, error) {
		tid, err := ids.ParseID(a.TopicID)
		if err != nil {
			return nil, nil, fmt.Errorf("topic_id 非法: %w", err)
		}
		row, err := gen.TopicStatus(ctx, tid)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(row)
	}
}
