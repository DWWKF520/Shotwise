// step1 时长收编迁移测试（对应 tests/lib/test_reference_video_duration_migration.py
// 的纯函数部分 + tests/test_script_review.py 的确认平移部分）。
package reference

import (
	"github.com/shotwise/shotwise/internal/service"
	"testing"

	"github.com/shotwise/shotwise/internal/domain"
)

// legacyStep1Unit 构造一份收编前形态的 unit（per-shot 时长 + duration_override 标记）。
func legacyStep1Unit(unitID string, shots []int, durationSeconds any) map[string]any {
	shotList := make([]any, 0, len(shots))
	total := 0
	for i, d := range shots {
		shotList = append(shotList, map[string]any{"duration": float64(d), "text": "镜头"})
		_ = i
		total += d
	}
	unit := map[string]any{
		"unit_id": unitID,
		"shots":   shotList,
		"references": []any{
			map[string]any{"kind": "original"},
		},
	}
	if durationSeconds != nil {
		unit["duration_seconds"] = durationSeconds
	} else if total > 0 {
		unit["duration_seconds"] = float64(total)
	}
	unit["duration_override"] = false
	return unit
}

func TestMigrateUnitDurationsSumsAndStrips(t *testing.T) {
	units := []any{legacyStep1Unit("E1U1", []int{3, 5}, nil)}
	changed, warnings := MigrateUnitDurations(units, nil)
	if !changed || len(warnings) != 0 {
		t.Fatalf("changed=%v warnings=%v", changed, warnings)
	}
	unit := units[0].(map[string]any)
	if service.IntValueOrZero(unit["duration_seconds"]) != 8 {
		t.Fatalf("duration_seconds = %v", unit["duration_seconds"])
	}
	shots := unit["shots"].([]any)
	for _, raw := range shots {
		shot := raw.(map[string]any)
		if _, present := shot["duration"]; present {
			t.Fatalf("shot duration not stripped: %v", shot)
		}
	}
	if _, present := unit["duration_override"]; present {
		t.Fatalf("duration_override not stripped")
	}
}

func TestMigrateUnitDurationsManualDurationWins(t *testing.T) {
	unit := legacyStep1Unit("E1U1", []int{1}, float64(12))
	unit["duration_override"] = true
	changed, _ := MigrateUnitDurations([]any{unit}, nil)
	if !changed {
		t.Fatalf("expected change")
	}
	if service.IntValueOrZero(unit["duration_seconds"]) != 12 {
		t.Fatalf("duration_seconds = %v, 手填值不得被镜头和改写", unit["duration_seconds"])
	}
}

func TestMigrateUnitDurationsFallsBackToShotSumWhenDirty(t *testing.T) {
	unit := legacyStep1Unit("E1U1", []int{4, 6}, nil)
	unit["duration_seconds"] = nil
	MigrateUnitDurations([]any{unit}, nil)
	if service.IntValueOrZero(unit["duration_seconds"]) != 10 {
		t.Fatalf("duration_seconds = %v", unit["duration_seconds"])
	}
}

func TestMigrateUnitDurationsClampsOutOfRange(t *testing.T) {
	unit := legacyStep1Unit("E1U1", []int{15, 15, 15, 15}, float64(9999))
	_, warnings := MigrateUnitDurations([]any{unit}, nil)
	_, high := domain.ReferenceUnitDurationRange()
	if service.IntValueOrZero(unit["duration_seconds"]) != high {
		t.Fatalf("duration_seconds = %v, want clamp 到区间上界 %d", unit["duration_seconds"], high)
	}
	found := false
	for _, w := range warnings {
		if w.Key == "val_unit_duration_clamped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected val_unit_duration_clamped warning, got %v", warnings)
	}
}

func TestMigrateUnitDurationsKeepsSlotMemberBeyondShotSum(t *testing.T) {
	unit := legacyStep1Unit("E1U1", []int{15, 15}, float64(120))
	_, warnings := MigrateUnitDurations([]any{unit}, []int{8, 120})
	if service.IntValueOrZero(unit["duration_seconds"]) != 120 {
		t.Fatalf("duration_seconds = %v", unit["duration_seconds"])
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestMigrateUnitDurationsTakesSlot(t *testing.T) {
	unit := legacyStep1Unit("E1U1", []int{3, 5}, nil)
	_, warnings := MigrateUnitDurations([]any{unit}, []int{4, 6, 12})
	if service.IntValueOrZero(unit["duration_seconds"]) != 12 {
		t.Fatalf("duration_seconds = %v", unit["duration_seconds"])
	}
	found := false
	for _, w := range warnings {
		if w.Key == "val_unit_duration_slotted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected val_unit_duration_slotted warning, got %v", warnings)
	}
}

func TestMigrateUnitDurationsSlotClampsDown(t *testing.T) {
	unit := legacyStep1Unit("E1U1", []int{15, 15}, nil)
	MigrateUnitDurations([]any{unit}, []int{4, 8})
	if service.IntValueOrZero(unit["duration_seconds"]) != 8 {
		t.Fatalf("duration_seconds = %v", unit["duration_seconds"])
	}
}

func TestMigrateUnitDurationsIdempotent(t *testing.T) {
	units := []any{legacyStep1Unit("E1U1", []int{3, 5}, nil)}
	MigrateUnitDurations(units, nil)
	first := service.ContentFingerprintOfData(map[string]any{"units": units})
	changed, _ := MigrateUnitDurations(units, nil)
	if changed {
		t.Fatalf("second run should not change")
	}
	if service.ContentFingerprintOfData(map[string]any{"units": units}) != first {
		t.Fatalf("second run altered content")
	}
}

func TestMigrateUnitDurationsLeavesMigratedUntouched(t *testing.T) {
	units := []any{map[string]any{
		"unit_id": "E1U1", "shots": []any{map[string]any{"text": "x"}}, "duration_seconds": float64(8),
	}}
	changed, warnings := MigrateUnitDurations(units, nil)
	if changed || len(warnings) != 0 {
		t.Fatalf("changed=%v warnings=%v, 收编后的 unit 不应再触发", changed, warnings)
	}
}

func TestMigrateUnitDurationsToleratesDirtyShapes(t *testing.T) {
	units := []any{
		"not a dict",
		map[string]any{"unit_id": "E1U1", "shots": "bad", "duration_override": false},
	}
	changed, _ := MigrateUnitDurations(units, nil)
	if !changed {
		t.Fatalf("duration_override 剥离本身就是变更")
	}
	if changed, _ := MigrateUnitDurations("not a list", nil); changed {
		t.Fatalf("非列表输入应为空操作")
	}
}

func TestMigrateUnitDurationsDoesNotInventDuration(t *testing.T) {
	unit := map[string]any{"unit_id": "E1U1", "shots": []any{map[string]any{"duration": float64(0), "text": "x"}}}
	MigrateUnitDurations([]any{unit}, nil)
	if _, present := unit["duration_seconds"]; present {
		t.Fatalf("无可用时长来源时不得杜撰 duration_seconds")
	}
}

func TestMigrateScriptUnitDurationsOnlyReferenceSkeleton(t *testing.T) {
	narration := map[string]any{"segments": []any{map[string]any{"segment_id": "E1S1", "duration_seconds": float64(4)}}}
	if changed, _ := MigrateScriptUnitDurations(narration); changed {
		t.Fatalf("narration 骨架应是空操作")
	}
	if changed, _ := MigrateScriptUnitDurations("not a dict"); changed {
		t.Fatalf("非对象输入应是空操作")
	}
}
