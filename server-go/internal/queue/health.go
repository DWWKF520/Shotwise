package queue

import (
	"log/slog"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/shotwise/shotwise/internal/provider"
)

// 供应商泳道健康度（端口自 lib/provider_health.py）：连续瞬态失败熔断泳道，半开探针恢复，
// 闭合期逐次降并发。
//
// 一条泳道 = 一个 (provider_id, media_type)。状态纯内存、不落库：进程重启即清零，
// 与「单 lease 单活 worker」的部署形态一致。
//
// 与 CapacityTable 正交：容量表是用户配置的并发上限，本表是在其之上的健康折减。
// 折减只收紧不放开——Capacity 的返回值永远不超过传入的上限；只要上限本身 >= 1，
// 折减结果也永远 >= 1（OPEN 除外）。熔断只推迟认领，不改变「该泳道是否支持该媒体类型」
// 这一配置事实（那是容量表 0 的语义），两者由调用方分开处理。

// LaneHealthState 是泳道健康状态。
type LaneHealthState string

const (
	// LaneClosed 正常：有效并发 = 上限 - 连续瞬态失败数，下限 1；任一成功即复原满血。
	LaneClosed LaneHealthState = "closed"
	// LaneOpen 熔断：有效并发 0，泳道进 claim 黑名单，任务留在 queued。
	LaneOpen LaneHealthState = "open"
	// LaneHalfOpen 冷却到期：有效并发压到 1，放一个真实任务当探针。
	LaneHalfOpen LaneHealthState = "half_open"
)

// HealthSettings 是熔断参数，三项都可由环境变量覆盖，调参不需要数据库迁移。
type HealthSettings struct {
	FailureThreshold int
	OpenSeconds      float64
	MaxOpenSeconds   float64
}

// DefaultHealthSettings 是与 Python 默认值同口径的熔断参数。
func DefaultHealthSettings() HealthSettings {
	return HealthSettings{FailureThreshold: 3, OpenSeconds: 60, MaxOpenSeconds: 600}
}

// HealthSettingsFromEnv 读环境变量覆盖熔断参数；非法值回退默认并告警，
// 构造期不因配置错误让 worker 起不来。
func HealthSettingsFromEnv() HealthSettings {
	settings := DefaultHealthSettings()
	settings.FailureThreshold = readHealthIntEnv(
		"PROVIDER_HEALTH_FAILURE_THRESHOLD", settings.FailureThreshold, 1)
	settings.OpenSeconds = readHealthFloatEnv("PROVIDER_HEALTH_OPEN_SECONDS", settings.OpenSeconds)
	// 环境变量可能把封顶值配到小于首轮冷却——取大者而不是报错。
	settings.MaxOpenSeconds = math.Max(
		readHealthFloatEnv("PROVIDER_HEALTH_MAX_OPEN_SECONDS", settings.MaxOpenSeconds), settings.OpenSeconds)
	return settings
}

func readHealthIntEnv(name string, fallback, minimum int) int {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		slog.Warn("环境变量取值非法，回退默认值", "name", name, "value", raw, "default", fallback)
		return fallback
	}
	return max(minimum, value)
}

func readHealthFloatEnv(name string, fallback float64) float64 {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		slog.Warn("环境变量取值非法，回退默认值", "name", name, "value", raw, "default", fallback)
		return fallback
	}
	return math.Max(0.1, value)
}

// laneHealth 是单条泳道的健康账；openUntil 为 0 表示从未熔断。
type laneHealth struct {
	consecutiveFailures int
	openUntil           float64
	openRounds          int
}

// LaneHealthSnapshot 是非满血泳道的只读快照（诊断 / 测试用）。
type LaneHealthSnapshot struct {
	State                LaneHealthState `json:"state"`
	ConsecutiveFailures  int             `json:"consecutive_failures"`
	OpenRounds           int             `json:"open_rounds"`
	OpenSecondsRemaining float64         `json:"open_seconds_remaining"`
}

// ProviderHealthTable 是 (provider_id, media_type) → 健康状态的内存表。
//
// 被动数据结构：不写 DB、不解析 provider、不决定任务终态。调用方负责把 Capacity 的结果
// 当作并发上限用，把 BlockedProviders 的结果并进 claim 黑名单。
// 并发访问受 mu 保护：主循环查熔断黑名单 / 有效容量，执行 goroutine 记成败，两路并发发生。
// Python 侧靠单线程事件循环隐式串行化，Go 侧需显式加锁。
type ProviderHealthTable struct {
	mu       sync.Mutex
	settings HealthSettings
	clock    func() float64
	lanes    map[laneKey]*laneHealth
}

type laneKey struct {
	ProviderID string
	MediaType  provider.MediaType
}

// NewProviderHealthTable 构造健康表；clock 为 nil 时用进程单调时钟。
func NewProviderHealthTable(settings HealthSettings, clock func() float64) *ProviderHealthTable {
	if clock == nil {
		start := time.Now()
		clock = func() float64 { return time.Since(start).Seconds() }
	}
	if settings.FailureThreshold < 1 {
		settings.FailureThreshold = 1
	}
	if settings.OpenSeconds <= 0 {
		settings.OpenSeconds = DefaultHealthSettings().OpenSeconds
	}
	if settings.MaxOpenSeconds < settings.OpenSeconds {
		settings.MaxOpenSeconds = settings.OpenSeconds
	}
	return &ProviderHealthTable{settings: settings, clock: clock, lanes: map[laneKey]*laneHealth{}}
}

// State 返回当前状态；OPEN 冷却到期后自动读作 HALF_OPEN（不落写，纯函数）。
func (t *ProviderHealthTable) State(providerID string, mediaType provider.MediaType) LaneHealthState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stateLocked(providerID, mediaType)
}

func (t *ProviderHealthTable) stateLocked(providerID string, mediaType provider.MediaType) LaneHealthState {
	lane := t.lanes[laneKey{providerID, mediaType}]
	if lane == nil || lane.openUntil <= 0 {
		return LaneClosed
	}
	if t.clock() < lane.openUntil {
		return LaneOpen
	}
	return LaneHalfOpen
}

// Capacity 返回健康折减后的有效并发上限。
//
// baseCapacity <= 0 原样返回：「该泳道不支持该媒体类型」由容量表定义，健康表不越权把 0
// 改成 1（否则不支持的泳道会拿到一个探针槽，任务从 fail-fast 变成真发上游）。
func (t *ProviderHealthTable) Capacity(providerID string, mediaType provider.MediaType, baseCapacity int) int {
	if baseCapacity <= 0 {
		return baseCapacity
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.stateLocked(providerID, mediaType) {
	case LaneOpen:
		return 0
	case LaneHalfOpen:
		return 1
	default:
		// 无账目 = 从未失败过，满血通过；有账目但未达阈值才逐次收紧。
		failures := 0
		if lane := t.lanes[laneKey{providerID, mediaType}]; lane != nil {
			failures = lane.consecutiveFailures
		}
		return max(1, baseCapacity-failures)
	}
}

// BlockedProviders 返回该媒体类型上熔断打开（OPEN）的 provider 集合——黑名单源，与占用无关。
// HALF_OPEN 不入内：它要放一个探针任务，把整个 provider 排掉就永远探不出来。
func (t *ProviderHealthTable) BlockedProviders(mediaType provider.MediaType) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	blocked := make([]string, 0, len(t.lanes))
	for key := range t.lanes {
		if key.MediaType != mediaType {
			continue
		}
		if t.stateLocked(key.ProviderID, key.MediaType) == LaneOpen {
			blocked = append(blocked, key.ProviderID)
		}
	}
	return blocked
}

// Snapshot 返回非满血泳道的只读快照，键为 "provider:media"。
func (t *ProviderHealthTable) Snapshot() map[string]LaneHealthSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	out := make(map[string]LaneHealthSnapshot, len(t.lanes))
	for key, lane := range t.lanes {
		remaining := 0.0
		if lane.openUntil > 0 {
			remaining = math.Max(0, lane.openUntil-now)
		}
		out[key.ProviderID+":"+string(key.MediaType)] = LaneHealthSnapshot{
			State:                t.stateLocked(key.ProviderID, key.MediaType),
			ConsecutiveFailures:  lane.consecutiveFailures,
			OpenRounds:           lane.openRounds,
			OpenSecondsRemaining: remaining,
		}
	}
	return out
}

// RecordSuccess 记一次成功：任一成功（含半开探针）都把泳道恢复满血——连续失败与熔断轮次清零。
func (t *ProviderHealthTable) RecordSuccess(providerID string, mediaType provider.MediaType) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.lanes[laneKey{providerID, mediaType}]; ok {
		delete(t.lanes, laneKey{providerID, mediaType})
		slog.Info("供应商泳道恢复满血", "provider", providerID, "media", mediaType)
	}
}

// RecordFailure 记一次失败，返回本次是否触发 / 维持了熔断。
//
// transient=false（能力不支持、参数非法、本地资产缺失……）不计健康账：这些失败与上游
// 可用性无关，计进去只会让泳道被无关错误熔断。
func (t *ProviderHealthTable) RecordFailure(providerID string, mediaType provider.MediaType, transient bool) bool {
	if !transient {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := laneKey{providerID, mediaType}
	lane := t.lanes[key]
	if lane == nil {
		lane = &laneHealth{}
		t.lanes[key] = lane
	}
	now := t.clock()
	if t.stateLocked(providerID, mediaType) == LaneHalfOpen {
		t.trip(lane, now)
		slog.Warn("供应商泳道半开探针失败，重新熔断",
			"provider", providerID, "media", mediaType, "cooldown", lane.openUntil-now, "rounds", lane.openRounds)
		return true
	}
	lane.consecutiveFailures++
	if lane.consecutiveFailures < t.settings.FailureThreshold {
		return false
	}
	t.trip(lane, now)
	slog.Warn("供应商泳道连续瞬态失败熔断",
		"provider", providerID, "media", mediaType, "cooldown", lane.openUntil-now, "rounds", lane.openRounds)
	return true
}

// trip 打开熔断：冷却按 2 的幂次翻倍，封顶 MaxOpenSeconds。
func (t *ProviderHealthTable) trip(lane *laneHealth, now float64) {
	lane.openRounds++
	delay := math.Min(
		t.settings.OpenSeconds*math.Pow(2, float64(lane.openRounds-1)),
		t.settings.MaxOpenSeconds,
	)
	lane.openUntil = now + delay
	lane.consecutiveFailures = 0
}
