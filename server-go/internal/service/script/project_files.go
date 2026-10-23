// 项目文件管理（对应 server/routers/files.py 的轻量端点组）。
//
// 本文件覆盖：静态文件服务（项目内 / 全局资产）、文件列表、source 文本文件读写删、
// 草稿（drafts/step*）读写删、角色参考音频删除。
// **刻意不含** `POST /upload/{type}`（需 SourceLoader 移植：docx/epub/pdf → UTF-8 归一）
// 与 `POST /style-image`（需 Gemini 风格分析）——这两条是上传管线的重头，随
// source_loader 迁移单独落地。
//
// 安全不变式：所有落盘路径先过 SafeJoin；静态服务跳过 symlink 且带 nosniff
// （字节来自用户上传，缺 nosniff 会构成存储型 XSS 向量）。
package script

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/fs"
)

// ProjectFileBuckets 是 Web UI 可见的文件桶（与 Python PROJECT_FILE_BUCKETS 一致）。
// source/raw 是上传原始字节的备份目录，刻意不出现在公开清单里。
var ProjectFileBuckets = []string{
	"source", "characters", "scenes", "props", "products", "storyboards", "videos", "output",
}

// GlobalLibraryAssetTypes 是全局资产库可服务的类型。
var GlobalLibraryAssetTypes = []string{"character", "scene", "prop"}

// FileService 承载项目文件的读取与轻量写入。
type FileService struct{ store *service.ProjectStore }

// NewFileService 构造。
func NewFileService(store *service.ProjectStore) *FileService {
	return &FileService{store: store}
}

// ResolveProjectFile 解析项目内静态文件路径（越界 → 403 语义）。
//
// 返回 (绝对路径, ok, err)：ok=false 表示越界（调用方回 403）；文件不存在同样在
// isNotExist 里区分（调用方回 404）。
func (s *FileService) ResolveProjectFile(projectName, relPath string) (string, bool, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return "", false, err
	}
	if !s.store.Exists(projectName) {
		return "", false, os.ErrNotExist
	}
	resolved, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(strings.Trim(relPath, "/")))
	if err != nil {
		return "", false, nil
	}
	return resolved, true, nil
}

// ResolveGlobalAsset 解析全局资产路径；类型不在白名单或文件名带路径分隔一律拒绝。
func (s *FileService) ResolveGlobalAsset(assetType, filename string) (string, bool, error) {
	allowed := false
	for _, candidate := range GlobalLibraryAssetTypes {
		if candidate == assetType {
			allowed = true
			break
		}
	}
	if !allowed || strings.ContainsAny(filename, "/\\") || strings.Contains(filename, "..") {
		return "", false, nil
	}
	root := s.store.Root()
	resolved, err := fs.SafeJoin(root, false, filepath.Join("_global_assets", assetType, filename))
	if err != nil {
		return "", false, nil
	}
	return resolved, true, nil
}

// FileEntry 是文件清单里的一项。
type FileEntry struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	RawFilename string `json:"raw_filename,omitempty"`
}

// ListProjectFiles 列出各桶可见文件（source/raw 刻意隐藏）。
func (s *FileService) ListProjectFiles(projectName string) (map[string][]FileEntry, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return nil, err
	}
	out := map[string][]FileEntry{}
	for _, bucket := range ProjectFileBuckets {
		entries := []FileEntry{}
		bucketDir := filepath.Join(projectDir, bucket)
		if items, err := os.ReadDir(bucketDir); err == nil {
			for _, item := range items {
				if item.IsDir() {
					// source/raw 是原始字节备份，不进公开清单。
					continue
				}
				if item.Type()&os.ModeSymlink != 0 {
					continue
				}
				info, statErr := item.Info()
				if statErr != nil {
					continue
				}
				entry := FileEntry{
					Name: item.Name(),
					Size: info.Size(),
					URL:  "/api/v1/files/" + projectName + "/" + bucket + "/" + item.Name(),
				}
				if bucket == "source" {
					entry.RawFilename = rawBackupOf(projectDir, strings.TrimSuffix(item.Name(), filepath.Ext(item.Name())))
				}
				entries = append(entries, entry)
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		out[bucket] = entries
	}
	return out, nil
}

// rawBackupOf 找 source/raw 下同 stem 的备份文件名。
func rawBackupOf(projectDir, stem string) string {
	rawDir := filepath.Join(projectDir, "source", "raw")
	items, err := os.ReadDir(rawDir)
	if err != nil {
		return ""
	}
	for _, item := range items {
		if item.IsDir() {
			continue
		}
		if strings.TrimSuffix(item.Name(), filepath.Ext(item.Name())) == stem {
			return item.Name()
		}
	}
	return ""
}

// ReadSourceFile 读 source 文本文件；越界回 403 语义、缺失回 404 语义。
func (s *FileService) ReadSourceFile(projectName, filename string) (string, error) {
	path, err := s.sourcePath(projectName, filename)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// WriteSourceFile 写 source 文本文件（新建或覆盖）。
func (s *FileService) WriteSourceFile(projectName, filename, content string) (string, error) {
	path, err := s.sourcePath(projectName, filename)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return "source/" + filename, nil
}

// DeleteSourceFile 删除 source 文件并级联删除同 stem 的原始字节备份。
func (s *FileService) DeleteSourceFile(projectName, filename string) error {
	path, err := s.sourcePath(projectName, filename)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return os.ErrNotExist
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	rawDir := filepath.Join(filepath.Dir(path), "raw")
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if items, err := os.ReadDir(rawDir); err == nil {
		for _, item := range items {
			if item.IsDir() {
				continue
			}
			if strings.TrimSuffix(item.Name(), filepath.Ext(item.Name())) == stem {
				_ = os.Remove(filepath.Join(rawDir, item.Name()))
			}
		}
	}
	return nil
}

// sourcePath 解析 source 子目录内的路径。
func (s *FileService) sourcePath(projectName, filename string) (string, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return "", err
	}
	resolved, err := fs.SafeJoin(projectDir, false, filepath.Join("source", filename))
	if err != nil {
		return "", &service.ValueError{Msg: "forbidden path"}
	}
	return resolved, nil
}

// ---- 草稿（drafts/step*）----

// DraftStepFile 是该模式在指定步骤应使用的目标文件名（ad 无 step1 → ok=false）。
func DraftStepFile(contentMode, generationMode string, stepNum int) (string, bool) {
	if contentMode == string(project.ContentAd) {
		return "", false
	}
	if stepNum != 1 {
		return "", false
	}
	if generationMode == "reference_video" {
		return "step1_reference_units.json", true
	}
	name, ok := Step1Filename(contentMode)
	if !ok {
		name, _ = Step1Filename("drama")
	}
	return name, true
}

// draftStep1Candidates 按 primary 所属模式优先回落，再兜底其他模式遗留文件。
func draftStep1Candidates(primary string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, mode := range []string{"reference_video", "narration", "drama"} {
		for _, candidate := range Step1ReadCandidates(mode) {
			if candidate == "step1_reference_units.json" {
				out = append(out, "step1_reference_units.md")
			}
			if !seen[candidate] {
				seen[candidate] = true
				out = append(out, candidate)
			}
		}
	}
	if !seen[primary] {
		out = append([]string{primary}, out...)
	}
	return out
}

// resolveDraftPath 主路径不存在时按候选回落（仅读取 / 删除侧；写入恒落主路径）。
func resolveDraftPath(draftsDir string, stepNum int, primary string) string {
	if stepNum != 1 {
		return filepath.Join(draftsDir, primary)
	}
	if _, err := os.Stat(filepath.Join(draftsDir, primary)); err == nil {
		return filepath.Join(draftsDir, primary)
	}
	for _, candidate := range draftStep1Candidates(primary) {
		if candidate == primary {
			continue
		}
		alt := filepath.Join(draftsDir, candidate)
		if _, err := os.Stat(alt); err == nil {
			return alt
		}
	}
	return filepath.Join(draftsDir, primary)
}

// ReadDraft 读草稿文本。
func (s *FileService) ReadDraft(projectName string, episode, stepNum int) (string, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return "", err
	}
	pf, err := s.store.Get(projectName)
	if err != nil {
		return "", err
	}
	primary, ok := DraftStepFile(pf.ContentMode, pf.GenerationMode, stepNum)
	if !ok {
		return "", &service.ValueError{Msg: fmt.Sprintf("invalid step_num: %d", stepNum)}
	}
	draftPath := resolveDraftPath(service.EpisodeDraftsDir(projectDir, episode), stepNum, primary)
	content, err := os.ReadFile(draftPath)
	if err != nil {
		return "", os.ErrNotExist
	}
	return string(content), nil
}

// WriteDraft 写草稿。
//
//   - 参考生视频 step1 走 ScriptReviewService 的单一写盘出口（结构校验 + references 重派生），
//     裸文本直写会成为绕开写盘语义的旁路；
//   - drama step1（结构化 .json）写入前校验 JSON 形状（顶层对象 + 非空 scenes + 每项带
//     非空 scene_id），避免保存成功但生成必失败；
//   - 其他草稿按普通文本落盘。
//
// 返回 (项目内相对路径, 是否新建)。
func (s *FileService) WriteDraft(
	ctx context.Context, projectName string, episode, stepNum int, content string, review Step1DraftSaver,
) (string, bool, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return "", false, err
	}
	pf, err := s.store.Get(projectName)
	if err != nil {
		return "", false, err
	}
	primary, ok := DraftStepFile(pf.ContentMode, pf.GenerationMode, stepNum)
	if !ok {
		return "", false, &service.ValueError{Msg: fmt.Sprintf("invalid step_num: %d", stepNum)}
	}
	// 写入恒落当前模式的目标文件：fallback 只用于读取/删除，否则切模式后生成侧读不到 step1。
	draftPath := filepath.Join(service.EpisodeDraftsDir(projectDir, episode), primary)

	if primary == "step1_reference_units.json" {
		if review == nil {
			return "", false, &service.ValueError{Msg: "script review service is not configured"}
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(content), &parsed); err != nil {
			return "", false, &service.ValueError{Msg: "invalid json: " + err.Error()}
		}
		_, isNew := readDraftMeta(draftPath)
		if _, err := review.SaveContent(ctx, projectName, episode, parsed, nil); err != nil {
			return "", false, err
		}
		return draftRelPath(projectDir, draftPath), isNew, nil
	}

	dramaStep1, _ := Step1Filename("drama")
	if primary == dramaStep1 {
		if err := validateDramaDraft(content); err != nil {
			return "", false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(draftPath), 0o755); err != nil {
		return "", false, err
	}
	isNew := false
	if _, statErr := os.Stat(draftPath); statErr != nil {
		isNew = true
	}
	if err := os.WriteFile(draftPath, []byte(content), 0o644); err != nil {
		return "", false, err
	}
	return draftRelPath(projectDir, draftPath), isNew, nil
}

// DeleteDraft 删除草稿（读取侧允许回落到遗留文件名）。
func (s *FileService) DeleteDraft(projectName string, episode, stepNum int) error {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return err
	}
	pf, err := s.store.Get(projectName)
	if err != nil {
		return err
	}
	primary, ok := DraftStepFile(pf.ContentMode, pf.GenerationMode, stepNum)
	if !ok {
		return &service.ValueError{Msg: fmt.Sprintf("invalid step_num: %d", stepNum)}
	}
	draftPath := resolveDraftPath(service.EpisodeDraftsDir(projectDir, episode), stepNum, primary)
	if _, statErr := os.Stat(draftPath); statErr != nil {
		return os.ErrNotExist
	}
	return os.Remove(draftPath)
}

// validateDramaDraft 校验 drama step1 的 JSON 形状（与读取契约同口径）。
func validateDramaDraft(content string) error {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return &service.ValueError{Msg: "draft_invalid_json"}
	}
	rawScenes, present := parsed["scenes"]
	if !present {
		return &service.ValueError{Msg: "draft_invalid_json"}
	}
	scenes, ok := rawScenes.([]any)
	if !ok || len(scenes) == 0 {
		return &service.ValueError{Msg: "draft_invalid_json"}
	}
	for _, raw := range scenes {
		scene, ok := raw.(map[string]any)
		if !ok {
			return &service.ValueError{Msg: "draft_invalid_json"}
		}
		id, _ := scene["scene_id"].(string)
		if strings.TrimSpace(id) == "" {
			return &service.ValueError{Msg: "draft_invalid_json"}
		}
	}
	return nil
}

// ---- 角色参考音频 ----

// DeleteCharacterReferenceAudio 删除角色的参考音频：先删文件、后清字段。
//
// 顺序不可反：unlink 失败（权限/占用）时字段仍指向文件、可重试；反过来会在物理删除
// 失败时留下「字段已清空但文件仍在」的孤儿，且没有指针可供重试发现它。
// 字段值可被 PATCH 写成任意项目内字符串：只有确认落在 characters/refs_audio 内才碰文件。
func (s *FileService) DeleteCharacterReferenceAudio(projectName, charName string) error {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return err
	}
	pf, err := s.store.Get(projectName)
	if err != nil {
		return err
	}
	bucket, ok := pf.BucketRef(project.AssetCharacter)
	if !ok || bucket == nil {
		return &service.MediaAssetNotFoundError{MediaAssetID: charName}
	}
	key, ok := bucket.ResolveKey(charName)
	if !ok || (*bucket)[key] == nil {
		return &service.MediaAssetNotFoundError{MediaAssetID: charName}
	}
	oldAudio := (*bucket)[key].MustString("reference_audio")
	refsAudioDir := filepath.Join(projectDir, "characters", "refs_audio")
	if oldAudio != "" {
		if stale := resolveAudioRefPathStrict(projectDir, refsAudioDir, oldAudio); stale != "" {
			_ = os.Remove(stale)
		}
	}
	return s.store.Update(projectName, func(update *project.ProjectFile) error {
		updateBucket, ok := update.BucketRef(project.AssetCharacter)
		if !ok || updateBucket == nil {
			return service.ErrResourceMissingInScript
		}
		updateKey, ok := updateBucket.ResolveKey(charName)
		if !ok || (*updateBucket)[updateKey] == nil {
			return service.ErrResourceMissingInScript
		}
		(*updateBucket)[updateKey].SetString("reference_audio", "")
		return nil
	})
}

// resolveAudioRefPathStrict 是 resolveAudioRefPath（reference_render.go）的加强版：
// 单靠 SafeResolve 只保证不越界项目目录，还须父目录命中 refs_audio，防止字段被
// PATCH 成项目内其它路径后被诱导删除（如 project.json）。
func resolveAudioRefPathStrict(projectDir, refsAudioDir, relPath string) string {
	if strings.TrimSpace(relPath) == "" {
		return ""
	}
	resolved, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(relPath))
	if err != nil {
		return ""
	}
	if filepath.Clean(filepath.Dir(resolved)) != filepath.Clean(refsAudioDir) {
		return ""
	}
	return resolved
}

// ---- 内部 ----

// readDraftMeta 读草稿当前是否存在（新建判定用）。
func readDraftMeta(path string) (struct{}, bool) {
	if _, err := os.Stat(path); err == nil {
		return struct{}{}, true
	}
	return struct{}{}, false
}

// draftRelPath 计算草稿的项目内相对路径。
func draftRelPath(projectDir, draftPath string) string {
	rel, err := filepath.Rel(projectDir, draftPath)
	if err != nil {
		return filepath.ToSlash(draftPath)
	}
	return filepath.ToSlash(rel)
}

// Step1DraftSaver 是参考生视频 step1 草稿写盘的窄接口（*ScriptReviewService 满足），
// 使 FileService 不依赖 script 域实现。
type Step1DraftSaver interface {
	SaveContent(ctx context.Context, projectName string, episode int, content any, base *string) (map[string]any, error)
}
