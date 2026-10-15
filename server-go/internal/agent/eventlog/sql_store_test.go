package eventlog

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func testSQLStore(t *testing.T) *SQLStore {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&eventLogRow{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewSQLStore(gdb)
}

func entry(session, typ string) *Entry {
	return &Entry{
		UUID:      "uuid-" + session + "-" + typ,
		SessionID: session,
		Type:      EntryType(typ),
		Subtype:   SubtypeQuestion,
		Content:   []Block{{Type: "text", Text: "hello " + typ}},
	}
}

func TestSQLStore_AppendAssignsMonotonicSeq(t *testing.T) {
	ctx := context.Background()
	s := testSQLStore(t)

	seqs, err := s.Append(ctx, []*Entry{entry("s1", "user"), entry("s1", "assistant")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("seqs = %v, want [1 2]", seqs)
	}
	// 第二次追加从上次 seq 继续。
	seqs, err = s.Append(ctx, []*Entry{entry("s1", "tool_result")})
	if err != nil {
		t.Fatalf("append again: %v", err)
	}
	if len(seqs) != 1 || seqs[0] != 3 {
		t.Fatalf("seqs = %v, want [3]", seqs)
	}
}

func TestSQLStore_ListFiltersBySeqAndSession(t *testing.T) {
	ctx := context.Background()
	s := testSQLStore(t)
	if _, err := s.Append(ctx, []*Entry{
		entry("s1", "user"), entry("s1", "assistant"), entry("s1", "assistant"),
		entry("s2", "user"),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := s.List(ctx, "s1", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("list(s1) = %d entries, want 3", len(got))
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("entry[%d].Seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.SessionID != "s1" {
			t.Fatalf("entry[%d] session = %q, want s1", i, e.SessionID)
		}
	}
	tail, err := s.List(ctx, "s1", 2)
	if err != nil {
		t.Fatalf("list after cursor: %v", err)
	}
	if len(tail) != 1 || tail[0].Seq != 3 {
		t.Fatalf("list(after=2) = %+v, want single entry seq 3", tail)
	}
	none, err := s.List(ctx, "s2", 5)
	if err != nil {
		t.Fatalf("list other session: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("list(s2, after=5) = %+v, want empty", none)
	}
}

// 条目全字段往返：subtype / content 块 / tool_use 入参不能丢。
func TestSQLStore_PayloadRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testSQLStore(t)
	in := &Entry{
		UUID:            "u1",
		SessionID:       "s1",
		Type:            EntryAssistant,
		MessageID:       "msg-1",
		ParentToolUseID: "parent-1",
		ToolUseID:       "tool-1",
		Content: []Block{
			{Type: "text", Text: "调工具"},
			{Type: "tool_use", ID: "tc-1", Name: "echo", Input: []byte(`{"message":"hi"}`)},
		},
	}
	if _, err := s.Append(ctx, []*Entry{in}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.List(ctx, "s1", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list = %d entries, want 1", len(got))
	}
	e := got[0]
	if e.UUID != "u1" || e.Type != EntryAssistant || e.MessageID != "msg-1" ||
		e.ParentToolUseID != "parent-1" || e.ToolUseID != "tool-1" {
		t.Fatalf("scalar fields lost: %+v", e)
	}
	if len(e.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(e.Content))
	}
	if e.Content[1].Name != "echo" || string(e.Content[1].Input) != `{"message":"hi"}` {
		t.Fatalf("tool_use block lost: %+v", e.Content[1])
	}
	if e.Timestamp.IsZero() {
		t.Fatal("timestamp lost")
	}
}

func TestSQLStore_LastSeq(t *testing.T) {
	ctx := context.Background()
	s := testSQLStore(t)
	got, err := s.LastSeq(ctx, "s1")
	if err != nil || got != 0 {
		t.Fatalf("LastSeq(empty) = (%d, %v), want (0, nil)", got, err)
	}
	if _, err := s.Append(ctx, []*Entry{entry("s1", "user"), entry("s1", "assistant")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err = s.LastSeq(ctx, "s1")
	if err != nil || got != 2 {
		t.Fatalf("LastSeq = (%d, %v), want (2, nil)", got, err)
	}
}

// 重开一个 store（模拟进程重启）后历史仍在，且 seq 从库里的最大值继续。
func TestSQLStore_SurvivesReopen(t *testing.T) {
	ctx := context.Background()
	gdb, err := gorm.Open(sqlite.Open("file:eventlog?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&eventLogRow{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := NewSQLStore(gdb).Append(ctx, []*Entry{entry("s1", "user")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// 新 Store 实例共享同一张表：历史可读，新条目 seq 接着走。
	s := NewSQLStore(gdb)
	seqs, err := s.Append(ctx, []*Entry{entry("s1", "assistant")})
	if err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	if len(seqs) != 1 || seqs[0] != 2 {
		t.Fatalf("seqs after reopen = %v, want [2]", seqs)
	}
	got, err := s.List(ctx, "s1", 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("list after reopen = (%d entries, %v), want 2", len(got), err)
	}
}
