package httpkit

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shotwise/shotwise/internal/apperr"
	"github.com/shotwise/shotwise/internal/service"
)

// DecodeJSONBody 解析请求体；畸形 JSON 写出 FastAPI 风格的 422。
func DecodeJSONBody(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body"}, Msg: "invalid request body", Type: "json_invalid"},
		}})
		return false
	}
	return true
}

// DecodeBody 把请求体读成 map，保持 extra="allow" 语义：定长结构体会静默丢掉
// 对端（上传流程、Python 服务）写入的字段。
func DecodeBody(c *gin.Context) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(c.Request.Body).Decode(&body); err != nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body"}, Msg: "invalid JSON body", Type: "value_error.jsondecode"},
		}})
		return nil, false
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, true
}

// RequiredString 取必填字符串字段；缺失或类型不对时按 FastAPI 的 422 形状出参。
func RequiredString(c *gin.Context, body map[string]any, field string) (string, bool) {
	raw, present := body[field]
	if !present || raw == nil {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body", field}, Msg: "field required", Type: "value_error.missing"},
		}})
		return "", false
	}
	s, ok := raw.(string)
	if !ok {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{
			{Loc: []string{"body", field}, Msg: "value is not a valid string", Type: "type_error.str"},
		}})
		return "", false
	}
	return s, true
}

// AnyMap 取一个对象字段；缺失 / null / 非对象都返回 nil（调用方按“未提供”处理）。
func AnyMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	m, _ := value.(map[string]any)
	return m
}

// OptionalBodyString 取可选字符串字段；缺失 / null 返回 nil。
func OptionalBodyString(body map[string]any, field string) *string {
	raw, present := body[field]
	if !present || raw == nil {
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	return &s
}

// OptionalBodyInt 取可选整数字段；缺失 / null 返回 nil。
func OptionalBodyInt(body map[string]any, field string) *int64 {
	raw, present := body[field]
	if !present || raw == nil {
		return nil
	}
	if v, ok := AsInt64(raw); ok {
		return &v
	}
	return nil
}

// PathInt64 解析正整数路径参数；非法时写出 FastAPI 风格 422。
func PathInt64(c *gin.Context, name string) (int64, bool) {
	raw := c.Param(name)
	value, ok := QueryInt64(c, name)
	if !ok || raw == "" {
		apperr.Abort(c, &apperr.Validation{Items: []apperr.ValidationItem{{
			Loc: []string{"path", name}, Msg: "Input should be a valid integer", Type: "int_parsing",
		}}})
		return 0, false
	}
	return value, true
}

// QueryInt64 解析整数路径参数。
func QueryInt64(c *gin.Context, name string) (int64, bool) {
	raw := c.Param(name)
	if raw == "" {
		return 0, false
	}
	var value int64
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		value = value*10 + int64(ch-'0')
	}
	return value, true
}

// ReadBounded 读满上限内的全部字节；超限返回上传超限错误。
func ReadBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, &service.UploadValidationError{Status: 413, Key: "upload_too_large", Params: map[string]any{
			"max_mb": maxBytes / (1024 * 1024),
		}}
	}
	return data, nil
}

// QueryAfterSeq 解析 ?after=<seq> 游标；缺省或非法按 -1（从头）处理。
func QueryAfterSeq(c *gin.Context) int64 {
	raw := strings.TrimSpace(c.Query("after"))
	if raw == "" {
		return -1
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return -1
	}
	return parsed
}

// AsInt64 把 JSON 解码后的数值（float64 / int64 / int）归一为 int64。
func AsInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}
