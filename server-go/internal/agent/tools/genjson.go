package tools

import (
	"github.com/shotwise/shotwise/internal/service/reference"
	"github.com/shotwise/shotwise/internal/service/script"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/shotwise/shotwise/internal/domain"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/service"
)

// 剧本生成 / 参考视频链共享的 LLM 输出解析与参考上下文辅助。

// MaxInstructionsLen 是 instructions 入参长度上限（超长会失控 token 用量并稀释模型对原文的处理）。
const MaxInstructionsLen = 4000

// TextMaxOutputTokens 是剧本类文本生成的输出上限（对应 lib/text_backends/base.DEFAULT_MAX_OUTPUT_TOKENS）。
const TextMaxOutputTokens = 64000

// ReadJSONObject 读 JSON 文件并要求顶层为对象；文件缺失返回 (nil, nil)。
func ReadJSONObject(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return nil, nil
	}
	return data, nil
}

// TargetLanguage 取项目输出语言：source_language 为唯一真相源，缺省回退中文。
func TargetLanguage(pf *domain.ProjectFile) string {
	if pf != nil && strings.TrimSpace(pf.SourceLanguage) != "" {
		return pf.SourceLanguage
	}
	return "中文"
}

// NormalizeInstructions 归一 instructions 入参：空白视同未传；超长返回参数错误文案。
func NormalizeInstructions(raw string) (string, string) {
	if len([]rune(raw)) > MaxInstructionsLen {
		return "", fmt.Sprintf("❌ 参数错误：instructions 过长（%d 字符，上限 %d），请精简后重试",
			len([]rune(raw)), MaxInstructionsLen)
	}
	return strings.TrimSpace(raw), ""
}

// UsesReferenceVideoUnits 判定项目是否产出参考视频 unit——隔离草稿只在这条路径上有意义。
func UsesReferenceVideoUnits(pf *domain.ProjectFile) bool {
	if pf == nil || pf.ContentMode == "ad" {
		return false
	}
	return pf.IsReferenceVideo()
}

// ReferenceResolver 把视频能力读取器提升为参考路径所需的完整能力解析器。
func ReferenceResolver(caps VideoCapabilityReader) (service.VideoCapabilityResolver, bool) {
	resolver, ok := caps.(service.VideoCapabilityResolver)
	return resolver, ok
}

// ReferenceContextForTool 为参考路径解析运行时能力上下文（拆分档位 + 逐 unit 取档 context）。
func ReferenceContextForTool(
	ctx context.Context, pf *domain.ProjectFile, caps VideoCapabilityReader,
) (script.ScriptReferenceContext, error) {
	resolver, ok := ReferenceResolver(caps)
	if !ok {
		return script.ScriptReferenceContext{}, fmt.Errorf("无法解析视频模型能力：参考路径需要完整视频能力解析器")
	}
	splitCaps, err := reference.FetchReferenceSplitCaps(ctx, resolver, pf)
	if err != nil {
		return script.ScriptReferenceContext{}, err
	}
	dctx := service.ResolveProjectDurationContext(ctx, resolver, pf, provider.VideoBucketForGenerationMode(pf.GenerationMode))
	return script.ScriptReferenceContext{Caps: splitCaps, DurationCtx: dctx}, nil
}
