package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/domain/media"
	domainproject "github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/infra/db"
)

// 跨域共享的小工具（原散落各域文件，目录化后上收根包）。

// ContainsString 判定切片是否含某值。
func ContainsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// StringField 取 map 里的字符串字段，缺失 / 类型不符记空串。
func StringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// ContainsInt 判定 int 切片是否含某值。
func ContainsInt(list []int, value int) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// DerefString 解引用字符串指针，nil 视为空串。
func DerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ParseJSONValue 把库里的 JSON 文本解析回对象；解析失败时返回 nil（列内容由本服务写入，视为可信）。
func ParseJSONValue(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	return v
}

// IsoformatTime 近似 Python datetime.isoformat()：小数秒按微秒输出，为 0 时省略。
func IsoformatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.999999-07:00")
}

// NullableISO 可空时间列的时间戳；未设置时返回 nil。
func NullableISO(ft db.FlexibleTime) any {
	if !ft.Valid {
		return nil
	}
	return IsoformatTime(ft.Time)
}

func CanonicalString(v any) (string, error) {
	b, err := media.CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// NowISO 当前时间 RFC3339 纳秒串（生成产物时间戳口径）。
func NowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// fmtString 近似 Python str() 的字段值字符串化（ID 匹配用）。
func fmtString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
	}
	return fmt.Sprint(v)
}

// pythonStrLike 复刻 Python str() 的文本形态（nil 记空串，布尔按 True/False）。
func PythonStrLike(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	default:
		return fmt.Sprint(v)
	}
}

// aspectRatioFor 移植 server/services/generation_tasks.py::get_aspect_ratio：
// 角色与多视图资产固定横版；其余读项目顶层字段（字符串或按资源类型分字典），
// 缺失时按 content_mode 推导（narration/ad 竖屏，drama 横屏）。
func AspectRatioFor(project *domainproject.ProjectFile, resourceType string) string {
	if resourceType == "characters" || resourceType == "scenes" || resourceType == "props" || resourceType == "products" {
		return "16:9"
	}
	if project.AspectRatio != "" {
		return project.AspectRatio
	}
	if project.ContentMode == string(domainproject.ContentNarration) || project.ContentMode == string(domainproject.ContentAd) {
		return "9:16"
	}
	return "16:9"
}

// ContentFingerprintOfData 计算已解析 JSON 对象的指纹：规范化 dump（键序 / 空白重排不改指纹）的 sha256。
//
// 规范形状与 Python 侧 content_fingerprint_of_data 对齐：键排序、紧凑分隔符、不转义非 ASCII。
func ContentFingerprintOfData(data any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(data) // 末尾带换行，去掉后与 Python 的 dumps 一致。
	canonical := strings.TrimRight(buf.String(), "\n")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func PromptYAMLScalar(value string) string {
	if value == "" {
		return `""`
	}
	unsafe := strings.Contains(value, ": ") || strings.Contains(value, " #") ||
		value != strings.TrimSpace(value) || strings.ContainsAny(value, "\n\r\t") ||
		strings.HasPrefix(value, "-") || strings.HasPrefix(value, "?") ||
		strings.HasPrefix(value, "&") || strings.HasPrefix(value, "*") ||
		strings.HasPrefix(value, "!") || strings.HasPrefix(value, "|") ||
		strings.HasPrefix(value, ">") || strings.HasPrefix(value, "%") ||
		strings.HasPrefix(value, "@") || strings.HasPrefix(value, "`") ||
		strings.HasPrefix(value, "'") || strings.HasPrefix(value, "\"") ||
		strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{")
	if !unsafe {
		return value
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

// EpisodeDraftsDir 返回该集 step1 草稿目录 {project}/drafts/episode_N。
func EpisodeDraftsDir(projectDir string, episode int) string {
	return filepath.Join(projectDir, "drafts", fmt.Sprintf("episode_%d", episode))
}

// NormalizeDurations 去重并升序；非正整数忽略。
func NormalizeDurations(durations []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(durations))
	for _, d := range durations {
		if d <= 0 || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}

// DurationConstraint 构造时长枚举约束：单值用 const、多值用 enum。
func DurationConstraint(durations []int) map[string]any {
	values := make([]any, 0, len(durations))
	for _, d := range durations {
		values = append(values, d)
	}
	if len(values) == 1 {
		return map[string]any{"const": values[0]}
	}
	return map[string]any{"enum": values}
}

// ParseJSONObject 剥离 markdown 代码栅栏后解析为 JSON 对象（顶层须为对象）。
func ParseJSONObject(responseText string) (map[string]any, error) {
	text := StripJSONCodeFences(responseText)
	var root map[string]any
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("结构异常：顶层应为对象")
	}
	return root, nil
}

// StripJSONCodeFences 剥离 LLM 输出最外层的 markdown 代码栅栏（对应 lib/text_utils.strip_json_code_fences）。
var (
	openingFenceRe = regexp.MustCompile(`^(` + "`" + "`" + "`" + `)[ \t]*(?i:json)?[ \t]*\n?`)
	closingFenceRe = regexp.MustCompile(`\n?(` + "`" + "`" + "`" + `)$`)
)

func StripJSONCodeFences(text string) string {
	out := strings.TrimSpace(text)
	out = openingFenceRe.ReplaceAllString(out, "")
	out = closingFenceRe.ReplaceAllString(out, "")
	return strings.TrimSpace(out)
}

// RejectUnknownKeys 拒绝白名单之外的键（对应 pydantic extra="forbid"）。
func RejectUnknownKeys(m map[string]any, allowed []string, loc string) error {
	set := StringSet(allowed)
	for key := range m {
		if !set[key] {
			return fmt.Errorf("%s: 不允许的字段 %q", loc, key)
		}
	}
	return nil
}

// CoerceDigitInt 机械强转：纯数字字符串 → int；整数数值原样；其余返回 0。
func CoerceDigitInt(value any) int {
	return IntValueOrZero(value)
}

// IntValueOrZero 取整数语义的值（JSON 数字的整数值 / 纯数字字符串），否则 0。
func IntValueOrZero(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		if v == float64(int(v)) {
			return int(v)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}

func StringValue(value any, fallback string) string {
	if s, ok := value.(string); ok {
		return s
	}
	return fallback
}

func StringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

// WriteFileAtomic 原子落盘：同目录临时文件 + rename，避免半截文件被前端读到。
func WriteFileAtomic(target string, content []byte) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, target)
}

// PositiveIntValue 取正整数语义的值（JSON 数字的整数值算整数；布尔不算）。
func PositiveIntValue(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, v > 0
	case int64:
		return int(v), v > 0
	case float64:
		if v != float64(int(v)) || int(v) <= 0 {
			return 0, false
		}
		return int(v), true
	}
	return 0, false
}

// GetProjectMediaAssetHook 由 media 包注册（依赖倒置：画布源校验在 generation 域，媒体索引归 media 域）。
var GetProjectMediaAssetHook func(projectRoot, mediaAssetID string) (map[string]any, error)

// truthy 复刻 Python 真值判断（nil / 空串 / 数字 0 / false / 空容器为假）。
func PyTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case int:
		return v != 0
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	default:
		return true
	}
}

// ReadJSONObjectOrNone 读 JSON 文件并要求顶层为对象；缺失 / 损坏 / 非对象返回 (nil, nil)。
func ReadJSONObjectOrNone(path string) (map[string]any, error) {
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

// RegisterMediaAssetHook 与其参数类型由 media 包注册/承载（依赖倒置：generation 画布链路写索引）。
var RegisterMediaAssetHook func(p RegisterMediaAssetHookParams) (*RegisteredMediaAsset, error)

// RegisterMediaAssetHookParams 是 RegisterMediaAsset 的 Hook 入参（避免根包依赖 media 类型）。
type RegisterMediaAssetHookParams struct {
	ProjectID       string
	ProjectRoot     string
	RelativePath    string
	Origin          string
	WorkflowRunID   *string
	WorkflowNodeKey *string
	ProviderID      *string
	ModelID         *string
	PromptSnapshot  *string
	OriginalName    *string
}

// RegisteredMediaAsset 是画布登记结果的最小投影（generation 消费侧只需这几个字段）。
type RegisteredMediaAsset struct {
	ID           string
	Kind         string
	OriginalName string
	PhysicalPath string
	MimeType     string
	Width        int
	Height       int
}
