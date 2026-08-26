package repo

import (
	"testing"
	"zhiwei/internal/repotest"
)

func TestNewDBPing(t *testing.T) {
	dsn := repotest.DSN(t) // 无 TEST_MYSQL_DSN 时 Skip
	db, err := NewDB(dsn)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
