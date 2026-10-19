// 系统级端点：诊断日志打包下载（对应 server/routers/system.py）。
//
// 安全要点（照抄 Python 的三条）：
//   - **跳过 symlink**：有人往 logs/ 里放一个指向 /etc/shadow 的软链，就能通过诊断包外泄；
//   - 单文件超限只记一行说明，不把整个包撑爆；
//   - 诊断文本只放环境事实（版本、平台、配置摘要），不放任何密钥。
package http

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/config"
)

// maxLogFileBytes 是单个日志文件进入诊断包的上限（超过只记一行说明）。
const maxLogFileBytes = 100 * 1024 * 1024

// SystemDeps 是系统级路由的依赖。
type SystemDeps struct {
	Config *config.Config
}

// RegisterSystem 注册系统级路由（整组管理员限定）。
func RegisterSystem(group *gin.RouterGroup, d SystemDeps) {
	group.GET("/system/logs/download", downloadLogs(d))
}

// downloadLogs 打包 logs/ 下的文件 + diagnostics.txt 返回。
func downloadLogs(d SystemDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		logDir := ""
		if d.Config != nil {
			logDir = d.Config.LogDir
		}
		var (
			buffer     bytes.Buffer
			skipped    []string
			fileCount  int
			totalBytes int64
		)
		archive := zip.NewWriter(&buffer)
		if logDir != "" {
			if entries, err := os.ReadDir(logDir); err == nil {
				names := make([]string, 0, len(entries))
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				sort.Strings(names)
				for _, name := range names {
					path := filepath.Join(logDir, name)
					info, statErr := os.Lstat(path)
					if statErr != nil {
						continue
					}
					// 软链一律跳过：诊断包不能成为越权读取的通道。
					if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
						continue
					}
					// 只收 shotwise.log* 轮转家族，避免把无关文件打进包。
					if !logFileMatches(name) {
						continue
					}
					if info.Size() > maxLogFileBytes {
						skipped = append(skipped,
							fmt.Sprintf("[skipped: too large: %s (%d bytes)]", name, info.Size()))
						continue
					}
					content, readErr := os.ReadFile(path)
					if readErr != nil {
						skipped = append(skipped, fmt.Sprintf("[skipped: unreadable: %s]", name))
						continue
					}
					writer, createErr := archive.Create("logs/" + name)
					if createErr != nil {
						apperr.Abort(c, apperr.Internal(createErr))
						return
					}
					if _, writeErr := writer.Write(content); writeErr != nil {
						apperr.Abort(c, apperr.Internal(writeErr))
						return
					}
					fileCount++
					totalBytes += info.Size()
				}
			}
		}

		diagnostics := buildDiagnostics(d.Config, fileCount, totalBytes, skipped)
		writer, err := archive.Create("diagnostics.txt")
		if err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if _, err := writer.Write([]byte(diagnostics)); err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}
		if err := archive.Close(); err != nil {
			apperr.Abort(c, apperr.Internal(err))
			return
		}

		filename := "shotwise-diagnostics-" + time.Now().UTC().Format("2006-01-02-1504") + "Z.zip"
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.Data(http.StatusOK, "application/zip", buffer.Bytes())
	}
}

// logFileMatches 判定文件名是否属于日志轮转家族（shotwise.log*）。
func logFileMatches(name string) bool {
	const prefix = "shotwise.log"
	return len(name) >= len(prefix) && name[:len(prefix)] == prefix
}

// buildDiagnostics 组装诊断文本。
//
// 只写环境事实：版本号、运行时、平台、日志目录与收集统计。**密钥、连接串、项目内容一概不写**——
// 这个文件会随诊断包外发。
func buildDiagnostics(cfg *config.Config, fileCount int, totalBytes int64, skipped []string) string {
	var builder bytes.Buffer
	builder.WriteString("SHOTWISE diagnostics\n")
	builder.WriteString("generated_at: " + time.Now().UTC().Format(time.RFC3339) + "\n")
	builder.WriteString("go_version: " + runtime.Version() + "\n")
	builder.WriteString("platform: " + runtime.GOOS + "/" + runtime.GOARCH + "\n")
	builder.WriteString(fmt.Sprintf("cpu: %d\n", runtime.NumCPU()))
	if cfg != nil {
		builder.WriteString("log_level: " + cfg.LogLevel + "\n")
		builder.WriteString("log_dir: " + cfg.LogDir + "\n")
		builder.WriteString("in_docker: " + boolText(cfg.InDocker) + "\n")
		builder.WriteString("sandbox_enabled: " + boolText(cfg.SandboxEnabled) + "\n")
		builder.WriteString("cors_origins: " + fmt.Sprint(len(cfg.CORSOrigins)) + "\n")
	}
	builder.WriteString(fmt.Sprintf("log_files_included: %d\n", fileCount))
	builder.WriteString(fmt.Sprintf("log_bytes_included: %d\n", totalBytes))
	if len(skipped) > 0 {
		builder.WriteString("\n" + joinLines(skipped) + "\n")
	}
	return builder.String()
}

// boolText 渲染布尔值。
func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// joinLines 用换行拼接。
func joinLines(lines []string) string {
	var builder bytes.Buffer
	for index, line := range lines {
		if index > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(line)
	}
	return builder.String()
}
