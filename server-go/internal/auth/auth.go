// Package auth 实现 JWT / API Key / 短时效下载 token 三种凭据，以及 gin 鉴权中间件。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/shotwise/shotwise/internal/apperr"
	"golang.org/x/crypto/bcrypt"
)

// 有效期常量，与 Python 侧一致。
const (
	TokenExpirySeconds         = 7 * 24 * 3600
	DownloadTokenExpirySeconds = 300
	APIKeyPrefix               = "shotwise-"
)

// User 是鉴权后的用户身份。查库失败一律降级为 user（fail-closed，不静默给 admin）。
type User struct {
	ID      string
	Sub     string
	Role    string
	AuthVia string // "jwt" | "apikey"
}

func (u *User) IsAdmin() bool { return u.Role == "admin" }

// Service 提供签发与校验。
type Service struct {
	secret   []byte
	enabled  bool
	username string
	passHash []byte // bcrypt 摘要；为空表示未设置密码
	lookup   UserLookup
}

// UserLookup 从库里解析 role。找不到就降级为 user。
type UserLookup interface {
	RoleOf(sub string) string
	APIKeyUser(keyHash string) (*User, bool)
}

// New 构造鉴权服务。secret 为空时生成运行时随机密钥（重启后 token 失效）。
func New(secret []byte, enabled bool, lookup UserLookup) *Service {
	if len(secret) == 0 {
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}
	return &Service{secret: secret, enabled: enabled, lookup: lookup}
}

// Enabled 是否启用认证。
func (s *Service) Enabled() bool { return s.enabled }

// SetCredentials 设置本地用户名与 bcrypt 密码摘要。
func (s *Service) SetCredentials(username string, hash []byte) {
	s.username = username
	s.passHash = hash
}

// CheckCredentials 校验用户名与密码。用户名用恒定时间比对，密码走 bcrypt。
// 认证关闭时一律放行（对应 Python 侧 AUTH_ENABLED=false 的匿名模式）。
func (s *Service) CheckCredentials(username, password string) bool {
	if !s.enabled {
		return true
	}
	if len(s.passHash) == 0 {
		return false
	}
	nameOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.username)) == 1
	pwdOK := bcrypt.CompareHashAndPassword(s.passHash, []byte(password)) == nil
	// 刻意不短路：用户名不匹配时仍跑一次 bcrypt，避免时序侧信道。
	return nameOK && pwdOK
}

// RoleOf 解析用户角色；查库失败或无记录一律降级 user（fail-closed）。
func (s *Service) RoleOf(sub string) string {
	if !s.enabled {
		return "admin"
	}
	if s.lookup != nil {
		if r := s.lookup.RoleOf(sub); r != "" {
			return r
		}
	}
	return "user"
}

// CreateToken 签发 7 天有效的 JWT。
func (s *Service) CreateToken(sub string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": sub,
		"iat": now.Unix(),
		"exp": now.Add(TokenExpirySeconds * time.Second).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// CreateDownloadToken 签发绑定项目的 300s 下载 token。
// 校验时会同时比对 purpose 与 project，防止拿 A 项目的 token 下 B 项目。
func (s *Service) CreateDownloadToken(sub, project, userID string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":     sub,
		"project": project,
		"purpose": "download",
		"user_id": userID,
		"iat":     now.Unix(),
		"exp":     now.Add(DownloadTokenExpirySeconds * time.Second).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// DownloadClaims 是下载 token 的解析结果。
type DownloadClaims struct {
	Sub     string
	Project string
	UserID  string
}

// VerifyDownloadToken 校验下载 token，要求 purpose 与 project 都匹配。
func (s *Service) VerifyDownloadToken(token, project string) (*DownloadClaims, error) {
	var claims jwt.MapClaims
	if _, err := jwt.ParseWithClaims(token, &claims, s.keyFunc); err != nil {
		return nil, err
	}
	if claims["purpose"] != "download" {
		return nil, errors.New("token purpose mismatch")
	}
	if claims["project"] != project {
		return nil, errors.New("token project mismatch")
	}
	return &DownloadClaims{
		Sub:     stringFrom(claims["sub"]),
		Project: stringFrom(claims["project"]),
		UserID:  stringFrom(claims["user_id"]),
	}, nil
}

// ParseToken 校验 JWT 并返回 sub。
func (s *Service) ParseToken(token string) (string, error) {
	var claims jwt.MapClaims
	if _, err := jwt.ParseWithClaims(token, &claims, s.keyFunc); err != nil {
		return "", err
	}
	sub := stringFrom(claims["sub"])
	if sub == "" {
		return "", errors.New("missing sub claim")
	}
	return sub, nil
}

func (s *Service) keyFunc(*jwt.Token) (any, error) { return s.secret, nil }

// GenerateAPIKey 生成 `shotwise-<32hex>` 形式的 key，并返回明文与其 SHA-256。
// 明文只在创建响应里出现一次。
func GenerateAPIKey() (plaintext, hash string, err error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate api key: %w", err)
	}
	plaintext = APIKeyPrefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, hex.EncodeToString(sum[:]), nil
}

// HashAPIKey 计算 key 摘要。
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// --- gin 中间件 ---

const userContextKey = "shotwise.user"

// unauthorized 统一产出 401。
func unauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", "Bearer")
	apperr.Abort(c, apperr.NewText(http.StatusUnauthorized, "unauthorized", nil))
}

// Middleware 标准 Bearer 鉴权：header Authorization 或 api key。
// AUTH_ENABLED=false 时全放行，返回固定 admin（对应 Python 的匿名模式）。
func (s *Service) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.enabled {
			c.Set(userContextKey, &User{ID: "default", Sub: "admin", Role: "admin", AuthVia: "jwt"})
			c.Next()
			return
		}
		token := bearerToken(c)
		if token == "" {
			unauthorized(c)
			return
		}
		u, err := s.resolve(token)
		if err != nil || u == nil {
			unauthorized(c)
			return
		}
		c.Set(userContextKey, u)
		c.Next()
	}
}

// FlexibleMiddleware 用于 SSE：header 优先，回落到 ?token= 查询参数
// （EventSource 无法自定义 header，对应 Python 的 CurrentUserFlexible）。
func (s *Service) FlexibleMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.enabled {
			c.Set(userContextKey, &User{ID: "default", Sub: "admin", Role: "admin", AuthVia: "jwt"})
			c.Next()
			return
		}
		token := bearerToken(c)
		if token == "" {
			token = c.Query("token")
		}
		if token == "" {
			unauthorized(c)
			return
		}
		u, err := s.resolve(token)
		if err != nil || u == nil {
			unauthorized(c)
			return
		}
		c.Set(userContextKey, u)
		c.Next()
	}
}

// RequireAdmin 非 admin 返回 403。
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		u := Current(c)
		if u == nil || !u.IsAdmin() {
			apperr.Abort(c, apperr.NewText(http.StatusForbidden, "forbidden", nil))
			return
		}
		c.Next()
	}
}

// Current 取出上下文中的用户。
func Current(c *gin.Context) *User {
	v, ok := c.Get(userContextKey)
	if !ok {
		return nil
	}
	u, _ := v.(*User)
	return u
}

// resolve 分派：shotwise- 前缀走 API Key，否则走 JWT。
func (s *Service) resolve(token string) (*User, error) {
	if strings.HasPrefix(token, APIKeyPrefix) {
		if s.lookup == nil {
			return nil, errors.New("api key lookup unavailable")
		}
		u, ok := s.lookup.APIKeyUser(HashAPIKey(token))
		if !ok {
			return nil, errors.New("unknown api key")
		}
		return u, nil
	}
	sub, err := s.ParseToken(token)
	if err != nil {
		return nil, err
	}
	role := "user"
	if s.lookup != nil {
		if r := s.lookup.RoleOf(sub); r != "" {
			role = r
		}
	}
	return &User{ID: sub, Sub: sub, Role: role, AuthVia: "jwt"}, nil
}

func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func stringFrom(v any) string {
	s, _ := v.(string)
	return s
}
