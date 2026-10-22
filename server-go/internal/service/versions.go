// 版本管理（对应 Python lib/version_manager.py 的生成路径最小面）：生成产物落盘后
// 在 versions/{resource_type}/ 下留快照并在 versions.json 记账，读侧据此做版本时光机。
//
// 只迁移 worker 执行路径用到的方法（add_version + 最近一条 created_at 读取）；rename /
// backup 等资产管理入口随资产域迁移补齐。文件名与 versions.json 结构必须与 Python 侧
// 逐字对齐——前端 file_url 与版本回溯按同一形状读两个服务共写的项目目录。
package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shotwise/shotwise/internal/infra/fs"
)

// resourcePattern 是单一资源类型的路径形状（对应 lib/resource_paths.py::_PATTERNS）。
type resourcePattern struct {
	subdir string
	ext    string
	prefix string
}

// resourcePatterns 是「资源类型 → 路径形状」的唯一真相源，与 Python 侧逐条对齐；
// storyboards/videos/end_frames 共用 scene_ 前缀（与镜头 id 同名），audio 用 segment_。
var resourcePatterns = map[string]resourcePattern{
	"storyboards":      {"storyboards", ".png", "scene_"},
	"end_frames":       {"end_frames", ".png", "scene_"},
	"videos":           {"videos", ".mp4", "scene_"},
	"characters":       {"characters", ".png", ""},
	"scenes":           {"scenes", ".png", ""},
	"props":            {"props", ".png", ""},
	"products":         {"products", ".png", ""},
	"grids":            {"grids", ".png", ""},
	"reference_videos": {"reference_videos", ".mp4", ""},
	"audio":            {"audio", ".wav", "segment_"},
}

// resourceRelativePath 返回资源在项目内的相对路径（posix 正斜杠）。
// storyboards 形如 storyboards/scene_{id}.png，audio 形如 audio/segment_{id}.wav，
// 其余 {subdir}/{id}{ext}；未知类型报错。
func ResourceRelativePath(resourceType, resourceID string) (string, error) {
	p, err := resourcePatternFor(resourceType)
	if err != nil {
		return "", err
	}
	return p.subdir + "/" + p.prefix + resourceID + p.ext, nil
}

// resourceExtension 返回资源类型的扩展名（含点）。
func resourceExtension(resourceType string) (string, error) {
	p, err := resourcePatternFor(resourceType)
	if err != nil {
		return "", err
	}
	return p.ext, nil
}

func resourcePatternFor(resourceType string) (resourcePattern, error) {
	p, ok := resourcePatterns[resourceType]
	if !ok {
		return resourcePattern{}, fmt.Errorf("不支持的资源类型: %s", resourceType)
	}
	return p, nil
}

// versionsLocks 进程内串行化同一项目 versions.json 的读写（按项目目录为键）。
// Python 侧用文件锁跨进程互斥；Go 侧 worker 生成任务由 worker lease 保证单实例执行，
// 进程内锁即够，双 worker 部署被 lease 互斥挡在架构层。
var versionsLocks = fs.NewKeyedLocks()

// versionRecord 是 versions.json 里单条版本记录，键名与 Python 写侧一致。
type versionRecord struct {
	Version   int            `json:"version"`
	File      string         `json:"file"`
	Prompt    string         `json:"prompt"`
	CreatedAt string         `json:"created_at"`
	Extra     map[string]any `json:"-"`
}

// versionsData 是 versions.json 的整体形状：{resource_type: {resource_id: {current_version, versions}}}。
type versionsData map[string]map[string]*versionsResource

type versionsResource struct {
	CurrentVersion int             `json:"current_version"`
	Versions       []versionRecord `json:"versions"`
}

// UnmarshalJSON 把已知键收进结构字段、其余键归入 Extra（元数据如 aspect_ratio 原样保真）。
func (r *versionRecord) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["version"]; ok {
		if err := json.Unmarshal(v, &r.Version); err != nil {
			return err
		}
	}
	if v, ok := raw["file"]; ok {
		if err := json.Unmarshal(v, &r.File); err != nil {
			return err
		}
	}
	if v, ok := raw["prompt"]; ok {
		if err := json.Unmarshal(v, &r.Prompt); err != nil {
			return err
		}
	}
	if v, ok := raw["created_at"]; ok {
		if err := json.Unmarshal(v, &r.CreatedAt); err != nil {
			return err
		}
	}
	extra := map[string]any{}
	for k, v := range raw {
		if k == "version" || k == "file" || k == "prompt" || k == "created_at" {
			continue
		}
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			return err
		}
		extra[k] = val
	}
	if len(extra) > 0 {
		r.Extra = extra
	}
	return nil
}

// MarshalJSON 把 Extra 摊平回顶层，保持与 Python **metadata 合并语义一致。
func (r versionRecord) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"version":    r.Version,
		"file":       r.File,
		"prompt":     r.Prompt,
		"created_at": r.CreatedAt,
	}
	for k, v := range r.Extra {
		out[k] = v
	}
	return json.Marshal(out)
}

// Versions 是单个项目目录上的版本账本。
type Versions struct {
	projectDir string
}

// withVersionsLock 在项目级锁内执行 fn；KeyedLocks 回调不返回错误，经外层变量转出。
func withVersionsLock(key string, fn func() error) error {
	var err error
	versionsLocks.WithLock(key, func() { err = fn() })
	return err
}

// NewVersions 构造；只记路径不做任何磁盘操作（只读用法不在项目下留空目录树）。
func NewVersions(projectDir string) *Versions { return &Versions{projectDir: projectDir} }

// AddVersion 追加一条版本记录并把源文件复制进版本目录，返回新版本号与 created_at。
//
// 版本号取该资源历史最大版本 +1（不依赖 current_version，与 Python 侧一致）；时间戳进
// 文件名保证同秒多次生成不互相覆盖。sourceFile 为空或不存在时只记元数据不落快照。
func (v *Versions) AddVersion(resourceType, resourceID, prompt, sourceFile string, metadata map[string]any) (int, string, error) {
	ext, err := resourceExtension(resourceType)
	if err != nil {
		return 0, "", err
	}
	var (
		version   int
		createdAt string
	)
	err = withVersionsLock(v.lockKey(), func() error {
		data, err := v.load()
		if err != nil {
			return err
		}
		bucket := data[resourceType]
		if bucket == nil {
			bucket = map[string]*versionsResource{}
			data[resourceType] = bucket
		}
		rec := bucket[resourceID]
		if rec == nil {
			rec = &versionsResource{}
			bucket[resourceID] = rec
		}
		for _, item := range rec.Versions {
			if item.Version > version {
				version = item.Version
			}
		}
		version++

		timestamp := time.Now().Format("20060102T150405")
		filename := fmt.Sprintf("%s_v%d_%s%s", resourceID, version, timestamp, ext)
		relPath := "versions/" + resourceType + "/" + filename
		if sourceFile != "" {
			if _, err := os.Stat(sourceFile); err == nil {
				if err := copyFileInto(filepath.Join(v.projectDir, filepath.FromSlash(relPath)), sourceFile); err != nil {
					return err
				}
			}
		}

		createdAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
		record := versionRecord{Version: version, File: relPath, Prompt: prompt, CreatedAt: createdAt}
		for k, val := range metadata {
			if val == nil {
				continue
			}
			if record.Extra == nil {
				record.Extra = map[string]any{}
			}
			record.Extra[k] = val
		}
		rec.Versions = append(rec.Versions, record)
		rec.CurrentVersion = version
		return v.save(data)
	})
	if err != nil {
		return 0, "", err
	}
	return version, createdAt, nil
}

// LatestCreatedAt 返回该资源最近一条版本的 created_at（对应 Python 侧
// get_versions(...)["versions"][-1]["created_at"]）；无任何版本时报错而不是静默空串，
// 调用方（任务 result 契约）依赖它非空。
func (v *Versions) LatestCreatedAt(resourceType, resourceID string) (string, error) {
	var createdAt string
	err := withVersionsLock(v.lockKey(), func() error {
		data, err := v.load()
		if err != nil {
			return err
		}
		rec := data[resourceType][resourceID]
		if rec == nil || len(rec.Versions) == 0 {
			return fmt.Errorf("资源无版本记录: %s/%s", resourceType, resourceID)
		}
		createdAt = rec.Versions[len(rec.Versions)-1].CreatedAt
		return nil
	})
	if err != nil {
		return "", err
	}
	return createdAt, nil
}

// EnsureCurrentTracked 确保磁盘上已存在的当前产物至少有一条版本记录（对应 Python 侧同名方法）：
// 再生成会把当前文件原地覆盖，未走过版本管理的历史内容（迁移遗留 / 外部放置）会因此不可回溯，
// 故生成前先补快照。该资源已有版本（current_version > 0）时不动；currentFile 不存在时静默跳过。
func (v *Versions) EnsureCurrentTracked(resourceType, resourceID, prompt, currentFile string, metadata map[string]any) (int, error) {
	if _, err := os.Stat(currentFile); err != nil {
		return 0, nil
	}
	tracked := false
	if err := withVersionsLock(v.lockKey(), func() error {
		data, err := v.load()
		if err != nil {
			return err
		}
		if rec := data[resourceType][resourceID]; rec != nil && rec.CurrentVersion > 0 {
			tracked = true
		}
		return nil
	}); err != nil {
		return 0, err
	}
	if tracked {
		return 0, nil
	}
	_, _, err := v.AddVersion(resourceType, resourceID, prompt, currentFile, metadata)
	return 0, err
}

// UpdateVersionMetadata 为既有版本记录补写元数据键（覆盖同名键），返回是否命中。
//
// 供生成 finalize 在版本入库后回填只有 finalize 阶段才确定的元数据（如参考单元产物的来源
// 签名）。目标记录不存在时返回 false 不报错：档案补写是 best-effort。
func (v *Versions) UpdateVersionMetadata(
	resourceType, resourceID string, version int, metadata map[string]any,
) (bool, error) {
	if _, err := resourcePatternFor(resourceType); err != nil {
		return false, fmt.Errorf("不支持的资源类型: %s", resourceType)
	}
	found := false
	err := withVersionsLock(v.lockKey(), func() error {
		data, err := v.load()
		if err != nil {
			return err
		}
		rec := data[resourceType][resourceID]
		if rec == nil {
			return nil
		}
		for i := range rec.Versions {
			if rec.Versions[i].Version != version {
				continue
			}
			if rec.Versions[i].Extra == nil {
				rec.Versions[i].Extra = map[string]any{}
			}
			for k, value := range metadata {
				rec.Versions[i].Extra[k] = value
			}
			found = true
			return v.save(data)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

func (v *Versions) lockKey() string { return v.projectDir }

func (v *Versions) load() (versionsData, error) {
	data := versionsData{}
	path := filepath.Join(v.projectDir, "versions", "versions.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return data, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("versions.json 解析失败: %w", err)
	}
	return data, nil
}

func (v *Versions) save(data versionsData) error {
	dir := filepath.Join(v.projectDir, "versions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// 显式 UTF-8 缩进输出，与 Python json.dump(ensure_ascii=False, indent=2) 同形。
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "versions.json"), raw, 0o644)
}

// copyFileInto 把源文件写到目标路径（父目录按需创建）；逐字节拷贝等价 shutil.copy2 的
// 内容语义（元数据时间戳对版本功能无影响，刻意不带）。
func copyFileInto(dst, src string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}
