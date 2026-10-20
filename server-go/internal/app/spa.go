package app

// SPA 静态服务：frontend/dist 挂载 + history 路由 fallback。
//
// 与 Python（server/app.py 末尾的 frontend/dist 挂载）语义对齐：
//   - 挂载条件是 dist/index.html 是真实文件（检查文件而非目录）：构建产物不完整时
//     跳过挂载仅记 warning，API 不受影响；
//   - fallback 仅对 GET/HEAD 生效，写请求误入页面路径仍返回 JSON 404；
//   - /app/ 深链末段可能带扩展名（如 /app/projects/x/source/chapter1.txt），
//     必须无条件兜底回外壳；dist/app/... 下恰有真实静态文件时优先返回，防被遮蔽；
//   - 其余路径：真实文件存在即原样服务（含 /assets/* 哈希资源，支持条件请求）；
//     带扩展名的缺失文件 404——缺失的哈希资源回 HTML 200 只会把问题拖成白屏；
//     无扩展名回外壳（wouter history 路由：/login、/app/... 等）；
//   - URL 路径是用户可控段：拒绝 NUL，Clean + dist 前缀 + symlink 落点三重校验，
//     不暴露 dist 之外的文件；
//   - 外壳 no-store 由 spaNoCacheMiddleware 按 content-type 统一处理，此处不重复。
//
// 与 Python 的显式差异：/api/ 前缀下的未知路径保持 JSON 404（前端 fetch 错误处理
// 按 JSON 信封解析，回 text/html 200 会静默破坏报错路径），不落入 SPA fallback。

import (
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// spaHandler 是已就绪的 SPA 静态服务。
type spaHandler struct {
	root         string // dist 目录绝对路径
	resolvedRoot string // root 经 symlink 解析后的真实落点（越界校验基准）
	shell        string // index.html 绝对路径
}

// newSPAHandler 检查构建产物并构造 handler；index.html 缺失时返回 nil。
func newSPAHandler(distDir string) *spaHandler {
	shell := filepath.Join(distDir, "index.html")
	info, err := os.Stat(shell)
	if err != nil || info.IsDir() {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(distDir)
	if err != nil {
		resolved = distDir
	}
	return &spaHandler{root: distDir, resolvedRoot: resolved, shell: shell}
}

// registerSPA 把 SPA 服务挂到 NoRoute（全部未匹配路由的兜底）。
// dist 不可用时返回 false 并记 warning，调用方保持原有 JSON 404 行为。
func registerSPA(r *gin.Engine, distDir string, log *slog.Logger) bool {
	spa := newSPAHandler(distDir)
	if spa == nil {
		log.Warn("frontend/dist/index.html 不存在，跳过前端页面挂载（API 不受影响）")
		return false
	}
	r.NoRoute(spa.handle)
	return true
}

func (s *spaHandler) handle(c *gin.Context) {
	urlPath := c.Request.URL.Path
	// API 面永远 JSON 404：未知端点回外壳会让前端 fetch 拿到 text/html 200。
	if strings.HasPrefix(urlPath, "/api/") {
		notFound(c)
		return
	}
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		notFound(c)
		return
	}
	if file, ok := s.resolve(urlPath); ok {
		http.ServeFile(c.Writer, c.Request, file)
		return
	}
	if !strings.HasPrefix(urlPath, "/app/") && path.Ext(urlPath) != "" {
		notFound(c)
		return
	}
	http.ServeFile(c.Writer, c.Request, s.shell)
}

// resolve 把 URL 路径映射到 dist 下的真实文件；目录、缺失或越界返回 ok=false。
func (s *spaHandler) resolve(urlPath string) (string, bool) {
	if strings.ContainsRune(urlPath, '\x00') {
		return "", false
	}
	// 前导 "/" 强制 Clean 从根开始：任何 "../" 逃逸在 Clean 后被钳到根，再经前缀
	// 与 symlink 落点校验双保险。文件名合法含 ".." 字符（如 a..b.js）不受影响。
	rel := path.Clean("/" + urlPath)
	if rel == "/" {
		return "", false
	}
	full := filepath.Join(s.root, filepath.FromSlash(rel))
	if !strings.HasPrefix(full, s.root+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return "", false
	}
	// dist 内的 symlink 落点也必须仍在 dist 内：构建产物里的链接不能借道读外部文件。
	if resolved, err := filepath.EvalSymlinks(full); err == nil {
		if resolved != s.resolvedRoot && !strings.HasPrefix(resolved, s.resolvedRoot+string(filepath.Separator)) {
			return "", false
		}
	}
	return full, true
}
