package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/response"
)

// Scope 是 API Token 的权限范围常量（规格书 8.2）。
//
// 所有对外接口都 MUST 声明所需 Scope，前端 SPA（Session/JWT 认证）
// 不受 Scope 限制，只受角色限制。
const (
	ScopeNodeRead    = "node:read"
	ScopeNodeWrite   = "node:write"
	ScopeNodeExec    = "node:exec"
	ScopeGroupRead   = "group:read"
	ScopeGroupWrite  = "group:write"
	ScopeRuleRead    = "rule:read"
	ScopeRuleWrite   = "rule:write"
	ScopeUserRead    = "user:read"
	ScopeUserWrite   = "user:write"
	ScopeTrafficRead = "traffic:read"
	ScopeSystemRead  = "system:read"
	ScopeSystemWrite = "system:write"
	ScopeMigrateRun  = "migrate:run"
	ScopeBackupRun   = "backup:run"
)

// AllScopes 返回全部可用 Scope，供创建 Token 时的多选列表使用。
func AllScopes() []string {
	return []string{
		ScopeNodeRead, ScopeNodeWrite, ScopeNodeExec,
		ScopeGroupRead, ScopeGroupWrite,
		ScopeRuleRead, ScopeRuleWrite,
		ScopeUserRead, ScopeUserWrite,
		ScopeTrafficRead,
		ScopeSystemRead, ScopeSystemWrite,
		ScopeMigrateRun, ScopeBackupRun,
	}
}

// RequireScope 返回校验 API Token Scope 的中间件。
//
// 行为：
//   - Session / JWT 认证（浏览器与 SPA）不受 Scope 限制，直接放行；
//   - API Token 认证必须持有指定 Scope，否则返回 40302；
//   - 管理员角色同样不受 Scope 限制（便于用 Token 做全量运维脚本时
//     仍可通过角色授权，但默认创建 Token 时建议只授予必要 Scope）。
//
// 参数 scopes 为「满足任意一个即可」的范围列表。
// 返回 gin.HandlerFunc。
func RequireScope(scopes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := CurrentAPIToken(c)
		if tok == nil {
			// 非 API Token 认证：角色中间件已经做过校验，这里放行。
			c.Next()
			return
		}

		for _, s := range scopes {
			if tok.HasScope(s) {
				c.Next()
				return
			}
		}

		response.Abort(c, response.New(response.CodeScopeMissing,
			"该 API Token 缺少所需权限: "+joinScopes(scopes)))
	}
}

// joinScopes 把 Scope 列表拼成可读文本，用于错误提示。
func joinScopes(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	out := scopes[0]
	for _, s := range scopes[1:] {
		out += " 或 " + s
	}
	return out
}

// RequireOwnerOrAdmin 返回「仅资源所有者或管理员可操作」的中间件。
//
// 参数 ownerID 从请求路径/查询参数中提取资源归属的用户 ID。
// 返回 gin.HandlerFunc。
//
// 用法：用于用户级别的接口，防止普通用户操作他人数据。
func RequireOwnerOrAdmin(ownerID func(c *gin.Context) uint64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if IsAdmin(c) {
			c.Next()
			return
		}
		uid := CurrentUserID(c)
		if ownerID != nil && ownerID(c) == uid {
			c.Next()
			return
		}
		response.Abort(c, response.New(response.CodeForbidden, ""))
	}
}
