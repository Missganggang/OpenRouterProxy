package app

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/openroute/openroute/internal/util"
)

// 初始化阶段的哨兵错误，供启动流程分支处理。
var (
	// ErrNoAdminCredentials 表示用户表为空且未提供管理员密码，
	// 调用方应进入交互式引导（规格书 3.3 第 7 步）。
	ErrNoAdminCredentials = errors.New("用户表为空，需要创建管理员账号")
	// ErrWeakAdminPassword 表示提供的管理员密码不满足最低强度要求。
	ErrWeakAdminPassword = errors.New("管理员密码长度不能少于 6 位")
)

// jsonUnmarshal 是 json.Unmarshal 的薄封装，供本包内使用。
func jsonUnmarshal(s string, v interface{}) error {
	return json.Unmarshal([]byte(s), v)
}

// trimSpace 是 strings.TrimSpace 的转发，保持调用点简洁。
func trimSpace(s string) string { return strings.TrimSpace(s) }

// hashPassword 转发到 util.HashPassword。
func hashPassword(p string) (string, error) { return util.HashPassword(p) }

// generateUserToken 转发到 util.GenerateUserToken。
func generateUserToken() (string, error) { return util.GenerateUserToken() }

// timeNow 返回当前 UTC 时间。
func timeNow() time.Time { return time.Now().UTC() }
