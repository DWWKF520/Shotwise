package checkpoint

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func testSQLStore(t *testing.T) *SQLStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	s, err := NewSQLStore(db)
	if err != nil {
		t.Fatalf("new sql store: %v", err)
	}
	return s
}

func TestSQLStore_SetGetDelete(t *testing.T) {
	ctx := context.Background()
	s := testSQLStore(t)

	// 不存在
	_, existed, err := s.Get(ctx, "sess_1")
	if err != nil || existed {
		t.Fatalf("Get(missing) = (%v, %v), want (nil, false)", existed, err)
	}

	// 写入
	if err := s.Set(ctx, "sess_1", []byte("payload-1")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	data, existed, err := s.Get(ctx, "sess_1")
	if err != nil || !existed || string(data) != "payload-1" {
		t.Fatalf("Get = (%q, %v, %v), want (payload-1, true, nil)", data, existed, err)
	}

	// 覆盖（upsert）
	if err := s.Set(ctx, "sess_1", []byte("payload-2")); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	data, _, _ = s.Get(ctx, "sess_1")
	if string(data) != "payload-2" {
		t.Fatalf("Get after overwrite = %q, want payload-2", data)
	}

	// 删除
	if err := s.Delete(ctx, "sess_1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, existed, _ = s.Get(ctx, "sess_1")
	if existed {
		t.Fatal("Get after delete: existed = true, want false")
	}
}

func TestMemStore_Basic(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	if err := s.Set(ctx, "a", []byte("x")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	data, existed, err := s.Get(ctx, "a")
	if err != nil || !existed || string(data) != "x" {
		t.Fatalf("Get = (%q, %v, %v)", data, existed, err)
	}
	// 隔离：写入的是副本，读出也是副本
	data[0] = 'y'
	again, _, _ := s.Get(ctx, "a")
	if string(again) != "x" {
		t.Fatalf("Get returned aliased buffer: %q", again)
	}
	_ = s.Delete(ctx, "a")
	if _, existed, _ := s.Get(ctx, "a"); existed {
		t.Fatal("deleted key still exists")
	}
}
