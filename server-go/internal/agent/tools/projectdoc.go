package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shotwise/shotwise/internal/infra/fs"
)

// 项目 / 剧本文档读取与 JSON 解析的共享辅助(原 agent 包 tools_review.go 的通用部分)。

// VideoPathFor 解析单元的 video_clip。无 clip 时返回空串且无错误；越界或非法路径返回错误。
// 绝对路径一律视为越界（Python 侧会校验其是否落在项目内，此处有意收紧）。
func VideoPathFor(projectDir string, assets map[string]any) (string, string, error) {
	clip, ok := AsString(assets["video_clip"])
	if !ok || strings.TrimSpace(clip) == "" {
		return "", "", nil
	}
	if filepath.IsAbs(clip) || filepath.VolumeName(clip) != "" {
		return "", "", &fs.PathTraversalError{Msg: "path escapes base directory"}
	}
	abs, err := fs.SafeJoin(projectDir, false, clip)
	if err != nil {
		return "", "", err
	}
	return abs, strings.ReplaceAll(clip, "\\", "/"), nil
}

// LoadRawProject 读取 project.json 的原始对象。保留全部字段（含 current_episode 等），
// 与 Python 侧 project dict 的口径一致，不经过域结构体裁剪。
func LoadRawProject(dir string) (map[string]any, error) {
	var raw map[string]any
	if err := fs.ReadJSON(filepath.Join(dir, "project.json"), &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
}

// LoadScript 按单段文件名读取 scripts/ 下的剧本（同 ProjectStore.ReadScriptJSON 的防穿越口径）。
func LoadScript(projectDir, name string) (map[string]any, error) {
	if err := ValidateScriptFilename(name); err != nil {
		return nil, err
	}
	p, err := fs.SafeResolve(filepath.Join(projectDir, "scripts"), name)
	if err != nil {
		return nil, fmt.Errorf("剧本文件不可用: %s", name)
	}
	var script map[string]any
	if err := fs.ReadJSON(p, &script); err != nil {
		return nil, err
	}
	if script == nil {
		script = map[string]any{}
	}
	return script, nil
}

// ValidateScriptFilename 拒绝任何非纯文件名的 script 参数（同 Python validate_script_filename）。
func ValidateScriptFilename(v string) error {
	if v == "" {
		return errors.New("script 文件名不能为空")
	}
	if strings.ContainsAny(v, `/\`) || v == "." || v == ".." {
		return fmt.Errorf("script 必须是纯文件名，禁止路径分隔符: %q", v)
	}
	return nil
}

// relativeScriptName 取 episodes[].script_file 的文件名部分；非法或缺失时返回空串。
func relativeScriptName(v any) string {
	s, ok := AsString(v)
	if !ok || strings.TrimSpace(s) == "" {
		return ""
	}
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	if ValidateScriptFilename(s) != nil {
		return ""
	}
	return s
}

func ScriptCandidates(project map[string]any) []string {
	var out []string
	episodes, _ := AsList(project["episodes"])
	for _, raw := range episodes {
		ep, ok := AsMap(raw)
		if !ok {
			continue
		}
		name := relativeScriptName(ep["script_file"])
		if name != "" && !containsReviewString(out, name) {
			out = append(out, name)
		}
	}
	return out
}

func EpisodeMetaFor(project map[string]any, scriptName string) map[string]any {
	episodes, _ := AsList(project["episodes"])
	for _, raw := range episodes {
		ep, ok := AsMap(raw)
		if ok && relativeScriptName(ep["script_file"]) == scriptName {
			return ep
		}
	}
	return map[string]any{}
}

// ChooseScript 决定审阅哪一集（同 Python _choose_script）。
func ChooseScript(project map[string]any, requested *string) (string, error) {
	if requested != nil {
		if err := ValidateScriptFilename(*requested); err != nil {
			return "", err
		}
		return *requested, nil
	}
	candidates := ScriptCandidates(project)
	for _, key := range []string{"current_episode", "active_episode", "selected_episode"} {
		selected, ok := project[key].(float64)
		if !ok || selected != math.Trunc(selected) {
			continue
		}
		for _, name := range candidates {
			if ep, ok := AsNumber(EpisodeMetaFor(project, name)["episode"]); ok && ep == selected {
				return name, nil
			}
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) > 0 {
		return "", errors.New("无法确定当前剧集，请传入 script（例如 episode_1.json）")
	}
	return "", errors.New("project.json 中没有可用的 episodes[].script_file")
}

// ResolveContentMode 对应 resolve_content_mode：剧本级缺失时回退到项目级，最终默认 narration。
// 非字符串值返回空串，与任何内容模式都不匹配。
func ResolveContentMode(script, project map[string]any) string {
	if v, ok := script["content_mode"]; ok {
		s, _ := AsString(v)
		return s
	}
	if v, ok := project["content_mode"]; ok {
		s, _ := AsString(v)
		return s
	}
	return "narration"
}

// ItemCollection 选择单元集合键并返回其中的对象条目（同 Python _item_collection）。
func ItemCollection(script, project map[string]any) (string, []map[string]any) {
	contentMode := ResolveContentMode(script, project)
	generationMode, _ := AsString(project["generation_mode"])
	var key string
	switch {
	case generationMode == "reference_video" && contentMode == "ad":
		key = "reference_units"
	case generationMode == "reference_video":
		key = "video_units"
	case contentMode == "ad":
		key = "shots"
	case contentMode == "drama":
		key = "scenes"
	default:
		key = "segments"
	}
	raw, _ := AsList(script[key])
	var items []map[string]any
	for _, v := range raw {
		if m, ok := AsMap(v); ok {
			items = append(items, m)
		}
	}
	return key, items
}

// ReviewItemID 取单元 ID：按 unit_id / scene_id / segment_id / shot_id 顺序，去空白后非空即用。
func ReviewItemID(item map[string]any) string {
	for _, key := range []string{"unit_id", "scene_id", "segment_id", "shot_id"} {
		if s, ok := AsString(item[key]); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// PlannedDuration 取规划时长：duration_seconds 优先，其次 duration；须为非负数。
func PlannedDuration(item map[string]any) (float64, bool) {
	for _, key := range []string{"duration_seconds", "duration"} {
		if v, ok := AsNumber(item[key]); ok && v >= 0 {
			return v, true
		}
	}
	return 0, false
}

// ReviewItemTexts 收集镜头文案：每个镜头取第一个非空字段，单元自身的字段全部收集。
func ReviewItemTexts(item map[string]any) []string {
	texts := []string{}
	textKeys := []string{"video_prompt", "image_prompt", "text", "action", "camera_language"}
	shots, _ := AsList(item["shots"])
	for _, raw := range shots {
		shot, ok := AsMap(raw)
		if !ok {
			continue
		}
		for _, key := range textKeys {
			if s, ok := AsString(shot[key]); ok && strings.TrimSpace(s) != "" {
				texts = append(texts, strings.TrimSpace(s))
				break
			}
		}
	}
	for _, key := range textKeys {
		if s, ok := AsString(item[key]); ok && strings.TrimSpace(s) != "" {
			texts = append(texts, strings.TrimSpace(s))
		}
	}
	return texts
}

// ReviewItemReferences 收集单元的参考列表，跳过名称为空（Python 真值判断）的条目。
func ReviewItemReferences(item map[string]any) []ReviewRef {
	refs := []ReviewRef{}
	list, _ := AsList(item["references"])
	for _, raw := range list {
		ref, ok := AsMap(raw)
		if !ok || !Truthy(ref["name"]) {
			continue
		}
		refType := ""
		if t, ok := ref["type"]; ok {
			refType = pyStr(t)
		}
		refs = append(refs, ReviewRef{Type: refType, Name: pyStr(ref["name"])})
	}
	return refs
}

type ReviewRef struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// BoolIs 判断 m[key] 是否为指定布尔值（Python 的 `is True` / `is False`）。
func BoolIs(m map[string]any, key string, want bool) bool {
	v, ok := m[key].(bool)
	return ok && v == want
}

func containsReviewString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// AsString / AsMap / AsList 对脏数据宽松降级：类型不符时返回零值与 false。
func AsString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func AsMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func AsList(v any) ([]any, bool) {
	l, ok := v.([]any)
	return l, ok
}

// AsNumber 对应 Python _as_number：排除布尔，接受数字与可解析的字符串；NaN/Inf 视为非数。
func AsNumber(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int:
		f = float64(x)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// Truthy 对应 Python 真值判断。
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr 近似 Python str()：用于参考名称与类型的文本化。
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// EncodeReviewJSON 输出缩进 JSON，关闭 HTML 转义，保证中文与 <>& 原样可读。
func EncodeReviewJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// OptionalFloat 把「存在且有效」的浮点数转为指针，便于 JSON 中区分缺省与零值。
func OptionalFloat(v float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &v
}

// RoundReview3 对应 Python round(x, 3)。
func RoundReview3(v float64) float64 {
	return math.Round(v*1000) / 1000
}
