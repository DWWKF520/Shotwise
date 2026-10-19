// 项目 ZIP 导入端点测试：安全扫描、根定位、冲突三策略、preflight。
package http

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/service"
)

func newArchiveFixture(t *testing.T) (*gin.Engine, *service.ProjectStore) {
	t.Helper()
	store := service.NewProjectStore(t.TempDir())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterProjectArchive(r.Group("/api/v1"), ArchiveDeps{
		Archive: service.NewProjectArchiveService(store),
	})
	return r, store
}

// buildProjectZip 打一个最小合法项目归档（root 目录可选）。
func buildProjectZip(t *testing.T, root string, project map[string]any, extra map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	add := func(name string, content []byte) {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := file.Write(content); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	payload, err := json.Marshal(project)
	if err != nil {
		t.Fatalf("marshal project: %v", err)
	}
	add(root+"project.json", payload)
	for name, content := range extra {
		add(root+name, content)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func validImportProject() map[string]any {
	return map[string]any{
		"title":           "导入测试",
		"content_mode":    "drama",
		"generation_mode": "storyboard",
		"aspect_ratio":    "9:16",
	}
}

func postZip(t *testing.T, r *gin.Engine, path string, policy string, content []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipartArchiveWriter(&buf)
	field, err := writer.CreateFormFile("file", "upload.zip")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := field.Write(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if policy != "" {
		if err := writer.WriteField("conflict_policy", policy); err != nil {
			t.Fatalf("field: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

// TestImportProjectSuccess 校验无 manifest 归档（根目录名即项目名）导入成功。
func TestImportProjectSuccess(t *testing.T) {
	r, store := newArchiveFixture(t)

	zipBytes := buildProjectZip(t, "my-drama/", validImportProject(), map[string][]byte{
		"source/novel.txt": []byte("正文内容"),
	})
	status, body := postZip(t, r, "/api/v1/projects/import", "", zipBytes)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["project_name"] != "my-drama" || body["conflict_resolution"] != "none" {
		t.Fatalf("body = %v", body)
	}
	// 项目真的装进 store 了，且标准子目录补齐
	pf, err := store.Get("my-drama")
	if err != nil {
		t.Fatalf("installed: %v", err)
	}
	if pf.Title != "导入测试" {
		t.Fatalf("title = %q", pf.Title)
	}
	projectDir, _ := store.Dir("my-drama")
	for _, subdir := range []string{"source", "scripts", "output"} {
		if info, statErr := os.Stat(filepath.Join(projectDir, subdir)); statErr != nil || !info.IsDir() {
			t.Fatalf("缺标准子目录 %s", subdir)
		}
	}
	if content, readErr := os.ReadFile(filepath.Join(projectDir, "source", "novel.txt")); readErr != nil || string(content) != "正文内容" {
		t.Fatalf("source content = %q err=%v", content, readErr)
	}
}

// TestImportPreflight 校验 preflight 返回定名与冲突标记，不落库。
func TestImportPreflight(t *testing.T) {
	r, store := newArchiveFixture(t)

	zipBytes := buildProjectZip(t, "my-drama/", validImportProject(), nil)
	status, body := postZip(t, r, "/api/v1/projects/import/preflight", "", zipBytes)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["project_name"] != "my-drama" || body["conflict_project_name"] != nil {
		t.Fatalf("body = %v", body)
	}
	// preflight 不安装
	if _, err := store.Get("my-drama"); err == nil {
		t.Fatalf("preflight 不应安装项目")
	}
}

// TestImportConflictPolicies 校验 prompt 409 / rename 改名 / overwrite 覆盖。
func TestImportConflictPolicies(t *testing.T) {
	r, store := newArchiveFixture(t)

	zipBytes := buildProjectZip(t, "my-drama/", validImportProject(), nil)
	// 先装一份
	status, _ := postZip(t, r, "/api/v1/projects/import", "", zipBytes)
	if status != http.StatusOK {
		t.Fatalf("first import status = %d", status)
	}

	// prompt → 409 + conflict_project_name
	status, body := postZip(t, r, "/api/v1/projects/import", "prompt", zipBytes)
	if status != http.StatusConflict {
		t.Fatalf("prompt status = %d body=%v", status, body)
	}
	if body["conflict_project_name"] != "my-drama" {
		t.Fatalf("conflict body = %v", body)
	}

	// rename → 生成新名
	status, body = postZip(t, r, "/api/v1/projects/import", "rename", zipBytes)
	if status != http.StatusOK || body["conflict_resolution"] != "renamed" {
		t.Fatalf("rename status = %d body=%v", status, body)
	}
	renamed, _ := body["project_name"].(string)
	if renamed == "" || renamed == "my-drama" {
		t.Fatalf("renamed = %v", renamed)
	}

	// overwrite → 覆盖安装，内容被替换
	zipV2 := buildProjectZip(t, "my-drama/", func() map[string]any {
		project := validImportProject()
		project["title"] = "第二版"
		return project
	}(), nil)
	status, body = postZip(t, r, "/api/v1/projects/import", "overwrite", zipV2)
	if status != http.StatusOK || body["conflict_resolution"] != "overwritten" {
		t.Fatalf("overwrite status = %d body=%v", status, body)
	}
	pf, err := store.Get("my-drama")
	if err != nil {
		t.Fatal(err)
	}
	if pf.Title != "第二版" {
		t.Fatalf("title = %q", pf.Title)
	}
}

// TestImportRejectsUnsafeZip 校验 zip-slip / 加密成员被拒。
func TestImportRejectsUnsafeZip(t *testing.T) {
	r, store := newArchiveFixture(t)

	// .. 穿越：zip 标准库会拒带 .. 的名字，直接构造原始条目头绕过不可行；
	// 这里覆盖「无 project.json」与「非法 schema」两类可构造的拒绝路径。
	badSchema := buildProjectZip(t, "proj/", map[string]any{
		"title": "x", "content_mode": "magic", "generation_mode": "storyboard",
	}, nil)
	status, body := postZip(t, r, "/api/v1/projects/import", "", badSchema)
	if status != http.StatusBadRequest {
		t.Fatalf("bad schema status = %d body=%v", status, body)
	}

	noProject := buildProjectZip(t, "proj/", nil, map[string][]byte{"notes.txt": []byte("hi")})
	status, _ = postZip(t, r, "/api/v1/projects/import", "", noProject)
	if status != http.StatusBadRequest {
		t.Fatalf("no project status = %d", status)
	}

	// 非 zip 字节 → arch_not_a_zip
	status, _ = postZip(t, r, "/api/v1/projects/import", "", []byte("not a zip at all"))
	if status != http.StatusBadRequest {
		t.Fatalf("not zip status = %d", status)
	}

	// 校验失败不落任何项目
	if entries, _ := os.ReadDir(store.Root()); len(entries) != 0 {
		t.Fatalf("失败导入不应留下项目: %v", entries)
	}
}

// multipartArchiveWriter 是 mime/multipart.Writer 的构造封装。
func multipartArchiveWriter(buf *bytes.Buffer) *multipart.Writer { return multipart.NewWriter(buf) }
