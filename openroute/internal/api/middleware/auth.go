package middleware

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// AuthDeps 是认证中间件的外部依赖。
//
// 显式声明依赖而不直接引用 *app.App，是为了避免 api 包与 app 包
// 形成「app 依赖 api/response，api 依赖 app」的循环引用。
type AuthDeps struct {
	// ParseToken 解析并校验 JWT，wantType 为期望的令牌类型（access / refresh）。
	ParseToken func(tokenStr, wantType string) (*JWTClaims, error)
	// DB 用于回查用户与 API Token。
	DB *gorm.DB
	// SecretKey 是面板密钥，用于 API Token 的签名校验。
	SecretKey string
}

// JWTClaims 是中间件需要的 JWT 声明子集。
type JWTClaims struct {
	UserID    uint64
	Username  string
	Role      string
	TokenType string
}

// Authenticate 返回统一认证中间件。
//
// 按以下顺序尝试认证（规格书 8.2）：
//  1. Session Cookie `or_session`（WebUI 浏览器）
//  2. `Authorization: Bearer <jwt>`（SPA / 第三方）
//  3. `X-API-Key`（服务端对接，可选签名与 IP 白名单）
//
// 全部失败时返回 40101。认证成功后把身份写入上下文。
//
// 参数 deps 为依赖；返回 gin.HandlerFunc。
func Authenticate(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. Session Cookie
		if token, err := c.Cookie("or_session"); err == nil && token != "" {
			if claims, err := deps.ParseToken(token, "access"); err == nil {
				if setUserContext(c, deps, claims, AuthTypeSession) {
					c.Next()
					return
				}
			}
		}

		// 2. Bearer JWT
		if h := c.GetHeader("Authorization"); h != "" {
			if token, ok := strings.CutPrefix(h, "Bearer "); ok {
				token = strings.TrimSpace(token)
				if token != "" {
					if claims, err := deps.ParseToken(token, "access"); err == nil {
						if setUserContext(c, deps, claims, AuthTypeJWT) {
							c.Next()
							return
						}
					}
				}
			}
		}

		// 3. API Token
		if key := strings.TrimSpace(c.GetHeader("X-API-Key")); key != "" {
			if err := authenticateAPIToken(c, deps, key); err != nil {
				response.Abort(c, err)
				return
			}
			c.Next()
			return
		}

		response.Abort(c, response.New(response.CodeUnauthorized, ""))
	}
}

// setUserContext 回查用户确认其仍然有效，然后写入上下文。
//
// 每次都回查数据库：这样「禁用用户」能立即生效，而不必等到 JWT 过期。
// 个人自用面板请求量很低，这次查询可以接受。
//
// 返回 false 表示用户已不存在或被禁用，调用方应继续尝试其它认证方式。
func setUserContext(c *gin.Context, deps AuthDeps, claims *JWTClaims, authType string) bool {
	var u model.User
	if err := deps.DB.WithContext(c.Request.Context()).First(&u, claims.UserID).Error; err != nil {
		return false
	}
	if u.Status != model.StatusEnabled {
		return false
	}
	c.Set(CtxUserID, u.ID)
	c.Set(CtxUsername, u.Username)
	// 角色以数据库为准：令牌签发后角色可能已被修改。
	c.Set(CtxRole, u.Role)
	c.Set(CtxAuthType, authType)
	return true
}

// authenticateAPIToken 校验 X-API-Key 与可选签名。
//
// 校验步骤（规格书 8.2）：
//  1. 按 Token 哈希查库，确认存在、启用且未过期；
//  2. 若请求带了 X-API-Timestamp，则校验时间偏差（±300 秒）与 HMAC 签名；
//  3. 校验 IP 白名单。
//
// 返回具体的 AppError（40101 / 40102 / 40103 / 40303），成功返回 nil。
func authenticateAPIToken(c *gin.Context, deps AuthDeps, key string) error {
	var tok model.APIToken
	err := deps.DB.WithContext(c.Request.Context()).
		Where("token_hash = ?", util.SHA256Hex(key)).First(&tok).Error
	if err != nil {
		return response.New(response.CodeUnauthorized, "API Token 无效")
	}
	if !tok.Enabled {
		return response.New(response.CodeUnauthorized, "API Token 已被禁用")
	}
	now := time.Now().UTC()
	if tok.Expired(now) {
		return response.New(response.CodeTokenExpired, "API Token 已过期")
	}

	// 时间戳与签名校验：仅在客户端提供了时间戳时强制校验签名，
	// 这样简单脚本可以只用 X-API-Key（个人自用的便利性取舍）。
	if tsStr := strings.TrimSpace(c.GetHeader("X-API-Timestamp")); tsStr != "" {
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			return response.New(response.CodeSignatureFailed, "X-API-Timestamp 格式错误")
		}
		skew := now.Unix() - ts
		if skew < 0 {
			skew = -skew
		}
		if skew > 300 {
			return response.New(response.CodeSignatureFailed,
				"请求时间戳偏差超过 300 秒，请检查本机时钟是否同步")
		}

		sign := strings.TrimSpace(c.GetHeader("X-API-Sign"))
		if sign == "" {
			return response.New(response.CodeSignatureFailed, "缺少 X-API-Sign 请求头")
		}
		body, err := readBody(c)
		if err != nil {
			return response.Wrap(response.CodeInternal, err, "读取请求体失败")
		}
		want := util.BuildAPISignature(key, c.Request.Method, c.Request.URL.Path, tsStr, body)
		if !constantTimeEqual(sign, want) {
			return response.New(response.CodeSignatureFailed, "")
		}
	}

	// IP 白名单：列表为空表示不限制。
	if nets := util.ParseCIDRList(tok.IPWhitelist.AsSlice()); len(nets) > 0 {
		ip := util.ClientIP(c.Request)
		if !util.IPInCIDRList(ip, nets) {
			return response.New(response.CodeIPNotAllowed,
				"当前 IP "+ip+" 不在该 API Token 的白名单内")
		}
	}

	// 记录最近使用时间；失败不影响请求成败。
	_ = deps.DB.WithContext(c.Request.Context()).Model(&model.APIToken{}).
		Where("id = ?", tok.ID).Update("last_used_at", now).Error

	// API Token 使用系统身份（userID=0），权限完全由 Scope 决定。
	c.Set(CtxUserID, uint64(0))
	c.Set(CtxUsername, "api:"+tok.Name)
	c.Set(CtxRole, model.RoleAdmin)
	c.Set(CtxAuthType, AuthTypeAPIToken)
	c.Set(CtxAPIToken, &tok)
	return nil
}

// readBody 读取并还原请求体。
//
// 签名需要原始请求体，读完后必须还原，否则后续 handler 无法再解析。
// 限制最大读取 10MB，避免超大请求体耗尽内存。
func readBody(c *gin.Context) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}
	const maxBody = 10 << 20
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody))
	if err != nil {
		return nil, err
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// constantTimeEqual 以常量时间比较两个字符串，避免时序侧信道。
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// ---------------------------------------------------------------------------
// 节点认证
// ---------------------------------------------------------------------------

// NodeAuthenticate 返回节点通信接口的认证中间件。
//
// 节点使用 `X-Node-Token` 请求头认证（规格书 8.16）。
// 认证成功后把节点对象写入上下文，供 handler 直接使用。
//
// 参数 deps 为依赖；返回 gin.HandlerFunc。
func NodeAuthenticate(deps AuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimSpace(c.GetHeader("X-Node-Token"))
		if token == "" {
			response.Abort(c, response.New(response.CodeNodeTokenInvalid, "缺少 X-Node-Token 请求头"))
			return
		}

		var node model.Node
		if err := deps.DB.WithContext(c.Request.Context()).
			Where("token = ?", token).First(&node).Error; err != nil {
			response.Abort(c, response.New(response.CodeNodeTokenInvalid, ""))
			return
		}

		c.Set(CtxNodeID, node.ID)
		c.Set(CtxNode, &node)
		c.Set(CtxAuthType, AuthTypeNode)
		c.Next()
	}
}

// RequireAdmin 返回要求管理员角色的中间件。
//
// 注意：API Token 认证默认以管理员身份运行，真正的权限边界由
// RequireScope 依据 Token 的 Scope 再做一次收敛。
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		_, _, role, ok := CurrentUser(c)
		if !ok {
			response.Abort(c, response.New(response.CodeUnauthorized, ""))
			return
		}
		if role != model.RoleAdmin {
			response.Abort(c, response.New(response.CodeForbidden, ""))
			return
		}
		c.Next()
	}
}
