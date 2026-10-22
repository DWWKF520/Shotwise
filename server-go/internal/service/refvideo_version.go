package service

// 参考生视频产物版本登记的共用件（version_service 与 generation 执行层共用；
// 原 generation/tasks_reference_video.go 内联件，归属版本侧避免反向依赖）。

// SignatureMode 区分「按剧本现算来源签名」与「沿用给定签名」两种写回口径。
type SignatureMode int

const (
	SignatureCompute SignatureMode = iota
	SignatureValue
)

// ApplyUnitVideoAssetsHook 由 generation 包注册（依赖倒置：版本还原侧写回 unit 产物
// 需要渲染件，根包不反向依赖 generation 域）。
var ApplyUnitVideoAssetsHook func(script map[string]any, resourceID, videoURI, thumbRel,
	generatedAt string, mode SignatureMode, signature string) (*string, error)

// reference_video 的 step1 文件名：generation_mode 维度、跨 content_mode，script 与 reference 两域共用。
const (
	ReferenceVideoStep1Filename       = "step1_reference_units.json"
	ReferenceVideoStep1LegacyFilename = "step1_reference_units.md"
	ReferenceVideoStep1QuarantineName = "step1_reference_units.invalid.json"
	ReferenceVideoStep2QuarantineName = "step2_reference_script.invalid.json"
)
