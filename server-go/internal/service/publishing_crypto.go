// 发布 OAuth 凭据的加密存储（对应 server/services/publishing_crypto.py）。
// 使用 AES-256-GCM，密钥来自 PUBLISHING_ENCRYPTION_KEY（urlsafe-base64，32 字节）。
package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
)

// PublishingKeyEnv 是发布凭据加密密钥的环境变量名。
const PublishingKeyEnv = "PUBLISHING_ENCRYPTION_KEY"

const (
	publishingKeyVersion = "v1"
	publishingNonceBytes = 12
	publishingKeyBytes   = 32
)

// PublishingEncryptionKeyError 表示发布凭据无法加密 / 解密。
type PublishingEncryptionKeyError struct{ Msg string }

func (e *PublishingEncryptionKeyError) Error() string { return e.Msg }

func publishingKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(PublishingKeyEnv))
	if raw == "" {
		return nil, &PublishingEncryptionKeyError{Msg: PublishingKeyEnv + " is not configured"}
	}
	decoded, err := decodeBase64URL(raw)
	if err != nil {
		return nil, &PublishingEncryptionKeyError{Msg: PublishingKeyEnv + " must be urlsafe-base64 encoded"}
	}
	if len(decoded) != publishingKeyBytes {
		return nil, &PublishingEncryptionKeyError{
			Msg: PublishingKeyEnv + " must decode to exactly 32 bytes",
		}
	}
	return decoded, nil
}

// encodeBase64URL 输出无填充的 urlsafe base64。
func encodeBase64URL(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

// decodeBase64URL 接受带或不带填充的 urlsafe base64。
func decodeBase64URL(value string) ([]byte, error) {
	trimmed := strings.TrimRight(value, "=")
	return base64.RawURLEncoding.DecodeString(trimmed)
}

// EncryptPublishingSecret 用 AES-256-GCM 加密一个 OAuth 密钥，返回带版本前缀的密文。
func EncryptPublishingSecret(value string) (string, error) {
	if value == "" {
		return "", errors.New("secret must not be empty")
	}
	key, err := publishingKey()
	if err != nil {
		return "", err
	}
	gcm, err := newPublishingGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, publishingNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(value), []byte(publishingKeyVersion))
	return publishingKeyVersion + "." + encodeBase64URL(nonce) + "." + encodeBase64URL(ciphertext), nil
}

// DecryptPublishingSecret 解密带版本前缀的密文；失败时不外泄凭据细节。
func DecryptPublishingSecret(token string) (string, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 || parts[0] != publishingKeyVersion {
		return "", &PublishingEncryptionKeyError{Msg: "invalid encrypted publishing credential"}
	}
	nonce, err := decodeBase64URL(parts[1])
	if err != nil || len(nonce) != publishingNonceBytes {
		return "", &PublishingEncryptionKeyError{Msg: "invalid encrypted publishing credential"}
	}
	ciphertext, err := decodeBase64URL(parts[2])
	if err != nil {
		return "", &PublishingEncryptionKeyError{Msg: "invalid encrypted publishing credential"}
	}
	key, err := publishingKey()
	if err != nil {
		return "", err
	}
	gcm, err := newPublishingGCM(key)
	if err != nil {
		return "", err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(publishingKeyVersion))
	if err != nil {
		return "", &PublishingEncryptionKeyError{Msg: "invalid encrypted publishing credential"}
	}
	return string(plaintext), nil
}

func newPublishingGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
