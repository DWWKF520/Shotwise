// 参考生视频正式 step1 的单一写盘出口（对应 lib/script_review.py 的
// step1_write_lock / write_step1_locked / assert_base_fingerprint 与 Step1WriteConflict）。
//
// 四条写路径（Web 端保存、重拆分、晋升、迁移回写）全部汇入本模块：锁、乐观并发比对与
// step2 隔离草稿清理只存在一处。Go 侧以进程内 per-path 排他锁等价 Python 的
// ProjectManager.file_lock（单进程部署；跨进程排他随部署形态增量接入）。
package reference

import (
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"os"
	"path/filepath"

	"github.com/shotwise/shotwise/internal/infra/fs"
)

// Step1Locks 是 step1 写盘出口的 per-path 排他锁表（键为正式文件绝对路径）。
var Step1Locks = fs.NewKeyedLocks()

// FingerprintExpectation 是乐观并发比对的基线：区分「不做比对」与「基线是 nil（写入方取基线时
// 正式文件不存在）」——两者都要能表达。
type FingerprintExpectation struct {
	// Unchecked 为 true 时跳过比对（重拆分整份重建、同临界区读改写）。
	Unchecked bool
	// Value 是写入方取基线时的正式文件指纹；nil 表示彼时文件不存在。
	Value *string
}

// UncheckedFingerprintExpectation 返回「不做基线比对」的基线。
func UncheckedFingerprintExpectation() FingerprintExpectation {
	return FingerprintExpectation{Unchecked: true}
}

// BaselineFingerprint 返回以给定指纹为基线（nil 表示取基线时文件不存在）的基线。
func BaselineFingerprint(value *string) FingerprintExpectation {
	return FingerprintExpectation{Value: value}
}

// Step1WriteConflict 是正式 step1 的乐观并发冲突：写入方的基线指纹与盘上现值不一致。
type Step1WriteConflict struct {
	Expected       *string
	Actual         *string
	CurrentContent map[string]any
}

func (e *Step1WriteConflict) Error() string {
	return fmt.Sprintf("step1 并发冲突：基线指纹 %v，盘上现值指纹 %v", step1DerefString(e.Expected), step1DerefString(e.Actual))
}

func step1DerefString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// OfficialReferenceStep1Path 返回该集参考生视频正式 step1 的路径。
func OfficialReferenceStep1Path(projectDir string, episode int) string {
	return filepath.Join(service.EpisodeDraftsDir(projectDir, episode), service.ReferenceVideoStep1Filename)
}

// AssertBaseFingerprint 做乐观并发比对：expected 与 path 盘上现值不一致时返回 *Step1WriteConflict、不落盘。
func AssertBaseFingerprint(path string, expected FingerprintExpectation) error {
	if expected.Unchecked {
		return nil
	}
	actual, err := service.ContentFingerprint(path)
	if err != nil {
		return err
	}
	if stringPtrEqual(expected.Value, actual) {
		return nil
	}
	var current map[string]any
	if loaded, err := service.ReadJSONObjectOrNone(path); err == nil {
		current = loaded
	}
	return &Step1WriteConflict{Expected: expected.Value, Actual: actual, CurrentContent: current}
}

// Step1WriteLock 是参考生视频正式 step1 的写临界区：建目录 + per-path 排他锁，回调收到正式文件路径。
func Step1WriteLock(projectDir string, episode int, fn func(step1Path string) error) error {
	draftsDir := service.EpisodeDraftsDir(projectDir, episode)
	if err := os.MkdirAll(draftsDir, 0o755); err != nil {
		return err
	}
	path := OfficialReferenceStep1Path(projectDir, episode)
	var inner error
	Step1Locks.WithLock(path, func() {
		inner = fn(path)
	})
	return inner
}

// WriteStep1Locked 是参考生视频正式 step1 的单一写盘出口：基线比对（OCC）→ 原子写 →
// 内容变化时清 step2 隔离草稿。返回内容是否发生变化。
//
// 调用方须已持有该文件的排他锁（Step1WriteLock）。expected 与盘上现值不一致时返回
// *Step1WriteConflict、不落盘——后写方拿冲突报告去合并，先写方内容不被静默覆盖。
func WriteStep1Locked(
	projectDir string, episode int, content map[string]any,
	expected FingerprintExpectation, clearStep2Quarantine bool,
) (bool, error) {
	path := OfficialReferenceStep1Path(projectDir, episode)
	if err := AssertBaseFingerprint(path, expected); err != nil {
		return false, err
	}
	previous, err := service.ReadJSONObjectOrNone(path)
	if err != nil {
		return false, err
	}
	changed := !fingerprintEqual(previous, content)
	if err := fs.WriteJSONAtomic(path, content); err != nil {
		return false, err
	}
	if changed && clearStep2Quarantine {
		if err := ClearQuarantine(projectDir, episode, QuarantineKindStep2); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// fingerprintEqual 判定两份已解析 JSON 对象语义相等（键序 / 整数与浮点等价均可）。
func fingerprintEqual(a, b map[string]any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return service.ContentFingerprintOfData(a) == service.ContentFingerprintOfData(b)
}

// stringPtrEqual 判定两个可空字符串相等（双 nil 为相等）。
func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
