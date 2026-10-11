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
	ValidContentModes     = []ContentMode{ContentNarration, ContentDrama, ContentAd}
	ValidGenerationModes  = []GenerationMode{GenerationStoryboard, GenerationReferenceVideo}
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

// AssetKind 是资产类型。四类资产共用一套抽象（对应 Python 的 ASSET_SPECS）。
type AssetKind string

const (
	AssetCharacter AssetKind = "character"
	AssetScene     AssetKind = "scene"
	AssetProp      AssetKind = "prop"
	AssetProduct   AssetKind = "product"
)

// AssetSpec 描述一类资产的存储契约。
type AssetSpec struct {
	Kind      AssetKind
	BucketKey string // project.json 上的键名
	Subdir    string // 项目目录下的子目录
	SheetKey  string // 定妆图/设计稿字段名
}

// AssetSpecs 是资产规格的单一真相源，对应 lib/asset_types.ASSET_SPECS。
var AssetSpecs = map[AssetKind]AssetSpec{
	AssetCharacter: {Kind: AssetCharacter, BucketKey: "characters", Subdir: "characters", SheetKey: "character_sheet"},
	AssetScene:     {Kind: AssetScene, BucketKey: "scenes", Subdir: "scenes", SheetKey: "scene_sheet"},
	AssetProp:      {Kind: AssetProp, BucketKey: "props", Subdir: "props", SheetKey: "prop_sheet"},
	AssetProduct:   {Kind: AssetProduct, BucketKey: "products", Subdir: "products", SheetKey: "product_sheet"},
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
	Episode       int    `json:"episode"`
	Title         string `json:"title"`
	ScriptFile    string `json:"script_file"`
	DisplayEpisode *int  `json:"display_episode,omitempty"`
	LedgerStatus  string `json:"ledger_status,omitempty"`
	Hook          string `json:"hook,omitempty"`
	SourceRange   *SourceRange `json:"source_range,omitempty"`
	Outline       *Outline     `json:"outline,omitempty"`
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

// Character 是角色资产条目。
type Character struct {
	Description             string   `json:"description"`
	CharacterSheet          string   `json:"character_sheet,omitempty"`
	VoiceStyle              string   `json:"voice_style,omitempty"`
	ReferenceImage          string   `json:"reference_image,omitempty"`
	ReferenceAudio          string   `json:"reference_audio,omitempty"`
	CharacterAvatar         string   `json:"character_avatar,omitempty"`
	VoiceNoticeDismissedAt  string   `json:"voice_notice_dismissed_at,omitempty"`
	VoiceUpdatedAt          string   `json:"voice_updated_at,omitempty"`
}

// Scene 是场景资产条目。
type Scene struct {
	Description string `json:"description"`
	SceneSheet  string `json:"scene_sheet,omitempty"`
}

// Prop 是道具资产条目。
type Prop struct {
	Description string `json:"description"`
	PropSheet   string `json:"prop_sheet,omitempty"`
}

// Product 是产品资产条目。
type Product struct {
	Description     string   `json:"description"`
	ProductSheet    string   `json:"product_sheet,omitempty"`
	Brand           string   `json:"brand,omitempty"`
	ReferenceImages []string `json:"reference_images,omitempty"`
	SellingPoints   []string `json:"selling_points,omitempty"`
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

	AspectRatio       string `json:"aspect_ratio,omitempty"`
	Style             string `json:"style"`
	StyleTemplateID   string `json:"style_template_id,omitempty"`
	StyleImage        string `json:"style_image,omitempty"`
	DefaultDuration   *int   `json:"default_duration,omitempty"`
	TargetDuration    *int   `json:"target_duration,omitempty"`
	Brief             string `json:"brief,omitempty"`
	SourceLanguage    string `json:"source_language,omitempty"`
	SpeechRate        *float64 `json:"speech_rate_units_per_second,omitempty"`

	NarrationVoice      string   `json:"narration_voice,omitempty"`
	NarrationSpeed      *float64 `json:"narration_speed,omitempty"`
	VideoGenerateAudio  *bool    `json:"video_generate_audio,omitempty"`

	// 后端覆盖（可选，空 = 继承全局）
	VideoBackend         string            `json:"video_backend,omitempty"`
	VideoProviderI2V     string            `json:"video_provider_i2v,omitempty"`
	VideoProviderR2V     string            `json:"video_provider_r2v,omitempty"`
	ImageProviderT2I     string            `json:"image_provider_t2i,omitempty"`
	ImageProviderI2I     string            `json:"image_provider_i2i,omitempty"`
	DefaultImageBackend  string            `json:"default_image_backend,omitempty"`
	AudioBackend         string            `json:"audio_backend,omitempty"`
	TextBackendSimple    string            `json:"text_backend_simple,omitempty"`
	TextBackendComplex   string            `json:"text_backend_complex,omitempty"`
	DefaultTextBackend   string            `json:"default_text_backend,omitempty"`
	ModelSettings        map[string]map[string]string `json:"model_settings,omitempty"`

	Overview *Overview `json:"overview,omitempty"`

	CharacterRelations []map[string]any `json:"character_relations,omitempty"`

	Metadata ProjectMetadata `json:"metadata,omitempty"`

	Episodes []EpisodeMeta `json:"episodes,omitempty"`

	Characters map[string]*Character `json:"characters,omitempty"`
	Scenes     map[string]*Scene     `json:"scenes,omitempty"`
	Props      map[string]*Prop      `json:"props,omitempty"`
	Products   map[string]*Product   `json:"products,omitempty"`
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
		Characters:     map[string]*Character{},
		Scenes:         map[string]*Scene{},
		Props:          map[string]*Prop{},
		Products:       map[string]*Product{},
	}
}

// IsReferenceVideo 判断是否走参考路线。
func (p *ProjectFile) IsReferenceVideo() bool {
	return p.GenerationMode == string(GenerationReferenceVideo)
}

// AssetBucket 返回指定资产类型的 map；未知类型返回 nil。
func (p *ProjectFile) AssetBucket(kind AssetKind) any {
	switch kind {
	case AssetCharacter:
		return p.Characters
	case AssetScene:
		return p.Scenes
	case AssetProp:
		return p.Props
	case AssetProduct:
		return p.Products
	}
	return nil
}

// Validate 校验 project.json 的结构约束（对应 lib/data_validator 的核心部分）。
func (p *ProjectFile) Validate() error {
	if p.Title == "" {
		return fmt.Errorf("title is required")
	}
	if !IsValidContentMode(p.ContentMode) {
		return fmt.Errorf("content_mode must be one of narration/drama/ad, got %q", p.ContentMode)
	}
	if !IsValidGenerationMode(p.GenerationMode) {
		return fmt.Errorf("generation_mode must be storyboard or reference_video, got %q", p.GenerationMode)
	}
	if p.SourceKind != "" && p.SourceKind != "novel" && p.SourceKind != "screenplay" {
		return fmt.Errorf("source_kind must be novel or screenplay, got %q", p.SourceKind)
	}
	if strings.TrimSpace(p.Style) == "" {
		return fmt.Errorf("style is required")
	}
	if p.ContentMode == string(ContentAd) {
		// ad 项目：时长必填、禁止 default_duration、禁止宫格、单集
		if p.TargetDuration == nil || *p.TargetDuration <= 0 {
			return fmt.Errorf("target_duration must be a positive integer for ad projects")
		}
		if p.DefaultDuration != nil {
			return fmt.Errorf("default_duration is not allowed for ad projects")
		}
		if p.GridStoryboard {
			return fmt.Errorf("grid_storyboard is not allowed for ad projects")
		}
		if len(p.Episodes) != 1 || p.Episodes[0].Episode != 1 {
			return fmt.Errorf("ad projects must have exactly one episode with episode=1")
		}
	} else if p.TargetDuration != nil {
		return fmt.Errorf("target_duration is only allowed for ad projects")
	}
	for _, ep := range p.Episodes {
		if ep.ScriptFile == "" {
			return fmt.Errorf("episode %d: script_file is required", ep.Episode)
		}
	}
	for name, c := range p.Characters {
		if c == nil || strings.TrimSpace(c.Description) == "" {
			return fmt.Errorf("character %q: description is required", name)
		}
	}
	for name, s := range p.Scenes {
		if s == nil || strings.TrimSpace(s.Description) == "" {
			return fmt.Errorf("scene %q: description is required", name)
		}
	}
	for name, pr := range p.Props {
		if pr == nil || strings.TrimSpace(pr.Description) == "" {
			return fmt.Errorf("prop %q: description is required", name)
		}
	}
	for name, pd := range p.Products {
		if pd == nil || strings.TrimSpace(pd.Description) == "" {
			return fmt.Errorf("product %q: description is required", name)
		}
	}
	return nil
}
