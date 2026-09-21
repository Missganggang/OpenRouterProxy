package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/util"
)

// Health 是存活检查接口，供负载均衡与自检脚本使用。
//
// 只反映「进程能响应请求」，不检查数据库等下游依赖；
// 需要完整健康检查请用 /api/v1/system/status。
func (h *Handlers) Health(c *gin.Context) {
	response.OK(c, gin.H{
		"status":  "ok",
		"version": app.Version,
		"uptime":  h.app.Uptime(),
	})
}

// Login 处理登录（规格书 8.5）。
func (h *Handlers) Login(c *gin.Context) {
	var in app.LoginInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, response.Field(response.CodeParamInvalid, "body", nil,
			"请求体不是合法 JSON："+err.Error()))
		return
	}

	ip := util.ClientIP(c.Request)
	result, err := h.app.Auth.Login(c.Request.Context(), in, ip, c.GetHeader("User-Agent"))
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 同时下发 Session Cookie，让 WebUI 刷新页面后仍保持登录；
	// SPA 则用返回的 access_token 走 Bearer 认证。
	h.setSessionCookie(c, result.AccessToken)

	response.OK(c, result)
}

// setSessionCookie 写入会话 Cookie。
//
// 安全属性（规格书 8.2）：HttpOnly 防止 XSS 读取，SameSite=Lax 防 CSRF，
// 且只在 HTTPS 下加 Secure（面板默认用 HTTP 访问，加了会导 Cookie 无法写入）。
func (h *Handlers) setSessionCookie(c *gin.Context, token string) {
	secure := c.Request.TLS != nil ||
		strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	c.SetSameSite(httpSameSiteLax)
	c.SetCookie(
		middleware.SessionCookieName,
		token,
		int(app.AccessTokenTTL.Seconds()),
		"/",
		"",
		secure,
		true, // HttpOnly
	)
}

// Logout 清除会话 Cookie。
//
// 由于令牌是无状态的 JWT，服务端无需维护黑名单；
// 前端同时丢弃本地 access_token 即可完成登出。
func (h *Handlers) Logout(c *gin.Context) {
	h.clearSessionCookie(c)

	if uid, un, _, ok := middleware.CurrentUser(c); ok {
		h.app.Audit.Write(c.Request.Context(), app.AuditEntry{
			UserID:    uid,
			Username:  un,
			Action:    "logout",
			Resource:  "auth",
			IP:        util.ClientIP(c.Request),
			UserAgent: c.GetHeader("User-Agent"),
			Message:   "登出",
		})
	}
	response.OK(c, nil)
}

// clearSessionCookie 把会话 Cookie 置为过期。
func (h *Handlers) clearSessionCookie(c *gin.Context) {
	c.SetSameSite(httpSameSiteLax)
	c.SetCookie(middleware.SessionCookieName, "", -1, "/", "", false, true)
}

// Refresh 用刷新令牌换取新的访问令牌。
//
// 刷新令牌优先从请求体读取，其次从 Cookie 读取——
// 这样 SPA（用 body）与 WebUI（用 Cookie）都能工作。
func (h *Handlers) Refresh(c *gin.Context) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	// 请求体可能为空，忽略绑定错误。
	_ = c.ShouldBindJSON(&body)

	token := strings.TrimSpace(body.RefreshToken)
	if token == "" {
		// WebUI 场景：Cookie 里存的是 access_token，没有独立的 refresh_token，
		// 因此这里用 access_token 续期即可（会重新校验用户状态）。
		if ck, err := c.Cookie(middleware.SessionCookieName); err == nil {
			token = ck
		}
	}
	if token == "" {
		response.Fail(c, response.New(response.CodeUnauthorized, "缺少刷新令牌"))
		return
	}

	// 先按 refresh 类型解析；失败再按 access 解析（Cookie 场景）。
	result, err := h.app.Auth.Refresh(c.Request.Context(), token)
	if err != nil {
		claims, aerr := h.app.Auth.ParseToken(token, "access")
		if aerr != nil {
			response.Fail(c, err)
			return
		}
		user, uerr := h.app.Auth.Me(c.Request.Context(), claims.UserID)
		if uerr != nil {
			response.Fail(c, uerr)
			return
		}
		access, refresh, serr := h.app.Auth.IssueTokensFor(user)
		if serr != nil {
			response.Fail(c, serr)
			return
		}
		result = &app.LoginResult{
			AccessToken:  access,
			RefreshToken: refresh,
			ExpiresIn:    int(app.AccessTokenTTL.Seconds()),
			User:         user,
		}
	}

	h.setSessionCookie(c, result.AccessToken)
	response.OK(c, result)
}

// Me 返回当前登录用户的信息与权限（规格书 8.5）。
func (h *Handlers) Me(c *gin.Context) {
	uid, _, _, _ := middleware.CurrentUser(c)
	user, err := h.app.Auth.Me(c.Request.Context(), uid)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 一并返回该用户的可见范围，便于前端决定菜单与按钮的可用性。
	scopes := []string{"*"}
	if tok := middleware.CurrentAPIToken(c); tok != nil {
		scopes = tok.ScopeList()
	}
	response.OK(c, gin.H{
		"user":   user,
		"scopes": scopes,
		"auth":   authType(c),
	})
}

// authType 读取本次请求使用的认证方式。
func authType(c *gin.Context) string {
	if v, ok := c.Get(middleware.CtxAuthType); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return middleware.AuthTypePublic
}

// ChangePassword 修改当前用户自己的密码（规格书 8.5）。
func (h *Handlers) ChangePassword(c *gin.Context) {
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(body.OldPassword) == "" {
		response.Fail(c, response.Field(response.CodeParamInvalid, "old_password", nil,
			"请输入原密码"))
		return
	}
	if len(body.NewPassword) < 6 {
		response.Fail(c, response.Field(response.CodeParamInvalid, "new_password", nil,
			"新密码长度不能少于 6 位"))
		return
	}

	uid, _, _, _ := middleware.CurrentUser(c)
	if err := h.app.Auth.ChangePassword(c.Request.Context(), uid, body.OldPassword, body.NewPassword); err != nil {
		response.Fail(c, err)
		return
	}

	// 密码已变更，旧的会话 Cookie 仍然有效（JWT 无状态），
	// 但主动清掉可以促使前端重新登录，避免留下悬空会话。
	h.clearSessionCookie(c)
	response.OKMessage(c, "密码已修改，请重新登录", nil)
}

// Captcha 返回一张图形验证码（规格书 8.5）。
func (h *Handlers) Captcha(c *gin.Context) {
	id, svg := h.app.Auth.NewCaptcha()
	response.OK(c, gin.H{
		"captcha_id": id,
		"svg":        svg,
	})
}

// httpSameSiteLax 是会话 Cookie 的 SameSite 策略。
//
// 使用 Lax 而非 Strict：Strict 会导致从外部链接跳回面板时丢失登录态，
// 而 Lax 已经能阻挡绝大多数跨站表单提交类 CSRF。
const httpSameSiteLax = http.SameSiteLaxMode
