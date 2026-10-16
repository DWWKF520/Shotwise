package http

// 渲染任务路由（对应 server/routers/media_rendering.py）：
// 预览/成片任务入队、生命周期操作、产物与审查帧下载、成片审查与字幕导出。

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
)

// MediaRenderingDeps 是渲染任务 handler 的依赖。
type MediaRenderingDeps struct {
	Renders *service.MediaRenderingService
}

// RegisterMediaRendering 注册渲染任务路由；调用方负责鉴权。
func RegisterMediaRendering(group *gin.RouterGroup, d MediaRenderingDeps) {
	group.POST("/assembly-plans/:plan_id/preview-renders", createPreviewRender(d))
	group.POST("/assembly-plans/:plan_id/final-renders", createFinalRender(d))
	group.GET("/assembly-plans/:plan_id/preview-renders", listRenderJobsHandler(d, "preview"))
	group.GET("/assembly-plans/:plan_id/final-renders", listRenderJobsHandler(d, "final"))
	group.GET("/assembly-plans/:plan_id/final-review", reviewFinalRender(d))
	group.GET("/assembly-plans/:plan_id/subtitles", exportAssemblySubtitles(d))
	group.GET("/render-jobs/:job_id", getRenderJob(d))
	group.POST("/render-jobs/:job_id/retry", retryPreviewRender(d))
	group.POST("/render-jobs/:job_id/final-retry", retryFinalRender(d))
	group.POST("/render-jobs/:job_id/cancel", cancelRenderJob(d))
	group.GET("/render-jobs/:job_id/artifact", getRenderArtifactForJob(d))
	group.GET("/render-artifacts/:artifact_id", getRenderArtifact(d))
	group.POST("/render-artifacts/:artifact_id/review-confirm", confirmFinalReview(d))
	group.GET("/render-artifacts/:artifact_id/review-frames/:position", getFinalReviewFrame(d))
}

// previewRenderRequest 对应 PreviewRenderRequest：revision_number 可空（≥1），
// max_attempts 默认 3、范围 [1,10]。
type previewRenderRequest struct {
	RevisionNumber *int64 `json:"revision_number"`
	MaxAttempts    *int64 `json:"max_attempts"`
}

// bindPreviewRenderRequest 解析任务入队请求体，字段越界写出 422。
func bindPreviewRenderRequest(c *gin.Context) (*int64, int64, bool) {
	var req previewRenderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortBodyBind(c, err)
		return nil, 0, false
	}
	if req.RevisionNumber != nil && *req.RevisionNumber < 1 {
		abortFieldConstraint(c, "revision_number", "greater_than_equal")
		return nil, 0, false
	}
	maxAttempts := int64(3)
	if req.MaxAttempts != nil {
		if *req.MaxAttempts < 1 {
			abortFieldConstraint(c, "max_attempts", "greater_than_equal")
			return nil, 0, false
		}
		if *req.MaxAttempts > 10 {
			abortFieldConstraint(c, "max_attempts", "less_than_equal")
			return nil, 0, false
		}
		maxAttempts = *req.MaxAttempts
	}
	return req.RevisionNumber, maxAttempts, true
}

func createPreviewRender(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		revisionNumber, maxAttempts, ok := bindPreviewRenderRequest(c)
		if !ok {
			return
		}
		result, err := d.Renders.CreatePreviewJob(
			c.Request.Context(), c.Param("plan_id"), currentUserID(c), revisionNumber, maxAttempts,
		)
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	}
}

func createFinalRender(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		revisionNumber, maxAttempts, ok := bindPreviewRenderRequest(c)
		if !ok {
			return
		}
		result, err := d.Renders.CreateFinalJob(
			c.Request.Context(), c.Param("plan_id"), currentUserID(c), revisionNumber, maxAttempts,
		)
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	}
}

func listRenderJobsHandler(d MediaRenderingDeps, kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := d.Renders.ListRenderJobs(c.Request.Context(), c.Param("plan_id"), currentUserID(c), &kind)
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func getRenderJob(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Renders.GetRenderJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func retryPreviewRender(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Renders.RetryPreviewJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	}
}

func retryFinalRender(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Renders.RetryFinalJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	}
}

func cancelRenderJob(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Renders.CancelRenderJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	}
}

func reviewFinalRender(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Renders.ReviewFinalArtifact(c.Request.Context(), c.Param("plan_id"), currentUserID(c))
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func confirmFinalReview(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Renders.ConfirmFinalReview(c.Request.Context(), c.Param("artifact_id"), currentUserID(c))
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// exportAssemblySubtitles 导出 srt/vtt 字幕（format 必填且限定两值）。
func exportAssemblySubtitles(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, present := c.GetQuery("format")
		if !present || raw == "" {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"query", "format"}, Msg: "field required", Type: "value_error.missing"},
			}})
			return
		}
		if raw != "srt" && raw != "vtt" {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
				{Loc: []string{"query", "format"}, Msg: "string does not match pattern ^(srt|vtt)$", Type: "string_pattern_mismatch"},
			}})
			return
		}
		result, err := d.Renders.ExportSubtitles(c.Request.Context(), c.Param("plan_id"), raw, currentUserID(c))
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		content, _ := result["content"].(string)
		mediaType, _ := result["media_type"].(string)
		filename, _ := result["filename"].(string)
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		c.Data(http.StatusOK, mediaType, []byte(content))
	}
}

func getRenderArtifact(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		metadata, path, err := d.Renders.GetRenderArtifactFile(
			c.Request.Context(), c.Param("artifact_id"), currentUserID(c),
		)
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		serveRenderArtifactFile(c, metadata, path)
	}
}

// getRenderArtifactForJob 返回任务最新产物；任务存在但尚无产物时按 404 口径拒绝。
func getRenderArtifactForJob(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := d.Renders.GetRenderJob(c.Request.Context(), c.Param("job_id"), currentUserID(c))
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		artifact, _ := job["artifact"].(map[string]any)
		if artifact == nil {
			apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "render_resource_not_found"))
			return
		}
		artifactID, _ := artifact["id"].(string)
		metadata, path, err := d.Renders.GetRenderArtifactFile(c.Request.Context(), artifactID, currentUserID(c))
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		serveRenderArtifactFile(c, metadata, path)
	}
}

func getFinalReviewFrame(d MediaRenderingDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		metadata, path, err := d.Renders.GetFinalReviewFrameFile(
			c.Request.Context(), c.Param("artifact_id"), c.Param("position"), currentUserID(c),
		)
		if err != nil {
			abortRenderErr(c, err)
			return
		}
		serveRenderArtifactFile(c, metadata, path)
	}
}

// serveRenderArtifactFile 输出产物字节：mime_type 由元数据驱动，加 nosniff 防内容嗅探型 XSS
// （对应 _artifact_file_response）。
func serveRenderArtifactFile(c *gin.Context, metadata map[string]any, path string) {
	file, err := os.Open(path)
	if err != nil {
		apperr.Abort(c, apperr.Internal(err))
		return
	}
	defer file.Close()
	info, statErr := file.Stat()
	if statErr != nil {
		apperr.Abort(c, apperr.Internal(statErr))
		return
	}
	name := filepath.Base(path)
	c.Header("X-Content-Type-Options", "nosniff")
	if mimeType, isString := metadata["mime_type"].(string); isString && mimeType != "" {
		c.Header("Content-Type", mimeType)
	}
	c.Header("Content-Disposition", contentDisposition(name))
	http.ServeContent(c.Writer, c.Request, name, info.ModTime(), file)
}

// abortRenderErr 把渲染领域错误映射成统一 {"detail": ...} 信封
// （对应 _not_found / _conflict / _not_ready / _review_failure / _review_conflict / _subtitle_error）。
func abortRenderErr(c *gin.Context, err error) {
	var (
		jobNotFound      *service.RenderJobNotFoundError
		artifactNotFound *service.RenderArtifactNotFoundError
		planNotFound     *service.AssemblyPlanNotFoundError
		jobConflict      *service.RenderJobConflictError
		revisionConflict *service.RenderRevisionConflictError
		notReady         *service.FinalRenderNotReadyError
		reviewConflict   *service.RenderReviewConflictError
		reviewFailure    *service.FinalRenderReviewError
		subtitleErr      *service.SubtitleExportError
	)
	switch {
	case errors.As(err, &jobNotFound), errors.As(err, &artifactNotFound), errors.As(err, &planNotFound):
		// Python 侧这里是字面量字符串 detail，不走 i18n；保持原样。
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "render_resource_not_found"))
	case errors.As(err, &jobConflict):
		apperr.Abort(c, apperr.New(http.StatusConflict, jobConflict.Code, renderConflictParams(jobConflict.Status)))
	case errors.As(err, &revisionConflict):
		apperr.Abort(c, apperr.New(http.StatusConflict, revisionConflict.Code, renderConflictParams(revisionConflict.Status)))
	case errors.As(err, &notReady):
		apperr.Abort(c, apperr.New(http.StatusConflict, "not_ready", map[string]any{"resource": notReady.PlanID}))
	case errors.As(err, &reviewConflict):
		apperr.Abort(c, apperr.New(http.StatusConflict, reviewConflict.Code, renderConflictParams(reviewConflict.Status)))
	case errors.As(err, &reviewFailure):
		apperr.Abort(c, apperr.New(
			http.StatusUnprocessableEntity, reviewFailure.Code, map[string]any{"message": reviewFailure.Message},
		))
	case errors.As(err, &subtitleErr):
		apperr.Abort(c, apperr.New(
			http.StatusUnprocessableEntity, "invalid_subtitles", map[string]any{"message": subtitleErr.Message},
		))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

// renderConflictParams 组装 409 detail 的可选 status 字段。
func renderConflictParams(status string) map[string]any {
	params := map[string]any{}
	if status != "" {
		params["status"] = status
	}
	return params
}
