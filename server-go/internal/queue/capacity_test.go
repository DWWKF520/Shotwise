package queue

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/shotwise/shotwise/internal/infra/db"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/migrations"
)

// ----------------------------------------------------------------------
// 测试替身与 DB 夹具
// ----------------------------------------------------------------------

// fakeSlotTask 是 SlotTask 的测试替身：done/cancelled/err 可显式设置。
type fakeSlotTask struct {
	done      bool
	cancelled bool
	err       error
}

func (f *fakeSlotTask) Done() bool      { return f.done }
func (f *fakeSlotTask) Cancel()         { f.cancelled = true; f.done = true }
func (f *fakeSlotTask) Cancelled() bool { return f.cancelled }
func (f *fakeSlotTask) Err() error      { return f.err }

func pendingTask() *fakeSlotTask { return &fakeSlotTask{} }
func doneTask() *fakeSlotTask    { return &fakeSlotTask{done: true} }

// unsetEnv 清空环境变量并在用例结束时还原（对应 Python 的 monkeypatch.delenv）。
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		key := key
		if old, ok := os.LookupEnv(key); ok {
			t.Cleanup(func() { _ = os.Setenv(key, old) })
		}
		_ = os.Unsetenv(key)
	}
}

func newQueueTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, dialect, err := db.Open("sqlite://"+filepath.Join(t.TempDir(), "queue.db"), logger.Silent)
	if err != nil {
		t.Fatalf("open temp sqlite: %v", err)
	}
	if _, err := db.Migrate(gdb, dialect, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// tasks.user_id 有到 users 的外键；Python 侧运行时保证 default 用户存在。
	if err := gdb.Exec(
		"INSERT INTO users (id, username, role, is_active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		repository.DefaultUserID, "default", "user", true, "2026-01-01T00:00:00+00:00", "2026-01-01T00:00:00+00:00",
	).Error; err != nil {
		t.Fatalf("seed default user: %v", err)
	}
	t.Cleanup(func() {
		if pool, err := gdb.DB(); err == nil {
			_ = pool.Close()
		}
	})
	return gdb
}

func queueTestTime(t *testing.T) db.FlexibleTime {
	t.Helper()
	var ts db.FlexibleTime
	if err := ts.Scan("2026-01-01T00:00:00+00:00"); err != nil {
		t.Fatalf("parse time: %v", err)
	}
	return ts
}

func setProviderConfig(t *testing.T, gdb *gorm.DB, providerID, key, value string) {
	t.Helper()
	cfg := repository.ProviderConfig{Provider: providerID, Key: key, Value: value, UpdatedAt: queueTestTime(t)}
	if err := gdb.Create(&cfg).Error; err != nil {
		t.Fatalf("seed provider config %s/%s: %v", providerID, key, err)
	}
}

// addCustomProviderRow 写入一个启用的自定义供应商行，返回 DB 主键。
func addCustomProviderRow(t *testing.T, gdb *gorm.DB, image, video, audio *int64) int64 {
	t.Helper()
	prov := repository.CustomProvider{
		DisplayName:     "custom",
		BaseURL:         "https://example.test",
		APIKey:          "sk",
		IsEnabled:       true,
		ImageMaxWorkers: image,
		VideoMaxWorkers: video,
		AudioMaxWorkers: audio,
		CreatedAt:       queueTestTime(t),
		UpdatedAt:       queueTestTime(t),
	}
	if err := gdb.Create(&prov).Error; err != nil {
		t.Fatalf("create custom provider: %v", err)
	}
	return prov.ID
}

func addCustomModelRow(t *testing.T, gdb *gorm.DB, providerID int64, modelID, endpoint string) {
	t.Helper()
	model := repository.CustomProviderModel{
		ProviderID:  providerID,
		ModelID:     modelID,
		DisplayName: modelID,
		IsEnabled:   true,
		Endpoint:    endpoint,
		CreatedAt:   queueTestTime(t),
		UpdatedAt:   queueTestTime(t),
	}
	if err := gdb.Create(&model).Error; err != nil {
		t.Fatalf("create custom model: %v", err)
	}
}

func int64Ptr(v int64) *int64 { return &v }

// ----------------------------------------------------------------------
// CapacityTable
// ----------------------------------------------------------------------

func TestCapacityGetThreeStates(t *testing.T) {
	table := NewCapacityTable(
		map[string]map[string]int{"known": {laneImage: 4, laneVideo: 0}},
		map[string]int{laneImage: 5, laneVideo: 3},
	)
	// 已知 + lane 在表 → 登记值（含 0=不支持该 lane）
	if got := table.Get("known", provider.MediaImage); got != 4 {
		t.Errorf("known image = %d, want 4", got)
	}
	if got := table.Get("known", provider.MediaVideo); got != 0 {
		t.Errorf("known video = %d, want 0", got)
	}
	// 已知 + lane 不在表 → 0
	if got := table.Get("known", provider.MediaAudio); got != 0 {
		t.Errorf("known audio = %d, want 0", got)
	}
	// 完全未知 → 默认，且纯查询不写回
	if got := table.Get("unknown", provider.MediaImage); got != 5 {
		t.Errorf("unknown image = %d, want 5", got)
	}
	if got := table.Get("unknown", provider.MediaVideo); got != 3 {
		t.Errorf("unknown video = %d, want 3", got)
	}
	if _, ok := table.Limits()["unknown"]; ok {
		t.Error("unknown provider must not be written back into the limits table")
	}
}

func TestCapacityReplaceOnlySwapsNumbers(t *testing.T) {
	table := NewCapacityTable(
		map[string]map[string]int{"p": {laneImage: 3, laneVideo: 3}},
		map[string]int{laneImage: 5, laneVideo: 3},
	)
	table.Replace(map[string]map[string]int{"p": {laneImage: 9, laneVideo: 1}})
	if got := table.Get("p", provider.MediaImage); got != 9 {
		t.Errorf("image after replace = %d, want 9", got)
	}
	if got := table.Get("p", provider.MediaVideo); got != 1 {
		t.Errorf("video after replace = %d, want 1", got)
	}
	// 默认不受 replace 影响
	if got := table.Get("unknown", provider.MediaImage); got != 5 {
		t.Errorf("unknown image after replace = %d, want the untouched default 5", got)
	}
}

func TestCapacitySnapshotExposesRegisteredLanesAsCopies(t *testing.T) {
	table := NewCapacityTable(
		map[string]map[string]int{"p": {laneImage: 3, laneVideo: 0}},
		map[string]int{laneImage: 5, laneVideo: 3},
	)
	snap := table.Snapshot()
	if len(snap) != 1 || snap["p"][laneImage] != 3 || snap["p"][laneVideo] != 0 {
		t.Fatalf("snapshot = %v, want only the registered lanes of p", snap)
	}
	snap["p"][laneImage] = 99
	if got := table.Get("p", provider.MediaImage); got != 3 {
		t.Errorf("snapshot must be a copy; capacity mutated to %d", got)
	}
}

func TestLaneLimitsProjectsUnsupportedLanesToZero(t *testing.T) {
	got := laneLimits(map[string]bool{laneAudio: true}, 5, 3, 10)
	if got[laneImage] != 0 || got[laneVideo] != 0 || got[laneAudio] != 10 {
		t.Errorf("laneLimits = %v, want image/video 0 and audio 10", got)
	}
}

func TestLaneDefaultPrefersDeclared(t *testing.T) {
	meta := provider.ProviderMeta{DefaultConcurrency: map[string]int{laneVideo: 1}}
	if got := laneDefault(meta, laneVideo, 3); got != 1 {
		t.Errorf("declared video default = %d, want 1", got)
	}
	if got := laneDefault(meta, laneImage, 5); got != 5 {
		t.Errorf("undeclared image default = %d, want the global 5", got)
	}
}

func TestCapacityFromEnvDerivesSupportAndDefaults(t *testing.T) {
	unsetEnv(t, "IMAGE_MAX_WORKERS", "VIDEO_MAX_WORKERS", "AUDIO_MAX_WORKERS")
	table := CapacityTableFromEnv()
	globals := map[string]int{laneImage: 5, laneVideo: 3, laneAudio: 10}
	for _, providerID := range provider.ProviderIDs() {
		meta, _ := provider.ProviderMetaFor(providerID)
		if _, ok := table.Limits()[providerID]; !ok {
			t.Fatalf("provider %s missing from the capacity table", providerID)
		}
		supported := mediaTypeSet(meta.MediaTypes())
		for lane, globalDefault := range globals {
			switch {
			case !supported[lane]:
				if got := table.Get(providerID, provider.MediaType(lane)); got != 0 {
					t.Errorf("%s/%s = %d, want 0 for an unsupported lane", providerID, lane, got)
				}
			case meta.DefaultConcurrency[lane] == 0:
				// 未声明出厂默认的 lane 必须落到硬编码全局默认（不反推 meta，否则非重言）。
				if got := table.Get(providerID, provider.MediaType(lane)); got != globalDefault {
					t.Errorf("%s/%s = %d, want the global default %d", providerID, lane, got, globalDefault)
				}
			}
		}
	}
}

func TestCapacityFromEnvReadsEnv(t *testing.T) {
	t.Setenv("IMAGE_MAX_WORKERS", "7")
	t.Setenv("VIDEO_MAX_WORKERS", "2")
	table := CapacityTableFromEnv()
	// 未知 provider 的懒默认跟随 env
	if got := table.Get("unknown", provider.MediaImage); got != 7 {
		t.Errorf("unknown image = %d, want 7", got)
	}
	if got := table.Get("unknown", provider.MediaVideo); got != 2 {
		t.Errorf("unknown video = %d, want 2", got)
	}
}

func TestCapacityFromEnvDeclaredDefaultOutranksGlobalEnv(t *testing.T) {
	unsetEnv(t, "IMAGE_MAX_WORKERS")
	t.Setenv("VIDEO_MAX_WORKERS", "5")
	table := CapacityTableFromEnv()

	// Agnes 视频 lane 出厂钉死 1，活跃的全局 env 也不解除。
	if got := table.Get("agnes", provider.MediaVideo); got != 1 {
		t.Errorf("agnes video = %d, want the declared default 1", got)
	}
	// 声明不外溢到未声明的 image lane。
	if got := table.Get("agnes", provider.MediaImage); got != 5 {
		t.Errorf("agnes image = %d, want the global default 5", got)
	}
	// 未声明默认的视频供应商仍跟随全局 env。
	if got := table.Get("ark", provider.MediaVideo); got != 5 {
		t.Errorf("ark video = %d, want to follow the global env 5", got)
	}
}

func TestCapacityFromDBKnownProvidersAndUnsupportedLanesZero(t *testing.T) {
	gdb := newQueueTestDB(t)
	table, err := CapacityTableFromDB(context.Background(), gdb)
	if err != nil {
		t.Fatalf("CapacityTableFromDB: %v", err)
	}
	for _, providerID := range provider.ProviderIDs() {
		meta, _ := provider.ProviderMetaFor(providerID)
		if _, ok := table.Limits()[providerID]; !ok {
			t.Fatalf("provider %s missing from the capacity table", providerID)
		}
		supported := mediaTypeSet(meta.MediaTypes())
		if !supported[laneImage] && table.Get(providerID, provider.MediaImage) != 0 {
			t.Errorf("%s image = %d, want 0 for an unsupported lane", providerID, table.Get(providerID, provider.MediaImage))
		}
		if !supported[laneVideo] && table.Get(providerID, provider.MediaVideo) != 0 {
			t.Errorf("%s video = %d, want 0 for an unsupported lane", providerID, table.Get(providerID, provider.MediaVideo))
		}
	}
}

func TestCapacityFromDBCustomProviderColumnsUsed(t *testing.T) {
	gdb := newQueueTestDB(t)
	id := addCustomProviderRow(t, gdb, int64Ptr(2), int64Ptr(7), nil)
	addCustomModelRow(t, gdb, id, "img", "openai-images")
	addCustomModelRow(t, gdb, id, "vid", "newapi-video")

	table, err := CapacityTableFromDB(context.Background(), gdb)
	if err != nil {
		t.Fatalf("CapacityTableFromDB: %v", err)
	}
	pid := "custom-1"
	if got := table.Get(pid, provider.MediaImage); got != 2 {
		t.Errorf("custom image = %d, want the column value 2", got)
	}
	if got := table.Get(pid, provider.MediaVideo); got != 7 {
		t.Errorf("custom video = %d, want the column value 7", got)
	}
	// 不支持的 lane（无 audio 模型）投影为 0
	if got := table.Get(pid, provider.MediaAudio); got != 0 {
		t.Errorf("custom audio = %d, want 0 for an unsupported lane", got)
	}
}

func TestCapacityFromDBCustomProviderNullFallsBackToGlobalDefault(t *testing.T) {
	unsetEnv(t, "IMAGE_MAX_WORKERS", "VIDEO_MAX_WORKERS", "AUDIO_MAX_WORKERS")
	gdb := newQueueTestDB(t)
	id := addCustomProviderRow(t, gdb, nil, nil, nil)
	addCustomModelRow(t, gdb, id, "img", "openai-images")
	addCustomModelRow(t, gdb, id, "vid", "newapi-video")

	table, err := CapacityTableFromDB(context.Background(), gdb)
	if err != nil {
		t.Fatalf("CapacityTableFromDB: %v", err)
	}
	pid := "custom-1"
	// 全局默认 image=5 / video=3
	if got := table.Get(pid, provider.MediaImage); got != 5 {
		t.Errorf("custom image = %d, want the global default 5", got)
	}
	if got := table.Get(pid, provider.MediaVideo); got != 3 {
		t.Errorf("custom video = %d, want the global default 3", got)
	}
}

func TestCapacityFromDBDirtyValueFallsBackPerKey(t *testing.T) {
	for _, dirty := range []string{"", "3.7", "abc"} {
		t.Run(dirty, func(t *testing.T) {
			gdb := newQueueTestDB(t)
			setProviderConfig(t, gdb, "ark", "image_max_workers", dirty)
			setProviderConfig(t, gdb, "ark", "video_max_workers", "2")
			setProviderConfig(t, gdb, "gemini-aistudio", "image_max_workers", "7")

			table, err := CapacityTableFromDB(context.Background(), gdb)
			if err != nil {
				t.Fatalf("CapacityTableFromDB: %v", err)
			}
			// 脏 key 回退默认值（image=5）
			if got := table.Get("ark", provider.MediaImage); got != 5 {
				t.Errorf("ark image = %d, want the fallback default 5", got)
			}
			// 同 provider 其余合法 key、其他 provider 的配置正常生效
			if got := table.Get("ark", provider.MediaVideo); got != 2 {
				t.Errorf("ark video = %d, want 2", got)
			}
			if got := table.Get("gemini-aistudio", provider.MediaImage); got != 7 {
				t.Errorf("gemini image = %d, want 7", got)
			}
		})
	}
}

func TestCapacityFromDBNegativeValueClampsToZero(t *testing.T) {
	gdb := newQueueTestDB(t)
	setProviderConfig(t, gdb, "ark", "video_max_workers", "-1")

	table, err := CapacityTableFromDB(context.Background(), gdb)
	if err != nil {
		t.Fatalf("CapacityTableFromDB: %v", err)
	}
	if got := table.Get("ark", provider.MediaVideo); got != 0 {
		t.Errorf("ark video = %d, want 0 for a negative configured value", got)
	}
}

// ----------------------------------------------------------------------
// SlotTable
// ----------------------------------------------------------------------

func TestSlotRegisterCountsTowardOccupiedAndHasRoom(t *testing.T) {
	st := NewSlotTable()
	st.Register("p", provider.MediaImage, "t1", pendingTask(), false)
	if got := st.Occupied("p", provider.MediaImage); got != 1 {
		t.Errorf("occupied = %d, want 1", got)
	}
	if !st.HasRoom("p", provider.MediaImage, 2) {
		t.Error("has_room(cap=2) = false, want true")
	}
	if st.HasRoom("p", provider.MediaImage, 1) {
		t.Error("has_room(cap=1) = true, want false")
	}
	// capacity<=0 → 永远无空位
	if st.HasRoom("p", provider.MediaImage, 0) {
		t.Error("has_room(cap=0) = true, want false")
	}
}

func TestSlotPendingCountsTowardCapacity(t *testing.T) {
	st := NewSlotTable()
	st.Register("p", provider.MediaVideo, "t1", pendingTask(), true)
	if got := st.Occupied("p", provider.MediaVideo); got != 1 {
		t.Errorf("occupied = %d, want 1", got)
	}
	if st.HasRoom("p", provider.MediaVideo, 1) {
		t.Error("has_room(cap=1) = true, want false while pending occupies the slot")
	}
}

func TestSlotRegisterReleaseIdempotent(t *testing.T) {
	st := NewSlotTable()
	f1 := pendingTask()
	f2 := pendingTask()
	st.Register("p", provider.MediaImage, "t1", f1, false)
	st.Register("p", provider.MediaImage, "t1", f2, false) // 同 id 覆盖
	if got := st.Occupied("p", provider.MediaImage); got != 1 {
		t.Errorf("occupied = %d, want 1 after overwrite", got)
	}
	if st.FindByTask("t1") != SlotTask(f2) {
		t.Error("find_by_task must return the overwriting task")
	}
	// release 不存在 → no-op
	st.Release("p", provider.MediaImage, "ghost")
	if got := st.Occupied("p", provider.MediaImage); got != 1 {
		t.Errorf("occupied = %d, want unchanged 1", got)
	}
	// release 两次 → 幂等
	st.Release("p", provider.MediaImage, "t1")
	st.Release("p", provider.MediaImage, "t1")
	if got := st.Occupied("p", provider.MediaImage); got != 0 {
		t.Errorf("occupied = %d, want 0", got)
	}
}

func TestSlotCapacityChangeDoesNotDisturbOccupancy(t *testing.T) {
	st := NewSlotTable()
	for i := 0; i < 3; i++ {
		st.Register("p", provider.MediaVideo, string(rune('a'+i)), pendingTask(), false)
	}
	if st.HasRoom("p", provider.MediaVideo, 3) {
		t.Error("has_room(cap=3) = true, want false with 3 occupants")
	}
	// 容量 3→5 只是换一个实参，占用记账纹丝不动
	if !st.HasRoom("p", provider.MediaVideo, 5) {
		t.Error("has_room(cap=5) = false, want true")
	}
	// 缩容超额容忍：no room 但绝不驱逐在跑的 3 个
	if st.HasRoom("p", provider.MediaVideo, 1) {
		t.Error("has_room(cap=1) = true, want false")
	}
	if got := st.Occupied("p", provider.MediaVideo); got != 3 {
		t.Errorf("occupied = %d, want the 3 in-flight kept", got)
	}
	if got := len(st.AllActiveTasks()); got != 3 {
		t.Errorf("all_active_tasks = %d, want 3", got)
	}
}

func TestSlotPromoteFlipsPhaseWithoutChangingCount(t *testing.T) {
	st := NewSlotTable()
	done := doneTask()
	st.Register("p", provider.MediaVideo, "t1", done, true)
	// promote 前：done 的 PENDING 不被 drain
	if got := st.DrainFinished(); len(got) != 0 {
		t.Errorf("drain before promote = %v, want empty", got)
	}
	if got := st.Occupied("p", provider.MediaVideo); got != 1 {
		t.Errorf("occupied = %d, want 1", got)
	}
	st.Promote("p", provider.MediaVideo, "t1")
	if got := st.Occupied("p", provider.MediaVideo); got != 1 {
		t.Errorf("occupied = %d, want 1 (promote only flips the flag)", got)
	}
	// promote 后：done 的 INFLIGHT 被 drain
	drained := st.DrainFinished()
	if len(drained) != 1 || drained[0].TaskID != "t1" {
		t.Errorf("drain after promote = %v, want [t1]", drained)
	}
	if got := st.Occupied("p", provider.MediaVideo); got != 0 {
		t.Errorf("occupied = %d, want 0 after drain", got)
	}
	// promote 不存在 → no-op
	st.Promote("p", provider.MediaVideo, "ghost")
}

func TestSlotFindByTask(t *testing.T) {
	st := NewSlotTable()
	f := pendingTask()
	st.Register("p", provider.MediaImage, "t1", f, false)
	if st.FindByTask("t1") != SlotTask(f) {
		t.Error("find_by_task must return the registered task")
	}
	if st.FindByTask("ghost") != nil {
		t.Error("find_by_task(ghost) must return nil")
	}
}

func TestSlotDrainFinishedOnlyDoneInflight(t *testing.T) {
	st := NewSlotTable()
	doneInflight := doneTask()
	runningInflight := pendingTask()
	donePending := doneTask()
	st.Register("p", provider.MediaImage, "done", doneInflight, false)
	st.Register("p", provider.MediaImage, "running", runningInflight, false)
	st.Register("p", provider.MediaVideo, "queued", donePending, true)

	drained := st.DrainFinished()
	if len(drained) != 1 || drained[0].TaskID != "done" {
		t.Fatalf("drain_finished = %v, want only the done inflight task", drained)
	}
	// 未完成的 inflight 保留
	if st.FindByTask("running") != SlotTask(runningInflight) {
		t.Error("running inflight must be kept")
	}
	// done 但仍是 PENDING 的不被 drain
	if st.FindByTask("queued") != SlotTask(donePending) {
		t.Error("done-but-pending must be kept")
	}
}

func TestSlotActiveViewsAndClear(t *testing.T) {
	st := NewSlotTable()
	a := pendingTask()
	b := pendingTask()
	st.Register("p", provider.MediaImage, "a", a, false)
	st.Register("p", provider.MediaVideo, "b", b, true)
	ids := st.ActiveTaskIDs()
	if len(ids) != 2 || !ids["a"] || !ids["b"] {
		t.Errorf("active_task_ids = %v, want {a, b}", ids)
	}
	if got := len(st.AllActiveTasks()); got != 2 {
		t.Errorf("all_active_tasks = %d, want 2", got)
	}
	st.Clear()
	if got := len(st.ActiveTaskIDs()); got != 0 {
		t.Errorf("active_task_ids after clear = %v, want empty", got)
	}
	if got := len(st.AllActiveTasks()); got != 0 {
		t.Errorf("all_active_tasks after clear = %d, want 0", got)
	}
}

func TestSlotOccupiedProvidersByMediaAndEmptyBucketPruned(t *testing.T) {
	st := NewSlotTable()
	st.Register("p-img", provider.MediaImage, "i1", pendingTask(), false)
	st.Register("p-vid", provider.MediaVideo, "v1", pendingTask(), false)
	st.Register("p-both", provider.MediaImage, "i2", pendingTask(), false)
	st.Register("p-both", provider.MediaVideo, "v2", pendingTask(), false)

	// 按 media_type 分隔：image 占用不出现在 video 集合
	if got := providerSet(st.OccupiedProviders(provider.MediaImage)); !got["p-img"] || !got["p-both"] || len(got) != 2 {
		t.Errorf("occupied image providers = %v, want {p-img, p-both}", got)
	}
	if got := providerSet(st.OccupiedProviders(provider.MediaVideo)); !got["p-vid"] || !got["p-both"] || len(got) != 2 {
		t.Errorf("occupied video providers = %v, want {p-vid, p-both}", got)
	}

	// release 掉 p-vid 的最后一个 video 占用 → 该 provider 不再出现（空 bucket 剪除）
	st.Release("p-vid", provider.MediaVideo, "v1")
	if got := providerSet(st.OccupiedProviders(provider.MediaVideo)); got["p-vid"] || !got["p-both"] || len(got) != 1 {
		t.Errorf("after release video providers = %v, want {p-both}", got)
	}

	// drain 路径同样剪空 bucket
	st.Release("p-both", provider.MediaVideo, "v2")
	st.Register("p-both", provider.MediaVideo, "v3", doneTask(), false)
	st.DrainFinished()
	if providerSet(st.OccupiedProviders(provider.MediaVideo))["p-both"] {
		t.Error("p-both must be pruned from video providers after its last occupant drained")
	}
	// image lane 不受影响
	if got := providerSet(st.OccupiedProviders(provider.MediaImage)); !got["p-img"] || !got["p-both"] {
		t.Errorf("image providers = %v, want {p-img, p-both} untouched", got)
	}
}

func TestSlotSnapshotReportsOccupancyAndSkipsEmptyBuckets(t *testing.T) {
	st := NewSlotTable()
	st.Register("p", provider.MediaImage, "i1", pendingTask(), false)
	st.Register("p", provider.MediaVideo, "v1", pendingTask(), true)
	st.Register("p", provider.MediaVideo, "v2", pendingTask(), false)

	snap := st.Snapshot()
	if got := snap[laneKey{"p", provider.MediaImage}]; got.Occupied != 1 || got.Inflight != 1 || got.Pending != 0 {
		t.Errorf("image snapshot = %+v, want occupied 1 / inflight 1 / pending 0", got)
	}
	if got := snap[laneKey{"p", provider.MediaVideo}]; got.Occupied != 2 || got.Inflight != 1 || got.Pending != 1 {
		t.Errorf("video snapshot = %+v, want occupied 2 / inflight 1 / pending 1", got)
	}

	// 快照是拷贝：篡改返回值不影响真实占用
	mutated := snap[laneKey{"p", provider.MediaVideo}]
	mutated.Occupied = 999
	if got := st.Occupied("p", provider.MediaVideo); got != 2 {
		t.Errorf("occupied = %d, want the snapshot mutation not to leak back", got)
	}

	// 清空一个 bucket 后它不再出现在快照里
	st.Release("p", provider.MediaImage, "i1")
	if _, ok := st.Snapshot()[laneKey{"p", provider.MediaImage}]; ok {
		t.Error("an emptied bucket must not appear in the snapshot")
	}
	if len(st.Snapshot()) != 1 {
		t.Errorf("snapshot = %v, want only the video lane", st.Snapshot())
	}
}

func providerSet(providers []string) map[string]bool {
	out := make(map[string]bool, len(providers))
	for _, p := range providers {
		out[p] = true
	}
	return out
}
