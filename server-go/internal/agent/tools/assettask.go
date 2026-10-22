package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
)

// PatchTableKinds 是资产表名到资产类型的映射（与 Python _TABLES 同源）。
var PatchTableKinds = map[string]domain.AssetKind{
	"characters": domain.AssetCharacter,
	"scenes":     domain.AssetScene,
	"props":      domain.AssetProp,
	"products":   domain.AssetProduct,
}

const (
	assetPollInterval = 1 * time.Second
	// assetWorkerOfflineGrace 是等待期间 worker 离线的容忍时长，超过即失败（与 Python WorkerOfflineError 对齐）。
	assetWorkerOfflineGrace = 30 * time.Second
	// assetDefaultLease 是 worker 租约名（与 queue.DefaultLeaseName 一致）。
	assetDefaultLease = "default"
)

// AwaitAssetTask 轮询任务至终态；worker 离线超过宽限期即失败。ctx 取消立即返回。
func AwaitAssetTask(ctx context.Context, q AssetQueue, taskID string) (repository.TaskData, error) {
	var offlineSince time.Time
	for {
		task, err := q.GetTask(ctx, taskID)
		if err != nil {
			return repository.TaskData{}, err
		}
		if task == nil {
			return repository.TaskData{}, fmt.Errorf("task not found: %s", taskID)
		}
		if _, terminal := repository.TerminalTaskStatuses[task.Status]; terminal {
			return *task, nil
		}

		online, err := q.IsWorkerOnline(ctx, assetDefaultLease)
		if err != nil {
			return repository.TaskData{}, err
		}
		if online {
			offlineSince = time.Time{}
		} else {
			if offlineSince.IsZero() {
				offlineSince = time.Now()
			} else if time.Since(offlineSince) >= assetWorkerOfflineGrace {
				return repository.TaskData{}, fmt.Errorf("queue worker offline while waiting for task '%s'", taskID)
			}
		}

		select {
		case <-ctx.Done():
			return repository.TaskData{}, ctx.Err()
		case <-time.After(assetPollInterval):
		}
	}
}

// DedupStrings 去重并保持原序。
func DedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
