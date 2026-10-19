// 项目管理扩展路由（对应 server/routers/projects.py 的剧本/分集/概述端点组，10 端点）。
//
// 与既有 projects.go（CRUD 5 + 资产工厂）同挂 /projects 前缀，但服务层分开：
// 这一组全部围绕「剧本内容 + 分集元数据」，与项目 CRUD 的 project.json 字段面不同。
package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
)

// ProjectExtraDeps 是项目扩展路由的依赖。
type ProjectExtraDeps struct {
	Store *service.ProjectStore
	Extra *service.ProjectExtraService
}

// RegisterProjectExtras 注册项目扩展端点。
func RegisterProjectExtras(group *gin.RouterGroup, d ProjectExtraDeps) {
	if d.Extra == nil {
		return
	}
	base := "/projects/:name"
	group.GET(base+"/scripts/:script_file", getProjectScript(d))
	group.GET(base+"/video-capabilities", getVideoCapabilities(d))
	group.PATCH(base+"/overview", patchProjectOverview(d))
	group.PATCH(base+"/episodes/:episode", patchProjectEpisode(d))
	group.PATCH(base+"/episodes/:episode/display-number", patchEpisodeDisplayNumber(d))
	group.DELETE(base+"/episodes/:episode", deleteProjectEpisode(d))
	group.PATCH(base+"/segments/:segment_id", patchProjectSegment(d))
	group.PATCH(base+"/script-scenes/:scene_id", patchScriptScene(d))
	group.PATCH(base+"/script-shots/:shot_id", patchScriptShot(d))
	group.POST(base+"/script-shots/reorder", reorderScriptShots(d))
}

// getProjectScript 读剧本内容。
func getProjectScript(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		script, err := d.Extra.GetScript(c.Param("name"), c.Param("script_file"))
		if err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"script": script})
	}
}

// getVideoCapabilities 解析当前项目视频模型能力 + 用户偏好。
func getVideoCapabilities(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		out, err := d.Extra.VideoCapabilities(c.Request.Context(), c.Param("name"))
		if err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// patchProjectOverview 手动编辑项目概述。
func patchProjectOverview(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			Synopsis     *string `json:"synopsis"`
			Genre        *string `json:"genre"`
			Theme        *string `json:"theme"`
			WorldSetting *string `json:"world_setting"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		input := service.OverviewInput{
			Synopsis: body.Synopsis, Genre: body.Genre, Theme: body.Theme, WorldSetting: body.WorldSetting,
		}
		overview, err := d.Extra.UpdateOverview(c.Param("name"), input)
		if err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "overview": overview})
	}
}

// patchProjectEpisode 更新分集标题。
func patchProjectEpisode(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		var body struct {
			Title *string `json:"title"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		if body.Title == nil || strings_isspace(*body.Title) {
			apperr.Abort(c, apperr.NewText(http.StatusUnprocessableEntity, "episode_title_empty", nil))
			return
		}
		if err := d.Extra.UpdateEpisodeTitle(
			c.Request.Context(), c.Param("name"), episode, *body.Title); err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"episode": gin.H{"episode": episode, "title": *body.Title},
		})
	}
}

// strings_isspace 判定串全空白。
func strings_isspace(value string) bool {
	for _, ch := range value {
		if ch != ' ' && ch != '\t' && ch != '\n' && ch != '\r' {
			return false
		}
	}
	return true
}

// patchEpisodeDisplayNumber 调整分集显示编号。
func patchEpisodeDisplayNumber(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		var body struct {
			DisplayEpisode *int `json:"display_episode"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		if body.DisplayEpisode == nil || *body.DisplayEpisode < 1 {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", "display_episode"},
				Msg: "Input should be greater than 0", Type: "greater_than",
			}}})
			return
		}
		if err := d.Extra.UpdateEpisodeDisplayNumber(c.Param("name"), episode, *body.DisplayEpisode); err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true, "episode": episode, "display_episode": *body.DisplayEpisode,
		})
	}
}

// deleteProjectEpisode 删除分集元数据并取消该集在途任务。
func deleteProjectEpisode(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		episode, ok := episodeParam(c)
		if !ok {
			return
		}
		out, err := d.Extra.DeleteEpisode(c.Request.Context(), c.Param("name"), episode)
		if err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// patchProjectSegment 更新 narration 分镜段。
func patchProjectSegment(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			ScriptFile          *string  `json:"script_file"`
			DurationSeconds     *int     `json:"duration_seconds"`
			SegmentBreak        *bool    `json:"segment_break"`
			ImagePrompt         any      `json:"image_prompt"`
			VideoPrompt         any      `json:"video_prompt"`
			TransitionToNext    *string  `json:"transition_to_next"`
			Note                *string  `json:"note"`
			CharactersInSegment []string `json:"characters_in_segment"`
			Scenes              []string `json:"scenes"`
			Props               []string `json:"props"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		if body.ScriptFile == nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", "script_file"}, Msg: "field required", Type: "missing",
			}}})
			return
		}
		segment, err := d.Extra.UpdateScriptSegment(
			c.Param("name"), *body.ScriptFile, c.Param("segment_id"), service.SegmentPatchInput{
				DurationSeconds:     body.DurationSeconds,
				SegmentBreak:        body.SegmentBreak,
				ImagePrompt:         &body.ImagePrompt,
				VideoPrompt:         &body.VideoPrompt,
				TransitionToNext:    body.TransitionToNext,
				Note:                body.Note,
				NoteSet:             body.Note != nil,
				CharactersInSegment: body.CharactersInSegment,
				Scenes:              body.Scenes,
				Props:               body.Props,
			})
		if err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "segment": segment})
	}
}

// patchScriptScene 更新 drama 剧本的单个场景。
func patchScriptScene(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		scriptFile, updates, ok := decodeScriptUpdates(c)
		if !ok {
			return
		}
		scene, err := d.Extra.UpdateScriptScene(c.Param("name"), scriptFile, c.Param("scene_id"), updates)
		if err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "scene": scene})
	}
}

// patchScriptShot 更新 ad 剧本的单个镜头。
func patchScriptShot(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		scriptFile, updates, ok := decodeScriptUpdates(c)
		if !ok {
			return
		}
		shot, err := d.Extra.UpdateScriptShot(c.Param("name"), scriptFile, c.Param("shot_id"), updates)
		if err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "shot": shot})
	}
}

// reorderScriptShots 重排 ad 剧本的 shots。
func reorderScriptShots(d ProjectExtraDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireExistingProject(c, d.Store) {
			return
		}
		var body struct {
			ScriptFile *string  `json:"script_file"`
			ShotIDs    []string `json:"shot_ids"`
		}
		if !decodeJSONBody(c, &body) {
			return
		}
		if body.ScriptFile == nil {
			apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
				Loc: []string{"body", "script_file"}, Msg: "field required", Type: "missing",
			}}})
			return
		}
		shots, err := d.Extra.ReorderScriptShots(c.Param("name"), *body.ScriptFile, body.ShotIDs)
		if err != nil {
			abortProjectExtraErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "shots": shots})
	}
}

// decodeScriptUpdates 解析 {script_file, updates} 提交体。
func decodeScriptUpdates(c *gin.Context) (string, map[string]any, bool) {
	var body struct {
		ScriptFile *string        `json:"script_file"`
		Updates    map[string]any `json:"updates"`
	}
	if !decodeJSONBody(c, &body) {
		return "", nil, false
	}
	var items []apperr.ValidationItem
	if body.ScriptFile == nil {
		items = append(items, apperr.ValidationItem{
			Loc: []string{"body", "script_file"}, Msg: "field required", Type: "missing"})
	}
	if body.Updates == nil {
		items = append(items, apperr.ValidationItem{
			Loc: []string{"body", "updates"}, Msg: "field required", Type: "missing"})
	}
	if len(items) > 0 {
		apperr.Abort(c, &apperr.Validation{Items: items})
		return "", nil, false
	}
	return *body.ScriptFile, body.Updates, true
}

// abortProjectExtraErr 映射项目扩展域错误。
func abortProjectExtraErr(c *gin.Context, err error) {
	var valueErr *service.ValueError
	switch {
	case errors.Is(err, service.ErrScriptNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "script_not_found",
			map[string]any{"name": c.Param("script_file")}))
	case errors.Is(err, service.ErrSceneNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "scene_not_found",
			map[string]any{"id": c.Param("scene_id")}))
	case errors.Is(err, service.ErrShotNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "shot_not_found",
			map[string]any{"id": c.Param("shot_id")}))
	case errors.Is(err, service.ErrSegmentNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "segment_not_found",
			map[string]any{"id": c.Param("segment_id")}))
	case errors.Is(err, service.ErrEpisodeNotFound):
		apperr.Abort(c, apperr.NewText(http.StatusNotFound, "episode_not_found",
			map[string]any{"episode": c.Param("episode")}))
	case errors.Is(err, service.ErrEpisodeScriptRebound):
		apperr.Abort(c, apperr.NewText(http.StatusConflict, "ref_script_rebound", nil))
	case errors.As(err, &valueErr):
		code := "request_invalid"
		params := map[string]any{"details": valueErr.Msg}
		switch {
		case strings_isspace("") == false && valueErr.Msg == "ad_mode_required":
			code, params = "ad_mode_required", nil
		case valueErr.Msg == "narration_mode_required":
			code, params = "narration_mode_required", nil
		case len(valueErr.Msg) > 10 && valueErr.Msg[:10] == "shot_ids_":
			code = valueErr.Msg
			params = nil
		case len(valueErr.Msg) > 31 && valueErr.Msg[:31] == "episode_display_number_invalid":
			code = "episode_display_number_invalid"
			params = map[string]any{"max": trimIntSuffix(valueErr.Msg)}
		}
		apperr.Abort(c, apperr.NewText(http.StatusBadRequest, code, params))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}

// trimIntSuffix 取「...: max N」里的 N。
func trimIntSuffix(message string) string {
	for i := len(message) - 1; i >= 0; i-- {
		if message[i] == ' ' {
			return message[i+1:]
		}
	}
	return message
}
