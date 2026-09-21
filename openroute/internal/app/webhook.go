package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 8.18「Webhook（出站通知）」。
//
// 请求体统一格式：
//
//	{
//	  "event": "node.offline",
//	  "timestamp": "2026-01-01T12:00:00Z",
//	  "data": { "node_id": 12, "node_name": "HK-01", "offline_seconds": 25 }
//	}
//
// 请求头带 X-OpenRoute-Event 与 X-OpenRoute-Signature（HMAC-SHA256，密钥为 secret-key）。
// 失败重试 3 次，间隔 5s / 30s / 300s。

// Webhook 事件名常量（与规格书 8.18 表格逐条对应）。
const (
	EventNodeOnline        = "node.online"
	EventNodeOffline       = "node.offline"
	EventRuleSyncFailed    = "rule.sync_failed"
	EventRuleSyncOK        = "rule.sync_ok"
	EventAlertFired        = "alert.fired"
	EventAlertResolved     = "alert.resolved"
	EventMigrationFinished = "migration.finished"
	EventBackupFinished    = "backup.finished"
	EventWebhookTest       = "webhook.test"
)

// Webhook 请求头名。
const (
	// HeaderWebhookEvent 标识事件类型。
	HeaderWebhookEvent = "X-OpenRoute-Event"
	// HeaderWebhookSignature 是请求体的 HMAC-SHA256 十六进制签名。
	HeaderWebhookSignature = "X-OpenRoute-Signature"
)

// webhookRetryDelays 是失败重试的间隔序列（规格书 8.18：5s / 30s / 300s）。
//
// 数组长度即重试次数；重试全部失败后放弃并记录日志。
var webhookRetryDelays = []time.Duration{5 * time.Second, 30 * time.Second, 300 * time.Second}

// webhookQueueSize 是异步发送队列的容量。
//
// 满了之后 SendAsync 会丢弃并记日志，而不是阻塞调用方——
// 转发链路绝不能被一个挂掉的 Webhook 端点拖慢。
const webhookQueueSize = 256

// defaultWebhookTimeout 是单次 HTTP 请求超时。
const defaultWebhookTimeout = 10 * time.Second

// WebhookPayload 是出站通知的请求体。
type WebhookPayload struct {
	Event     string      `json:"event"`
	Timestamp string      `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// WebhookService 负责把面板事件推送给用户配置的 Webhook 地址。
//
// 两种发送方式：
//   - SendAsync：即发即忘，投递到内部队列后立刻返回，转发 / 心跳链路绝不阻塞；
//   - SendSync：同步发送一次，供 POST /api/v1/webhooks/test 使用，
//     让用户能立刻知道地址填得对不对。
type WebhookService struct {
	app *App

	client *http.Client
	// now 可注入，便于单测固定时间戳。
	now func() time.Time
	// retryDelays 可注入，便于单测把 5s/30s/300s 压缩掉。
	retryDelays []time.Duration

	// queue 是异步发送队列；startOnce 保证后台消费协程只起一个。
	queue     chan webhookJob
	startOnce sync.Once
	closedMu  sync.RWMutex
	closed    bool
}

// webhookJob 是一条待发送的异步任务。
type webhookJob struct {
	url   string
	event string
	data  interface{}
}

// NewWebhookService 构造 Webhook 发送器。
//
// 参数 a 为运行时依赖容器；返回可立即使用的实例。
func NewWebhookService(a *App) *WebhookService {
	return &WebhookService{
		app:         a,
		client:      &http.Client{Timeout: defaultWebhookTimeout},
		now:         func() time.Time { return time.Now().UTC() },
		retryDelays: webhookRetryDelays,
		queue:       make(chan webhookJob, webhookQueueSize),
	}
}

// ValidWebhookEvent 校验事件名是否属于规格书 8.18 定义的事件集合。
//
// 用于 /webhooks/test 的参数校验：拒绝拼错的事件名比「发一个没人认识的事件」更容易排查。
func ValidWebhookEvent(ev string) bool {
	switch ev {
	case EventNodeOnline, EventNodeOffline, EventRuleSyncFailed, EventRuleSyncOK,
		EventAlertFired, EventAlertResolved, EventMigrationFinished, EventBackupFinished, EventWebhookTest:
		return true
	}
	return false
}

// ------------ 业务事件的便捷封装 ------------

// SendNodeOnline 通知节点上线。
func (s *WebhookService) SendNodeOnline(data map[string]interface{}) {
	s.SendAsync(s.SettingWebhookURL(), EventNodeOnline, data)
}

// SendNodeOffline 通知节点离线。
//
// 离线事件是最常用的一类，data 建议带 node_id / node_name / offline_seconds。
func (s *WebhookService) SendNodeOffline(data map[string]interface{}) {
	s.SendAsync(s.SettingWebhookURL(), EventNodeOffline, data)
}

// SendRuleSyncFailed 通知规则同步失败。
func (s *WebhookService) SendRuleSyncFailed(data map[string]interface{}) {
	s.SendAsync(s.SettingWebhookURL(), EventRuleSyncFailed, data)
}

// SendRuleSyncOK 通知规则同步恢复。
func (s *WebhookService) SendRuleSyncOK(data map[string]interface{}) {
	s.SendAsync(s.SettingWebhookURL(), EventRuleSyncOK, data)
}

// SendAlertFired 通知触发告警。
func (s *WebhookService) SendAlertFired(data map[string]interface{}) {
	s.SendAsync(s.SettingWebhookURL(), EventAlertFired, data)
}

// SendAlertResolved 通知告警恢复。
func (s *WebhookService) SendAlertResolved(data map[string]interface{}) {
	s.SendAsync(s.SettingWebhookURL(), EventAlertResolved, data)
}

// SendMigrationFinished 通知迁移完成。
func (s *WebhookService) SendMigrationFinished(data map[string]interface{}) {
	s.SendAsync(s.SettingWebhookURL(), EventMigrationFinished, data)
}

// SendBackupFinished 通知备份完成。
func (s *WebhookService) SendBackupFinished(data map[string]interface{}) {
	s.SendAsync(s.SettingWebhookURL(), EventBackupFinished, data)
}

// ------------ 发送 ------------

// SettingWebhookURL 返回系统设置里配置的默认 Webhook 地址。
//
// 未配置时返回空串；调用方据此跳过发送（个人自用默认不配 Webhook，不该报错）。
func (s *WebhookService) SettingWebhookURL() string {
	if s.app.Setting == nil {
		return ""
	}
	return trimSpace(s.app.Setting.GetString(modelSettingWebhookURL))
}

// modelSettingWebhookURL 是系统设置中 Webhook 地址的键名。
//
// 直接引用 model 常量会在无设置服务的测试环境下多一次依赖，此处以常量形式固化。
const modelSettingWebhookURL = "webhook_url"

// SendAsync 异步发送一条 Webhook，永不阻塞调用方。
//
// 语义：
//   - 地址为空时直接返回（未配置 Webhook 是正常状态，不算错误）；
//   - 队列满时丢弃并记录告警，绝不阻塞转发链路；
//   - 重试在后台进行，调用方拿不到结果——需要结果请用 SendSync。
//
// 参数 url 为目标地址；event 为事件名；data 为事件负载。
func (s *WebhookService) SendAsync(url string, event string, data interface{}) {
	url = trimSpace(url)
	if url == "" {
		return
	}
	s.startWorker()

	s.closedMu.RLock()
	closed := s.closed
	s.closedMu.RUnlock()
	if closed {
		return
	}

	select {
	case s.queue <- webhookJob{url: url, event: event, data: data}:
	default:
		s.app.Log.Warn("Webhook 队列已满，事件被丢弃",
			zap.String("event", event), zap.String("url", url))
	}
}

// SendSync 同步发送一次 Webhook；后台事件由 SendAsync 重试。
//
// 供 POST /api/v1/webhooks/test 使用：用户点击「测试」时应当立刻看到成功或失败，
// 而不是等后台重试完才知道地址填错了。
//
// 参数 ctx；url 为目标地址；event 为事件名；data 为负载。返回错误。
func (s *WebhookService) SendSync(ctx context.Context, url, event string, data interface{}) error {
	url = trimSpace(url)
	if url == "" {
		return response.New(response.CodeParamInvalid, "Webhook 地址不能为空")
	}
	if event == "" {
		event = EventAlertFired
	}
	if !ValidWebhookEvent(event) {
		return response.Field(response.CodeParamInvalid, "event", event,
			"事件名必须是规格书 8.18 定义的事件之一")
	}

	body, err := s.buildPayload(event, data)
	if err != nil {
		return response.Wrap(response.CodeInternal, err, "序列化 Webhook 请求体失败")
	}
	return s.post(ctx, url, event, body)
}

// buildPayload 把事件与负载序列化为请求体，并附带时间戳。
func (s *WebhookService) buildPayload(event string, data interface{}) ([]byte, error) {
	p := WebhookPayload{
		Event:     event,
		Timestamp: s.now().Format(time.RFC3339),
		Data:      data,
	}
	raw := util.ToJSON(p)
	if raw == "null" {
		return nil, fmt.Errorf("事件负载无法序列化为 JSON")
	}
	return []byte(raw), nil
}

// deliver 执行一次带重试的投递。
//
// 重试语义（规格书 8.18）：失败后按 5s / 30s / 300s 重试 3 次；
// 期间若 ctx 被取消（请求断开或进程退出）则立即停止重试。
func (s *WebhookService) deliver(ctx context.Context, url, event string, body []byte) error {
	var lastErr error
	attempts := 1 + len(s.retryDelays)

	for i := 0; i < attempts; i++ {
		if i > 0 {
			// 退避等待期间也要能感知取消，否则关闭进程会被最长的 300s 拖住。
			delay := s.retryDelays[i-1]
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-s.app.Done():
				timer.Stop()
				return fmt.Errorf("面板正在关闭，放弃重试")
			case <-timer.C:
			}
		}

		lastErr = s.post(ctx, url, event, body)
		if lastErr == nil {
			return nil
		}
		s.app.Log.Warn("Webhook 投递失败",
			zap.String("event", event), zap.String("url", url),
			zap.Int("attempt", i+1), zap.Error(lastErr))
	}
	return lastErr
}

// post 发送一次 HTTP POST。
//
// 请求头（规格书 8.18）：
//   - X-OpenRoute-Event：事件名；
//   - X-OpenRoute-Signature：HMAC-SHA256(body, secret-key) 的十六进制串；
//   - Content-Type：application/json。
func (s *WebhookService) post(ctx context.Context, url, event string, body []byte) error {
	// 同步测试遵守请求取消；异步队列使用自己的生命周期上下文。
	reqCtx := ctx

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造 Webhook 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderWebhookEvent, event)
	req.Header.Set(HeaderWebhookSignature, util.HMACSHA256Hex(s.app.Config.SecretKey, string(body)))
	req.Header.Set("User-Agent", "OpenRoute/"+Version)

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			s.app.Log.Debug("关闭 Webhook 响应体失败", zap.Error(cerr))
		}
	}()

	// 2xx 视为成功；其余状态码都重试——包括 4xx，
	// 因为个人自用场景下对方网关返回 403/404 往往只是配置还没生效。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Webhook 返回非成功状态码 %d", resp.StatusCode)
	}
	return nil
}

// httpClient 返回注入的 HTTP 客户端或默认客户端。
func (s *WebhookService) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return &http.Client{Timeout: defaultWebhookTimeout}
}

// startWorker 启动后台消费协程，重复调用只生效一次。
//
// 消费协程由 App 生命周期托管：App.Stop() 时会一同退出。
func (s *WebhookService) startWorker() {
	s.startOnce.Do(func() {
		s.app.Go(func(ctx context.Context) {
			for {
				select {
				case <-ctx.Done():
					return
				case <-s.app.Done():
					return
				case job := <-s.queue:
					s.handle(ctx, job)
				}
			}
		})
	})
}

// handle 处理一条异步任务：序列化 + 带重试投递，失败只记日志。
func (s *WebhookService) handle(ctx context.Context, job webhookJob) {
	body, err := s.buildPayload(job.event, job.data)
	if err != nil {
		s.app.Log.Warn("序列化 Webhook 请求体失败",
			zap.String("event", job.event), zap.Error(err))
		return
	}
	if err := s.deliver(ctx, job.url, job.event, body); err != nil {
		s.app.Log.Warn("Webhook 最终投递失败（已重试 %d 次）",
			zap.String("event", job.event), zap.String("url", job.url),
			zap.Int("retries", len(s.retryDelays)), zap.Error(err))
	}
}

// Close 停止接受新的异步任务。
//
// 用于优雅关闭：已在队列中的任务会由消费协程继续处理，新任务被丢弃。
func (s *WebhookService) Close() {
	s.closedMu.Lock()
	s.closed = true
	s.closedMu.Unlock()
}

// QueueLen 返回当前待发送队列长度，供 /system/status 观测积压情况。
func (s *WebhookService) QueueLen() int { return len(s.queue) }
