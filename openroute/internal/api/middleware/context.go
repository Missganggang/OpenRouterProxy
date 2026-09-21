// Package middleware 提供认证、鉴权、限流、审计、CORS 与日志中间件。
package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/model"
)

// 存放在 gin.Context 中的键名。
//
// 统一在这里定义，避免各处字符串拼写不一致导致取值失败。
const (
	// CtxUserID 是认证后的用户 ID（uint64）。
	CtxUserID = "or_user_id"
	// CtxUsername 是认证后的用户名（string）。
	CtxUsername = "or_username"
	// CtxRole 是认证后的角色（string，admin / user）。
	CtxRole = "or_role"
	// CtxAuthType 记录本次请求使用的认证方式（session / jwt / apitoken / node）。
	CtxAuthType = "or_auth_type"
	// CtxAPIToken 是本次请求使用的 API Token（*model.APIToken），仅 API Token 认证时存在。
	CtxAPIToken = "or_api_token"
	// CtxNodeID 是节点通信接口认证后的节点 ID（uint64）。
	CtxNodeID = "or_node_id"
	// CtxNode 是节点通信接口认证后的节点对象（*model.Node）。
	CtxNode = "or_node"
)

// 认证方式常量，用于审计日志与调试。
const (
	AuthTypeSession  = "session"
	AuthTypeJWT      = "jwt"
	AuthTypeAPIToken = "apitoken"
	AuthTypeNode     = "node"
	AuthTypePublic   = "public"
)

// SessionCookieName 是 WebUI 会话 Cookie 的名称（规格书 8.2）。
//
// 认证中间件读取它、登录接口写入它，两边必须一致，
// 因此定义在中间件包里作为唯一定义点。
const SessionCookieName = "or_session"

// CurrentUser 从上下文取出当前登录用户的基本信息。
//
// 返回 userID、username、role 与「是否已认证」。
// 未认证时返回零值且 ok 为 false。
func CurrentUser(c *gin.Context) (uint64, string, string, bool) {
	v, exists := c.Get(CtxUserID)
	if !exists {
		return 0, "", "", false
	}
	uid, ok := v.(uint64)
	if !ok {
		return 0, "", "", false
	}
	username, _ := c.Get(CtxUsername)
	role, _ := c.Get(CtxRole)
	un, _ := username.(string)
	r, _ := role.(string)
	return uid, un, r, true
}

// CurrentUserID 返回当前登录用户 ID，未认证时返回 0。
//
// 用于 service 调用前的快速取值。
func CurrentUserID(c *gin.Context) uint64 {
	uid, _, _, _ := CurrentUser(c)
	return uid
}

// IsAdmin 判断当前用户是否为管理员。
func IsAdmin(c *gin.Context) bool {
	_, _, role, ok := CurrentUser(c)
	return ok && role == model.RoleAdmin
}

// CurrentAPIToken 返回本次请求使用的 API Token，非 API Token 认证时为 nil。
func CurrentAPIToken(c *gin.Context) *model.APIToken {
	v, exists := c.Get(CtxAPIToken)
	if !exists {
		return nil
	}
	t, _ := v.(*model.APIToken)
	return t
}

// CurrentNode 返回节点通信接口的节点对象，非节点请求时为 nil。
func CurrentNode(c *gin.Context) *model.Node {
	v, exists := c.Get(CtxNode)
	if !exists {
		return nil
	}
	n, _ := v.(*model.Node)
	return n
}

// GetRequestID 返回本次请求的 ID。
//
// 命名上刻意与中间件函数 RequestID() 区分，避免调用时产生歧义。
func GetRequestID(c *gin.Context) string {
	if v, ok := c.Get("openroute_request_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
