package service

import (
	"errors"

	"github.com/shotwise/shotwise/internal/domain/media"
)

// FindShot 定位分镜条目（shot_upload 上传校验用；asset 包 end_frame 另有带 404 语义的同形实现）。
func FindShot(script map[string]any, shotID string) (map[string]any, error) {
	items, idField, _ := media.StoryboardItems(script)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fmtString(item[idField]) == shotID {
			return item, nil
		}
	}
	return nil, errors.New("segment not found: " + shotID)
}
