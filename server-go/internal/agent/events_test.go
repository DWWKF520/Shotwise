package agent

import (
	"strings"
	"testing"
)

func TestEvent_EncodeSSE_Frame(t *testing.T) {
	ev := Event{Seq: 7, SessionID: "s1", Type: EventEntry, Raw: []byte(`{"seq":7,"type":"user"}`)}
	got := ev.EncodeSSE()
	want := "event: entry\nid: 7\ndata: {\"seq\":7,\"type\":\"user\"}\n\n"
	if got != want {
		t.Fatalf("EncodeSSE() = %q, want %q", got, want)
	}
}

// 无条目事件（delta 等）不带 id 字段，避免污染 Last-Event-ID 游标。
func TestEvent_EncodeSSE_NoIDWithoutSeq(t *testing.T) {
	got := Event{Seq: 0, SessionID: "s1", Type: EventDelta, Text: "hi"}.EncodeSSE()
	if strings.Contains(got, "\nid:") {
		t.Fatalf("frame carries id for seq-less event: %q", got)
	}
	if !strings.HasPrefix(got, "event: delta\n") {
		t.Fatalf("frame missing event name: %q", got)
	}
	if !strings.Contains(got, `"text":"hi"`) {
		t.Fatalf("frame missing payload: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("frame not terminated by blank line: %q", got)
	}
}
