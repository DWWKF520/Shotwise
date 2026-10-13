package queue

import (
	"context"
	"log/slog"
	"strconv"
	"sync"

	"gorm.io/gorm"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// 供应商并发容量表与占用台账（端口自 lib/generation_worker.py）。
//
// 拆成两件独立的东西：CapacityTable（上限，来自用户配置，纯标量）与 SlotTable（运行时占用，
// 承载 inflight/pending）。改并发只换前者的数字，后者纹丝不动。

// lane 名（与 provider.MediaType 的取值一致）。
const (
	laneImage = "image"
	laneVideo = "video"
	laneAudio = "audio"
)

// 全局默认并发（未知 provider 的懒默认 + 未声明默认的 lane 回退）。
const (
	defaultImageWorkers = 5
	defaultVideoWorkers = 3
	defaultAudioWorkers = 10
)

// CapacityTable 是 provider_id × media_type → 并发上限的纯标量配置表。
//
// 唯一真相来自 provider config。Get 三态语义区分「已知但不支持（0）」与「provider 未知
// （懒默认）」。
// 并发访问受 mu 保护：主循环读上限、reload 换整表、执行 goroutine 查上限并发发生。
// Python 侧靠单线程事件循环隐式串行化，Go 侧需显式加锁。
type CapacityTable struct {
	mu       sync.RWMutex
	limits   map[string]map[string]int // provider_id → {lane → 上限}
	defaults map[string]int            // 未知 provider 的懒默认
}

// NewCapacityTable 构造容量表；limits/defaults 为 nil 时用空表，避免调用方判空。
func NewCapacityTable(limits map[string]map[string]int, defaults map[string]int) *CapacityTable {
	if limits == nil {
		limits = map[string]map[string]int{}
	}
	if defaults == nil {
		defaults = map[string]int{}
	}
	return &CapacityTable{limits: limits, defaults: defaults}
}

// Get 返回 (provider, media) 的并发上限。
//
//   - provider 已知 + lane 在表 → 登记值（可能 0=不支持该 lane）
//   - provider 已知 + lane 不在表 → 0（不支持）
//   - provider 整个未知 → defaults[media]（纯查询，不写回表；缺失为 0）
func (t *CapacityTable) Get(providerID string, media provider.MediaType) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	lanes, ok := t.limits[providerID]
	if !ok {
		return t.defaults[string(media)]
	}
	return lanes[string(media)]
}

// Replace 整表换数字（reload 入口）。占用台账与默认值不受影响。
func (t *CapacityTable) Replace(newLimits map[string]map[string]int) {
	if newLimits == nil {
		newLimits = map[string]map[string]int{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.limits = newLimits
}

// Limits 返回内部 lane 表（只读；调用方不得改写）。
func (t *CapacityTable) Limits() map[string]map[string]int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.limits
}

// Snapshot 返回只读容量快照：provider_id → {media_type: 上限}（逐 lane 浅拷贝）。
//
// 快照刻意不含 defaults（未知 provider 的懒默认）——诊断视图只关心已登记的泳道。
func (t *CapacityTable) Snapshot() map[string]map[string]int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]map[string]int, len(t.limits))
	for providerID, lanes := range t.limits {
		copied := make(map[string]int, len(lanes))
		for lane, limit := range lanes {
			copied[lane] = limit
		}
		out[providerID] = copied
	}
	return out
}

// laneLimits 按 provider 支持的媒体集合把上限投影成 lane 字典；不支持的 lane → 0。
//
// 容量装载的单一映射点：新增 lane 时在这里加一行即可。
func laneLimits(supported map[string]bool, image, video, audio int) map[string]int {
	return map[string]int{
		laneImage: pickLimit(supported, laneImage, image),
		laneVideo: pickLimit(supported, laneVideo, video),
		laneAudio: pickLimit(supported, laneAudio, audio),
	}
}

func pickLimit(supported map[string]bool, lane string, limit int) int {
	if supported[lane] {
		return limit
	}
	return 0
}

// laneDefault 返回某条 lane 在用户未配时的回退默认：供应商注册表声明默认（若有）→ 否则全局默认。
//
// 三层回退的中间层单点：fromEnv / fromDB 共用，避免两路漂移。
func laneDefault(meta provider.ProviderMeta, lane string, globalDefault int) int {
	if declared, ok := meta.DefaultConcurrency[lane]; ok {
		return declared
	}
	return globalDefault
}

func mediaTypeSet(mediaTypes []provider.MediaType) map[string]bool {
	out := make(map[string]bool, len(mediaTypes))
	for _, media := range mediaTypes {
		out[string(media)] = true
	}
	return out
}

// glomLimitEnvs 读三档全局默认（IMAGE/VIDEO/AUDIO_MAX_WORKERS），DB 不可用前与测试用。
func glomLimitEnvs() (image, video, audio int) {
	return readHealthIntEnv("IMAGE_MAX_WORKERS", defaultImageWorkers, 1),
		readHealthIntEnv("VIDEO_MAX_WORKERS", defaultVideoWorkers, 1),
		readHealthIntEnv("AUDIO_MAX_WORKERS", defaultAudioWorkers, 1)
}

// CapacityTableFromEnv 从环境变量 / 默认值构造容量表（DB 不可用前或测试用）。
func CapacityTableFromEnv() *CapacityTable {
	imageMax, videoMax, audioMax := glomLimitEnvs()
	limits := make(map[string]map[string]int, len(provider.ProviderIDs()))
	for _, providerID := range provider.ProviderIDs() {
		meta, _ := provider.ProviderMetaFor(providerID)
		// 无用户配置的装载路径：每条 lane 取声明默认（若有）→ 否则全局默认
		limits[providerID] = laneLimits(
			mediaTypeSet(meta.MediaTypes()),
			laneDefault(meta, laneImage, imageMax),
			laneDefault(meta, laneVideo, videoMax),
			laneDefault(meta, laneAudio, audioMax),
		)
	}
	return NewCapacityTable(limits, map[string]int{laneImage: imageMax, laneVideo: videoMax, laneAudio: audioMax})
}

// parseLaneMax 逐 key 容错解析单条 lane 的并发上限。
//
// 解析失败回退默认值并告警，不让单个坏值（写入校验上线前的存量脏数据）拖垮整表加载；
// 可解析的负数沿用 clamp 语义（→0，即该 lane fail-fast）并告警。
func parseLaneMax(cfg map[string]string, key string, def int, providerID string) int {
	raw, ok := cfg[key]
	if !ok {
		return def
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		slog.Warn("供应商 lane 配置值非法，回退默认值", "provider", providerID, "key", key, "value", raw, "default", def)
		return def
	}
	if parsed < 0 {
		slog.Warn("供应商 lane 配置值为负，按 0 处理（该 lane 关闭）", "provider", providerID, "key", key, "value", raw)
	}
	return max(0, parsed)
}

// CapacityTableFromDB 从 ConfigService + provider 注册表 + 自定义供应商加载容量表。
func CapacityTableFromDB(ctx context.Context, gdb *gorm.DB) (*CapacityTable, error) {
	defaultImage, defaultVideo, defaultAudio := glomLimitEnvs()

	allConfigs, err := config.NewConfigService(gdb).AllProviderConfigs(ctx)
	if err != nil {
		return nil, err
	}

	limits := make(map[string]map[string]int, len(provider.ProviderIDs()))
	for _, providerID := range provider.ProviderIDs() {
		meta, _ := provider.ProviderMetaFor(providerID)
		cfg := allConfigs[providerID]
		// 用户未配某条 lane 时，parseLaneMax 回退到此处给的 default——即声明默认（若有）→
		// 否则全局默认；用户配了则解析值覆盖之。
		imageMax := parseLaneMax(cfg, "image_max_workers", laneDefault(meta, laneImage, defaultImage), providerID)
		videoMax := parseLaneMax(cfg, "video_max_workers", laneDefault(meta, laneVideo, defaultVideo), providerID)
		audioMax := parseLaneMax(cfg, "audio_max_workers", laneDefault(meta, laneAudio, defaultAudio), providerID)
		limits[providerID] = laneLimits(mediaTypeSet(meta.MediaTypes()), imageMax, videoMax, audioMax)
	}

	customLimits, err := customProviderCapacity(ctx, gdb, defaultImage, defaultVideo, defaultAudio)
	if err != nil {
		return nil, err
	}
	for providerID, lanes := range customLimits {
		limits[providerID] = lanes
	}

	slog.Info("从 DB 加载供应商容量表", "limits", limits)
	return NewCapacityTable(limits, map[string]int{laneImage: defaultImage, laneVideo: defaultVideo, laneAudio: defaultAudio}), nil
}

// customProviderCapacity 装载自定义供应商的容量：两层回退（列有值取列值，列为 NULL 走全局默认），
// 投影仍交给 laneLimits 统一处理不支持的 lane。
func customProviderCapacity(
	ctx context.Context, gdb *gorm.DB, defaultImage, defaultVideo, defaultAudio int,
) (map[string]map[string]int, error) {
	var providers []repository.CustomProvider
	if err := gdb.WithContext(ctx).Where("is_enabled = ?", true).Order("id").Find(&providers).Error; err != nil {
		return nil, err
	}

	out := make(map[string]map[string]int, len(providers))
	for i := range providers {
		prov := providers[i]
		var models []repository.CustomProviderModel
		if err := gdb.WithContext(ctx).
			Where("provider_id = ? AND is_enabled = ?", prov.ID, true).Find(&models).Error; err != nil {
			return nil, err
		}
		mediaTypes := make(map[string]bool, len(models))
		for j := range models {
			media, err := config.EndpointMediaType(models[j].Endpoint)
			if err != nil {
				// 未知 endpoint（写入校验上线前的脏数据）：跳过该行，不拖垮整表。
				continue
			}
			mediaTypes[media] = true
		}
		out[config.MakeProviderID(prov.ID)] = laneLimits(
			mediaTypes,
			clampNullableLimit(prov.ImageMaxWorkers, defaultImage),
			clampNullableLimit(prov.VideoMaxWorkers, defaultVideo),
			clampNullableLimit(prov.AudioMaxWorkers, defaultAudio),
		)
	}
	return out, nil
}

// clampNullableLimit 把 DB 可空列读成上限：NULL 走全局默认，非空取列值。
func clampNullableLimit(value *int64, fallback int) int {
	if value == nil {
		return fallback
	}
	return int(*value)
}

// ----------------------------------------------------------------------
// 占用台账
// ----------------------------------------------------------------------

// SlotTask 是占用台账记录的执行体句柄。
//
// Done 报告是否结束；Cancel 请求取消（幂等）；Cancelled 报告是否因取消而结束；
// Err 返回执行错误（未结束或成功时为 nil）。
type SlotTask interface {
	Done() bool
	Cancel()
	Cancelled() bool
	Err() error
}

// slotOccupant 是一条占用：执行体 + phase 标志。
//
// pending=true 仅由 video 的 sem-throttled dispatcher 在子任务排队期产生；image / audio
// 无 sem dispatcher（audio 后端同步），一律 pending=false。promote 只翻这个标志（天然原子），
// 避免「两层容器间瞬时既不在 pending 也不在 inflight」的窗口。
type slotOccupant struct {
	task    SlotTask
	pending bool
}

// FinishedSlot 是 drain 出来的已完成占用。
type FinishedSlot struct {
	TaskID string
	Task   SlotTask
}

// SlotTable 是 (provider_id, media_type) → {task_id: occupant} 的占用台账。
//
// 被动纯内存数据结构，容量无关：HasRoom 由 caller 传入 capacity。不写 DB、不解析 provider、
// 不决定孤儿策略、不碰状态机守卫。
//
// 空 bucket 不残留（by design）：Release / DrainFinished 移除最后一个占用时一并删掉该
// (provider, media) bucket，保证 OccupiedProviders 永不返回已清空的 provider（池满黑名单
// 决策的支点）。
// 并发访问受 mu 保护：主循环 claim/drain、执行 goroutine 释放、API 侧 cancel 查找并发发生。
type SlotTable struct {
	mu    sync.Mutex
	slots map[laneKey]map[string]*slotOccupant
}

// NewSlotTable 构造空占用台账。
func NewSlotTable() *SlotTable {
	return &SlotTable{slots: map[laneKey]map[string]*slotOccupant{}}
}

// Register 登记占用；幂等覆盖。bucket 不存在时自动创建。
func (t *SlotTable) Register(providerID string, media provider.MediaType, taskID string, task SlotTask, pending bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := laneKey{providerID, media}
	bucket := t.slots[key]
	if bucket == nil {
		bucket = map[string]*slotOccupant{}
		t.slots[key] = bucket
	}
	bucket[taskID] = &slotOccupant{task: task, pending: pending}
}

// Promote PENDING→INFLIGHT：sem 获取成功后调用，只翻 pending 标志。不存在则 no-op。
func (t *SlotTable) Promote(providerID string, media provider.MediaType, taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	bucket := t.slots[laneKey{providerID, media}]
	if bucket == nil {
		return
	}
	if occ := bucket[taskID]; occ != nil {
		occ.pending = false
	}
}

// Release 释放，不论 phase；幂等；清空后移除该 (provider, media) bucket。
func (t *SlotTable) Release(providerID string, media provider.MediaType, taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := laneKey{providerID, media}
	bucket := t.slots[key]
	if bucket == nil {
		return
	}
	delete(bucket, taskID)
	if len(bucket) == 0 {
		delete(t.slots, key)
	}
}

// HasRoom 返回 capacity>0 且 占用数（含 pending）< capacity。
func (t *SlotTable) HasRoom(providerID string, media provider.MediaType, capacity int) bool {
	if capacity <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	bucket := t.slots[laneKey{providerID, media}]
	return len(bucket) < capacity
}

// Occupied 返回当前占用数（含 pending）。
func (t *SlotTable) Occupied(providerID string, media provider.MediaType) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.slots[laneKey{providerID, media}])
}

// OccupiedProviders 返回该 media 下有占用（≥1）的 provider；空 bucket 不计（黑名单源，含未知 provider）。
func (t *SlotTable) OccupiedProviders(media provider.MediaType) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.slots))
	for key, bucket := range t.slots {
		if key.MediaType == media && len(bucket) > 0 {
			out = append(out, key.ProviderID)
		}
	}
	return out
}

// SlotSnapshotLane 是单条泳道的占用计数。
type SlotSnapshotLane struct {
	Occupied int `json:"occupied"`
	Inflight int `json:"inflight"`
	Pending  int `json:"pending"`
}

// Snapshot 返回只读占用快照：(provider, media) → {occupied, inflight, pending}。
//
// pending 是 video sem-throttled dispatcher 已登记、尚未拿到 sem 的子任务；occupied =
// inflight + pending，与 HasRoom 的计数口径一致。纯诊断，不参与调度决策。
func (t *SlotTable) Snapshot() map[laneKey]SlotSnapshotLane {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[laneKey]SlotSnapshotLane, len(t.slots))
	for key, bucket := range t.slots {
		if len(bucket) == 0 {
			continue
		}
		pending := 0
		for _, occ := range bucket {
			if occ.pending {
				pending++
			}
		}
		out[key] = SlotSnapshotLane{Occupied: len(bucket), Inflight: len(bucket) - pending, Pending: pending}
	}
	return out
}

// FindByTask 跨全表按 task_id 找执行体（cancel 用）；未命中返回 nil。
func (t *SlotTable) FindByTask(taskID string) SlotTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, bucket := range t.slots {
		if occ := bucket[taskID]; occ != nil {
			return occ.task
		}
	}
	return nil
}

// DrainFinished 移除并返回所有 done 的 INFLIGHT 占用（pending 不动）。
func (t *SlotTable) DrainFinished() []FinishedSlot {
	t.mu.Lock()
	defer t.mu.Unlock()
	finished := make([]FinishedSlot, 0)
	for key, bucket := range t.slots {
		var doneIDs []string
		for taskID, occ := range bucket {
			if !occ.pending && occ.task.Done() {
				doneIDs = append(doneIDs, taskID)
			}
		}
		for _, taskID := range doneIDs {
			finished = append(finished, FinishedSlot{TaskID: taskID, Task: bucket[taskID].task})
			delete(bucket, taskID)
		}
		if len(bucket) == 0 {
			delete(t.slots, key)
		}
	}
	return finished
}

// ActiveTaskIDs 返回所有占用的 task_id（pending+inflight）：self-active 扫描用。
func (t *SlotTable) ActiveTaskIDs() map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]bool)
	for _, bucket := range t.slots {
		for taskID := range bucket {
			out[taskID] = true
		}
	}
	return out
}

// AllActiveTasks 返回所有占用的执行体（pending+inflight）：shutdown wait 用。
func (t *SlotTable) AllActiveTasks() []SlotTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SlotTask, 0)
	for _, bucket := range t.slots {
		for _, occ := range bucket {
			out = append(out, occ.task)
		}
	}
	return out
}

// Clear 清空全表（shutdown 收尾）。
func (t *SlotTable) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.slots = map[laneKey]map[string]*slotOccupant{}
}
