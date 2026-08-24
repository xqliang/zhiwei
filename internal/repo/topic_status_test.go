package repo

import (
	"encoding/json"
	"testing"

	"zhiwei/internal/ids"
)

func TestTopicStatusInsertAndGetLatest(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	sr := &TopicStatusRepo{DB: db}
	ctx := t.Context()
	topicID := ids.New() // 唯一，隔离本用例快照

	if err := sr.Insert(ctx, 1, topicID, json.RawMessage(`{"summary":"第一次"}`)); err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	if err := sr.Insert(ctx, 1, topicID, json.RawMessage(`{"summary":"第二次"}`)); err != nil {
		t.Fatalf("Insert 2: %v", err)
	}

	got, err := sr.GetLatest(ctx, topicID)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if got == nil || got.Content == nil {
		t.Fatal("GetLatest 返回空或 content 为 nil")
	}
	var body struct{ Summary string }
	if err := json.Unmarshal(*got.Content, &body); err != nil {
		t.Fatalf("content 非合法 JSON: %v", err)
	}
	if body.Summary != "第二次" {
		t.Errorf("应取最新快照, got %q", body.Summary)
	}

	// 无快照的 topic 返回 (nil, nil)
	none, err := sr.GetLatest(ctx, ids.New())
	if err != nil {
		t.Fatalf("GetLatest none: %v", err)
	}
	if none != nil {
		t.Error("无快照应返回 nil")
	}
}
