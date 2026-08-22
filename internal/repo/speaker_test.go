package repo

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
)

// TestSpeakerCRUD 覆盖 SpeakerRepo 的增删改查全链路（真实 MySQL）：
// 创建回填雪花 ID、Get 读回、UpdateName / UpdateEmbedding 改字段、
// List 只返回 active、Delete 后查不到。未设 TEST_MYSQL_DSN 时经 TestDSN 跳过。
func TestSpeakerCRUD(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &SpeakerRepo{DB: db}
	ctx := context.Background()

	// 创建：Source/Status 留空由 Create 兜底默认；embedding 存 4 字节占位
	sp := &Speaker{Name: "说话人ab12c", Source: "auto", Embedding: []byte{0, 0, 0, 0}, SampleCount: 1}
	if err := r.Create(ctx, sp); err != nil {
		t.Fatalf("create: %v", err)
	}
	if sp.ID == 0 {
		t.Fatal("id 未回填")
	}

	// Get：字段读回正确，UserID/Status 默认值已落库
	got, err := r.Get(ctx, sp.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "说话人ab12c" || got.Source != "auto" {
		t.Fatalf("got %+v", got)
	}
	if got.UserID != 1 || got.Status != "active" || got.SampleCount != 1 {
		t.Fatalf("默认值/字段异常: %+v", got)
	}
	if !bytes.Equal(got.Embedding, []byte{0, 0, 0, 0}) {
		t.Fatalf("embedding 读回不一致: %v", got.Embedding)
	}

	// UpdateName：改名后应可读回新名
	if err := r.UpdateName(ctx, sp.ID, "张三"); err != nil {
		t.Fatalf("updateName: %v", err)
	}
	if g2, _ := r.Get(ctx, sp.ID); g2.Name != "张三" {
		t.Fatalf("name=%s", g2.Name)
	}

	// UpdateEmbedding：声纹重建后灾备 BLOB 应被覆盖
	if err := r.UpdateEmbedding(ctx, sp.ID, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("updateEmbedding: %v", err)
	}
	if g3, _ := r.Get(ctx, sp.ID); !bytes.Equal(g3.Embedding, []byte{1, 2, 3, 4}) {
		t.Fatalf("embedding 未更新: %v", g3.Embedding)
	}

	// List：active 名册非空（含刚建的这条）
	list, err := r.List(ctx)
	if err != nil || len(list) == 0 {
		t.Fatalf("list: %v %d", err, len(list))
	}

	// Delete：删除后再 Get 应返回 sql.ErrNoRows
	if err := r.Delete(ctx, sp.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Get(ctx, sp.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("删除后仍可查到: err=%v", err)
	}
}
