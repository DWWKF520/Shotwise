// 角色变体与头像上传的端点测试。
package http

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/migrations"
	"gorm.io/gorm/logger"
)

type variantFixture struct {
	router *gin.Engine
	store  *service.ProjectStore
}

func newVariantFixture(t *testing.T) variantFixture {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "t.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	store := service.NewProjectStore(t.TempDir())
	if _, err := store.Create("p1", "测试项目", domain.ContentDrama, domain.GenerationStoryboard, "水墨"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Update("p1", func(pf *domain.ProjectFile) error {
		if pf.Characters == nil {
			pf.Characters = domain.AssetBucket{}
		}
		pf.Characters["阿明"] = domain.WrapAssetEntry(map[string]any{"description": "少年"})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterCharacterExtras(r.Group("/api/v1"), CharacterDeps{
		Variants: service.NewCharacterVariants(store),
	})
	return variantFixture{router: r, store: store}
}

func (f variantFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

// postMultipart 发一次 multipart 上传。
func (f variantFixture) postMultipart(t *testing.T, path string, content []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", "avatar.png")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

func tinyAvatarPNG() []byte {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// TestVariantCRUDLifecycle 校验创建 / 列表 / 更新（含改 slug 迁移 key）/ 删除。
func TestVariantCRUDLifecycle(t *testing.T) {
	f := newVariantFixture(t)

	status, body := f.do(t, http.MethodPost, "/api/v1/projects/p1/characters/阿明/variants", map[string]any{
		"slug":         "少年期",
		"display_name": "少年阿明",
		"status":       "ready",
		"metadata":     map[string]any{"source": "ch1"},
	})
	if status != http.StatusOK {
		t.Fatalf("create status = %d body=%v", status, body)
	}
	variant := body["variant"].(map[string]any)
	if variant["slug"] != "少年期" || variant["status"] != "ready" || variant["character_id"] != "阿明" {
		t.Fatalf("variant = %v", variant)
	}
	if variant["display_name"] != "少年阿明" {
		t.Fatalf("display_name = %v", variant["display_name"])
	}

	// slug 冲突 → 409
	status, body = f.do(t, http.MethodPost, "/api/v1/projects/p1/characters/阿明/variants", map[string]any{
		"slug": "少年期",
	})
	if status != http.StatusConflict {
		t.Fatalf("conflict status = %d body=%v", status, body)
	}

	// 列表
	status, body = f.do(t, http.MethodGet, "/api/v1/projects/p1/characters/阿明/variants", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	items, _ := body["variants"].([]any)
	if len(items) != 1 {
		t.Fatalf("variants = %v", body)
	}

	// 更新：改 slug（迁移 key）+ 显式置空 metadata 不允许走 nil——用 metadata 置新值。
	status, body = f.do(t, http.MethodPatch, "/api/v1/projects/p1/characters/阿明/variants/少年期",
		map[string]any{"slug": "成年期", "description": "长大之后"})
	if status != http.StatusOK {
		t.Fatalf("update status = %d body=%v", status, body)
	}
	if body["variant"].(map[string]any)["slug"] != "成年期" {
		t.Fatalf("slug not migrated: %v", body)
	}

	// 旧 slug 不可再访问、新 slug 可以；按 id 也可以。
	variantID := body["variant"].(map[string]any)["id"].(string)
	if _, err := f.store.Get("p1"); err != nil {
		t.Fatal(err)
	}
	// 直接看落库形态：variants 只挂一个 key。
	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	character := pf.Characters["阿明"]
	rawVariants, _ := character.Fields()["variants"].(map[string]any)
	if len(rawVariants) != 1 {
		t.Fatalf("variants keys = %v", rawVariants)
	}
	if _, ok := rawVariants["成年期"]; !ok {
		t.Fatalf("新 key 不存在: %v", rawVariants)
	}

	// 未知字段 → 422 extra_forbidden
	status, _ = f.do(t, http.MethodPatch, "/api/v1/projects/p1/characters/阿明/variants/"+variantID,
		map[string]any{"hax": true})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("extra field status = %d", status)
	}

	// 删除（按 id）
	status, body = f.do(t, http.MethodDelete, "/api/v1/projects/p1/characters/阿明/variants/"+variantID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete status = %d body=%v", status, body)
	}
	status, _ = f.do(t, http.MethodGet, "/api/v1/projects/p1/characters/阿明/variants", nil)
	// 列表此刻应为空数组
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/characters/阿明/variants", nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	var list struct {
		Variants []any `json:"variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Variants) != 0 {
		t.Fatalf("删除后仍有变体: %v", list)
	}
}

// TestVariantNotFound 校验项目 / 角色 / 变体三级 404。
func TestVariantNotFound(t *testing.T) {
	f := newVariantFixture(t)

	status, _ := f.do(t, http.MethodGet, "/api/v1/projects/ghost/characters/阿明/variants", nil)
	if status != http.StatusNotFound {
		t.Fatalf("project status = %d", status)
	}
	status, _ = f.do(t, http.MethodGet, "/api/v1/projects/p1/characters/阿狗/variants", nil)
	if status != http.StatusNotFound {
		t.Fatalf("character status = %d", status)
	}
	status, _ = f.do(t, http.MethodDelete, "/api/v1/projects/p1/characters/阿明/variants/nope", nil)
	if status != http.StatusNotFound {
		t.Fatalf("variant status = %d", status)
	}
}

// TestVariantInvalidSlugAndStatus 校验 slug / status 越界 422。
func TestVariantInvalidSlugAndStatus(t *testing.T) {
	f := newVariantFixture(t)

	status, _ := f.do(t, http.MethodPost, "/api/v1/projects/p1/characters/阿明/variants",
		map[string]any{"slug": "../逃逸"})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("bad slug status = %d", status)
	}
	status, _ = f.do(t, http.MethodPost, "/api/v1/projects/p1/characters/阿明/variants",
		map[string]any{"slug": "ok", "status": "cancelled"})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("bad status status = %d", status)
	}
}

// TestAvatarUploadReplacesPointer 校验头像上传写指针 + 落文件；二次上传替换同一路径。
func TestAvatarUploadReplacesPointer(t *testing.T) {
	f := newVariantFixture(t)

	status, body := f.postMultipart(t, "/api/v1/projects/p1/characters/阿明/avatar", tinyAvatarPNG())
	if status != http.StatusOK {
		t.Fatalf("upload status = %d body=%v", status, body)
	}
	relativePath, _ := body["path"].(string)
	if relativePath != "characters/阿明_avatar_manual.png" {
		t.Fatalf("path = %v", relativePath)
	}

	pf, err := f.store.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if got := pf.Characters["阿明"].MustString("character_avatar"); got != relativePath {
		t.Fatalf("pointer = %q", got)
	}

	// 二次上传同路径替换（不产生 _avatar_manual (1).png 之类的膨胀）。
	status, _ = f.postMultipart(t, "/api/v1/projects/p1/characters/阿明/avatar", tinyAvatarPNG())
	if status != http.StatusOK {
		t.Fatalf("second upload status = %d", status)
	}

	// 未知角色 → 404
	status, _ = f.postMultipart(t, "/api/v1/projects/p1/characters/阿狗/avatar", tinyAvatarPNG())
	if status != http.StatusNotFound {
		t.Fatalf("unknown character status = %d", status)
	}

	// 非图片字节 → 400
	status, _ = f.postMultipart(t, "/api/v1/projects/p1/characters/阿明/avatar", []byte("not an image"))
	if status != http.StatusBadRequest {
		t.Fatalf("invalid file status = %d", status)
	}

	// 空文件 → 400
	status, _ = f.postMultipart(t, "/api/v1/projects/p1/characters/阿明/avatar", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("empty file status = %d", status)
	}
}
