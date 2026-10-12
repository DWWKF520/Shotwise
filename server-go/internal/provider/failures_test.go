package provider

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// 全部 50 个失败码的字面值（跨端落库契约，改名即破坏已存数据）。
var allFailureCodeStrings = []string{
	"cascade_blocked_dependency",
	"image_capability_missing_i2i",
	"image_capability_missing_t2i",
	"image_dashscope_4k_t2i_only",
	"image_endpoint_mismatch_no_i2i",
	"image_endpoint_mismatch_no_t2i",
	"image_reference_images_unreadable",
	"provider_unsupported_media",
	"ref_payload_floor_exceeded",
	"resume_endpoint_changed_detail",
	"resume_expired_detail",
	"resume_unsupported_capacity_zero",
	"resume_unsupported_detail",
	"resume_unsupported_provider",
	"restart_lost_audio",
	"restart_lost_image",
	"restart_lost_no_job_id",
	"restart_lost_resume_no_job_id",
	"script_edit_error",
	"script_edit_generated_assets_invalid",
	"script_edit_items_not_list",
	"script_edit_unit_lists_invalid",
	"video_capabilities_unresolved",
	"video_capability_missing_i2v",
	"video_capability_missing_r2v",
	"video_capability_missing_t2v",
	"video_capability_reference_unavailable",
	"video_duration_invalid",
	"video_duration_not_supported",
	"video_end_image_requires_start_image",
	"video_end_image_unreadable",
	"video_kling_model_endpoint_unsupported",
	"video_last_frame_requires_pro",
	"video_last_frame_unsupported",
	"video_prompt_too_long",
	"video_reference_audio_duration_exceeded",
	"video_reference_audio_exceeded",
	"video_reference_audio_format_unsupported",
	"video_reference_audio_slots_insufficient",
	"video_reference_audio_unreadable",
	"video_reference_audio_unsupported",
	"video_reference_images_duration_unsupported",
	"video_reference_images_exceeded",
	"video_reference_images_required",
	"video_reference_images_unreadable",
	"video_reference_images_unsupported",
	"video_reference_images_with_frames_unsupported",
	"video_resolution_duration_unsupported",
	"video_resolution_not_supported",
	"video_start_image_unreadable",
}

func TestFailureCodeTable(t *testing.T) {
	if len(FailureCodeKeys) != 50 {
		t.Fatalf("失败码总数应为 50（32 能力类 + 18 通用），实得 %d", len(FailureCodeKeys))
	}
	if len(CapabilityFailureCodes) != 32 {
		t.Fatalf("能力类失败码应为 32 个，实得 %d", len(CapabilityFailureCodes))
	}
	got := make([]string, 0, len(FailureCodeKeys))
	for code := range FailureCodeKeys {
		got = append(got, string(code))
	}
	sort.Strings(got)
	want := append([]string(nil), allFailureCodeStrings...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("码表数量不符：%d vs %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("码表不一致：位置 %d 实得 %q，期望 %q", i, got[i], want[i])
		}
	}
	for code := range CapabilityFailureCodes {
		if !IsKnownFailureCode(code) {
			t.Errorf("能力类码 %q 未登记进 FailureCodeKeys", code)
		}
	}
	if IsKnownFailureCode("not_a_code") {
		t.Fatal("未登记码不应被识别")
	}
}

func TestFailureCodeI18nKeys(t *testing.T) {
	// 能力类码、解析闸码与剧本编辑码为身份映射。
	identity := []FailureCode{
		FailureImageCapabilityMissingI2I,
		FailureImageCapabilityMissingT2I,
		FailureImageDashscope4KT2IOnly,
		FailureImageEndpointMismatchNoI2I,
		FailureImageEndpointMismatchNoT2I,
		FailureImageReferenceImagesUnreadable,
		FailureRefPayloadFloorExceeded,
		FailureVideoCapabilitiesUnresolved,
		FailureVideoCapabilityMissingT2V,
		FailureVideoDurationInvalid,
		FailureVideoDurationNotSupported,
		FailureVideoEndImageRequiresStartImage,
		FailureVideoEndImageUnreadable,
		FailureVideoLastFrameRequiresPro,
		FailureVideoLastFrameUnsupported,
		FailureVideoPromptTooLong,
		FailureVideoReferenceImagesDurationUnsupported,
		FailureVideoReferenceImagesExceeded,
		FailureVideoReferenceImagesRequired,
		FailureVideoReferenceImagesUnreadable,
		FailureVideoReferenceImagesUnsupported,
		FailureVideoResolutionNotSupported,
		FailureVideoKlingModelEndpointUnsupported,
		FailureVideoReferenceImagesWithFramesUnsupported,
		FailureVideoReferenceAudioDurationExceeded,
		FailureVideoReferenceAudioExceeded,
		FailureVideoReferenceAudioFormatUnsupported,
		FailureVideoReferenceAudioSlotsInsufficient,
		FailureVideoReferenceAudioUnreadable,
		FailureVideoReferenceAudioUnsupported,
		FailureVideoResolutionDurationUnsupported,
		FailureVideoStartImageUnreadable,
		FailureVideoCapabilityMissingI2V,
		FailureVideoCapabilityMissingR2V,
		FailureVideoCapabilityReferenceUnavailable,
		FailureScriptEditError,
		FailureScriptEditItemsNotList,
		FailureScriptEditUnitListsInvalid,
		FailureScriptEditGeneratedAssetsInvalid,
	}
	for _, code := range identity {
		key, ok := FailureI18nKey(code)
		if !ok {
			t.Errorf("码 %q 无 i18n key", code)
			continue
		}
		if key != string(code) {
			t.Errorf("码 %q 应为身份映射，实得 key %q", code, key)
		}
	}

	// 其余通用码带 task_fail_ 前缀。
	prefixed := []FailureCode{
		FailureProviderUnsupportedMedia,
		FailureRestartLostImage,
		FailureRestartLostAudio,
		FailureRestartLostNoJobID,
		FailureRestartLostResumeNoJobID,
		FailureResumeUnsupportedProvider,
		FailureResumeUnsupportedCapacityZero,
		FailureResumeUnsupportedDetail,
		FailureResumeExpiredDetail,
		FailureResumeEndpointChangedDetail,
		FailureCascadeBlockedDependency,
	}
	for _, code := range prefixed {
		key, ok := FailureI18nKey(code)
		if !ok {
			t.Errorf("码 %q 无 i18n key", code)
			continue
		}
		if key != "task_fail_"+string(code) {
			t.Errorf("码 %q 的 key 应为 task_fail_ 前缀，实得 %q", code, key)
		}
	}
	if _, ok := FailureI18nKey("not_a_code"); ok {
		t.Fatal("未登记码不应有 i18n key")
	}

	// 两份清单应恰好覆盖全部 50 个码，防止漏登记。
	covered := map[FailureCode]struct{}{}
	for _, code := range append(append([]FailureCode{}, identity...), prefixed...) {
		covered[code] = struct{}{}
	}
	if len(covered) != len(FailureCodeKeys) {
		t.Fatalf("身份/前缀清单仅覆盖 %d 个码，码表共 %d 个", len(covered), len(FailureCodeKeys))
	}
}

func TestNonResumableVideoProviders(t *testing.T) {
	if len(NonResumableVideoProviders) != 2 {
		t.Fatalf("不支持续跑的 provider 应为 grok / vidu 两家，实得 %v", NonResumableVideoProviders)
	}
	if !IsNonResumableVideoProvider("grok") || !IsNonResumableVideoProvider("vidu") {
		t.Fatal("grok 与 vidu 应不支持续跑")
	}
	if IsNonResumableVideoProvider("ark") || IsNonResumableVideoProvider("") {
		t.Fatal("ark 与空串不应被判定为不支持续跑")
	}
}

func TestEncodeFailureMessage(t *testing.T) {
	cases := []struct {
		name   string
		code   FailureCode
		params map[string]any
		want   string
	}{
		{
			name: "无参数时只有码",
			code: FailureVideoLastFrameUnsupported,
			want: "[video_last_frame_unsupported]",
		},
		{
			name:   "空参数字典等价无参数",
			code:   FailureVideoLastFrameUnsupported,
			params: map[string]any{},
			want:   "[video_last_frame_unsupported]",
		},
		{
			name: "参数按键排序且冒号逗号后带空格（json.dumps 默认分隔符）",
			code: FailureVideoPromptTooLong,
			params: map[string]any{
				"provider": "ark", "model": "m", "limit": 100, "count": 120,
			},
			want: `[video_prompt_too_long] {"count": 120, "limit": 100, "model": "m", "provider": "ark"}`,
		},
		{
			name:   "中文不转义（ensure_ascii=false）且 HTML 字符不转义",
			code:   FailureResumeExpiredDetail,
			params: map[string]any{"detail": "远端返回中文 <b> 错误"},
			want:   `[resume_expired_detail] {"detail": "远端返回中文 <b> 错误"}`,
		},
		{
			name:   "浮点参数按 JSON 数字编码",
			code:   FailureVideoReferenceAudioDurationExceeded,
			params: map[string]any{"limit": 30.5},
			want:   `[video_reference_audio_duration_exceeded] {"limit": 30.5}`,
		},
	}
	for _, c := range cases {
		if got := EncodeFailureMessage(c.code, c.params); got != c.want {
			t.Errorf("%s: EncodeFailureMessage = %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestEncodeFailureMessagePanicsOnUnknownCode(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("未登记的码应 panic（fail-fast，不静默落库）")
		}
	}()
	EncodeFailureMessage(FailureCode("not_a_code"), nil)
}

func TestDecodeFailureMessage(t *testing.T) {
	code, params, ok := DecodeFailureMessage("[video_last_frame_unsupported]")
	if !ok || code != FailureVideoLastFrameUnsupported || len(params) != 0 {
		t.Fatalf("无参数解码异常：%q %v %v", code, params, ok)
	}
	code, params, ok = DecodeFailureMessage(`[video_prompt_too_long] {"count": 120, "provider": "ark"}`)
	if !ok || code != FailureVideoPromptTooLong {
		t.Fatalf("带参数解码异常：%q %v", code, ok)
	}
	if params["count"] != float64(120) || params["provider"] != "ark" {
		t.Fatalf("参数解码结果错误：%v", params)
	}

	// 透传场景：识别不了或参数畸形的原文一律 ok=false，由调用方原样保留。
	passthrough := []string{
		"[restart_lost] 服务重启导致任务丢失", // 旧格式：带中文尾巴
		"[restart_lost]", // 码本身未登记
		"[not_a_code]",
		"[video_prompt_too_long] 不是JSON",
		`[video_prompt_too_long] [1, 2]`, // 参数必须是对象
		`[video_prompt_too_long] {`,
		"供应商原始异常文本",
		"",
		"[video_prompt_too_long] ", // 尾随空格不合法
		" [video_prompt_too_long]",
	}
	for _, message := range passthrough {
		code, params, ok := DecodeFailureMessage(message)
		if ok || code != "" || params != nil {
			t.Errorf("透传串 %q 不应被识别：%q %v %v", message, code, params, ok)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		code   FailureCode
		params map[string]any
	}{
		{FailureVideoPromptTooLong, map[string]any{"provider": "ark", "model": "m", "limit": 1000, "count": 1200}},
		{FailureVideoReferenceImagesExceeded, map[string]any{"provider": "vidu", "limit": 3, "count": 4}},
		{FailureCascadeBlockedDependency, map[string]any{
			"dependency_task_id": "task-1",
			"reason":             `[video_prompt_too_long] {"count": 1, "limit": 0}`,
		}},
		{FailureResumeUnsupportedDetail, nil},
	}
	for _, c := range cases {
		message := EncodeFailureMessage(c.code, c.params)
		code, params, ok := DecodeFailureMessage(message)
		if !ok || code != c.code {
			t.Fatalf("往返失败：%q → %q %v", message, code, ok)
		}
		// 有参数时按 JSON 回读比对（int 回落为 float64；嵌套字符串原样）。
		if len(c.params) != len(params) {
			t.Fatalf("往返参数数量不一致：%v vs %v", c.params, params)
		}
		for key, want := range c.params {
			got, exists := params[key]
			if !exists {
				t.Fatalf("往返丢参 %q：%v", key, params)
			}
			switch want := want.(type) {
			case int:
				if got != float64(want) {
					t.Fatalf("参数 %q = %v，期望 %v", key, got, want)
				}
			default:
				if got != want {
					t.Fatalf("参数 %q = %v，期望 %v", key, got, want)
				}
			}
		}
	}
}

func TestBoundFailureReason(t *testing.T) {
	if got := BoundFailureReason("[video_last_frame_unsupported]", 100); got != "[video_last_frame_unsupported]" {
		t.Fatalf("未超限时应原样返回：%q", got)
	}
	// 非结构化文本按字符裁剪。
	long := strings.Repeat("中", 3000)
	if got := BoundFailureReason(long, 100); utf8.RuneCountInString(got) != 100 || !strings.HasPrefix(got, "中") {
		t.Fatalf("非结构化裁剪异常：%d", utf8.RuneCountInString(got))
	}
	// 结构化串优先裁剪最长字符串参数，保持仍是可解析信封。
	message := EncodeFailureMessage(FailureResumeExpiredDetail, map[string]any{"detail": strings.Repeat("x", 3000)})
	if utf8.RuneCountInString(message) > MaxFailureMessageChars {
		t.Fatalf("编码后应已裁剪到 %d 以内，实得 %d", MaxFailureMessageChars, utf8.RuneCountInString(message))
	}
	code, params, ok := DecodeFailureMessage(message)
	if !ok || code != FailureResumeExpiredDetail {
		t.Fatalf("裁剪后仍应是合法信封：%q", message)
	}
	detail, _ := params["detail"].(string)
	if len(detail) == 0 || len(detail) >= 3000 {
		t.Fatalf("detail 参数应被收窄：%d", len(detail))
	}
	// 多字符串参数：裁剪最长的那个。
	message = EncodeFailureMessage(FailureResumeExpiredDetail, map[string]any{
		"detail": strings.Repeat("a", 1500),
		"extra":  strings.Repeat("b", 1500),
	})
	if utf8.RuneCountInString(message) > MaxFailureMessageChars {
		t.Fatalf("多参数裁剪失败：%d", utf8.RuneCountInString(message))
	}
	if _, _, ok := DecodeFailureMessage(message); !ok {
		t.Fatalf("多参数裁剪后应仍可解析：%q", message)
	}
	// 容器参数降级为文本后参与裁剪。
	encoded := EncodeFailureMessage(FailureVideoDurationInvalid, map[string]any{
		"durations": []any{strings.Repeat("c", 4000)},
	})
	if utf8.RuneCountInString(encoded) > MaxFailureMessageChars {
		t.Fatalf("容器参数裁剪失败：%d", utf8.RuneCountInString(encoded))
	}
	_, params, ok = DecodeFailureMessage(encoded)
	if !ok {
		t.Fatalf("容器参数裁剪后应仍可解析：%q", encoded)
	}
	if _, isString := params["durations"].(string); !isString {
		t.Fatalf("容器参数应降级为文本参与裁剪：%v", params)
	}
	// 非法 JSON 参数退回按字符裁剪。
	broken := `[video_duration_invalid] {` + strings.Repeat("9", 2500) + `}`
	if got := BoundFailureReason(broken, 200); utf8.RuneCountInString(got) != 200 {
		t.Fatalf("畸形参数串应按字符裁剪到 200，实得 %d", utf8.RuneCountInString(got))
	}
	// 多字节字符串按字符（rune）计，不按字节。
	message = EncodeFailureMessage(FailureResumeExpiredDetail, map[string]any{"detail": strings.Repeat("中", 1500)})
	if utf8.RuneCountInString(message) > MaxFailureMessageChars {
		t.Fatalf("多字节参数裁剪失败：%d", utf8.RuneCountInString(message))
	}
}

func TestCollapseCascadeReason(t *testing.T) {
	root := EncodeFailureMessage(FailureVideoPromptTooLong, map[string]any{"provider": "ark", "limit": 10})
	mid := EncodeFailureMessage(FailureCascadeBlockedDependency, map[string]any{
		"dependency_task_id": "t1",
		"reason":             root,
	})
	outer := EncodeFailureMessage(FailureCascadeBlockedDependency, map[string]any{
		"dependency_task_id": "t2",
		"reason":             mid,
	})
	if got := CollapseCascadeReason(outer); got != root {
		t.Fatalf("折叠结果 = %q，期望 %q", got, root)
	}
	if got := CollapseCascadeReason(root); got != root {
		t.Fatalf("非级联串应原样返回：%q", got)
	}
	noReason := EncodeFailureMessage(FailureCascadeBlockedDependency, map[string]any{"dependency_task_id": "t"})
	if got := CollapseCascadeReason(noReason); got != noReason {
		t.Fatalf("缺 reason 参数时应原样返回：%q", got)
	}
	plain := "供应商原始异常文本"
	if got := CollapseCascadeReason(plain); got != plain {
		t.Fatalf("普通文本应原样返回：%q", got)
	}
	// 深层畸形嵌套不应空转挂死。
	deep := root
	for i := 0; i < 120; i++ {
		deep = EncodeFailureMessage(FailureCascadeBlockedDependency, map[string]any{"reason": deep})
	}
	if got := CollapseCascadeReason(deep); got == "" {
		t.Fatal("深层嵌套应返回非空结果")
	}
}

func TestSanitizeFailureReason(t *testing.T) {
	cases := []struct {
		name string
		in   string
		not  []string
		has  []string
	}{
		{
			name: "查询串凭证",
			in:   "下载失败 https://cdn.example.com/x?token=abc123&size=2",
			not:  []string{"abc123"},
			has:  []string{"token=[REDACTED]", "size=2"},
		},
		{
			name: "键值凭证",
			in:   "api_key=sk-abcdef",
			not:  []string{"sk-abcdef"},
			has:  []string{"api_key=[REDACTED]"},
		},
		{
			name: "Bearer 头",
			in:   "Authorization: Bearer tok-xyz",
			not:  []string{"tok-xyz"},
			has:  []string{"Bearer [REDACTED]"},
		},
		{
			name: "Windows 绝对路径",
			in:   `读取失败 C:\wkf\Shotwise\data\secret.png`,
			not:  []string{`C:\wkf`},
			has:  []string{"[REDACTED_PATH]"},
		},
		{
			name: "POSIX 绝对路径",
			in:   "打开 /Users/bob/project/asset.png 失败",
			not:  []string{"/Users/bob"},
			has:  []string{"[REDACTED_PATH]"},
		},
	}
	for _, c := range cases {
		got := SanitizeFailureReason(c.in)
		for _, frag := range c.not {
			if strings.Contains(got, frag) {
				t.Errorf("%s: 脱敏结果仍含 %q：%q", c.name, frag, got)
			}
		}
		for _, frag := range c.has {
			if !strings.Contains(got, frag) {
				t.Errorf("%s: 脱敏结果缺 %q：%q", c.name, frag, got)
			}
		}
	}

	// 结构化信封保持可解析，敏感键整体遮蔽。
	message := EncodeFailureMessage(FailureResumeExpiredDetail, map[string]any{
		"api_key": "sk-secret", "detail": "token=abc123",
	})
	sanitized := SanitizeFailureReason(message)
	code, params, ok := DecodeFailureMessage(sanitized)
	if !ok || code != FailureResumeExpiredDetail {
		t.Fatalf("脱敏后应仍是合法信封：%q", sanitized)
	}
	if params["api_key"] != "[REDACTED]" {
		t.Fatalf("敏感键应整体遮蔽：%v", params["api_key"])
	}
	if got, _ := params["detail"].(string); strings.Contains(got, "abc123") {
		t.Fatalf("值内凭证应脱敏：%v", params["detail"])
	}

	// 未登记码的原文按文本脱敏（旧格式不丢内容）。
	legacy := SanitizeFailureReason("[restart_lost] token=abc at C:\\wkf\\out.mp4")
	if strings.Contains(legacy, "abc") || strings.Contains(legacy, "C:\\wkf") {
		t.Fatalf("旧格式文本脱敏不彻底：%q", legacy)
	}
	if !strings.HasPrefix(legacy, "[restart_lost]") {
		t.Fatalf("旧格式前缀应保留：%q", legacy)
	}

	// 普通文本原样返回。
	if got := SanitizeFailureReason("供应商返回 503 Service Unavailable"); got != "供应商返回 503 Service Unavailable" {
		t.Fatalf("普通文本不应被改写：%q", got)
	}
}

func TestSanitizeFailureNestedValues(t *testing.T) {
	message := EncodeFailureMessage(FailureCascadeBlockedDependency, map[string]any{
		"reason": map[string]any{"token": "abc", "note": "api_key=xyz"},
	})
	sanitized := SanitizeFailureReason(message)
	_, params, ok := DecodeFailureMessage(sanitized)
	if !ok {
		t.Fatalf("脱敏后应仍是合法信封：%q", sanitized)
	}
	nested, _ := params["reason"].(map[string]any)
	if nested == nil {
		t.Fatalf("嵌套对象应保留：%v", params)
	}
	if nested["token"] != "[REDACTED]" {
		t.Fatalf("嵌套敏感键应遮蔽：%v", nested["token"])
	}
	if strings.Contains(nested["note"].(string), "xyz") {
		t.Fatalf("嵌套文本应脱敏：%v", nested["note"])
	}
	if _, err := json.Marshal(params); err != nil {
		t.Fatalf("参数应可再序列化：%v", err)
	}
}
