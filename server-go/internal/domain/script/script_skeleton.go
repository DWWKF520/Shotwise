// 剧本骨架知识的单一真相源（纯逻辑，零外部依赖）。对应 lib/script_skeleton.py。
//
// 把「剧本按骨架种类如何组织」这一结构事实收归一处：一张以骨架种类为键的窄表，
// 加上两个把 content_mode / generation_mode 两轴交互收口的解析器。
//
// 行为不进表：结构校验器映射、编辑白名单留各消费方本地。
package script

import (
	"fmt"

	"github.com/shotwise/shotwise/internal/domain/project"
)

// ScriptSkeleton 是某个骨架种类的结构事实：每项 id 字段名 / 每项角色名单字段名。
//
// 键（骨架种类）本身即剧本里的条目数组键。CharsField 为空表示该骨架无逐条角色名单字段
// （video_units 的角色以 references 中 type == "character" 的条目形态存在），
// 消费方拿到空串必须显式决策，不得当字段名去取空值。
type ScriptSkeleton struct {
	IDField    string
	CharsField string
}

// ScriptSkeletons 把骨架种类映射到其结构事实。
var ScriptSkeletons = map[string]ScriptSkeleton{
	"segments":    {IDField: "segment_id", CharsField: "characters_in_segment"},
	"scenes":      {IDField: "scene_id", CharsField: "characters_in_scene"},
	"shots":       {IDField: "shot_id", CharsField: "characters_in_shot"},
	"video_units": {IDField: "unit_id"},
}

// scriptSkeletonKinds 是骨架种类的完整集合（表自洽校验与遍历共用）。
var scriptSkeletonKinds = []string{"segments", "scenes", "shots", "video_units"}

// ScriptSkeletonItemNouns / EntityTypes / AnchorTypes 是按骨架种类推导的事件元数据。
var (
	ScriptSkeletonItemNouns = map[string]string{
		"segments": "分镜", "scenes": "场景", "shots": "镜头", "video_units": "视频单元",
	}
	ScriptSkeletonEntityTypes = map[string]string{
		"segments": "segment", "scenes": "drama_scene", "shots": "shot", "video_units": "reference_unit",
	}
	ScriptSkeletonAnchorTypes = map[string]string{
		"segments": "segment", "scenes": "segment", "shots": "segment", "video_units": "reference_unit",
	}
)

// ScriptSkeletonOf 取骨架种类的结构事实；种类非法时返回 ok=false。
func ScriptSkeletonOf(kind string) (ScriptSkeleton, bool) {
	skeleton, ok := ScriptSkeletons[kind]
	return skeleton, ok
}

// ResolveDeclaredKind 规范解析：由项目声明的 (content_mode, generation_mode) 定骨架种类。
//
//	ad      → shots（恒定，不随生成路径变）
//	narration / drama + reference_video → video_units
//	narration → segments，drama → scenes
//
// 未知或缺失 content_mode 时返回错误（fail loud，不静默默认到 drama/narration）。
// 手持剧本 dict 的消费方走 ResolveScriptKind。
func ResolveDeclaredKind(contentMode, generationMode string) (string, error) {
	switch contentMode {
	case string(project.ContentAd):
		return "shots", nil
	case string(project.ContentNarration):
		if generationMode == string(project.GenerationReferenceVideo) {
			return "video_units", nil
		}
		return "segments", nil
	case string(project.ContentDrama):
		if generationMode == string(project.GenerationReferenceVideo) {
			return "video_units", nil
		}
		return "scenes", nil
	}
	return "", fmt.Errorf("未知或缺失 content_mode: %q", contentMode)
}

// ResolveScriptKind 取证解析：由剧本 dict 判别当前的分镜数组种类。
//
// 只看数据形状——回答的是「这份剧本现在长什么样」，与项目声明无关，因此骨架与项目路线
// 失配的剧本同样可读可编辑。生成路径不走本解析（由生成入口按路线分派并经 EnsureRouteSkeleton 拒绝失配）。
//
// 判别顺序：
//  1. video_units 在场且 segments / scenes / shots 都不在 → video_units
//  2. content_mode 为权威（drama → scenes，narration → segments，ad → shots）
//  3. content_mode=narration 但数据落 scenes 键（无 segments）的历史遗留兼容
//  4. content_mode 缺失时按顶层键存在性推断
func ResolveScriptKind(script map[string]any) string {
	_, hasVideoUnits := script["video_units"]
	hasStoryboard := scriptHasAnyKey(script, "segments", "scenes", "shots")
	if hasVideoUnits && !hasStoryboard {
		return "video_units"
	}
	switch mode, _ := script["content_mode"].(string); mode {
	case string(project.ContentAd):
		return "shots"
	case string(project.ContentDrama):
		return "scenes"
	case string(project.ContentNarration):
		// 畸形脚本兼容：content_mode=narration 但数据实际落在 scenes 键下（无 segments 键）。
		if _, hasSegments := script["segments"]; !hasSegments {
			if _, hasScenes := script["scenes"]; hasScenes {
				return "scenes"
			}
		}
		return "segments"
	}
	_, hasSegments := script["segments"]
	if _, hasScenes := script["scenes"]; hasScenes && !hasSegments {
		return "scenes"
	}
	if _, hasShots := script["shots"]; hasShots && !hasSegments {
		return "shots"
	}
	return "segments"
}

// SkeletonRouteMismatchError 表示剧本骨架与项目生成路线不属同一族——生成被拒。
//
// 失配剧本的唯一出路是重拆重生成：路线创建时锁定，剧本不可就地换族。
// 查看 / 编辑 / 导出不经本闸门，失配剧本仍可读可改可导出。
// Actual 为空表示剧本一个骨架数组都没有（畸形或半成品剧本）。
type SkeletonRouteMismatchError struct {
	Expected       string
	Actual         string
	HasActual      bool
	GenerationMode string
}

func (e *SkeletonRouteMismatchError) Error() string {
	route := "分镜路线"
	if e.GenerationMode == string(project.GenerationReferenceVideo) {
		route = "参考生视频路线"
	}
	if !e.HasActual {
		return fmt.Sprintf("剧本没有任何骨架数组，%s要求 %s：请先拆出内容再生成", route, e.Expected)
	}
	return fmt.Sprintf("剧本骨架为 %s，与%s要求的 %s 不属同一族：请按当前路线重新拆分生成", e.Actual, route, e.Expected)
}

// EnsureRouteSkeleton 是生成入口的路线闸门：确认剧本骨架属于项目路线要求的族，返回剧本实际骨架种类。
//
// 判据是「路线要读的那个数组在不在」，两个分支对称按键在场性判定：参考路线只问 video_units，
// 分镜路线只问 segments / scenes / shots 有没有一个在场。族内的历史形态差异（narration 数据落
// scenes 键）照实放行并原样返回取证解析的答案；三个键全缺时不能放行（ResolveScriptKind 会按
// content_mode 合成一个族内答案，顺着走下去会落进假成功）。
//
// 两个分支都只问键在不在、不问值的类型："video_units": {} 这类脏数据是类型错误而非路线失配，
// 报错权归下游的「必须是数组」校验。
//
// contentMode 未知或缺失时返回 ResolveDeclaredKind 的错误。
func EnsureRouteSkeleton(script map[string]any, contentMode, generationMode string) (string, error) {
	expected, err := ResolveDeclaredKind(contentMode, generationMode)
	if err != nil {
		return "", err
	}
	_, hasReference := script["video_units"]
	hasStoryboard := scriptHasAnyKey(script, "segments", "scenes", "shots")
	if expected == "video_units" {
		if hasReference {
			return "video_units", nil
		}
		return "", newRouteMismatch(expected, script, hasStoryboard, generationMode)
	}
	if hasStoryboard {
		return ResolveScriptKind(script), nil
	}
	return "", newRouteMismatch(expected, script, hasReference, generationMode)
}

func newRouteMismatch(expected string, script map[string]any, hasOther bool, generationMode string) error {
	mismatch := &SkeletonRouteMismatchError{Expected: expected, GenerationMode: generationMode}
	if hasOther {
		mismatch.Actual = ResolveScriptKind(script)
		mismatch.HasActual = true
	}
	return mismatch
}

func scriptHasAnyKey(script map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := script[key]; ok {
			return true
		}
	}
	return false
}
