package app

import (
	"sync"

	"go.uber.org/zap"
)

// Event 是推送给 WebSocket 订阅者的事件。
//
// Type 取值：
//
//	config_changed     配置版本变更（节点据此立即拉取）
//	node_online        节点上线
//	node_offline       节点离线
//	node_metrics       节点实时指标
//	rule_sync          规则同步结果
//	alert_fired        触发告警
//	alert_resolved     告警恢复
type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// subscriber 是一个订阅者（浏览器 WebSocket 或节点长连接）。
type subscriber struct {
	ch   chan Event
	name string
	// isNode 标记是否为节点连接——节点只需要 config_changed 类事件。
	isNode bool
	// nodeID 为节点连接对应的节点 ID，0 表示浏览器连接。
	nodeID uint64
}

// Hub 是进程内的发布订阅枢纽。
//
// 设计取舍：规格书要求「不引入消息队列、不引入 Redis」，
// 因此用带缓冲的 channel 做进程内广播，慢消费者会被主动断开而不是阻塞发布者。
type Hub struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
	log  *zap.Logger
	// closed 标记枢纽是否已关闭，避免关闭后仍有注册。
	closed bool
}

// subscriberBuffer 是单个订阅者的发送缓冲。
//
// 取 64：足以吸收一次配置变更引发的突发事件，
// 同时保证慢消费者不会占用过多内存。
const subscriberBuffer = 64

// newHub 构造推送枢纽。
func newHub(log *zap.Logger) *Hub {
	return &Hub{
		subs: make(map[*subscriber]struct{}),
		log:  log,
	}
}

// Subscribe 注册一个订阅者并返回其事件通道与取消函数。
//
// 参数 name 用于日志标识；nodeID 为 0 表示浏览器订阅者，
// 非 0 表示节点长连接（只接收 config_changed 事件）。
// 返回的 cancel 必须被调用（通常 defer 之），否则会造成订阅泄漏。
func (h *Hub) Subscribe(name string, nodeID uint64) (<-chan Event, func()) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		// 已关闭时返回一个已关闭的通道，调用方读取会立即得到零值并退出。
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	sub := &subscriber{
		ch:     make(chan Event, subscriberBuffer),
		name:   name,
		isNode: nodeID > 0,
		nodeID: nodeID,
	}
	h.subs[sub] = struct{}{}
	count := len(h.subs)
	h.mu.Unlock()

	h.log.Debug("订阅者已注册", zap.String("name", name), zap.Uint64("node_id", nodeID), zap.Int("total", count))

	return sub.ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[sub]; ok {
			delete(h.subs, sub)
			close(sub.ch)
		}
		h.mu.Unlock()
		h.log.Debug("订阅者已注销", zap.String("name", name))
	}
}

// Broadcast 把事件推送给所有订阅者。
//
// 非阻塞语义：订阅者缓冲满时视为「消费过慢」，直接跳过该事件而不是阻塞。
// 对于配置变更事件，跳过的订阅者会由节点侧的轮询兜底保证最终一致。
func (h *Hub) Broadcast(ev Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	dropped := 0
	// Sends are nonblocking; retain the read lock so unsubscribe cannot close a
	// channel between choosing a subscriber and delivering the event.
	for s := range h.subs {
		// 节点连接只需要配置变更类事件，其余事件推给浏览器。
		if s.isNode && ev.Type != "config_changed" {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			dropped++
		}
	}
	if dropped > 0 {
		h.log.Warn("部分订阅者消费过慢，事件被丢弃",
			zap.String("event", ev.Type), zap.Int("dropped", dropped))
	}
}

// BroadcastToNode 把事件定向推送给某个节点的长连接。
//
// 用于节点升级、重启等只与该节点相关的指令。
func (h *Hub) BroadcastToNode(nodeID uint64, ev Event) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	sent := false
	for s := range h.subs {
		if s.nodeID != nodeID {
			continue
		}
		select {
		case s.ch <- ev:
			sent = true
		default:
		}
	}
	return sent
}

// NodeConnected 判断指定节点是否持有长连接。
//
// 供「配置下发」判断能否走即时推送路径。
func (h *Hub) NodeConnected(nodeID uint64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if s.nodeID == nodeID {
			return true
		}
	}
	return false
}

// SubscriberCount 返回当前订阅者数量，供 /system/status 展示。
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Close 关闭枢纽并释放所有订阅者。
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for s := range h.subs {
		close(s.ch)
		delete(h.subs, s)
	}
}
