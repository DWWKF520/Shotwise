package creative

// 语义创意板路由（对应 server/routers/creative_boards.py）。
// 执行仍走 workflow 端点；本组只承载语义画布的读写与版本管理。

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service/creative"
	"github.com/shotwise/shotwise/internal/transport/http/httpkit"
)

// CreativeBoardDeps 是创意板 handler 的依赖。
type CreativeBoardDeps struct {
	Boards *creative.CreativeBoardService
}

// RegisterCreativeBoards 注册创意板路由；调用方负责挂 feature gate。
func RegisterCreativeBoards(group *gin.RouterGroup, d CreativeBoardDeps) {
	group.POST("/projects/:name/creative-boards", createCreativeBoard(d))
	group.GET("/projects/:name/creative-boards", listCreativeBoards(d))

	group.GET("/creative-boards/:board_id", getCreativeBoard(d))
	group.PUT("/creative-boards/:board_id/snapshot", replaceCreativeBoardSnapshot(d))
	group.PATCH("/creative-boards/:board_id", updateCreativeBoard(d))
	group.DELETE("/creative-boards/:board_id", deleteCreativeBoard(d))
	group.POST("/creative-boards/:board_id/copy", copyCreativeBoard(d))

	group.POST("/creative-boards/:board_id/versions", createCreativeBoardVersion(d))
	group.GET("/creative-boards/:board_id/versions", listCreativeBoardVersions(d))
	group.GET("/creative-boards/:board_id/versions/:version_id", getCreativeBoardVersion(d))
	group.POST("/creative-boards/:board_id/versions/:version_id/restore", restoreCreativeBoardVersion(d))

	group.POST("/creative-boards/:board_id/items", addCreativeBoardItem(d))
	group.PATCH("/creative-boards/:board_id/items/:item_id", updateCreativeBoardItem(d))
	group.DELETE("/creative-boards/:board_id/items/:item_id", deleteCreativeBoardItem(d))

	group.POST("/creative-boards/:board_id/edges", addCreativeBoardEdge(d))
	group.DELETE("/creative-boards/:board_id/edges/:edge_id", deleteCreativeBoardEdge(d))
}

func createCreativeBoard(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		name, ok := httpkit.RequiredString(c, body, "name")
		if !ok {
			return
		}
		board, err := d.Boards.CreateBoard(
			c.Request.Context(), httpkit.CurrentUserID(c), c.Param("name"), name,
			httpkit.AnyMap(body["viewport"]), httpkit.AnyMap(body["display_settings"]),
		)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, board)
	}
}

func listCreativeBoards(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Boards.ListBoards(c.Request.Context(), httpkit.CurrentUserID(c), c.Param("name"))
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getCreativeBoard(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		board, err := d.Boards.GetBoard(c.Request.Context(), c.Param("board_id"), httpkit.CurrentUserID(c))
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, board)
	}
}

func updateCreativeBoard(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		expected, ok := bodyRevision(c, body)
		if !ok {
			return
		}
		board, err := d.Boards.UpdateBoard(
			c.Request.Context(), c.Param("board_id"), httpkit.CurrentUserID(c),
			httpkit.OptionalBodyString(body, "name"), httpkit.AnyMap(body["viewport"]), httpkit.AnyMap(body["display_settings"]), expected,
		)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, board)
	}
}

func replaceCreativeBoardSnapshot(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		expected, ok := requiredRevision(c, body)
		if !ok {
			return
		}
		board, err := d.Boards.ReplaceSnapshot(c.Request.Context(), c.Param("board_id"), httpkit.CurrentUserID(c), body, &expected)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, board)
	}
}

func createCreativeBoardVersion(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		versionName, ok := httpkit.RequiredString(c, body, "version_name")
		if !ok {
			return
		}
		expected, ok := bodyRevision(c, body)
		if !ok {
			return
		}
		version, err := d.Boards.CreateVersion(
			c.Request.Context(), c.Param("board_id"), httpkit.CurrentUserID(c), versionName, expected,
		)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, version)
	}
}

func listCreativeBoardVersions(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := d.Boards.ListVersions(c.Request.Context(), c.Param("board_id"), httpkit.CurrentUserID(c))
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func getCreativeBoardVersion(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		version, err := d.Boards.GetVersion(
			c.Request.Context(), c.Param("board_id"), c.Param("version_id"), httpkit.CurrentUserID(c),
		)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, version)
	}
}

func restoreCreativeBoardVersion(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		expected, ok := requiredRevision(c, body)
		if !ok {
			return
		}
		board, err := d.Boards.RestoreVersion(
			c.Request.Context(), c.Param("board_id"), c.Param("version_id"), httpkit.CurrentUserID(c), expected,
		)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, board)
	}
}

func copyCreativeBoard(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		board, err := d.Boards.CopyBoard(
			c.Request.Context(), c.Param("board_id"), httpkit.CurrentUserID(c),
			httpkit.OptionalBodyString(body, "name"), httpkit.OptionalBodyString(body, "version_id"),
		)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, board)
	}
}

func addCreativeBoardItem(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		itemType, ok := httpkit.RequiredString(c, body, "item_type")
		if !ok {
			return
		}
		resourceType, ok := httpkit.RequiredString(c, body, "resource_type")
		if !ok {
			return
		}
		resourceID, ok := httpkit.RequiredString(c, body, "resource_id")
		if !ok {
			return
		}
		expected, ok := bodyRevision(c, body)
		if !ok {
			return
		}
		result, err := d.Boards.AddItem(
			c.Request.Context(), c.Param("board_id"), httpkit.CurrentUserID(c), itemType, resourceType, resourceID,
			httpkit.AnyMap(body["position"]), httpkit.AnyMap(body["size"]), httpkit.OptionalBodyString(body, "group_id"),
			httpkit.AnyMap(body["display_settings"]), expected,
		)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func updateCreativeBoardItem(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		expected, ok := bodyRevision(c, body)
		if !ok {
			return
		}
		// group_id 的“显式清空”与“未传”语义不同，须按字段是否存在判定。
		groupID, groupIDPresent := optionalMaybeString(body, "group_id")
		board, err := d.Boards.UpdateItem(
			c.Request.Context(), c.Param("board_id"), c.Param("item_id"), httpkit.CurrentUserID(c),
			httpkit.AnyMap(body["position"]), httpkit.AnyMap(body["size"]), groupID, groupIDPresent,
			httpkit.AnyMap(body["display_settings"]), expected,
		)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, board)
	}
}

func deleteCreativeBoardItem(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		expected, ok := queryRevision(c, "expected_revision")
		if !ok {
			return
		}
		result, err := d.Boards.DeleteItem(c.Request.Context(), c.Param("board_id"), c.Param("item_id"), httpkit.CurrentUserID(c), expected)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func addCreativeBoardEdge(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := httpkit.DecodeBody(c)
		if !ok {
			return
		}
		source, ok := httpkit.RequiredString(c, body, "source_item_id")
		if !ok {
			return
		}
		target, ok := httpkit.RequiredString(c, body, "target_item_id")
		if !ok {
			return
		}
		relation, ok := httpkit.RequiredString(c, body, "relation")
		if !ok {
			return
		}
		expected, ok := bodyRevision(c, body)
		if !ok {
			return
		}
		result, err := d.Boards.AddEdge(
			c.Request.Context(), c.Param("board_id"), httpkit.CurrentUserID(c), source, target, relation,
			httpkit.OptionalBodyInt(body, "ordinal"), httpkit.AnyMap(body["metadata"]), expected,
		)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func deleteCreativeBoardEdge(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		expected, ok := queryRevision(c, "expected_revision")
		if !ok {
			return
		}
		result, err := d.Boards.DeleteEdge(c.Request.Context(), c.Param("board_id"), c.Param("edge_id"), httpkit.CurrentUserID(c), expected)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func deleteCreativeBoard(d CreativeBoardDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		expected, ok := queryRevision(c, "expected_revision")
		if !ok {
			return
		}
		result, err := d.Boards.DeleteBoard(c.Request.Context(), c.Param("board_id"), httpkit.CurrentUserID(c), expected)
		if err != nil {
			abortCreativeBoardErr(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// --- 请求体小工具 ---

// optionalMaybeString 取可选字符串字段，并返回它是否在请求体里出现（含 null）。
func optionalMaybeString(body map[string]any, field string) (*string, bool) {
	raw, present := body[field]
	if !present {
		return nil, false
	}
	if raw == nil {
		return nil, true
	}
	s, ok := raw.(string)
	if !ok {
		return nil, true
	}
	return &s, true
}

// bodyRevision 取可选 expected_revision（ge=1）；缺失返回 nil，非法值写出 422。
func bodyRevision(c *gin.Context, body map[string]any) (*int64, bool) {
	raw, present := body["expected_revision"]
	if !present || raw == nil {
		return nil, true
	}
	v, ok := httpkit.AsInt64(raw)
	if !ok || v < 1 {
		httpkit.AbortFieldConstraint(c, "expected_revision", "greater_than_equal")
		return nil, false
	}
	return &v, true
}

// requiredRevision 取必填 expected_revision（ge=1）。
func requiredRevision(c *gin.Context, body map[string]any) (int64, bool) {
	raw, present := body["expected_revision"]
	if !present || raw == nil {
		httpkit.AbortMissingField(c, "expected_revision")
		return 0, false
	}
	v, ok := httpkit.AsInt64(raw)
	if !ok {
		httpkit.AbortFieldConstraint(c, "expected_revision", "integer")
		return 0, false
	}
	if v < 1 {
		httpkit.AbortFieldConstraint(c, "expected_revision", "greater_than_equal")
		return 0, false
	}
	return v, true
}

// queryRevision 取可选 query 参数 expected_revision（ge=1）。
func queryRevision(c *gin.Context, field string) (*int64, bool) {
	raw, present := c.GetQuery(field)
	if !present || raw == "" {
		return nil, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 1 {
		httpkit.AbortFieldConstraint(c, field, "greater_than_equal")
		return nil, false
	}
	return &v, true
}

// abortCreativeBoardErr 把创意板领域错误映射成统一 {"detail": ...} 信封。
func abortCreativeBoardErr(c *gin.Context, err error) {
	var (
		validation *creative.CreativeBoardValidationError
		notFound   *creative.CreativeBoardNotFoundError
		conflict   *creative.CreativeBoardConflictError
	)
	switch {
	case errors.As(err, &notFound):
		// Python 侧此处是字面量 detail，不走 i18n。
		apperr.Abort(c, apperr.NewLiteral(http.StatusNotFound, "creative_board_not_found"))
	case errors.As(err, &conflict):
		apperr.Abort(c, apperr.New(http.StatusConflict, "creative_board_revision_conflict", map[string]any{
			"current_revision":   conflict.CurrentRevision,
			"current_updated_at": httpkit.IsoTimestamp(conflict.CurrentUpdatedAt),
		}))
	case errors.As(err, &validation):
		apperr.Abort(c, apperr.NewLiteral(http.StatusBadRequest, validation.Msg))
	default:
		apperr.Abort(c, apperr.Internal(err))
	}
}
