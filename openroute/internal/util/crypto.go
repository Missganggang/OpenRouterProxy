// Package util 提供全项目共用的纯函数工具：加密、IP 匹配、JSON、校验。
//
// 本包不依赖任何业务模型，保持无副作用，便于单测与复用。
package util

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost 取 10：个人自用面板，登录频率低，
// 10 在安全性与登录耗时（约 50ms）之间较为平衡。
const bcryptCost = 10

// HashPassword 用 bcrypt 生成密码哈希。
//
// 参数 password 为明文密码；返回可存入 users.password_hash 的哈希串。
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("密码不能为空")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("生成密码哈希失败: %w", err)
	}
	return string(b), nil
}

// CheckPassword 校验明文密码与哈希是否匹配。
//
// 返回 nil 表示匹配；返回非 nil 表示不匹配或哈希格式非法。
// 调用方不应把底层错误直接暴露给用户，统一转成 40104。
func CheckPassword(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// IsBcryptHash 判断字符串是否为 bcrypt 哈希格式。
//
// 迁移场景使用：源库的密码哈希若为其它算法（md5/sha256 等），
// 迁移工具据此标记该用户需要重置密码。
func IsBcryptHash(s string) bool {
	return strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$") ||
		strings.HasPrefix(s, "$2y$") || strings.HasPrefix(s, "$2$")
}

// RandomString 生成指定长度的随机字符串（base62 字符集）。
//
// 用于节点密钥、订阅 Token、API Token 等。
// 长度建议不低于 32；实现使用 crypto/rand，不是伪随机。
func RandomString(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if n <= 0 {
		return "", fmt.Errorf("随机字符串长度必须为正数")
	}
	var b strings.Builder
	b.Grow(n)
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("生成随机数失败: %w", err)
		}
		b.WriteByte(alphabet[idx.Int64()])
	}
	return b.String(), nil
}

// MustRandomString 是 RandomString 的便捷版本，失败时返回空串。
//
// 仅用于「失败也不影响主流程」的非关键场景，例如生成请求 ID。
func MustRandomString(n int) string {
	s, err := RandomString(n)
	if err != nil {
		return ""
	}
	return s
}

// GenerateNodeToken 生成节点密钥，前缀 nsk_ 便于识别。
func GenerateNodeToken() (string, error) {
	s, err := RandomString(32)
	if err != nil {
		return "", err
	}
	return "nsk_" + s, nil
}

// GenerateAPIToken 生成对外 API Token，前缀 ort_ 便于识别。
//
// 返回值为明文，仅在创建时返回一次；库中只存其 SHA256 哈希。
func GenerateAPIToken() (string, error) {
	s, err := RandomString(40)
	if err != nil {
		return "", err
	}
	return "ort_" + s, nil
}

// GenerateUserToken 生成用户级订阅 Token（用于 /sub/<token>）。
func GenerateUserToken() (string, error) {
	s, err := RandomString(32)
	if err != nil {
		return "", err
	}
	return "ortu_" + s, nil
}

// SHA256Hex 返回字符串的 SHA256 十六进制摘要。
//
// 用于 API Token 落库、备份校验和、迁移行校验和等。
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// SHA256HexBytes 返回字节切片的 SHA256 十六进制摘要。
func SHA256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HMACSHA256Hex 计算 HMAC-SHA256 并返回十六进制字符串。
//
// 用于 API Token 的请求签名与 Webhook 的出站签名。
func HMACSHA256Hex(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// BuildAPISignature 构造 API Token 的签名原文（规格书 8.2）。
//
//	sign = HMAC-SHA256(token_secret, method + "\n" + path + "\n" + timestamp + "\n" + body_sha256)
//
// 参数 method 为 HTTP 方法（建议大写）；path 为请求路径（不含 query）；
// timestamp 为 Unix 秒字符串；body 为原始请求体，内部会做 SHA256。
func BuildAPISignature(secret, method, path, timestamp string, body []byte) string {
	bodyHash := SHA256HexBytes(body)
	payload := strings.Join([]string{method, path, timestamp, bodyHash}, "\n")
	return HMACSHA256Hex(secret, payload)
}

// MaskSecret 对密钥做脱敏展示，只保留首尾少量字符。
//
// 用于日志与备份清单中展示 token 而不泄露完整值。
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	// 太短的直接全部打码，避免「脱敏后反而能推断出原值」。
	if len(s) <= 12 {
		return strings.Repeat("*", len(s))
	}
	return s[:6] + "..." + s[len(s)-4:]
}

// RandomPassword 生成一个可读性尚可的随机初始密码（16 位 base62）。
func RandomPassword() (string, error) {
	return RandomString(16)
}

// Base64Std 返回标准 base64 编码，用于需要嵌入 JSON 的二进制数据。
func Base64Std(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
