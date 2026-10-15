package domain

import "strings"

// 设计图（scene / prop / product）prompt 构造，移植自 lib/prompt_builders.py。
// 常量与拼接顺序与 Python 逐字一致：这些文本是生成结果的一部分，改动即改变产物。

const (
	sceneLayout   = "主画面占四分之三区域展示环境整体外观与氛围，右下角嵌入关键细节小图。"
	propLayout    = "三视图水平排列于纯净浅灰背景：左侧正面全视图、中间 45° 侧视图体现立体感、右侧关键细节特写。"
	productLayout = "标准多角度产品参考图，纯净浅灰背景、均匀棚拍布光：正面、45° 侧面、背面三视图水平排列，" +
		"下方一排关键细节特写（logo、文字、材质、接缝）。"

	sceneGuard = "画面中没有人物出镜，空间透视正常，陈设固定，光影统一。"
	propGuard  = "外观结构完整，焦点清晰。"

	productFidelityCore = "logo、文字、配色、材质、比例与结构不得改变或臆造"
	productGuard        = "产品外观必须忠实于参考图中的真实产品：" + productFidelityCore + "；各视图为同一件产品。" +
		"参考图中的手部、模特及其他出镜人物一律不保留，画面只呈现产品本体；" +
		"包装上印刷的人像图案属于产品外观，须原样保留。"

	negativeTailScene   = "画面避免：出镜人物、水印、多余文字、Logo。"
	negativeTailProp    = "画面避免：出镜人物、水印、多余文字、Logo。"
	negativeTailProduct = "画面避免：出镜人物、水印、多余文字、Logo。"
)

// stylePrefix 组合视觉风格前缀（两者皆空时为空串）。
func stylePrefix(style, styleDescription string) string {
	var parts []string
	if style != "" {
		parts = append(parts, "风格："+style)
	}
	if styleDescription != "" {
		parts = append(parts, "描述："+styleDescription)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n") + "\n\n"
}

// BuildScenePrompt 场景设计图 prompt（主 + 细节）。
func BuildScenePrompt(name, description, style, styleDescription string) string {
	return stylePrefix(style, styleDescription) +
		"标志性场景「" + name + "」的视觉参考。\n\n" +
		description + "\n\n" +
		sceneLayout + "\n\n" +
		sceneGuard + "\n\n" +
		negativeTailScene
}

// BuildPropPrompt 道具设计图 prompt（三视图）。
func BuildPropPrompt(name, description, style, styleDescription string) string {
	return stylePrefix(style, styleDescription) +
		"道具「" + name + "」的多视角展示。\n\n" +
		description + "\n\n" +
		propLayout + "\n\n" +
		propGuard + "\n\n" +
		negativeTailProp
}

// BuildProductPrompt 产品标准参考图 prompt。产品 sheet 不注入画风（与 Python 一致）。
func BuildProductPrompt(name, description string) string {
	return "产品「" + name + "」的标准参考图。\n\n" +
		description + "\n\n" +
		productLayout + "\n\n" +
		productGuard + "\n\n" +
		negativeTailProduct
}
