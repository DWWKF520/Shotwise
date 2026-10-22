package service

import (
	"context"
	"github.com/shotwise/shotwise/internal/repository"
)

// GridQueue 是宫格入队依赖的队列能力（*queue.Queue 满足）。
// GridQueue 是宫格入队需要的队列能力。
type GridQueue interface {
	EnqueueTask(ctx context.Context, p repository.EnqueueParams) (repository.EnqueueResult, error)
}
