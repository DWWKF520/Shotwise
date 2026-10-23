// 镜头尾帧的设置与清除（对应 server/services/end_frame.py）。
//
// 尾帧是镜头的用户意图持久属性（`end_frame_image`），不是运行时产出，故不进 generated_assets。
// 两条设置通道（上传任意图片 / 选项目内已有图片）在这里汇成同一落点：归一为 PNG 后快照复制到
// `end_frames/scene_{id}.png`，剧本条目只存该固定相对路径。源图与快照就此解耦——源图重生成、
// 版本回滚、删除都动不到已定尾帧，结构上不存在悬空引用。
//
// 并发正确性：设置与清除都在同一把剧本锁临界区内完成「文件副作用 + 字段写回」，两者互斥；
// 落盘失败时按「操作代次」条件回滚快照文件（只撤销自己的效果，不覆盖并发请求已落盘的成果）。
package asset

import (
	"context"
	"errors"
	"github.com/shotwise/shotwise/internal/service"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/shotwise/shotwise/internal/domain/media"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/infra/img"
)

// EndFrameUploadMaxBytes 是尾帧上传/选图源的字节上限（与 UPLOAD_IMAGE_MAX_BYTES 同口径）。
const EndFrameUploadMaxBytes = 30 << 20

// EndFrameImageDir 是尾帧快照所在子目录。
const EndFrameImageDir = "end_frames"

// EndFrameError 是尾帧操作的领域错误（携带状态码 + i18n key + 参数）。
type EndFrameError struct {
	Status int
	Key    string
	Params map[string]any
}

func (e *EndFrameError) Error() string { return e.Key }

func endFrameErr(status int, key string, params map[string]any) *EndFrameError {
	return &EndFrameError{Status: status, Key: key, Params: params}
}

// shotGenerationKey 标识「同一项目同一剧本同一镜头」的操作代次。
type shotGenerationKey struct{ project, script, shot string }

var (
	shotGenerationMu sync.Mutex
	shotGenerations  = map[shotGenerationKey]int{}
)

// currentShotGeneration 读当前代次。
func currentShotGeneration(key shotGenerationKey) int {
	shotGenerationMu.Lock()
	defer shotGenerationMu.Unlock()
	return shotGenerations[key]
}

// advanceShotGeneration 推进代次并返回新值。
//
// 在临界区内、文件 mutation 之前调用：只要有人进入过临界区，代次必然前移——失败一方的
// 补偿据此判断是否已被并发操作接管。
func advanceShotGeneration(key shotGenerationKey) int {
	shotGenerationMu.Lock()
	defer shotGenerationMu.Unlock()
	shotGenerations[key]++
	return shotGenerations[key]
}

// EndFrameService 承载尾帧设置/清除。
type EndFrameService struct{ store *service.ProjectStore }

// NewEndFrameService 构造。
func NewEndFrameService(store *service.ProjectStore) *EndFrameService {
	return &EndFrameService{store: store}
}

// SetFromBytes 上传通道：把图片字节落成该镜头的尾帧快照。
func (s *EndFrameService) SetFromBytes(
	ctx context.Context, projectName, scriptFile, shotID string, content []byte,
) (string, error) {
	projectDir, err := s.locateShot(projectName, scriptFile, shotID)
	if err != nil {
		return "", err
	}
	return s.applySnapshot(projectDir, projectName, scriptFile, shotID, content)
}

// SetFromProjectImage 项目内选图通道：读源图字节后走与上传完全相同的归一 + 快照落点。
func (s *EndFrameService) SetFromProjectImage(
	ctx context.Context, projectName, scriptFile, shotID, sourcePath string,
) (string, error) {
	projectDir, err := s.locateShot(projectName, scriptFile, shotID)
	if err != nil {
		return "", err
	}
	content, err := ReadProjectImage(projectDir, sourcePath)
	if err != nil {
		return "", err
	}
	return s.applySnapshot(projectDir, projectName, scriptFile, shotID, content)
}

// Clear 清除镜头尾帧：在同一临界区内把字段置空并删快照文件。
func (s *EndFrameService) Clear(ctx context.Context, projectName, scriptFile, shotID string) error {
	projectDir, err := s.locateShot(projectName, scriptFile, shotID)
	if err != nil {
		return err
	}
	target, _, err := snapshotTarget(projectDir, shotID)
	if err != nil {
		return err
	}
	key := shotGenerationKey{
		project: projectName, script: service.NormalizeScriptFilename(scriptFile), shot: shotID,
	}
	var (
		generation int
		oldBytes   []byte
		hadOld     bool
		entered    bool
	)
	err = s.store.UpdateScript(projectName, scriptFile, func(script map[string]any) error {
		// 基线必须在锁内读：锁外预读可能读到比「本次操作前」更早的内容，
		// 失败补偿时会把别人已成功的写覆盖掉。
		oldBytes, hadOld = readFileIfExists(target)
		matched, err := findShot(script, shotID)
		if err != nil {
			return err
		}
		generation = advanceShotGeneration(key)
		entered = true
		matched["end_frame_image"] = nil
		if removeErr := os.Remove(target); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
		return nil
	})
	if err != nil {
		if entered {
			s.restoreAfterPersistFailure(projectName, scriptFile, target, oldBytes, hadOld, key, generation)
		}
		return err
	}
	return nil
}

// ---- 内部 ----

// locateShot 校验该镜头可设尾帧并返回项目绝对路径。
//
// 参考生视频路径没有首尾帧概念，一律拒绝；判定按 project.json 的生成路线，不看剧本级戳
// （ad 的内容模式剧本不携带该戳，只看剧本会放过「ad + 参考路线」组合，让用户设下一个
// 生成时永不消费的尾帧）。
func (s *EndFrameService) locateShot(projectName, scriptFile, shotID string) (string, error) {
	projectDir, err := s.store.Dir(projectName)
	if err != nil {
		return "", endFrameErr(400, "invalid_project_name", map[string]any{"name": projectName})
	}
	if !s.store.Exists(projectName) {
		return "", endFrameErr(404, "project_not_found", map[string]any{"name": projectName})
	}
	script, err := s.store.ReadScriptJSON(projectName, scriptFile)
	if err != nil {
		return "", endFrameErr(400, "invalid_script_file", map[string]any{"name": scriptFile})
	}
	project, err := s.store.Get(projectName)
	if err != nil {
		return "", endFrameErr(404, "project_not_found", map[string]any{"name": projectName})
	}
	if project.IsReferenceVideo() {
		return "", endFrameErr(400, "end_frame_reference_video_unsupported", nil)
	}
	if _, err := findShot(script, shotID); err != nil {
		return "", err
	}
	return projectDir, nil
}

// applySnapshot 两条设置通道的共同落点：归一为 PNG → 写固定路径 → 写回字段。
//
// 换图即原地覆盖同一路径：字段值不变，前端靠资产指纹 cache-bust。
func (s *EndFrameService) applySnapshot(
	projectDir, projectName, scriptFile, shotID string, content []byte,
) (string, error) {
	target, relative, err := snapshotTarget(projectDir, shotID)
	if err != nil {
		return "", err
	}
	pngBytes, err := img.NormalizeToPNG(content, defaultEndFrameLongEdge())
	if err != nil {
		if errors.Is(err, img.ErrPixelLimit) {
			return "", endFrameErr(400, "image_pixels_too_large", map[string]any{
				"max_megapixels": img.MaxUploadPixels / 1_000_000,
			})
		}
		return "", endFrameErr(400, "invalid_image_file", nil)
	}

	key := shotGenerationKey{
		project: projectName, script: service.NormalizeScriptFilename(scriptFile), shot: shotID,
	}
	var (
		generation int
		oldBytes   []byte
		hadOld     bool
		entered    bool
	)
	err = s.store.UpdateScript(projectName, scriptFile, func(script map[string]any) error {
		oldBytes, hadOld = readFileIfExists(target)
		matched, err := findShot(script, shotID)
		if err != nil {
			return err
		}
		generation = advanceShotGeneration(key)
		entered = true
		// 先写文件：字段写回后若落盘失败，要靠代次补偿把文件还原。
		if err := service.WriteFileAtomic(target, pngBytes); err != nil {
			return err
		}
		matched["end_frame_image"] = relative
		return nil
	})
	if err != nil {
		if entered {
			s.restoreAfterPersistFailure(projectName, scriptFile, target, oldBytes, hadOld, key, generation)
		}
		return "", err
	}
	return relative, nil
}

// restoreAfterPersistFailure 剧本持久化失败后的补偿：重新过锁、按代次条件回滚快照文件。
//
// 回滚前复核代次是否仍是本次失败前那一次：仍是则说明无人进入过临界区，安全回滚；
// 已前移则说明并发操作已接管，回滚只会用陈旧字节覆盖它的成果，直接跳过。
func (s *EndFrameService) restoreAfterPersistFailure(
	projectName, scriptFile, target string, oldBytes []byte, hadOld bool,
	key shotGenerationKey, expectedGeneration int,
) {
	// 只改文件、不碰剧本：mutate 直接返回错误以跳过写盘，锁照常释放。
	_ = s.store.UpdateScript(projectName, scriptFile, func(script map[string]any) error {
		if currentShotGeneration(key) != expectedGeneration {
			return errSkipWrite
		}
		if hadOld {
			_ = service.WriteFileAtomic(target, oldBytes)
		} else if removeErr := os.Remove(target); removeErr != nil && !os.IsNotExist(removeErr) {
			return errSkipWrite
		}
		return errSkipWrite
	})
}

// errSkipWrite 是「回滚完成，跳过剧本写盘」的哨兵错误。
var errSkipWrite = errors.New("end frame rollback: skip script write")

// findShot 定位分镜条目；不存在时是 404 segment_not_found。
func findShot(script map[string]any, shotID string) (map[string]any, error) {
	items, idField, _ := media.StoryboardItems(script)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if service.PythonStrLike(item[idField]) == shotID {
			return item, nil
		}
	}
	return nil, endFrameErr(404, "segment_not_found", map[string]any{"id": shotID})
}

// snapshotTarget 返回尾帧快照的 (绝对路径, 项目内相对路径)；路径越界时是 400。
func snapshotTarget(projectDir, shotID string) (string, string, error) {
	relative, err := service.ResourceRelativePath(EndFrameImageDir, shotID)
	if err != nil {
		return "", "", endFrameErr(400, "invalid_resource_id", map[string]any{"resource_id": shotID})
	}
	target, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(relative))
	if err != nil {
		return "", "", endFrameErr(400, "invalid_resource_id", map[string]any{"resource_id": shotID})
	}
	return target, relative, nil
}

// ReadProjectImage 读取项目内已有图片的字节。
//
// 不限定来源子目录——分镜图 / 角色 / 场景 / 宫格切图都可直接选用，越界防护由 SafeJoin
// 统一负责。按上传通道同一上限有界读取，项目内合法存在的大文件（如视频）被当作选图源时
// 不至于无界内存占用。
func ReadProjectImage(projectDir, sourcePath string) ([]byte, error) {
	normalized := strings.TrimSpace(strings.ReplaceAll(sourcePath, "\\", "/"))
	if normalized == "" {
		return nil, endFrameErr(400, "invalid_end_frame_source", map[string]any{"path": sourcePath})
	}
	resolved, err := fs.SafeJoin(projectDir, false, filepath.FromSlash(normalized))
	if err != nil {
		return nil, endFrameErr(400, "invalid_end_frame_source", map[string]any{"path": sourcePath})
	}
	info, statErr := os.Stat(resolved)
	if statErr != nil || info.IsDir() {
		return nil, endFrameErr(404, "end_frame_source_not_found", map[string]any{"path": normalized})
	}
	file, openErr := os.Open(resolved)
	if openErr != nil {
		return nil, endFrameErr(404, "end_frame_source_not_found", map[string]any{"path": normalized})
	}
	defer func() { _ = file.Close() }()
	content, readErr := img.ReadAllBounded(file, EndFrameUploadMaxBytes)
	if readErr != nil {
		if errors.Is(readErr, img.ErrTooLarge) {
			return nil, endFrameErr(400, "end_frame_source_too_large", map[string]any{
				"max_mb": EndFrameUploadMaxBytes / (1024 * 1024),
				"path":   normalized,
			})
		}
		return nil, readErr
	}
	return content, nil
}

// readFileIfExists 读文件字节；不存在返回 (nil, false)。
func readFileIfExists(path string) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, false
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return content, true
}

// defaultEndFrameLongEdge 是尾帧快照的长边上限（与 Python normalize_storyboard_upload 默认值一致）。
func defaultEndFrameLongEdge() *int {
	limit := 2048
	return &limit
}
