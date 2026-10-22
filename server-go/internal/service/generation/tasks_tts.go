package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/repository"
)

// 旁白配音（tts）任务执行（对应 server/services/generation_tasks.py::execute_tts_task）。
//
// 文本来源：payload.text / payload.prompt 显式优先；否则从剧本分镜的 novel_text 读取。
// 文本为空 / 分镜找不到 / 剧本未生成一律显式报错，绝不把空串送给 backend 合成。

// SetAudioGenerator 注入音频生成器；未注入时 tts 任务 fail loud。
func (d *TaskDispatcher) SetAudioGenerator(audio *AudioGenerator) { d.audio = audio }

// executeTTS 合成单段旁白并把产物路径回写到剧本的 generated_assets。
func (d *TaskDispatcher) executeTTS(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	if d.audio == nil {
		return nil, fmt.Errorf("tts task requires an audio generator")
	}
	project, err := d.store.Get(task.ProjectName)
	if err != nil {
		return nil, err
	}
	projectDir, err := d.store.Dir(task.ProjectName)
	if err != nil {
		return nil, err
	}
	scriptFile := payloadString(task.Payload, "script_file")

	text := payloadString(task.Payload, "text")
	if strings.TrimSpace(text) == "" {
		text = payloadString(task.Payload, "prompt")
	}
	if strings.TrimSpace(text) == "" {
		if scriptFile == "" {
			return nil, fmt.Errorf("tts task 需要 payload.text 或 payload.script_file 之一")
		}
		text, err = d.segmentNarrationText(task.ProjectName, scriptFile, task.ResourceID)
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("segment %s 无可合成的旁白文本（novel_text 为空）", task.ResourceID)
	}
	text = strings.TrimSpace(text)

	out, err := d.audio.Generate(ctx, AudioGenerateParams{
		ProjectName:  task.ProjectName,
		Project:      project,
		ProjectDir:   projectDir,
		UserID:       task.UserID,
		ResourceType: "audio",
		ResourceID:   task.ResourceID,
		Text:         text,
		Payload:      task.Payload,
	})
	if err != nil {
		return nil, err
	}

	audioRel, err := resourceRelativePath("audio", task.ResourceID)
	if err != nil {
		return nil, err
	}
	// 回写剧本：旁白音频是分镜级运行时产出，落在 generated_assets 里，
	// 工具据此判定该段已配好音，不重复入队。
	if scriptFile != "" {
		if err := d.writeScriptGeneratedAsset(task.ProjectName, scriptFile, task.ResourceID, "narration_audio", audioRel); err != nil {
			return nil, err
		}
	}

	createdAt, _ := NewVersions(projectDir).LatestCreatedAt("audio", task.ResourceID)
	return map[string]any{
		"version":       out.Version,
		"file_path":     audioRel,
		"created_at":    createdAt,
		"resource_type": "audio",
		"resource_id":   task.ResourceID,
	}, nil
}

// segmentNarrationText 从剧本分镜取 novel_text；分镜或剧本缺失时报错。
func (d *TaskDispatcher) segmentNarrationText(project, scriptFile, resourceID string) (string, error) {
	script, err := d.store.ReadScriptJSON(project, scriptFile)
	if err != nil {
		return "", err
	}
	items, idField, _, err := domain.ResolveScriptItems(script)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		segment, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(segment[idField]) != resourceID {
			continue
		}
		text, _ := segment["novel_text"].(string)
		return text, nil
	}
	return "", fmt.Errorf("segment not found: %s", resourceID)
}

// writeScriptGeneratedAsset 把产物路径回写进指定分镜的 generated_assets（对应 Python update_scene_asset）。
func (d *TaskDispatcher) writeScriptGeneratedAsset(project, scriptFile, itemID, assetType, assetPath string) error {
	return d.store.UpdateScript(project, scriptFile, func(script map[string]any) error {
		items, idField, _, err := domain.ResolveScriptItems(script)
		if err != nil {
			return err
		}
		for _, item := range items {
			segment, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if fmt.Sprint(segment[idField]) != itemID {
				continue
			}
			assets, ok := segment["generated_assets"].(map[string]any)
			if !ok {
				assets = map[string]any{}
				segment["generated_assets"] = assets
			}
			assets[assetType] = assetPath
			return nil
		}
		return fmt.Errorf("未找到 id=%q 的分镜（%s）", itemID, idField)
	})
}
