package service

// 不可变 AssemblyPlanRevision 的剪映兼容草稿包导出（对应
// server/services/jianying_assembly_export.py）。
//
// 导出物是一个自包含 ZIP：manifest.json + timeline.json + subtitles.srt/vtt +
// media/ 下的源文件拷贝。拷贝前用 source snapshot 里登记的 sha256 复核每个
// 源文件，防止「快照之后源文件被改」的静默不一致。

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/repository"
	"gorm.io/gorm"
)

// JianyingExportError 表示 revision 无法被安全导出；Status 对应 HTTP 状态码。
type JianyingExportError struct {
	Code    string
	Message string
	Status  int
}

func (e *JianyingExportError) Error() string { return e.Message }

// JianyingExportBundle 是临时 ZIP 与其属主目录；响应写出完成后须删除 TempDir。
type JianyingExportBundle struct {
	Path           string
	Filename       string
	RevisionNumber int64
	TempDir        string
}

// JianyingExportService 承载剪映草稿导出。
type JianyingExportService struct {
	repo  *repository.AssemblyPlanRepo
	store *ProjectStore
}

// NewJianyingExportService 构造服务；store 用于解析项目根目录。
func NewJianyingExportService(gdb *gorm.DB, store *ProjectStore) *JianyingExportService {
	return &JianyingExportService{repo: repository.NewAssemblyPlanRepo(gdb), store: store}
}

// ExportCurrentRevision 导出计划当前 revision 的剪映草稿包（对应 export_current_revision）。
func (s *JianyingExportService) ExportCurrentRevision(
	ctx context.Context, planID, userID string,
) (*JianyingExportBundle, error) {
	plan, err := s.repo.GetOwned(ctx, planID, userID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, &AssemblyPlanNotFoundError{PlanID: planID}
	}
	revision, err := s.repo.GetCurrentRevision(ctx, plan)
	if err != nil {
		return nil, err
	}
	if revision == nil {
		return nil, &AssemblyPlanConflictError{Code: "current_revision_missing", Status: plan.Status}
	}
	projectRoot, err := s.store.Dir(plan.ProjectName)
	if err != nil {
		return nil, err
	}
	return s.buildExport(plan, revision, projectRoot)
}

// jianyingLoadSection 解析 revision 的一个 JSON section；解析失败 → revision_json_invalid。
func jianyingLoadSection(raw, section string) (any, error) {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, &JianyingExportError{
			Code: "revision_json_invalid", Message: "revision " + section + " is invalid",
			Status: 422,
		}
	}
	return value, nil
}

// jianyingResolveMedia 在项目根内解析媒体相对路径（对应 _resolve_media）：
// 非字符串/空白 → source_ref_invalid；越界、不存在、不是文件 → source_file_missing。
func jianyingResolveMedia(projectRoot string, sourceRef any) (string, error) {
	ref, ok := sourceRef.(string)
	if !ok || strings.TrimSpace(ref) == "" {
		return "", &JianyingExportError{
			Code: "source_ref_invalid", Message: "media source_ref must be a non-empty string", Status: 422,
		}
	}
	resolved, err := jianyingSafeResolve(projectRoot, ref)
	if err != nil {
		return "", &JianyingExportError{
			Code: "source_file_missing", Message: "media source is unavailable: " + ref, Status: 409,
		}
	}
	return resolved, nil
}

// jianyingExtension 返回小写后缀；空、超长或含非字母数字字符时退化为 .bin。
func jianyingExtension(path string) string {
	suffix := strings.ToLower(filepath.Ext(path))
	if len(suffix) < 2 || len(suffix) > 12 {
		return ".bin"
	}
	for _, r := range suffix[1:] {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return ".bin"
		}
	}
	return suffix
}

// jianyingTruthy 复刻 Python bool()：非空字符串/非零数值/非空容器为真。
func jianyingTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

// snapshotExpectedSHA 从 source snapshot 里取登记的源文件 sha256（对应 _snapshot_expected_sha）。
func snapshotExpectedSHA(snapshot any, sourceRef string) string {
	mapping, ok := snapshot.(map[string]any)
	if !ok {
		return ""
	}
	items, ok := mapping["items"].([]any)
	if !ok {
		return ""
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		source, ok := item["source"].(map[string]any)
		fp, fpOK := item["file_fingerprint"].(map[string]any)
		if ok && source["path"] == sourceRef && fpOK {
			sha, ok := fp["sha256"].(string)
			if ok && sha != "" {
				return sha
			}
			return ""
		}
	}
	return ""
}

// jianyingCopyAsset 指纹复核通过后把源文件拷进草稿目录（对应 _copy_asset）。
func jianyingCopyAsset(
	source, destination, relativePath, sourceRef string, snapshot any,
) (map[string]any, error) {
	expectedSHA := snapshotExpectedSHA(snapshot, sourceRef)
	actualSHA, err := fingerprint(source)
	if err != nil {
		return nil, err
	}
	if expectedSHA != "" && actualSHA != expectedSHA {
		return nil, &JianyingExportError{
			Code: "source_fingerprint_conflict", Message: "media source has changed: " + sourceRef, Status: 409,
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return nil, err
	}
	if err := copyFileBytes(source, destination); err != nil {
		return nil, err
	}
	st, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path":        relativePath,
		"size_bytes":  st.Size(),
		"fingerprint": actualSHA,
	}, nil
}

// jianyingJSONBytes 输出缩进 2、键有序、不转义非 ASCII、以换行收尾的 JSON
// （对应 _json_bytes：indent=2 + sort_keys + ensure_ascii=False + "\n"）。
func jianyingJSONBytes(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildExport 组装草稿目录并打包为 ZIP（对应 _build_export）。
func (s *JianyingExportService) buildExport(
	plan *repository.AssemblyPlan, revision *repository.AssemblyPlanRevision, projectRoot string,
) (*JianyingExportBundle, error) {
	if plan.CurrentRevisionNumber != revision.VersionNumber {
		return nil, &JianyingExportError{
			Code: "revision_conflict", Message: "plan does not point to the requested revision", Status: 409,
		}
	}
	if plan.CurrentSourceFingerprint != revision.SourceFingerprint {
		return nil, &JianyingExportError{
			Code: "source_fingerprint_conflict", Message: "revision source fingerprint is stale", Status: 409,
		}
	}

	timelineRaw, err := jianyingLoadSection(revision.TimelineJSON, "timeline")
	if err != nil {
		return nil, err
	}
	audioRaw, err := jianyingLoadSection(revision.AudioJSON, "audio")
	if err != nil {
		return nil, err
	}
	subtitleRaw, err := jianyingLoadSection(revision.SubtitleJSON, "subtitle")
	if err != nil {
		return nil, err
	}
	packagingRaw, err := jianyingLoadSection(revision.PackagingJSON, "packaging")
	if err != nil {
		return nil, err
	}
	snapshot, err := jianyingLoadSection(revision.SourceSnapshotJSON, "source_snapshot")
	if err != nil {
		return nil, err
	}
	timeline, timelineOK := timelineRaw.([]any)
	audio, audioOK := audioRaw.(map[string]any)
	subtitle, subtitleOK := subtitleRaw.(map[string]any)
	packaging, packagingOK := packagingRaw.(map[string]any)
	if !timelineOK || !audioOK || !subtitleOK || !packagingOK {
		return nil, &JianyingExportError{
			Code: "revision_shape_invalid", Message: "revision sections have invalid shapes", Status: 422,
		}
	}

	tempDir, err := os.MkdirTemp("", "shotwise-jianying-")
	if err != nil {
		return nil, err
	}
	// 对应 Python 的 except Exception: rmtree + raise：组装失败时回收临时目录。
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(tempDir)
		}
	}()

	timelineExport := make([]map[string]any, 0, len(timeline))
	tracksRaw, _ := audio["tracks"].([]any)
	audioTracks := make([]map[string]any, 0, len(tracksRaw))
	for _, rawTrack := range tracksRaw {
		if track, ok := rawTrack.(map[string]any); ok {
			audioTracks = append(audioTracks, track)
		}
	}
	audio["tracks"] = audioTracks
	assets := make([]map[string]any, 0, len(timeline)+len(audioTracks)+1)

	for index, rawItem := range timeline {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, &JianyingExportError{
				Code: "timeline_invalid", Message: fmt.Sprintf("timeline item %d is invalid", index+1), Status: 422,
			}
		}
		sourceRef := item["source_ref"]
		source, err := jianyingResolveMedia(projectRoot, sourceRef)
		if err != nil {
			return nil, err
		}
		ref, _ := sourceRef.(string)
		relative := fmt.Sprintf("media/%03d-clip%s", index+1, jianyingExtension(source))
		asset, err := jianyingCopyAsset(
			source, filepath.Join(tempDir, filepath.FromSlash(relative)), relative, ref, snapshot,
		)
		if err != nil {
			return nil, err
		}
		asset["role"] = "video"
		asset["source_ref"] = ref
		asset["timeline_index"] = index
		assets = append(assets, asset)
		item["source_ref"] = relative
		item["media_path"] = relative
		timelineExport = append(timelineExport, item)
	}

	for index, track := range audioTracks {
		sourceRef := track["source_ref"]
		source, err := jianyingResolveMedia(projectRoot, sourceRef)
		if err != nil {
			return nil, err
		}
		ref, _ := sourceRef.(string)
		relative := fmt.Sprintf("media/audio-%03d%s", index+1, jianyingExtension(source))
		asset, err := jianyingCopyAsset(
			source, filepath.Join(tempDir, filepath.FromSlash(relative)), relative, ref, snapshot,
		)
		if err != nil {
			return nil, err
		}
		asset["role"] = "audio"
		asset["source_ref"] = ref
		asset["audio_index"] = index
		assets = append(assets, asset)
		track["source_ref"] = relative
	}

	if cover, ok := packaging["cover"].(map[string]any); ok && jianyingTruthy(cover["source_ref"]) {
		sourceRef := cover["source_ref"]
		source, err := jianyingResolveMedia(projectRoot, sourceRef)
		if err != nil {
			return nil, err
		}
		ref, _ := sourceRef.(string)
		relative := "media/cover" + jianyingExtension(source)
		asset, err := jianyingCopyAsset(
			source, filepath.Join(tempDir, filepath.FromSlash(relative)), relative, ref, snapshot,
		)
		if err != nil {
			return nil, err
		}
		asset["role"] = "cover"
		asset["source_ref"] = ref
		assets = append(assets, asset)
		coverCopy := make(map[string]any, len(cover)+1)
		for k, v := range cover {
			coverCopy[k] = v
		}
		coverCopy["source_ref"] = relative
		packaging["cover"] = coverCopy
	}

	cuesRaw, present := subtitle["cues"]
	if !present {
		cuesRaw = []any{}
	}
	cues, ok := cuesRaw.([]any)
	if !ok {
		return nil, &JianyingExportError{
			Code: "subtitle_invalid", Message: "subtitle cues must be a list", Status: 422,
		}
	}
	srt, err := domain.GenerateSRT(cues)
	if err != nil {
		return nil, &JianyingExportError{Code: "subtitle_invalid", Message: err.Error(), Status: 422}
	}
	vtt, err := domain.GenerateVTT(cues)
	if err != nil {
		return nil, &JianyingExportError{Code: "subtitle_invalid", Message: err.Error(), Status: 422}
	}

	manifest := map[string]any{
		"schema_version":     "shotwise-jianying-draft/v1",
		"plan_id":            plan.ID,
		"revision_id":        revision.ID,
		"revision_number":    revision.VersionNumber,
		"source_fingerprint": revision.SourceFingerprint,
		"assets":             assets,
		"timeline":           timelineExport,
		"audio":              audio,
		"subtitle":           map[string]any{"cue_count": len(cues), "srt": "subtitles.srt", "vtt": "subtitles.vtt"},
		"packaging":          packaging,
	}
	if err := jianyingWriteJSONFile(filepath.Join(tempDir, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	if err := jianyingWriteJSONFile(filepath.Join(tempDir, "timeline.json"), timelineExport); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tempDir, "subtitles.srt"), []byte(srt), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tempDir, "subtitles.vtt"), []byte(vtt), 0o644); err != nil {
		return nil, err
	}
	readme := "Shotwise Jianying draft package\n" +
		fmt.Sprintf("Plan: %s\nRevision: %d\n", plan.ID, revision.VersionNumber) +
		"This package uses relative media paths and is intended as an editable interchange package.\n"
	if err := os.WriteFile(filepath.Join(tempDir, "README.txt"), []byte(readme), 0o644); err != nil {
		return nil, err
	}

	filename := fmt.Sprintf("shotwise-plan-%s-r%d.zip", plan.ID, revision.VersionNumber)
	zipPath := filepath.Join(tempDir, filename)
	if err := jianyingZipDir(tempDir, zipPath); err != nil {
		return nil, err
	}
	success = true
	return &JianyingExportBundle{
		Path:           zipPath,
		Filename:       filename,
		RevisionNumber: revision.VersionNumber,
		TempDir:        tempDir,
	}, nil
}

// jianyingWriteJSONFile 按导出规范写一个 JSON 文件。
func jianyingWriteJSONFile(path string, value any) error {
	data, err := jianyingJSONBytes(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// jianyingZipDir 把 tempDir 下除 zipPath 外的所有文件按字典序打包（对应
// sorted(rglob) + ZIP_DEFLATED）。
func jianyingZipDir(tempDir, zipPath string) error {
	var files []string
	err := filepath.WalkDir(tempDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)

	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()
	archive := zip.NewWriter(out)
	for _, file := range files {
		if file == zipPath {
			continue
		}
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(tempDir, file)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Deflate
		entry, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		src, err := os.Open(file)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(entry, src)
		src.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return out.Close()
}

// copyFileBytes 等价 shutil.copyfile：整文件覆盖拷贝。
func copyFileBytes(source, destination string) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(destination)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, src)
	if closeErr := dst.Close(); copyErr == nil {
		copyErr = closeErr
	}
	return copyErr
}

// jianyingSafeResolve 要求 base 内的 rel 是已存在的文件；越界、不存在或
// 是目录统一报错（对应 safe_join(require_file=True) 的拒绝面）。
func jianyingSafeResolve(base, rel string) (string, error) {
	return fs.SafeResolve(base, rel)
}
