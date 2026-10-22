// 分集目标时长规划路由（对应 server/routers/episode_duration.py）。
//
// Mount prefix: /api/v1/projects/:name/episodes/:episode/duration-plan
//
// 语义要点：
//   - 修订号过期是 409 的对象形 detail（带 actual_revision），前端据它重取基线；
//   - 剧本绑定被并发改绑是 409 的字符串 detail（让用户刷新重试）；
//   - 条目不存在的 KeyError 与「集不存在」都归 404 episode_duration_resource_not_found，
//     但项目本身不存在是 404 project_not_found，两者不要混。
package http

import (
	servicescript "github.com/shotwise/shotwise/internal/service/script"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/i18n"
	"github.com/shotwise/shotwise/internal/service"
)

// EpisodeDurationDeps 是时长规划路由的依赖。
type EpisodeDurationDeps struct {
	Store    *service.ProjectStore
	Duration *servicescript.EpisodeDurationService
}

// RegisterEpisodeDuration 注册分集时长规划路由。
func RegisterEpisodeDuration(group *gin.RouterGroup, d EpisodeDurationDeps) {
	if d.Duration == nil {
		return
	}
	base := "/projects/:name/episodes/:episode/duration-plan"
	group.GET(base, getDurationPlan(d))
	group.PUT(base, saveDurationPlan(d))
	group.POST(base+"/preview", previewDurationPlan(d))
	group.POST(base+"/apply", applyDurationPlan(d))
	group.PATCH(base+"/items/:resource_id/lock", setDurationLock(d))
}

// durationPlanBody 是 PUT / POST 的提交体。
type durationPlanBody struct {
	TargetSeconds     *int           `json:"target_seconds"`
	Strategy          *string        `json:"strategy"`
	ManualAllocations map[string]int `json:"manual_allocations"`
	ExpectedRevision  *string        `json:"expected_revision"`
}

// durationPlanStrategies 对应 pydantic 的 Literal["equal", "proportional", "manual"]。
var durationPlanStrategies = map[string]bool{"equal": true, "proportional": true, "manual": true}

// validateDurationPlanBody 复刻 pydantic 的字段校验，返回 FastAPI 风格的 422 项。
//
// requireRevision 区分「保存/应用」需要 expected_revision 与「预览」不需要。
func validateDurationPlanBody(body durationPlanBody, requireRevision bool) []apperr.ValidationItem {
	var items []apperr.ValidationItem
	add := func(loc, msg, typ string) {
		items = append(items, apperr.ValidationItem{Loc: []string{"body", loc}, Msg: msg, Type: typ})
	}
	if body.TargetSeconds == nil {
		add("target_seconds", "field required", "missing")
	} else if *body.TargetSeconds < 1 {
		add("target_seconds", "Input should be greater than or equal to 1", "greater_than_equal")
	}
	if body.Strategy != nil && !durationPlanStrategies[*body.Strategy] {
		add("strategy", "Input should be 'equal', 'proportional' or 'manual'", "literal_error")
	}
	if requireRevision {
		if body.ExpectedRevision == nil {
			add("expected_revision", "field required", "missing")
		} else if *body.ExpectedRevision == "" {
			add("expected_revision", "String should have at least 1 character", "string_too_short")
		}
	}
	return items
}

// toInput 归一为服务层入参（strategy 缺省 equal）。
func (b durationPlanBody) toInput() servicescript.EpisodeDurationPlanInput {
	strategy := "equal"
	if b.Strategy != nil {
		strategy = *b.Strategy
	}
	allocations := b.ManualAllocations
	if allocations == nil {
		allocations = map[string]int{}
	}
	target := 0
	if b.TargetSeconds != nil {
		target = *b.TargetSeconds
	}
	return servicescript.EpisodeDurationPlanInput{
		TargetSeconds:     target,
		Strategy:          strategy,
		ManualAllocations: allocations,
	}
}

// getDurationPlan 返回当前规划设置与逐项状态。
func getDurationPlan(d EpisodeDurationDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		out, err := d.Duration.State(c.Request.Context(), c.Param("name"), episode)
		writeDurationResult(c, out, err)
	}
}

// saveDurationPlan 只保存规划设置（不改分镜时长）。
func saveDurationPlan(d EpisodeDurationDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		var body durationPlanBody
		if !decodeJSONBody(c, &body) {
			return
		}
		if items := validateDurationPlanBody(body, true); len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		out, err := d.Duration.Save(
			c.Request.Context(), c.Param("name"), episode, *body.ExpectedRevision, body.toInput())
		writeDurationResult(c, out, err)
	}
}

// previewDurationPlan 返回落盘前的时长 diff。
func previewDurationPlan(d EpisodeDurationDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		var body durationPlanBody
		if !decodeJSONBody(c, &body) {
			return
		}
		if items := validateDurationPlanBody(body, false); len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		out, err := d.Duration.Preview(c.Request.Context(), c.Param("name"), episode, body.toInput())
		writeDurationResult(c, out, err)
	}
}

// applyDurationPlan 保存并写回分镜时长。
func applyDurationPlan(d EpisodeDurationDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		var body durationPlanBody
		if !decodeJSONBody(c, &body) {
			return
		}
		if items := validateDurationPlanBody(body, true); len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		out, err := d.Duration.Apply(
			c.Request.Context(), c.Param("name"), episode, *body.ExpectedRevision, body.toInput())
		writeDurationResult(c, out, err)
	}
}

// setDurationLock 切换单个条目的时长锁。
func setDurationLock(d EpisodeDurationDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		var body struct {
			Locked           *bool   `json:"locked"`
			ExpectedRevision *string `json:"expected_revision"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		var items []apperr.ValidationItem
		if body.Locked == nil {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "locked"}, Msg: "field required", Type: "missing"})
		}
		if body.ExpectedRevision == nil {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "expected_revision"}, Msg: "field required", Type: "missing"})
		} else if *body.ExpectedRevision == "" {
			items = append(items, apperr.ValidationItem{
				Loc: []string{"body", "expected_revision"}, Msg: "String should have at least 1 character",
				Type: "string_too_short"})
		}
		if len(items) > 0 {
			apperr.Abort(c, &apperr.Validation{Items: items})
			return
		}
		out, err := d.Duration.SetLock(
			c.Request.Context(), c.Param("name"), episode, c.Param("resource_id"), *body.Locked, *body.ExpectedRevision)
		writeDurationResult(c, out, err)
	}
}

// writeDurationResult 统一写响应或错误信封。
func writeDurationResult(c *gin.Context, out map[string]any, err error) {
	if err != nil {
		abortDurationErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// abortDurationErr 把时长规划错误映射成统一 {"detail": ...} 信封。
func abortDurationErr(c *gin.Context, err error) {
	var (
		conflict *servicescript.EpisodeDurationRevisionConflictError
		notFound *servicescript.EpisodeDurationNotFoundError
		invalid  *servicescript.EpisodeDurationInvalidError
	)
	loc := i18n.FromRequest(c.Request)
	switch {
	case errors.As(err, &conflict):
		// 对象形 detail：前端要读 actual_revision 才能重算。
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"detail": map[string]any{
			"code":            "episode_duration_revision_conflict",
			"message":         i18n.T(loc, "episode_duration_revision_conflict", nil),
			"actual_revision": conflict.Actual,
		}})
	case errors.Is(err, service.ErrEpisodeScriptRebound):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, "episode_duration_script_rebound", nil))
	case errors.As(err, &notFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "episode_duration_resource_not_found", nil))
	case errors.As(err, &invalid):
		apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "episode_duration_invalid",
			map[string]any{"details": invalid.Detail}))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
