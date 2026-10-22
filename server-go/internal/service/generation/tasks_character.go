// 角色设计图任务（对应 server/services/generation_tasks.py::execute_character_task 与
// _execute_character_task_locked）：四视图设计图 + 从设计图裁出标准头像，写回
// character_sheet / character_avatar 两个字段。
package generation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/service"
	"github.com/shotwise/shotwise/internal/infra/fs"
	"github.com/shotwise/shotwise/internal/infra/img"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/repository"
)

// executeCharacterTask 生成角色四视图设计图并派生头像。
//
// 与 Python 的收敛差异：① 人脸检测裁剪不迁（依赖 OpenCV），头像一律走宽版面板兜底
// 裁剪（见 infra/img.CreateAvatar）；② 每角色的 asyncio 生成锁不迁——Go worker 的
// 并发通道不按资源串行，改由「写回前 previous_avatar 比对」竞态守卫承担同样不变式：
// 过期生成不得覆盖手动上传或已完赛的并发生成，落败方的头像文件删掉、不删胜者的。
func (d *TaskDispatcher) executeCharacterTask(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	raw := strings.TrimSpace(payloadString(task.Payload, "prompt"))
	if raw == "" {
		return nil, fmt.Errorf("prompt is required for character task")
	}
	characterName, err := domain.ValidateAssetName(task.ResourceID)
	if err != nil {
		return nil, fmt.Errorf("invalid character name: %s", task.ResourceID)
	}
	pf, err := d.store.Get(task.ProjectName)
	if err != nil {
		return nil, err
	}
	projectDir, err := d.store.Dir(task.ProjectName)
	if err != nil {
		return nil, err
	}
	characters, ok := pf.BucketRef(domain.AssetCharacter)
	if !ok {
		return nil, fmt.Errorf("character bucket missing")
	}
	key, ok := characters.ResolveKey(characterName)
	if !ok {
		return nil, fmt.Errorf("character not found: %s", characterName)
	}
	entry := (*characters)[key]

	fullPrompt := domain.BuildCharacterPrompt(key, raw, pf.Style, pf.StyleDescription)
	// 角色参考图（手动上传的定妆锚点）作为 i2i 输入；缺失或越界时退化为纯文生图。
	var refs []provider.ReferenceImage
	if refRel, ok := entry.String("reference_image"); ok && strings.TrimSpace(refRel) != "" {
		if full := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(refRel)); full != "" {
			if info, statErr := os.Stat(full); statErr == nil && !info.IsDir() {
				refs = []provider.ReferenceImage{{Path: full}}
			}
		}
	}
	previousAvatar := entry.MustString("character_avatar")

	sheetRel, err := service.ResourceRelativePath("characters", key)
	if err != nil {
		return nil, err
	}
	sheetAbs := filepath.Join(projectDir, filepath.FromSlash(sheetRel))
	avatarRel := "characters/" + characterAvatarResourceID(key, task.TaskID) + ".png"
	avatarAbs := filepath.Join(projectDir, filepath.FromSlash(avatarRel))

	request := provider.NewImageGenerationRequest(fullPrompt, sheetAbs)
	request.ReferenceImages = refs
	request.AspectRatio = service.AspectRatioFor(pf, "characters")
	if size := payloadString(task.Payload, "image_size"); size != "" {
		request.ImageSize = size
	}
	request.ProjectName = task.ProjectName

	capability := config.ImageCapT2I
	if len(refs) > 0 {
		capability = config.ImageCapI2I
	}
	if _, err := d.images.Generate(ctx, ImageGenerateParams{
		ProjectName: task.ProjectName,
		Project:     pf,
		Capability:  capability,
		Request:     request,
		UserID:      task.UserID,
		SegmentID:   segmentIDFor("image", "characters", key),
	}); err != nil {
		return nil, err
	}

	// 头像从设计图裁出；裁剪失败时清掉可能写了一半的文件再失败。
	if err := img.CreateAvatar(sheetAbs, avatarAbs); err != nil {
		_ = os.Remove(avatarAbs)
		return nil, err
	}

	finalized := false
	if err := d.store.Update(task.ProjectName, func(p *domain.ProjectFile) error {
		b, ok := p.BucketRef(domain.AssetCharacter)
		if !ok {
			return fmt.Errorf("character bucket missing")
		}
		k, ok := b.ResolveKey(key)
		if !ok {
			return fmt.Errorf("角色 '%s' 不存在", key)
		}
		e := (*b)[k]
		// 手动上传或并发任务在生成期间更新过头像时，本次结果已无归属：跳过写回，
		// 调用方按 finalized=false 清理产物。
		if e.MustString("character_avatar") != previousAvatar {
			return nil
		}
		sheetSpec, _ := domain.Spec(domain.AssetCharacter)
		e.SetString(sheetSpec.SheetKey, sheetRel)
		e.SetString("character_avatar", avatarRel)
		finalized = true
		return nil
	}); err != nil {
		_ = os.Remove(avatarAbs)
		return nil, err
	}
	if !finalized {
		// 产物已不被项目引用；绝不删除赢下竞态的那份头像。
		_ = os.Remove(avatarAbs)
	} else if shouldRetirePreviousAvatar(previousAvatar, key, avatarRel) {
		if old := fs.TrySafeJoin(projectDir, false, filepath.FromSlash(previousAvatar)); old != "" {
			_ = os.Remove(old)
		}
	}

	return finishImageResult(projectDir, "characters", key, raw, request.AspectRatio)
}

// characterAvatarResourceID 派生本次生成专属的头像资源 id：task_id 去连字符取前 12 位，
// 与既有生成/上传产物不共用文件名，杜绝跨任务覆盖（task_id 缺失的直调路径退化为 uuid）。
func characterAvatarResourceID(characterName, taskID string) string {
	suffix := strings.ReplaceAll(taskID, "-", "")
	if suffix == "" {
		suffix = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	return characterName + "_avatar_" + suffix
}

// shouldRetirePreviousAvatar 判断旧头像文件是否可删：仅限「由历史生成任务产生」的
// 头像（文件名带 _avatar_ 后缀段），手动上传（*_avatar_manual.png）与当前头像除外。
func shouldRetirePreviousAvatar(previous, characterKey, current string) bool {
	if previous == "" || previous == current {
		return false
	}
	if previous == "characters/"+characterKey+"_avatar_manual.png" {
		return false
	}
	base := filepath.Base(filepath.FromSlash(previous))
	return strings.HasPrefix(base, characterKey+"_avatar_") && strings.HasSuffix(previous, ".png")
}
