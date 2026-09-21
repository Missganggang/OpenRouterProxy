package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// AuthService 负责登录、会话与令牌签发。
//
// 认证方式（规格书 8.2）：
//   - Session Cookie：WebUI 浏览器，Cookie 名 or_session，HttpOnly + SameSite=Lax
//   - JWT Bearer：前端 SPA / 第三方
//   - API Token：服务端对接，X-API-Key + 可选签名
//   - 节点 Token：节点通信，X-Node-Token
type AuthService struct {
	app *App

	// 登录失败计数与图形验证码的进程内状态。
	//
	// 个人自用场景下重启丢状态可以接受，因此不落库；
	// 但登录尝试记录会写库，便于审计「谁在什么时候尝试登录」。
	attemptMu sync.Mutex
	captchaMu sync.Mutex
	captchas  map[string]captchaEntry
}

// captchaEntry 是一个图形验证码的答案与过期时间。
type captchaEntry struct {
	code      string
	expiresAt time.Time
}

// 登录失败阈值（规格书 8.17）。
const (
	// CaptchaThreshold 连续失败达到该次数后要求验证码。
	CaptchaThreshold = 3
	// LockThreshold 连续失败达到该次数后锁定。
	LockThreshold = 10
	// LockDuration 锁定时长。
	LockDuration = 15 * time.Minute
	// AccessTokenTTL 访问令牌有效期。
	AccessTokenTTL = 2 * time.Hour
	// RefreshTokenTTL 刷新令牌有效期。
	RefreshTokenTTL = 7 * 24 * time.Hour
	// CaptchaTTL 验证码有效期。
	CaptchaTTL = 5 * time.Minute
)

// SessionCookieName 是 WebUI 会话 Cookie 名。
const SessionCookieName = "or_session"

// Claims 是签发的 JWT 载荷。
type Claims struct {
	UserID   uint64 `json:"uid"`
	Username string `json:"usr"`
	Role     string `json:"role"`
	// TokenType 区分 access 与 refresh，防止刷新令牌被当作访问令牌使用。
	TokenType string `json:"typ"`
	jwt.RegisteredClaims
}

// LoginResult 是登录成功后的返回结构。
type LoginResult struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresIn    int         `json:"expires_in"`
	User         *model.User `json:"user"`
	// CaptchaRequired 在登录失败需要验证码时为 true。
	CaptchaRequired bool `json:"captcha_required,omitempty"`
}

// LoginInput 是登录请求参数。
type LoginInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Captcha  string `json:"captcha"`
	// CaptchaID 是验证码标识，服务端据此取回答案。
	CaptchaID string `json:"captcha_id"`
}

// NewAuthService 构造认证服务。
func NewAuthService(a *App) *AuthService {
	s := &AuthService{
		app:      a,
		captchas: make(map[string]captchaEntry),
	}
	// 启动后台清理，避免验证码 map 无限增长。
	a.Go(func(ctx context.Context) {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.Done():
				return
			case <-ticker.C:
				s.gcCaptcha()
			}
		}
	})
	return s
}

// Login 校验用户名密码并签发令牌。
//
// 安全策略（规格书 8.17）：
//   - 连续失败 3 次要求图形验证码；
//   - 连续失败 10 次锁定 15 分钟；
//   - 无论成功失败都写入 login_attempts，供审计与风控。
//
// 参数 ctx 为上下文；in 为登录参数；ip 与 ua 用于记录与风控。
// 返回令牌结果或 AppError。
func (s *AuthService) Login(ctx context.Context, in LoginInput, ip, ua string) (*LoginResult, error) {
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || in.Password == "" {
		return nil, response.Field(response.CodeParamInvalid, "username", in.Username,
			"用户名与密码不能为空")
	}

	// 锁定检查必须在密码校验之前，否则锁定形同虚设。
	fails, err := s.recentFailCount(ctx, in.Username, ip)
	if err != nil {
		return nil, err
	}
	if fails >= LockThreshold {
		s.recordAttempt(ctx, in.Username, ip, false)
		return nil, response.New(response.CodeAccountLocked,
			fmt.Sprintf("连续登录失败已达 %d 次，账号锁定 %d 分钟", LockThreshold, int(LockDuration.Minutes())))
	}

	// 达到阈值后强制验证码。
	if fails >= CaptchaThreshold {
		if in.Captcha == "" || in.CaptchaID == "" {
			return nil, response.New(response.CodeCaptchaRequired, "")
		}
		if !s.verifyCaptcha(in.CaptchaID, in.Captcha) {
			s.recordAttempt(ctx, in.Username, ip, false)
			return nil, response.New(response.CodeCaptchaWrong, "")
		}
	}

	var user model.User
	err = s.app.DB.WithContext(ctx).Where("username = ?", in.Username).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			s.recordAttempt(ctx, in.Username, ip, false)
			// 不区分「用户不存在」与「密码错误」，避免用户名枚举。
			return nil, response.New(response.CodeBadCredentials, "")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询用户失败")
	}

	if err := util.CheckPassword(user.PasswordHash, in.Password); err != nil {
		s.recordAttempt(ctx, in.Username, ip, false)
		return nil, response.New(response.CodeBadCredentials, "")
	}

	if user.Status != model.StatusEnabled {
		s.recordAttempt(ctx, in.Username, ip, false)
		return nil, response.New(response.CodeAccountDisabled, "")
	}

	// 登录成功：清空失败记录并更新最后登录信息。
	now := time.Now().UTC()
	if err := s.app.DB.WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).
		Updates(map[string]interface{}{
			"last_login_at": now,
			"last_login_ip": ip,
		}).Error; err != nil {
		s.app.Log.Warn("更新最后登录信息失败", zap.Uint64("user_id", user.ID), zap.Error(err))
	}
	user.LastLoginAt = &now
	user.LastLoginIP = ip

	s.clearFailures(ctx, in.Username, ip)
	s.recordAttempt(ctx, in.Username, ip, true)

	access, refresh, err := s.issueTokens(&user)
	if err != nil {
		return nil, err
	}

	s.app.Audit.Write(ctx, AuditEntry{
		UserID:    user.ID,
		Username:  user.Username,
		Action:    model.ActionLogin,
		Resource:  "auth",
		IP:        ip,
		UserAgent: ua,
		Result:    model.ResultSuccess,
		Message:   "登录成功",
	})

	return &LoginResult{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(AccessTokenTTL.Seconds()),
		User:         &user,
	}, nil
}

// IssueTokensFor 为已确认有效的用户重新签发令牌对。
//
// 供 handler 层在「用 Session Cookie 续期」等场景使用：
// 调用方需自行确认用户存在且启用，本方法只负责签发。
//
// 参数 u 为用户；返回 access token、refresh token 与错误。
func (s *AuthService) IssueTokensFor(u *model.User) (string, string, error) {
	return s.issueTokens(u)
}

// issueTokens 为指定用户签发访问令牌与刷新令牌。
func (s *AuthService) issueTokens(u *model.User) (string, string, error) {
	now := time.Now().UTC()

	access, err := s.sign(Claims{
		UserID:    u.ID,
		Username:  u.Username,
		Role:      u.Role,
		TokenType: "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", u.ID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
			Issuer:    "openroute",
		},
	})
	if err != nil {
		return "", "", err
	}

	refresh, err := s.sign(Claims{
		UserID:    u.ID,
		Username:  u.Username,
		Role:      u.Role,
		TokenType: "refresh",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", u.ID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(RefreshTokenTTL)),
			Issuer:    "openroute",
		},
	})
	if err != nil {
		return "", "", err
	}

	return access, refresh, nil
}

// sign 用面板 secret-key 签发 JWT。
func (s *AuthService) sign(c Claims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	str, err := token.SignedString([]byte(s.app.Config.SecretKey))
	if err != nil {
		return "", response.Wrap(response.CodeInternal, err, "签发令牌失败")
	}
	return str, nil
}

// ParseToken 校验并解析 JWT。
//
// 参数 tokenStr 为令牌字符串；wantType 为期望的令牌类型（access / refresh）。
// 返回解析出的声明或 AppError。
func (s *AuthService) ParseToken(tokenStr, wantType string) (*Claims, error) {
	tokenStr = strings.TrimSpace(tokenStr)
	if tokenStr == "" {
		return nil, response.New(response.CodeUnauthorized, "")
	}

	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		// 必须校验签名算法，否则存在 alg=none 攻击风险。
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("非预期的签名算法: %v", t.Header["alg"])
		}
		return []byte(s.app.Config.SecretKey), nil
	}, jwt.WithIssuer("openroute"))

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, response.New(response.CodeTokenExpired, "")
		}
		return nil, response.New(response.CodeUnauthorized, "")
	}
	if !tok.Valid {
		return nil, response.New(response.CodeUnauthorized, "")
	}
	if wantType != "" && claims.TokenType != wantType {
		return nil, response.New(response.CodeUnauthorized, "令牌类型不匹配")
	}
	return claims, nil
}

// Refresh 用刷新令牌换取新的访问令牌。
//
// 同时会重新读取用户，确保已被禁用或删除的账号无法续期。
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (*LoginResult, error) {
	claims, err := s.ParseToken(refreshToken, "refresh")
	if err != nil {
		return nil, err
	}

	var user model.User
	if err := s.app.DB.WithContext(ctx).First(&user, claims.UserID).Error; err != nil {
		return nil, response.New(response.CodeUnauthorized, "")
	}
	if user.Status != model.StatusEnabled {
		return nil, response.New(response.CodeAccountDisabled, "")
	}

	access, refresh, err := s.issueTokens(&user)
	if err != nil {
		return nil, err
	}
	return &LoginResult{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(AccessTokenTTL.Seconds()),
		User:         &user,
	}, nil
}

// Me 返回当前登录用户的完整信息。
func (s *AuthService) Me(ctx context.Context, userID uint64) (*model.User, error) {
	var user model.User
	if err := s.app.DB.WithContext(ctx).First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "用户不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询用户失败")
	}
	return &user, nil
}

// ChangePassword 修改自己的密码。
//
// 要求提供旧密码，防止会话被劫持后直接改密。
// 若用户被标记 PasswordResetRequired（迁移场景），允许在提供旧密码后清除该标记。
func (s *AuthService) ChangePassword(ctx context.Context, userID uint64, oldPwd, newPwd string) error {
	if len(newPwd) < 6 {
		return response.Field(response.CodeParamInvalid, "new_password", "",
			"新密码长度不能少于 6 位")
	}

	var user model.User
	if err := s.app.DB.WithContext(ctx).First(&user, userID).Error; err != nil {
		return response.New(response.CodeNotFound, "用户不存在")
	}

	if err := util.CheckPassword(user.PasswordHash, oldPwd); err != nil {
		return response.New(response.CodeBadCredentials, "原密码不正确")
	}

	hash, err := util.HashPassword(newPwd)
	if err != nil {
		return response.Wrap(response.CodeInternal, err, "生成密码哈希失败")
	}

	if err := s.app.DB.WithContext(ctx).Model(&model.User{}).Where("id = ?", userID).
		Updates(map[string]interface{}{
			"password_hash":           hash,
			"password_reset_required": false,
		}).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "更新密码失败")
	}

	s.app.Audit.Write(ctx, AuditEntry{
		UserID:     userID,
		Username:   user.Username,
		Action:     model.ActionUpdate,
		Resource:   "user",
		ResourceID: userID,
		Result:     model.ResultSuccess,
		Message:    "修改密码",
	})
	return nil
}

// -------------------- 图形验证码 --------------------

// NewCaptcha 生成一个图形验证码，返回标识与可直接展示的 SVG 数据。
//
// 实现说明：为避免引入额外图形库，验证码用简单几何变形渲染成 SVG，
// 对「阻止脚本暴力登录」这一目标而言足够，且不增加二进制体积。
//
// 返回 captchaID 与 svg 字符串。
func (s *AuthService) NewCaptcha() (string, string) {
	code := randomDigits(4)
	id := util.MustRandomString(24)

	s.captchaMu.Lock()
	s.captchas[id] = captchaEntry{code: code, expiresAt: time.Now().Add(CaptchaTTL)}
	s.captchaMu.Unlock()

	return id, renderCaptchaSVG(code)
}

// verifyCaptcha 校验验证码，成功即销毁（一次性使用）。
func (s *AuthService) verifyCaptcha(id, answer string) bool {
	s.captchaMu.Lock()
	entry, ok := s.captchas[id]
	// 无论对错都删除，防止同一验证码被反复尝试。
	delete(s.captchas, id)
	s.captchaMu.Unlock()

	if !ok || time.Now().After(entry.expiresAt) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(answer), entry.code)
}

// gcCaptcha 清理过期验证码。
func (s *AuthService) gcCaptcha() {
	now := time.Now()
	s.captchaMu.Lock()
	for id, e := range s.captchas {
		if now.After(e.expiresAt) {
			delete(s.captchas, id)
		}
	}
	s.captchaMu.Unlock()
}

// -------------------- 登录失败计数 --------------------

// recentFailCount 统计当前窗口内的连续失败次数。
//
// 统计维度为「用户名 + IP」与「用户名」两者取大：
// 前者防止针对单机的爆破，后者防止换 IP 绕过。
func (s *AuthService) recentFailCount(ctx context.Context, username, ip string) (int, error) {
	since := time.Now().UTC().Add(-LockDuration)

	var byUser int64
	if err := s.app.DB.WithContext(ctx).Model(&model.LoginAttempt{}).
		Where("username = ? AND success = ? AND created_at > ?", username, false, since).
		Count(&byUser).Error; err != nil {
		return 0, response.Wrap(response.CodeInternal, err, "统计登录失败次数失败")
	}

	var byIP int64
	if ip != "" {
		if err := s.app.DB.WithContext(ctx).Model(&model.LoginAttempt{}).
			Where("ip = ? AND success = ? AND created_at > ?", ip, false, since).
			Count(&byIP).Error; err != nil {
			return 0, response.Wrap(response.CodeInternal, err, "统计登录失败次数失败")
		}
	}

	n := int(byUser)
	if int(byIP) > n {
		n = int(byIP)
	}
	return n, nil
}

// clearFailures 在登录成功后清空该用户名与 IP 的失败记录。
func (s *AuthService) clearFailures(ctx context.Context, username, ip string) {
	if err := s.app.DB.WithContext(ctx).
		Where("username = ? AND success = ?", username, false).
		Delete(&model.LoginAttempt{}).Error; err != nil {
		s.app.Log.Warn("清理登录失败记录失败", zap.String("username", username), zap.Error(err))
	}
	if ip != "" {
		if err := s.app.DB.WithContext(ctx).
			Where("ip = ? AND success = ?", ip, false).
			Delete(&model.LoginAttempt{}).Error; err != nil {
			s.app.Log.Warn("清理登录失败记录失败", zap.String("ip", ip), zap.Error(err))
		}
	}
}

// recordAttempt 记录一次登录尝试。
func (s *AuthService) recordAttempt(ctx context.Context, username, ip string, success bool) {
	rec := model.LoginAttempt{
		Username:  util.Truncate(username, 64),
		IP:        util.Truncate(ip, 64),
		Success:   success,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.app.DB.WithContext(ctx).Create(&rec).Error; err != nil {
		s.app.Log.Warn("记录登录尝试失败", zap.Error(err))
	}
	// 失败时同步写入审计日志，便于「谁在爆破」的追溯。
	if !success {
		s.app.Audit.Write(ctx, AuditEntry{
			Username: username,
			Action:   model.ActionLogin,
			Resource: "auth",
			IP:       ip,
			Result:   model.ResultFailed,
			Message:  "登录失败",
		})
	}
}

// -------------------- 密码重置 --------------------

// ResetPassword 由管理员重置指定用户的密码。
//
// 返回新生成的明文密码，仅在本次响应中返回一次。
func (s *AuthService) ResetPassword(ctx context.Context, operatorID, targetUserID uint64) (string, error) {
	newPwd, err := util.RandomPassword()
	if err != nil {
		return "", response.Wrap(response.CodeInternal, err, "生成随机密码失败")
	}
	hash, err := util.HashPassword(newPwd)
	if err != nil {
		return "", response.Wrap(response.CodeInternal, err, "生成密码哈希失败")
	}

	res := s.app.DB.WithContext(ctx).Model(&model.User{}).Where("id = ?", targetUserID).
		Updates(map[string]interface{}{
			"password_hash":           hash,
			"password_reset_required": true,
		})
	if res.Error != nil {
		return "", response.Wrap(response.CodeInternal, res.Error, "重置密码失败")
	}
	if res.RowsAffected == 0 {
		return "", response.New(response.CodeNotFound, "用户不存在")
	}

	var target model.User
	if err := s.app.DB.WithContext(ctx).First(&target, targetUserID).Error; err == nil {
		s.app.Audit.Write(ctx, AuditEntry{
			UserID:     operatorID,
			Action:     model.ActionUpdate,
			Resource:   "user",
			ResourceID: targetUserID,
			Message:    fmt.Sprintf("重置用户 %s 的密码", target.Username),
			Result:     model.ResultSuccess,
		})
	}
	return newPwd, nil
}
