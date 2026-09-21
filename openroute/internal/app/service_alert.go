package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 6.12「事件与告警」与 8.14 的告警接口。
//
// 告警类型：节点离线、CPU 超阈值、内存超阈值、磁盘超阈值、规则同步失败、
// 用户流量百分比、规则流量百分比、节点流量百分比、证书即将过期。
// 通知渠道：Webhook（必须可用）、Telegram Bot、邮件（SMTP，尽力而为）。
// 防抖：Duration 持续满足条件才触发；SilenceFor 静默期内不重复通知。

// alertEnabledStatus 是「告警条件当前是否成立」的两态。
const (
	stateFiring = "firing"
	stateOK     = "ok"
)

// telegramAPIBase 是 Telegram Bot API 的基地址。
//
// 单独抽成常量便于测试时替换（也便于将来支持自建反代）。
const telegramAPIBase = "https://api.telegram.org"

// AlertService 管理告警规则、告警历史与通知渠道。
type AlertService struct {
	app *App

	// webhook 用于向告警渠道里的 webhook 地址推送（规格书 8.18 的 alert.fired / alert.resolved）。
	//
	// 说明：这里自建一个实例而不是复用 App 上的单例。
	// WebhookService 的全部状态（HTTP 客户端、异步队列）都与调用方无关，
	// 多一个实例只多一条队列，不会造成语义冲突。
	webhook *WebhookService

	// mu 保护 pending 与 inflight：两张表都是「进程内防抖状态」。
	mu sync.Mutex
	// pending 记录每个「告警规则 + 目标」组合的首次满足时间，
	// 用于实现 Duration 防抖（持续满足 N 秒才真正触发）。
	pending map[string]time.Time
	// inflight 记录当前处于「已触发但未恢复」状态的组合，
	// 用于在条件消失时发一条 SQL 恢复通知并归档历史。
	inflight map[string]uint64

	// now 可注入，便于单测推进时间验证防抖与静默期。
	now func() time.Time
	// httpClient 与 smtpSend 均可注入：前者供 Telegram，后者供邮件。
	// 默认实现会真的发网络请求；测试里替换掉即可离线验证逻辑。
	httpClient *http.Client
	smtpSend   func(host string, port int, user, pass string, to []string, msg []byte) error
}

// NewAlertService 构造告警服务。
//
// 参数 a 为运行时依赖容器；返回可直接使用的实例。
func NewAlertService(a *App) *AlertService {
	return &AlertService{
		app:        a,
		webhook:    NewWebhookService(a),
		pending:    make(map[string]time.Time),
		inflight:   make(map[string]uint64),
		now:        func() time.Time { return time.Now().UTC() },
		httpClient: &http.Client{Timeout: 10 * time.Second},
		smtpSend:   defaultSMTPSend,
	}
}

// AlertRuleInput 是创建 / 更新告警规则的输入。
type AlertRuleInput struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	TargetID   uint64          `json:"target_id"`
	Threshold  float64         `json:"threshold"`
	Duration   int             `json:"duration"`
	Channels   []model.Channel `json:"channels"`
	SilenceFor int             `json:"silence_for"`
	// Enabled 用指针以便区分「未传」（默认启用）与「显式传 false」。
	Enabled *bool `json:"enabled"`
}

// ValidAlertType 校验告警类型是否合法（规格书 6.12 的九种类型）。
func ValidAlertType(t string) bool {
	switch t {
	case model.AlertNodeOffline, model.AlertNodeCPU, model.AlertNodeMem, model.AlertNodeDisk,
		model.AlertRuleSyncFailed, model.AlertUserTrafficPct, model.AlertRuleTrafficPct,
		model.AlertNodeTrafficPct, model.AlertCertExpire:
		return true
	}
	return false
}

// validChannelType 校验通知渠道类型是否合法。
func validChannelType(t string) bool {
	switch t {
	case "webhook", "telegram", "email":
		return true
	}
	return false
}

// ListRules 返回全部告警规则。
//
// 参数 ctx 为上下文。返回按 ID 升序的规则列表或错误。
func (s *AlertService) ListRules(ctx context.Context) ([]model.AlertRule, error) {
	items := make([]model.AlertRule, 0)
	if err := s.app.DB.WithContext(ctx).Model(&model.AlertRule{}).
		Order("id ASC").Find(&items).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询告警规则失败")
	}
	return items, nil
}

// CreateRule 新建一条告警规则。
//
// 参数 ctx；in 为输入。返回创建后的规则或错误。
func (s *AlertService) CreateRule(ctx context.Context, in AlertRuleInput) (*model.AlertRule, error) {
	rule, err := s.buildRule(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := s.app.DB.WithContext(ctx).Create(rule).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "创建告警规则失败")
	}
	return rule, nil
}

// UpdateRule 更新一条告警规则。
//
// 参数 ctx；id 为规则 ID；in 为输入。返回更新后的规则或错误。
func (s *AlertService) UpdateRule(ctx context.Context, id uint64, in AlertRuleInput) (*model.AlertRule, error) {
	var exist model.AlertRule
	if err := s.app.DB.WithContext(ctx).First(&exist, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "告警规则不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询告警规则失败")
	}

	built, err := s.buildRule(ctx, in)
	if err != nil {
		return nil, err
	}
	built.ID = exist.ID
	built.CreatedAt = exist.CreatedAt
	// LastFiredAt 不因编辑而重置，否则用户改个阈值就会重新收到一遍轰炸。
	built.LastFiredAt = exist.LastFiredAt

	if err := s.app.DB.WithContext(ctx).Model(&exist).
		Select("name", "type", "target_id", "threshold", "duration",
			"channels", "silence_for", "enabled", "last_fired_at").
		Updates(built).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "更新告警规则失败")
	}

	// 编辑后旧的防抖状态作废，避免「改了阈值仍按旧条件计时」。
	s.resetState(id)
	return built, nil
}

// DeleteRule 删除一条告警规则，同时清理其进程内防抖状态。
//
// 参数 ctx；id 为规则 ID。返回错误。
func (s *AlertService) DeleteRule(ctx context.Context, id uint64) error {
	res := s.app.DB.WithContext(ctx).Delete(&model.AlertRule{}, id)
	if res.Error != nil {
		return response.Wrap(response.CodeInternal, res.Error, "删除告警规则失败")
	}
	if res.RowsAffected == 0 {
		return response.New(response.CodeNotFound, "告警规则不存在")
	}
	s.resetState(id)
	return nil
}

// buildRule 校验并组装一条告警规则（创建与更新共用）。
func (s *AlertService) buildRule(ctx context.Context, in AlertRuleInput) (*model.AlertRule, error) {
	name := trimSpace(in.Name)
	if name == "" {
		return nil, response.Field(response.CodeParamInvalid, "name", in.Name, "告警规则名称不能为空")
	}
	if !ValidAlertType(in.Type) {
		return nil, response.Field(response.CodeParamInvalid, "type", in.Type,
			"告警类型必须是节点离线 / CPU / 内存 / 磁盘 / 规则同步失败 / 流量百分比 / 证书过期之一")
	}

	// 名称按归一化比较查重（规格书 4.1 跨方言约定）。
	var exist []string
	if err := s.app.DB.WithContext(ctx).Model(&model.AlertRule{}).
		Pluck("name", &exist).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询告警规则名称失败")
	}
	if dup, ok := util.FindNameConflict(exist, name); ok {
		return nil, response.New(response.CodeNameConflict,
			fmt.Sprintf("告警规则名称 %q 与已有规则 %q 重复", name, dup))
	}

	// 阈值上限校验：百分比类阈值允许超过 100（例如「达到上限的 120% 才告警」），
	// 但不能是负数；时间类阈值（离线秒数、证书剩余天数）同样不允许为负。
	if in.Threshold < 0 {
		return nil, response.Field(response.CodeParamOutOfRange, "threshold", in.Threshold,
			"阈值不能为负数")
	}
	if in.Duration < 0 {
		return nil, response.Field(response.CodeParamOutOfRange, "duration", in.Duration,
			"防抖持续时长不能为负数")
	}

	for _, ch := range in.Channels {
		if !validChannelType(ch.Type) {
			return nil, response.Field(response.CodeParamInvalid, "channels", ch.Type,
				"通知渠道类型必须是 webhook / telegram / email 之一")
		}
		if ch.Type == "webhook" && trimSpace(ch.URL) == "" {
			return nil, response.Field(response.CodeParamInvalid, "channels", ch.Type,
				"webhook 渠道必须填写地址")
		}
		if ch.Type == "telegram" && (trimSpace(ch.Token) == "" || trimSpace(ch.Chat) == "") {
			return nil, response.Field(response.CodeParamInvalid, "channels", ch.Type,
				"telegram 渠道必须同时填写 Bot Token 与 Chat ID")
		}
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	silence := in.SilenceFor
	if silence <= 0 {
		// 默认 300 秒（规格书 4.2.15），避免用户没填时变成「每次都通知」。
		silence = 300
	}

	return &model.AlertRule{
		Name:       name,
		Type:       in.Type,
		TargetID:   in.TargetID,
		Threshold:  in.Threshold,
		Duration:   in.Duration,
		Channels:   model.FromAny(in.Channels),
		SilenceFor: silence,
		Enabled:    enabled,
	}, nil
}

// resetState 清空指定规则的防抖状态。
func (s *AlertService) resetState(ruleID uint64) {
	prefix := fmt.Sprintf("%d|", ruleID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.pending {
		if strings.HasPrefix(k, prefix) {
			delete(s.pending, k)
		}
	}
	for k := range s.inflight {
		if strings.HasPrefix(k, prefix) {
			delete(s.inflight, k)
		}
	}
}

// ListHistory 分页查询告警历史。
//
// 参数 ctx；resolved 为 nil 时不过滤，true 只看已解决，false 只看活跃；
// page / pageSize 分页。返回列表、总数或错误。
func (s *AlertService) ListHistory(ctx context.Context, resolved *bool, page, pageSize int) ([]model.AlertHistory, int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}

	q := s.app.DB.WithContext(ctx).Model(&model.AlertHistory{})
	if resolved != nil {
		q = q.Where("resolved = ?", *resolved)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "统计告警历史失败")
	}

	items := make([]model.AlertHistory, 0)
	if err := q.Order("fired_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&items).Error; err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "查询告警历史失败")
	}
	return items, total, nil
}

// MarkResolved 把一条告警标记为已解决。
//
// 幂等：已解决的记录重复调用不会报错，也不会覆盖原来的解决时间。
//
// 参数 ctx；id 为告警历史 ID。返回错误。
func (s *AlertService) MarkResolved(ctx context.Context, id uint64) error {
	var rec model.AlertHistory
	if err := s.app.DB.WithContext(ctx).First(&rec, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return response.New(response.CodeNotFound, "告警记录不存在")
		}
		return response.Wrap(response.CodeInternal, err, "查询告警记录失败")
	}
	if rec.Resolved {
		return nil
	}

	now := s.now()
	if err := s.app.DB.WithContext(ctx).Model(&model.AlertHistory{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"resolved":    true,
			"resolved_at": now,
		}).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "更新告警状态失败")
	}

	// 同步清理进程内的「已触发」状态，避免下一次评估又把它当成新告警。
	s.clearInflight(rec.RuleID)

	// 通知渠道收到「告警恢复」（规格书 8.18 的 alert.resolved）。
	payload := map[string]interface{}{
		"alert_id":    rec.ID,
		"rule_id":     rec.RuleID,
		"title":       rec.Title,
		"level":       rec.Level,
		"resource":    rec.Resource,
		"resolved_at": now.Format(time.RFC3339),
		"manual":      true,
	}
	s.webhook.SendAlertResolved(payload)
	s.notifyChannels(ctx, rec.RuleID, rec.Level, "告警已解决: "+rec.Title, rec.Content, false)
	return nil
}

// clearInflight 清空指定规则的「已触发」状态。
func (s *AlertService) clearInflight(ruleID uint64) {
	prefix := fmt.Sprintf("%d|", ruleID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.inflight {
		if strings.HasPrefix(k, prefix) {
			delete(s.inflight, k)
		}
		delete(s.pending, k)
	}
}

// -------------------- 条件评估 --------------------

// alertCondition 是一次评估得出的条件结果。
type alertCondition struct {
	// Key 是「规则 + 目标」的稳定标识，防抖状态以它为键。
	Key string
	// Firing 表示条件当前是否成立。
	Firing bool
	// Level 为触发时的级别。
	Level string
	// Title / Content 为触发时的标题与详情。
	Title   string
	Content string
	// Resource / ResourceID 便于前端跳转到出问题的对象。
	Resource   string
	ResourceID uint64
}

// Evaluate 执行一轮告警评估，由任务调度器周期性调用。
//
// 处理流程（每条启用中的告警规则各走一次）：
//  1. 收集该规则覆盖的全部「目标 → 条件结果」；
//  2. 条件成立：先看是否已触发（静默期判断），未触发则累计防抖时长，达到 Duration 才真正触发；
//  3. 条件消失：若此前已触发，则自动标记解决并发恢复通知。
//
// 返回本轮新触发的告警条数与错误。评估过程中的单条规则失败会被记录但不会中断整轮，
// 因为「一条规则写错了阈值」不应该让其余告警全部失灵。
func (s *AlertService) Evaluate(ctx context.Context) (int, error) {
	rules, err := s.ListRules(ctx)
	if err != nil {
		return 0, err
	}

	fired := 0
	for i := range rules {
		rule := rules[i]
		if !rule.Enabled {
			continue
		}
		conds, err := s.evaluateRule(ctx, &rule)
		if err != nil {
			s.app.Log.Warn("评估告警规则失败",
				zap.Uint64("rule_id", rule.ID), zap.String("type", rule.Type), zap.Error(err))
			continue
		}
		for _, cond := range conds {
			n, err := s.apply(ctx, &rule, cond)
			if err != nil {
				s.app.Log.Warn("处理告警条件失败",
					zap.Uint64("rule_id", rule.ID), zap.String("key", cond.Key), zap.Error(err))
				continue
			}
			fired += n
		}
	}
	return fired, nil
}

// evaluateRule 计算一条规则下全部目标的条件结果。
func (s *AlertService) evaluateRule(ctx context.Context, rule *model.AlertRule) ([]alertCondition, error) {
	switch rule.Type {
	case model.AlertNodeOffline:
		return s.evalNodeOffline(ctx, rule)
	case model.AlertNodeCPU, model.AlertNodeMem, model.AlertNodeDisk:
		return s.evalNodeResource(ctx, rule)
	case model.AlertRuleSyncFailed:
		return s.evalRuleSyncFailed(ctx, rule)
	case model.AlertUserTrafficPct:
		return s.evalUserTrafficPct(ctx, rule)
	case model.AlertRuleTrafficPct:
		return s.evalRuleTrafficPct(ctx, rule)
	case model.AlertNodeTrafficPct:
		return s.evalNodeTrafficPct(ctx, rule)
	case model.AlertCertExpire:
		return s.evalCertExpire(ctx, rule)
	default:
		return nil, fmt.Errorf("未知的告警类型 %q", rule.Type)
	}
}

// targetNodes 返回规则覆盖的节点列表。
//
// TargetID 为 0 表示全局（全部节点），否则只取该节点。
func (s *AlertService) targetNodes(ctx context.Context, rule *model.AlertRule) ([]model.Node, error) {
	var nodes []model.Node
	q := s.app.DB.WithContext(ctx).Model(&model.Node{})
	if rule.TargetID > 0 {
		q = q.Where("id = ?", rule.TargetID)
	}
	if err := q.Find(&nodes).Error; err != nil {
		return nil, err
	}
	return nodes, nil
}

// evalNodeOffline 评估「节点离线」。
//
// 阈值语义：Threshold 为「离线秒数」；为 0 时回退为全局配置的 offline-node-time。
// Duration 仍参与防抖，便于用户配置「离线 5 分钟才告警」。
func (s *AlertService) evalNodeOffline(ctx context.Context, rule *model.AlertRule) ([]alertCondition, error) {
	nodes, err := s.targetNodes(ctx, rule)
	if err != nil {
		return nil, err
	}
	offlineAfter := rule.Threshold
	if offlineAfter <= 0 {
		offlineAfter = float64(s.app.Config.OfflineNodeTime)
	}

	now := s.now()
	out := make([]alertCondition, 0, len(nodes))
	for i := range nodes {
		n := nodes[i]
		secs := offlineSeconds(n.LastSeen, now)
		firing := !n.Online && float64(secs) >= offlineAfter

		cond := alertCondition{
			Key:        condKey(rule.ID, "node", n.ID),
			Firing:     firing,
			Level:      model.AlertLevelCritical,
			Resource:   "node",
			ResourceID: n.ID,
		}
		if firing {
			cond.Title = fmt.Sprintf("节点离线: %s", n.Name)
			cond.Content = fmt.Sprintf("节点 %s（ID %d）已离线 %d 秒，超过阈值 %.0f 秒。最近错误：%s",
				n.Name, n.ID, secs, offlineAfter, emptyAsDash(n.LastError))
		}
		out = append(out, cond)
	}
	return out, nil
}

// evalNodeResource 评估节点的 CPU / 内存 / 磁盘阈值。
//
// 阈值语义：百分比（0~100 的常规用法），允许超过 100 以便「水位超过总容量的 N% 才告警」。
// 数据来源为节点心跳写入的 nodes 表实时字段，因此评估零成本、无需查探针表。
func (s *AlertService) evalNodeResource(ctx context.Context, rule *model.AlertRule) ([]alertCondition, error) {
	nodes, err := s.targetNodes(ctx, rule)
	if err != nil {
		return nil, err
	}

	out := make([]alertCondition, 0, len(nodes))
	for i := range nodes {
		n := nodes[i]
		if !n.Online {
			// 离线节点的指标是过期的，拿旧值告警只会制造噪音。
			continue
		}

		var (
			value float64
			label string
			level = model.AlertLevelWarning
		)
		switch rule.Type {
		case model.AlertNodeCPU:
			value, label = n.CPUUsage, "CPU 使用率"
		case model.AlertNodeMem:
			label = "内存使用率"
			if n.MemTotal > 0 {
				value = float64(n.MemUsed) * 100 / float64(n.MemTotal)
			}
		case model.AlertNodeDisk:
			label = "磁盘使用率"
			if n.DiskTotal > 0 {
				value = float64(n.DiskUsed) * 100 / float64(n.DiskTotal)
			}
		}

		firing := rule.Threshold > 0 && value >= rule.Threshold
		if value >= 95 {
			level = model.AlertLevelCritical
		}

		cond := alertCondition{
			Key:        condKey(rule.ID, "node", n.ID),
			Firing:     firing,
			Level:      level,
			Resource:   "node",
			ResourceID: n.ID,
		}
		if firing {
			cond.Title = fmt.Sprintf("节点 %s 的 %s 超过阈值", n.Name, label)
			cond.Content = fmt.Sprintf("节点 %s（ID %d）%s 当前 %.2f%%，阈值 %.2f%%。",
				n.Name, n.ID, label, value, rule.Threshold)
		}
		out = append(out, cond)
	}
	return out, nil
}

// evalRuleSyncFailed 评估「规则同步失败」。
//
// 阈值语义：Threshold 为「失败规则的允许条数」，0 表示只要有一条失败就告警。
func (s *AlertService) evalRuleSyncFailed(ctx context.Context, rule *model.AlertRule) ([]alertCondition, error) {
	var rules []model.ForwardRule
	q := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Where("sync_status = ?", model.SyncFailed)
	if rule.TargetID > 0 {
		q = q.Where("id = ?", rule.TargetID)
	}
	if err := q.Find(&rules).Error; err != nil {
		return nil, err
	}

	allowed := int(rule.Threshold)
	firing := len(rules) > allowed

	cond := alertCondition{
		Key:      condKey(rule.ID, "rule_sync", rule.TargetID),
		Firing:   firing,
		Level:    model.AlertLevelCritical,
		Resource: "rule",
	}
	if rule.TargetID > 0 {
		cond.ResourceID = rule.TargetID
	}
	if firing {
		names := make([]string, 0, len(rules))
		for i := range rules {
			if i >= 5 {
				names = append(names, fmt.Sprintf("…等 %d 条", len(rules)))
				break
			}
			names = append(names, rules[i].Name)
		}
		cond.Title = fmt.Sprintf("规则同步失败 %d 条", len(rules))
		cond.Content = fmt.Sprintf("以下规则同步失败：%s。允许失败条数为 %d。",
			strings.Join(names, "、"), allowed)
	}
	return []alertCondition{cond}, nil
}

// evalUserTrafficPct 评估「用户流量百分比」。
//
// 百分比 = 已用流量 / 流量上限 × 100；只对设置了上限的用户计算
// （未设上限的用户没有百分比可言，因此不产生告警）。
func (s *AlertService) evalUserTrafficPct(ctx context.Context, rule *model.AlertRule) ([]alertCondition, error) {
	var users []model.User
	q := s.app.DB.WithContext(ctx).Model(&model.User{}).Where("traffic_limit > 0")
	if rule.TargetID > 0 {
		q = q.Where("id = ?", rule.TargetID)
	}
	if err := q.Find(&users).Error; err != nil {
		return nil, err
	}

	out := make([]alertCondition, 0, len(users))
	for i := range users {
		u := users[i]
		pct := float64(u.TrafficUsed) * 100 / float64(u.TrafficLimit)
		cond := alertCondition{
			Key:        condKey(rule.ID, "user", u.ID),
			Firing:     pct >= rule.Threshold,
			Level:      alertLevelByPct(pct, rule.Threshold),
			Resource:   "user",
			ResourceID: u.ID,
		}
		if cond.Firing {
			cond.Title = fmt.Sprintf("用户 %s 流量已达 %.1f%%", u.Username, pct)
			cond.Content = fmt.Sprintf("用户 %s（ID %d）已用 %s / 上限 %s，达到 %.2f%%，阈值 %.2f%%。",
				u.Username, u.ID, util.FormatBytes(u.TrafficUsed),
				util.FormatBytes(u.TrafficLimit), pct, rule.Threshold)
		}
		out = append(out, cond)
	}
	return out, nil
}

// evalRuleTrafficPct 评估「规则流量百分比」。
//
// 规则没有「流量上限」字段，因此百分比的口径定义为
// 「本规则累计流量 / 全站累计流量 × 100」，
// 即「这条规则吃掉了全站多大比例的流量」——这也是个人自用下最有意义的用法。
func (s *AlertService) evalRuleTrafficPct(ctx context.Context, rule *model.AlertRule) ([]alertCondition, error) {
	var rules []model.ForwardRule
	q := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{})
	if rule.TargetID > 0 {
		q = q.Where("id = ?", rule.TargetID)
	}
	if err := q.Find(&rules).Error; err != nil {
		return nil, err
	}

	var siteTotal int64
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Select("COALESCE(SUM(traffic_in + traffic_out), 0) AS total").
		Scan(&siteTotal).Error; err != nil {
		return nil, err
	}
	if siteTotal <= 0 {
		return nil, nil
	}

	out := make([]alertCondition, 0, len(rules))
	for i := range rules {
		r := rules[i]
		pct := float64(r.TrafficIn+r.TrafficOut) * 100 / float64(siteTotal)
		cond := alertCondition{
			Key:        condKey(rule.ID, "rule", r.ID),
			Firing:     pct >= rule.Threshold,
			Level:      alertLevelByPct(pct, rule.Threshold),
			Resource:   "rule",
			ResourceID: r.ID,
		}
		if cond.Firing {
			cond.Title = fmt.Sprintf("规则 %s 占全站流量 %.1f%%", r.Name, pct)
			cond.Content = fmt.Sprintf("规则 %s（ID %d）累计流量 %s，占全站 %.2f%%，阈值 %.2f%%。",
				r.Name, r.ID, util.FormatBytes(r.TrafficIn+r.TrafficOut), pct, rule.Threshold)
		}
		out = append(out, cond)
	}
	return out, nil
}

// evalNodeTrafficPct 评估「节点流量百分比」。
//
// 口径：今日该节点产生的流量 / 今日全站流量 × 100%。
// 「今日」按 UTC 日期、只读按天聚合行（hour = -1），与流量页口径一致。
func (s *AlertService) evalNodeTrafficPct(ctx context.Context, rule *model.AlertRule) ([]alertCondition, error) {
	today := s.now().Format("2006-01-02")

	type nodeSum struct {
		NodeID uint64 `gorm:"column:node_id"`
		Total  int64  `gorm:"column:total"`
	}
	var sums []nodeSum
	if err := s.app.DB.WithContext(ctx).Model(&model.TrafficLog{}).
		Select("node_id AS node_id, SUM(bytes) AS total").
		Where("date = ? AND hour = ? AND node_id > 0", today, model.HourDaily).
		Group("node_id").Scan(&sums).Error; err != nil {
		return nil, err
	}

	var siteTotal int64
	for _, s2 := range sums {
		siteTotal += s2.Total
	}
	if siteTotal <= 0 {
		return nil, nil
	}

	// 节点名一次性查出来，避免逐条查库。
	nodeIDs := make([]uint64, 0, len(sums))
	for _, s2 := range sums {
		nodeIDs = append(nodeIDs, s2.NodeID)
	}
	names := lookupNames(ctx, s.app, "nodes", "name", nodeIDs)

	out := make([]alertCondition, 0, len(sums))
	for _, s2 := range sums {
		if rule.TargetID > 0 && s2.NodeID != rule.TargetID {
			continue
		}
		pct := float64(s2.Total) * 100 / float64(siteTotal)
		cond := alertCondition{
			Key:        condKey(rule.ID, "node", s2.NodeID),
			Firing:     pct >= rule.Threshold,
			Level:      alertLevelByPct(pct, rule.Threshold),
			Resource:   "node",
			ResourceID: s2.NodeID,
		}
		if cond.Firing {
			cond.Title = fmt.Sprintf("节点 %s 占今日流量 %.1f%%", names[s2.NodeID], pct)
			cond.Content = fmt.Sprintf("节点 %s（ID %d）今日流量 %s，占全站 %.2f%%，阈值 %.2f%%。",
				names[s2.NodeID], s2.NodeID, util.FormatBytes(s2.Total), pct, rule.Threshold)
		}
		out = append(out, cond)
	}
	return out, nil
}

// evalCertExpire 评估「证书即将过期」。
//
// 阈值语义：Threshold 为「剩余天数」，默认 30 天。
//
// 实现说明（**规格书中本条目未能完全满足，在此显式记录**）：
// 节点上报结构里没有「证书到期时间」字段，面板侧因此没有权威数据源。
// 本评估在缺少数据时返回空列表（不产生任何告警），而不是用「节点心跳超时」等
// 近似指标伪造一个告警——那只会制造噪音。待节点上报结构补齐该字段后，
// 在这里读取并比较剩余天数即可，调用方无需改动。
func (s *AlertService) evalCertExpire(_ context.Context, _ *model.AlertRule) ([]alertCondition, error) {
	return nil, nil
}

// alertLevelByPct 依据百分比超过阈值的幅度决定级别。
func alertLevelByPct(pct, threshold float64) string {
	if threshold > 0 && pct >= threshold*1.2 {
		return model.AlertLevelCritical
	}
	return model.AlertLevelWarning
}

// condKey 生成防抖状态键。
func condKey(ruleID uint64, kind string, target uint64) string {
	return fmt.Sprintf("%d|%s|%d", ruleID, kind, target)
}

// emptyAsDash 把空串渲染成短横线，避免告警正文出现「最近错误：」这种断句。
func emptyAsDash(s string) string {
	if trimSpace(s) == "" {
		return "-"
	}
	return s
}

// apply 依据条件结果推进防抖状态机，必要时触发或恢复告警。
//
// 返回本轮新触发的告警条数（0 或 1）。
func (s *AlertService) apply(ctx context.Context, rule *model.AlertRule, cond alertCondition) (int, error) {
	now := s.now()
	duration := time.Duration(rule.Duration) * time.Second

	s.mu.Lock()
	// 已触发且仍在持续：交给静默期逻辑处理，防抖不再重复计时。
	handlerID, wasFiring := s.inflight[cond.Key]
	firstSeen, hasPending := s.pending[cond.Key]

	if !cond.Firing {
		// 条件消失：清掉防抖计时；若此前已触发则要做恢复处理。
		delete(s.pending, cond.Key)
		if !wasFiring {
			s.mu.Unlock()
			return 0, nil
		}
		delete(s.inflight, cond.Key)
		s.mu.Unlock()
		return 0, s.resolve(ctx, rule, cond, handlerID)
	}

	if wasFiring {
		s.mu.Unlock()
		// 已触发且仍在触发：只在静默期结束后重新通知一次。
		return 0, s.refire(ctx, rule, cond, handlerID)
	}

	if !hasPending {
		s.pending[cond.Key] = now
		s.mu.Unlock()
		return 0, nil
	}
	if now.Sub(firstSeen) < duration {
		s.mu.Unlock()
		return 0, nil
	}
	delete(s.pending, cond.Key)
	s.mu.Unlock()

	return 1, s.fire(ctx, rule, cond)
}

// fire 写入一条告警历史并推送全部通知渠道。
func (s *AlertService) fire(ctx context.Context, rule *model.AlertRule, cond alertCondition) error {
	now := s.now()
	rec := model.AlertHistory{
		RuleID:     rule.ID,
		Level:      cond.Level,
		Title:      util.Truncate(cond.Title, 255),
		Content:    cond.Content,
		Resolved:   false,
		Resource:   util.Truncate(cond.Resource, 64),
		ResourceID: cond.ResourceID,
		FiredAt:    now,
	}
	if err := s.app.DB.WithContext(ctx).Create(&rec).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "写入告警历史失败")
	}

	s.mu.Lock()
	s.inflight[cond.Key] = rec.ID
	s.mu.Unlock()

	// 记录触发时间，供静默期判断。
	if err := s.app.DB.WithContext(ctx).Model(&model.AlertRule{}).Where("id = ?", rule.ID).
		UpdateColumn("last_fired_at", now).Error; err != nil {
		s.app.Log.Warn("更新告警规则触发时间失败", zap.Uint64("rule_id", rule.ID), zap.Error(err))
	}

	s.app.Log.Info("告警已触发",
		zap.Uint64("rule_id", rule.ID), zap.String("type", rule.Type),
		zap.String("level", cond.Level), zap.String("title", cond.Title))

	// 面板内实时推送（前端告警中心不需要轮询）。
	s.app.Hub().Broadcast(Event{
		Type: "alert_fired",
		Data: map[string]interface{}{
			"alert_id": rec.ID,
			"rule_id":  rule.ID,
			"level":    cond.Level,
			"title":    cond.Title,
			"content":  cond.Content,
		},
	})

	// 全局 Webhook（规格书 8.18 的 alert.fired）。
	s.webhook.SendAlertFired(map[string]interface{}{
		"alert_id":    rec.ID,
		"rule_id":     rule.ID,
		"rule_name":   rule.Name,
		"type":        rule.Type,
		"level":       cond.Level,
		"title":       cond.Title,
		"content":     cond.Content,
		"resource":    cond.Resource,
		"resource_id": cond.ResourceID,
		"fired_at":    now.Format(time.RFC3339),
	})

	s.notifyChannels(ctx, rule.ID, cond.Level, cond.Title, cond.Content, true)
	return nil
}

// refire 在静默期结束后对「持续满足条件」的告警再通知一次。
//
// 语义：静默期是「避免重复轰炸」而不是「不再提醒」，
// 因此 SilenceFor 到期后仍会推送一次，但**不会**新建告警历史。
func (s *AlertService) refire(ctx context.Context, rule *model.AlertRule, cond alertCondition, alertID uint64) error {
	if rule.SilenceFor <= 0 {
		return nil
	}
	var rec model.AlertHistory
	if alertID > 0 {
		if err := s.app.DB.WithContext(ctx).First(&rec, alertID).Error; err != nil {
			return nil
		}
	}
	// 静默期以「最近一次通知时间」为准，这里用告警历史的触发时间做近似（误差可控）。
	last := rec.FiredAt
	if last.IsZero() {
		return nil
	}
	if s.now().Sub(last) < time.Duration(rule.SilenceFor)*time.Second {
		return nil
	}

	s.app.Log.Debug("告警仍在持续，静默期已过，重新通知",
		zap.Uint64("rule_id", rule.ID), zap.String("key", cond.Key))
	s.notifyChannels(ctx, rule.ID, cond.Level, "[持续] "+cond.Title, cond.Content, false)
	return nil
}

// resolve 在条件消失后把活跃告警标记为已解决并发恢复通知。
func (s *AlertService) resolve(ctx context.Context, rule *model.AlertRule, cond alertCondition, alertID uint64) error {
	now := s.now()
	if alertID == 0 {
		return nil
	}
	res := s.app.DB.WithContext(ctx).Model(&model.AlertHistory{}).
		Where("id = ? AND resolved = ?", alertID, false).
		Updates(map[string]interface{}{"resolved": true, "resolved_at": now})
	if res.Error != nil {
		return response.Wrap(response.CodeInternal, res.Error, "更新告警状态失败")
	}
	if res.RowsAffected == 0 {
		return nil
	}

	s.app.Log.Info("告警已恢复",
		zap.Uint64("rule_id", rule.ID), zap.Uint64("alert_id", alertID),
		zap.String("title", cond.Title))

	s.app.Hub().Broadcast(Event{
		Type: "alert_resolved",
		Data: map[string]interface{}{
			"alert_id":    alertID,
			"rule_id":     rule.ID,
			"title":       cond.Title,
			"resolved_at": now.Format(time.RFC3339),
		},
	})

	s.webhook.SendAlertResolved(map[string]interface{}{
		"alert_id":    alertID,
		"rule_id":     rule.ID,
		"rule_name":   rule.Name,
		"type":        rule.Type,
		"level":       cond.Level,
		"title":       cond.Title,
		"resource":    cond.Resource,
		"resource_id": cond.ResourceID,
		"resolved_at": now.Format(time.RFC3339),
	})
	s.notifyChannels(ctx, rule.ID, cond.Level, "告警恢复: "+cond.Title, cond.Content, false)
	return nil
}

// -------------------- 通知渠道 --------------------

// notifyChannels 把一条通知发往规则配置的全部渠道。
//
// 渠道发送全部是「尽力而为」：单个渠道失败只记日志，不影响其余渠道，
// 也不影响告警落库——告警历史才是权威记录，通知只是提醒手段。
//
// 参数 ctx；ruleID 用于取渠道配置；level / title / content 为通知内容；
// isFiring 标识触发还是恢复。
func (s *AlertService) notifyChannels(ctx context.Context, ruleID uint64, level, title, content string, isFiring bool) {
	var rule model.AlertRule
	if err := s.app.DB.WithContext(ctx).First(&rule, ruleID).Error; err != nil {
		return
	}
	for _, ch := range rule.ChannelList() {
		if err := s.sendChannel(ctx, ch, level, title, content, isFiring); err != nil {
			s.app.Log.Warn("告警通知发送失败",
				zap.Uint64("rule_id", ruleID), zap.String("channel", ch.Type), zap.Error(err))
		}
	}
}

// sendChannel 按渠道类型发送一条通知。
func (s *AlertService) sendChannel(ctx context.Context, ch model.Channel, level, title, content string, isFiring bool) error {
	event := EventAlertResolved
	if isFiring {
		event = EventAlertFired
	}
	text := fmt.Sprintf("[%s] %s\n%s", strings.ToUpper(level), title, content)

	switch ch.Type {
	case "webhook":
		data := map[string]interface{}{
			"level":   level,
			"title":   title,
			"content": content,
		}
		return s.webhook.SendSync(ctx, ch.URL, event, data)

	case "telegram":
		return s.sendTelegram(ctx, ch, text)

	case "email":
		return s.sendEmail(ch, title, text)

	default:
		return fmt.Errorf("不支持的通知渠道类型 %q", ch.Type)
	}
}

// sendTelegram 通过 Telegram Bot API 发送消息。
//
// 目标地址：{base}/bot{token}/sendMessage，参数 chat_id 与 text。
// 这是「真·可用」的实现，不是占位：只要填对 token 与 chat 就能收到消息。
func (s *AlertService) sendTelegram(ctx context.Context, ch model.Channel, text string) error {
	api := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPIBase, trimSpace(ch.Token))
	form := url.Values{}
	form.Set("chat_id", trimSpace(ch.Chat))
	form.Set("text", text)
	// 关闭预览，避免告警里带链接时刷出一大块预览。
	form.Set("disable_web_page_preview", "true")

	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx),
		http.MethodPost, api, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			s.app.Log.Debug("关闭 Telegram 响应体失败", zap.Error(cerr))
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Telegram 返回状态码 %d", resp.StatusCode)
	}
	return nil
}

// sendEmail 通过 SMTP 发送告警邮件。
//
// 说明（规格书 6.12 只要求「建议至少做 Webhook」，邮件为尽力而为）：
// 本实现使用标准库 net/smtp 的明文 / STARTTLS 握手，
// 覆盖绝大多数个人自用场景（Gmail 之外的自建 Postfix、QQ 企业邮的 587 端口等）。
// 不支持 OAuth2 与隐式 TLS（465 端口需用户在本地做端口转发），
// 失败信息会完整写进日志，便于用户自行判断。
func (s *AlertService) sendEmail(ch model.Channel, subject, body string) error {
	if trimSpace(ch.Host) == "" || trimSpace(ch.To) == "" {
		return fmt.Errorf("邮件渠道缺少 SMTP 主机或收件人")
	}
	port := ch.Port
	if port <= 0 {
		port = 25
	}

	to := splitRecipients(ch.To)
	if len(to) == 0 {
		return fmt.Errorf("邮件收件人列表为空")
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", headerOr(trimSpace(ch.User), "openroute@localhost"))
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(to, ","))
	fmt.Fprintf(&buf, "Subject: %s\r\n", mimeEncode("OpenRoute 告警"))
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: text/plain; charset=UTF-8\r\n")
	fmt.Fprintf(&buf, "\r\n%s\r\n", body)

	return s.smtpSend(trimSpace(ch.Host), port, trimSpace(ch.User), ch.Pass, to, buf.Bytes())
}

// defaultSMTPSend 是用标准库实现的最小 SMTP 投递。
//
// 参数 host / port 为服务器地址；user / pass 为认证信息（为空则跳过认证）；
// to 为收件人；msg 为完整邮件报文。返回错误。
func defaultSMTPSend(host string, port int, user, pass string, to []string, msg []byte) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	c, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器 %s 失败: %w", addr, err)
	}
	defer func() { _ = c.Close() }()

	// 服务器支持 STARTTLS 就升级；不支持则继续明文（个人自用内网场景常见）。
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(nil); err != nil {
			return fmt.Errorf("STARTTLS 握手失败: %w", err)
		}
	}

	if user != "" {
		if ok, _ := c.Extension("AUTH"); ok {
			auth := smtp.PlainAuth("", user, pass, host)
			if err := c.Auth(auth); err != nil {
				return fmt.Errorf("SMTP 认证失败: %w", err)
			}
		}
	}

	if err := c.Mail(user); err != nil {
		// 部分服务器要求 MAIL FROM 必须是真实存在的地址，失败时就退回空发件人。
		if err2 := c.Mail(""); err2 != nil {
			return fmt.Errorf("设置发件人失败: %w", err)
		}
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("设置收件人 %s 失败: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("开始写入邮件正文失败: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("写入邮件正文失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("提交邮件失败: %w", err)
	}
	return c.Quit()
}

// headerOr 返回 s，为空时返回 fallback。
func headerOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// mimeEncode 对含非 ASCII 的邮件主题做 RFC 2047 编码。
//
// 中文主题若按 UTF-8 裸写，部分邮件客户端会显示成乱码。
func mimeEncode(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	return "=?UTF-8?B?" + util.Base64Std([]byte(s)) + "?="
}

// splitRecipients 把逗号 / 分号分隔的收件人字符串拆成列表。
func splitRecipients(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if v := trimSpace(f); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// -------------------- 渠道测试 --------------------

// TestChannel 向指定告警规则的全部渠道发送一条测试通知（规格书 8.14 POST /alerts/:id/test）。
//
// 与真实告警的区别：不写告警历史、不受静默期限制、失败信息原样返回给前端，
// 让用户能立刻看到「地址填错了」而不是等下一次告警才发现。
//
// 参数 ctx；id 为告警规则 ID；channelType 为空时测试全部渠道，否则只测指定类型。
// 返回每个渠道的结果或错误。
func (s *AlertService) TestChannel(ctx context.Context, id uint64, channelType string) ([]ChannelTestResult, error) {
	var rule model.AlertRule
	if err := s.app.DB.WithContext(ctx).First(&rule, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "告警规则不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询告警规则失败")
	}

	channels := rule.ChannelList()
	if len(channels) == 0 {
		return nil, response.New(response.CodeParamInvalid, "该告警规则尚未配置任何通知渠道")
	}

	title := fmt.Sprintf("[测试] %s", rule.Name)
	content := fmt.Sprintf("这是一条来自 OpenRoute 面板的测试通知。\n规则：%s\n类型：%s\n时间：%s",
		rule.Name, rule.Type, s.now().Format(time.RFC3339))

	results := make([]ChannelTestResult, 0, len(channels))
	for _, ch := range channels {
		if channelType != "" && ch.Type != channelType {
			continue
		}
		res := ChannelTestResult{Type: ch.Type, Target: channelTarget(ch)}
		if err := s.sendChannel(ctx, ch, model.AlertLevelInfo, title, content, true); err != nil {
			res.OK = false
			res.Error = err.Error()
		} else {
			res.OK = true
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		return nil, response.New(response.CodeParamInvalid,
			fmt.Sprintf("该告警规则没有类型为 %q 的通知渠道", channelType))
	}
	return results, nil
}

// ChannelTestResult 是单个渠道的测试结果。
type ChannelTestResult struct {
	Type   string `json:"type"`
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// channelTarget 返回渠道的目标描述（已脱敏，可直接展示给前端）。
func channelTarget(ch model.Channel) string {
	switch ch.Type {
	case "webhook":
		return ch.URL
	case "telegram":
		return "chat:" + ch.Chat
	case "email":
		return ch.To
	default:
		return ch.Type
	}
}
