package providers

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// gemini 族共享基础设施：两种接入形态（AI Studio 走 API key、Vertex AI 走 OAuth2 服务账号）
// 在鉴权、端点拼接、模型默认值上分叉，故统一收口在此，供 gemini_image.go / gemini_video.go 复用。
//
// 与 Python 侧 lib/gemini_shared.py + google-genai SDK 的对应关系：Python 经 SDK 发请求，
// 本移植直接走 REST（NewAPI/其它家亦如此），故请求体用 SDK 序列化后的 REST JSON 形状
// （camelCase 字段）手工构造。

const (
	// geminiProviderID 是计费 / 日志归因用的供应商标识（Python PROVIDER_GEMINI）。
	// 注意与 backend 的 Name() 不同：Name() 是 gemini-aistudio / gemini-vertex。
	geminiProviderID = "gemini"

	// geminiBackendAistudio / geminiBackendVertex 是 backend_type 取值。
	geminiBackendAistudio = "aistudio"
	geminiBackendVertex   = "vertex"

	// geminiDefaultImageModel / geminiDefaultVideoModel 与 Python cost_calculator 的
	// DEFAULT_IMAGE_MODEL / DEFAULT_VIDEO_MODEL 同值。
	geminiDefaultImageModel = "gemini-3.1-flash-image-preview"
	geminiDefaultVideoModel = "veo-3.1-lite-generate-preview"

	// geminiAistudioDefaultBaseURL 是 AI Studio（Gemini Developer API）默认端点根；
	// 对应 google-genai SDK 的默认 base_url。尾部带 / 供路径拼接。
	geminiAistudioDefaultBaseURL = "https://generativelanguage.googleapis.com/"
	// geminiVertexDefaultBaseURL 是 Vertex AI（location=global）默认端点根。
	geminiVertexDefaultBaseURL = "https://aiplatform.googleapis.com/"
	// geminiVertexLocation 与 Python 构造 SDK client 时固定传的 location 一致。
	geminiVertexLocation = "global"

	// geminiAistudioAPIVersion / geminiVertexAPIVersion 是 REST 路径里的 API 版本段，
	// 分别对应 SDK 对 MLDev / Vertex 端点默认拼接的版本。
	geminiAistudioAPIVersion = "v1beta"
	geminiVertexAPIVersion   = "v1beta1"

	// geminiHTTPTimeout 是单次出站请求的墙钟预算。
	geminiHTTPTimeout = 600 * time.Second
)

// geminiVertexScopes 是 Vertex AI 服务账号所需 OAuth scopes（Python VERTEX_SCOPES）。
var geminiVertexScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/generative-language",
}

// geminiServiceAccount 是服务账号凭证文件里本实现用到的字段。
type geminiServiceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// geminiAuth 持有一次 backend 实例的鉴权与端点信息。
type geminiAuth struct {
	backendType string
	baseURL     string
	apiKey      string

	// 以下仅 vertex 模式使用。
	projectID   string
	clientEmail string
	privateKey  *rsa.PrivateKey
	tokenURI    string

	client *httpc.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// newGeminiAuth 按 backend_type 构造鉴权信息。
//
// aistudio：api_key 必填（缺失即 fail loud，与 Python resolve_gemini_api_key 同义）；
// vertex：读取 credentials_path 指向的服务账号 JSON，缺失时报「未找到 Vertex AI 凭证文件」。
// base_url 两种模式都接受覆盖（缺省用各自默认端点根），便于中转 / 本地网关与离线测试。
func newGeminiAuth(opts provider.BackendOptions) (*geminiAuth, error) {
	backendType := strings.ToLower(strings.TrimSpace(opts.Extra["backend_type"]))
	if backendType == "" {
		backendType = geminiBackendAistudio
	}
	auth := &geminiAuth{
		backendType: backendType,
		client:      httpc.NewClient(geminiHTTPTimeout),
	}

	if backendType == geminiBackendVertex {
		credPath := strings.TrimSpace(opts.Extra["credentials_path"])
		if credPath == "" {
			return nil, errors.New("未找到 Vertex AI 凭证文件")
		}
		if err := auth.loadServiceAccount(credPath); err != nil {
			return nil, err
		}
		auth.baseURL = geminiNormalizeBaseURL(opts.BaseURL, geminiVertexDefaultBaseURL)
		return auth, nil
	}

	apiKey := strings.TrimSpace(opts.APIKey)
	if apiKey == "" {
		return nil, errors.New("请到系统配置页填写 Gemini API Key")
	}
	auth.apiKey = apiKey
	auth.baseURL = geminiNormalizeBaseURL(opts.BaseURL, geminiAistudioDefaultBaseURL)
	return auth, nil
}

// loadServiceAccount 读取并解析服务账号 JSON（含 PEM 私钥）。
func (a *geminiAuth) loadServiceAccount(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 Vertex AI 凭证文件失败: %w", err)
	}
	var sa geminiServiceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return fmt.Errorf("解析 Vertex AI 凭证文件失败: %w", err)
	}
	key, err := geminiParseRSAPrivateKey(sa.PrivateKey)
	if err != nil {
		return err
	}
	a.projectID = sa.ProjectID
	a.clientEmail = sa.ClientEmail
	a.privateKey = key
	a.tokenURI = strings.TrimSpace(sa.TokenURI)
	if a.tokenURI == "" {
		a.tokenURI = "https://oauth2.googleapis.com/token"
	}
	return nil
}

// geminiParseRSAPrivateKey 解析 PEM 私钥，兼容 PKCS#8（PRIVATE KEY）与 PKCS#1（RSA PRIVATE KEY）。
func geminiParseRSAPrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("服务账号私钥不是合法 PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("服务账号私钥不是 RSA 密钥")
		}
		return rsaKey, nil
	}
	rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析服务账号私钥失败: %w", err)
	}
	return rsaKey, nil
}

// geminiNormalizeBaseURL 去空白并按需补尾部 /；空值回落默认端点根。
func geminiNormalizeBaseURL(raw, fallback string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = fallback
	}
	if !strings.HasSuffix(trimmed, "/") {
		trimmed += "/"
	}
	return trimmed
}

// headers 返回本次请求的鉴权头。
func (a *geminiAuth) headers(ctx context.Context) (map[string]string, error) {
	if a.backendType == geminiBackendVertex {
		token, err := a.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]string{"Authorization": "Bearer " + token}, nil
	}
	// AI Studio 用 API key；REST 侧标准承载头是 x-goog-api-key。
	return map[string]string{"x-goog-api-key": a.apiKey}, nil
}

// accessToken 返回（并缓存）服务账号换取到的 OAuth2 access token。
func (a *geminiAuth) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && time.Now().Before(a.tokenExpiry) {
		token := a.token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()

	assertion, err := a.signAssertion()
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 Vertex AI access token 失败: %w", err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := decodeJSON(resp.Body, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", errors.New("Vertex AI access token 响应缺少 access_token")
	}
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	a.mu.Lock()
	a.token = tok.AccessToken
	// 提前 60 秒过期，避免边界处用到即将失效的 token。
	a.tokenExpiry = time.Now().Add(ttl - time.Minute)
	a.mu.Unlock()
	return tok.AccessToken, nil
}

// signAssertion 生成 RS256 签名的 JWT assertion（OAuth2 服务账号授权流程）。
func (a *geminiAuth) signAssertion() (string, error) {
	if a.privateKey == nil {
		return "", errors.New("缺少服务账号私钥，无法签署 Vertex AI 断言")
	}
	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iss":   a.clientEmail,
		"scope": strings.Join(geminiVertexScopes, " "),
		"aud":   a.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := geminiBase64URL(headerJSON) + "." + geminiBase64URL(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("签署 Vertex AI 断言失败: %w", err)
	}
	return signingInput + "." + geminiBase64URL(signature), nil
}

// geminiBase64URL 是 JWT 各段所用的无填充 base64url 编码。
func geminiBase64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// imageURL 拼接图片生成端点。
func (a *geminiAuth) imageURL(model string) string {
	return httpc.JoinURL(a.baseURL, a.modelPath(model)+":generateContent")
}

// videoURL 拼接视频生成（长任务）端点。
func (a *geminiAuth) videoURL(model string) string {
	return httpc.JoinURL(a.baseURL, a.modelPath(model)+":predictLongRunning")
}

// operationURL 拼接长任务状态查询端点：operation.name 已含版本内的资源路径。
func (a *geminiAuth) operationURL(name string) string {
	return httpc.JoinURL(a.baseURL, a.apiVersion()+"/"+strings.TrimLeft(name, "/"))
}

// modelPath 返回模型资源路径（含 API 版本段，不含尾部动作）。
func (a *geminiAuth) modelPath(model string) string {
	if a.backendType == geminiBackendVertex {
		return fmt.Sprintf(
			"%s/projects/%s/locations/%s/publishers/google/models/%s",
			geminiVertexAPIVersion, a.projectID, geminiVertexLocation, model,
		)
	}
	return fmt.Sprintf("%s/models/%s", geminiAistudioAPIVersion, model)
}

// apiVersion 返回本模式的 API 版本段。
func (a *geminiAuth) apiVersion() string {
	if a.backendType == geminiBackendVertex {
		return geminiVertexAPIVersion
	}
	return geminiAistudioAPIVersion
}

// geminiRequestJSON 发送 JSON 请求体，按提交阶段（非幂等 POST）语义重试。
//
// 经 SubmitPost 把「请求可能已送达」的传输错误转成终态失败，连接类失败交 SubmitRetryable
// 重试，避免重复建任务 / 重复计费。
func geminiRequestJSON(
	ctx context.Context, client *httpc.Client, policy provider.RetryPolicy, target string,
	headers map[string]string, payload any,
) (*httpc.Response, error) {
	var resp *httpc.Response
	err := provider.RunWithRetry(ctx, policy, provider.SubmitRetryable, func(ctx context.Context) error {
		result, err := SubmitPost(ctx, geminiProviderID, func(ctx context.Context) (*httpc.Response, error) {
			return client.PostJSON(ctx, target, headers, payload)
		})
		if err != nil {
			return err
		}
		resp = result
		return nil
	})
	return resp, err
}
