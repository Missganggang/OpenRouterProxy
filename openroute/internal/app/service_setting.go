package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/openroute/openroute/internal/model"
)

// SettingService 管理系统设置（键值表）与初始化数据。
type SettingService struct {
	app *App

	// cache 是设置的内存缓存，避免每次读设置都查库。
	// 设置读取非常频繁（每个请求可能都要读站点名/主题），因此必须缓存。
	cacheMu sync.RWMutex
	cache   map[string]string
	loaded  bool
}

// NewSettingService 构造设置服务。
func NewSettingService(a *App) *SettingService {
	return &SettingService{
		app:   a,
		cache: make(map[string]string),
	}
}

// DefaultSettings 返回首次初始化时的默认设置（规格书 4.6）。
//
// 返回值是「键 → JSON 值」的映射。
func DefaultSettings() map[string]string {
	return map[string]string{
		model.SettingSiteName: `"OpenRoute"`,
		model.SettingTheme:    `"classic"`,
		model.SettingLogo:     `""`,
		model.SettingFavicon:  `""`,
		// 公告为空，前端不展示公告栏。
		model.SettingAnnouncement: `""`,
		// 默认限速 0 = 不限。
		model.SettingDefaultSpeed: `0`,
		// 告警默认阈值：CPU 90%、内存 90%、磁盘 90%。
		model.SettingAlertThreshold: `{"cpu":90,"mem":90,"disk":90}`,
		model.SettingWebhookURL:     `""`,
		// API 与文档默认开启，个人自用无需额外收紧。
		model.SettingAPIEnabled:  `true`,
		model.SettingOpenAPIDocs: `true`,
	}
}

// Load 把全部设置读入内存缓存。
//
// 启动时调用一次；后续写入会同步更新缓存。
func (s *SettingService) Load(ctx context.Context) error {
	// 投影成匿名结构，避免把 model.SystemSetting 的写入逻辑带进只读路径。
	var rows []struct {
		Key   string
		Value model.JSON
	}
	if err := s.app.DB.WithContext(ctx).Table("system_settings").
		Select("key, value").Find(&rows).Error; err != nil {
		return err
	}

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cache = make(map[string]string, len(rows))
	for _, r := range rows {
		s.cache[r.Key] = string(r.Value)
	}
	s.loaded = true
	return nil
}

// Get 返回指定键的设置值（原始 JSON 文本）。
//
// 键不存在时返回默认值；默认值也不存在时返回空串。
func (s *SettingService) Get(key string) string {
	s.cacheMu.RLock()
	v, ok := s.cache[key]
	s.cacheMu.RUnlock()
	if ok {
		return v
	}
	if d, ok := DefaultSettings()[key]; ok {
		return d
	}
	return ""
}

// GetString 返回去掉 JSON 引号的字符串值，便于直接展示。
func (s *SettingService) GetString(key string) string {
	raw := s.Get(key)
	if raw == "" || raw == "null" {
		return ""
	}
	// 形如 "OpenRoute" 的 JSON 字符串，去掉首尾引号。
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

// GetBool 返回布尔值设置。
func (s *SettingService) GetBool(key string, def bool) bool {
	switch s.Get(key) {
	case "true":
		return true
	case "false":
		return false
	}
	return def
}

// All 返回全部设置的 map，供 GET /api/v1/settings 使用。
//
// 返回值中的 value 是已解析的任意 JSON 类型，前端可直接使用。
func (s *SettingService) All() map[string]interface{} {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()

	out := make(map[string]interface{}, len(s.cache))
	for k, v := range s.cache {
		out[k] = decodeSettingValue(v)
	}
	return out
}

// Update 批量更新设置。
//
// 参数 values 为「键 → 任意值」，内部序列化为 JSON 存储。
// 使用 UPSERT 保证键不存在时创建、存在时覆盖。
//
// 返回更新后的全部设置或错误。
func (s *SettingService) Update(ctx context.Context, values map[string]interface{}) error {
	if len(values) == 0 {
		return nil
	}

	err := s.app.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for k, v := range values {
			row := model.SystemSetting{
				Key:       k,
				Value:     model.FromAny(v),
				UpdatedAt: time.Now().UTC(),
			}
			// 跨方言 UPSERT：SQLite/PG 走 ON CONFLICT，MySQL 走 ON DUPLICATE KEY。
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return s.Load(ctx)
}

// decodeSettingValue 把存储的 JSON 文本解析为 Go 值。
//
// 解析失败时原样返回字符串，保证设置页不会因单个坏值而报错。
func decodeSettingValue(raw string) interface{} {
	if raw == "" {
		return ""
	}
	switch raw {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	// 数量与对象交给标准库解析。
	if raw[0] == '"' {
		return raw[1 : len(raw)-1]
	}
	var v interface{}
	if err := jsonUnmarshal(raw, &v); err != nil {
		return raw
	}
	return v
}

// EnsureInitialized 写入首次初始化数据（规格书 4.6）。
//
// 幂等：已存在的数据不会被覆盖，因此可以安全地在每次启动时调用。
//
// 参数 ctx 为上下文；返回错误。
func (s *SettingService) EnsureInitialized(ctx context.Context) error {
	return s.app.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. 默认用户分组
		var ugCount int64
		if err := tx.Model(&model.UserGroup{}).Count(&ugCount).Error; err != nil {
			return err
		}
		if ugCount == 0 {
			if err := tx.Create(&model.UserGroup{
				Name:         "默认分组",
				RuleGroupIDs: model.FromAny([]uint64{}),
				Remark:       "首次初始化自动创建",
			}).Error; err != nil {
				return err
			}
		}

		// 2. 默认规则分组
		var rgCount int64
		if err := tx.Model(&model.RuleGroup{}).Count(&rgCount).Error; err != nil {
			return err
		}
		if rgCount == 0 {
			if err := tx.Create(&model.RuleGroup{
				Name:   "默认规则组",
				Sort:   0,
				Remark: "首次初始化自动创建",
			}).Error; err != nil {
				return err
			}
		}

		// 3. 默认系统设置：逐键 UPSERT，但只在键不存在时插入，
		//    避免把用户已经改过的设置重置回默认值。
		for k, v := range DefaultSettings() {
			row := model.SystemSetting{
				Key:       k,
				Value:     model.JSON(v),
				UpdatedAt: time.Now().UTC(),
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoNothing: true,
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// EnsureAdmin 确保存在一个管理员账号。
//
// 行为（规格书 3.3 第 7 步）：
//   - 用户表非空时直接返回；
//   - 空表且提供了 username / password 时按给定值创建；
//   - 空表且未提供密码时返回 ErrNoAdminCredentials，由调用方进入交互式引导。
//
// 返回创建的管理员（可能为 nil）与错误。
func (s *SettingService) EnsureAdmin(ctx context.Context, username, password string) (*model.User, error) {
	var count int64
	if err := s.app.DB.WithContext(ctx).Model(&model.User{}).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, nil
	}

	username = trimSpace(username)
	if username == "" {
		username = DefaultAdminUsername
	}
	if password == "" {
		return nil, ErrNoAdminCredentials
	}
	if len(password) < 6 {
		return nil, ErrWeakAdminPassword
	}

	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	token, err := generateUserToken()
	if err != nil {
		return nil, err
	}

	admin := model.User{
		Username:     username,
		PasswordHash: hash,
		Nickname:     "管理员",
		Role:         model.RoleAdmin,
		Status:       model.StatusEnabled,
		Token:        token,
		// 默认归入第一个用户分组（确保初始化已先执行）。
		GroupID: firstUserGroupID(ctx, s.app),
	}
	if err := s.app.DB.WithContext(ctx).Create(&admin).Error; err != nil {
		return nil, err
	}
	return &admin, nil
}

// firstUserGroupID 返回第一个用户分组的 ID，无分组时返回 0。
func firstUserGroupID(ctx context.Context, a *App) uint64 {
	var g model.UserGroup
	if err := a.DB.WithContext(ctx).Order("id ASC").First(&g).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			a.Log.Warn("读取默认用户分组失败")
		}
		return 0
	}
	return g.ID
}
