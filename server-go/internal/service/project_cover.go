// 项目封面与资产指纹的读时计算。
//
// 封面（对应 server/services/project_cover.py）：按「视频首帧 > 分镜图 > 场景设计图 >
// 角色设计图」的偏好顺序挑一个项目相对路径，转成 /api/v1/files/... URL；全无则 null。
// 不写回 project.json，每次列表请求按磁盘真相重算。
//
// 资产指纹（对应 lib/asset_fingerprints.py）：扫描固定媒体子目录，产出
// {项目相对路径: mtime_ns}，供前端做 cache-bust。
package service

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain/media"
)

// coverMediaSubdirs 是参与指纹扫描的媒体子目录。
var coverMediaSubdirs = []string{
	"storyboards", "end_frames", "videos", "thumbnails",
	"characters", "scenes", "props", "products", "grids", "reference_videos",
}

// rootMediaSuffixes 是项目根目录下允许参与指纹的媒体后缀。
var rootMediaSuffixes = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".mp4": true}

// ResolveProjectCover 返回封面 URL（/api/v1/files/{project}/{rel}），无可用封面返回 nil。
func ResolveProjectCover(
	store *ProjectStore, projectName string, project map[string]any, preloaded map[string]map[string]any,
) any {
	url := func(rel string) string {
		return "/api/v1/files/" + projectName + "/" + strings.TrimPrefix(rel, "/")
	}

	// 先加载全部剧本；缺失/损坏的集跳过，不阻塞列表。
	scripts := make([]map[string]any, 0)
	if episodes, ok := project["episodes"].([]any); ok {
		for _, raw := range episodes {
			ep, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			scriptFile, _ := ep["script_file"].(string)
			if scriptFile == "" {
				continue
			}
			if preloaded != nil {
				if script, hit := preloaded[scriptFile]; hit {
					scripts = append(scripts, script)
					continue
				}
			}
			if store == nil {
				continue
			}
			if script, err := store.ReadScriptJSON(projectName, scriptFile); err == nil {
				scripts = append(scripts, script)
			}
		}
	}

	// 第一趟：任意一集有视频首帧都胜过任何分镜图，故两趟分开而不是逐 item 并列判断。
	for _, script := range scripts {
		for _, item := range coverItems(script) {
			if thumb, ok := media.GeneratedAssets(item)["video_thumbnail"].(string); ok && thumb != "" {
				return url(thumb)
			}
		}
	}
	for _, script := range scripts {
		for _, item := range coverItems(script) {
			if sheet, ok := media.GeneratedAssets(item)["storyboard_image"].(string); ok && sheet != "" {
				return url(sheet)
			}
		}
	}

	// 项目级资产：scene > character。
	if scenes, ok := project["scenes"].(map[string]any); ok {
		for _, raw := range scenes {
			if entry, ok := raw.(map[string]any); ok {
				if sheet, ok := entry["scene_sheet"].(string); ok && sheet != "" {
					return url(sheet)
				}
			}
		}
	}
	if characters, ok := project["characters"].(map[string]any); ok {
		for _, raw := range characters {
			if entry, ok := raw.(map[string]any); ok {
				if sheet, ok := entry["character_sheet"].(string); ok && sheet != "" {
					return url(sheet)
				}
			}
		}
	}
	return nil
}

// coverItems 合并 segments（storyboard/grid）与 video_units（reference）两种集级结构。
//
// 不能写成 `video_units else segments`：storyboard 项目被误塞空 video_units 时，
// segments 里的真实产物会被整体丢弃。
func coverItems(script map[string]any) []map[string]any {
	out := make([]map[string]any, 0)
	for _, key := range []string{"segments", "video_units"} {
		if items, ok := script[key].([]any); ok {
			for _, raw := range items {
				if item, ok := raw.(map[string]any); ok {
					out = append(out, item)
				}
			}
		}
	}
	return out
}

// ComputeProjectAssetFingerprints 扫描项目媒体文件，返回 {相对路径: mtime_ns}。
func ComputeProjectAssetFingerprints(projectDir string) map[string]int64 {
	out := map[string]int64{}
	if projectDir == "" {
		return out
	}

	for _, subdir := range coverMediaSubdirs {
		scanFingerprintDir(subdir, filepath.Join(projectDir, subdir), out)
	}

	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if !rootMediaSuffixes[ext] {
			continue
		}
		if info, err := entry.Info(); err == nil {
			out[entry.Name()] = info.ModTime().UnixNano()
		}
	}
	return out
}

// scanFingerprintDir 扫描单个媒体子目录及其一级子目录（跳过 versions/）。
func scanFingerprintDir(prefix, dirPath string, out map[string]int64) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		full := filepath.Join(dirPath, entry.Name())
		if entry.IsDir() {
			if entry.Name() == "versions" {
				continue
			}
			subEntries, err := os.ReadDir(full)
			if err != nil {
				continue
			}
			for _, sub := range subEntries {
				if sub.IsDir() {
					continue
				}
				if info, err := sub.Info(); err == nil {
					out[prefix+"/"+entry.Name()+"/"+sub.Name()] = info.ModTime().UnixNano()
				}
			}
			continue
		}
		if info, err := entry.Info(); err == nil {
			out[prefix+"/"+entry.Name()] = info.ModTime().UnixNano()
		}
	}
}

// ProjectFingerprint 按 project.json 的 mtime 计算项目指纹。
//
// 口径与 transport/http 里项目事件流首帧的指纹一致：快照序列化差分（Python 侧 sha1 of stable
// json）尚未迁移，Go 侧以 project.json mtime 作指纹——足以驱动前端「指纹变了就重拉」的刷新语义。
// 文件不存在时返回空串。
func ProjectFingerprint(dir string) string {
	st, err := os.Stat(filepath.Join(dir, "project.json"))
	if err != nil {
		return ""
	}
	return st.ModTime().UTC().Format(time.RFC3339Nano)
}
