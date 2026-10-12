package provider

import (
	"errors"
	"unicode/utf8"
)

// 视频生成的请求期能力校验（GateVideoRequest）与首帧比例改写。
//
// 三条可选输入路径（尾帧 / 参考图 / 参考音频）在一处判定，而不是散在各 backend 的 payload
// 组装里各写一套——散写的后果是同一种违约在不同供应商下有的硬失败、有的静默截断，用户拿到照常
// 扣费但与意图不符的结果却无从得知。故本函数只做拒绝，不做降级：能力不支持一律返回错误，由上层
// 渲染成用户可读错误。
//
// 能力闸门先于记账括号：调用方在开记账括号前调用本函数，失败不扣费、不留 failed ApiCall 行。
// 校验会返回错误、帧槽位组装是纯函数，两者调用时机不同，故分离。

// VideoCapabilityProbe 是能力查询实际需要的最小后端接口：只读能力声明，不碰 generate / resume。
//
// 比 VideoBackend 窄是有意的——收窄到用得着的那一个成员，调用方与测试替身都不必为一次能力查询
// 实现整个生成协议。
type VideoCapabilityProbe interface {
	VideoCapabilities() VideoCapabilities
}

// TierAwareVideoCapabilityProbe 是档位感知能力查询的可选成员。
//
// 某些后端（如 Kling）的 last_frame 仅在特定 service_tier 生效，无请求上下文的静态
// VideoCapabilities 只能保守声明，会让合法档位的请求被误判为不支持；实现本接口的后端按本次
// 请求档位给出精确声明。
type TierAwareVideoCapabilityProbe interface {
	VideoCapabilitiesForTier(serviceTier string, resolution string) VideoCapabilities
}

// ResolveVideoCapabilities 解析后端在本次请求档位下的视频能力：优先档位感知查询（若后端实现），
// 否则回落静态声明。
func ResolveVideoCapabilities(probe VideoCapabilityProbe, serviceTier string, resolution string) VideoCapabilities {
	if tierAware, ok := probe.(TierAwareVideoCapabilityProbe); ok {
		return tierAware.VideoCapabilitiesForTier(serviceTier, resolution)
	}
	return probe.VideoCapabilities()
}

// ResolveFirstFrameAspectRatio 按 caps.FirstFrameRatioAdaptiveOnly 施加首帧任务的 ratio 覆盖。
//
// 声明该约束的后端在带首帧（image-to-video）的请求上只接受 FirstFrameAdaptiveRatio——调用方
// 原始持有的 aspectRatio（用户比例意图，仍完整作用于分镜图生成）不受影响，本函数只决定实际下发给
// 该次视频请求的值。caps 为 nil、未声明该约束、或实际下发的请求不带首帧（纯文生 / 仅参考图）时
// 原样返回 aspectRatio。
//
// hasFirstFrame 由调用方按「实际下发的请求是否带首帧」传入，而不是按入参是否非空——帧槽位组装
// 只让文件源进槽位，判定须与真正下发的 VideoGenerationRequest.StartImage 一致，否则约束会与
// 实际请求脱节。
func ResolveFirstFrameAspectRatio(caps *VideoCapabilities, aspectRatio string, hasFirstFrame bool) string {
	if caps != nil && caps.FirstFrameRatioAdaptiveOnly && hasFirstFrame {
		return FirstFrameAdaptiveRatio
	}
	return aspectRatio
}

// VideoGateInput 是能力闸门的输入。各字段按「实际下发的请求」填写：
// Prompt 为 nil 表示调用方未提供提示词文本（不计长度）；EndImage 表示请求携带尾帧；
// 参考图 / 参考音频只关心段数；ReferenceAudioTotalSeconds 为 nil 表示多段音频总时长未知。
type VideoGateInput struct {
	Prompt                     *string
	EndImage                   bool
	ReferenceImages            []string
	ReferenceAudioFiles        []string
	ReferenceAudioTotalSeconds *float64
}

// GateVideoRequest 是统一的请求期能力前置校验：违约返回 *CapabilityError（携带失败码与参数），
// 通过返回 nil。
//
// caps 为 nil 表示调用方未查询后端能力——三条路径都不走时能力声明不影响任何结果，调用方可省去
// 这次查询。传 nil 却带任一路径的输入一律按不支持拒绝，而不是放行：占位一份「支持」的假能力会让
// 未经能力核实的请求下发出去，正是本函数要堵的降级。
//
// ReferenceAudioTotalSeconds 是调用方前置探测好的多段参考音频总时长（秒），本函数不做 I/O、
// 不自行探测——探测需要读音频元数据，只能在能拿到文件的组装前置校验处完成。nil 表示总时长未知
// （探测失败 / 环境不支持），此时跳过总时长校验而不是当作超限拒绝。
//
// Prompt 与三条可选路径不同：它在每个请求上都存在，故 caps 未声明 MaxPromptChars 时（含 caps
// 为 nil）跳过该项——未声明约束不等于上限为 0。
func GateVideoRequest(caps *VideoCapabilities, providerName, model string, in VideoGateInput) error {
	if caps != nil {
		if limit := caps.MaxPromptChars; limit != nil && in.Prompt != nil {
			// 供应商对超长 prompt 普遍是静默截断而非报错，照常扣费却产出与意图不符的成片——
			// 付费前拒绝，不放行。计量口径为字符数（中英文同权），不是字节数。
			if count := utf8.RuneCountInString(*in.Prompt); count > *limit {
				return NewVideoCapabilityError(FailureVideoPromptTooLong, map[string]any{
					"provider": providerName,
					"model":    model,
					"limit":    *limit,
					"count":    count,
				})
			}
		}
	}

	if in.EndImage && (caps == nil || !caps.LastFrame) {
		return NewVideoCapabilityError(FailureVideoLastFrameUnsupported, map[string]any{
			"provider": providerName,
			"model":    model,
		})
	}

	if len(in.ReferenceImages) > 0 {
		limit := 0
		if caps != nil {
			limit = caps.MaxReferenceImages
		}
		if limit <= 0 {
			return NewVideoCapabilityError(FailureVideoReferenceImagesUnsupported, map[string]any{
				"provider": providerName,
				"model":    model,
			})
		}
		if len(in.ReferenceImages) > limit {
			return NewVideoCapabilityError(FailureVideoReferenceImagesExceeded, map[string]any{
				"provider": providerName,
				"model":    model,
				"limit":    limit,
				"count":    len(in.ReferenceImages),
			})
		}
	}

	if len(in.ReferenceAudioFiles) > 0 {
		// 不支持音色输入的模型收到音频不静默丢弃：静默丢弃会生成一段音色随机的视频并照常扣费，
		// 用户以为角色声音已受控，直到成片拼接才发现跨片段音色不一致。
		mode := ReferenceAudioNone
		if caps != nil {
			mode = caps.ReferenceAudioMode
			if mode == "" {
				mode = ReferenceAudioNone
			}
		}
		if mode == ReferenceAudioNone {
			return NewVideoCapabilityError(FailureVideoReferenceAudioUnsupported, map[string]any{
				"provider": providerName,
				"model":    model,
			})
		}
		audioLimit := 0
		if caps != nil {
			audioLimit = caps.MaxReferenceAudioCount
		}
		if len(in.ReferenceAudioFiles) > audioLimit {
			return NewVideoCapabilityError(FailureVideoReferenceAudioExceeded, map[string]any{
				"provider": providerName,
				"model":    model,
				"limit":    audioLimit,
				"count":    len(in.ReferenceAudioFiles),
			})
		}
		if caps != nil && caps.MaxReferenceAudioTotalSeconds != nil && in.ReferenceAudioTotalSeconds != nil {
			if *in.ReferenceAudioTotalSeconds > *caps.MaxReferenceAudioTotalSeconds {
				return NewVideoCapabilityError(FailureVideoReferenceAudioDurationExceeded, map[string]any{
					"provider": providerName,
					"model":    model,
					"limit":    *caps.MaxReferenceAudioTotalSeconds,
					"total":    *in.ReferenceAudioTotalSeconds,
				})
			}
		}
	}

	return nil
}

// AssertCapabilityError 是给调用方的便捷判别：错误链上是否有能力错误，供编码为结构化失败原因。
func AssertCapabilityError(err error) (*CapabilityError, bool) {
	if err == nil {
		return nil, false
	}
	var capabilityErr *CapabilityError
	if errors.As(err, &capabilityErr) {
		return capabilityErr, true
	}
	return nil, false
}
