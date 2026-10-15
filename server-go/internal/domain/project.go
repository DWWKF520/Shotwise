// Package domain 定义领域模型。project.json 是项目元数据与资产目录的唯一真相源。
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SchemaVersion 与 Python 侧 CURRENT_SCHEMA_VERSION 对齐。
const SchemaVersion = 5

// ContentMode 决定剧本结构与 agent profile 加载哪个 CLAUDE.*.md 变体。
type ContentMode string

const (
	ContentNarration ContentMode = "narration"
	ContentDrama     ContentMode = "drama"
	ContentAd        ContentMode = "ad"
)

// GenerationMode 是项目级生成路线，创建时二选一、创建后不可更改。
type GenerationMode string

const (
	GenerationStoryboard     GenerationMode = "storyboard"
	GenerationReferenceVideo GenerationMode = "reference_video"
)

// ValidContentModes / ValidGenerationModes 用于校验。
var (
	ValidContentModes    = []ContentMode{ContentNarration, ContentDrama, ContentAd}
	ValidGenerationModes = []GenerationMode{GenerationStoryboard, GenerationReferenceVideo}
)

// IsValidContentMode 判断内容模式是否合法。
func IsValidContentMode(m string) bool {
	for _, v := range ValidContentModes {
		if string(v) == m {
			return true
		}
	}
	return false
}

// IsValidGenerationMode 判断生成路线是否合法。
func IsValidGenerationMode(m string) bool {
	for _, v := range ValidGenerationModes {
		if string(v) == m {
			return true
		}
	}
	return false
}

// ProjectSubdirs 是创建项目时建立的子目录。
var ProjectSubdirs = []string{
	"source", "scripts", "drafts", "characters", "scenes", "props", "products",
	"storyboards", "videos", "thumbnails", "output", "grids",
}

// projectNameRe 项目名只允许字母、数字与连字符。
var projectNameRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// ErrInvalidProjectName 项目名非法。
var ErrInvalidProjectName = errors.New("invalid project name")

// ValidateProjectName 校验并归一项目名。
func ValidateProjectName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if !projectNameRe.MatchString(n) {
		return "", ErrInvalidProjectName
	}
	return n, nil
}

// --- project.json 结构 ---

// Overview 是 AI 生成的项目概述。
type Overview struct {
	Synopsis     string `json:"synopsis,omitempty"`
	Genre        string `json:"genre,omitempty"`
	Theme        string `json:"theme,omitempty"`
	WorldSetting string `json:"world_setting,omitempty"`
	Language     string `json:"language,omitempty"`
	GeneratedAt  string `json:"generated_at,omitempty"`
}

// EpisodeMeta 是 project.json.episodes 的一项。
type EpisodeMeta struct {
	Episode        int          `json:"episode"`
	Title          string       `json:"title"`
	ScriptFile     string       `json:"script_file"`
	DisplayEpisode *int         `json:"display_episode,omitempty"`
	LedgerStatus   string       `json:"ledger_status,omitempty"`
	Hook           string       `json:"hook,omitempty"`
	SourceRange    *SourceRange `json:"source_range,omitempty"`
	Outline        *Outline     `json:"outline,omitempty"`
}

// SourceRange 记录该集在源文件中的字符区间。
type SourceRange struct {
	SourceFile string `json:"source_file"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
}

// Outline 是分集大纲。
type Outline struct {
	StoryBeats        []string `json:"story_beats,omitempty"`
	NextEpisodeTeaser string   `json:"next_episode_teaser,omitempty"`
}

// ProjectMetadata 是 project.json.metadata。
type ProjectMetadata struct {
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ProjectFile 是 project.json 的完整结构。
//
// 字段顺序刻意与 Python 侧 create_project_metadata() 保持一致，便于逐字段核对。
type ProjectFile struct {
	SchemaVersion int    `json:"schema_version"`
	Title         string `json:"title"`

	ContentMode    string `json:"content_mode"`
	SourceKind     string `json:"source_kind,omitempty"`
	GenerationMode string `json:"generation_mode"`
	GridStoryboard bool   `json:"grid_storyboard,omitempty"`

	AspectRatio     string   `json:"aspect_ratio,omitempty"`
	Style           string   `json:"style"`
	StyleTemplateID string   `json:"style_template_id,omitempty"`
	StyleImage      string   `json:"style_image,omitempty"`
	DefaultDuration *int     `json:"default_duration,omitempty"`
	TargetDuration  *int     `json:"target_duration,omitempty"`
	Brief           string   `json:"brief,omitempty"`
	SourceLanguage  string   `json:"source_language,omitempty"`
	SpeechRate      *float64 `json:"speech_rate_units_per_second,omitempty"`

	NarrationVoice     string   `json:"narration_voice,omitempty"`
	NarrationSpeed     *float64 `json:"narration_speed,omitempty"`
	VideoGenerateAudio *bool    `json:"video_generate_audio,omitempty"`

	// 分集规划覆盖项（null/缺省 = 工具内部默认）。
	EpisodeTargetUnits  *int `json:"episode_target_units,omitempty"`
	PlanningWindowChars *int `json:"planning_window_chars,omitempty"`
	PlanningMaxEpisodes *int `json:"planning_max_episodes,omitempty"`

	// 后端覆盖（可选，空 = 继承全局）
	VideoBackend        string                       `json:"video_backend,omitempty"`
	VideoProviderI2V    string                       `json:"video_provider_i2v,omitempty"`
	VideoProviderR2V    string                       `json:"video_provider_r2v,omitempty"`
	ImageProviderT2I    string                       `json:"image_provider_t2i,omitempty"`
	ImageProviderI2I    string                       `json:"image_provider_i2i,omitempty"`
	DefaultImageBackend string                       `json:"default_image_backend,omitempty"`
	AudioBackend        string                       `json:"audio_backend,omitempty"`
	TextBackendSimple   string                       `json:"text_backend_simple,omitempty"`
	TextBackendComplex  string                       `json:"text_backend_complex,omitempty"`
	DefaultTextBackend  string                       `json:"default_text_backend,omitempty"`
	ModelSettings       map[string]map[string]string `json:"model_settings,omitempty"`

	Overview *Overview `json:"overview,omitempty"`

	// 关系图是 {revision, edges[], suppressed_pairs[], node_positions{}} 结构，
	// 节点身份直接用角色名，因此改名/删角色都要同步它。
	CharacterRelations map[string]any `json:"character_relations,omitempty"`

	Metadata ProjectMetadata `json:"metadata,omitempty"`

	Episodes []EpisodeMeta `json:"episodes,omitempty"`

	Characters AssetBucket `json:"characters,omitempty"`
	Scenes     AssetBucket `json:"scenes,omitempty"`
	Props      AssetBucket `json:"props,omitempty"`
	Products   AssetBucket `json:"products,omitempty"`
}

// NewProjectFile 构造新项目的 project.json。
// contentMode / generationMode / sourceKind 创建即定，创建后不可更改。
func NewProjectFile(title string, mode ContentMode, genMode GenerationMode) *ProjectFile {
	now := time.Now().UTC().Format(time.RFC3339)
	return &ProjectFile{
		SchemaVersion:  SchemaVersion,
		Title:          title,
		ContentMode:    string(mode),
		GenerationMode: string(genMode),
		SourceKind:     "novel",
		AspectRatio:    "9:16",
		Metadata:       ProjectMetadata{CreatedAt: now, UpdatedAt: now},
		Episodes:       []EpisodeMeta{},
		Characters:     AssetBucket{},
		Scenes:         AssetBucket{},
		Props:          AssetBucket{},
		Products:       AssetBucket{},
	}
}

// IsReferenceVideo 判断是否走参考路线。
func (p *ProjectFile) IsReferenceVideo() bool {
	return p.GenerationMode == string(GenerationReferenceVideo)
}

// BucketRef 返回指定类型资产桶的指针，供写入路径就地改桶。
func (p *ProjectFile) BucketRef(kind AssetKind) (*AssetBucket, bool) {
	switch kind {
	case AssetCharacter:
		return &p.Characters, true
	case AssetScene:
		return &p.Scenes, true
	case AssetProp:
		return &p.Props, true
	case AssetProduct:
		return &p.Products, true
	}
	return nil, false
}

// ValidateErrors 收集 project.json 的全部结构问题。
//
// 写盘守卫用的是「不更坏」语义（见 service.ProjectStore.Update）：存量项目可能本就带
// 着历史遗留问题，只要这次写入没有引入**新的**错误就不该拒绝，否则老项目的资产一改都写不回去。
func (p *ProjectFile) ValidateErrors() []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if p.Title == "" {
		add("title is required")
	}
	if !IsValidContentMode(p.ContentMode) {
		add("content_mode must be one of narration/drama/ad, got %q", p.ContentMode)
	}
	if !IsValidGenerationMode(p.GenerationMode) {
		add("generation_mode must be storyboard or reference_video, got %q", p.GenerationMode)
	}
	if p.SourceKind != "" && p.SourceKind != "novel" && p.SourceKind != "screenplay" {
		add("source_kind must be novel or screenplay, got %q", p.SourceKind)
	}
	if strings.TrimSpace(p.Style) == "" {
		add("style is required")
	}
	if p.ContentMode == string(ContentAd) {
		// ad 项目：时长必填、禁止 default_duration、禁止宫格、单集
		if p.TargetDuration == nil || *p.TargetDuration <= 0 {
			add("target_duration must be a positive integer for ad projects")
		}
		if p.DefaultDuration != nil {
			add("default_duration is not allowed for ad projects")
		}
		if p.GridStoryboard {
			add("grid_storyboard is not allowed for ad projects")
		}
		if len(p.Episodes) != 1 || p.Episodes[0].Episode != 1 {
			add("ad projects must have exactly one episode with episode=1")
		}
	} else if p.TargetDuration != nil {
		add("target_duration is only allowed for ad projects")
	}
	for _, ep := range p.Episodes {
		if ep.ScriptFile == "" {
			add("episode %d: script_file is required", ep.Episode)
		}
	}
	for _, kind := range AssetTypeOrder {
		bucket, ok := p.BucketRef(kind)
		if !ok || bucket == nil {
			continue
		}
		for name, entry := range *bucket {
			if entry == nil || strings.TrimSpace(entry.Description()) == "" {
				add("%s %q: description is required", kind, name)
			}
		}
	}
	return errs
}

// Validate 返回第一个结构问题，无问题时返回 nil。
func (p *ProjectFile) Validate() error {
	if errs := p.ValidateErrors(); len(errs) > 0 {
		return errors.New(errs[0])
	}
	return nil
}

// Clone 深拷贝项目，供 dry_run 与「不更坏」比对在副本上试算。
func (p *ProjectFile) Clone() *ProjectFile {
	out := *p
	if p.Episodes != nil {
		out.Episodes = append([]EpisodeMeta(nil), p.Episodes...)
	}
	if v := cloneValue(p.Characters); v != nil {
		out.Characters = v.(AssetBucket)
	}
	if v := cloneValue(p.Scenes); v != nil {
		out.Scenes = v.(AssetBucket)
	}
	if v := cloneValue(p.Props); v != nil {
		out.Props = v.(AssetBucket)
	}
	if v := cloneValue(p.Products); v != nil {
		out.Products = v.(AssetBucket)
	}
	if v := cloneValue(p.CharacterRelations); v != nil {
		out.CharacterRelations = v.(map[string]any)
	}
	if p.Overview != nil {
		overview := *p.Overview
		out.Overview = &overview
	}
	return &out
}

// NewErrors 返回 after 中不在 allow 里的错误，即本次写入新引入的问题。
func NewErrors(after, allow []string) []string {
	allowed := make(map[string]bool, len(allow))
	for _, e := range allow {
		allowed[e] = true
	}
	var out []string
	for _, e := range after {
		if !allowed[e] {
			out = append(out, e)
		}
	}
	return out
}
