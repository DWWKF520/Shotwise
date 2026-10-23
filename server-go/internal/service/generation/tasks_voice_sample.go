// 角色试听样本任务（对应 server/services/generation_tasks.py::execute_character_voice_sample_task）：
// 为角色参考音频候选合成一段 TTS 预览，产物满足参考音频同口径校验，confirm 端点据此提升为
// 角色 reference_audio。
package generation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	domainproject "github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/ffmpeg"
	"github.com/shotwise/shotwise/internal/repository"
	"github.com/shotwise/shotwise/internal/service"
)

// 试听样本的参考音频同口径校验阈值（对应 lib/audio_utils.py 的三个常量）。
const (
	voiceSampleMaxBytes    = 15 * 1024 * 1024
	voiceSampleMinSeconds  = 2.0
	voiceSampleMaxSeconds  = 10.0
	voiceSampleNameMaxUTF8 = 80 // 资源 id 里角色名的 UTF-8 字节上限
)

// voiceSampleResourceID 是试听样本在 audio/ 下的资源 id（区别于旁白 segment id 命名空间）。
//
// 带任务专属 task_id 而非只用角色名：前一次成功样本尚未确认时重新生成，固定角色名会
// 原地覆盖旧任务 result.file_path 引用的物理内容——旧任务 confirm 会把错误字节提升为
// 角色参考音频。每次生成用任务专属文件名，杜绝跨任务覆盖。
func voiceSampleResourceID(characterName, taskID string) string {
	safeName := truncateUTF8Bytes(characterName, voiceSampleNameMaxUTF8)
	return "voice_sample__" + safeName + "__" + taskID
}

// truncateUTF8Bytes 按 UTF-8 字节数裁剪：完整 rune 逐个容纳，放不下时截断
// （对应 Python encode[:n].decode(errors="ignore") 丢弃残缺尾的语义）。
func truncateUTF8Bytes(name string, maxBytes int) string {
	if len(name) <= maxBytes {
		return name
	}
	cut := 0
	for idx, r := range name {
		size := utf8.RuneLen(r)
		if size < 1 {
			size = 1 // 非法字节按单字节计
		}
		if cut+size > maxBytes {
			break
		}
		cut = idx + size
	}
	return name[:cut]
}

// executeVoiceSampleTask 生成一段试听样本：文本与音色显式来自 payload（prompt = 待合成
// 文本，voice = 用户选定音色），不回落全局旁白配置。生成产物须满足与参考音频上传同口径
// 的校验（≤15MB、时长 2-10 秒）；校验失败任务落 failed，不静默放行不合规样本。
func (d *TaskDispatcher) executeVoiceSampleTask(ctx context.Context, task *repository.TaskData) (map[string]any, error) {
	characterName, err := domainproject.ValidateAssetName(task.ResourceID)
	if err != nil {
		return nil, fmt.Errorf("invalid character name: %s", task.ResourceID)
	}
	text := payloadString(task.Payload, "prompt")
	voice := payloadString(task.Payload, "voice")
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("voice sample 任务需要非空 payload.prompt（待合成文本）")
	}
	if strings.TrimSpace(voice) == "" {
		return nil, fmt.Errorf("voice sample 任务需要 payload.voice（音色 id）")
	}
	if task.TaskID == "" {
		// 恒由 worker 传入队列任务自身 id；缺失说明调用方绕过了常规队列执行路径，
		// sample_id 的跨任务隔离依赖它，fail-fast。
		return nil, fmt.Errorf("voice sample 任务需要 task_id")
	}

	project, err := d.store.Get(task.ProjectName)
	if err != nil {
		return nil, err
	}
	projectDir, err := d.store.Dir(task.ProjectName)
	if err != nil {
		return nil, err
	}
	// 入队后、worker 取到任务前角色可能已被删除：执行前重新核实存在，避免花钱合成
	// 一段没有归属的孤儿预览。
	if characters, ok := project.BucketRef(domainproject.AssetCharacter); !ok {
		return nil, fmt.Errorf("character bucket missing")
	} else if _, ok := characters.ResolveKey(characterName); !ok {
		return nil, fmt.Errorf("character not found: %s", characterName)
	}
	if d.audio == nil {
		return nil, fmt.Errorf("voice_sample task requires an audio generator")
	}

	sampleID := voiceSampleResourceID(characterName, task.TaskID)
	res, err := d.audio.Generate(ctx, AudioGenerateParams{
		ProjectName:  task.ProjectName,
		Project:      project,
		ProjectDir:   projectDir,
		UserID:       task.UserID,
		ResourceType: "audio",
		ResourceID:   sampleID,
		Text:         strings.TrimSpace(text),
		Voice:        strings.TrimSpace(voice),
	})
	if err != nil {
		return nil, err
	}

	audioRel, err := service.ResourceRelativePath("audio", sampleID)
	if err != nil {
		return nil, err
	}
	audioAbs := filepath.Join(projectDir, filepath.FromSlash(audioRel))
	info, err := os.Stat(audioAbs)
	if err != nil {
		return nil, fmt.Errorf("语音样本产物缺失: %w", err)
	}
	if info.Size() > voiceSampleMaxBytes {
		return nil, fmt.Errorf("生成的语音样本超过 %dMB 限制", voiceSampleMaxBytes/(1024*1024))
	}
	// ffprobe 不可用（ok=false）时跳过时长校验，与仓库降级口径一致。
	duration, ok := ffmpeg.AudioDuration(ctx, audioAbs)
	if ok && (duration < voiceSampleMinSeconds || duration > voiceSampleMaxSeconds) {
		return nil, fmt.Errorf("生成的语音样本时长 %.1fs 超出 %.0f-%.0f 秒范围",
			duration, voiceSampleMinSeconds, voiceSampleMaxSeconds)
	}

	result := map[string]any{
		"version":        res.Version,
		"file_path":      audioRel,
		"resource_type":  "audio",
		"resource_id":    sampleID,
		"character_name": characterName,
		"voice":          strings.TrimSpace(voice),
	}
	if ok {
		result["duration_seconds"] = duration
	}
	return result, nil
}
