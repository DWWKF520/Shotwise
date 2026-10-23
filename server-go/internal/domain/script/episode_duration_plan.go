// 剧集时长规划（对应 lib/episode_duration_plan.py）。
//
// 规划器刻意把「创作目标」与「provider 可执行请求」分开：目标秒数只是规划请求，
// 不写回剧本的分镜时长；只有调用方显式确认后才落盘。locked / generated 的分镜恒被
// 保留且永不出现在 changes 里。
package script

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shotwise/shotwise/internal/domain/grid"
	"math"
	"sort"
	"strconv"
)

// DurationPlanningStrategy 是可调整分镜的分配策略。
type DurationPlanningStrategy string

const (
	StrategyEqual        DurationPlanningStrategy = "equal"
	StrategyProportional DurationPlanningStrategy = "proportional"
	StrategyManual       DurationPlanningStrategy = "manual"
)

// EpisodeDurationRevisionConflict 表示时长变更基于过期的剧本快照。
type EpisodeDurationRevisionConflict struct {
	Expected string
	Actual   string
}

func (e *EpisodeDurationRevisionConflict) Error() string { return "episode duration revision is stale" }

// episodeDurationRevisions 是规划器内部返回的错误文案（不面向终端用户）。
var (
	errTargetNotPositive = errors.New("target_seconds must be positive")
	errTargetBelowFixed  = errors.New("target_seconds cannot be less than the fixed shot duration total")
	errDuplicateShotID   = errors.New("shot_id values must be unique")
)

// EpisodeDurationRevision 返回一份剧本快照的稳定乐观并发修订号（sha256 十六进制）。
//
// 与 Python 侧 json.dumps(sort_keys=True, separators=(",", ":"), ensure_ascii=False)
// 的字节形态对齐；两套实现共写的剧本才能拿到同一个修订号。
func EpisodeDurationRevision(script map[string]any) string {
	canonical, err := canonicalPlanJSON(script)
	if err != nil {
		// 不可序列化的剧本本就不该进入规划器；退化为空串让上层按「无修订号」处理。
		return ""
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// canonicalPlanJSON 输出键排序、无空格、不转义 HTML 与非 ASCII 的紧凑 JSON。
func canonicalPlanJSON(value any) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// EpisodeDurationPlanConfig 是可持久化的剧集规划设置（与分镜时长解耦）。
type EpisodeDurationPlanConfig struct {
	TargetSeconds     int
	Strategy          DurationPlanningStrategy
	ManualAllocations map[string]int
}

// AsDict 返回持久化形态，键名与 Python 侧 as_dict 一致。
func (c EpisodeDurationPlanConfig) AsDict() map[string]any {
	manual := make(map[string]any, len(c.ManualAllocations))
	for k, v := range c.ManualAllocations {
		manual[k] = v
	}
	return map[string]any{
		"target_seconds":     c.TargetSeconds,
		"strategy":           string(c.Strategy),
		"manual_allocations": manual,
	}
}

// ReadEpisodeDurationPlan 读取兼容形态的规划配置，不修改入参；无配置返回 nil。
//
// 规范形态在 script.metadata.episode_duration_plan；episode_target_duration 与
// ad 项目既有的 project.target_duration 作为兼容回退，老项目无需迁移。畸形数据
// 一律忽略，把时长交还给既有生成路径。
func ReadEpisodeDurationPlan(project, script map[string]any) *EpisodeDurationPlanConfig {
	var metadata map[string]any
	if raw, ok := script["metadata"].(map[string]any); ok {
		metadata = raw
	}
	var rawPlan map[string]any
	if metadata != nil {
		if candidate, ok := metadata["episode_duration_plan"].(map[string]any); ok {
			rawPlan = candidate
		}
	}
	if rawPlan == nil {
		rawPlan = findLegacyDurationPlan(project, script, metadata)
	}
	if rawPlan == nil {
		return nil
	}
	return coercePlanConfig(rawPlan)
}

// findLegacyDurationPlan 查找不迁移的兼容字段。
func findLegacyDurationPlan(project, script, metadata map[string]any) map[string]any {
	episode, hasEpisode := script["episode"]
	if plans, ok := project["episode_duration_plans"].(map[string]any); ok && hasEpisode && episode != nil {
		if candidate, ok := plans[grid.PythonStr(episode)].(map[string]any); ok {
			return candidate
		}
	}
	if entries, ok := project["episodes"].([]any); ok {
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if !sameJSONScalar(entry["episode"], episode) {
				continue
			}
			if candidate, ok := entry["episode_duration_plan"].(map[string]any); ok {
				return candidate
			}
			if value, present := entry["episode_target_duration"]; present {
				return map[string]any{"target_seconds": value}
			}
		}
	}
	if value, present := script["episode_target_duration"]; present {
		return map[string]any{"target_seconds": value}
	}
	if metadata != nil {
		if value, present := metadata["episode_target_duration"]; present {
			return map[string]any{"target_seconds": value}
		}
	}
	if value, present := project["episode_target_duration"]; present {
		return map[string]any{"target_seconds": value}
	}
	if value, present := project["target_duration"]; present {
		return map[string]any{"target_seconds": value}
	}
	return nil
}

// coercePlanConfig 把原始 dict 收敛成配置；目标秒数或策略非法时返回 nil。
func coercePlanConfig(raw map[string]any) *EpisodeDurationPlanConfig {
	target, present := raw["target_seconds"]
	if !present {
		target = raw["episode_target_duration"]
	}
	seconds, ok := pythonInt(target)
	if !ok || seconds <= 0 {
		return nil
	}
	strategyRaw, present := raw["strategy"]
	if !present {
		strategyRaw = string(StrategyEqual)
	}
	strategyText, ok := strategyRaw.(string)
	if !ok {
		return nil
	}
	strategy := DurationPlanningStrategy(strategyText)
	switch strategy {
	case StrategyEqual, StrategyProportional, StrategyManual:
	default:
		return nil
	}
	manualRaw, present := raw["manual_allocations"]
	if !present || manualRaw == nil {
		manualRaw = map[string]any{}
	}
	manualMap, ok := manualRaw.(map[string]any)
	if !ok {
		return nil
	}
	manual := make(map[string]int, len(manualMap))
	for shotID, value := range manualMap {
		if shotID == "" {
			return nil
		}
		seconds, ok := pythonInt(value)
		if !ok || seconds <= 0 {
			return nil
		}
		manual[shotID] = seconds
	}
	return &EpisodeDurationPlanConfig{TargetSeconds: seconds, Strategy: strategy, ManualAllocations: manual}
}

// IsItemDurationLocked 返回分镜的时长锁定标记（duration_locked / locked 任一为 true）。
func IsItemDurationLocked(item map[string]any) bool {
	if locked, ok := item["duration_locked"].(bool); ok && locked {
		return true
	}
	if locked, ok := item["locked"].(bool); ok && locked {
		return true
	}
	return false
}

// IsItemVideoGenerated 判断分镜已有成片，不把规划时长当实际产物。
func IsItemVideoGenerated(item map[string]any) bool {
	assets, ok := item["generated_assets"].(map[string]any)
	if !ok {
		return false
	}
	if pyTruthyValue(assets["video_clip"]) || pyTruthyValue(assets["video_uri"]) {
		return true
	}
	status, _ := assets["status"].(string)
	return status == "completed"
}

// ShotDurationInput 是单个分镜的稳定规划视图（不拥有持久化数据）。
type ShotDurationInput struct {
	ShotID             string
	CurrentSeconds     *int
	SupportedDurations []int
	Locked             bool
	Generated          bool
	Weight             float64
}

// IsAdjustable 判断该分镜是否可被规划调整。
func (s ShotDurationInput) IsAdjustable() bool { return !s.Locked && !s.Generated }

func (s ShotDurationInput) weight() float64 {
	if s.Weight <= 0 {
		return 1.0
	}
	return s.Weight
}

// ShotDurationAllocation 是可解释的分配 + 请求夹取 + 未来实际结果槽位。
type ShotDurationAllocation struct {
	ShotID           string
	CurrentSeconds   *int
	AllocatedSeconds *int
	RequestedSeconds *int
	ActualSeconds    *int
	Locked           bool
	Generated        bool
	ClampReason      *string
}

// EpisodeDurationPlan 是不可变的预览产物，永不自施加于剧本。
type EpisodeDurationPlan struct {
	TargetSeconds  int
	Strategy       DurationPlanningStrategy
	Allocations    []ShotDurationAllocation
	SourceRevision *string
}

// ByShotID 按 shot_id 建索引。
func (p EpisodeDurationPlan) ByShotID() map[string]ShotDurationAllocation {
	out := make(map[string]ShotDurationAllocation, len(p.Allocations))
	for _, allocation := range p.Allocations {
		out[allocation.ShotID] = allocation
	}
	return out
}

// DurationChange 是计划产生的单条用户可确认变更。
type DurationChange struct {
	ShotID      string
	FromSeconds *int
	ToSeconds   int
	ClampReason *string
}

// DurationPlanPreview 是落盘前必须展示给用户的 diff。
type DurationPlanPreview struct {
	TargetSeconds int
	Changes       []DurationChange
}

// EpisodeDurationPlanner 把剧集目标分配到可调整分镜上。
type EpisodeDurationPlanner struct{}

// BuildPlan 构造规划。target 为正、shot_id 唯一、locked/generated 分镜的时长计入
// 固定总量且不参与分配；目标小于固定总量时报错。
func (EpisodeDurationPlanner) BuildPlan(
	targetSeconds int, shots []ShotDurationInput, strategy DurationPlanningStrategy,
	manualAllocations map[string]int, sourceRevision *string,
) (EpisodeDurationPlan, error) {
	if targetSeconds <= 0 {
		return EpisodeDurationPlan{}, errTargetNotPositive
	}
	if err := validateShots(shots); err != nil {
		return EpisodeDurationPlan{}, err
	}
	fixedTotal := 0
	adjustable := make([]ShotDurationInput, 0, len(shots))
	for _, shot := range shots {
		if shot.IsAdjustable() {
			adjustable = append(adjustable, shot)
			continue
		}
		if shot.CurrentSeconds != nil {
			fixedTotal += *shot.CurrentSeconds
		}
	}
	if targetSeconds < fixedTotal {
		return EpisodeDurationPlan{}, errTargetBelowFixed
	}
	remaining := targetSeconds - fixedTotal
	allocated, err := allocate(remaining, adjustable, strategy, manualAllocations)
	if err != nil {
		return EpisodeDurationPlan{}, err
	}
	allocations := make([]ShotDurationAllocation, 0, len(shots))
	for _, shot := range shots {
		seconds, present := allocated[shot.ShotID]
		var pointer *int
		if present {
			value := seconds
			pointer = &value
		}
		allocations = append(allocations, makeAllocation(shot, pointer))
	}
	return EpisodeDurationPlan{
		TargetSeconds:  targetSeconds,
		Strategy:       strategy,
		Allocations:    allocations,
		SourceRevision: sourceRevision,
	}, nil
}

// PreviewReplan 返回相对当前分镜时长的差异（locked / generated / 无请求的分镜不出现）。
func (EpisodeDurationPlanner) PreviewReplan(shots []ShotDurationInput, plan EpisodeDurationPlan) DurationPlanPreview {
	currentByID := make(map[string]ShotDurationInput, len(shots))
	for _, shot := range shots {
		currentByID[shot.ShotID] = shot
	}
	changes := []DurationChange{}
	for _, allocation := range plan.Allocations {
		shot, ok := currentByID[allocation.ShotID]
		if !ok || !shot.IsAdjustable() || allocation.RequestedSeconds == nil {
			continue
		}
		if shot.CurrentSeconds != nil && *shot.CurrentSeconds == *allocation.RequestedSeconds {
			continue
		}
		changes = append(changes, DurationChange{
			ShotID:      shot.ShotID,
			FromSeconds: shot.CurrentSeconds,
			ToSeconds:   *allocation.RequestedSeconds,
			ClampReason: allocation.ClampReason,
		})
	}
	return DurationPlanPreview{TargetSeconds: plan.TargetSeconds, Changes: changes}
}

// ApplyConfirmedPlan 只返回用户已确认的安全更新，由调用方原子落盘。
func (p EpisodeDurationPlanner) ApplyConfirmedPlan(shots []ShotDurationInput, plan EpisodeDurationPlan) map[string]int {
	updates := map[string]int{}
	for _, change := range p.PreviewReplan(shots, plan).Changes {
		updates[change.ShotID] = change.ToSeconds
	}
	return updates
}

// RequestedSecondsFor 返回单个分镜的 provider 就绪请求；计划里没有该分镜时返回 nil。
func (EpisodeDurationPlanner) RequestedSecondsFor(shotID string, plan EpisodeDurationPlan) *int {
	allocation, ok := plan.ByShotID()[shotID]
	if !ok {
		return nil
	}
	return allocation.RequestedSeconds
}

func validateShots(shots []ShotDurationInput) error {
	seen := make(map[string]bool, len(shots))
	for _, shot := range shots {
		if seen[shot.ShotID] {
			return errDuplicateShotID
		}
		seen[shot.ShotID] = true
		if shot.ShotID == "" {
			return errors.New("shot_id is required")
		}
		if shot.CurrentSeconds != nil && *shot.CurrentSeconds <= 0 {
			return errors.New("current_seconds must be positive when set")
		}
		if shot.Weight <= 0 {
			return errors.New("weight must be positive")
		}
		if len(shot.SupportedDurations) == 0 {
			return errors.New("supported_durations must not be empty")
		}
		for _, duration := range shot.SupportedDurations {
			if duration <= 0 {
				return errors.New("supported_durations must contain positive values")
			}
		}
	}
	return nil
}

// allocate 把剩余秒数分给可调整分镜：manual 直取，equal/proportional 按权重取整后
// 用余数补齐（稳定 tie-break 保留剧本顺序，保证预览可复现）。
func allocate(
	remaining int, shots []ShotDurationInput, strategy DurationPlanningStrategy, manual map[string]int,
) (map[string]int, error) {
	if len(shots) == 0 {
		return map[string]int{}, nil
	}
	if strategy == StrategyManual {
		missing := make([]string, 0)
		values := make(map[string]int, len(shots))
		for _, shot := range shots {
			value, ok := manual[shot.ShotID]
			if !ok {
				missing = append(missing, shot.ShotID)
				continue
			}
			if value <= 0 {
				return nil, errors.New("manual allocations must be positive")
			}
			values[shot.ShotID] = value
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("manual allocations are required for: %v", missing)
		}
		return values, nil
	}

	weights := make([]float64, len(shots))
	var totalWeight float64
	for i, shot := range shots {
		if strategy == StrategyEqual {
			weights[i] = 1.0
		} else {
			weights[i] = shot.weight()
		}
		totalWeight += weights[i]
	}
	raw := make([]float64, len(shots))
	floors := make([]int, len(shots))
	for i, weight := range weights {
		raw[i] = float64(remaining) * weight / totalWeight
		floors[i] = int(raw[i])
	}
	sumFloors := 0
	for _, value := range floors {
		sumFloors += value
	}
	leftovers := remaining - sumFloors
	indices := make([]int, len(shots))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(a, b int) bool {
		left, right := indices[a], indices[b]
		leftRemainder := raw[left] - float64(floors[left])
		rightRemainder := raw[right] - float64(floors[right])
		if leftRemainder != rightRemainder {
			return leftRemainder > rightRemainder
		}
		// 相同余数按原始顺序（-index 降序 == index 升序）优先。
		return left < right
	})
	if leftovers > len(indices) {
		leftovers = len(indices)
	}
	for _, index := range indices[:leftovers] {
		floors[index]++
	}
	out := make(map[string]int, len(shots))
	for i, shot := range shots {
		out[shot.ShotID] = floors[i]
	}
	return out, nil
}

func makeAllocation(shot ShotDurationInput, allocatedSeconds *int) ShotDurationAllocation {
	if !shot.IsAdjustable() {
		return ShotDurationAllocation{
			ShotID:           shot.ShotID,
			CurrentSeconds:   shot.CurrentSeconds,
			AllocatedSeconds: shot.CurrentSeconds,
			RequestedSeconds: shot.CurrentSeconds,
			Locked:           shot.Locked,
			Generated:        shot.Generated,
		}
	}
	if allocatedSeconds == nil {
		return ShotDurationAllocation{ShotID: shot.ShotID, CurrentSeconds: shot.CurrentSeconds}
	}
	requested, reason := clampToSupported(*allocatedSeconds, shot.SupportedDurations)
	return ShotDurationAllocation{
		ShotID:           shot.ShotID,
		CurrentSeconds:   shot.CurrentSeconds,
		AllocatedSeconds: allocatedSeconds,
		RequestedSeconds: requested,
		ClampReason:      reason,
	}
}

// clampToSupported 把分配值夹到 provider 支持的档位，并给出夹取原因。
func clampToSupported(target int, supported []int) (*int, *string) {
	if len(supported) == 0 {
		value := target
		return &value, nil
	}
	unique := map[int]bool{}
	values := make([]int, 0, len(supported))
	for _, duration := range supported {
		if unique[duration] {
			continue
		}
		unique[duration] = true
		values = append(values, duration)
	}
	sort.Ints(values)
	minValue, maxValue := values[0], values[len(values)-1]
	if target < minValue {
		value := minValue
		reason := "provider_min_duration"
		return &value, &reason
	}
	if target > maxValue {
		value := maxValue
		reason := "provider_max_duration"
		return &value, &reason
	}
	for _, value := range values {
		if value == target {
			copied := target
			return &copied, nil
		}
	}
	nearest := values[0]
	for _, value := range values {
		if math.Abs(float64(value-target)) < math.Abs(float64(nearest-target)) ||
			(math.Abs(float64(value-target)) == math.Abs(float64(nearest-target)) && value < nearest) {
			nearest = value
		}
	}
	value := nearest
	reason := "nearest_supported_duration"
	return &value, &reason
}

// ---- Python 语义助手 ----

// pythonInt 复刻 Python 的 int 判定：bool 不算 int，整数值的 float 算 int。
func pythonInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if v != math.Trunc(v) {
			return 0, false
		}
		return int(v), true
	case json.Number:
		parsed, err := strconv.ParseFloat(string(v), 64)
		if err != nil || parsed != math.Trunc(parsed) {
			return 0, false
		}
		return int(parsed), true
	}
	return 0, false
}

// sameJSONScalar 比较两个 JSON 标量（Python 的 == 语义）。
func sameJSONScalar(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftInt, leftOK := pythonInt(left)
	rightInt, rightOK := pythonInt(right)
	if leftOK && rightOK {
		return leftInt == rightInt
	}
	leftText, leftIsText := left.(string)
	rightText, rightIsText := right.(string)
	if leftIsText && rightIsText {
		return leftText == rightText
	}
	return false
}

// pyTruthyValue 复刻 Python 的真值判定（空串 / 0 / false / 空容器 / nil 为假）。
func pyTruthyValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}
