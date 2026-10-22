package service

import "errors"

// ValueError 对应 Python 的 ValueError（如非法相对路径）。
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// ErrAssetNotFound 表示资产条目不存在（HTTP 404 语义）。
var ErrAssetNotFound = errors.New("asset not found")

// MediaAssetNotFoundError 表示索引里没有这条媒体资产（对应 Python 的 KeyError）。
type MediaAssetNotFoundError struct{ MediaAssetID string }

func (e *MediaAssetNotFoundError) Error() string { return e.MediaAssetID }

// UploadValidationError 是上传校验 / 落盘超限失败；transport 按 (Status, Key, Params) 翻译。
// media（镜头上传）与 reference（参考生视频上传）两域共用同一翻译口径。
type UploadValidationError struct {
	Status int
	Key    string
	Params map[string]any
}

func (e *UploadValidationError) Error() string { return e.Key }

// UploadVideoMaxBytes 是用户上传视频的字节上限（两域上传链路共用同一值）。
const UploadVideoMaxBytes = 200 * 1024 * 1024

// UploadVersionSource 是用户上传产物在版本记录里的来源标识（reference / media / generation 共用）。
const UploadVersionSource = "manual_upload"

// MediaOriginExtracted 是媒体来源「从成品抽取」的取值（generation 画布链路与 media 索引共用）。
const MediaOriginExtracted = "extracted"

// MediaOriginGenerated 是媒体来源「生成产出」的取值（generation 链路与 media 索引共用）。
const MediaOriginGenerated = "generated"
