// Package apperr 定义面向 HTTP 的统一错误信封。
//
// 与 Python 侧一致：所有错误响应统一为 {"detail": ...}，
// detail 要么是已翻译的字符串，要么是 {"code": ..., ...params} 对象。
// 401 一律带 WWW-Authenticate: Bearer。
package apperr

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/i18n"
)

// Error 是业务错误。Code 是稳定机器码（同时作为 i18n key），Params 用于渲染。
type Error struct {
	Status int
	Code   string
	Params map[string]any
	Object bool // true → detail 渲染成 {"code":..., ...params}；false → 渲染成翻译后的字符串
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Err }

// Is 让 errors.Is 能按 code 匹配。
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

// New 构造对象形 detail 的错误（业务码场景）。
func New(status int, code string, params map[string]any) *Error {
	return &Error{Status: status, Code: code, Params: params, Object: true}
}

// NewText 构造字符串形 detail 的错误（i18n key 渲染）。
func NewText(status int, key string, params map[string]any) *Error {
	return &Error{Status: status, Code: key, Params: params, Object: false}
}

func BadRequest(code string, params map[string]any) *Error  { return New(http.StatusBadRequest, code, params) }
func NotFound(code string, params map[string]any) *Error    { return New(http.StatusNotFound, code, params) }
func Conflict(code string, params map[string]any) *Error    { return New(http.StatusConflict, code, params) }
func Unavailable(code string, params map[string]any) *Error { return New(http.StatusServiceUnavailable, code, params) }

// Internal 服务端错误。原始 error 只进日志，绝不回传客户端。
func Internal(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: "internal_server_error", Object: false, Err: err}
}

// Validation 复刻 FastAPI 的 RequestValidationError 输出形状。
type Validation struct{ Items []ValidationItem }

type ValidationItem struct {
	Loc  []string `json:"loc"`
	Msg  string   `json:"msg"`
	Type string   `json:"type"`
}

func (v *Validation) Error() string { return "validation_error" }

// Detail 渲染 detail 字段的值。
func (e *Error) Detail(loc i18n.Locale) any {
	if !e.Object {
		return i18n.T(loc, e.Code, e.Params)
	}
	out := map[string]any{"code": e.Code}
	for k, v := range e.Params {
		if k == "code" {
			continue
		}
		out[k] = v
	}
	return out
}

// Abort 把错误写进 gin 上下文。日志里保留原始 error，响应体只有 code/翻译文案。
func Abort(c *gin.Context, err error) {
	loc := i18n.FromRequest(c.Request)
	status := http.StatusInternalServerError
	detail := any(i18n.T(loc, "internal_server_error", nil))

	switch e := err.(type) {
	case *Error:
		status = e.Status
		detail = e.Detail(loc)
	case *Validation:
		status = http.StatusUnprocessableEntity
		detail = e.Items
	}

	if status == http.StatusUnauthorized {
		c.Header("WWW-Authenticate", "Bearer")
	}
	c.AbortWithStatusJSON(status, gin.H{"detail": detail})
}
