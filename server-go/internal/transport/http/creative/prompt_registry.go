package creative

// 提示词模板注册表路由（对应 server/routers/prompt_registry.py，2 个端点）。
//
// agent_runtime_profile 的只读元数据投影：按流程环节分组列出模板，或读单条模板正文。
// 两个端点只读，不触发任何运行时 prompt 组装；只做常规认证，不做管理员限制。
// 模板是随发行版发布的静态文件，既不含密钥也不含用户数据。

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/domain"
)

// PromptRegistryDeps 是注册表 handler 的依赖。
type PromptRegistryDeps struct {
	// ProfileRoot 返回 profile 根目录；nil 时用 domain.AgentProfileRoot
	// （SHOTWISE_PROFILE_DIR > 从 cwd 向上回溯 agent_runtime_profile）。
	ProfileRoot func() string
}

// RegisterPromptRegistry 注册提示词模板路由。
func RegisterPromptRegistry(group *gin.RouterGroup, d PromptRegistryDeps) {
	root := d.ProfileRoot
	if root == nil {
		root = domain.AgentProfileRoot
	}
	group.GET("/prompts/registry", listPromptTemplates(root))
	// Python 侧是 {template_path:path}，可含多段；gin 的 *path 通配符同义。
	group.GET("/prompts/registry/*template_path", getPromptTemplate(root))
}

// listPromptTemplates 列出注册表全部条目的元数据（不含正文），顺序即设置页展示顺序。
func listPromptTemplates(profileRoot func() string) gin.HandlerFunc {
	return func(c *gin.Context) {
		entries := domain.BuildPromptRegistry(profileRoot())
		if entries == nil {
			entries = []domain.PromptTemplate{}
		}
		c.JSON(http.StatusOK, gin.H{"templates": entries})
	}
}

// getPromptTemplate 返回单条模板的元数据与正文；未知路径返回 404。
func getPromptTemplate(profileRoot func() string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// gin 的 *path 带前导 "/"，剥掉后与 Python 的 POSIX 相对路径对齐。
		templatePath := c.Param("template_path")
		if len(templatePath) > 0 && templatePath[0] == '/' {
			templatePath = templatePath[1:]
		}
		detail := domain.FindPromptTemplate(profileRoot(), templatePath)
		if detail == nil {
			apperr.Abort(c, apperr.NewText(http.StatusNotFound, "prompt_template_not_found", map[string]any{"path": templatePath}))
			return
		}
		c.JSON(http.StatusOK, gin.H{"template": detail.Template, "content": detail.Content})
	}
}
