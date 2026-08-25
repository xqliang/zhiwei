package api

import "zhiwei/internal/ids"

// idPtr 返回 ids.ID 的指针，用于构造带可空 session_id/其他可空 ID 字段的测试夹具。
func idPtr(id ids.ID) *ids.ID { return &id }
