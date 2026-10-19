// 项目源文上传与 AI 概述生成。对应 projects.py 的 POST /source 与 /generate-overview，
// 以及 project_manager.generate_overview / _read_source_files。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/textgen"
)

// 源文上传常量（与 Python 一致）。
const (
	// SourceMaxChars 是源文最大字符数（约 10 万汉字）。
	SourceMaxChars = 200_000
	// OverviewSourceMaxChars 是概述生成读取源文的上限。
	OverviewSourceMaxChars = 50_000
)

// sourceTextSuffixes 是概述聚合认的源文本后缀。
var sourceTextSuffixes = map[string]bool{".txt": true, ".md": true}

// 源文与概述域错误（传输层映射状态码）。
var (
	ErrSourceEmpty         = errors.New("overview_source_empty")
	ErrSourceDecode        = errors.New("invalid_encoding")
	ErrOverviewInvalid     = errors.New("overview_ai_response_invalid")
	ErrTextProviderMissing = errors.New("text_provider_not_configured")
)

// SourceReadError 是源目录存在非 UTF-8 文件（绕过 API 拷入）。
type SourceReadError struct{ Filename string }

func (e *SourceReadError) Error() string { return "source decode: " + e.Filename }

// ProjectSource 承载源文上传与概述生成。
type ProjectSource struct {
	store *ProjectStore
	db    *gorm.DB
}

// NewProjectSource 构造服务。
func NewProjectSource(store *ProjectStore, gdb *gorm.DB) *ProjectSource {
	return &ProjectSource{store: store, db: gdb}
}

// SetSourceInput 是源文上传入参（content 与 file 二选一）。
type SetSourceInput struct {
	Content          *string // 直接提交的文本（命名 novel.txt）
	FileContent      []byte  // 上传的原始字节
	OriginalName     string  // 上传文件名（取 basename）
	GenerateOverview bool
}

// SourceValueError 是带翻译参数的源文校验错误。
type SourceValueError struct {
	Msg    string
	Params map[string]any
}

func (e *SourceValueError) Error() string { return e.Msg }

// SetSourceResult 是上传结果（可选附带概述）。
type SetSourceResult struct {
	Filename      string
	Chars         int
	Overview      map[string]any // nil = 未生成或生成失败
	OverviewError string         // 概述失败的翻译 key（降级回传，不影响上传成功）
}

// SetSource 写源文件；generate_overview 时附带生成概述（失败降级不回滚上传）。
func (s *ProjectSource) SetSource(ctx context.Context, project string, in SetSourceInput) (*SetSourceResult, error) {
	if len(in.FileContent) == 0 && in.Content == nil {
		return nil, &SourceValueError{Msg: "content_or_file_required"}
	}
	if len(in.FileContent) > 0 && in.Content != nil {
		return nil, &SourceValueError{Msg: "one_of_content_or_file"}
	}

	projectDir, err := s.store.Dir(project)
	if err != nil {
		return nil, ErrAssetNotFound
	}
	sourceDir := filepath.Join(projectDir, "source")
	var filename string
	var text string
	if len(in.FileContent) > 0 {
		safeName := filepath.Base(strings.ReplaceAll(in.OriginalName, "\\", "/"))
		suffix := strings.ToLower(filepath.Ext(safeName))
		if !sourceTextSuffixes[suffix] {
			return nil, &SourceValueError{Msg: "unsupported_file_type", Params: map[string]any{"name": suffix}}
		}
		decoded, err := decodeUTF8(in.FileContent)
		if err != nil {
			return nil, ErrSourceDecode
		}
		if len([]rune(decoded)) > SourceMaxChars {
			return nil, &SourceValueError{Msg: "file_too_large", Params: map[string]any{"max_chars": SourceMaxChars}}
		}
		text = decoded
		filename = safeName
	} else {
		text = *in.Content
		if len([]rune(text)) > SourceMaxChars {
			return nil, &SourceValueError{Msg: "file_too_large", Params: map[string]any{"max_chars": SourceMaxChars}}
		}
		filename = "novel.txt"
	}
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(sourceDir, filename), []byte(text), 0o644); err != nil {
		return nil, err
	}

	result := &SetSourceResult{Filename: filename, Chars: len([]rune(text))}
	if !in.GenerateOverview {
		return result, nil
	}
	// 概述是上传的可选后续：失败仅降级回传提示，不影响上传成功。
	overview, err := s.GenerateOverview(ctx, project)
	if err != nil {
		result.OverviewError = overviewErrorKey(err)
		return result, nil
	}
	result.Overview = overview
	return result, nil
}

// GenerateOverview 用文本模型生成项目概述并落 project.json。
//
// 与 Python 同语义：source 目录为空 → overview_source_empty；模型输出不合 schema →
// overview_ai_response_invalid（最后防线，裸错误串不透传）；供应商未配置 →
// text_provider_not_configured。
func (s *ProjectSource) GenerateOverview(ctx context.Context, project string) (map[string]any, error) {
	pf, err := s.store.Get(project)
	if err != nil {
		return nil, ErrAssetNotFound
	}
	sourceContent, err := s.readSourceFiles(project, OverviewSourceMaxChars)
	if err != nil {
		return nil, err
	}
	if sourceContent == "" {
		return nil, ErrSourceEmpty
	}
	generator, err := textgen.NewGenerator(ctx, s.db, pf, textgen.TaskOverview)
	if err != nil {
		return nil, ErrTextProviderMissing
	}
	sourceKind := resolveSourceKind(pf)
	targetLanguage := strings.TrimSpace(pf.SourceLanguage)
	if targetLanguage == "" {
		targetLanguage = "中文"
	}
	response, err := generator.Generate(ctx, buildOverviewPrompt(sourceContent, sourceKind, targetLanguage),
		overviewSchema(), 0)
	if err != nil {
		return nil, ErrTextProviderMissing
	}
	overview, err := parseOverviewResponse(response)
	if err != nil {
		return nil, ErrOverviewInvalid
	}
	overview["generated_at"] = time.Now().UTC().Format("2006-01-02T15:04:05.999999+00:00")
	language, _ := overview["language"].(string)
	// 落 project.json 用域结构体（Overview 是强类型）；响应仍回 map（与 Python model_dump 同形）。
	if err := s.store.Update(project, func(updated *domain.ProjectFile) error {
		raw, marshalErr := json.Marshal(overview)
		if marshalErr != nil {
			return marshalErr
		}
		var parsed domain.Overview
		if unmarshalErr := json.Unmarshal(raw, &parsed); unmarshalErr != nil {
			return unmarshalErr
		}
		updated.Overview = &parsed
		updated.SourceLanguage = language
		return nil
	}); err != nil {
		return nil, err
	}
	return overview, nil
}

// readSourceFiles 聚合 source 目录的 UTF-8 文本文件（排序、截断到 max_chars）。
//
// 非 UTF-8 文件显式报错：上传路径已统一规范化，这里再遇到说明用户绕过 API 拷入，
// 「源目录为空」的提示会误导排查方向。
func (s *ProjectSource) readSourceFiles(project string, maxChars int) (string, error) {
	projectDir, err := s.store.Dir(project)
	if err != nil {
		return "", ErrAssetNotFound
	}
	sourceDir := filepath.Join(projectDir, "source")
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !sourceTextSuffixes[strings.ToLower(filepath.Ext(entry.Name()))] {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	var parts []string
	total := 0
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(sourceDir, name))
		if err != nil {
			continue
		}
		content, err := decodeUTF8(raw)
		if err != nil {
			return "", &SourceReadError{Filename: name}
		}
		remaining := maxChars - total
		if remaining <= 0 {
			break
		}
		runes := []rune(content)
		if len(runes) > remaining {
			content = string(runes[:remaining])
		}
		parts = append(parts, "--- "+name+" ---\n"+content)
		total += len([]rune(content))
	}
	return strings.Join(parts, "\n\n"), nil
}

// overviewSchema 是 ProjectOverview 的 JSON Schema（供 Structured Outputs）。
func overviewSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"synopsis":      map[string]any{"type": "string", "description": "故事梗概，200-300字，概括主线剧情"},
			"genre":         map[string]any{"type": "string", "description": "题材类型，如：古装宫斗、现代悬疑、玄幻修仙"},
			"theme":         map[string]any{"type": "string", "description": "核心主题，如：复仇与救赎、成长与蜕变"},
			"world_setting": map[string]any{"type": "string", "description": "时代背景和世界观设定，100-200字"},
			"language":      map[string]any{"type": "string", "enum": []string{"zh", "en", "vi"}, "description": "小说源语言代码"},
		},
		"required":             []string{"synopsis", "genre", "theme", "world_setting", "language"},
		"additionalProperties": false,
	}
}

// parseOverviewResponse 解析并校验模型输出。
func parseOverviewResponse(response string) (map[string]any, error) {
	trimmed := strings.TrimSpace(response)
	// 剥掉可能的 markdown 代码围栏（部分后端即使 Structured Outputs 也会带）。
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	var overview map[string]any
	if err := json.Unmarshal([]byte(trimmed), &overview); err != nil {
		return nil, err
	}
	for _, key := range []string{"synopsis", "genre", "theme", "world_setting", "language"} {
		if value, ok := overview[key].(string); !ok || strings.TrimSpace(value) == "" {
			return nil, errors.New("overview field missing: " + key)
		}
	}
	lang, _ := overview["language"].(string)
	switch lang {
	case "zh", "en", "vi":
	default:
		return nil, errors.New("overview language invalid")
	}
	return overview, nil
}

// resolveSourceKind 解析源类型（screenplay 优先提取作者创作方案）。
func resolveSourceKind(pf *domain.ProjectFile) string {
	if pf == nil {
		return "novel"
	}
	if pf.SourceKind == "screenplay" {
		return "screenplay"
	}
	return "novel"
}

// buildOverviewPrompt 构造概述生成 prompt（与 Python build_overview_prompt 同形）。
func buildOverviewPrompt(sourceContent, sourceKind, targetLanguage string) string {
	task := "请分析以下小说内容，提取关键信息："
	if sourceKind == "screenplay" {
		task = "请分析以下成品剧本，提炼项目概述（题材 / 主题 / 故事梗概 / 世界观）。\n" +
			"剧本里可能附有作者写下的创作方案——以任意形态（开篇前言、大纲、设定卡等，标题与排版各异）" +
			"写明题材、主题、一句话故事、世界观等核心设定。若能识别出这类创作方案，" +
			"请优先照用作者已写下的设定填充对应字段（忠于原意，可精炼归并、不另起炉灶重新推断）；" +
			"剧本未附创作方案时，再从剧本正文自行归纳。"
	}
	return task + "\n\n**输出语言**：所有字符串值必须使用 " + targetLanguage +
		"；JSON 键名 / 枚举值保持英文。\n\n" + sourceContent
}

// overviewErrorKey 把概述错误映射为回传的翻译 key。
func overviewErrorKey(err error) string {
	switch {
	case errors.Is(err, ErrOverviewInvalid):
		return "overview_ai_response_invalid"
	case errors.Is(err, ErrSourceEmpty):
		return "overview_source_empty"
	default:
		return "overview_generation_failed"
	}
}

// decodeUTF8 严格 UTF-8 校验 + 解码（非 UTF-8 一律报错，不做猜测性转码）。
func decodeUTF8(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", errors.New("invalid utf-8")
	}
	return string(raw), nil
}
