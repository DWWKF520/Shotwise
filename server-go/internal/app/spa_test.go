package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/config"
)

// spaTestEngine 构造带完整 dist 产物的测试引擎，返回引擎与 dist 目录。
func spaTestEngine(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	dist := t.TempDir()
	shell := "<html><body>SPA SHELL</body></html>"
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte(shell), 0o644); err != nil {
		t.Fatal(err)
	}
	assets := filepath.Join(dist, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "app.js"), []byte("console.log('ok')"), 0o644); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	if !registerSPA(r, dist, slog.New(slog.NewTextHandler(io.Discard, nil))) {
		t.Fatal("registerSPA returned false for a complete dist")
	}
	return r, dist
}

func spaRequest(t *testing.T, r *gin.Engine, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func TestSPAServing(t *testing.T) {
	r, _ := spaTestEngine(t)

	t.Run("根路径与无扩展名路由回外壳", func(t *testing.T) {
		for _, target := range []string{"/", "/login", "/app/projects/x", "/missing-route"} {
			w := spaRequest(t, r, http.MethodGet, target)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "SPA SHELL") {
				t.Fatalf("GET %s = %d %q，期望外壳 200", target, w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
				t.Fatalf("GET %s content-type = %q，期望 text/html", target, ct)
			}
		}
	})

	t.Run("app深链带扩展名仍回外壳", func(t *testing.T) {
		w := spaRequest(t, r, http.MethodGet, "/app/projects/x/source/chapter1.txt")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "SPA SHELL") {
			t.Fatalf("/app 深链 = %d %q，期望外壳", w.Code, w.Body.String())
		}
	})

	t.Run("真实静态文件原样服务", func(t *testing.T) {
		w := spaRequest(t, r, http.MethodGet, "/assets/app.js")
		if w.Code != http.StatusOK || w.Body.String() != "console.log('ok')" {
			t.Fatalf("静态文件 = %d %q", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Fatalf("content-type = %q，期望 javascript", ct)
		}
	})

	t.Run("带扩展名的缺失资源404", func(t *testing.T) {
		w := spaRequest(t, r, http.MethodGet, "/assets/missing.js")
		if w.Code != http.StatusNotFound {
			t.Fatalf("缺失哈希资源 = %d，期望 404", w.Code)
		}
	})

	t.Run("写请求不回外壳", func(t *testing.T) {
		w := spaRequest(t, r, http.MethodPost, "/login")
		if w.Code != http.StatusNotFound {
			t.Fatalf("POST /login = %d，期望 404", w.Code)
		}
	})

	t.Run("未知API路径保持JSON404", func(t *testing.T) {
		w := spaRequest(t, r, http.MethodGet, "/api/v1/unknown")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"detail"`) {
			t.Fatalf("未知 API = %d %q，期望 JSON 404", w.Code, w.Body.String())
		}
	})

	t.Run("路径穿越被钳回dist内", func(t *testing.T) {
		w := spaRequest(t, r, http.MethodGet, "/assets/../../secret.txt")
		if w.Code == http.StatusOK {
			t.Fatalf("穿越请求意外返回 200 %q", w.Body.String())
		}
	})

	t.Run("HEAD请求返回外壳", func(t *testing.T) {
		w := spaRequest(t, r, http.MethodHead, "/")
		if w.Code != http.StatusOK {
			t.Fatalf("HEAD / = %d，期望 200", w.Code)
		}
	})
}

func TestRegisterSPASkipsIncompleteDist(t *testing.T) {
	dist := t.TempDir() // 无 index.html
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if registerSPA(r, dist, slog.New(slog.NewTextHandler(io.Discard, nil))) {
		t.Fatal("不完整 dist 不应挂载")
	}
}

func TestResolveFrontendDistEnvOverride(t *testing.T) {
	dist := t.TempDir()
	t.Setenv("SHOTWISE_FRONTEND_DIST", dist)
	if got := config.ResolveFrontendDist(); got != dist {
		t.Fatalf("ResolveFrontendDist() = %q，期望 %q", got, dist)
	}
}
