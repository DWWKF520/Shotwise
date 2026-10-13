package queue

import (
	"testing"

	"github.com/shotwise/shotwise/internal/provider"
)

// fakeClock 是可手动推进的单调时钟。
type fakeClock struct{ now float64 }

func (c *fakeClock) advance(seconds float64) { c.now += seconds }
func (c *fakeClock) read() float64           { return c.now }

func testHealthTable(t *testing.T) (*ProviderHealthTable, *fakeClock) {
	t.Helper()
	clock := &fakeClock{}
	table := NewProviderHealthTable(HealthSettings{FailureThreshold: 3, OpenSeconds: 60, MaxOpenSeconds: 600}, clock.read)
	return table, clock
}

func TestHealthTableClosedFoldsCapacityByConsecutiveFailures(t *testing.T) {
	table, _ := testHealthTable(t)

	if got := table.State("p1", provider.MediaImage); got != LaneClosed {
		t.Fatalf("state = %s, want closed with no accounting", got)
	}
	if got := table.Capacity("p1", provider.MediaImage, 5); got != 5 {
		t.Fatalf("capacity = %d, want the full base capacity", got)
	}

	// 阈值 3：前两次瞬态失败只逐次收紧，不熔断。
	if table.RecordFailure("p1", provider.MediaImage, true) {
		t.Error("RecordFailure = true before reaching the threshold")
	}
	if table.RecordFailure("p1", provider.MediaImage, true) {
		t.Error("RecordFailure = true before reaching the threshold")
	}
	if got := table.Capacity("p1", provider.MediaImage, 5); got != 3 {
		t.Errorf("capacity = %d, want base minus two failures", got)
	}
	if blocked := table.BlockedProviders(provider.MediaImage); len(blocked) != 0 {
		t.Errorf("blocked = %v, want none while still closed", blocked)
	}
}

func TestHealthTableNonTransientFailuresAreNotCounted(t *testing.T) {
	table, _ := testHealthTable(t)

	for range 5 {
		if table.RecordFailure("p1", provider.MediaImage, false) {
			t.Fatal("RecordFailure(transient=false) = true, want capability failures excluded")
		}
	}
	if got := table.Capacity("p1", provider.MediaImage, 4); got != 4 {
		t.Errorf("capacity = %d, want the untouched base capacity", got)
	}
}

func TestHealthTableOpensOnThresholdAndBlocksClaims(t *testing.T) {
	table, _ := testHealthTable(t)

	table.RecordFailure("p1", provider.MediaVideo, true)
	table.RecordFailure("p1", provider.MediaVideo, true)
	if !table.RecordFailure("p1", provider.MediaVideo, true) {
		t.Fatal("third transient failure = false, want the lane to trip")
	}
	if got := table.State("p1", provider.MediaVideo); got != LaneOpen {
		t.Fatalf("state = %s, want open", got)
	}
	if got := table.Capacity("p1", provider.MediaVideo, 3); got != 0 {
		t.Errorf("capacity = %d, want 0 while open", got)
	}
	blocked := table.BlockedProviders(provider.MediaVideo)
	if len(blocked) != 1 || blocked[0] != "p1" {
		t.Errorf("blocked = %v, want [p1]", blocked)
	}
	// blocked 按 media 分桶：别的 lane 不受影响。
	if blocked := table.BlockedProviders(provider.MediaImage); len(blocked) != 0 {
		t.Errorf("blocked(image) = %v, want none", blocked)
	}
}

func TestHealthTableCapacityNeverRaisesUnsupportedLanes(t *testing.T) {
	table, _ := testHealthTable(t)
	table.RecordFailure("p1", provider.MediaImage, true)
	table.RecordFailure("p1", provider.MediaImage, true)
	table.RecordFailure("p1", provider.MediaImage, true)

	if got := table.Capacity("p1", provider.MediaImage, 0); got != 0 {
		t.Errorf("capacity = %d, want 0 preserved for an unsupported lane", got)
	}
	if got := table.Capacity("p1", provider.MediaImage, -2); got != -2 {
		t.Errorf("capacity = %d, want the negative base preserved", got)
	}
}

func TestHealthTableHalfOpenProbeSuccessRestoresFullCapacity(t *testing.T) {
	table, clock := testHealthTable(t)
	for range 3 {
		table.RecordFailure("p1", provider.MediaVideo, true)
	}

	clock.advance(60)
	if got := table.State("p1", provider.MediaVideo); got != LaneHalfOpen {
		t.Fatalf("state = %s, want half_open once the cooldown elapses", got)
	}
	if got := table.Capacity("p1", provider.MediaVideo, 4); got != 1 {
		t.Errorf("capacity = %d, want a single probe slot", got)
	}
	if blocked := table.BlockedProviders(provider.MediaVideo); len(blocked) != 0 {
		t.Errorf("blocked = %v, want half-open lanes claimable as probes", blocked)
	}

	table.RecordSuccess("p1", provider.MediaVideo)
	if got := table.State("p1", provider.MediaVideo); got != LaneClosed {
		t.Fatalf("state = %s, want closed after a successful probe", got)
	}
	if got := table.Capacity("p1", provider.MediaVideo, 4); got != 4 {
		t.Errorf("capacity = %d, want the full base capacity back", got)
	}
}

func TestHealthTableHalfOpenProbeFailureDoublesCooldown(t *testing.T) {
	table, clock := testHealthTable(t)
	for range 3 {
		table.RecordFailure("p1", provider.MediaVideo, true)
	}
	clock.advance(60)

	if !table.RecordFailure("p1", provider.MediaVideo, true) {
		t.Fatal("failed half-open probe = false, want the lane re-tripped")
	}
	clock.advance(60)
	if got := table.State("p1", provider.MediaVideo); got != LaneOpen {
		t.Errorf("state = %s, want still open after only the first cooldown", got)
	}
	clock.advance(60)
	if got := table.State("p1", provider.MediaVideo); got != LaneHalfOpen {
		t.Errorf("state = %s, want half_open after the doubled cooldown", got)
	}

	snapshot := table.Snapshot()["p1:video"]
	if snapshot.OpenRounds != 2 {
		t.Errorf("open rounds = %d, want 2", snapshot.OpenRounds)
	}
}

func TestHealthTableCooldownIsCappedByMaxOpenSeconds(t *testing.T) {
	clock := &fakeClock{}
	table := NewProviderHealthTable(HealthSettings{FailureThreshold: 1, OpenSeconds: 60, MaxOpenSeconds: 120}, clock.read)

	table.RecordFailure("p1", provider.MediaAudio, true)
	clock.advance(120)
	table.RecordFailure("p1", provider.MediaAudio, true)
	clock.advance(120)
	table.RecordFailure("p1", provider.MediaAudio, true)

	snapshot := table.Snapshot()["p1:audio"]
	if snapshot.OpenRounds != 3 {
		t.Fatalf("open rounds = %d, want 3", snapshot.OpenRounds)
	}
	if snapshot.OpenSecondsRemaining > 120 {
		t.Errorf("cooldown = %v, want it capped at max_open_seconds", snapshot.OpenSecondsRemaining)
	}
	// 第四轮次本应 60*2^3=480s，封顶后只等 120s 就能进半开。
	clock.advance(120)
	if got := table.State("p1", provider.MediaAudio); got != LaneHalfOpen {
		t.Errorf("state = %s, want half_open at the capped cooldown", got)
	}
}

func TestHealthTableSnapshotIsEmptyWhileHealthy(t *testing.T) {
	table, _ := testHealthTable(t)

	if snapshot := table.Snapshot(); len(snapshot) != 0 {
		t.Errorf("snapshot = %v, want no entries for healthy lanes", snapshot)
	}
	table.RecordFailure("p1", provider.MediaImage, true)
	table.RecordSuccess("p1", provider.MediaImage)
	if snapshot := table.Snapshot(); len(snapshot) != 0 {
		t.Errorf("snapshot = %v, want the lane dropped after a success", snapshot)
	}
}

func TestHealthSettingsFallBackOnInvalidEnv(t *testing.T) {
	t.Setenv("PROVIDER_HEALTH_FAILURE_THRESHOLD", "not-a-number")
	// 负数不是「非法」而是会被夹到下限 0.1（与 provider_health.py 同口径），
	// 这里要验证的是「解析不了」的回退，故用不可解析的字符串。
	t.Setenv("PROVIDER_HEALTH_OPEN_SECONDS", "not-a-number")
	t.Setenv("PROVIDER_HEALTH_MAX_OPEN_SECONDS", "nan")

	settings := HealthSettingsFromEnv()
	want := DefaultHealthSettings()
	if settings.FailureThreshold != want.FailureThreshold {
		t.Errorf("failure threshold = %d, want the default %d", settings.FailureThreshold, want.FailureThreshold)
	}
	if settings.OpenSeconds != want.OpenSeconds {
		t.Errorf("open seconds = %v, want the default %v", settings.OpenSeconds, want.OpenSeconds)
	}
	if settings.MaxOpenSeconds != want.MaxOpenSeconds {
		t.Errorf("max open seconds = %v, want the default %v", settings.MaxOpenSeconds, want.MaxOpenSeconds)
	}
}

func TestHealthSettingsCapsCooldownBelowFirstRound(t *testing.T) {
	t.Setenv("PROVIDER_HEALTH_OPEN_SECONDS", "300")
	t.Setenv("PROVIDER_HEALTH_MAX_OPEN_SECONDS", "60")

	settings := HealthSettingsFromEnv()
	if settings.MaxOpenSeconds != 300 {
		t.Errorf("max open seconds = %v, want it raised to the first-round cooldown", settings.MaxOpenSeconds)
	}
}
