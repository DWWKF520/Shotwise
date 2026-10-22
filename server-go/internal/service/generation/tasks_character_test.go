package generation

import "github.com/shotwise/shotwise/internal/service"

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/img"
	"github.com/shotwise/shotwise/internal/repository"
)

func characterTask(resourceID string, payload map[string]any) *repository.TaskData {
	return &repository.TaskData{
		TaskID:      "task-abc-123",
		ProjectName: "p1",
		TaskType:    "character",
		MediaType:   "image",
		ResourceID:  resourceID,
		Payload:     payload,
		UserID:      repository.DefaultUserID,
	}
}

// seedCharacter 往项目角色桶写入一条最小条目（description 是「不更坏」守卫的必填字段）。
func seedCharacter(t *testing.T, store *service.ProjectStore, projectName, name string, fields map[string]string) {
	t.Helper()
	if err := store.Update(projectName, func(p *domain.ProjectFile) error {
		bucket, ok := p.BucketRef(domain.AssetCharacter)
		if !ok {
			t.Fatal("character bucket missing")
		}
		if *bucket == nil {
			*bucket = domain.AssetBucket{}
		}
		key := domain.NormalizeAssetName(name)
		entry, ok := (*bucket)[key]
		if !ok {
			entry = domain.NewAssetEntry()
			entry.SetString("description", "测试角色")
			(*bucket)[key] = entry
		}
		for field, value := range fields {
			entry.SetString(field, value)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed character: %v", err)
	}
}

func TestCharacterTaskEndToEnd(t *testing.T) {
	dispatcher, store, projectName := newDispatcherFixture(t, 200)
	seedCharacter(t, store, projectName, "阿明", nil)
	projectDir, err := store.Dir(projectName)
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}

	result, err := dispatcher.Execute(context.Background(), characterTask("阿明", map[string]any{
		"prompt": "少年剑客，蓝白色调",
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result["file_path"] != "characters/阿明.png" {
		t.Fatalf("unexpected file_path: %v", result["file_path"])
	}

	// 设计图与头像都落盘；头像为 256×256 PNG。
	if _, err := os.Stat(filepath.Join(projectDir, "characters", "阿明.png")); err != nil {
		t.Fatalf("sheet missing: %v", err)
	}
	avatarPath := filepath.Join(projectDir, "characters", "阿明_avatar_taskabc123.png")
	raw, err := os.ReadFile(avatarPath)
	if err != nil {
		t.Fatalf("avatar missing: %v", err)
	}
	width, height, err := img.DecodeConfig(raw)
	if err != nil || width != img.AvatarSize || height != img.AvatarSize {
		t.Fatalf("avatar size = %dx%d err=%v，期望 256×256 PNG", width, height, err)
	}

	// 字段写回：sheet 与头像路径都指向本次产物。
	pf, err := store.Get(projectName)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	characters, _ := pf.BucketRef(domain.AssetCharacter)
	entry := (*characters)[domain.NormalizeAssetName("阿明")]
	if got := entry.MustString("character_sheet"); got != "characters/阿明.png" {
		t.Fatalf("character_sheet = %q", got)
	}
	if got := entry.MustString("character_avatar"); got != "characters/阿明_avatar_taskabc123.png" {
		t.Fatalf("character_avatar = %q", got)
	}
}

func TestCharacterTaskStaleAvatarGuard(t *testing.T) {
	_, store, projectName := newDispatcherFixture(t, 200)
	seedCharacter(t, store, projectName, "阿明", map[string]string{
		"character_avatar": "characters/阿明_avatar_manual.png",
	})
	projectDir := mustProjectDir(t, store, projectName)
	if err := os.MkdirAll(filepath.Join(projectDir, "characters"), 0o755); err != nil {
		t.Fatal(err)
	}
	manualPath := filepath.Join(projectDir, "characters", "阿明_avatar_manual.png")
	if err := os.WriteFile(manualPath, []byte("manual"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 桩在返回产物前抢先改写头像字段，模拟并发任务在执行器生成期间赢下写回竞态。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = store.Update(projectName, func(p *domain.ProjectFile) error {
			b, _ := p.BucketRef(domain.AssetCharacter)
			(*b)[domain.NormalizeAssetName("阿明")].SetString("character_avatar", "characters/阿明_avatar_concurrent.png")
			return nil
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"b64_json": minimalPNGBase64}},
		})
	}))
	t.Cleanup(srv.Close)
	generator, _, _ := newImageGeneratorFixtureWithStub(t, srv)
	dispatcher := NewTaskDispatcher(store, generator)

	if _, err := dispatcher.Execute(context.Background(), characterTask("阿明", map[string]any{
		"prompt": "少年剑客",
	})); err != nil {
		t.Fatalf("execute: %v", err)
	}

	pf, err := store.Get(projectName)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	characters, _ := pf.BucketRef(domain.AssetCharacter)
	if got := (*characters)[domain.NormalizeAssetName("阿明")].MustString("character_avatar"); got != "characters/阿明_avatar_concurrent.png" {
		t.Fatalf("并发赢家的头像应保留，得到 %q", got)
	}
	// 落败方（本次任务）的头像文件被清理；手动上传的文件不受影响。
	entries, _ := filepath.Glob(filepath.Join(projectDir, "characters", "阿明_avatar_task*.png"))
	if len(entries) != 0 {
		t.Fatalf("落败方头像应被清理: %v", entries)
	}
	if _, err := os.Stat(manualPath); err != nil {
		t.Fatalf("手动上传文件不应被触碰: %v", err)
	}
}

func TestCharacterAvatarResourceID(t *testing.T) {
	if got := characterAvatarResourceID("阿明", "task-abc-1234567890"); got != "阿明_avatar_taskabc12345" {
		t.Fatalf("resource id = %q", got)
	}
	if got := characterAvatarResourceID("阿明", ""); !strings.HasPrefix(got, "阿明_avatar_") || len(got) != len("阿明_avatar_")+12 {
		t.Fatalf("缺 task_id 时应退化为 12 位 uuid 后缀: %q", got)
	}
}

func TestShouldRetirePreviousAvatar(t *testing.T) {
	cases := []struct {
		previous string
		want     bool
	}{
		{"characters/阿明_avatar_ab12cd34ef56.png", true},
		{"", false},
		{"characters/阿明_avatar_manual.png", false},
		{"characters/阿明_avatar_new.png", false}, // == current 时不可删
		{"characters/refs/阿明_avatar_ab12cd34ef56.png", true},
		{"characters/阿明.png", false},
	}
	for _, tc := range cases {
		got := shouldRetirePreviousAvatar(tc.previous, "阿明", "characters/阿明_avatar_new.png")
		if got != tc.want {
			t.Fatalf("shouldRetirePreviousAvatar(%q) = %v, want %v", tc.previous, got, tc.want)
		}
	}
}

func mustProjectDir(t *testing.T, store *service.ProjectStore, projectName string) string {
	t.Helper()
	dir, err := store.Dir(projectName)
	if err != nil {
		t.Fatalf("project dir: %v", err)
	}
	return dir
}
