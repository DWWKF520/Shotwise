package service

import "net/http"

// GenerateError 是生成请求校验错误（携带 HTTP 状态码与 i18n 参数；原 generation/generate.go 内联件）。
// GenerateError 是生成入队路径的领域错误：Status 是 HTTP 状态，Code 是 i18n errors key。
type GenerateError struct {
	Status int
	Code   string
	Params map[string]any
}

func (e *GenerateError) Error() string { return e.Code }

func GenerateBadRequest(code string, params map[string]any) *GenerateError {
	return &GenerateError{Status: http.StatusBadRequest, Code: code, Params: params}
}
