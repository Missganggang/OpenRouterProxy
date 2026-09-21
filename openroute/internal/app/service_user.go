package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 4.2.1 / 4.2.2 / 8.11 的用户与用户分组管理。
//
// 重要约定（规格书 4.2.1）：**没有注册入口**，管理员在面板中手工建号。
// 保留多用户的目的是实现用户级限速、用户级流量统计与规则归属，
// 因此本服务不涉及任何支付、订单、套餐、许可证或域名绑定语义。

// UserService 负责用户与用户分组的增删改查、令牌重置与订阅输出。
type UserService struct {
	app *App

	// 订阅端点的独立限流器（规格书 8.17：订阅接口 10 次 / 分钟 / Token）。
	subLimit *LimitService
}

// NewUserService 构造用户服务。
//
// 参数 a 为运行时依赖容器；返回可直接使用的实例。
func NewUserService(a *App) *UserService {
	return &UserService{
		app:      a,
		subLimit: NewLimitService(a),
	}
}

// -------------------- 用户 CRUD --------------------

// UserInput 是创建 / 更新用户的输入。
//
// 密码只在创建时必填；更新时留空表示不改密码（避免误把密码清空）。
type UserInput struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	Nickname     string `json:"nickname"`
	Role         string `json:"role"`
	GroupID      uint64 `json:"group_id"`
	TrafficLimit int64  `json:"traffic_limit"`
	SpeedLimit   int64  `json:"speed_limit"`
	IPLimit      int    `json:"ip_limit"`
	DeviceLimit  int    `json:"device_limit"`
	ConnLimit    int    `json:"conn_limit"`
	Remark       string `json:"remark"`
	// Status 为 1 启用 / 0 禁用；指针以便区分「未传」与「显式传 0」。
	Status *int `json:"status"`
}

// UserListFilter 是用户列表的筛选条件。
type UserListFilter struct {
	// Keyword 按用户名 / 昵称模糊匹配。
	Keyword  string
	GroupID  uint64
	Role     string
	Status   *int
	Page     int
	PageSize int
}

// List 分页查询用户列表。
//
// 参数 ctx；filter 为筛选条件。返回列表、总数或错误。
func (s *UserService) List(ctx context.Context, filter UserListFilter) ([]model.User, int64, error) {
	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 20
	}
	if filter.PageSize > 200 {
		filter.PageSize = 200
	}

	q := s.app.DB.WithContext(ctx).Model(&model.User{})
	if kw := trimSpace(filter.Keyword); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("username LIKE ? OR nickname LIKE ?", like, like)
	}
	if filter.GroupID > 0 {
		q = q.Where("group_id = ?", filter.GroupID)
	}
	if filter.Role != "" {
		q = q.Where("role = ?", filter.Role)
	}
	if filter.Status != nil {
		q = q.Where("status = ?", *filter.Status)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "统计用户数失败")
	}

	items := make([]model.User, 0)
	if err := q.Order("id ASC").
		Offset((filter.Page - 1) * filter.PageSize).Limit(filter.PageSize).
		Find(&items).Error; err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "查询用户列表失败")
	}
	return items, total, nil
}

// Get 按 ID 读取用户。
//
// 参数 ctx；id 为用户 ID。返回用户或错误。
func (s *UserService) Get(ctx context.Context, id uint64) (*model.User, error) {
	var user model.User
	if err := s.app.DB.WithContext(ctx).First(&user, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "用户不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询用户失败")
	}
	return &user, nil
}

// GetByToken 按订阅 Token 读取用户。
//
// 供 /sub/:token 使用：Token 是订阅端点的唯一凭据，因此这里只按 Token 查。
//
// 参数 ctx；token 为订阅 Token。返回用户或错误。
func (s *UserService) GetByToken(ctx context.Context, token string) (*model.User, error) {
	token = trimSpace(token)
	if token == "" {
		return nil, response.New(response.CodeParamInvalid, "订阅 Token 不能为空")
	}
	var user model.User
	if err := s.app.DB.WithContext(ctx).Where("token = ?", token).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "订阅 Token 无效")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询订阅用户失败")
	}
	return &user, nil
}

// Create 手工创建一个用户账号（规格书 4.2.1：没有注册入口）。
//
// 参数 ctx；in 为输入。返回创建的用户（含生成的订阅 Token）或错误。
func (s *UserService) Create(ctx context.Context, in UserInput) (*model.User, error) {
	username := trimSpace(in.Username)
	if username == "" {
		return nil, response.Field(response.CodeParamInvalid, "username", in.Username, "用户名不能为空")
	}
	if len(username) > 64 {
		return nil, response.Field(response.CodeParamOutOfRange, "username", username,
			"用户名长度不能超过 64 个字符")
	}
	if len(in.Password) < 6 {
		return nil, response.Field(response.CodeParamInvalid, "password", "",
			"密码长度不能少于 6 位")
	}

	if err := s.ensureUsernameFree(ctx, username, 0); err != nil {
		return nil, err
	}
	if err := s.ensureGroupExists(ctx, in.GroupID); err != nil {
		return nil, err
	}

	hash, err := hashPassword(in.Password)
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "生成密码哈希失败")
	}
	token, err := generateUserToken()
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "生成订阅 Token 失败")
	}

	role := in.Role
	if role != model.RoleAdmin && role != model.RoleUser {
		role = model.RoleUser
	}

	user := model.User{
		Username:     username,
		PasswordHash: hash,
		Nickname:     util.Truncate(trimSpace(in.Nickname), 64),
		Role:         role,
		Status:       model.StatusEnabled,
		GroupID:      in.GroupID,
		Token:        token,
		TrafficLimit: in.TrafficLimit,
		SpeedLimit:   in.SpeedLimit,
		IPLimit:      in.IPLimit,
		DeviceLimit:  in.DeviceLimit,
		ConnLimit:    in.ConnLimit,
		Remark:       util.Truncate(trimSpace(in.Remark), 255),
	}
	if in.Status != nil {
		user.Status = *in.Status
	}
	if err := s.validateLimits(in); err != nil {
		return nil, err
	}

	if err := s.app.DB.WithContext(ctx).Create(&user).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "创建用户失败")
	}

	s.app.Audit.Write(ctx, AuditEntry{
		Action:     model.ActionCreate,
		Resource:   "user",
		ResourceID: user.ID,
		Message:    fmt.Sprintf("创建用户 %s", user.Username),
		Result:     model.ResultSuccess,
	})
	return &user, nil
}

// Update 更新一个用户。
//
// 密码留空表示不改；改动密码时会顺带清除 PasswordResetRequired 标记。
//
// 参数 ctx；id 为用户 ID；in 为输入。返回更新后的用户或错误。
func (s *UserService) Update(ctx context.Context, id uint64, in UserInput) (*model.User, error) {
	user, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if username := trimSpace(in.Username); username != "" && !util.EqualFoldName(username, user.Username) {
		if err := s.ensureUsernameFree(ctx, username, id); err != nil {
			return nil, err
		}
		user.Username = username
	}
	if err := s.ensureGroupExists(ctx, in.GroupID); err != nil {
		return nil, err
	}
	if err := s.validateLimits(in); err != nil {
		return nil, err
	}

	before := util.Clone(*user)

	if n := trimSpace(in.Nickname); n != "" {
		user.Nickname = util.Truncate(n, 64)
	}
	if in.Role == model.RoleAdmin || in.Role == model.RoleUser {
		user.Role = in.Role
	}
	user.GroupID = in.GroupID
	user.TrafficLimit = in.TrafficLimit
	user.SpeedLimit = in.SpeedLimit
	user.IPLimit = in.IPLimit
	user.DeviceLimit = in.DeviceLimit
	user.ConnLimit = in.ConnLimit
	user.Remark = util.Truncate(trimSpace(in.Remark), 255)
	if in.Status != nil {
		user.Status = *in.Status
	}

	if in.Password != "" {
		if len(in.Password) < 6 {
			return nil, response.Field(response.CodeParamInvalid, "password", "",
				"密码长度不能少于 6 位")
		}
		hash, err := hashPassword(in.Password)
		if err != nil {
			return nil, response.Wrap(response.CodeInternal, err, "生成密码哈希失败")
		}
		user.PasswordHash = hash
		user.PasswordResetRequired = false
	}

	if err := s.app.DB.WithContext(ctx).Save(user).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "更新用户失败")
	}

	// 限速变化需要立即生效：清掉该用户的令牌桶，下一次读取按新速率重建。
	if before.SpeedLimit != user.SpeedLimit {
		s.subLimit.SetRate(userLimiterKey(id), user.SpeedLimit)
	}

	s.app.Audit.Write(ctx, AuditEntry{
		Action:     model.ActionUpdate,
		Resource:   "user",
		ResourceID: user.ID,
		Before:     before,
		After:      util.Clone(*user),
		Message:    fmt.Sprintf("更新用户 %s", user.Username),
		Result:     model.ResultSuccess,
	})
	return user, nil
}

// Delete 删除一个用户。
//
// 语义：用户被删除时，其名下的规则会一并删除（否则规则会变成无主规则，
// 既不会被任何人的限速约束，也不会出现在任何人的统计里）。
// 因此这里要求先处理规则，或由调用方显式传 force。
//
// 参数 ctx；id 为用户 ID；force 为 true 时连同规则一起删除。返回错误。
func (s *UserService) Delete(ctx context.Context, id uint64, force bool) error {
	user, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	var ruleCount int64
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Where("user_id = ?", id).Count(&ruleCount).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "统计用户规则数失败")
	}
	if ruleCount > 0 && !force {
		return response.New(response.CodeResourceInUse,
			fmt.Sprintf("该用户名下仍有 %d 条转发规则，请先转移或删除规则", ruleCount))
	}

	err = s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		if ruleCount > 0 {
			if err := tx.WithContext(ctx).Where("user_id = ?", id).
				Delete(&model.ForwardRule{}).Error; err != nil {
				return err
			}
		}
		// 会话与流量明细保留：它们是历史事实，删除会让统计凭空少一块。
		if err := tx.WithContext(ctx).Delete(&model.User{}, id).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return response.Wrap(response.CodeInternal, err, "删除用户失败")
	}

	s.app.Audit.Write(ctx, AuditEntry{
		Action:     model.ActionDelete,
		Resource:   "user",
		ResourceID: id,
		Before:     util.Clone(*user),
		Message:    fmt.Sprintf("删除用户 %s（连带 %d 条规则）", user.Username, ruleCount),
		Result:     model.ResultSuccess,
	})
	return nil
}

// SetStatus 启用或禁用一个用户（规格书 8.11 的 /users/:id/disable）。
//
// 禁用后：登录被拒绝、入口准入判定直接拒绝其新连接。
//
// 参数 ctx；id 为用户 ID；enabled 为 true 启用、false 禁用。返回错误。
func (s *UserService) SetStatus(ctx context.Context, id uint64, enabled bool) error {
	user, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	status := model.StatusDisabled
	if enabled {
		status = model.StatusEnabled
	}
	if user.Status == status {
		return nil
	}

	if err := s.app.DB.WithContext(ctx).Model(&model.User{}).Where("id = ?", id).
		Update("status", status).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "更新用户状态失败")
	}

	msg := "禁用用户 "
	if enabled {
		msg = "启用用户 "
	}
	s.app.Audit.Write(ctx, AuditEntry{
		Action:     model.ActionUpdate,
		Resource:   "user",
		ResourceID: id,
		Message:    msg + user.Username,
		Result:     model.ResultSuccess,
	})
	return nil
}

// ResetToken 重置用户的订阅 Token（规格书 8.11：/users/:id/reset-token）。
//
// 返回新的 Token（仅在本次响应中可见）。
//
// 参数 ctx；id 为用户 ID。返回新 Token 或错误。
func (s *UserService) ResetToken(ctx context.Context, id uint64) (string, error) {
	user, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}

	token, err := generateUserToken()
	if err != nil {
		return "", response.Wrap(response.CodeInternal, err, "生成订阅 Token 失败")
	}
	if err := s.app.DB.WithContext(ctx).Model(&model.User{}).Where("id = ?", id).
		Update("token", token).Error; err != nil {
		return "", response.Wrap(response.CodeInternal, err, "更新订阅 Token 失败")
	}

	s.app.Audit.Write(ctx, AuditEntry{
		Action:     model.ActionUpdate,
		Resource:   "user",
		ResourceID: id,
		Message:    fmt.Sprintf("重置用户 %s 的订阅 Token", user.Username),
		Result:     model.ResultSuccess,
	})
	return token, nil
}

// ResetPassword 由管理员重置用户密码（规格书 8.11：/users/:id/reset-password）。
//
// 直接委托 AuthService.ResetPassword，保证「重置密码」的规则
// （生成随机密码、标记首次登录必须改密、写审计）只有一份实现。
//
// 参数 ctx；operatorID 为操作者；id 为目标用户。返回新生成的明文密码或错误。
func (s *UserService) ResetPassword(ctx context.Context, operatorID, id uint64) (string, error) {
	if s.app.Auth == nil {
		return "", response.New(response.CodeInternal, "认证服务未初始化")
	}
	return s.app.Auth.ResetPassword(ctx, operatorID, id)
}

// Rules 返回某个用户拥有的全部规则（规格书 8.11：/users/:id/rules）。
//
// 参数 ctx；userID 为用户 ID。返回规则列表或错误。
func (s *UserService) Rules(ctx context.Context, userID uint64) ([]model.ForwardRule, error) {
	if _, err := s.Get(ctx, userID); err != nil {
		return nil, err
	}
	items := make([]model.ForwardRule, 0)
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Where("user_id = ?", userID).Order("id ASC").
		Find(&items).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询用户规则失败")
	}
	return items, nil
}

// Traffic 返回某个用户的流量统计（规格书 8.11：/users/:id/traffic）。
//
// 委托 TrafficService.UserBreakdown，保证「剩余流量 / 百分比」的口径
// 与流量页完全一致，不存在两处算法。
//
// 参数 ctx；userID 为用户 ID；mode 为展示口径。返回统计或错误。
func (s *UserService) Traffic(ctx context.Context, userID uint64, mode BytesMode) (*UserTraffic, error) {
	if s.app.Traffic == nil {
		return nil, response.New(response.CodeInternal, "流量服务未初始化")
	}
	return s.app.Traffic.UserBreakdown(ctx, userID, mode)
}

// ensureUsernameFree 按归一化比较校验用户名唯一（规格书 4.1 跨方言约定）。
//
// 参数 ctx；username 为待校验名称；excludeID 为更新场景下需排除的自身 ID。
func (s *UserService) ensureUsernameFree(ctx context.Context, username string, excludeID uint64) error {
	q := s.app.DB.WithContext(ctx).Model(&model.User{})
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	var names []string
	if err := q.Pluck("username", &names).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "查询用户名失败")
	}
	if dup, ok := util.FindNameConflict(names, username); ok {
		return response.New(response.CodeNameConflict,
			fmt.Sprintf("用户名 %q 与已有账号 %q 重复（不区分大小写）", username, dup))
	}
	return nil
}

// ensureGroupExists 校验用户分组是否存在（GroupID 为 0 表示不分组）。
func (s *UserService) ensureGroupExists(ctx context.Context, groupID uint64) error {
	if groupID == 0 {
		return nil
	}
	var count int64
	if err := s.app.DB.WithContext(ctx).Model(&model.UserGroup{}).
		Where("id = ?", groupID).Count(&count).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "校验用户分组失败")
	}
	if count == 0 {
		return response.New(response.CodeNotFound, "指定的用户分组不存在")
	}
	return nil
}

// validateLimits 校验限制类字段不为负。
func (s *UserService) validateLimits(in UserInput) error {
	if in.TrafficLimit < 0 || in.SpeedLimit < 0 || in.IPLimit < 0 || in.DeviceLimit < 0 || in.ConnLimit < 0 {
		return response.Field(response.CodeParamOutOfRange, "traffic_limit", in.TrafficLimit,
			"流量上限、限速与各类连接数限制都不能为负数（0 表示不限）")
	}
	return nil
}

// userLimiterKey 生成用户维度限流器的键。
//
// 前缀与 LimitService 内部保持一致（user:<id>），
// 这样「改完限速立刻清桶」能命中同一个键。
func userLimiterKey(id uint64) string { return limiterKey("user", id) }

// -------------------- 用户分组 CRUD --------------------

// UserGroupParam 是创建 / 更新用户分组的输入。
type UserGroupParam struct {
	Name         string   `json:"name"`
	TrafficLimit int64    `json:"traffic_limit"`
	SpeedLimit   int64    `json:"speed_limit"`
	IPLimit      int      `json:"ip_limit"`
	ConnLimit    int      `json:"conn_limit"`
	RuleGroupIDs []uint64 `json:"rule_group_ids"`
	Remark       string   `json:"remark"`
}

// ListGroups 返回全部用户分组。
//
// 参数 ctx 为上下文。返回分组列表或错误。
func (s *UserService) ListGroups(ctx context.Context) ([]model.UserGroup, error) {
	items := make([]model.UserGroup, 0)
	if err := s.app.DB.WithContext(ctx).Model(&model.UserGroup{}).
		Order("id ASC").Find(&items).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询用户分组失败")
	}
	return items, nil
}

// GetGroup 按 ID 读取用户分组。
//
// 参数 ctx；id 为分组 ID。返回分组或错误。
func (s *UserService) GetGroup(ctx context.Context, id uint64) (*model.UserGroup, error) {
	var g model.UserGroup
	if err := s.app.DB.WithContext(ctx).First(&g, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "用户分组不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询用户分组失败")
	}
	return &g, nil
}

// CreateGroup 新建用户分组。
//
// 参数 ctx；in 为输入。返回创建的分组或错误。
func (s *UserService) CreateGroup(ctx context.Context, in UserGroupParam) (*model.UserGroup, error) {
	name := trimSpace(in.Name)
	if name == "" {
		return nil, response.Field(response.CodeParamInvalid, "name", in.Name, "分组名称不能为空")
	}
	if err := s.ensureGroupNameFree(ctx, name, 0); err != nil {
		return nil, err
	}
	if in.TrafficLimit < 0 || in.SpeedLimit < 0 || in.IPLimit < 0 || in.ConnLimit < 0 {
		return nil, response.Field(response.CodeParamOutOfRange, "traffic_limit", in.TrafficLimit,
			"分组策略中的限制值不能为负数（0 表示不限）")
	}

	g := model.UserGroup{
		Name:         util.Truncate(name, 64),
		TrafficLimit: in.TrafficLimit,
		SpeedLimit:   in.SpeedLimit,
		IPLimit:      in.IPLimit,
		ConnLimit:    in.ConnLimit,
		RuleGroupIDs: model.FromAny(nonNil(in.RuleGroupIDs)),
		Remark:       util.Truncate(trimSpace(in.Remark), 255),
	}
	if err := s.app.DB.WithContext(ctx).Create(&g).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "创建用户分组失败")
	}

	s.app.Audit.Write(ctx, AuditEntry{
		Action:     model.ActionCreate,
		Resource:   "user_group",
		ResourceID: g.ID,
		Message:    fmt.Sprintf("创建用户分组 %s", g.Name),
		Result:     model.ResultSuccess,
	})
	return &g, nil
}

// UpdateGroup 更新用户分组。
//
// 参数 ctx；id 为分组 ID；in 为输入。返回更新后的分组或错误。
func (s *UserService) UpdateGroup(ctx context.Context, id uint64, in UserGroupParam) (*model.UserGroup, error) {
	g, err := s.GetGroup(ctx, id)
	if err != nil {
		return nil, err
	}
	if name := trimSpace(in.Name); name != "" && !util.EqualFoldName(name, g.Name) {
		if err := s.ensureGroupNameFree(ctx, name, id); err != nil {
			return nil, err
		}
		g.Name = util.Truncate(name, 64)
	}
	if in.TrafficLimit < 0 || in.SpeedLimit < 0 || in.IPLimit < 0 || in.ConnLimit < 0 {
		return nil, response.Field(response.CodeParamOutOfRange, "traffic_limit", in.TrafficLimit,
			"分组策略中的限制值不能为负数（0 表示不限）")
	}

	before := util.Clone(*g)
	g.TrafficLimit = in.TrafficLimit
	g.SpeedLimit = in.SpeedLimit
	g.IPLimit = in.IPLimit
	g.ConnLimit = in.ConnLimit
	g.RuleGroupIDs = model.FromAny(nonNil(in.RuleGroupIDs))
	g.Remark = util.Truncate(trimSpace(in.Remark), 255)

	if err := s.app.DB.WithContext(ctx).Save(g).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "更新用户分组失败")
	}

	s.app.Audit.Write(ctx, AuditEntry{
		Action:     model.ActionUpdate,
		Resource:   "user_group",
		ResourceID: g.ID,
		Before:     before,
		After:      util.Clone(*g),
		Message:    fmt.Sprintf("更新用户分组 %s", g.Name),
		Result:     model.ResultSuccess,
	})
	return g, nil
}

// DeleteGroup 删除用户分组。
//
// 分组下仍有用户时拒绝删除，避免用户的 group_id 变成悬空引用。
//
// 参数 ctx；id 为分组 ID。返回错误。
func (s *UserService) DeleteGroup(ctx context.Context, id uint64) error {
	g, err := s.GetGroup(ctx, id)
	if err != nil {
		return err
	}

	var used int64
	if err := s.app.DB.WithContext(ctx).Model(&model.User{}).
		Where("group_id = ?", id).Count(&used).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "统计分组用户数失败")
	}
	if used > 0 {
		return response.New(response.CodeResourceInUse,
			fmt.Sprintf("分组 %s 下仍有 %d 个用户，请先转移用户", g.Name, used))
	}

	if err := s.app.DB.WithContext(ctx).Delete(&model.UserGroup{}, id).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "删除用户分组失败")
	}

	s.app.Audit.Write(ctx, AuditEntry{
		Action:     model.ActionDelete,
		Resource:   "user_group",
		ResourceID: id,
		Before:     util.Clone(*g),
		Message:    fmt.Sprintf("删除用户分组 %s", g.Name),
		Result:     model.ResultSuccess,
	})
	return nil
}

// ensureGroupNameFree 按归一化比较校验分组名唯一。
func (s *UserService) ensureGroupNameFree(ctx context.Context, name string, excludeID uint64) error {
	q := s.app.DB.WithContext(ctx).Model(&model.UserGroup{})
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	var names []string
	if err := q.Pluck("name", &names).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "查询分组名称失败")
	}
	if dup, ok := util.FindNameConflict(names, name); ok {
		return response.New(response.CodeNameConflict,
			fmt.Sprintf("分组名称 %q 与已有分组 %q 重复（不区分大小写）", name, dup))
	}
	return nil
}

// -------------------- 订阅输出（规格书 8.11 /sub/:token） --------------------

// SubscriptionRatePerMinute 是订阅端点的限流阈值（规格书 8.17：10 次 / 分钟 / Token）。
const SubscriptionRatePerMinute = 10

// subscriptionLimiterKey 生成订阅端点的限流键。
//
// 用 Token 的 SHA256 摘要而不是明文：限流器键会驻留在内存里，
// 用摘要可以避免内存被 dump 时泄露可用的订阅 Token。
func subscriptionLimiterKey(token string) string {
	return "sub:" + util.SHA256Hex(trimSpace(token))[:16]
}

// Subscribe 渲染某个订阅 Token 对应的配置文本（规格书 8.11：`GET /sub/:token`，无需认证）。
//
// 输出格式（面向「贴进客户端」的纯文本，字段用 `key: value` 逐行列出）：
//
//	# OpenRoute 订阅
//	# 用户: alice
//	# 生成时间: 2026-01-01T12:00:00Z
//	# 共 2 条规则
//
//	[rule] 香港-入口-01
//	name: 香港-入口-01
//	id: 12
//	listen_port: 8443
//	targets: 1.2.3.4:443, example.com:80
//	protocol: tls
//	...
//
// 设计取舍：规格书要求「返回该用户可用的规则配置文本」但没有规定具体格式，
// 因此这里选择**人类可读、机器也易解析**的 INI 风格：
// 客户端脚本用正则逐段解析即可，用户直接用浏览器打开也能看懂。
//
// 安全：Token 就是凭据，因此这里只输出「入口地址 + 端口 + 目标」等必要信息，
// 绝不含用户密码、节点密钥或其它用户的任何数据。
//
// 参数 ctx；token 为订阅 Token。返回配置文本或错误。
func (s *UserService) Subscribe(ctx context.Context, token string) (string, error) {
	// 订阅端点的独立限流（规格书 8.17）：按 Token 维度，每分钟 10 次。
	//
	// 限流键用 Token 的 SHA256 摘要而非明文，
	// 这样即使限流器内存被 dump 也不会泄露可用的订阅 Token。
	rateKey := subscriptionLimiterKey(token)
	if !s.subLimit.Allow(rateKey, SubscriptionRatePerMinute, "http", 1) {
		return "", response.New(response.CodeRateLimited, "订阅请求过于频繁，请稍后重试")
	}

	user, err := s.GetByToken(ctx, token)
	if err != nil {
		return "", err
	}
	if user.Status != model.StatusEnabled {
		return "", response.New(response.CodeAccountDisabled, "该订阅对应的账号已被禁用")
	}

	rules, err := s.Rules(ctx, user.ID)
	if err != nil {
		return "", err
	}

	// 预取设备组与节点，用于把 ID 翻译成「入口地址:端口」。
	groups, nodes, err := s.loadGroupAndNodes(ctx)
	if err != nil {
		return "", err
	}

	enabled := make([]model.ForwardRule, 0, len(rules))
	for _, r := range rules {
		if r.Enable {
			enabled = append(enabled, r)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# OpenRoute 订阅\n")
	fmt.Fprintf(&b, "# 用户: %s\n", user.Username)
	fmt.Fprintf(&b, "# 生成时间: %s\n", timeNow().Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(&b, "# 共 %d 条规则（已启用）\n", len(enabled))
	fmt.Fprintf(&b, "\n")

	for _, r := range enabled {
		writeRuleText(&b, &r, groups, nodes)
	}
	return b.String(), nil
}

// writeRuleText 把一条规则渲染为订阅文本的一段。
func writeRuleText(b *strings.Builder, r *model.ForwardRule, groups map[uint64]model.DeviceGroup, nodes map[uint64]model.Node) {
	fmt.Fprintf(b, "[rule] %s\n", r.Name)
	fmt.Fprintf(b, "id: %d\n", r.ID)
	fmt.Fprintf(b, "name: %s\n", r.Name)

	// 入口地址：取入口组内第一个在线（无在线则第一个）节点的连接地址 + 监听端口。
	var inGroup *model.DeviceGroup
	if g, ok := groups[r.InboundGroupID]; ok {
		gc := g
		inGroup = &gc
		fmt.Fprintf(b, "inbound_group: %s\n", g.Name)
		if addr, port := pickEndpoint(&g, nodes, r); addr != "" {
			fmt.Fprintf(b, "server: %s\n", addr)
			fmt.Fprintf(b, "port: %d\n", port)
		}
	}
	if r.ListenPort > 0 {
		fmt.Fprintf(b, "listen_port: %d\n", r.ListenPort)
	}
	if r.ListenPortEnd > r.ListenPort && r.ListenPortEnd > 0 {
		fmt.Fprintf(b, "listen_port_end: %d\n", r.ListenPortEnd)
	}

	// 目标地址清单。
	targets := r.TargetList()
	if len(targets) > 0 {
		parts := make([]string, 0, len(targets))
		for _, t := range targets {
			parts = append(parts, t.String())
		}
		fmt.Fprintf(b, "targets: %s\n", strings.Join(parts, ", "))
	}

	fmt.Fprintf(b, "balance: %s\n", r.TargetBalance)
	fmt.Fprintf(b, "inbound_multiplier: %s\n", formatFloat(r.InboundMultiplier))
	fmt.Fprintf(b, "outbound_multiplier: %s\n", formatFloat(r.OutboundMultiplier))
	if r.SpeedLimit > 0 {
		fmt.Fprintf(b, "speed_limit: %d\n", r.SpeedLimit)
	}
	if r.ConnLimit > 0 {
		fmt.Fprintf(b, "conn_limit: %d\n", r.ConnLimit)
	}
	if r.IPLimit > 0 {
		fmt.Fprintf(b, "ip_limit: %d\n", r.IPLimit)
	}

	// 协议：由规则与入口组共同决定（无出口组即入口直出）。
	fmt.Fprintf(b, "protocol: %s\n", ruleProtocol(r, inGroup))
	if r.SNI != "" {
		fmt.Fprintf(b, "sni: %s\n", r.SNI)
	}
	fmt.Fprintf(b, "remark: %s\n", r.Remark)
	fmt.Fprintf(b, "\n")
}

// pickEndpoint 从入口组里挑一个可用的入口地址与端口。
//
// 优先级（与规格书 6.3 的连接地址优先级一致）：在线节点 → 静态连接地址 → 公网 IP。
// 找不到可用节点时返回空串，调用方会跳过 server / port 两行。
func pickEndpoint(g *model.DeviceGroup, nodes map[uint64]model.Node, r *model.ForwardRule) (string, int) {
	port := r.ListenPort
	if port <= 0 {
		port = defaultListenPort(g)
	}

	var fallback model.Node
	hasFallback := false
	for _, id := range g.NodeIDs.AsUint64Slice() {
		n, ok := nodes[id]
		if !ok {
			continue
		}
		if !hasFallback {
			fallback, hasFallback = n, true
		}
		if !n.Online {
			continue
		}
		if addr := nodeConnectAddr(&n); addr != "" {
			return addr, port
		}
	}
	if hasFallback {
		if addr := nodeConnectAddr(&fallback); addr != "" {
			return addr, port
		}
	}
	return "", 0
}

// nodeConnectAddr 返回节点的可连接地址。
//
// 取值顺序：ConnectHost（静态连接地址）→ PublicIPv4 → PublicIPv6。
func nodeConnectAddr(n *model.Node) string {
	if v := trimSpace(n.ConnectHost); v != "" {
		return v
	}
	if v := trimSpace(n.PublicIPv4); v != "" {
		return v
	}
	return trimSpace(n.PublicIPv6)
}

// defaultListenPort 从入口组配置里取默认监听端口。
//
// 取不到时返回 0，调用方会跳过端口行而不是给出一个错误端口。
func defaultListenPort(g *model.DeviceGroup) int {
	var cfg struct {
		ListenPort int `json:"listen_port"`
	}
	if len(g.Config) == 0 {
		return 0
	}
	if err := jsonUnmarshal(string(g.Config), &cfg); err != nil {
		return 0
	}
	return cfg.ListenPort
}

// formatFloat 用最短表示格式化浮点数，避免出现 1.000000 这种噪音。
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// loadGroupAndNodes 一次性载入设备组与节点，避免订阅渲染过程中反复查库。
func (s *UserService) loadGroupAndNodes(ctx context.Context) (map[uint64]model.DeviceGroup, map[uint64]model.Node, error) {
	groups := make([]model.DeviceGroup, 0)
	if err := s.app.DB.WithContext(ctx).Model(&model.DeviceGroup{}).
		Find(&groups).Error; err != nil {
		return nil, nil, response.Wrap(response.CodeInternal, err, "查询设备组失败")
	}
	nodes := make([]model.Node, 0)
	if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).
		Find(&nodes).Error; err != nil {
		return nil, nil, response.Wrap(response.CodeInternal, err, "查询节点失败")
	}

	gm := make(map[uint64]model.DeviceGroup, len(groups))
	for _, g := range groups {
		gm[g.ID] = g
	}
	nm := make(map[uint64]model.Node, len(nodes))
	for _, n := range nodes {
		nm[n.ID] = n
	}
	return gm, nm, nil
}

// SubscriptionURL 生成用户的订阅地址，供用户页展示。
//
// 参数 baseURL 为面板的外部访问地址。返回形如 `{base}/sub/{token}` 的地址。
func (s *UserService) SubscriptionURL(baseURL, token string) string {
	base := strings.TrimRight(trimSpace(baseURL), "/")
	return base + "/sub/" + trimSpace(token)
}
