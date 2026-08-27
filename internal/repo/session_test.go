package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func newTestSession(id ids.ID) *AudioSession {
	return &AudioSession{
		ID: id, UserID: 1, Source: "web_upload", Filename: "a.wav",
		StoragePath: "/tmp/a.wav", Mime: "audio/wav", Status: "uploaded",
	}
}

func TestSessionCreateGet(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &SessionRepo{DB: db}
	id := ids.New()
	ctx := context.Background()

	if err := r.Create(ctx, newTestSession(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, 1, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Filename != "a.wav" || got.Status != "uploaded" {
		t.Fatalf("got %+v", got)
	}
}

func TestSessionListAndUpdateStatus(t *testing.T) {
	db, _ := NewDB(repotest.DSN(t))
	r := &SessionRepo{DB: db}
	id := ids.New()
	ctx := context.Background()
	if err := r.Create(ctx, newTestSession(id)); err != nil {
		t.Fatal(err)
	}

	if err := r.UpdateStatus(ctx, id, "processing"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	list, err := r.List(ctx, 1, 20, 0)
	if err != nil || len(list) == 0 {
		t.Fatalf("List: %v len=%d", err, len(list))
	}
}
