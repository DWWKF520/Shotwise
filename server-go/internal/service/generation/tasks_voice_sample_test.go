package service

import (
	"context"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/repository"
)

func TestVoiceSampleResourceID(t *testing.T) {
	if got := voiceSampleResourceID("阿明", "task-1"); got != "voice_sample__阿明__task-1" {
		t.Fatalf("resource id = %q", got)
	}
	// 角色名超长时按 UTF-8 字节截断，task_id 段保持完整。
	longName := strings.Repeat("喵", 60) // 每字 3 字节，60 字 = 180 字节 > 80
	got := voiceSampleResourceID(longName, "task-2")
	if !strings.HasPrefix(got, "voice_sample__") || !strings.HasSuffix(got, "__task-2") {
		t.Fatalf("resource id 形状漂移: %q", got)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(got, "voice_sample__"), "__task-2")
	if len(name) > 80 {
		t.Fatalf("角色名段应截到 ≤80 字节，得到 %d", len(name))
	}
}

func TestTruncateUTF8Bytes(t *testing.T) {
	cases := []struct {
		name  string
		input string
		limit int
		want  string
	}{
		{"短串原样", "阿明", 80, "阿明"},
		{"多字节截断丢残缺尾", "阿明", 4, "阿"}, // 6 字节，截 4 字节丢半个「明」
		{"截断点恰在边界", "阿明", 3, "阿"},
		{"空串", "", 10, ""},
		{"latin 原样", "abcdef", 3, "abc"},
	}
	for _, tc := range cases {
		if got := truncateUTF8Bytes(tc.input, tc.limit); got != tc.want {
			t.Fatalf("truncateUTF8Bytes(%q, %d) = %q, want %q", tc.input, tc.limit, got, tc.want)
		}
	}
}

func TestVoiceSampleTaskValidationOrder(t *testing.T) {
	dispatcher, store, projectName := newDispatcherFixture(t, 200)
	base := repository.TaskData{
		ProjectName: projectName,
		TaskType:    "voice_sample",
		TaskID:      "task-1",
		UserID:      repository.DefaultUserID,
	}

	cases := []struct {
		name string
		task repository.TaskData
		want string
	}{
		{"非法角色名", cloneTask(base, func(b *repository.TaskData) { b.ResourceID = "../x" }), "invalid character name"},
		{"空文本", cloneTask(base, func(b *repository.TaskData) {
			b.ResourceID = "阿明"
			b.Payload = map[string]any{"voice": "alibaba-qwen-tts-longxiaochun"}
		}), "payload.prompt"},
		{"空音色", cloneTask(base, func(b *repository.TaskData) {
			b.ResourceID = "阿明"
			b.Payload = map[string]any{"prompt": "你好"}
		}), "payload.voice"},
		{"缺 task_id", cloneTask(base, func(b *repository.TaskData) {
			b.ResourceID = "阿明"
			b.TaskID = ""
			b.Payload = map[string]any{"prompt": "你好", "voice": "v1"}
		}), "task_id"},
		{"角色不存在", cloneTask(base, func(b *repository.TaskData) {
			b.ResourceID = "路人"
			b.Payload = map[string]any{"prompt": "你好", "voice": "v1"}
		}), "character not found"},
	}
	for _, tc := range cases {
		task := tc.task
		if _, err := dispatcher.Execute(context.Background(), &task); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want 含 %q", tc.name, err, tc.want)
		}
	}
	_ = store
}

func cloneTask(base repository.TaskData, mutate func(*repository.TaskData)) repository.TaskData {
	task := base
	task.Payload = map[string]any{}
	for key, value := range base.Payload {
		task.Payload[key] = value
	}
	mutate(&task)
	return task
}
