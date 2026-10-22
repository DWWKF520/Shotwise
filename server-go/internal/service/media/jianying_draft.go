// 剪映草稿导出服务。对应 server/services/jianying_draft_service.py（621 行）。
//
// 草稿装配基于 vendor 的 go-capcut（MIT，internal/capcut；pyJianYingDraft 的 Go 移植）。
// 与 Python 的差异：
//   - 视频时长用 ffmpeg.DurationSeconds 探测（pyJianYingDraft 的 VideoMaterial 内部也是
//     ffprobe，口径一致）；
//   - draft_meta_info.json 从 pyJianYingDraft 0.3.1 的模板原样嵌入（Python 侧同样只复制
//     模板不补字段，剪映打开时自补）；
//   - 闪黑/叠化转场元数据已按 pyJianYingDraft 同名字段登记进 vendor 库。
package media

import (
	"archive/zip"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/capcut/material"
	"github.com/shotwise/shotwise/internal/capcut/metadata"
	"github.com/shotwise/shotwise/internal/capcut/script"
	capcutSegment "github.com/shotwise/shotwise/internal/capcut/segment"
	"github.com/shotwise/shotwise/internal/capcut/track"
	"github.com/shotwise/shotwise/internal/capcut/types"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// 剪映草稿导出的域错误。
var (
	// ErrNoCompletedSegments 表示本集没有已完成视频片段（422）。
	ErrNoCompletedSegments = errors2("jianying_no_completed_segments")
	// ErrJianyingExportFailed 表示暂存/写入阶段的失败（500，细节进日志）。
	ErrJianyingExportFailed = errors2("jianying_export_failed")
)

func errors2(key string) error { return &JianyingError{Key: key} }

// JianyingError 是带 i18n key 的剪映导出错误。
type JianyingError struct{ Key string }

func (e *JianyingError) Error() string { return e.Key }

// subtitleSpan 是一条字幕在片段内的定位（秒）。
type subtitleSpan struct {
	OffsetSeconds   float64
	DurationSeconds float64
	Text            string
}

// jianyingClip 是收集出的一个待导出片段。
type jianyingClip struct {
	ID              string
	DurationSeconds int
	VideoClip       string // 剧本内相对路径
	AbsPath         string
	SubtitleText    string
	SubtitleSpans   []subtitleSpan
	TransitionTo    string
	NarrationAudio  string // 绝对路径；空 = 无
}

// JianyingDraftService 承载剪映草稿导出。
type JianyingDraftService struct {
	store *service.ProjectStore
}

// NewJianyingDraftService 构造服务。
func NewJianyingDraftService(store *service.ProjectStore) *JianyingDraftService {
	return &JianyingDraftService{store: store}
}

// ExportEpisodeDraft 导出指定集的剪映草稿 ZIP，返回 ZIP 的临时文件路径（调用方发送后清理）。
//
// 流程对齐 Python：定位剧本 → 收集片段 → 定画布 → 暂存素材（同源去重）→ 生成草稿 →
// 素材入草稿 assets/ → 路径改写（staging 前缀 → 用户本地草稿目录前缀）→
// draft_info.json（剪映 6+）→ 打 ZIP（视频 STORED 其余 DEFLATED）。
func (s *JianyingDraftService) ExportEpisodeDraft(
	ctx context.Context, projectName string, episode int, draftPath string, useDraftInfoName bool,
) (string, string, error) {
	pf, err := s.store.Get(projectName)
	if err != nil {
		return "", "", ErrNoCompletedSegments
	}
	// 1. 定位剧本
	scriptData, scriptErr := s.findEpisodeScript(projectName, pf, episode)
	if scriptErr != nil {
		return "", "", scriptErr
	}
	// 2. 收集片段
	contentMode := jianyingScriptContentMode(scriptData)
	clips, err := s.collectVideoClips(projectName, scriptData, pf)
	if err != nil {
		return "", "", err
	}
	if len(clips) == 0 {
		return "", "", ErrNoCompletedSegments
	}
	// 3. 画布尺寸
	width, height := resolveCanvasSize(pf, clips[0].AbsPath)

	// 4. 草稿名与临时目录
	title := strings.TrimSpace(pf.Title)
	if title == "" {
		title = projectName
	}
	safeTitle := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(title)
	draftName := safeTitle
	if contentMode != "ad" {
		draftName = fmt.Sprintf("%s_第%d集", safeTitle, episode)
	}
	if strings.ReplaceAll(strings.TrimSpace(draftName), ".", "") == "" {
		draftName = projectName
	}
	tmpDir, err := os.MkdirTemp("", "shotwise_jy_")
	if err != nil {
		return "", "", err
	}
	stagingDir := filepath.Join(tmpDir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", err
	}
	defer func() {
		// 成功时 zip 在 tmpDir 里由调用方读走后再清理失败路径；统一在发送端清理。
		_ = tmpDir
	}()

	// 暂存素材：同一来源文件只暂存一次。
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", err
	}
	stagedBySrc := map[string]string{}
	stageOnce := func(src string) (string, error) {
		if staged, ok := stagedBySrc[src]; ok {
			return staged, nil
		}
		// 暂存前重校验：收集与暂存之间文件可能被换成越界 symlink。
		resolved, joinErr := fs.SafeJoin(projectDir, true, src)
		if joinErr != nil {
			return "", &JianyingError{Key: "path_traversal"}
		}
		if _, statErr := os.Stat(resolved); statErr != nil {
			return "", &JianyingError{Key: "path_traversal"}
		}
		staged, copyErr := stageJianyingFile(resolved, stagingDir)
		if copyErr != nil {
			return "", copyErr
		}
		stagedBySrc[src] = staged
		return staged, nil
	}
	localClips := make([]jianyingLocalClip, 0, len(clips))
	for _, clip := range clips {
		localPath, stageErr := stageOnce(clip.AbsPath)
		if stageErr != nil {
			_ = os.RemoveAll(tmpDir)
			return "", "", stageErr
		}
		item := jianyingLocalClip{jianyingClip: clip, LocalPath: localPath}
		if clip.NarrationAudio != "" {
			narr, narrErr := stageOnce(clip.NarrationAudio)
			if narrErr != nil {
				_ = os.RemoveAll(tmpDir)
				return "", "", narrErr
			}
			item.NarrationLocal = narr
		}
		localClips = append(localClips, item)
	}

	// 5. 生成草稿（草稿放独立父目录，避免与暂存区同名被替换逻辑误删）。
	draftDir := filepath.Join(tmpDir, "draft", draftName)
	if err := generateJianyingDraft(draftDir, draftName, localClips, width, height, contentMode); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", err
	}

	// 6. 素材移入草稿 assets/。
	assetsDir := filepath.Join(draftDir, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", err
	}
	entries, _ := os.ReadDir(stagingDir)
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(stagingDir, entry.Name()), filepath.Join(assetsDir, entry.Name())); err != nil {
			_ = os.RemoveAll(tmpDir)
			return "", "", err
		}
	}

	// 7. 路径改写：staging 前缀 → 用户本地草稿目录。
	contentPath := filepath.Join(draftDir, "draft_content.json")
	if err := replaceJianyingPaths(contentPath, stagingDir, draftPath+"/"+draftName+"/assets"); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", err
	}

	// 8. 剪映 6+ 用 draft_info.json。
	if useDraftInfoName {
		if err := os.Rename(contentPath, filepath.Join(draftDir, "draft_info.json")); err != nil {
			_ = os.RemoveAll(tmpDir)
			return "", "", err
		}
	}

	// 9. 打 ZIP：视频 STORED（已压缩格式避免二次压缩耗时），其余 DEFLATED。
	zipPath := filepath.Join(tmpDir, draftName+".zip")
	if err := writeJianyingZip(zipPath, draftDir, draftName); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", err
	}
	return zipPath, draftName, nil
}

// findEpisodeScript 定位指定集剧本。
func (s *JianyingDraftService) findEpisodeScript(projectName string, pf *domain.ProjectFile, episode int) (map[string]any, error) {
	for _, meta := range pf.Episodes {
		if meta.Episode != episode {
			continue
		}
		dir, err := s.store.Dir(projectName)
		if err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(filepath.Join(dir, meta.ScriptFile))
		if err != nil {
			return nil, err
		}
		var scriptData map[string]any
		if err := json.Unmarshal(raw, &scriptData); err != nil {
			return nil, err
		}
		return scriptData, nil
	}
	return nil, os.ErrNotExist
}

// scriptContentMode 读取剧本 content_mode（脏值归一空串）。
func jianyingScriptContentMode(scriptData map[string]any) string {
	value, _ := scriptData["content_mode"].(string)
	return value
}

// collectVideoClips 从剧本收集已完成视频片段（storyboard 与 reference_video 两路）。
func (s *JianyingDraftService) collectVideoClips(
	projectName string, scriptData map[string]any, pf *domain.ProjectFile,
) ([]jianyingClip, error) {
	generationMode := pf.GenerationMode
	if generationMode == "reference_video" {
		kind, err := domain.ResolveDeclaredKind(jianyingScriptContentMode(scriptData), generationMode)
		if err != nil {
			return nil, err
		}
		return collectReferenceUnitClips(scriptData, s, projectName, kind)
	}
	kind, err := domain.ResolveDeclaredKind(jianyingScriptContentMode(scriptData), "")
	if err != nil {
		return nil, err
	}
	items, _ := scriptData[kind].([]any)
	idField := domain.ScriptSkeletons[kind].IDField
	subtitleField := map[string]string{"narration": "novel_text", "ad": "voiceover_text"}[jianyingScriptContentMode(scriptData)]
	isDrama := jianyingScriptContentMode(scriptData) == "drama"
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	clips := []jianyingClip{}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		videoClip := generatedAssetString(item, "video_clip")
		if videoClip == "" {
			continue
		}
		absPath, resolveErr := fs.SafeResolve(projectDir, videoClip)
		if resolveErr != nil {
			continue // 越界或不存在：跳过
		}
		subtitleText := ""
		if subtitleField != "" {
			if value, ok := item[subtitleField].(string); ok {
				subtitleText = value
			}
		}
		clip := jianyingClip{
			ID:              jyStringField(item, idField),
			DurationSeconds: jyIntField(item, "duration_seconds", 8),
			VideoClip:       videoClip,
			AbsPath:         absPath,
			SubtitleText:    subtitleText,
			TransitionTo:    service.StringField(item, "transition_to_next"),
			NarrationAudio:  generatedAssetString(item, "narration_audio"),
		}
		if clip.TransitionTo == "" {
			clip.TransitionTo = "cut"
		}
		if clip.NarrationAudio != "" {
			if narr, narrErr := fs.SafeResolve(projectDir, clip.NarrationAudio); narrErr == nil {
				clip.NarrationAudio = narr
			} else {
				clip.NarrationAudio = ""
			}
		}
		if isDrama {
			clip.SubtitleSpans = utteranceSubtitleSpans(item["utterances"], pf.SourceLanguage,
				service.ProjectSpeechRateOverride(pf))
		}
		clips = append(clips, clip)
	}
	return clips, nil
}

// collectReferenceUnitClips 参考直出的 unit 级片段收集。
func collectReferenceUnitClips(
	scriptData map[string]any, s *JianyingDraftService, projectName, kind string,
) ([]jianyingClip, error) {
	var unitsKey string
	shotsByID := map[string]map[string]any{}
	switch kind {
	case "shots":
		unitsKey = "reference_units"
		shotsByID = domain.AdShotsByID(scriptData)
	case "video_units":
		unitsKey = "video_units"
	default:
		return nil, fmt.Errorf("参考生视频不支持的骨架种类: %s", kind)
	}
	units, _ := scriptData[unitsKey].([]any)
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	clips := []jianyingClip{}
	for _, raw := range units {
		unit, _ := raw.(map[string]any)
		if unit == nil {
			continue
		}
		videoClip := generatedAssetString(unit, "video_clip")
		if videoClip == "" {
			continue
		}
		absPath, resolveErr := fs.SafeResolve(projectDir, videoClip)
		if resolveErr != nil {
			continue
		}
		clip := jianyingClip{
			ID:              jyStringField(unit, "unit_id"),
			DurationSeconds: jyIntField(unit, "duration_seconds", 0),
			VideoClip:       videoClip,
			AbsPath:         absPath,
			TransitionTo:    service.StringField(unit, "transition_to_next"),
		}
		if clip.TransitionTo == "" {
			clip.TransitionTo = "cut"
		}
		if kind == "shots" {
			// ad 字幕按成员镜头口播在 unit 内逐镜头对齐；转场取末位成员镜头的值。
			offset := 0
			transition := "cut"
			shotIDs, _ := unit["shot_ids"].([]any)
			for _, shotID := range shotIDs {
				id, _ := shotID.(string)
				shot := shotsByID[id]
				if shot == nil {
					continue
				}
				duration := domain.AdShotDurationSeconds(shot)
				text, _ := shot["voiceover_text"].(string)
				if text != "" && duration > 0 {
					clip.SubtitleSpans = append(clip.SubtitleSpans, subtitleSpan{
						OffsetSeconds: float64(offset), DurationSeconds: float64(duration), Text: text,
					})
				}
				offset += max(duration, 0)
				if value, ok := shot["transition_to_next"].(string); ok && value != "" {
					transition = value
				}
			}
			clip.DurationSeconds = offset
			clip.TransitionTo = transition
		}
		clips = append(clips, clip)
	}
	return clips, nil
}

// utteranceSubtitleSpans 从 drama 场景的有序 utterances 派生字幕 span（按语速估时长、顺次摆放）。
func utteranceSubtitleSpans(utterances any, language string, override *float64) []subtitleSpan {
	spans := []subtitleSpan{}
	offset := 0.0
	list, _ := utterances.([]any)
	for _, raw := range list {
		utterance, _ := raw.(map[string]any)
		if utterance == nil {
			continue
		}
		text, _ := utterance["text"].(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		duration := service.EstimateSpokenSeconds(text, language, override)
		if duration <= 0 {
			continue
		}
		spans = append(spans, subtitleSpan{OffsetSeconds: offset, DurationSeconds: duration, Text: text})
		offset += duration
	}
	return spans
}

// resolveCanvasSize 画布尺寸：项目 aspect_ratio 优先，未设时从首个视频探测（探测失败回退竖屏）。
func resolveCanvasSize(pf *domain.ProjectFile, firstVideo string) (int, int) {
	switch pf.AspectRatio {
	case "16:9":
		return 1920, 1080
	case "9:16":
		return 1080, 1920
	case "1:1":
		return 1080, 1080
	}
	if width, height, ok := ffmpeg.ProbeSize(context.Background(), firstVideo); ok && width > 0 {
		return width, height
	}
	return 1080, 1920
}

// jianyingLocalClip 是暂存后的片段（local 路径已解析）。
type jianyingLocalClip struct {
	jianyingClip
	LocalPath      string
	NarrationLocal string
}

// generateJianyingDraft 用 capcut 装配草稿目录（draft_content.json + draft_meta_info.json）。
func generateJianyingDraft(
	draftDir, draftName string, clips []jianyingLocalClip, width, height int, contentMode string,
) error {
	if err := os.MkdirAll(draftDir, 0o755); err != nil {
		return err
	}
	sf, err := script.NewScriptFile(width, height)
	if err != nil {
		return err
	}
	videoTrackName := "视频"
	textTrackName := "字幕"
	audioTrackName := "旁白"
	sf.AddTrack(track.TrackTypeVideo, &videoTrackName)
	isPortrait := height > width
	hasSubtitle := contentMode == "narration" || contentMode == "ad" || contentMode == "drama"
	if hasSubtitle {
		sf.AddTrack(track.TrackTypeText, &textTrackName)
	}
	videoTrack, err := sf.GetTrack("", &videoTrackName)
	if err != nil {
		return err
	}
	var textTrack *track.Track
	if hasSubtitle {
		textTrack, _ = sf.GetTrack("", &textTrackName)
	}
	var audioTrack *track.Track
	needsAudio := false
	for _, clip := range clips {
		if clip.NarrationLocal != "" {
			needsAudio = true
			break
		}
	}
	if needsAudio {
		sf.AddTrack(track.TrackTypeAudio, &audioTrackName)
		audioTrack, _ = sf.GetTrack("", &audioTrackName)
	}

	offsetUs := int64(0)
	lastIndex := len(clips) - 1
	var narrationPlacements []struct {
		startUs int64
		path    string
	}
	for index, clip := range clips {
		durationSec, ok := ffmpeg.DurationSeconds(context.Background(), clip.LocalPath)
		if !ok || durationSec <= 0 {
			return &JianyingError{Key: "jianying_export_failed"}
		}
		durationUs := int64(durationSec * 1_000_000)
		videoMaterial, err := material.NewVideoMaterial(
			material.MaterialTypeVideo, jyStrPtr(clip.LocalPath), nil, nil, nil,
			nil, jyFltPtr(durationSec), jyIntPtr(width), jyIntPtr(height))
		if err != nil {
			return err
		}
		sf.AddMaterial(videoMaterial)
		segmentVideo := capcutSegment.NewVideoSegment(
			videoMaterial.MaterialID,
			types.NewTimerange(0, durationUs),
			types.NewTimerange(offsetUs, durationUs),
			1.0, 1.0, nil)
		// 转场挂前一段；最后一段不挂；cut 不挂。
		if index < lastIndex && clip.TransitionTo != "cut" {
			if transition, ok := jianyingTransition(clip.TransitionTo); ok {
				segmentVideo.Transition = transition
			}
		}
		if err := videoTrack.AddSegment(segmentVideo); err != nil {
			return err
		}
		// 字幕：span 派生模式逐条对齐（越界 span 跳过、夹到片段末尾）；单字段模式整段一条。
		if hasSubtitle && textTrack != nil {
			spans := clip.SubtitleSpans
			if len(spans) == 0 && clip.SubtitleText != "" {
				spans = []subtitleSpan{{Text: clip.SubtitleText}}
			}
			for _, span := range spans {
				spanStart := offsetUs + int64(span.OffsetSeconds*1_000_000)
				spanDuration := int64(span.DurationSeconds * 1_000_000)
				clipEnd := offsetUs + durationUs
				if spanStart >= clipEnd || span.Text == "" {
					continue
				}
				if spanDuration > clipEnd-spanStart {
					spanDuration = clipEnd - spanStart
				}
				textSegment := capcutSegment.NewTextSegment(
					span.Text, types.NewTimerange(spanStart, spanDuration), "",
					jianyingTextStyle(isPortrait), jianyingSubtitlePosition(isPortrait))
				textSegment.SetBorder(1.0, [3]float64{0, 0, 0}, 30.0)
				textSegment.SetShadow(true, 0.7, -45.0, "#000000", 3.0, 8.0)
				if err := textTrack.AddSegment(textSegment); err != nil {
					return err
				}
			}
		}
		if clip.NarrationLocal != "" {
			narrationPlacements = append(narrationPlacements, struct {
				startUs int64
				path    string
			}{offsetUs, clip.NarrationLocal})
		}
		offsetUs += durationUs
	}
	// 旁白音频：视频排布完成后统一摆放；不可解析的跳过不报错。
	for _, placement := range narrationPlacements {
		seconds, ok := ffmpeg.DurationSeconds(context.Background(), placement.path)
		if !ok || seconds <= 0 {
			continue
		}
		audioMaterial, err := material.NewAudioMaterial(
			jyStrPtr(placement.path), nil, nil, nil, jyFltPtr(seconds))
		if err != nil {
			continue
		}
		sf.AddMaterial(audioMaterial)
		audioSegment := capcutSegment.NewAudioSegment(
			audioMaterial.MaterialID, types.NewTimerange(placement.startUs, int64(seconds*1_000_000)),
			nil, 1.0, 1.0)
		_ = audioTrack.AddSegment(audioSegment)
	}

	// 落盘：draft_content.json + draft_meta_info.json（模板原样，剪映打开时自补）。
	content, err := sf.Dumps()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(draftDir, "draft_content.json"), []byte(content), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(draftDir, "draft_meta_info.json"), []byte(draftMetaInfoTemplate), 0o644)
}

// draftMetaInfoTemplate 是嵌入的剪映 meta 模板（pyJianYingDraft 0.3.1 assets 原件）。
//
//go:embed draft_meta_info_template.json
var draftMetaInfoTemplate string

// jianyingTransition 映射剧本转场语义到 capcut 转场元数据。
func jianyingTransition(transitionTo string) (*capcutSegment.Transition, bool) {
	var chosen metadata.EffectEnumerable
	switch transitionTo {
	case "fade":
		chosen = metadata.TransitionType闪黑
	case "dissolve":
		chosen = metadata.TransitionType叠化
	default:
		return nil, false
	}
	meta, ok := chosen.GetMeta().(metadata.TransitionMeta)
	if !ok {
		return nil, false
	}
	// NewTransition(name, effectID, resourceID, duration us)。
	return capcutSegment.NewTransition(meta.Name, meta.EffectID, meta.ResourceID, meta.DefaultDuration), true
}

// jianyingTextStyle 是字幕样式（与 Python TextStyle/TextBorder/TextShadow 同参）。
func jianyingTextStyle(isPortrait bool) *capcutSegment.TextStyle {
	size := 8.0
	if isPortrait {
		size = 12.0
	}
	return &capcutSegment.TextStyle{Size: size, Color: [3]float64{1, 1, 1}, Align: 1, Bold: true}
}

// jianyingSubtitlePosition 是字幕位置（竖屏/横屏的 transform_y 与 Python 一致）。
func jianyingSubtitlePosition(isPortrait bool) *capcutSegment.ClipSettings {
	y := -0.8
	if isPortrait {
		y = -0.75
	}
	return &capcutSegment.ClipSettings{TransformY: y}
}

// replaceJianyingPaths 把草稿 JSON 里的 staging 前缀替换为用户本地草稿目录前缀。
func replaceJianyingPaths(jsonPath, tmpPrefix, targetPrefix string) error {
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	var walk func(node any) any
	walk = func(node any) any {
		switch value := node.(type) {
		case string:
			if strings.Contains(value, tmpPrefix) {
				return strings.ReplaceAll(value, tmpPrefix, targetPrefix)
			}
			return value
		case map[string]any:
			out := make(map[string]any, len(value))
			for key, item := range value {
				out[key] = walk(item)
			}
			return out
		case []any:
			out := make([]any, len(value))
			for i, item := range value {
				out[i] = walk(item)
			}
			return out
		}
		return node
	}
	replaced, err := json.Marshal(walk(data))
	if err != nil {
		return err
	}
	return os.WriteFile(jsonPath, replaced, 0o644)
}

// videoSuffixCompress 判断 zip 条目是否应压缩（视频 STORED，其余 DEFLATED）。
func videoSuffixCompress(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".webm", ".mov", ".avi", ".mkv":
		return false // STORED
	}
	return true // DEFLATED
}

// writeJianyingZip 打包草稿目录。
func writeJianyingZip(zipPath, draftDir, draftName string) error {
	file, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	writer := zip.NewWriter(file)
	defer func() { _ = writer.Close() }()
	// 视频已是压缩格式走 STORED 避免二次压缩，其余 DEFLATED（按条目选压缩方法）。
	return filepath.WalkDir(draftDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(draftDir, path)
		if relErr != nil {
			return nil
		}
		header := &zip.FileHeader{
			Name:   filepath.ToSlash(filepath.Join(draftName, rel)),
			Method: zip.Deflate,
		}
		if !videoSuffixCompress(header.Name) {
			header.Method = zip.Store
		}
		create, createErr := writer.CreateHeader(header)
		if createErr != nil {
			return createErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		_, writeErr := create.Write(content)
		return writeErr
	})
}

// stageJianyingFile 复制素材进暂存区（保留扩展名，随机文件名防碰撞）。
func stageJianyingFile(source, stagingDir string) (string, error) {
	target := filepath.Join(stagingDir,
		fmt.Sprintf("%s%s", domain.NewVariantID(), strings.ToLower(filepath.Ext(source))))
	content, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	return target, os.WriteFile(target, content, 0o644)
}

// --- 小助手 ---

func jyStrPtr(value string) *string { return &value }
func jyFltPtr(value float64) *float64 { return &value }
func jyIntPtr(value int) *int       { return &value }
func jyStringField(item map[string]any, key string) string {
	value, _ := item[key].(string)
	return value
}
func jyIntField(item map[string]any, key string, fallback int) int {
	if value, ok := item[key].(float64); ok && value == float64(int(value)) {
		return int(value)
	}
	return fallback
}
func generatedAssetString(item map[string]any, key string) string {
	assets, _ := item["generated_assets"].(map[string]any)
	if assets == nil {
		return ""
	}
	value, _ := assets[key].(string)
	return value
}
