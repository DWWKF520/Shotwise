package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/provider"
)

// fakeVideoCapsResolver 按注入结果返回项目视频能力与音频开关。
type fakeVideoCapsResolver struct {
	view       config.VideoCapabilityView
	viewErr    error
	generateAu bool
	audioErr   error
	resolution string
	byBucket   map[provider.VideoBucket]config.VideoCapabilityView
}

func (f *fakeVideoCapsResolver) VideoCapabilitiesForProjectBucket(
	_ context.Context, _ *project.ProjectFile, bucket provider.VideoBucket,
) (config.VideoCapabilityView, error) {
	if f.viewErr != nil {
		return config.VideoCapabilityView{}, f.viewErr
	}
	if f.byBucket != nil {
		if view, ok := f.byBucket[bucket]; ok {
			return view, nil
		}
	}
	return f.view, nil
}

func (f *fakeVideoCapsResolver) VideoGenerateAudioForProject(context.Context, *project.ProjectFile) (bool, error) {
	return f.generateAu, f.audioErr
}

func (f *fakeVideoCapsResolver) ResolveResolution(context.Context, *project.ProjectFile, string, string) (string, error) {
	return f.resolution, nil
}

func veoView() config.VideoCapabilityView {
	return config.VideoCapabilityView{
		ProviderID:         "gemini-aistudio",
		ModelID:            "veo-3.1-generate-preview",
		SupportedDurations: []int{4, 6, 8},
		Caps:               provider.DefaultVideoCapabilities(),
	}
}

func TestResolveDurationSlotCapacitySemantics(t *testing.T) {
	exact := ResolveDurationSlot(8, []int{4, 6, 8})
	if exact.Seconds != 8 || exact.Adjustment != AdjustmentExact || exact.NeedsConfirmation() {
		t.Fatalf("精确取档有误: %+v", exact)
	}
	up := ResolveDurationSlot(5, []int{4, 6, 8})
	if up.Seconds != 6 || up.Adjustment != AdjustmentUp || !up.NeedsConfirmation() {
		t.Fatalf("向上取档有误: %+v", up)
	}
	down := ResolveDurationSlot(20, []int{4, 6, 8})
	if down.Seconds != 8 || down.Adjustment != AdjustmentDown || !down.NeedsConfirmation() {
		t.Fatalf("超上限取最大档有误: %+v", down)
	}
	unconstrained := ResolveDurationSlot(5, nil)
	if unconstrained.Seconds != 5 || unconstrained.Adjustment != AdjustmentUnconstrained || unconstrained.NeedsConfirmation() {
		t.Fatalf("空档位应原样透传: %+v", unconstrained)
	}
}

func TestEffectiveReferenceDurationsAppliesModelConstraints(t *testing.T) {
	withRefs := EffectiveReferenceDurations("gemini-aistudio", "veo-3.1-generate-preview", []int{4, 6, 8}, "1080p", true)
	if len(withRefs) != 1 || withRefs[0] != 8 {
		t.Fatalf("带参考图 + 1080p 应只留 8s: %v", withRefs)
	}
	noRefs := EffectiveReferenceDurations("gemini-aistudio", "veo-3.1-generate-preview", []int{4, 6, 8}, "", false)
	if len(noRefs) != 3 {
		t.Fatalf("无分辨率约束时应保留全集: %v", noRefs)
	}
	unknown := EffectiveReferenceDurations("unknown", "model", []int{4}, "1080p", true)
	if len(unknown) != 1 || unknown[0] != 4 {
		t.Fatalf("未登记型号不收窄: %v", unknown)
	}
}

func TestPrecheckUnitWithAdShotsAndBucketContext(t *testing.T) {
	dctx := ProjectDurationContext{
		SupportedDurations: []int{4, 6, 8},
		Resolution:         "1080p",
		ProviderID:         "gemini-aistudio",
		ModelName:          "veo-3.1-generate-preview",
	}
	unit := map[string]any{"unit_id": "E1U1", "duration_seconds": float64(5)}
	adShots := []map[string]any{{"shot_id": "S1", "duration_seconds": float64(5), "characters_in_shot": []any{"小明"}}}
	slot, err := PrecheckUnit(dctx, unit, adShots)
	if err != nil {
		t.Fatal(err)
	}
	if slot.Seconds != 8 || !slot.NeedsConfirmation() {
		t.Fatalf("带参考图退化镜头按 i2v 档取到 8s 且需确认: %+v", slot)
	}

	// 未配分辨率时不施加分辨率约束（普通路径「不传 resolution 参数」的近似）。
	noRes := ProjectDurationContext{
		SupportedDurations: []int{4, 6, 8},
		ProviderID:         "gemini-aistudio",
		ModelName:          "veo-3.1-generate-preview",
	}
	unitNoRef := map[string]any{"unit_id": "E1U2", "duration_seconds": float64(5)}
	slot, err = PrecheckUnit(noRes, unitNoRef, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slot.Seconds != 6 {
		t.Fatalf("无参考图无分辨率约束应按容量取 6s: %+v", slot)
	}
}

func TestUnitScriptDurationDirtyData(t *testing.T) {
	if got, err := UnitScriptDuration(map[string]any{"duration_seconds": "8"}, nil); err != nil || got != 8 {
		t.Fatalf("字符串整数秒应可解析: %d %v", got, err)
	}
	if got, err := UnitScriptDuration(map[string]any{}, nil); err != nil || got != FallbackUnitDuration {
		t.Fatalf("缺值应兜底 %d: %d %v", FallbackUnitDuration, got, err)
	}
	if _, err := UnitScriptDuration(map[string]any{"duration_seconds": "abc"}, nil); err == nil {
		t.Fatal("非数值脏数据应报错")
	}
	adShots := []map[string]any{{"duration_seconds": float64(3)}, {"duration_seconds": float64(4)}}
	if got, err := UnitScriptDuration(nil, adShots); err != nil || got != 7 {
		t.Fatalf("ad 应求成员镜头和: %d %v", got, err)
	}
}

func TestDefaultUnitDurationPrefersProjectPreference(t *testing.T) {
	preferred := 6
	pf := &project.ProjectFile{DefaultDuration: &preferred}
	dctx := ProjectDurationContext{SupportedDurations: []int{4, 6, 8}, ProviderID: "gemini-aistudio", ModelName: "veo-3.1-generate-preview"}
	if got := DefaultUnitDuration(dctx, pf, false); got != 6 {
		t.Fatalf("项目偏好应被采信: %d", got)
	}
	drifted := 5
	pf2 := &project.ProjectFile{DefaultDuration: &drifted}
	if got := DefaultUnitDuration(dctx, pf2, false); got != 4 {
		t.Fatalf("偏好非档位成员应退到最短档: %d", got)
	}
	empty := ProjectDurationContext{}
	if got := DefaultUnitDuration(empty, pf, false); got != FallbackUnitDuration {
		t.Fatalf("能力不可解析应兜底: %d", got)
	}
}

func TestAssertAudioSwitchSupported(t *testing.T) {
	project := &project.ProjectFile{ContentMode: "drama", GenerationMode: "reference_video"}
	resolver := &fakeVideoCapsResolver{view: veoView(), generateAu: false}
	err := AssertAudioSwitchSupported(context.Background(), resolver, project, provider.BucketR2V)
	var conflict *AudioSwitchConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("恒有声模型 + 关闭音频应冲突: %v", err)
	}
	resolver.generateAu = true
	if err := AssertAudioSwitchSupported(context.Background(), resolver, project, provider.BucketR2V); err != nil {
		t.Fatalf("音频开启时不应冲突: %v", err)
	}
}

func TestResolveProjectIsSilent(t *testing.T) {
	project := &project.ProjectFile{ContentMode: "drama", GenerationMode: "storyboard"}
	// 本集关闭音频 → 无声。
	resolver := &fakeVideoCapsResolver{view: veoView(), generateAu: false}
	if !ResolveProjectIsSilent(context.Background(), resolver, project) {
		t.Fatal("关闭音频应判无声")
	}
	// 音频开启且模型有音轨 → 有声。
	resolver.generateAu = true
	if ResolveProjectIsSilent(context.Background(), resolver, project) {
		t.Fatal("有音轨模型 + 音频开启不该判无声")
	}
	// 能力解析失败但音频开启 → 不判无声（档位按 soft 兜底）。
	failed := &fakeVideoCapsResolver{viewErr: errors.New("boom"), generateAu: true}
	if ResolveProjectIsSilent(context.Background(), failed, project) {
		t.Fatal("能力失败且音频开启不该判无声")
	}
	// 双重失败 → 按无声处理。
	doubleFailed := &fakeVideoCapsResolver{viewErr: errors.New("boom"), audioErr: errors.New("boom2")}
	if !ResolveProjectIsSilent(context.Background(), doubleFailed, project) {
		t.Fatal("双重解析失败应按无声处理")
	}
}

func TestVideoPromptToYAMLOrdersSections(t *testing.T) {
	prompt := map[string]any{
		"action":         "走进房间",
		"camera_motion":  "Static",
		"ambiance_audio": "雨声",
		"dialogue":       []any{map[string]any{"speaker": "阿明", "line": "你好"}},
	}
	out, err := VideoPromptToYAML(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Action: 走进房间\n") {
		t.Fatalf("Action 应为首段: %q", out)
	}
	if !strings.Contains(out, "Dialogue:\n- Speaker: 阿明\n  Line: 你好\n") {
		t.Fatalf("Dialogue 段格式不符: %q", out)
	}
	if !strings.HasSuffix(out, "Avoid: BGM、文字字幕、水印\n") {
		t.Fatalf("Avoid 应垫底: %q", out)
	}
}

func TestBuildDramaVideoPromptInjectsVoiceProfiles(t *testing.T) {
	entry := project.NewAssetEntry()
	entry.SetString("voice_style", "低沉")
	bucket := project.AssetBucket{"阿明": entry}
	prompt := map[string]any{"action": "说话", "voice_profiles": []any{map[string]any{"Speaker": "残留"}}}
	utterances := []any{map[string]any{"kind": "dialogue", "speaker": "阿明", "text": "你好"},
		map[string]any{"kind": "voiceover", "text": "旁白"}}
	out := BuildDramaVideoPrompt(prompt, utterances, &bucket)
	profiles, _ := out["voice_profiles"].([]any)
	if len(profiles) != 1 {
		t.Fatalf("应剥离残留并注入派生声线: %+v", out["voice_profiles"])
	}
	dialogue, _ := out["dialogue"].([]any)
	if len(dialogue) != 1 {
		t.Fatalf("只剩 dialogue-kind 进口型音轨: %+v", dialogue)
	}
	// characters 为 nil（真无声）时不注入声线，但台词照常下发。
	noVoice := BuildDramaVideoPrompt(prompt, utterances, nil)
	if _, present := noVoice["voice_profiles"]; present {
		t.Fatal("真无声时不应注入 Voice_Profiles")
	}
}
