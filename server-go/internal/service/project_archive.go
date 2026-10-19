// 项目 ZIP 归档导入与导入前校验。对应 server/services/project_archive.py 的核心导入语义。
//
// 与 Python 的收敛差异（刻意为之，均已在响应语义中保持兼容）：
//   - Python 在 staging 副本上跑完整迁移链（legacy provider 名归一、image_backend 拆分、
//     源文编码迁移）；Go 侧 store 只写当前 schema，导入时做**最小 schema 校验**：
//     project.json 必须可解析、content_mode / generation_mode 必须是合法枚举，
//     否则 blocking 拒绝。导出早于 schema v2 的旧归档需先用 Python 服务升级。
//   - project memory（memory/project_memories.json）按用户口径暂不参与（memory 服务未迁），
//     归档里若带该成员则跳过，不报错。
//   - agent profile sync 不适用（Go 无 CLAUDE.*.md 档案同步）。
package service

import (
	"archive/zip"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/shotwise/shotwise/internal/domain"
)

// 归档常量（与 Python 一致）。
const (
	ArchiveManifestName       = "shotwise-export.json"
	archiveFormatVersion      = 2
	archiveScriptSchemaVerion = 2
	defaultImportFilename     = "imported-project.zip"
	// maxArchiveMembers / maxArchiveTotalBytes 是 zip 炸弹防线（Python 靠逐成员流式
	// 解压天然限流，Go 显式声明上限）。
	maxArchiveMembers     = 50_000
	maxArchiveTotalBytes  = 4 << 30 // 4GB
	maxArchiveMemberBytes = 1 << 30 // 单成员 1GB
)

// ProjectSubdirs 是标准项目子目录（导入后补齐缺失）。
var ProjectSubdirs = []string{
	"source", "scripts", "drafts", "characters", "scenes", "props", "products",
	"storyboards", "videos", "thumbnails", "output", "grids",
}

// projectNamePattern 是项目标识模式（仅英文字母数字中划线）。
var projectNamePattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// ArchiveValidationError 是导入校验失败（detail + errors/warnings + 额外负载）。
type ArchiveValidationError struct {
	Status   int
	Detail   string           // 翻译 key
	Errors   []ArchiveMessage // [{code, params}]
	Warnings []ArchiveMessage
	Extra    map[string]any // 如 conflict_project_name
}

// ArchiveMessage 是一条可渲染的诊断消息。
type ArchiveMessage struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

func (e *ArchiveValidationError) Error() string { return "archive invalid: " + e.Detail }

// literal 构造一条字面消息（不走翻译表）。
func archiveLiteral(text string) ArchiveMessage { return ArchiveMessage{Code: "\x00literal:" + text} }

// ImportDiagnostics 是导入的诊断集合。
type ImportDiagnostics struct {
	Errors   []ArchiveMessage `json:"errors"`
	Warnings []ArchiveMessage `json:"warnings"`
}

// ProjectImportResult 是导入成功结果。
type ProjectImportResult struct {
	ProjectName        string              `json:"project_name"`
	Project            *domain.ProjectFile `json:"project"`
	Warnings           []ArchiveMessage    `json:"warnings"`
	ConflictResolution string              `json:"conflict_resolution"`
	Diagnostics        ImportDiagnostics   `json:"diagnostics"`
}

// ArchivePreflightResult 是导入前校验结果。
type ArchivePreflightResult struct {
	ProjectName         string            `json:"project_name"`
	ProjectTitle        string            `json:"project_title"`
	ConflictProjectName *string           `json:"conflict_project_name"`
	Diagnostics         ImportDiagnostics `json:"diagnostics"`
}

// ProjectArchiveService 承载导入管线。
type ProjectArchiveService struct {
	store *ProjectStore
}

// NewProjectArchiveService 构造服务。
func NewProjectArchiveService(store *ProjectStore) *ProjectArchiveService {
	return &ProjectArchiveService{store: store}
}

// archiveMember 是扫描后的归档成员。
type archiveMember struct {
	info  *zip.File
	parts []string
	isDir bool
}

// scanArchiveMembers 做成员级安全扫描：加密、绝对路径、盘符、.. 穿越、symlink 全拒。
func scanArchiveMembers(archive *zip.Reader) ([]archiveMember, error) {
	if len(archive.File) > maxArchiveMembers {
		return nil, &ArchiveValidationError{
			Status: httpStatusArchive, Detail: "arch_import_validation_failed",
			Errors: []ArchiveMessage{archiveLiteral("too many entries")},
		}
	}
	members := make([]archiveMember, 0, len(archive.File))
	var total int64
	for _, info := range archive.File {
		if info.Flags&0x1 != 0 {
			return nil, validationFailed("arch_zip_encrypted_entry", map[string]any{"name": info.Name})
		}
		normalized := strings.ReplaceAll(info.Name, "\\", "/")
		if strings.HasPrefix(normalized, "/") {
			return nil, validationFailed("arch_zip_absolute_path_entry", map[string]any{"name": info.Name})
		}
		stripped := strings.Trim(normalized, "/")
		if stripped == "" {
			continue
		}
		parts := splitPathParts(stripped)
		if len(parts[0]) == 2 && parts[0][1] == ':' {
			return nil, validationFailed("arch_zip_absolute_path_entry", map[string]any{"name": info.Name})
		}
		for _, part := range parts {
			if part == ".." {
				return nil, validationFailed("arch_zip_traversal_entry", map[string]any{"name": info.Name})
			}
		}
		mode := info.Mode()
		if mode&os.ModeSymlink != 0 {
			return nil, validationFailed("arch_zip_symlink_entry", map[string]any{"name": info.Name})
		}
		total += int64(info.UncompressedSize64)
		if total > maxArchiveTotalBytes || int64(info.UncompressedSize64) > maxArchiveMemberBytes {
			return nil, validationFailedLiteral("archive entry exceeds size limit")
		}
		members = append(members, archiveMember{
			info:  info,
			parts: parts,
			isDir: info.FileInfo().IsDir() || strings.HasSuffix(normalized, "/"),
		})
	}
	return members, nil
}

// isHiddenMember 是隐藏成员判定（点开头 / __MACOSX）。
func isHiddenMember(parts []string) bool {
	for _, part := range parts {
		if strings.HasPrefix(part, ".") || part == "__MACOSX" {
			return true
		}
	}
	return false
}

// locateProjectRoot 定位归档内项目根：优先 manifest，其次唯一 project.json。
func (s *ProjectArchiveService) locateProjectRoot(
	archive *zip.Reader, members []archiveMember,
) ([]string, map[string]any, error) {
	var visible []archiveMember
	for _, member := range members {
		if !isHiddenMember(member.parts) {
			visible = append(visible, member)
		}
	}
	var manifestMembers []archiveMember
	for _, member := range visible {
		if member.parts[len(member.parts)-1] == ArchiveManifestName {
			manifestMembers = append(manifestMembers, member)
		}
	}
	if len(manifestMembers) > 0 {
		candidates := map[string]bool{}
		var root []string
		for _, member := range manifestMembers {
			key := strings.Join(member.parts[:len(member.parts)-1], "/")
			if !candidates[key] {
				candidates[key] = true
				root = member.parts[:len(member.parts)-1]
			}
		}
		if len(candidates) != 1 {
			return nil, nil, validationFailedLiteral("multiple manifests")
		}
		hasProjectJSON := false
		for _, member := range visible {
			if joinParts(member.parts) == joinParts(root)+"/project.json" ||
				equalPartsSuffix(member.parts, root, "project.json") {
				hasProjectJSON = true
				break
			}
		}
		if !hasProjectJSON {
			return nil, nil, validationFailedLiteral("manifest missing project.json")
		}
		manifest, err := loadMemberJSON(manifestMembers[0])
		if err != nil {
			return nil, nil, err
		}
		if err := validateArchiveManifest(manifest); err != nil {
			return nil, nil, err
		}
		return root, manifest, nil
	}
	rootCandidates := map[string]bool{}
	var singleRoot []string
	for _, member := range visible {
		if member.parts[len(member.parts)-1] == "project.json" {
			key := strings.Join(member.parts[:len(member.parts)-1], "/")
			if !rootCandidates[key] {
				rootCandidates[key] = true
				singleRoot = member.parts[:len(member.parts)-1]
			}
		}
	}
	if len(rootCandidates) == 0 {
		return nil, nil, validationFailedLiteral("no project.json in archive")
	}
	if len(rootCandidates) != 1 {
		return nil, nil, validationFailedLiteral("multiple project.json in archive")
	}
	return singleRoot, nil, nil
}

// extractArchiveRoot 解根目录到 staging，跳过隐藏成员 / manifest / memory 成员。
func extractArchiveRoot(
	archive *zip.Reader, members []archiveMember, root []string, stagingDir string,
) error {
	for _, member := range members {
		if len(member.parts) < len(root) || !equalPrefix(member.parts, root) {
			continue
		}
		relative := member.parts[len(root):]
		if len(relative) == 0 {
			continue
		}
		if relative[len(relative)-1] == ArchiveManifestName ||
			(len(relative) == 2 && relative[0] == "memory" && relative[1] == "project_memories.json") {
			continue
		}
		if isHiddenMember(relative) {
			continue
		}
		target := filepath.Join(append([]string{stagingDir}, relative...)...)
		// staging 内部路径已由扫描保证无穿越；这里再防御一次。
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(stagingDir)+string(os.PathSeparator)) {
			return validationFailedLiteral("extract path traversal")
		}
		if member.isDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		reader, err := member.info.Open()
		if err != nil {
			return err
		}
		writer, err := os.Create(target)
		if err != nil {
			_ = reader.Close()
			return err
		}
		_, copyErr := io.Copy(writer, reader)
		closeErr := writer.Close()
		_ = reader.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// validateImportedProject 是最小 schema 校验（收敛自 Python 完整 validator + 迁移链）。
func validateImportedProject(project map[string]any) error {
	contentMode, _ := project["content_mode"].(string)
	switch domain.ContentMode(contentMode) {
	case domain.ContentNarration, domain.ContentDrama, domain.ContentAd:
	default:
		return validationFailedLiteral(
			"content_mode must be one of narration/drama/ad (legacy archives need migration first)")
	}
	generationMode, _ := project["generation_mode"].(string)
	switch generationMode {
	case "storyboard", "reference_video":
	default:
		return validationFailedLiteral(
			"generation_mode must be storyboard or reference_video (legacy archives need migration first)")
	}
	return nil
}

// resolveTargetProjectName 按 manifest → 根目录名 → 标题 slug → 上传文件名的顺序定名。
func (s *ProjectArchiveService) resolveTargetProjectName(
	project map[string]any, manifest map[string]any, root []string, uploadedFilename string,
) (string, error) {
	if manifest != nil {
		if name := normalizeArchiveProjectName(manifest["project_name"]); name != "" {
			return name, nil
		}
	}
	if len(root) > 0 {
		if name := normalizeArchiveProjectName(root[len(root)-1]); name != "" {
			return name, nil
		}
	}
	title, _ := project["title"].(string)
	if strings.TrimSpace(title) != "" {
		return s.generateProjectName(title), nil
	}
	stem := uploadedFilename
	if stem == "" {
		stem = defaultImportFilename
	}
	stem = strings.TrimSuffix(filepath.Base(strings.ReplaceAll(stem, "\\", "/")), filepath.Ext(stem))
	return s.generateProjectName(stem), nil
}

// resolveConflict 按三策略解析目标名冲突；prompt 策略冲突时带 409 错误。
func (s *ProjectArchiveService) resolveConflict(preferred string, conflictPolicy string) (string, string, error) {
	if conflictPolicy != "prompt" && conflictPolicy != "rename" && conflictPolicy != "overwrite" {
		return "", "", &ArchiveValidationError{
			Status: httpStatusArchive, Detail: "arch_invalid_conflict_policy",
			Errors: []ArchiveMessage{{Code: "arch_conflict_policy_unsupported", Params: map[string]any{"value": conflictPolicy}}},
		}
	}
	target := filepath.Join(s.store.Root(), preferred)
	exists := dirExists(target)
	switch conflictPolicy {
	case "prompt":
		if exists {
			return "", "", &ArchiveValidationError{
				Status: 409, Detail: "arch_conflict_detected",
				Errors: []ArchiveMessage{{Code: "arch_project_name_conflict", Params: map[string]any{"name": preferred}}},
				Extra:  map[string]any{"conflict_project_name": preferred},
			}
		}
		return preferred, "none", nil
	case "rename":
		if exists {
			return s.generateProjectName(preferred), "renamed", nil
		}
		return preferred, "none", nil
	default: // overwrite
		if exists {
			return preferred, "overwritten", nil
		}
		return preferred, "none", nil
	}
}

// ImportArchive 是导入主流程：扫描 → 定根 → 解压 staging → 校验 → 冲突解析 → 安装。
func (s *ProjectArchiveService) ImportArchive(
	archivePath, uploadedFilename, conflictPolicy string,
) (*ProjectImportResult, error) {
	diagnostics, _, targetName, stagingDir, err := s.prepareImport(archivePath, uploadedFilename)
	defer cleanupStaging(stagingDir)
	if err != nil {
		return nil, err
	}
	targetName, resolution, err := s.resolveConflict(targetName, conflictPolicy)
	if err != nil {
		return nil, err
	}
	// staging 根下才是项目本体（prepareImport 解压到 <tmp>/project）。
	if err := s.installProjectDir(filepath.Join(stagingDir, "project"), targetName, conflictPolicy == "overwrite"); err != nil {
		return nil, err
	}
	installed, err := s.store.Get(targetName)
	if err != nil {
		return nil, fmt.Errorf("installed project unreadable: %w", err)
	}
	return &ProjectImportResult{
		ProjectName:        targetName,
		Project:            installed,
		Warnings:           diagnostics.Warnings,
		ConflictResolution: resolution,
		Diagnostics:        *diagnostics,
	}, nil
}

// PreflightArchive 走同一管线到校验为止，不安装。
func (s *ProjectArchiveService) PreflightArchive(
	archivePath, uploadedFilename string,
) (*ArchivePreflightResult, error) {
	diagnostics, project, targetName, stagingDir, err := s.prepareImport(archivePath, uploadedFilename)
	defer cleanupStaging(stagingDir)
	if err != nil {
		return nil, err
	}
	title, _ := project["title"].(string)
	conflictName := targetName
	if !dirExists(filepath.Join(s.store.Root(), targetName)) {
		conflictName = ""
	}
	var conflictPtr *string
	if conflictName != "" {
		conflictPtr = &conflictName
	}
	return &ArchivePreflightResult{
		ProjectName:         targetName,
		ProjectTitle:        title,
		ConflictProjectName: conflictPtr,
		Diagnostics:         *diagnostics,
	}, nil
}

// prepareImport 是两条入口共享的前半段：解压 → 校验 → 定名。
// 返回 staging 目录（调用方负责清理，即使出错——错误路径下 staging 可能为空串）。
func (s *ProjectArchiveService) prepareImport(
	archivePath, uploadedFilename string,
) (*ImportDiagnostics, map[string]any, string, string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, "", "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, "", "", err
	}
	archive, err := zip.NewReader(file, info.Size())
	if err != nil {
		return nil, nil, "", "", &ArchiveValidationError{
			Status: httpStatusArchive, Detail: "arch_not_a_zip",
			Errors: []ArchiveMessage{archiveLiteral(err.Error())},
		}
	}
	members, err := scanArchiveMembers(archive)
	if err != nil {
		return nil, nil, "", "", err
	}
	root, manifest, err := s.locateProjectRoot(archive, members)
	if err != nil {
		return nil, nil, "", "", err
	}
	stagingDir, err := os.MkdirTemp("", "shotwise-import-")
	if err != nil {
		return nil, nil, "", "", err
	}
	staging := filepath.Join(stagingDir, "project")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, nil, "", stagingDir, err
	}
	if err := extractArchiveRoot(archive, members, root, staging); err != nil {
		return nil, nil, "", stagingDir, err
	}

	diagnostics := &ImportDiagnostics{Errors: []ArchiveMessage{}, Warnings: []ArchiveMessage{}}
	projectBytes, err := os.ReadFile(filepath.Join(staging, "project.json"))
	if err != nil {
		return nil, nil, "", stagingDir, &ArchiveValidationError{
			Status: httpStatusArchive, Detail: "arch_import_validation_failed",
			Errors: []ArchiveMessage{{Code: "arch_invalid_project_json"}},
		}
	}
	var project map[string]any
	if err := json.Unmarshal(projectBytes, &project); err != nil {
		return nil, nil, "", stagingDir, &ArchiveValidationError{
			Status: httpStatusArchive, Detail: "arch_import_validation_failed",
			Errors: []ArchiveMessage{{Code: "arch_invalid_project_json"}},
		}
	}
	if err := validateImportedProject(project); err != nil {
		return nil, nil, "", stagingDir, err
	}
	targetName, err := s.resolveTargetProjectName(project, manifest, root, uploadedFilename)
	if err != nil {
		return nil, nil, "", stagingDir, err
	}
	return diagnostics, project, targetName, stagingDir, nil
}

// installProjectDir 安装 staging 到目标目录；overwrite 先备份旧目录，失败回滚。
func (s *ProjectArchiveService) installProjectDir(stagingDir, projectName string, overwrite bool) error {
	target := filepath.Join(s.store.Root(), projectName)
	backup := ""
	if overwrite && dirExists(target) {
		backup = filepath.Join(s.store.Root(),
			fmt.Sprintf(".import-backup-%s-%d", projectName, time.Now().UnixNano()))
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	}
	moveErr := func() error {
		// os.Rename 跨盘会失败：先试 rename，失败退化为拷贝（staging 在临时目录，
		// 与数据根可能不同盘）。
		if err := os.Rename(stagingDir, target); err == nil {
			return nil
		}
		return copyTree(stagingDir, target)
	}()
	if moveErr != nil {
		// 回滚：删半安装的 target，恢复备份。
		_ = os.RemoveAll(target)
		if backup != "" {
			_ = os.Rename(backup, target)
		}
		return moveErr
	}
	// 标准子目录补齐（Python _ensure_standard_subdirs）。
	for _, subdir := range ProjectSubdirs {
		_ = os.MkdirAll(filepath.Join(target, subdir), 0o755)
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

// generateProjectName 生成唯一项目标识（标题 slug + 8 位十六进制）。
func (s *ProjectArchiveService) generateProjectName(title string) string {
	prefix := slugifyProjectTitle(title)
	for attempt := 0; attempt < 64; attempt++ {
		candidate := fmt.Sprintf("%s-%s", prefix, randomHex(4))
		if !dirExists(filepath.Join(s.store.Root(), candidate)) {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// slugifyProjectTitle 标题转 slug（NFKD → ASCII → 非字母数字折叠为中划线 → 截 24）。
// 纯数字 / 空结果退化为中性前缀 proj（与 Python 同语义）。
func slugifyProjectTitle(title string) string {
	var sb strings.Builder
	for _, r := range norm.NFKD.String(strings.TrimSpace(title)) {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			sb.WriteRune(unicode.ToLower(r))
		} else if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r < 128 {
			sb.WriteByte('-')
		}
	}
	slug := strings.Trim(sb.String(), "-_")
	if len(slug) > 24 {
		slug = slug[:24]
	}
	slug = strings.Trim(slug, "-")
	hasLetter := false
	for _, r := range slug {
		if unicode.IsLetter(r) {
			hasLetter = true
			break
		}
	}
	if !hasLetter {
		return "proj"
	}
	return slug
}

// normalizeArchiveProjectName 归一项目名；非法返回空串（Python 返回 None）。
func normalizeArchiveProjectName(value any) string {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if text == "" || !projectNamePattern.MatchString(text) {
		return ""
	}
	return text
}

// validateArchiveManifest 校验 manifest 版本（向后兼容无 script_schema_version 的 v1）。
func validateArchiveManifest(manifest map[string]any) error {
	formatVersion := manifestVersion(manifest, "format_version", -1)
	if formatVersion > archiveFormatVersion {
		return validationFailed("arch_manifest_future_version", map[string]any{"field": "format_version"})
	}
	scriptDefault := -1
	if formatVersion == 1 {
		scriptDefault = 1
	}
	scriptVersion := manifestVersion(manifest, "script_schema_version", scriptDefault)
	if scriptVersion > archiveScriptSchemaVerion {
		return validationFailed("arch_manifest_future_version", map[string]any{"field": "script_schema_version"})
	}
	return nil
}

// manifestVersion 读 manifest 里的版本整数。
func manifestVersion(manifest map[string]any, key string, fallback int) int {
	raw, ok := manifest[key].(float64)
	if !ok {
		return fallback
	}
	return int(raw)
}

// --- 小工具 ---

const httpStatusArchive = 400

func validationFailed(code string, params map[string]any) error {
	return &ArchiveValidationError{
		Status: httpStatusArchive, Detail: "arch_import_validation_failed",
		Errors: []ArchiveMessage{{Code: code, Params: params}},
	}
}

func validationFailedLiteral(text string) error {
	return &ArchiveValidationError{
		Status: httpStatusArchive, Detail: "arch_import_validation_failed",
		Errors: []ArchiveMessage{archiveLiteral(text)},
	}
}

// loadMemberJSON 读一个 JSON 成员。
func loadMemberJSON(member archiveMember) (map[string]any, error) {
	reader, err := member.info.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	var payload map[string]any
	if err := json.NewDecoder(reader).Decode(&payload); err != nil {
		return nil, validationFailedLiteral("member json invalid: " + member.info.Name)
	}
	return payload, nil
}

// splitPathParts 按斜杠切分并去空段。
func splitPathParts(name string) []string {
	var parts []string
	for _, part := range strings.Split(name, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

// joinParts 用斜杠拼回。
func joinParts(parts []string) string { return strings.Join(parts, "/") }

// equalPrefix 判断 parts 是否以 prefix 开头。
func equalPrefix(parts, prefix []string) bool {
	if len(parts) < len(prefix) {
		return false
	}
	for i := range prefix {
		if parts[i] != prefix[i] {
			return false
		}
	}
	return true
}

// equalPartsSuffix 判断 parts 是否为 prefix + tail 的拼接。
func equalPartsSuffix(parts []string, prefix []string, tail string) bool {
	return len(parts) == len(prefix)+1 && equalPrefix(parts, prefix) && parts[len(parts)-1] == tail
}

// dirExists 判断目录是否存在。
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// cleanupStaging 清理 staging 临时根目录（空串安全）。
func cleanupStaging(stagingDir string) {
	if stagingDir != "" {
		_ = os.RemoveAll(stagingDir)
	}
}

// copyTree 递归拷贝目录（跨盘安装的退化路径）。
func copyTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(source, path)
		if relErr != nil {
			return relErr
		}
		destination := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if mkdirErr := os.MkdirAll(filepath.Dir(destination), 0o755); mkdirErr != nil {
			return mkdirErr
		}
		return os.WriteFile(destination, content, 0o644)
	})
}

// randomHex 生成 n 字节的十六进制串（crypto/rand；随机源故障用纳秒时间戳兜底）。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := crand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buf)
}
