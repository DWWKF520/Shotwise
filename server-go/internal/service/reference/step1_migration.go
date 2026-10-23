// 参考生视频 unit 时长的存量迁移（对应 lib/reference_video/duration_migration.py）。
//
// 时长曾经挂在 shots[*].duration 上、unit 时长是它们的求和派生。收编后 unit 时长是唯一
// 真相（ReferenceVideoUnit.duration_seconds），镜头不再承载时长。存量落盘因此需要一次性
// 改写：求和写进 unit 字段、剥掉镜头上的时长与随之退役的 duration_override 标记。
//
// 迁移器是纯函数（就地改写传入的数据、返回是否变更 + warning 列表），由加载侧在锁内做
// read-modify-write 回写——迁移一次落盘、谁先跑谁定终局，二次加载不再触发。
//
// supportedDurations 为空（项目尚未配置可解析的视频型号，以及剧集脚本同步加载链）时只做
// 结构区间 clamp；给了档位表则按容量语义取档，偏移记 warning。
package reference

import (
	"fmt"
	"github.com/shotwise/shotwise/internal/service"
	"log/slog"
	"strings"

	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/domain/script"
)

// legacyShotDurationField 与 legacyUnitDurationField 是迁移会剥掉的镜头级字段与 unit 级退役字段。
const (
	legacyShotDurationField = "duration"
	legacyUnitDurationField = "duration_override"
)

// stripLegacyShotDurations 剥掉各镜头的遗留时长字段，返回 (时长之和, 是否剥掉过字段)。
//
// 和为 0 表示「没有可用的时长来源」——无镜头时长、或全是脏值（和为 0）都归此类：
// 0 秒不是合法时长，两种情形都应让调用方回退到 unit 自身的字段而非杜撰一个值。
// 第二项独立回传：剥字段本身就是需要落盘的变更，与和是否可用无关。
func stripLegacyShotDurations(unit map[string]any) (int, bool) {
	shots, ok := unit["shots"].([]any)
	if !ok {
		return 0, false
	}
	total := 0
	stripped := false
	for _, raw := range shots {
		shot, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		value, present := shot[legacyShotDurationField]
		if !present {
			continue
		}
		stripped = true
		delete(shot, legacyShotDurationField)
		if n, ok := service.PositiveIntValue(value); ok {
			total += n
		}
	}
	return total, stripped
}

// MigrateUnitDurations 就地把 units 的 per-shot 时长收编到 unit 级，返回 (是否发生变更, warnings)。
//
// 每个 unit 的目标时长优先取已有的 duration_seconds（收编前它已是求和结果或用户手填值，
// 两种情形下都已是这个 unit 实际申请的秒数），缺失或脏值时回退为各镜头时长之和。两者都取
// 不到的 unit 只剥字段、不杜撰时长——它在收编前就缺必填字段，迁移不负责把非法数据补成合法。
//
// supportedDurations 非空时按容量语义取档（超出最大档位即 clamp 到最大档位），偏移记
// warning；空则只 clamp 到 unit 时长的结构合理性区间。
func MigrateUnitDurations(units any, supportedDurations []int) (bool, []service.MigrationWarning) {
	list, ok := units.([]any)
	if !ok {
		return false, nil
	}

	changed := false
	var warnings []service.MigrationWarning
	low, high := script.ReferenceUnitDurationRange()

	for _, raw := range list {
		unit, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		legacyTotal, stripped := stripLegacyShotDurations(unit)
		_, hadOverride := unit[legacyUnitDurationField]
		delete(unit, legacyUnitDurationField)
		if !stripped && !hadOverride {
			continue
		}
		changed = true

		target, ok := service.PositiveIntValue(unit["duration_seconds"])
		if !ok {
			target = legacyTotal
		}
		if target == 0 {
			continue
		}

		clamped := min(max(target, low), high)
		if clamped != target {
			warnings = append(warnings, service.MigrationWarning{
				Key: "val_unit_duration_clamped",
				Params: map[string]any{
					"unit_id": unit["unit_id"], "target": target,
					"low": low, "high": high, "clamped": clamped,
				},
			})
		}
		if len(supportedDurations) > 0 {
			slot := service.ResolveDurationSlot(float64(clamped), supportedDurations)
			if slot.Seconds != clamped {
				warnings = append(warnings, service.MigrationWarning{
					Key: "val_unit_duration_slotted",
					Params: map[string]any{
						"unit_id": unit["unit_id"], "duration": clamped,
						"durations": service.SortedUniqueInts(supportedDurations), "slot": slot.Seconds,
					},
				})
			}
			clamped = slot.Seconds
		}
		unit["duration_seconds"] = clamped
	}

	return changed, warnings
}

// MigrateScriptUnitDurations 剧集脚本级入口：仅对参考生视频骨架（含 video_units）生效。
//
// 按数据形状而非 generation_mode 戳判定——脏数据与半成品剧本可能缺戳，而收编只关心
// 「有没有 per-shot 时长要剥」，对其它骨架天然是空操作。
func MigrateScriptUnitDurations(script any) (bool, []service.MigrationWarning) {
	m, ok := script.(map[string]any)
	if !ok {
		return false, nil
	}
	return MigrateUnitDurations(m["video_units"], nil)
}

// migrationWarningTemplates 是迁移警告 key 的默认（中文）渲染模板，与
// lib/i18n/zh/validation.py 的同名 key 同文案；未登记的 key 原样返回（便于发现漏配）。
var migrationWarningTemplates = map[string]string{
	"val_unit_duration_clamped": "unit {unit_id} 时长 {target}s 超出 {low}-{high}s 合理区间，已裁剪为 {clamped}s",
	"val_unit_duration_slotted": "unit {unit_id} 时长 {duration}s 不是模型档位（{durations}）成员，已取档为 {slot}s",
}

// RenderMigrationWarning 按 key + params 渲染一条迁移警告的默认文本（供智能体与日志边界消费）。
func RenderMigrationWarning(w service.MigrationWarning) string {
	tmpl, ok := migrationWarningTemplates[w.Key]
	if !ok {
		return w.Key
	}
	out := tmpl
	for k, v := range w.Params {
		out = strings.ReplaceAll(out, "{"+k+"}", fmt.Sprint(v))
	}
	return out
}

// RenderMigrationWarnings 渲染警告列表，以「；」连接（对齐 Python 的 join 语义）。
func RenderMigrationWarnings(warnings []service.MigrationWarning) string {
	parts := make([]string, 0, len(warnings))
	for _, w := range warnings {
		parts = append(parts, RenderMigrationWarning(w))
	}
	return strings.Join(parts, "；")
}

// CarryConfirmationThroughMigration 把确认指纹从迁移前内容平移到迁移后内容
// （对应 lib/script_review.py::carry_confirmation_through_migration）。//
// 存量 step1 草稿的时长收编迁移是机械格式收编、不是内容编辑，但回写会让内容指纹漂移。
// 若该集确认指纹恰好记的是迁移前内容（before），就把它平移到迁移后的值（after），
// 避免一个早已确认的分集仅因被迁移回写就退回待审；指纹本就对不上（step1 在迁移外确实被
// 改过）时不动，返回 false，照常按待审处理。
//
// 仅适用于迁移无 warnings 的情形（纯格式收编，时长取值未被 clamp 改写）。
func CarryConfirmationThroughMigration(pf *project.ProjectFile, episode int, before, after string) bool {
	stored := service.StoredReview(pf, episode)
	if stored == nil || stored.Fingerprint != before {
		return false
	}
	return service.ApplyConfirmation(pf, episode, after, stored.ConfirmedAt)
}

// step1MigrationStore 是 step1 迁移对项目读写的最小依赖（*service.ProjectStore 与
// ScriptGenStore 实现方都满足）。
type step1MigrationStore interface {
	Dir(name string) (string, error)
	Update(name string, mutate func(*project.ProjectFile) error) error
}

// MigrateStep1DraftInPlace 对已读入内存的 step1 草稿就地做一次性时长收编迁移并回写
// （对应 lib/script_review.py::migrate_step1_draft_in_place），返回 (最新 project, warnings,
// 错误)。未发生迁移时 project 为 nil、warnings 为空。
//
// 调用方须已持有该文件的排他锁（Step1WriteLock）——回写经单一写盘出口 WriteStep1Locked，
// 与 Web 端保存 / 重拆分写盘同一把 per-path 锁。迁移是机械格式收编、不是内容编辑，不作废
// step2 隔离草稿；同临界区读改写也无并发窗口，不做基线比对。
//
// 迁移多数情况下是机械格式收编，回写会让内容指纹漂移：经 store.Update 在锁内把该集确认
// 指纹平移到迁移后的值，避免已确认分集仅因被加载就退回待审。warnings 非空说明迁移按档位 /
// 结构区间 clamp 改写了实际时长取值——那是内容变更，不平移确认，已确认分集经指纹比对照常
// 退回待审；从未存过指纹、靠 grandfather 判据（step2 产物已存在）放行的存量集则显式记下
// 迁移前内容的指纹，使其同样失配退回待审。
//
// project 侧的确认标记先落盘、草稿后落盘：两次写之间中断时草稿仍是迁移前内容，下次加载
// 重跑迁移即自愈。反序则草稿已丢失旧字段、重跑判 changed=false，标记永久缺失——靠
// grandfather 判据放行的存量集会带着被 clamp 的时长停在 confirmed，绕过审阅门。
//
// supportedDurations 非空时（step2 加载侧持有模型档位）收编结果直接取档；空（web gate 侧，
// 能力解析拿不到档位）只做结构区间 clamp。
func MigrateStep1DraftInPlace(
	store step1MigrationStore, projectName string,
	content map[string]any, episode int, supportedDurations []int,
) (*project.ProjectFile, []service.MigrationWarning, error) {
	if content == nil {
		return nil, nil, nil
	}
	before := service.ContentFingerprintOfData(content)
	changed, warnings := MigrateUnitDurations(content["units"], supportedDurations)
	for _, message := range warnings {
		slog.Warn("step1 草稿时长收编迁移",
			"file", service.ReferenceVideoStep1Filename, "key", message.Key, "params", message.Params)
	}
	if !changed {
		return nil, nil, nil
	}

	var updated *project.ProjectFile
	if len(warnings) > 0 {
		// clamp 改写了实际时长取值，是内容变更：不平移确认。已存过指纹的分集不插手——
		// 确认的是迁移前内容时指纹已自然失配，确认的是别的内容时 review_status 本就判
		// pending_review；无指纹的 grandfather 存量集显式记下迁移前指纹使其退回待审。
		err := store.Update(projectName, func(pf *project.ProjectFile) error {
			updated = pf
			stored := service.StoredReview(pf, episode)
			if stored == nil || stored.Fingerprint == "" {
				confirmedAt := ""
				if stored != nil {
					confirmedAt = stored.ConfirmedAt
				}
				service.ApplyConfirmation(pf, episode, before, confirmedAt)
			}
			return nil
		})
		if err != nil {
			return nil, warnings, err
		}
	} else {
		after := service.ContentFingerprintOfData(content)
		err := store.Update(projectName, func(pf *project.ProjectFile) error {
			updated = pf
			CarryConfirmationThroughMigration(pf, episode, before, after)
			return nil
		})
		if err != nil {
			return nil, warnings, err
		}
	}

	// 草稿后落盘（project 侧标记已在上面的 Update 内先落盘）。
	projectDir, err := store.Dir(projectName)
	if err != nil {
		return nil, warnings, err
	}
	if _, err := WriteStep1Locked(projectDir, episode, content, UncheckedFingerprintExpectation(), false); err != nil {
		return nil, warnings, err
	}
	return updated, warnings, nil
}

func init() {
	// 依赖倒置：剧本写盘守卫（根包 script_store）经钩子调用本域的时长收编迁移。
	service.MigrateScriptUnitDurationsHook = MigrateScriptUnitDurations
}
