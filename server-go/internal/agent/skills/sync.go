package skills

// profile 同步：把 agent_runtime_profile 投影到用户项目的 .claude 与 CLAUDE.md。
//
// 目录契约与 Python lib/profile_manifest.py::resolve_profile_files_for_mode 一致：
//   - 顶层 CLAUDE.md（通用）或 CLAUDE.<mode>.md（变体）→ 项目根 CLAUDE.md
//   - .claude/skills/<name>/SKILL.md（通用）或 SKILL.<mode>.md（变体）→ 同名目录 SKILL.md
//   - .claude 下其余文件（agents/*.md、skill 附带脚本等）原样复制
//
// 项目侧用 `.shotwise_profile_manifest.json`（与 Python 同名同 schema）记录上次写入
// 的内容 sha256，据此区分三种状态：未改的内置文件（随 profile 升级更新）、用户修改
// （保留）、用户删除（记 tombstone，不复活）。manifest 缺失时按内容比对接管：
// 与源一致的补记录，不一致的视为用户修改保留。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

// ProfileDirEnv 覆盖 profile 源目录（与 Python SHOTWISE_PROFILE_DIR 同名）。
const ProfileDirEnv = "SHOTWISE_PROFILE_DIR"

const (
	profileDirName = "agent_runtime_profile"
	claudeTreeName = ".claude"
	// manifestFile 与 Python MANIFEST_FILENAME 同名，两侧可互读。
	manifestFile          = ".shotwise_profile_manifest.json"
	manifestSchemaVersion = 1
)

// ResolveProfileDir 返回 agent profile 源目录。优先级：SHOTWISE_PROFILE_DIR >
// 从工作目录逐层向上查找 agent_runtime_profile。找不到时返回空串（调用方跳过同步）。
func ResolveProfileDir() string {
	if v := strings.TrimSpace(os.Getenv(ProfileDirEnv)); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 4; i++ {
		candidate := filepath.Join(dir, profileDirName)
		if isDir(filepath.Join(candidate, claudeTreeName)) {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// SyncProfile 把 profile 按 contentMode 投影到项目目录，返回本次新建/更新的文件数。
//
// 幂等：内容未变的文件不重写。profileDir 为空或不存在时直接返回 (0, nil)——部署
// 环境未带 profile 时不阻断服务。contentMode 为空按 narration 处理（与 Python
// sync_agent_profile 的缺省回退一致）。
func SyncProfile(profileDir, projectDir, contentMode string) (int, error) {
	if profileDir == "" || !isDir(profileDir) {
		return 0, nil
	}
	if contentMode == "" {
		contentMode = "narration"
	}
	if !IsValidMode(contentMode) {
		return 0, fmt.Errorf("profile: invalid content_mode %q", contentMode)
	}
	files, err := projectProfileFiles(profileDir, contentMode)
	if err != nil {
		return 0, err
	}
	mf := loadManifest(projectDir)
	changed := 0
	for _, rel := range files {
		src := filepath.Join(profileDir, filepath.FromSlash(rel.src))
		dst := filepath.Join(projectDir, filepath.FromSlash(rel.dst))
		if !within(projectDir, dst) {
			return changed, fmt.Errorf("profile: 目标路径越界 %s", rel.dst)
		}
		srcHash, size, err := hashFile(src)
		if err != nil {
			return changed, err
		}
		n, err := applyFile(src, dst, rel.dst, srcHash, size, mf)
		if err != nil {
			return changed, err
		}
		changed += n
	}
	if mf.dirty {
		if err := saveManifest(projectDir, mf); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// manifest 是项目侧 profile 同步记录；字段名与 Python 侧一致。
type manifest struct {
	SchemaVersion int                      `json:"schema_version"`
	Files         map[string]manifestEntry `json:"files"`
	dirty         bool
}

type manifestEntry struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Source 为 "profile"（已同步）或 "tombstone"（用户删除，不再复活）。
	Source string `json:"source"`
}

func loadManifest(projectDir string) *manifest {
	mf := &manifest{SchemaVersion: manifestSchemaVersion, Files: map[string]manifestEntry{}}
	raw, err := os.ReadFile(filepath.Join(projectDir, manifestFile))
	if err != nil {
		return mf
	}
	var loaded manifest
	if json.Unmarshal(raw, &loaded) != nil || loaded.SchemaVersion != manifestSchemaVersion {
		return mf // 版本不符 / 损坏：按无记录处理（已存在文件保留，不覆盖）
	}
	for k, v := range loaded.Files {
		if v.SHA256 != "" && (v.Source == "profile" || v.Source == "tombstone") {
			mf.Files[k] = v
		}
	}
	return mf
}

func saveManifest(projectDir string, mf *manifest) error {
	raw, err := json.MarshalIndent(struct {
		SchemaVersion int                      `json:"schema_version"`
		Files         map[string]manifestEntry `json:"files"`
	}{mf.SchemaVersion, mf.Files}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(projectDir, manifestFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// applyFile 按 manifest 状态决定单个文件的处理，返回 1 表示新建或更新。
//
// 决策表：
//   - 目标缺失 + 记录为 profile → 用户删除，记 tombstone（不复活）
//   - 目标缺失 + 无有效记录 → 复制并记录
//   - 目标存在 + 记录为 profile 且目标哈希 == 记录 → 内置文件：源变了才覆盖
//   - 目标存在 + 记录为 profile 但目标哈希 != 记录 → 用户改过，保留
//   - 目标存在 + 无记录 → 内容与源一致则补记录（接管），否则视为用户修改保留
func applyFile(src, dst, logicalRel, srcHash string, size int64, mf *manifest) (int, error) {
	entry, recorded := mf.Files[logicalRel]
	dstHash, _, dstErr := hashFile(dst)
	dstExists := dstErr == nil

	switch {
	case !dstExists:
		if recorded && entry.Source == "profile" {
			mf.Files[logicalRel] = manifestEntry{SHA256: srcHash, Size: size, Source: "tombstone"}
			mf.dirty = true
			return 0, nil
		}
		if err := copyFile(src, dst); err != nil {
			return 0, err
		}
		mf.Files[logicalRel] = manifestEntry{SHA256: srcHash, Size: size, Source: "profile"}
		mf.dirty = true
		return 1, nil
	case recorded && entry.Source == "profile":
		if dstHash != entry.SHA256 {
			return 0, nil // 用户修改过：保留，不更新记录
		}
		if dstHash == srcHash {
			return 0, nil // 内置文件未变
		}
		if err := copyFile(src, dst); err != nil {
			return 0, err
		}
		mf.Files[logicalRel] = manifestEntry{SHA256: srcHash, Size: size, Source: "profile"}
		mf.dirty = true
		return 1, nil
	case recorded:
		return 0, nil // tombstone：用户删过，保持删除
	case dstHash == srcHash:
		mf.Files[logicalRel] = manifestEntry{SHA256: srcHash, Size: size, Source: "profile"}
		mf.dirty = true
		return 0, nil
	default:
		return 0, nil // 无记录且内容不同：视为用户修改
	}
}

// hashFile 返回文件的 sha256 与大小（64KiB 分块，避免大文件整读入内存）。
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, f, make([]byte, 64*1024))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// copyFile 以原子写覆盖目标（同目录临时文件 + rename）。
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// profileFile 是一条投影映射：profile 内相对路径 → 项目内相对路径（POSIX 斜杠）。
type profileFile struct{ src, dst string }

// projectProfileFiles 按 contentMode 枚举投影文件。变体只保留匹配 mode 的那份，
// 项目侧名字去掉 .<mode> 后缀；顶层只收 CLAUDE 家族（其余顶层文件刻意忽略）。
func projectProfileFiles(profileDir, contentMode string) ([]profileFile, error) {
	entries, err := os.ReadDir(profileDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]string{} // dst → src，用于识别通用/变体并存与多 mode 并存
	var out []profileFile
	add := func(src, dst string) error {
		if prev, ok := seen[dst]; ok {
			return fmt.Errorf("profile: %s 与 %s 投影到同一目标 %s（通用与变体 / 多变体不允许并存）", prev, src, dst)
		}
		seen[dst] = src
		out = append(out, profileFile{src: src, dst: dst})
		return nil
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "CLAUDE") {
			continue
		}
		if rel, ok := logicalName(e.Name(), contentMode); ok {
			if err := add(e.Name(), rel); err != nil {
				return nil, err
			}
		}
	}

	claudeDir := filepath.Join(profileDir, claudeTreeName)
	err = filepath.WalkDir(claudeDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(profileDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		dir, base := pathpkg.Dir(rel), filepath.Base(rel)
		logical, ok := logicalName(base, contentMode)
		if !ok {
			return nil
		}
		if dir == "." {
			return add(rel, logical)
		}
		return add(rel, dir+"/"+logical)
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return out, nil
}

// logicalName 把 <stem>.<mode>.<ext> 投影成 <stem>.<ext>；变体 mode 与目标 mode
// 不匹配时返回 false（该文件不参与投影）。非变体文件名原样返回。
func logicalName(name, contentMode string) (string, bool) {
	ext := filepath.Ext(name)
	if ext == "" {
		return name, true
	}
	stem := strings.TrimSuffix(name, ext)
	base, variant, ok := strings.Cut(stem, ".")
	if !ok || !IsValidMode(variant) {
		return name, true
	}
	if variant != contentMode {
		return "", false
	}
	return base + ext, true
}

// within 报告 target 是否位于 root 之内（路径逃逸防护）。
func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
