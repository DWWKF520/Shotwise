// 分镜图字段值的解析与校验（对应 lib/storyboard_sequence.py::resolve_storyboard_image_ref）。
//
// 字段值来自磁盘上的剧本 JSON，不可信任（归档导入、外部编辑、脏数据都能落值）：绝对路径 /
// .. 会把项目外任意文件当作分镜图使用，须先做类型检查、显式拒绝绝对路径，再解析并确认结果
// 落在 storyboards/ 目录内。旧宫格项目的非 canonical 文件名仍需正常解析，故只做目录归属校验。
package service

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/infra/fs"
)

// ResolveStoryboardImageRef 校验 “generated_assets.storyboard_image“ 字段值，返回落在
// storyboards/ 内的绝对路径；字段未设置（nil / ""）返回空串，由调用方按各自默认路径回退。
func ResolveStoryboardImageRef(projectDir string, storyboardRel any) (string, error) {
	if storyboardRel == nil {
		return "", nil
	}
	raw, ok := storyboardRel.(string)
	if !ok {
		return "", fmt.Errorf("invalid storyboard image path: %v", storyboardRel)
	}
	if raw == "" {
		return "", nil
	}
	// os.path.join 遇到绝对路径会丢弃 base；Windows 无盘符的根路径同样会在拼接时丢弃 base，
	// 故按正斜杠归一后一并挡掉。
	if filepath.IsAbs(raw) || strings.HasPrefix(strings.ReplaceAll(raw, "\\", "/"), "/") {
		return "", fmt.Errorf("invalid storyboard image path: %q", raw)
	}
	resolved := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(raw))
	if resolved == "" {
		return "", fmt.Errorf("invalid storyboard image path: %q", raw)
	}
	storyboardsRoot, err := fs.SafeJoin(projectDir, true, "storyboards")
	if err != nil {
		return "", fmt.Errorf("invalid storyboard image path: %q", raw)
	}
	rel, err := filepath.Rel(storyboardsRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// 与越界/脏数据分开措辞：这一支是「项目内但不在 storyboards/」。
		return "", fmt.Errorf("storyboard image path must stay under storyboards/: %q", raw)
	}
	return resolved, nil
}
