// list.go 提供 ID 的 JSON 数组类型，用于 memory.transcript_segment_ids 这类
// 「DB 存 JSON 文本、API 输出字符串数组」的列。
package ids

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// List 是 ID 数组。DB 侧序列化为 JSON 文本（写 MySQL JSON 列），
// API 侧输出 ["123","456"] 字符串数组（元素仍走 ID 的字符串序列化）。
type List []ID

// Value 实现 driver.Valuer：写库时序列化为 JSON 文本（string）；nil 写 NULL。
func (l List) Value() (driver.Value, error) {
	if l == nil {
		return nil, nil
	}
	b, err := l.toJSON()
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan 实现 sql.Scanner：从 JSON 文本（[]byte 或 string）还原。
func (l *List) Scan(src any) error {
	if src == nil {
		*l = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("ids.List.Scan: 不支持的源类型 %T", src)
	}
	return json.Unmarshal(raw, (*[]ID)(l))
}

// MarshalJSON 输出字符串数组；nil 输出 null。
func (l List) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("null"), nil
	}
	return l.toJSON()
}

// UnmarshalJSON 接受字符串数组。
func (l *List) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, (*[]ID)(l))
}

func (l List) toJSON() ([]byte, error) {
	return json.Marshal([]ID(l))
}

// 编译期断言接口实现完整。
var _ driver.Valuer = List(nil)
var _ sql.Scanner = (*List)(nil)
