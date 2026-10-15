package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// 端到端：read_file 经 guard + filesystem 中间件读取项目内文件，回包非错误且含内容。
func TestFilesystemReadThroughRuntime(t *testing.T) {
	m := &scriptedModel{script: []*schema.Message{
		toolCallMsg("f1", "read_file", `{"file_path":"notes.txt"}`),
		textMsg("done"),
	}}
	r := newTestRuntime(t, m)
	writeProjectClaude(t, r.dataDir, "proj")
	if err := os.WriteFile(filepath.Join(r.dataDir, "proj", "notes.txt"), []byte("MARKER-123"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, ch, unsub, err := r.Query(context.Background(), "fs-s1", "proj", "read notes")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	evs := drainEvents(t, ch)

	var ok bool
	for _, ev := range evs {
		if ev.Type == EventToolResult && ev.ToolName == "read_file" && !ev.IsError && strings.Contains(ev.Text, "MARKER-123") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("read_file result with content not found; events=%+v", evs)
	}
}

// 围栏拒绝敏感路径（.env）：回包为 is_error，且不中断整轮（模型仍收到最终回答）。
func TestFilesystemGuardDeniesSensitivePathWithoutAbortingTurn(t *testing.T) {
	m := &scriptedModel{script: []*schema.Message{
		toolCallMsg("f2", "read_file", `{"file_path":".env"}`),
		textMsg("final-answer"),
	}}
	r := newTestRuntime(t, m)
	writeProjectClaude(t, r.dataDir, "proj")
	if err := os.WriteFile(filepath.Join(r.dataDir, "proj", ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, ch, unsub, err := r.Query(context.Background(), "fs-s2", "proj", "read env")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	evs := drainEvents(t, ch)

	var denied, finished bool
	for _, ev := range evs {
		if ev.Type == EventToolResult && ev.ToolName == "read_file" && ev.IsError {
			denied = true
		}
		if ev.Type == EventDelta && strings.Contains(ev.Text, "final-answer") {
			finished = true
		}
	}
	if !denied {
		t.Fatalf("sensitive read must be denied; events=%+v", evs)
	}
	if !finished {
		t.Fatalf("turn must continue after a denied tool call; events=%+v", evs)
	}
}

// 编辑不存在的匹配串：工具错误回灌模型（is_error），整轮正常结束。
func TestFilesystemToolErrorIsReturnedToModel(t *testing.T) {
	m := &scriptedModel{script: []*schema.Message{
		toolCallMsg("f3", "edit_file", `{"file_path":"notes.txt","old_string":"absent","new_string":"x"}`),
		textMsg("recovered"),
	}}
	r := newTestRuntime(t, m)
	writeProjectClaude(t, r.dataDir, "proj")
	if err := os.WriteFile(filepath.Join(r.dataDir, "proj", "notes.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, ch, unsub, err := r.Query(context.Background(), "fs-s3", "proj", "edit")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	evs := drainEvents(t, ch)

	var errResult, recovered bool
	for _, ev := range evs {
		if ev.Type == EventToolResult && ev.ToolName == "edit_file" && ev.IsError {
			errResult = true
		}
		if ev.Type == EventDelta && strings.Contains(ev.Text, "recovered") {
			recovered = true
		}
	}
	if !errResult || !recovered {
		t.Fatalf("edit failure must be fed back and turn must finish; events=%+v", evs)
	}
}
