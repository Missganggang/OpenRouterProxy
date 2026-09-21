package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/model"
)

// ListAlerts 返回告警规则与当前活跃告警（规格书 8.14）。
//
// 一次返回两部分，供告警中心页首屏使用：rules 为全部规则，
// active 为尚未解决的告警历史。
func (h *Handlers) ListAlerts(c *gin.Context) {
	ctx := c.Request.Context()

	rules, err := h.app.Alert.ListRules(ctx)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if rules == nil {
		rules = []model.AlertRule{}
	}

	// 活跃告警 = 未解决的告警历史。
	resolved := false
	active, _, err := h.app.Alert.ListHistory(ctx, &resolved, 1, 100)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if active == nil {
		active = []model.AlertHistory{}
	}

	response.OK(c, gin.H{
		"rules":  rules,
		"active": active,
	})
}

// CreateAlert 创建告警规则（规格书 8.14）。
func (h *Handlers) CreateAlert(c *gin.Context) {
	var in app.AlertRuleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}
	if err := validateAlertInput(in); err != nil {
		response.Fail(c, err)
		return
	}

	ctx := c.Request.Context()
	rule, err := h.app.Alert.CreateRule(ctx, in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionCreate,
		Resource:   "alert_rule",
		ResourceID: rule.ID,
		After:      rule,
		Message:    "创建告警规则 " + rule.Name,
	})

	response.OK(c, rule)
}

// UpdateAlert 更新告警规则（规格书 8.14）。
func (h *Handlers) UpdateAlert(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "告警规则 ID 必须是正整数")
		return
	}

	var in app.AlertRuleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		badRequest(c, "body", "请求体不是合法 JSON："+err.Error())
		return
	}
	if err := validateAlertInput(in); err != nil {
		response.Fail(c, err)
		return
	}

	ctx := c.Request.Context()
	rule, err := h.app.Alert.UpdateRule(ctx, id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionUpdate,
		Resource:   "alert_rule",
		ResourceID: id,
		After:      rule,
		Message:    "更新告警规则 " + rule.Name,
	})

	response.OK(c, rule)
}

// DeleteAlert 删除告警规则（规格书 8.14）。
func (h *Handlers) DeleteAlert(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "告警规则 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	if err := h.app.Alert.DeleteRule(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionDelete,
		Resource:   "alert_rule",
		ResourceID: id,
		Message:    "删除告警规则",
	})

	response.OK(c, nil)
}

// AlertHistory 返回告警历史（规格书 8.14）。
func (h *Handlers) AlertHistory(c *gin.Context) {
	page, pageSize := pageParams(c)

	// resolved 参数缺省时不筛选，便于「全部历史」视图。
	resolved := queryBool(c, "resolved")

	items, total, err := h.app.Alert.ListHistory(c.Request.Context(), resolved, page, pageSize)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if items == nil {
		items = []model.AlertHistory{}
	}
	response.List(c, items, page, pageSize, total)
}

// TestAlert 测试告警规则配置的通知渠道（规格书 8.14）。
//
// channel_type 可选，用于只测某一个渠道；为空时测全部。
func (h *Handlers) TestAlert(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "告警规则 ID 必须是正整数")
		return
	}

	results, err := h.app.Alert.TestChannel(c.Request.Context(), id, strings.TrimSpace(c.Query("channel_type")))
	if err != nil {
		response.Fail(c, err)
		return
	}
	if results == nil {
		results = []app.ChannelTestResult{}
	}

	// 全部成功才给出成功提示，否则让前端逐个展示失败原因。
	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
		}
	}
	if failed > 0 {
		response.OKMessage(c, "测试完成，其中 "+itoa(failed)+" 个渠道发送失败", results)
		return
	}
	response.OKMessage(c, "测试通知已发送", results)
}

// ResolveAlert 手动把一条告警标记为已解决（规格书 8.14）。
func (h *Handlers) ResolveAlert(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		badRequest(c, "id", "告警 ID 必须是正整数")
		return
	}

	ctx := c.Request.Context()
	if err := h.app.Alert.MarkResolved(ctx, id); err != nil {
		response.Fail(c, err)
		return
	}

	uid, un, _, _ := middleware.CurrentUser(c)
	h.app.Audit.Write(ctx, app.AuditEntry{
		UserID:     uid,
		Username:   un,
		Action:     model.ActionUpdate,
		Resource:   "alert",
		ResourceID: id,
		Message:    "标记告警已解决",
	})

	response.OK(c, nil)
}

// TestWebhook 向指定地址发送一条测试 Webhook（规格书 8.14）。
//
// 参数 url 为空时回退到系统设置中的 webhook_url，
// 便于前端在「设置 → 通知设置」里直接测试当前配置。
func (h *Handlers) TestWebhook(c *gin.Context) {
	var body struct {
		URL string `json:"url"`
	}
	_ = c.ShouldBindJSON(&body)

	url := strings.TrimSpace(body.URL)
	if url == "" {
		url = strings.TrimSpace(h.app.Setting.GetString(model.SettingWebhookURL))
	}
	if url == "" {
		response.Fail(c, response.Field(response.CodeParamInvalid, "url", nil,
			"未提供 Webhook 地址，且系统设置中也没有配置"))
		return
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		response.Fail(c, response.Field(response.CodeParamInvalid, "url", url,
			"Webhook 地址必须以 http:// 或 https:// 开头"))
		return
	}

	err := h.app.Webhook.SendSync(c.Request.Context(), url, "webhook.test", map[string]interface{}{
		"message": "这是一条来自 OpenRoute 面板的测试通知",
	})
	if err != nil {
		// 测试失败不是服务器错误，返回业务可读的结果供前端展示。
		response.OKMessage(c, "测试失败："+err.Error(), gin.H{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	response.OKMessage(c, "测试通知已发送成功", gin.H{"ok": true})
}

// validateAlertInput 校验告警规则的必填项与取值合法性。
//
// 规则（规格书 6.12 / 4.2.15）：
//   - 名称与类型必填；
//   - 类型必须在已知集合内；
//   - 百分比类阈值为 0~100；
//   - 至少配置一个通知渠道。
//
// 返回 *response.AppError，nil 表示通过。
func validateAlertInput(in app.AlertRuleInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return response.Field(response.CodeParamInvalid, "name", nil, "请填写告警名称")
	}
	if !validAlertType(in.Type) {
		return response.Field(response.CodeParamInvalid, "type", in.Type,
			"未知的告警类型；可用值：node_offline、node_cpu、node_mem、node_disk、"+
				"rule_sync_failed、user_traffic_pct、rule_traffic_pct、node_traffic_pct、cert_expire")
	}
	// 百分比类告警的阈值必须落在 0~100。
	switch in.Type {
	case model.AlertNodeCPU, model.AlertNodeMem, model.AlertNodeDisk,
		model.AlertUserTrafficPct, model.AlertRuleTrafficPct, model.AlertNodeTrafficPct:
		if in.Threshold <= 0 || in.Threshold > 100 {
			return response.Field(response.CodeParamOutOfRange, "threshold", in.Threshold,
				"百分比阈值必须大于 0 且不超过 100")
		}
	}
	if len(in.Channels) == 0 {
		return response.Field(response.CodeParamInvalid, "channels", nil,
			"至少要配置一个通知渠道（webhook / telegram / email）")
	}
	for i, ch := range in.Channels {
		if !validChannelType(ch.Type) {
			return response.Field(response.CodeParamInvalid, "channels", ch.Type,
				"第 "+itoa(i+1)+" 个渠道类型不合法，可用：webhook、telegram、email")
		}
		// 渠道各自的必填字段。
		switch ch.Type {
		case "webhook":
			if strings.TrimSpace(ch.URL) == "" {
				return response.Field(response.CodeParamInvalid, "channels", nil,
					"第 "+itoa(i+1)+" 个 webhook 渠道缺少 url")
			}
		case "telegram":
			if strings.TrimSpace(ch.Token) == "" || strings.TrimSpace(ch.Chat) == "" {
				return response.Field(response.CodeParamInvalid, "channels", nil,
					"第 "+itoa(i+1)+" 个 telegram 渠道需要同时填写 token 与 chat")
			}
		case "email":
			if strings.TrimSpace(ch.Host) == "" || strings.TrimSpace(ch.To) == "" {
				return response.Field(response.CodeParamInvalid, "channels", nil,
					"第 "+itoa(i+1)+" 个 email 渠道需要同时填写 host 与 to")
			}
		}
	}
	if in.SilenceFor < 0 {
		return response.Field(response.CodeParamOutOfRange, "silence_for", in.SilenceFor,
			"静默期不能为负数")
	}
	if in.Duration < 0 {
		return response.Field(response.CodeParamOutOfRange, "duration", in.Duration,
			"持续时长不能为负数")
	}
	return nil
}

// validAlertType 判断告警类型是否合法。
func validAlertType(t string) bool {
	switch t {
	case model.AlertNodeOffline, model.AlertNodeCPU, model.AlertNodeMem, model.AlertNodeDisk,
		model.AlertRuleSyncFailed, model.AlertUserTrafficPct, model.AlertRuleTrafficPct,
		model.AlertNodeTrafficPct, model.AlertCertExpire:
		return true
	}
	return false
}

// validChannelType 判断通知渠道类型是否合法。
func validChannelType(t string) bool {
	switch t {
	case "webhook", "telegram", "email":
		return true
	}
	return false
}
