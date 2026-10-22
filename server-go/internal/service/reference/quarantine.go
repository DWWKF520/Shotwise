// 违约的 step1 / step2 产出的隔离草稿（对应 lib/reference_video/quarantine.py 与
// lib/reference_video/draft_validation.py 的违约类型与报告渲染）。
//
// 生成一次要付费，产物违约时丢弃重抽既烧钱又不收敛。改为：正式文件一步不动，违约产物连同逐条
// 违约报告落到同目录的隔离草稿，agent 修 content 后经晋升工具按同一校验器全量重判。
//
// 信封形状：{"kind": ..., "episode": N, "meta": {...}, "violations": [...], "content": {...}}
package reference

import (
	"github.com/shotwise/shotwise/internal/service"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/infra/fs"
)

// 隔离草稿的两种产出来源。
const (
	QuarantineKindStep1 = "reference_video_step1"
	QuarantineKindStep2 = "reference_video_step2"

	QuarantineSchemaV1      = 1
	QuarantineSchemaVersion = 2
)

// PromoteToolName / Step1EditToolName 是处置指引要指名的工具名。
const (
	PromoteToolName   = "validate_and_promote_reference_draft"
	Step1EditToolName = "open_reference_step1_for_edit"
)

// quarantineFilenames 按来源映射隔离草稿文件名。
var quarantineFilenames = map[string]string{
	QuarantineKindStep1: service.ReferenceVideoStep1QuarantineName,
	QuarantineKindStep2: service.ReferenceVideoStep2QuarantineName,
}

// quarantineEnvelopeKeys 是信封的已知键；未知键原样保留（前后兼容）。
var quarantineEnvelopeKeys = map[string]bool{
	"schema_version": true, "kind": true, "episode": true,
	"meta": true, "violations": true, "content": true,
}

// DraftViolation 是书写层产出违约：消息含定位与修复出路，供工具错误信封原样回传给 agent。
//
// Code 是违约类的机读标识，Label 是 unit 定位（"unit E1U02" 一类前缀），Line 是该 unit 正文内
// 0-based 的原始行号（仅具体某行的违约才有值）。
type DraftViolation struct {
	Code    string
	Label   string
	Message string
	Line    *int
}

func (v DraftViolation) Error() string { return v.Message }

// RenderViolationReport 把违约条目渲染成逐条编号的报告文本（一行一条，带违约类标注）。
func RenderViolationReport(violations []DraftViolation) string {
	lines := make([]string, 0, len(violations))
	for index, violation := range violations {
		suffix := ""
		if violation.Code != "" {
			suffix = "[" + violation.Code + "] "
		}
		lines = append(lines, fmt.Sprintf("%d. %s%s", index+1, suffix, violation.Message))
	}
	return strings.Join(lines, "\n")
}

// ViolationEntries 把违约条目转成落盘 / 呈现用的结构化 map。
func ViolationEntries(violations []DraftViolation) []map[string]any {
	out := make([]map[string]any, 0, len(violations))
	for _, v := range violations {
		entry := map[string]any{"code": v.Code, "label": v.Label, "message": v.Message, "line": nil}
		if v.Line != nil {
			entry["line"] = *v.Line
		}
		out = append(out, entry)
	}
	return out
}

// QuarantinedDraft 是一份读回内存的隔离草稿。
type QuarantinedDraft struct {
	Kind       string
	Episode    int
	Content    map[string]any
	Violations []map[string]any
	Meta       map[string]any
	Extra      map[string]any
	Path       string
}

// UnsupportedQuarantineSchemaError 表示隔离草稿使用了当前版本无法读取的未来信封版本。
type UnsupportedQuarantineSchemaError struct {
	Version int
	Path    string
}

func (e *UnsupportedQuarantineSchemaError) Error() string {
	location := ""
	if e.Path != "" {
		location = " (" + e.Path + ")"
	}
	return fmt.Sprintf("隔离草稿%s使用了不受支持的 schema_version=%d；当前支持版本为 %d",
		location, e.Version, QuarantineSchemaVersion)
}

// QuarantinePath 返回该集该阶段的隔离草稿路径（与正式 step1 同目录）。
func QuarantinePath(projectDir string, episode int, kind string) string {
	filename, ok := quarantineFilenames[kind]
	if !ok {
		return ""
	}
	return filepath.Join(service.EpisodeDraftsDir(projectDir, episode), filename)
}

// QuarantineExists 判定该集该阶段是否有隔离草稿在场（不解析内容）。
func QuarantineExists(projectDir string, episode int, kind string) bool {
	path := QuarantinePath(projectDir, episode, kind)
	if path == "" {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && !info.IsDir()
}

// ClearQuarantine 清除隔离草稿；缺失时静默。
func ClearQuarantine(projectDir string, episode int, kind string) error {
	path := QuarantinePath(projectDir, episode, kind)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// migrateQuarantineEnvelope 把已知旧信封迁移到当前内存形状；未知 / 未来版本返回错误或 nil。
func migrateQuarantineEnvelope(data map[string]any, path string) (map[string]any, error) {
	raw, ok := data["schema_version"]
	if !ok {
		raw = QuarantineSchemaV1
	}
	version, ok := IntValueOK(raw)
	if !ok {
		return nil, nil
	}
	if version > QuarantineSchemaVersion {
		return nil, &UnsupportedQuarantineSchemaError{Version: version, Path: path}
	}
	if version < QuarantineSchemaV1 {
		return nil, nil
	}
	migrated := make(map[string]any, len(data))
	for k, v := range data {
		migrated[k] = v
	}
	migrated["schema_version"] = QuarantineSchemaVersion
	return migrated, nil
}

// ReadQuarantine 读回隔离草稿；文件缺失 / 非法 JSON / 信封形状坏时返回 (nil, nil)。
//
// kind / episode 须与请求一致，缺失或对不上同样按形状坏处理。
func ReadQuarantine(projectDir string, episode int, kind string) (*QuarantinedDraft, error) {
	path := QuarantinePath(projectDir, episode, kind)
	if path == "" {
		return nil, nil
	}
	data, err := service.ReadJSONObjectOrNone(path)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	migrated, err := migrateQuarantineEnvelope(data, path)
	if err != nil {
		return nil, err
	}
	if migrated == nil {
		return nil, nil
	}
	content, ok := migrated["content"].(map[string]any)
	if !ok {
		return nil, nil
	}
	if kindValue, _ := migrated["kind"].(string); kindValue != kind {
		return nil, nil
	}
	episodeValue, ok := IntValueOK(migrated["episode"])
	if !ok || episodeValue != episode {
		return nil, nil
	}
	var violations []map[string]any
	if raw, ok := migrated["violations"].([]any); ok {
		for _, item := range raw {
			if entry, ok := item.(map[string]any); ok {
				violations = append(violations, entry)
			}
		}
	}
	meta, _ := migrated["meta"].(map[string]any)
	extra := map[string]any{}
	for key, value := range migrated {
		if !quarantineEnvelopeKeys[key] {
			extra[key] = value
		}
	}
	return &QuarantinedDraft{
		Kind: kind, Episode: episode, Content: content,
		Violations: violations, Meta: meta, Extra: extra, Path: path,
	}, nil
}

// WriteQuarantine 把违约产物与报告写入隔离草稿（原子写，整份覆盖），返回草稿路径。
func WriteQuarantine(
	projectDir string, episode int, kind string,
	content map[string]any, violations []DraftViolation,
	meta, extra map[string]any,
) (string, error) {
	path := QuarantinePath(projectDir, episode, kind)
	if path == "" {
		return "", fmt.Errorf("未知隔离草稿类型: %s", kind)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	existing, err := service.ReadJSONObjectOrNone(path)
	if err != nil {
		return "", err
	}
	envelope := map[string]any{}
	if existing != nil {
		for key, value := range existing {
			if !quarantineEnvelopeKeys[key] {
				envelope[key] = value
			}
		}
	}
	for key, value := range extra {
		envelope[key] = value
	}
	if meta == nil {
		meta = map[string]any{}
	}
	envelope["schema_version"] = QuarantineSchemaVersion
	envelope["kind"] = kind
	envelope["episode"] = episode
	envelope["meta"] = meta
	envelope["violations"] = ViolationEntries(violations)
	envelope["content"] = content
	if err := fs.WriteJSONAtomic(path, envelope); err != nil {
		return "", err
	}
	return path, nil
}

// QuarantineAndReport 是违约处置的单一出口：落隔离草稿 + 渲染报告，返回给 agent 的报告文本。
func QuarantineAndReport(
	projectDir string, episode int, kind string,
	content map[string]any, violations []DraftViolation,
	meta, extra map[string]any,
) (string, error) {
	path, err := WriteQuarantine(projectDir, episode, kind, content, violations, meta, extra)
	if err != nil {
		return "", err
	}
	return RenderQuarantineReport(path, kind, violations, episode), nil
}

// RenderQuarantineReport 渲染回给 agent 的违约报告：逐条定位 + 按处置路径写的修复指引。
func RenderQuarantineReport(draftPath, kind string, violations []DraftViolation, episode int) string {
	stage := "step1 拆分"
	field := "units[i].text / source_text / duration_seconds"
	if kind == QuarantineKindStep2 {
		stage = "step2 视觉展开"
		field = "units[i].text"
	}
	return fmt.Sprintf("❌ %s产出有 %d 处违约，已隔离到草稿（正式文件未被改动）：%s\n\n%s\n\n"+
		"处置：直接编辑该草稿的 content.%s 修正违约；"+
		"若违约是「资产名未登记」，也可改为在 project.json 登记该资产、或改用已登记的名称。\n"+
		"改完调用 %s({\"episode\": %d}) 重新全量校验并晋升为正式文件；"+
		"仍有违约时会返回刷新后的报告，可继续修改再晋升，无轮次上限。",
		stage, len(violations), draftPath, RenderViolationReport(violations), field, PromoteToolName, episode)
}


// IntValueOK 取整数语义的值（bool 不算）。
func IntValueOK(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	}
	return 0, false
}
