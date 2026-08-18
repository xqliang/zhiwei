// Package ids 提供雪花 ID。雪花 ID 超过 JS 2^53 精度上限，
// 因此 ID 类型在 JSON 中始终序列化为字符串，前后端统一按 string 处理。
package ids

import (
	"strconv"

	"github.com/bwmarrin/snowflake"
)

// ID 是业务主键类型。数据库列 BIGINT，JSON 字符串。
type ID int64

var node *snowflake.Node

// Init 初始化雪花节点。nodeID 取 0-1023，单体单进程固定用 1；
// 未来拆多实例时按服务分配不同 nodeID 即可。
func Init(nodeID int64) error {
	n, err := snowflake.NewNode(nodeID)
	if err != nil {
		return err
	}
	node = n
	return nil
}

// New 生成一个新 ID。Init 未调用时 panic（属于启动装配错误）。
func New() ID {
	return ID(node.Generate().Int64())
}

func (i ID) Int64() int64   { return int64(i) }
func (i ID) String() string { return strconv.FormatInt(int64(i), 10) }

// MarshalJSON 序列化为 JSON 字符串，规避前端精度丢失。
func (i ID) MarshalJSON() ([]byte, error) {
	return []byte(`"` + i.String() + `"`), nil
}

// UnmarshalJSON 接受带引号字符串或不带引号数字。
func (i *ID) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*i = ID(v)
	return nil
}
