package kling

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
	"github.com/shotwise/shotwise/internal/providers/provkit"
)

// 可灵 Kling 图像/视频后端共享脚手架（端口自 lib/kling_backend_base.py + lib/kling_shared.py）。
//
// 收口两个媒体后端逐字重复的装配：鉴权头（JWT / Bearer 双模式）、base_url 归一化、submit / poll
// 的 HTTP 骨架、以及异步任务响应解析。per-medium 差异（端点路径、payload 构建、能力声明、
// job_id 编解码）留给 kling_image.go / kling_video.go。

const (
	// klingProvider 与 registry 的 provider_id 同名。
	klingProvider = "kling"
	// klingBaseURL 是官方 base（含 /v1），与 registry.DefaultBaseURL 一致。
	klingBaseURL = "https://api-beijing.klingai.com/v1"

	klingDefaultHTTPTimeout = 60 * time.Second

	// JWT 寿命与刷新策略：约 30 分钟过期；nbf 略提前容忍轻微时钟偏移；距过期 <60s 即重签，
	// 避免长任务途中 token 失效。
	klingTokenTTLSeconds           = 1800
	klingTokenNBFSkewSeconds       = 5
	klingTokenRefreshMarginSeconds = 60

	// 异步任务终态：旧 API 用 succeed，新 API 用 succeeded。
	klingStatusSucceed   = "succeed"
	klingStatusSucceeded = "succeeded"
	klingStatusFailed    = "failed"
)

// klingVersionPathRe 匹配结尾的版本路径（/v1、/v2 等），用于缺省补 /v1。
var klingVersionPathRe = regexp.MustCompile(`/v\d+$`)

// klingJWTManager 是可灵 JWT HS256 token 管理器（按需重签，时钟可注入）。
//
// payload = {"iss": access_key, "exp": now+1800, "nbf": now-5}，secret_key 签名。缓存 token，
// 每次取用前检查：距过期 >60s 复用缓存，否则重签——重点是长任务跨越单 token 寿命时自动续签。
type klingJWTManager struct {
	accessKey   string
	secretKey   string
	now         func() time.Time
	cachedToken string
	expiresAt   int64
}

// newKlingJWTManager 构造 JWT 管理器；now 缺省用墙钟，测试可注入固定时钟。
func newKlingJWTManager(accessKey, secretKey string) *klingJWTManager {
	return &klingJWTManager{accessKey: accessKey, secretKey: secretKey, now: time.Now}
}

// token 返回有效 token：距过期 >60s 复用缓存，否则按需重签。
func (m *klingJWTManager) token() string {
	now := m.now()
	if m.cachedToken != "" && m.expiresAt-now.Unix() > klingTokenRefreshMarginSeconds {
		return m.cachedToken
	}
	return m.mint(now)
}

// mint 按当前时刻签一枚新 token 并写回缓存。
func (m *klingJWTManager) mint(now time.Time) string {
	exp := now.Unix() + klingTokenTTLSeconds
	nbf := now.Unix() - klingTokenNBFSkewSeconds
	token := klingSignJWT(m.accessKey, m.secretKey, exp, nbf)
	m.cachedToken = token
	m.expiresAt = exp
	return token
}

// authHeaders 返回鉴权头（每次取用触发过期检查 + 按需重签）。
func (m *klingJWTManager) authHeaders() map[string]string {
	return klingBearerHeaders(m.token())
}

// klingSignJWT 用 HS256 签一枚 JWT：base64url(header).base64url(payload).base64url(hmac)。
func klingSignJWT(issuer, secret string, exp, nbf int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadJSON, _ := json.Marshal(map[string]any{"iss": issuer, "exp": exp, "nbf": nbf})
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// klingBearerHeaders 构造静态 Bearer 鉴权头（bearer 模式与 JWT 模式共用同一形态）。
func klingBearerHeaders(token string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}
}

// klingResolveJWTCredentials 校验并归一化可灵 JWT 双密钥；任一缺失即报错，不走 env fallback。
func klingResolveJWTCredentials(accessKey, secretKey string) (string, string, error) {
	ak := strings.TrimSpace(accessKey)
	sk := strings.TrimSpace(secretKey)
	if ak == "" || sk == "" {
		return "", "", errors.New("请到系统配置页填写可灵 Kling 的 Access Key 与 Secret Key")
	}
	return ak, sk, nil
}

// klingResolveAPIKey 校验并归一化 bearer 模式静态 api_key；缺失即报错。
func klingResolveAPIKey(apiKey string) (string, error) {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return "", errors.New("请填写可灵 Kling 的 API Key")
	}
	return key, nil
}

// klingAuthMode 按凭证形态决定 auth_mode：api_key 非空优先 bearer，否则 jwt。
//
// 显式声明的 auth_mode 优先；空串时按 api_key 是否非空推导（与 Python 装配侧同一业务规则）。
func klingAuthMode(opts provider.BackendOptions) string {
	if mode := strings.TrimSpace(opts.Extra["auth_mode"]); mode != "" {
		return mode
	}
	if strings.TrimSpace(opts.APIKey) != "" {
		return "bearer"
	}
	return "jwt"
}

// klingEnsureBaseURL 补全 /v1 版本后缀；空值回落官方 base。
func klingEnsureBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimRight(trimmed, "/")
	if trimmed == "" {
		return klingBaseURL
	}
	if !klingVersionPathRe.MatchString(trimmed) {
		trimmed += "/v1"
	}
	return trimmed
}

// klingHTTPTimeout 解析 http_timeout（秒，Extra 传入）；缺省 60s。
func klingHTTPTimeout(opts provider.BackendOptions) time.Duration {
	if raw := strings.TrimSpace(opts.Extra["http_timeout"]); raw != "" {
		if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	return klingDefaultHTTPTimeout
}

// klingBackendBase 是图像 / 视频后端共享的装配体。
type klingBackendBase struct {
	authMode     string
	model        string
	baseURL      string
	httpTimeout  time.Duration
	jwt          *klingJWTManager
	staticAPIKey string
	client       *httpc.Client
	opts         provider.BackendOptions
	// mediaLabel 仅用于 operator 日志（"图像" / "视频"），不影响行为。
	mediaLabel string
	// resolveURL 解析相对端点 → 绝对地址；视频后端用它在不改鉴权与重试骨架的前提下走 3.0 官方根路径。
	resolveURL func(endpointPath string) string
}

// newKlingBackendBase 按凭证形态装配共享脚手架。
func newKlingBackendBase(opts provider.BackendOptions, model string) (*klingBackendBase, error) {
	b := &klingBackendBase{
		model:       model,
		baseURL:     klingEnsureBaseURL(opts.BaseURL),
		httpTimeout: klingHTTPTimeout(opts),
		opts:        opts,
	}
	b.resolveURL = b.defaultRequestURL
	switch mode := klingAuthMode(opts); mode {
	case "jwt":
		ak, sk, err := klingResolveJWTCredentials(opts.Extra["access_key"], opts.Extra["secret_key"])
		if err != nil {
			return nil, err
		}
		b.authMode = mode
		b.jwt = newKlingJWTManager(ak, sk)
	case "bearer":
		key, err := klingResolveAPIKey(opts.APIKey)
		if err != nil {
			return nil, err
		}
		b.authMode = mode
		b.staticAPIKey = key
	default:
		return nil, fmt.Errorf("未知 Kling auth_mode: %s", mode)
	}
	b.client = httpc.NewClient(b.httpTimeout)
	return b, nil
}

// Name 返回供应商标识。
func (b *klingBackendBase) Name() string { return klingProvider }

// Model 返回实际下发的 registry 键名。
func (b *klingBackendBase) Model() string { return b.model }

// headers 鉴权头：jwt 模式每次调用触发过期检查 + 按需重签；bearer 模式用静态 key。
func (b *klingBackendBase) headers() map[string]string {
	if b.jwt != nil {
		return b.jwt.authHeaders()
	}
	return klingBearerHeaders(b.staticAPIKey)
}

// defaultRequestURL 把相对端点拼到当前 base_url。
func (b *klingBackendBase) defaultRequestURL(endpointPath string) string {
	return b.baseURL + "/" + strings.TrimLeft(endpointPath, "/")
}

// requestURL 解析绝对地址（视频后端可经 resolveURL 覆写走 3.0 官方根路径）。
func (b *klingBackendBase) requestURL(endpointPath string) string {
	return b.resolveURL(endpointPath)
}

// submitTask 提交非幂等「建任务 + 计费」POST，返回 task_id。
//
// provkit.SubmitPost 把歧义传输错误转 AmbiguousSubmitError 终态失败，避免重试重复建任务 + 重复计费；
// >=400 抛 HTTPStatusError 交 SubmitRetryable 按状态码分流。
func (b *klingBackendBase) submitTask(ctx context.Context, endpointPath string, payload any) (string, error) {
	var taskID string
	err := provider.RunWithRetry(ctx, provider.DefaultRetryPolicy, provider.SubmitRetryable, func(ctx context.Context) error {
		resp, err := provkit.SubmitPost(ctx, klingProvider, func(ctx context.Context) (*httpc.Response, error) {
			return b.client.PostJSON(ctx, b.requestURL(endpointPath), b.headers(), payload)
		})
		if err != nil {
			return err
		}
		id, err := klingExtractTaskIDFromBody(resp.Body)
		if err != nil {
			return err
		}
		taskID = id
		return nil
	})
	if err != nil {
		return "", err
	}
	return taskID, nil
}

// pollQuery 发起幂等 GET 并解析 JSON 响应。
func (b *klingBackendBase) pollQuery(ctx context.Context, endpointPath string) (map[string]any, error) {
	resp, err := b.client.Get(ctx, b.requestURL(endpointPath), b.headers())
	if err != nil {
		return nil, err
	}
	var payload any
	if err := provkit.DecodeJSON(resp.Body, &payload); err != nil {
		return nil, err
	}
	return klingAsDict(payload), nil
}

// pollUntilTerminal 轮询至终态，注入 Kling 终态 / 失败 / 重试谓词。
func (b *klingBackendBase) pollUntilTerminal(
	ctx context.Context,
	pollFn func(ctx context.Context) (map[string]any, error),
	pollInterval, maxWait time.Duration,
) (map[string]any, error) {
	return provkit.PollWithRetry(ctx, provkit.PollConfig[map[string]any]{
		PollFn:       pollFn,
		IsDone:       klingIsTaskTerminal,
		IsFailed:     klingTaskFailureReason,
		PollInterval: pollInterval,
		MaxWait:      maxWait,
		RetryIf:      provider.PollRetryable,
		Label:        "Kling",
		OnProgress: func(v map[string]any, elapsed time.Duration) {
			slog.Info("Kling 生成中", "media", b.mediaLabel, "status", klingTaskStatus(v), "elapsed", int(elapsed.Seconds()))
		},
	})
}

// ── 异步任务响应解析 ──────────────────────────────────────────────────────

// klingAsDict 把任意值归一化为 map：非对象（nil / 数组 / 字符串）一律回空 map。
func klingAsDict(value any) map[string]any {
	if d, ok := value.(map[string]any); ok {
		return d
	}
	return map[string]any{}
}

// klingAsStr 把任意值归一化为字符串：非字符串（含显式 null）一律回空串。
func klingAsStr(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

// klingTruthyString 归一化「可作字符串的非空标量」：空串 / 零 / 非标量回空串。
func klingTruthyString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		if v == 0 {
			return ""
		}
		return klingFormatCode(v)
	}
	return ""
}

// klingFormatCode 按 Python 打印口径格式化 code / task_id 数值：整数值不带小数点。
func klingFormatCode(code float64) string {
	if code == math.Trunc(code) {
		return strconv.FormatInt(int64(code), 10)
	}
	return strconv.FormatFloat(code, 'g', -1, 64)
}

// klingIsErrorCode 判断 code 是否表示错误：0 / 缺失 / null 表成功，其余（含无法解析）表错误。
func klingIsErrorCode(code any) bool {
	switch c := code.(type) {
	case float64:
		return int(c) != 0
	case string:
		parsed, err := strconv.ParseFloat(c, 64)
		if err != nil {
			return true
		}
		return int(parsed) != 0
	default:
		return true
	}
}

// klingCodeError 从 scope 取 code / scope[messageKey] 构造错误描述；0 或缺失回空串。
func klingCodeError(scope map[string]any, messageKey string) string {
	code, present := scope["code"]
	if !present || code == nil {
		return ""
	}
	if !klingIsErrorCode(code) {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("Kling API code=%s: %s", klingFormatCodeValue(code), klingAsStr(scope[messageKey])))
}

// klingFormatCodeValue 按原值格式化 code（数值走整数口径，其余按原样）。
func klingFormatCodeValue(code any) string {
	if f, ok := code.(float64); ok {
		return klingFormatCode(f)
	}
	return fmt.Sprintf("%v", code)
}

// klingResponseError 顶层 code != 0 或 data.code（业务级 msg）!= 0 时返回错误描述，否则空串。
func klingResponseError(payload any) string {
	p := klingAsDict(payload)
	if err := klingCodeError(p, "message"); err != "" {
		return err
	}
	return klingCodeError(klingAsDict(p["data"]), "msg")
}

// klingExtractTaskIDFromBody 从提交响应体提取 data.task_id。
func klingExtractTaskIDFromBody(body []byte) (string, error) {
	var payload any
	if err := provkit.DecodeJSON(body, &payload); err != nil {
		return "", err
	}
	return klingExtractTaskID(payload)
}

// klingExtractTaskID 从提交响应提取 data.task_id（兼容 id）；缺失则按 code 错误或原样报错。
func klingExtractTaskID(submitPayload any) (string, error) {
	data := klingAsDict(klingAsDict(submitPayload)["data"])
	taskID := klingTruthyString(data["task_id"])
	if taskID == "" {
		taskID = klingTruthyString(data["id"])
	}
	if taskID == "" {
		if reason := klingResponseError(submitPayload); reason != "" {
			return "", errors.New(reason)
		}
		return "", fmt.Errorf("Kling 提交响应缺少 task_id: %v", submitPayload)
	}
	return taskID, nil
}

// klingTaskData 从旧对象型或新版列表型 data 中取出任务对象。
func klingTaskData(payload map[string]any) map[string]any {
	data := payload["data"]
	if d, ok := data.(map[string]any); ok {
		return d
	}
	if list, ok := data.([]any); ok {
		for _, item := range list {
			if d, ok := item.(map[string]any); ok {
				return d
			}
		}
	}
	return map[string]any{}
}

// klingTaskStatus 读 data.task_status（兼容 status）。
func klingTaskStatus(payload map[string]any) string {
	data := klingTaskData(payload)
	if status := klingAsStr(data["task_status"]); status != "" {
		return status
	}
	return klingAsStr(data["status"])
}

// klingIsTaskTerminal succeed / succeeded / failed 为终态；其余（含未知/空）继续轮询。
func klingIsTaskTerminal(payload map[string]any) bool {
	switch klingTaskStatus(payload) {
	case klingStatusSucceed, klingStatusSucceeded, klingStatusFailed:
		return true
	}
	return false
}

// klingTaskFailureReason task_status=failed 或顶层 code 硬错误 → 错误描述；否则空串。
func klingTaskFailureReason(payload map[string]any) string {
	if err := klingResponseError(payload); err != "" {
		return err
	}
	if klingTaskStatus(payload) != klingStatusFailed {
		return ""
	}
	data := klingTaskData(payload)
	taskID := klingTruthyString(data["task_id"])
	if taskID == "" {
		taskID = klingTruthyString(data["id"])
	}
	message := data["task_status_msg"]
	if klingAsStr(message) == "" {
		if m := data["message"]; klingAsStr(m) != "" {
			message = m
		} else if e := data["error"]; klingAsStr(e) != "" {
			message = e
		} else {
			message = ""
		}
	}
	return strings.TrimSpace(fmt.Sprintf("Kling 任务失败 task_id=%s: %s", taskID, klingAsStr(message)))
}

// klingExtractTaskResultURLs 从 succeed 响应提取 data.task_result.{collectionKey}[].url。
func klingExtractTaskResultURLs(payload map[string]any, collectionKey string) []string {
	data := klingTaskData(payload)
	result := klingAsDict(data["task_result"])
	items, isList := result[collectionKey].([]any)
	if !isList && collectionKey == "videos" {
		// Kling 3.0 查询响应把产物放在 data.outputs[].url。
		items, _ = data["outputs"].([]any)
	}
	urls := make([]string, 0, len(items))
	for _, item := range items {
		if url := klingAsStr(klingAsDict(item)["url"]); url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}

// klingExtractVideoURL 从 succeed 响应提取首个视频 URL；缺失则按 code 错误或原样报错。
func klingExtractVideoURL(payload map[string]any) (string, error) {
	for _, url := range klingExtractTaskResultURLs(payload, "videos") {
		return url, nil
	}
	if reason := klingResponseError(payload); reason != "" {
		return "", errors.New(reason)
	}
	return "", fmt.Errorf("Kling 视频任务完成但缺少视频 URL: %v", payload)
}

// klingExtractImageURLs 从 succeed 响应提取普通图或 Omni 组图 URL（按返回顺序）。
func klingExtractImageURLs(payload map[string]any) ([]string, error) {
	urls := klingExtractTaskResultURLs(payload, "images")
	if len(urls) == 0 {
		urls = klingExtractTaskResultURLs(payload, "series_images")
	}
	if len(urls) > 0 {
		return urls, nil
	}
	if reason := klingResponseError(payload); reason != "" {
		return nil, errors.New(reason)
	}
	return nil, fmt.Errorf("Kling 图像任务完成但缺少图片 URL: %v", payload)
}

// klingImageToBase64 本地图片 → 纯 base64 字符串（可灵 image / image_tail 无 data URI 前缀）。
func klingImageToBase64(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// klingBaseName 取路径文件名；无文件名（"." / 分隔符结尾）回空串。
func klingBaseName(path string) string {
	if path == "" {
		return ""
	}
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || base == "/" || base == "\\" {
		return ""
	}
	return base
}
