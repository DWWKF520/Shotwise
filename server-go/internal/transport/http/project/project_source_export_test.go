// 源文上传与导出端点测试。
package project

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
	"github.com/shotwise/shotwise/internal/transport/http/httpkit/httpkittest"

	"github.com/shotwise/shotwise/internal/auth"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/service"
)

func newSourceExportFixture(t *testing.T) (*gin.Engine, *service.ProjectStore) {
	t.Helper()
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", project.ContentDrama, project.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create: %v", err)
	}
	authService := auth.New([]byte("test-secret-key-32-bytes-long!!"), false, nil)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	authed := v1.Group("", authService.Middleware())
	RegisterProjectSourceExport(v1, authed, NewSourceExportDeps(store, authService, nil))
	return r, store
}

// postMultipartField 通用 multipart 提交。
func postSourceForm(t *testing.T, r *gin.Engine, path string, fields map[string]string, fileName string, fileContent []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("field %s: %v", key, err)
		}
	}
	if fileContent != nil {
		part, err := writer.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		if _, err := part.Write(fileContent); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
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

// TestSetSourceContent 校验文本直传（novel.txt 命名 + 字符计数）。
func TestSetSourceContent(t *testing.T) {
	r, store := newSourceExportFixture(t)

	status, body := postSourceForm(t, r, "/api/v1/projects/p1/source",
		map[string]string{"content": "第一章 天启\n这是正文。", "generate_overview": "false"}, "", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
	if body["filename"] != "novel.txt" || body["chars"] != float64(len([]rune("第一章 天启\n这是正文。"))) {
		t.Fatalf("body = %v", body)
	}
	// 落盘确认（概述未触发：无文本后端）
	projectDir, err := store.Dir("p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "source", "novel.txt")); statErr != nil {
		t.Fatalf("source file missing: %v", statErr)
	}

	// content 与 file 互斥 → 400
	status, _ = postSourceForm(t, r, "/api/v1/projects/p1/source",
		map[string]string{"content": "text", "generate_overview": "false"}, "a.txt", []byte("file text"))
	if status != http.StatusBadRequest {
		t.Fatalf("both status = %d", status)
	}
	// 都不提供 → 400
	status, _ = postSourceForm(t, r, "/api/v1/projects/p1/source",
		map[string]string{"generate_overview": "false"}, "", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("none status = %d", status)
	}
	// 非法后缀 → 400
	status, _ = postSourceForm(t, r, "/api/v1/projects/p1/source",
		map[string]string{"generate_overview": "false"}, "evil.docx", []byte("binary"))
	if status != http.StatusBadRequest {
		t.Fatalf("bad suffix status = %d", status)
	}
	// 非 UTF-8 → 400
	status, _ = postSourceForm(t, r, "/api/v1/projects/p1/source",
		map[string]string{"generate_overview": "false"}, "gbk.txt", []byte{0xD6, 0xD0, 0xCE, 0xC4})
	if status != http.StatusBadRequest {
		t.Fatalf("gbk status = %d", status)
	}
}

// TestExportTokenAndArchive 校验 token 签发 + ZIP 下载全链路；错项目 token 被拒。
func TestExportTokenAndArchive(t *testing.T) {
	r, store := newSourceExportFixture(t)

	// 落一个源文件进项目目录，让导出包含可校验内容
	projectDir, err := store.Dir("p1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "source", "novel.txt"), []byte("hello 源文"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httpkittest.PostJSON(t, r, "/api/v1/projects/p1/export/token?scope=full", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("token status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	token, _ := body["download_token"].(string)
	if token == "" || body["expires_in"] != float64(auth.DownloadTokenExpirySeconds) {
		t.Fatalf("body = %v", body)
	}

	// 带 token 下载
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/export?download_token="+token+"&scope=full", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("export status = %d body=%s", w.Code, w.Body.String())
	}
	zipReader, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip open: %v", err)
	}
	found := false
	for _, file := range zipReader.File {
		if file.Name == "source/novel.txt" {
			found = true
			content, _ := readZipFile(file)
			if string(content) != "hello 源文" {
				t.Fatalf("content = %q", content)
			}
		}
	}
	if !found {
		t.Fatalf("zip 缺 source/novel.txt: %v", zipReader.File)
	}

	// 拿 p1 的 token 下 ghost 项目 → 401（purpose/project 双校验）
	req = httptest.NewRequest(http.MethodGet, "/api/v1/projects/ghost/export?download_token="+token, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("cross-project status = %d", w.Code)
	}

	// 无 token → 401
	req = httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/export", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d", w.Code)
	}
}

// readZipFile 读 zip 条目内容。
func readZipFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
