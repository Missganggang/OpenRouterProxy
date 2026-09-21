package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
)

// 本文件实现规格书 8.11「用户接口」：用户与用户分组的 CRUD、
// 密码 / 订阅 Token 重置、用户级流量与规则，以及无需认证的订阅输出。
//
// 访问控制规则（**重要，所有 handler 都必须遵守**）：
//
//	管理员（middleware.IsAdmin）  可读写全部用户；
//	普通用户                     只能读取「自己」这一条记录与自己的规则，
//	                             任何越权请求一律返回 40301，不做降级返回。
//
// 明确的非目标（规格书 4.2.1）：**没有注册入口**，账号由管理员手工创建；
// 本文件不涉及任何支付、订单、套餐、许可证或域名绑定语义。

// userResponse 是用户详情响应。
//
// 在 model.User 之外附加 subscribe_url（前端用户抽屉里要直接展示订阅地址）。
// 用嵌入而不是「另建一个 DTO」的原因：model.User 的字段就是前端表格的全部列，
// 嵌入可以保证新增字段时响应自动跟上，不需要同步维护两份结构。
type userResponse struct {
	model.User
	// SubscribeURL 是该用户的订阅地址，形如 {面板地址}/sub/{token}。
	SubscribeURL string `json:"subscribe_url"`
}

// userResponseOf 把用户模型包装为响应结构（方法形式，便于复用 h.app）。
//
// 参数 c 为上下文（用于推导面板地址）；u 为用户。
// 返回带订阅地址的响应结构。
func (h *Handlers) userResponseOf(c *gin.Context, u *model.User) userResponse {
	return userResponse{
		User:         *u,
		SubscribeURL: h.userSubscriptionURL(c, u),
	}
}

// userSubscriptionURL 生成用户的订阅地址。
//
// 参数 c 为上下文；u 为用户。返回订阅地址；用户或 Token 为空时返回空串。
func (h *Handlers) userSubscriptionURL(c *gin.Context, u *model.User) string {
	if u == nil || strings.TrimSpace(u.Token) == "" {
		return ""
	}
	return h.app.User.SubscriptionURL(baseURL(c), u.Token)
}

// userSelfOnly 判断当前请求是否被「普通用户只能看自己」的规则限制。
//
// 管理员不受限制；普通用户传 0 视为「自己」。
// 参数 c 为上下文；id 为请求中的目标用户 ID。返回（生效的 ID，是否越权）。
func userSelfOnly(c *gin.Context, id uint64) (uint64, bool) {
	if middleware.IsAdmin(c) {
		return id, false
	}
	me := middleware.CurrentUserID(c)
	if id == 0 || id == me {
		return me, false
	}
	return id, true
}

// -------------------- 用户 CRUD --------------------

// ListUsers 返回用户列表（规格书 8.11 GET /users）。
//
// 查询参数：keyword / group_id / role / status / page / page_size。
//
// 访问控制：管理员可列出全部用户；普通用户无论传什么筛选条件，
// 都只会返回「自己」这一条记录（这是最小暴露面的做法——
// 用户表里有流量、限速等敏感字段，不该让普通用户读到别人的）。
func (h *Handlers) ListUsers(c *gin.Context) {
	page, pageSize := pageParams(c)

	filter := app.UserListFilter{
		Keyword:  strings.TrimSpace(c.Query("keyword")),
		GroupID:  queryUint64(c, "group_id", 0),
		Role:     strings.TrimSpace(c.Query("role")),
		Page:     page,
		PageSize: pageSize,
	}
	if v := queryInt(c, "status", -1); v == model.StatusEnabled || v == model.StatusDisabled {
		status := v
		filter.Status = &status
	}

	ctx := c.Request.Context()
	if !middleware.IsAdmin(c) {
		// 普通用户：直接按自己的 ID 查，完全忽略其它筛选条件。
		me := middleware.CurrentUserID(c)
		if me == 0 {
			response.Fail(c, response.New(response.CodeForbidden, "无法确定当前用户身份"))
			return
		}
		self, err := h.app.User.Get(ctx, me)
		if err != nil {
			response.Fail(c, err)
			return
		}
		items := []userResponse{h.userResponseOf(c, self)}
		response.List(c, items, 1, pageSize, 1)
		return
	}

	items, total, err := h.app.User.List(ctx, filter)
	if err != nil {
		response.Fail(c, err)
		return
	}
	out := make([]userResponse, 0, len(items))
	for i := range items {
		out = append(out, h.userResponseOf(c, &items[i]))
	}
	response.List(c, out, page, pageSize, total)
}

// CreateUser 手工创建用户（规格书 8.11 POST /users，规格书 4.2.1 无注册入口）。
//
// 请求体含明文密码，创建成功后密码立即以哈希落库，明文不会出现在响应里。
// 路由层已经限制为管理员，这里再校验一次，避免将来路由改动时静默放开。
func (h *Handlers) CreateUser(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以创建用户"))
		return
	}
	var in app.UserInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", err.Error())
		return
	}

	user, err := h.app.User.Create(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 审计由 UserService.Create 内部写入，这里不再重复写一条。
	response.OK(c, h.userResponseOf(c, user))
}

// GetUser 返回用户详情（规格书 8.11 GET /users/:id）。
//
// 响应额外带 subscribe_url，供前端用户抽屉直接展示订阅地址。
//
// 访问控制：普通用户访问自己（或省略 ID）时按「自己」处理；
// 访问别人一律 40301。
func (h *Handlers) GetUser(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "用户 ID 必须是正整数")
		return
	}
	target, forbidden := userSelfOnly(c, id)
	if forbidden {
		response.Fail(c, response.New(response.CodeForbidden, "只能查看自己的账号信息"))
		return
	}

	user, err := h.app.User.Get(c.Request.Context(), target)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, h.userResponseOf(c, user))
}

// UpdateUser 更新用户（规格书 8.11 PUT /users/:id）。
//
// 密码留空表示不改动密码（见 app.UserInput 的注释）。
// 路由层已限制为管理员，这里再校验一次作为纵深防御。
func (h *Handlers) UpdateUser(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以修改用户"))
		return
	}
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "用户 ID 必须是正整数")
		return
	}
	var in app.UserInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", err.Error())
		return
	}

	// 不允许把管理员自己降级：那会把面板锁死（没有别的管理员可用了）。
	if me := middleware.CurrentUserID(c); me == id && in.Role == model.RoleUser {
		response.Fail(c, response.New(response.CodeParamInvalid, "不能把自己降级为普通用户"))
		return
	}

	user, err := h.app.User.Update(c.Request.Context(), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 审计由 UserService.Update 内部写入。
	response.OK(c, h.userResponseOf(c, user))
}

// DeleteUser 删除用户（规格书 8.11 DELETE /users/:id）。
//
// 查询参数 `?force=true` 表示连同该用户名下的转发规则一起删除；
// 不传时若用户仍有规则，返回 40903 提示先处理规则。
func (h *Handlers) DeleteUser(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以删除用户"))
		return
	}
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "用户 ID 必须是正整数")
		return
	}
	// 不允许删除自己：删除后当前会话立即失效，容易造成「面板进不去」。
	if me := middleware.CurrentUserID(c); me == id {
		response.Fail(c, response.New(response.CodeParamInvalid, "不能删除当前登录的账号"))
		return
	}

	force := queryBoolValue(c, "force")
	if err := h.app.User.Delete(c.Request.Context(), id, force); err != nil {
		response.Fail(c, err)
		return
	}
	// 审计由 UserService.Delete 内部写入。
	response.OK(c, nil)
}

// -------------------- 密码 / Token / 状态 --------------------

// ResetUserPassword 由管理员重置用户密码（规格书 8.11 POST /users/:id/reset-password）。
//
// 响应中返回**一次性明文密码**——这是唯一一次能看到它的机会，
// 因此 message 明确提示操作者立即复制。响应体不会被记入日志。
func (h *Handlers) ResetUserPassword(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以重置密码"))
		return
	}
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "用户 ID 必须是正整数")
		return
	}
	operator := middleware.CurrentUserID(c)

	plain, err := h.app.User.ResetPassword(c.Request.Context(), operator, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 审计由 UserService.ResetPassword（委托 AuthService）内部写入，
	// 且只记「重置了谁的密码」，不记明文。
	response.OKMessage(c, "密码已重置，请立即复制保存（明文只显示这一次）", gin.H{
		"id":       id,
		"password": plain,
	})
}

// ResetUserToken 重置用户的订阅 Token（规格书 8.11 POST /users/:id/reset-token）。
//
// 重置后旧订阅地址立即失效，新 Token 的订阅地址在新响应里给出。
func (h *Handlers) ResetUserToken(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "用户 ID 必须是正整数")
		return
	}
	// 普通用户只能重置自己的 Token；管理员可重置任意用户。
	if !middleware.IsAdmin(c) && middleware.CurrentUserID(c) != id {
		response.Fail(c, response.New(response.CodeForbidden, "只能重置自己的订阅 Token"))
		return
	}

	token, err := h.app.User.ResetToken(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 审计由 UserService.ResetToken 内部写入。
	response.OKMessage(c, "订阅 Token 已重置，旧订阅地址已失效", gin.H{
		"id":            id,
		"token":         token,
		"subscribe_url": h.app.User.SubscriptionURL(baseURL(c), token),
	})
}

// DisableUser 禁用用户（规格书 8.11 POST /users/:id/disable）。
//
// 禁用后：登录被拒绝，入口准入判定直接拒绝其新连接（规格书 6.9 的准入口径）。
func (h *Handlers) DisableUser(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以禁用用户"))
		return
	}
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "用户 ID 必须是正整数")
		return
	}
	// 禁用自己会立刻把自己锁在门外，直接拒绝。
	if me := middleware.CurrentUserID(c); me == id {
		response.Fail(c, response.New(response.CodeParamInvalid, "不能禁用当前登录的账号"))
		return
	}

	if err := h.app.User.SetStatus(c.Request.Context(), id, false); err != nil {
		response.Fail(c, err)
		return
	}
	// 审计由 UserService.SetStatus 内部写入。
	response.OKMessage(c, "用户已禁用", gin.H{"id": id, "status": model.StatusDisabled})
}

// -------------------- 用户级流量与规则 --------------------

// UserTraffic 返回某个用户的流量统计（规格书 8.11 GET /users/:id/traffic）。
//
// 响应为 app.UserTraffic：已用 / 剩余 / 百分比 + 今日、昨日、本月、累计
// + 按规则的分布。不涉及任何充值或消费口径（规格书 6.10 只有流量维度）。
func (h *Handlers) UserTraffic(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "用户 ID 必须是正整数")
		return
	}
	target, forbidden := userSelfOnly(c, id)
	if forbidden {
		response.Fail(c, response.New(response.CodeForbidden, "只能查看自己的流量统计"))
		return
	}

	data, err := h.app.User.Traffic(c.Request.Context(), target, bytesMode(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, data)
}

// UserRules 返回某个用户拥有的全部规则（规格书 8.11 GET /users/:id/rules）。
//
// 普通用户只能查自己的规则列表。
func (h *Handlers) UserRules(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "用户 ID 必须是正整数")
		return
	}
	target, forbidden := userSelfOnly(c, id)
	if forbidden {
		response.Fail(c, response.New(response.CodeForbidden, "只能查看自己的转发规则"))
		return
	}

	items, err := h.app.User.Rules(c.Request.Context(), target)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.ForwardRule{}
	}
	response.OK(c, items)
}

// -------------------- 用户分组 --------------------

// ListUserGroups 返回全部用户分组（规格书 8.11 GET /user-groups）。
//
// 分组里承载的是默认的限速与限制策略，因此只有管理员能查看完整策略；
// 普通用户调用时返回 40301（分组信息对它没有用途，前端也不展示）。
func (h *Handlers) ListUserGroups(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以查看用户分组"))
		return
	}
	items, err := h.app.User.ListGroups(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.UserGroup{}
	}
	response.OK(c, items)
}

// CreateUserGroup 新建用户分组（规格书 8.11 POST /user-groups）。
func (h *Handlers) CreateUserGroup(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以创建用户分组"))
		return
	}
	var in app.UserGroupParam
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", err.Error())
		return
	}
	group, err := h.app.User.CreateGroup(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 审计由 UserService.CreateGroup 内部写入。
	response.OK(c, group)
}

// GetUserGroup 返回单个用户分组（规格书 8.11 GET /user-groups/:id）。
func (h *Handlers) GetUserGroup(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以查看用户分组"))
		return
	}
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}
	group, err := h.app.User.GetGroup(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, group)
}

// UpdateUserGroup 更新用户分组（规格书 8.11 PUT /user-groups/:id）。
func (h *Handlers) UpdateUserGroup(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以修改用户分组"))
		return
	}
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}
	var in app.UserGroupParam
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", err.Error())
		return
	}
	group, err := h.app.User.UpdateGroup(c.Request.Context(), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 审计由 UserService.UpdateGroup 内部写入。
	response.OK(c, group)
}

// DeleteUserGroup 删除用户分组（规格书 8.11 DELETE /user-groups/:id）。
//
// 分组下仍有用户时 service 会返回 40903，避免出现悬空的 group_id 引用。
func (h *Handlers) DeleteUserGroup(c *gin.Context) {
	if !middleware.IsAdmin(c) {
		response.Fail(c, response.New(response.CodeForbidden, "只有管理员可以删除用户分组"))
		return
	}
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "分组 ID 必须是正整数")
		return
	}
	if err := h.app.User.DeleteGroup(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	// 审计由 UserService.DeleteGroup 内部写入。
	response.OK(c, nil)
}

// -------------------- 订阅（无需认证） --------------------

// Subscribe 输出某个订阅 Token 对应的配置文本（规格书 8.11 `GET /sub/:token`）。
//
// 该端点**无需认证**，Token 本身就是凭据；因此：
//
//   - 返回 text/plain（用户直接用浏览器打开就能看懂，脚本也能解析）；
//   - Token 无效或已过期时返回 40401，不透露「这个 Token 曾经存在过」之类的信息；
//   - 输出内容只含入口地址、端口与目标，绝不含密码、密钥或其它用户的数据；
//   - 路由层已挂 SubscribeRateLimit（规格书 8.17：10 次 / 分钟 / Token）。
func (h *Handlers) Subscribe(c *gin.Context) {
	token := strings.TrimSpace(c.Param("token"))
	if token == "" {
		response.Fail(c, response.New(response.CodeNotFound, "订阅 Token 不能为空"))
		return
	}

	text, err := h.app.User.Subscribe(c.Request.Context(), token)
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(text))
}
