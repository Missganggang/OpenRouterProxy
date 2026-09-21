package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 本文件实现规格书 8.8 的设备组接口所依赖的全部业务逻辑，
// 以及规则分组 / 用户分组 / 节点分组的 CRUD。
//
// 设计取舍：设备组的 Config 是**自由 JSON**（规格书 4.2.5 明确要求「前端用表单编辑、
// 后端存原文」），因此本服务做的是「解析 → 校验 → 原样回写」，
// 不会把用户的字段值规范化后写回，避免覆盖前端/节点侧还不认识的扩展字段。

// ───────────────────────── 协议与枚举常量 ─────────────────────────

// 入口组 / 出口组的通信协议取值（规格书 4.3 / 4.4）。
const (
	ProtocolWS     = "ws"
	ProtocolHTTP   = "http"
	ProtocolTLS    = "tls"
	ProtocolDirect = "direct"
)

// 出口组的连接方式（规格书 4.4）。
const (
	ConnectTypeDynIPv4 = "dyn_ip4"
	ConnectTypeDynIPv6 = "dyn_ip6"
	ConnectTypeStatic  = "static"
)

// TLS 入站策略（规格书 4.3）。
const (
	// TLSInboundNone 不处理。
	TLSInboundNone = 0
	// TLSInboundStrip 强制 TLS 剥离。
	TLSInboundStrip = 1
	// TLSInboundSNISplit SNI 分流模式。
	TLSInboundSNISplit = 2
)

// 整流选项取值（规格书 6.8）。
const (
	ShapeRejectTLS12 = 1    // 拒绝 TLS 1.2 握手
	ShapeRejectTLS   = 2    // 拒绝全部 TLS 握手
	ShapeRejectHTTP1 = 3    // 拒绝 HTTP/1.x
	ShapeRejectWS    = 4    // 拒绝 WebSocket 升级
	ShapeSkipIPv6    = 2000 // 跳过 IPv6 目标
)

// ShapeOption 是整流选项的元数据。
type ShapeOption struct {
	Value int    `json:"value"`
	Name  string `json:"name"`
	Label string `json:"label"`
}

// ShapeOptions 返回全部整流选项，供前端渲染多选框（规格书 6.8 表格）。
func ShapeOptions() []ShapeOption {
	return []ShapeOption{
		{Value: ShapeRejectTLS12, Name: "Shape_RejectTLS12", Label: "拒绝 TLS 1.2 握手"},
		{Value: ShapeRejectTLS, Name: "Shape_RejectTLS", Label: "拒绝全部 TLS 握手"},
		{Value: ShapeRejectHTTP1, Name: "Shape_RejectHttp1", Label: "拒绝 HTTP/1.x"},
		{Value: ShapeRejectWS, Name: "Shape_RejectWs", Label: "拒绝 WebSocket 升级"},
		{Value: ShapeSkipIPv6, Name: "Shape_SkipIPv6", Label: "跳过 IPv6 目标"},
	}
}

// ValidShape 判断整流选项取值是否合法。
func ValidShape(v int) bool {
	for _, o := range ShapeOptions() {
		if o.Value == v {
			return true
		}
	}
	return false
}

// chfp 允许的 TLS 指纹取值（规格书 4.5 MUST 严格保持）。
var chfpAllowed = []string{"chrome", "firefox", "safari", "ios", "android", "edge", "360", "qq"}

// ChfpOptions 返回允许的指纹列表，供前端渲染下拉框。
func ChfpOptions() []string {
	out := make([]string, len(chfpAllowed))
	copy(out, chfpAllowed)
	return out
}

// ValidChfp 判断 TLS 指纹取值是否合法（空值合法 = 使用 Go 默认指纹）。
func ValidChfp(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return true
	}
	for _, a := range chfpAllowed {
		if a == v {
			return true
		}
	}
	return false
}

// ValidProtocol 判断协议取值是否合法。
func ValidProtocol(p string) bool {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case ProtocolWS, ProtocolHTTP, ProtocolTLS, ProtocolDirect:
		return true
	}
	return false
}

// ProtocolSupportsUDP 判断协议是否支持 UDP 链式/隧道（direct 与 tls 支持原生 UDP）。
func ProtocolSupportsUDP(p string) bool {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case ProtocolTLS, ProtocolDirect:
		return true
	}
	return false
}

// ───────────────────────── 配置结构体（规格书 4.3 / 4.4 / 4.5） ─────────────────────────

// WSConfig 是 WebSocket / FakeHTTP 的伪装配置（规格书 4.5）。
//
// 说明：不是完整 WebSocket 协议，仅做握手伪装；
// `http`（FakeHTTP）协议复用同一份配置来设置 host / path，
// 区别只是不发送 `Upgrade` / `Switch Protocol` 头。
type WSConfig struct {
	Host string `json:"host,omitempty"` // 伪装 Host 头
	Path string `json:"path,omitempty"` // 伪装请求路径
	// Request / Response 为原始 HTTP 报文模板，MUST 允许用户自定义
	// （用于绕过特征检测），因此这里不做任何转义或校验。
	Request  string `json:"request,omitempty"`
	Response string `json:"response,omitempty"`
}

// TLSConfig 是隧道协议侧的 TLS 细节（规格书 4.5 的 tls_simple）。
//
// 注意：这是**出口侧 / 隧道**的 TLS 参数，与规则级的
// 「TLS 入站配置」（证书分流）不是同一个结构，后者定义在 RuleTLSConfig。
type TLSConfig struct {
	SNI  string   `json:"sni,omitempty"`
	ALPN []string `json:"alpn,omitempty"`
	CHFP string   `json:"chfp,omitempty"` // chrome|firefox|safari|ios|android|edge|360|qq
}

// InboundConfig 是入口组配置（规格书 4.3，字段顺序与 MUST 输出的 JSON 一致）。
//
// json tag 统一 snake_case；字段顺序刻意与规格书 4.3 的示例 JSON 完全对齐，
// 这样 DefaultInboundConfig 序列化后与规格书逐字节一致（有单元测试保证）。
type InboundConfig struct {
	AllowedHost       []string  `json:"allowed_host"`
	BlockedHost       []string  `json:"blocked_host"`
	BlockedPath       []string  `json:"blocked_path"`
	BlockedProtocol   []string  `json:"blocked_protocol"`
	TLSInboundPolicy  int       `json:"tls_inbound_policy"`
	TLSRejectEmptySNI bool      `json:"tls_reject_empty_sni"`
	DisableUDP        bool      `json:"disable_udp"`
	UDPOverTCP        bool      `json:"udp_over_tcp"`
	IPv6Group         []uint64  `json:"ipv6_group"`
	MaxFail           int       `json:"max_fail"`
	FailTimoutSec     int       `json:"fail_timout_sec"`
	ReverseGroup      []uint64  `json:"reverse_group"`
	Protocol          string    `json:"protocol"`
	TLS               TLSConfig `json:"tls"`
	// WS 为**扩展**字段：规格书 4.3 的入口组示例里没有它，
	// 但附录 A 的速查表明确列出 `ws.host / ws.path / ws.request / ws.response`，
	// 且 4.5 说明 ws 配置对入口与出口都适用，因此保留。
	// omitempty 保证默认值序列化时不会污染规格书 4.3 的示例输出。
	WS *WSConfig `json:"ws,omitempty"`
}

// OutboundConfig 是出口组配置（规格书 4.4，字段顺序与 MUST 输出的 JSON 一致）。
type OutboundConfig struct {
	ConnectType    string   `json:"connect_type"`
	ConnectAddress string   `json:"connect_address"`
	ConnectPort    int      `json:"connect_port"`
	Protocol       string   `json:"protocol"`
	WS             WSConfig `json:"ws"`
	UDPOverTCP     bool     `json:"udp_over_tcp"`
	// TLS 为**扩展**字段：规格书 4.4 的示例只列了 ws，但 4.4 正文写明
	// 「`ws` / `tls`：协议细节，见 4.5」，附录 A 也列出 tls。
	// omitempty 保证默认值序列化时与规格书 4.4 示例一致（tls 为空对象时不输出）。
	TLS *TLSConfig `json:"tls,omitempty"`
}

// RuleTLSConfig 是规则级 TLS 入站配置（规格书 4.5 的「用于 SNI 分流 / 自定义证书」）。
type RuleTLSConfig struct {
	EmptySNI      bool     `json:"empty_sni"`
	ForceEmptySNI bool     `json:"force_empty_sni"`
	SNI           string   `json:"sni"`
	ALPN          []string `json:"alpn"`
	Cert          []string `json:"cert"`
	Key           []string `json:"key"`
}

// DefaultInboundConfig 返回入口组的默认配置。
//
// 返回值序列化后 MUST 与规格书 4.3 的示例 JSON 完全一致（含字段顺序）。
func DefaultInboundConfig() InboundConfig {
	return InboundConfig{
		AllowedHost:       []string{},
		BlockedHost:       []string{},
		BlockedPath:       []string{},
		BlockedProtocol:   []string{},
		TLSInboundPolicy:  TLSInboundNone,
		TLSRejectEmptySNI: false,
		DisableUDP:        false,
		UDPOverTCP:        false,
		IPv6Group:         []uint64{},
		MaxFail:           3,
		FailTimoutSec:     30,
		ReverseGroup:      []uint64{},
		Protocol:          ProtocolTLS,
		TLS:               TLSConfig{},
	}
}

// DefaultOutboundConfig 返回出口组的默认配置。
//
// 返回值序列化后 MUST 与规格书 4.4 的示例 JSON 完全一致（含字段顺序）。
func DefaultOutboundConfig() OutboundConfig {
	return OutboundConfig{
		ConnectType:    ConnectTypeStatic,
		ConnectAddress: "node.example.com",
		ConnectPort:    2333,
		Protocol:       ProtocolWS,
		WS:             WSConfig{},
		UDPOverTCP:     false,
	}
}

// ParseInboundConfig 解析入口组 Config JSON；空值返回默认配置。
//
// 解析失败返回错误，由调用方转成 40001。
func ParseInboundConfig(raw []byte) (InboundConfig, error) {
	out := DefaultInboundConfig()
	if len(raw) == 0 {
		return out, nil
	}
	if err := jsonUnmarshal(string(raw), &out); err != nil {
		return out, err
	}
	// 解析后补默认值：JSON 里缺失的字段会保留默认配置里的值，
	// 但显式写了 null 的数组字段会被置为 nil，这里统一修正为空切片。
	if out.AllowedHost == nil {
		out.AllowedHost = []string{}
	}
	if out.BlockedHost == nil {
		out.BlockedHost = []string{}
	}
	if out.BlockedPath == nil {
		out.BlockedPath = []string{}
	}
	if out.BlockedProtocol == nil {
		out.BlockedProtocol = []string{}
	}
	if out.IPv6Group == nil {
		out.IPv6Group = []uint64{}
	}
	if out.ReverseGroup == nil {
		out.ReverseGroup = []uint64{}
	}
	if out.MaxFail <= 0 {
		out.MaxFail = 3
	}
	if out.FailTimoutSec <= 0 {
		out.FailTimoutSec = 30
	}
	if out.Protocol == "" {
		out.Protocol = ProtocolTLS
	}
	return out, nil
}

// ParseOutboundConfig 解析出口组 Config JSON；空值返回默认配置。
func ParseOutboundConfig(raw []byte) (OutboundConfig, error) {
	out := DefaultOutboundConfig()
	out.WS = WSConfig{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := jsonUnmarshal(string(raw), &out); err != nil {
		return out, err
	}
	if out.Protocol == "" {
		out.Protocol = ProtocolWS
	}
	if out.ConnectType == "" {
		out.ConnectType = ConnectTypeStatic
	}
	return out, nil
}

// ───────────────────────── 设备组 Schema（规格书 8.8 /schema） ─────────────────────────

// SchemaField 描述配置 JSON 中的一个字段，供前端动态渲染表单。
type SchemaField struct {
	// Key 是配置 JSON 里的字段名（snake_case）。
	Key string `json:"key"`
	// Label 是中文标签。
	Label string `json:"label"`
	// Type 是控件类型：bool / int / string / select / multiselect /
	// string_array / uint_array / textarea / object。
	Type string `json:"type"`
	// Default 是默认值（与 DefaultInboundConfig / DefaultOutboundConfig 一致）。
	Default interface{} `json:"default"`
	// Options 是 select / multiselect 的候选值。
	Options []SchemaOption `json:"options,omitempty"`
	// Help 是字段说明（规格书 4.3 的语义原文，供 UI 直接展示）。
	Help string `json:"help,omitempty"`
	// Children 是嵌套子字段（ws / tls）。
	Children []SchemaField `json:"children,omitempty"`
}

// SchemaOption 是 select / multiselect 的一个候选项。
type SchemaOption struct {
	Value interface{} `json:"value"`
	Label string      `json:"label"`
}

// DeviceGroupSchema 是一种设备组的完整字段 schema。
type DeviceGroupSchema struct {
	Type   string        `json:"type"`
	Fields []SchemaField `json:"fields"`
	// TableFields 描述**表字段**（非 config JSON）的控件，
	// 出口组才有（balance、健康检查、failover_group_id）。
	TableFields []SchemaField `json:"table_fields,omitempty"`
	// Notices 是该类型组的强制提示（前端 MUST 展示）。
	Notices []string `json:"notices,omitempty"`
}

// GetDeviceGroupSchema 返回指定类型设备组的配置字段 schema（规格书 8.8 /schema）。
//
// 参数 groupType 取 model.GroupTypeInbound / model.GroupTypeOutbound；
// 其它取值返回 nil。
//
// 覆盖范围：规格书 4.3（入口组）与 4.4（出口组）的**每一个字段**，
// 含 4.5 的 ws / tls 子配置（chfp 的 8 个允许值）。
func (s *GroupService) GetDeviceGroupSchema(groupType string) *DeviceGroupSchema {
	switch groupType {
	case model.GroupTypeInbound:
		return &DeviceGroupSchema{
			Type:   model.GroupTypeInbound,
			Fields: inboundSchemaFields(),
			Notices: []string{
				"入口组不做面板侧负载均衡：组内每台机器各自独立监听与转发，" +
					"用户的多个入口地址由用户侧决定。",
				"connect 地址选择没有回退机制：若选用 IPv6 且目标不可达，不会自动回退 IPv4。",
			},
		}
	case model.GroupTypeOutbound:
		return &DeviceGroupSchema{
			Type:        model.GroupTypeOutbound,
			Fields:      outboundSchemaFields(),
			TableFields: outboundTableSchemaFields(),
		}
	}
	return nil
}

// inboundSchemaFields 构建入口组 config 的全部字段描述（规格书 4.3 + 附录 A）。
func inboundSchemaFields() []SchemaField {
	protoOpts := []SchemaOption{
		{Value: ProtocolTLS, Label: "TLS"},
		{Value: ProtocolWS, Label: "WebSocket"},
		{Value: ProtocolHTTP, Label: "HTTP"},
		{Value: ProtocolDirect, Label: "入口直出（direct）"},
	}
	return []SchemaField{
		{
			Key: "allowed_host", Label: "允许的目标域名白名单", Type: "string_array",
			Default: []string{},
			Help:    "后缀匹配，如 .qq.com 匹配 a.qq.com；空数组 = 不限制",
		},
		{
			Key: "blocked_host", Label: "禁止的目标域名黑名单", Type: "string_array",
			Default: []string{},
			Help:    "后缀匹配；优先级高于白名单",
		},
		{
			Key: "blocked_path", Label: "禁止的 HTTP 路径前缀", Type: "string_array",
			Default: []string{},
			Help:    "前缀匹配，/admin 会拦截 /admin 与 /admin/x",
		},
		{
			Key: "blocked_protocol", Label: "禁止的入站协议类型", Type: "multiselect",
			Default: []string{},
			Options: []SchemaOption{
				{Value: "socks", Label: "socks（SOCKS 握手）"},
				{Value: "fet", Label: "fet（完全加密的无法识别流量）"},
				{Value: "http", Label: "http"},
				{Value: "tls", Label: "tls"},
			},
			Help: "被禁止的入站协议类型会被直接拒绝",
		},
		{
			Key: "tls_inbound_policy", Label: "TLS 入站策略", Type: "select",
			Default: TLSInboundNone,
			Options: []SchemaOption{
				{Value: TLSInboundNone, Label: "0 - 不处理"},
				{Value: TLSInboundStrip, Label: "1 - 强制 TLS 剥离"},
				{Value: TLSInboundSNISplit, Label: "2 - SNI 分流模式"},
			},
			Help: "选 2 后同一入口组的其它规则也受该策略影响",
		},
		{
			Key: "tls_reject_empty_sni", Label: "拒绝无 SNI 的 TLS 握手", Type: "bool",
			Default: false,
			Help:    "SNI 分流模式下建议开启",
		},
		{
			Key: "disable_udp", Label: "完全关闭 UDP 转发", Type: "bool", Default: false,
		},
		{
			Key: "udp_over_tcp", Label: "UDP 走 TCP 隧道（UoT）", Type: "bool",
			Default: false,
			Help:    "关闭时 UDP 使用原生转发（28 字节 MTU 开销，简单加密）",
		},
		{
			Key: "ipv6_group", Label: "IPv6 优先策略", Type: "uint_array",
			Default: []uint64{},
			Help:    "[] 默认；[0] 所有出口优先 IPv6；[0,1,2] 所有出口优先 IPv6，但节点 1、2 优先 IPv4",
		},
		{
			Key: "max_fail", Label: "出口连续失败次数阈值", Type: "int",
			Default: 3,
			Help:    "连续失败达到该次数后摘除出口",
		},
		{
			Key: "fail_timout_sec", Label: "故障判定超时（秒）", Type: "int",
			Default: 30,
			Help:    "TCP 握手超过该时长即计为一次失败（字段名沿用 Nyanpass 拼写）",
		},
		{
			Key: "reverse_group", Label: "反向隧道设备组", Type: "uint_array",
			Default: []uint64{},
			Help:    "参与反向隧道的出口设备组 ID 列表（出口主动连接入口）",
		},
		{
			Key: "protocol", Label: "与出口通信的协议", Type: "select",
			Default: ProtocolTLS, Options: protoOpts,
		},
		{
			Key: "ws", Label: "WebSocket / FakeHTTP 伪装", Type: "object",
			Default:  nil,
			Help:     "protocol 选 ws / http 时生效；http 复用同一份配置但不下发 Upgrade 头",
			Children: wsSchemaFields(),
		},
		{
			Key: "tls", Label: "TLS 隧道细节", Type: "object",
			Default:  TLSConfig{},
			Help:     "protocol 选 tls 时生效",
			Children: tlsSchemaFields(),
		},
	}
}

// outboundSchemaFields 构建出口组 config 的全部字段描述（规格书 4.4 + 附录 A）。
func outboundSchemaFields() []SchemaField {
	protoOpts := []SchemaOption{
		{Value: ProtocolWS, Label: "WebSocket"},
		{Value: ProtocolHTTP, Label: "HTTP（FakeHTTP）"},
		{Value: ProtocolTLS, Label: "TLS"},
		{Value: ProtocolDirect, Label: "直连（direct，专线场景）"},
	}
	return []SchemaField{
		{
			Key: "connect_type", Label: "连接方式", Type: "select",
			Default: ConnectTypeStatic,
			Options: []SchemaOption{
				{Value: ConnectTypeDynIPv4, Label: "dyn_ip4 - 动态 IPv4"},
				{Value: ConnectTypeDynIPv6, Label: "dyn_ip6 - 动态 IPv6"},
				{Value: ConnectTypeStatic, Label: "static - 静态地址"},
			},
		},
		{
			Key: "connect_address", Label: "静态连接地址", Type: "string",
			Default: "node.example.com",
			Help:    "connect_type = static 时必填，支持域名或 IP",
		},
		{
			Key: "connect_port", Label: "静态连接端口", Type: "int",
			Default: 2333,
			Help:    "connect_type = static 时的连接端口",
		},
		{
			Key: "protocol", Label: "隧道协议", Type: "select",
			Default: ProtocolWS, Options: protoOpts,
		},
		{
			Key: "ws", Label: "WebSocket / FakeHTTP 伪装", Type: "object",
			Default:  WSConfig{},
			Help:     "protocol 选 ws / http 时生效",
			Children: wsSchemaFields(),
		},
		{
			Key: "tls", Label: "TLS 隧道细节", Type: "object",
			Default:  nil,
			Help:     "protocol 选 tls 时生效；不填 chfp 则使用 Go 默认 TLS 指纹",
			Children: tlsSchemaFields(),
		},
		{
			Key: "udp_over_tcp", Label: "UDP 走 TCP 隧道（UoT）", Type: "bool", Default: false,
		},
	}
}

// outboundTableSchemaFields 构建出口组**表字段**的控件描述（附录 A 的出口组清单）。
func outboundTableSchemaFields() []SchemaField {
	return []SchemaField{
		{
			Key: "balance", Label: "负载均衡策略", Type: "select",
			Default: model.BalanceLeastConn,
			Options: []SchemaOption{
				{Value: model.BalanceLeastConn, Label: "least_conn - 最少连接数（默认）"},
				{Value: model.BalanceRoundRobin, Label: "round_robin - 加权平滑轮询"},
				{Value: model.BalanceHashIP, Label: "hash_ip - 源 IP 哈希"},
				{Value: model.BalanceWeighted, Label: "weighted - 按权重随机"},
			},
		},
		{Key: "health_check_enable", Label: "启用健康检查", Type: "bool", Default: true},
		{Key: "health_check_interval", Label: "健康检查间隔（秒）", Type: "int", Default: 10},
		{Key: "health_check_timeout", Label: "健康检查超时（秒）", Type: "int", Default: 3},
		{
			Key: "health_check_fail_count", Label: "连续失败次数后摘除", Type: "int",
			Default: 3,
		},
		{
			Key: "health_check_succ_count", Label: "连续成功次数后恢复", Type: "int",
			Default: 2,
		},
		{
			Key: "failover_group_id", Label: "故障转移组", Type: "object",
			Default: 0,
			Help:    "本组所有出口均为 down 时转投该组；该组 MUST 具备相同的入口组权限配置 JSON",
		},
		{
			Key: "node_ids", Label: "组内节点（有序）", Type: "uint_array",
			Default: []uint64{},
			Help:    "顺序参与负载均衡初始化，可拖动排序",
		},
	}
}

// wsSchemaFields 构建 ws 子配置的字段描述（规格书 4.5）。
func wsSchemaFields() []SchemaField {
	return []SchemaField{
		{Key: "host", Label: "伪装 Host", Type: "string", Default: ""},
		{Key: "path", Label: "伪装路径", Type: "string", Default: ""},
		{
			Key: "request", Label: "请求报文模板", Type: "textarea",
			Default: "",
			Help:    `原始 HTTP 报文，如 "GET / HTTP/1.5\r\n\r\n"；可自定义以绕过特征检测`,
		},
		{
			Key: "response", Label: "响应报文模板", Type: "textarea",
			Default: "",
			Help:    `原始 HTTP 报文，如 "HTTP/1.5 200 OK\r\n\r\n"`,
		},
	}
}

// tlsSchemaFields 构建 tls 子配置的字段描述（规格书 4.5）。
func tlsSchemaFields() []SchemaField {
	opts := make([]SchemaOption, 0, len(chfpAllowed))
	for _, v := range chfpAllowed {
		opts = append(opts, SchemaOption{Value: v, Label: v})
	}
	return []SchemaField{
		{Key: "sni", Label: "TLS SNI", Type: "string", Default: ""},
		{
			Key: "alpn", Label: "ALPN 列表", Type: "string_array",
			Default: []string{"http/1.1"},
		},
		{
			Key: "chfp", Label: "TLS 指纹", Type: "select",
			Default: "", Options: append([]SchemaOption{{Value: "", Label: "（Go 默认指纹）"}}, opts...),
			Help: "不填则使用 Go 默认 TLS 指纹",
		},
	}
}

// ───────────────────────── GroupService ─────────────────────────

// GroupService 管理设备组、规则分组、用户分组、节点分组。
type GroupService struct {
	app *App

	// trackerMu 保护 trackers。故障转移状态是**运行期状态**（不入库），
	// 由健康检查任务通过 FailoverTracker 读写，面板重启后从「全部 up」重新开始，
	// 这是刻意的取舍：健康状态本来就会在下一次探测中很快重新收敛。
	trackerMu sync.Mutex
	trackers  map[uint64]*FailoverTracker
}

// NewGroupService 构建设备组服务。
func NewGroupService(a *App) *GroupService {
	return &GroupService{
		app:      a,
		trackers: make(map[uint64]*FailoverTracker),
	}
}

// GroupListFilter 是设备组列表的过滤条件。
type GroupListFilter struct {
	Type    string // inbound | outbound | 空 = 全部
	Keyword string // 名称模糊匹配
}

// DeviceGroupDetail 是设备组详情（规格书 8.8：含**解析后的 Config**）。
type DeviceGroupDetail struct {
	model.DeviceGroup
	// InboundConfig / OutboundConfig 二选一，取决于组的类型。
	InboundConfig  *InboundConfig  `json:"inbound_config,omitempty"`
	OutboundConfig *OutboundConfig `json:"outbound_config,omitempty"`
	// Nodes 是组成员节点的精简信息，按 node_ids 顺序排列。
	Nodes []DeviceGroupNodeBrief `json:"nodes"`
	// RuleCount 是引用该组的规则数（入口组含子规则）。
	RuleCount int64 `json:"rule_count"`
}

// DeviceGroupNodeBrief 是设备组成员节点的精简视图。
type DeviceGroupNodeBrief struct {
	ID          uint64 `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Online      bool   `json:"online"`
	Weight      int    `json:"weight"`
	MaxConn     int    `json:"max_conn"`
	CurrentConn int    `json:"current_conn"`
	ConnectHost string `json:"connect_host"`
	PublicIPv4  string `json:"public_ipv4"`
	PublicIPv6  string `json:"public_ipv6"`
}

// DeviceGroupInput 是创建设备组的入参（规格书 8.8 的请求体）。
type DeviceGroupInput struct {
	Name                 string     `json:"name"`
	Type                 string     `json:"type"`
	NodeIDs              []uint64   `json:"node_ids"`
	Config               model.JSON `json:"config"`
	Balance              string     `json:"balance"`
	HealthCheckEnable    *bool      `json:"health_check_enable"`
	HealthCheckInterval  int        `json:"health_check_interval"`
	HealthCheckTimeout   int        `json:"health_check_timeout"`
	HealthCheckFailCount int        `json:"health_check_fail_count"`
	HealthCheckSuccCount int        `json:"health_check_succ_count"`
	FailoverGroupID      uint64     `json:"failover_group_id"`
	Remark               string     `json:"remark"`
}

// List 返回设备组列表（规格书 8.8 的 ?type=）。
//
// 参数 ctx 为上下文；filter 为过滤条件。
// 返回设备组列表（按 type、id 排序）与错误。
func (s *GroupService) List(ctx context.Context, filter GroupListFilter) ([]model.DeviceGroup, error) {
	q := s.app.DB.WithContext(ctx).Model(&model.DeviceGroup{})
	if t := strings.TrimSpace(filter.Type); t != "" {
		q = q.Where("type = ?", t)
	}
	if kw := strings.TrimSpace(filter.Keyword); kw != "" {
		q = q.Where("name LIKE ?", "%"+kw+"%")
	}
	var out []model.DeviceGroup
	if err := q.Order("type asc, id asc").Find(&out).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询设备组失败")
	}
	if out == nil {
		out = []model.DeviceGroup{}
	}
	return out, nil
}

// Get 返回设备组详情，含解析后的 Config（规格书 8.8 GET /:id）。
//
// 参数 ctx 为上下文；id 为设备组 ID。
// 返回详情或 40401。
func (s *GroupService) Get(ctx context.Context, id uint64) (*DeviceGroupDetail, error) {
	g, err := s.mustGet(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.detail(ctx, g)
}

// detail 组装设备组详情。
func (s *GroupService) detail(ctx context.Context, g *model.DeviceGroup) (*DeviceGroupDetail, error) {
	det := &DeviceGroupDetail{DeviceGroup: *g, Nodes: []DeviceGroupNodeBrief{}}

	switch g.Type {
	case model.GroupTypeInbound:
		cfg, err := ParseInboundConfig(g.Config)
		if err != nil {
			return nil, response.Wrap(response.CodeInternal, err,
				fmt.Sprintf("设备组 %s 的 config 不是合法 JSON", g.Name))
		}
		det.InboundConfig = &cfg
	case model.GroupTypeOutbound:
		cfg, err := ParseOutboundConfig(g.Config)
		if err != nil {
			return nil, response.Wrap(response.CodeInternal, err,
				fmt.Sprintf("设备组 %s 的 config 不是合法 JSON", g.Name))
		}
		det.OutboundConfig = &cfg
	}

	det.Nodes = s.nodeBriefs(ctx, g.NodeIDs.AsUint64Slice())
	det.RuleCount = s.countRules(ctx, g)
	return det, nil
}

// nodeBriefs 按给定顺序加载节点精简信息。
//
// 参数 ids 为节点 ID（有序）；返回同样有序的列表（节点已被删除的项被跳过）。
func (s *GroupService) nodeBriefs(ctx context.Context, ids []uint64) []DeviceGroupNodeBrief {
	out := make([]DeviceGroupNodeBrief, 0, len(ids))
	if len(ids) == 0 {
		return out
	}
	var nodes []model.Node
	if err := s.app.DB.WithContext(ctx).Where("id IN ?", ids).Find(&nodes).Error; err != nil {
		s.app.Log.Warn("加载设备组成员节点失败", zap.Error(err))
		return out
	}
	byID := make(map[uint64]model.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, id := range ids {
		n, ok := byID[id]
		if !ok {
			continue
		}
		out = append(out, DeviceGroupNodeBrief{
			ID: id, Name: n.Name, Role: n.Role, Online: n.Online,
			Weight: n.Weight, MaxConn: n.MaxConn, CurrentConn: n.CurrentConn,
			ConnectHost: n.TunnelHost(), PublicIPv4: n.PublicIPv4, PublicIPv6: n.PublicIPv6,
		})
	}
	return out
}

// countRules 统计引用该设备组的规则数。
//
// 入口组被引用的位置：forward_rules.inbound_group_id（含子规则，子规则也选同一入口组）。
// 出口组被引用的位置：forward_rules.outbound_group_id、chain_groups、
// reverse_group_id，以及入口组 config 的 reverse_group。
func (s *GroupService) countRules(ctx context.Context, g *model.DeviceGroup) int64 {
	db := s.app.DB.WithContext(ctx)
	var n int64
	if g.IsInbound() {
		if err := db.Model(&model.ForwardRule{}).
			Where("inbound_group_id = ?", g.ID).Count(&n).Error; err != nil {
			s.app.Log.Warn("统计入口组引用失败", zap.Uint64("group_id", g.ID), zap.Error(err))
		}
		return n
	}

	// 出口组：分别统计三种引用方式，去重交给前端（数量级很小，不必优化）。
	var direct, chain, reverse int64
	_ = db.Model(&model.ForwardRule{}).Where("outbound_group_id = ?", g.ID).Count(&direct).Error
	_ = db.Model(&model.ForwardRule{}).
		Where("chain_groups LIKE ?", fmt.Sprintf("%%%d%%", g.ID)).Count(&chain).Error
	_ = db.Model(&model.ForwardRule{}).Where("reverse_group_id = ?", g.ID).Count(&reverse).Error
	return direct + chain + reverse
}

// Create 创建设备组。
//
// 校验顺序（任一失败即返回）：名称非空 → 类型合法 → 名称查重（EqualFold）
// → 配置语义校验 → 节点角色匹配 → 落库。
//
// 参数 ctx 为上下文；in 为创建入参。
// 返回创建后的设备组详情或 AppError。
func (s *GroupService) Create(ctx context.Context, in DeviceGroupInput) (*DeviceGroupDetail, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, response.Field(response.CodeParamInvalid, "name", in.Name, "组名不能为空")
	}
	// 长度按字节截断保护：name 列为 varchar(64)。
	if len(name) > 64 {
		name = util.Truncate(name, 64)
	}
	if in.Type != model.GroupTypeInbound && in.Type != model.GroupTypeOutbound {
		return nil, response.Field(response.CodeParamInvalid, "type", in.Type,
			"组类型只能是 inbound 或 outbound")
	}
	if err := s.checkNameConflict(ctx, name, 0); err != nil {
		return nil, err
	}

	g := &model.DeviceGroup{
		Name:    name,
		Type:    in.Type,
		NodeIDs: model.FromAny(dedupeUint64(in.NodeIDs)),
		Config:  in.Config,
		Remark:  util.Truncate(strings.TrimSpace(in.Remark), 255),
	}
	if len(g.Config) == 0 || string(g.Config) == "null" {
		// 未提供 config 时写入该类型的默认配置，保证前端拿到一个可编辑的表单。
		if in.Type == model.GroupTypeInbound {
			g.Config = model.FromAny(DefaultInboundConfig())
		} else {
			g.Config = model.FromAny(DefaultOutboundConfig())
		}
	}
	s.applyMeta(g, in)

	// 语义校验（含故障转移组兼容性、链式限制、节点角色）。
	if err := s.ValidateDeviceGroupConfig(ctx, g); err != nil {
		return nil, err
	}

	if err := s.app.DB.WithContext(ctx).Create(g).Error; err != nil {
		return nil, s.wrapWriteErr(err, "创建设备组失败")
	}
	s.app.BumpConfigVersion(fmt.Sprintf("创建设备组 %s", g.Name))
	s.app.Log.Info("设备组已创建", zap.Uint64("id", g.ID), zap.String("name", g.Name),
		zap.String("type", g.Type))
	return s.detail(ctx, g)
}

// Update 更新设备组。
//
// 更新语义为「整体替换」：请求中未提供的表字段保持原值，
// 但 config 若被提供则整体替换（与规格书 4.2.5「后端存原文」一致）。
//
// 参数 ctx 为上下文；id 为设备组 ID；in 为更新入参。
// 返回更新后的详情或 AppError。
func (s *GroupService) Update(ctx context.Context, id uint64, in DeviceGroupInput) (*DeviceGroupDetail, error) {
	g, err := s.mustGet(ctx, id)
	if err != nil {
		return nil, err
	}
	before := util.Clone(*g)

	if name := strings.TrimSpace(in.Name); name != "" && !util.EqualFoldName(name, g.Name) {
		if err := s.checkNameConflict(ctx, name, id); err != nil {
			return nil, err
		}
		g.Name = util.Truncate(name, 64)
	}
	// 类型不允许修改：改类型会让已有规则的语义失效（入口组与出口组的字段集不同）。
	if t := strings.TrimSpace(in.Type); t != "" && t != g.Type {
		return nil, response.Field(response.CodeParamInvalid, "type", in.Type,
			"设备组类型创建后不可修改，请新建一个组")
	}
	if in.NodeIDs != nil {
		g.NodeIDs = model.FromAny(dedupeUint64(in.NodeIDs))
	}
	if len(in.Config) > 0 && string(in.Config) != "null" {
		g.Config = in.Config
	}
	if in.Remark != "" {
		g.Remark = util.Truncate(strings.TrimSpace(in.Remark), 255)
	}
	s.applyMeta(g, in)

	if err := s.ValidateDeviceGroupConfig(ctx, g); err != nil {
		return nil, err
	}

	if err := s.app.DB.WithContext(ctx).Save(g).Error; err != nil {
		return nil, s.wrapWriteErr(err, "更新设备组失败")
	}
	// 组成员或健康检查阈值变化后清空运行期故障转移状态，
	// 让新成员立刻以 up 参与分发（否则会残留「已被摘除」的旧状态）。
	s.ResetFailoverTracker(g.ID)
	s.app.BumpConfigVersion(fmt.Sprintf("更新设备组 %s", g.Name))
	s.app.Log.Info("设备组已更新", zap.Uint64("id", id), zap.String("name", g.Name))
	_ = before
	return s.detail(ctx, g)
}

// applyMeta 把入参里的出口组专用字段写入模型。
//
// 入参中未提供的字段保持模型的当前值；balance 非法时**不**报错，
// 而是退回 least_conn——它是规格书规定的默认策略。
func (s *GroupService) applyMeta(g *model.DeviceGroup, in DeviceGroupInput) {
	if b := strings.TrimSpace(in.Balance); b != "" {
		if model.ValidBalance(b) {
			g.Balance = b
		} else {
			g.Balance = model.BalanceLeastConn
		}
	} else if g.Balance == "" {
		g.Balance = model.BalanceLeastConn
	}
	if in.HealthCheckEnable != nil {
		g.HealthCheckEnable = *in.HealthCheckEnable
	}
	if in.HealthCheckInterval > 0 {
		g.HealthCheckInterval = in.HealthCheckInterval
	}
	if in.HealthCheckTimeout > 0 {
		g.HealthCheckTimeout = in.HealthCheckTimeout
	}
	if in.HealthCheckFailCount > 0 {
		g.HealthCheckFailCount = in.HealthCheckFailCount
	}
	if in.HealthCheckSuccCount > 0 {
		g.HealthCheckSuccCount = in.HealthCheckSuccCount
	}
	if in.FailoverGroupID > 0 || in.FailoverGroupID == 0 {
		// 允许显式清空（传 0 = 取消故障转移组），因此这里无条件赋值。
		g.FailoverGroupID = in.FailoverGroupID
	}
	// 默认值补齐（首次创建时字段为零值）。
	if g.HealthCheckInterval <= 0 {
		g.HealthCheckInterval = 10
	}
	if g.HealthCheckTimeout <= 0 {
		g.HealthCheckTimeout = 3
	}
	if g.HealthCheckFailCount <= 0 {
		g.HealthCheckFailCount = 3
	}
	if g.HealthCheckSuccCount <= 0 {
		g.HealthCheckSuccCount = 2
	}
}

// Delete 删除设备组。
//
// 被规则引用时返回 40903（规格书 8.8 明确要求）。
// 同时禁止删除仍作为故障转移目标的组，以及仍被其它入口组引用的反向组。
//
// 参数 ctx 为上下文；id 为设备组 ID。
// 返回错误（成功时为 nil）。
func (s *GroupService) Delete(ctx context.Context, id uint64) error {
	g, err := s.mustGet(ctx, id)
	if err != nil {
		return err
	}

	if refs := s.collectReferences(ctx, id, g.Type); len(refs) > 0 {
		return response.New(response.CodeResourceInUse,
			fmt.Sprintf("设备组 %s 正在被引用，无法删除：%s", g.Name, strings.Join(refs, "；"))).
			WithDetails(response.DetailsField{
				Field: "id", Value: id,
				Hint: "请先解除引用（改规则、改故障转移组、改反向组）后再删除",
			})
	}

	if err := s.app.DB.WithContext(ctx).Delete(&model.DeviceGroup{}, id).Error; err != nil {
		return s.wrapWriteErr(err, "删除设备组失败")
	}
	s.ResetFailoverTracker(id)
	s.app.BumpConfigVersion(fmt.Sprintf("删除设备组 %s", g.Name))
	s.app.Log.Info("设备组已删除", zap.Uint64("id", id), zap.String("name", g.Name))
	return nil
}

// collectReferences 汇总阻止删除的引用原因。
//
// 参数 id 为设备组 ID；groupType 为该组的类型（inbound / outbound）。
// 返回人类可读的引用说明列表；为空表示可以安全删除。
func (s *GroupService) collectReferences(ctx context.Context, id uint64, groupType string) []string {
	db := s.app.DB.WithContext(ctx)
	reasons := []string{}

	var n int64
	if groupType == model.GroupTypeInbound {
		if err := db.Model(&model.ForwardRule{}).
			Where("inbound_group_id = ?", id).Count(&n).Error; err == nil && n > 0 {
			reasons = append(reasons, fmt.Sprintf("%d 条转发规则以本组为入口组", n))
		}
		// 反向组引用：入口组 config.reverse_group 含该出口组 ID。
		var entrances []model.DeviceGroup
		if err := db.Where("type = ?", model.GroupTypeInbound).Find(&entrances).Error; err == nil {
			cnt := 0
			for i := range entrances {
				if containsUint64(entrances[i].Config.AsUint64Slice(), id) {
					cnt++
				}
			}
			if cnt > 0 {
				reasons = append(reasons, fmt.Sprintf("%d 个入口组的反向组配置引用了本组", cnt))
			}
		}
	} else {
		var direct, chain, reverse int64
		_ = db.Model(&model.ForwardRule{}).
			Where("outbound_group_id = ?", id).Count(&direct).Error
		_ = db.Model(&model.ForwardRule{}).
			Where("chain_groups LIKE ?", fmt.Sprintf("%%%d%%", id)).Count(&chain).Error
		_ = db.Model(&model.ForwardRule{}).
			Where("reverse_group_id = ?", id).Count(&reverse).Error
		var failover int64
		_ = db.Model(&model.DeviceGroup{}).
			Where("failover_group_id = ?", id).Count(&failover).Error

		if direct > 0 {
			reasons = append(reasons, fmt.Sprintf("%d 条转发规则以本组为出口组", direct))
		}
		if chain > 0 {
			reasons = append(reasons, fmt.Sprintf("%d 条转发规则把本组用作链式出口", chain))
		}
		if reverse > 0 {
			reasons = append(reasons, fmt.Sprintf("%d 条转发规则把本组用作反向组", reverse))
		}
		if failover > 0 {
			reasons = append(reasons, fmt.Sprintf("%d 个设备组以本组为故障转移组", failover))
		}
	}
	return reasons
}

// Reorder 调整组内节点顺序（规格书 8.8 /reorder）。
//
// 出口组的顺序参与负载均衡初始化与并列打破，因此这不是纯展示层面的排序。
// 节点 ID 必须都是当前成员；传入的集合与当前成员不一致时返回 40001，
// 避免「只传了部分 ID」导致成员被意外截断。
//
// 参数 ctx 为上下文；groupID 为设备组 ID；nodeIDs 为新的顺序。
// 返回错误（成功时为 nil）。
func (s *GroupService) Reorder(ctx context.Context, groupID uint64, nodeIDs []uint64) error {
	g, err := s.mustGet(ctx, groupID)
	if err != nil {
		return err
	}
	newOrder := dedupeUint64(nodeIDs)
	current := g.NodeIDs.AsUint64Slice()
	if len(newOrder) != len(current) {
		return response.Field(response.CodeParamInvalid, "node_ids", nodeIDs,
			"节点集合与组成员不一致：重排序 MUST 提交组的全部节点")
	}
	if !sameUint64Set(newOrder, current) {
		return response.Field(response.CodeParamInvalid, "node_ids", nodeIDs,
			"提交的节点不是当前组成员")
	}

	g.NodeIDs = model.FromAny(newOrder)
	if err := s.app.DB.WithContext(ctx).
		Model(&model.DeviceGroup{}).Where("id = ?", groupID).
		Update("node_ids", g.NodeIDs).Error; err != nil {
		return s.wrapWriteErr(err, "调整组内节点顺序失败")
	}
	s.app.BumpConfigVersion(fmt.Sprintf("调整设备组 %s 的节点顺序", g.Name))
	s.app.Log.Info("设备组节点顺序已调整",
		zap.Uint64("group_id", groupID), zap.Any("node_ids", newOrder))
	return nil
}

// GroupHealth 是组内节点健康状态与负载分布（规格书 8.8 /health）。
type GroupHealth struct {
	GroupID   uint64 `json:"group_id"`
	GroupName string `json:"group_name"`
	Type      string `json:"type"`
	// Strategy 是当前负载均衡策略（仅出口组有意义）。
	Strategy string `json:"strategy"`
	// HealthyCount / TotalCount / OnlineCount 是概览计数。
	HealthyCount int `json:"healthy_count"`
	OnlineCount  int `json:"online_count"`
	TotalCount   int `json:"total_count"`
	// AllDown 标记本组所有出口均不可用（此时按规格书 6.3 转投故障转移组）。
	AllDown bool `json:"all_down"`
	// FailoverGroupID / FailoverGroupName 是转投目标。
	FailoverGroupID   uint64 `json:"failover_group_id"`
	FailoverGroupName string `json:"failover_group_name"`
	// Members 是逐节点的健康与负载明细。
	Members []MemberHealth `json:"members"`
	// Distribution 是负载分布（按 least_conn 的比较键给出理论占比）。
	Distribution []LoadDistribution `json:"distribution"`
}

// MemberHealth 是单个成员的实时健康状态。
type MemberHealth struct {
	NodeID uint64 `json:"node_id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
	// Healthy 为 false 表示被故障转移状态机摘除（连续失败达到阈值）。
	Healthy bool `json:"healthy"`
	// Reason 是最近一次失败原因的中文说明。
	Reason      string `json:"reason,omitempty"`
	Weight      int    `json:"weight"`
	MaxConn     int    `json:"max_conn"`
	CurrentConn int    `json:"current_conn"`
	// Excluded 说明该节点当前为何不参与分发（空 = 正常参与）。
	Excluded string `json:"excluded,omitempty"`
}

// Health 返回组内节点健康状态与负载分布（规格书 8.8 /health）。
//
// 参数 ctx 为上下文；groupID 为设备组 ID。
// 返回健康概览或 AppError。
func (s *GroupService) Health(ctx context.Context, groupID uint64) (*GroupHealth, error) {
	g, err := s.mustGet(ctx, groupID)
	if err != nil {
		return nil, err
	}
	ids := g.NodeIDs.AsUint64Slice()
	briefs := s.nodeBriefs(ctx, ids)

	restrict := s.GroupExitRestriction(ctx, g)
	cfg := s.failoverConfigOf(g)
	tracker := s.FailoverTracker(groupID)

	out := &GroupHealth{
		GroupID:         g.ID,
		GroupName:       g.Name,
		Type:            g.Type,
		Strategy:        g.Balance,
		TotalCount:      len(ids),
		Members:         []MemberHealth{},
		Distribution:    []LoadDistribution{},
		FailoverGroupID: g.FailoverGroupID,
	}

	cands := make([]BalanceCandidate, 0, len(briefs))
	for _, b := range briefs {
		st := tracker.State(b.ID)
		m := MemberHealth{
			NodeID: b.ID, Name: b.Name, Online: b.Online,
			Healthy: st.Up, Weight: normalWeight(b.Weight),
			MaxConn: b.MaxConn, CurrentConn: b.CurrentConn,
		}
		if !st.Up && st.LastReason != "" {
			m.Reason = FailureReasonText(st.LastReason)
		}
		m.Excluded = exclusionReason(b, restrict, cfg, st)
		if m.Excluded == "" {
			out.HealthyCount++
		}
		if b.Online {
			out.OnlineCount++
		}
		out.Members = append(out.Members, m)

		cands = append(cands, BalanceCandidate{
			NodeID: b.ID, Name: b.Name, Weight: normalWeight(b.Weight),
			MaxConn: b.MaxConn, CurrentConn: b.CurrentConn,
			Online: b.Online, Healthy: st.Up, Up: st.Up,
		})
	}

	out.Distribution = ComputeLoadDistribution(cands)
	// 全挂判定只看「在线且 up」的成员（规格书 6.3）。
	sel, needFailover := SelectExit(g, cands, BalanceFilter{
		Restrict:    restrict,
		HealthCheck: cfg.Enable,
	}, BalanceOptions{})
	out.AllDown = sel.Direct
	if needFailover {
		var fg model.DeviceGroup
		if err := s.app.DB.WithContext(ctx).First(&fg, g.FailoverGroupID).Error; err == nil {
			out.FailoverGroupName = fg.Name
		}
	}
	return out, nil
}

// exclusionReason 返回节点不参与分发的原因（空 = 参与分发）。
func exclusionReason(b DeviceGroupNodeBrief, r ExitRestriction, cfg FailoverConfig, st NodeFailoverState) string {
	switch {
	case !b.Online:
		return "节点离线"
	case !st.Up:
		if st.LastReason != "" {
			return "已被摘除：" + FailureReasonText(st.LastReason)
		}
		return "已被摘除（连续失败达到阈值）"
	case b.Weight <= 0:
		return "权重为 0，不参与分发"
	case cfg.Enable && !st.Up:
		return "未通过健康检查"
	case b.MaxConn > 0 && b.CurrentConn >= b.MaxConn:
		return "已达到本机最大连接数"
	case !r.AllowNode(b.ID):
		if len(r.AllowIDs) > 0 {
			return "被「限制出口」白名单排除"
		}
		return "被「限制出口」规则排除"
	}
	return ""
}

// failoverConfigOf 从设备组推导故障转移阈值。
//
// 说明：出口组表上有 health_check_* 字段；入口组把这些阈值放在 config
// （max_fail / fail_timout_sec，规格书 4.3）。两种来源在这里统一。
func (s *GroupService) failoverConfigOf(g *model.DeviceGroup) FailoverConfig {
	if g.IsInbound() {
		cfg, err := ParseInboundConfig(g.Config)
		if err != nil {
			return DefaultFailoverConfig()
		}
		return FailoverConfig{
			MaxFail:        cfg.MaxFail,
			FailTimeoutSec: cfg.FailTimoutSec,
			SuccCount:      g.HealthCheckSuccCount,
			// 入口组的健康检查在规格书 4.3 里没有独立开关，
			// 只有 disable_udp 之类的转发开关，因此恒为开启。
			Enable: true,
		}.Normalize()
	}
	return FailoverConfig{
		MaxFail:        g.HealthCheckFailCount,
		FailTimeoutSec: g.HealthCheckTimeout,
		SuccCount:      g.HealthCheckSuccCount,
		Enable:         g.HealthCheckEnable,
	}.Normalize()
}

// FailoverTracker 返回指定设备组的故障转移跟踪器（不存在时创建）。
//
// 这是健康检查任务（job 包）与规则选路的共同入口：所有读写同组状态的调用
// 都 MUST 走本方法，否则会出现「两套状态互相覆盖」。
//
// 参数 groupID 为设备组 ID。返回跟踪器指针（进程内共享，调用方自行加锁读写）。
func (s *GroupService) FailoverTracker(groupID uint64) *FailoverTracker {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()
	if t, ok := s.trackers[groupID]; ok {
		return t
	}
	t := NewFailoverTracker(DefaultFailoverConfig())
	s.trackers[groupID] = t
	return t
}

// LoadFailoverConfig 从数据库读取设备组的健康检查阈值并应用到跟踪器。
//
// 参数 ctx 为上下文；group 为设备组；不存在时返回 nil。
// 返回错误（仅数据库错误）。健康检查任务在每轮探测前调用本方法，
// 保证管理员改了阈值后能立刻生效。
func (s *GroupService) LoadFailoverConfig(ctx context.Context, group *model.DeviceGroup) error {
	if group == nil {
		return nil
	}
	t := s.FailoverTracker(group.ID)
	cfg := s.failoverConfigOf(group)
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()
	t.SetConfig(cfg)
	return nil
}

// RecordProbeResult 记录一次出口探测结果并落盘状态变化（健康检查任务调用）。
//
// 参数 groupID 为设备组 ID；nodeID 为被探测的出口节点；
// fail 为本次是否计为失败；reason 为失败原因。
// 返回（是否发生 up/down 状态变化，错误的 AppError）。
//
// 状态变化时会把结论同步到节点表（online 之外的「已被摘除」不落库，
// 因为它是运行期结论，重启后重新探测），并广播给前端。
func (s *GroupService) RecordProbeResult(ctx context.Context, groupID, nodeID uint64, fail bool, reason FailureReason) (bool, error) {
	if groupID == 0 || nodeID == 0 {
		return false, response.Field(response.CodeParamInvalid, "node_id", nodeID,
			"设备组与节点 ID 不能为空")
	}
	t := s.FailoverTracker(groupID)
	changed := t.Record(nodeID, fail, reason)
	if !changed {
		return false, nil
	}
	state := t.State(nodeID)
	s.app.Log.Info("出口故障转移状态变化",
		zap.Uint64("group_id", groupID), zap.Uint64("node_id", nodeID),
		zap.Bool("up", state.Up), zap.Int("consecutive_fail", state.ConsecutiveFail))
	s.app.Hub().Broadcast(Event{
		Type: "node_metrics",
		Data: map[string]interface{}{
			"group_id": groupID,
			"node_id":  nodeID,
			"up":       state.Up,
			"reason":   FailureReasonText(state.LastReason),
		},
	})
	return true, nil
}

// ResetFailoverTracker 清空指定设备组的故障转移状态（组成员变更或组被删除时调用）。
//
// 参数 groupID 为设备组 ID。
func (s *GroupService) ResetFailoverTracker(groupID uint64) {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()
	delete(s.trackers, groupID)
}

// GroupExitRestriction 读取设备组的「限制出口」。
//
// 参数 ctx 为上下文；group 为设备组。返回解析后的限制；组为 nil 时返回「不限制」。
func (s *GroupService) GroupExitRestriction(ctx context.Context, group *model.DeviceGroup) ExitRestriction {
	if group == nil {
		return ExitRestriction{}
	}
	return s.exitRestrictionOf(ctx, group)
}

// GroupBalanceFilter 构造该设备组的选路过滤条件。
//
// 参数 ctx 为上下文；group 为设备组。返回可直接传给 PickExit / SelectExit 的过滤器。
func (s *GroupService) GroupBalanceFilter(ctx context.Context, group *model.DeviceGroup) BalanceFilter {
	if group == nil {
		return BalanceFilter{}
	}
	return BalanceFilter{
		Restrict:    s.exitRestrictionOf(ctx, group),
		HealthCheck: s.failoverConfigOf(group).Enable,
	}
}

// GroupMembers 返回设备组成员的选路投影（含在线、权重、连接数、健康状态）。
//
// 参数 ctx 为上下文；group 为设备组。返回按组内顺序排列的候选列表。
func (s *GroupService) GroupMembers(ctx context.Context, group *model.DeviceGroup) []BalanceCandidate {
	if group == nil {
		return []BalanceCandidate{}
	}
	order := group.NodeIDs.AsUint64Slice()
	briefs := s.nodeBriefs(ctx, order)
	tracker := s.FailoverTracker(group.ID)

	cands := make([]BalanceCandidate, 0, len(briefs))
	for _, b := range briefs {
		st := tracker.State(b.ID)
		cands = append(cands, BalanceCandidate{
			NodeID: b.ID, Name: b.Name, Weight: normalWeight(b.Weight),
			MaxConn: b.MaxConn, CurrentConn: b.CurrentConn,
			Online: b.Online, Healthy: st.Up, Up: st.Up,
		})
	}
	return SortCandidatesByNodeOrder(cands, order)
}

// exitRestrictionOf 读取设备组的「限制出口」。
//
// 存储位置：组的 config JSON 里的 `exit_restrict` 字段（Nyanpass 的写法兼容）。
// 该字段不在规格书 4.3 / 4.4 的示例里，因此解析失败或缺失都视为「不限制」。
func (s *GroupService) exitRestrictionOf(ctx context.Context, g *model.DeviceGroup) ExitRestriction {
	var raw struct {
		ExitRestrict string `json:"exit_restrict"`
		RestrictExit string `json:"restrict_exit"`
	}
	if len(g.Config) > 0 {
		_ = jsonUnmarshal(string(g.Config), &raw)
	}
	if raw.ExitRestrict != "" {
		return ParseExitRestriction(raw.ExitRestrict)
	}
	if raw.RestrictExit != "" {
		return ParseExitRestriction(raw.RestrictExit)
	}
	return ExitRestriction{}
}

// ───────────────────────── 配置语义校验（规格书 8.8 /validate） ─────────────────────────

// ValidateResult 是校验结果（规格书 8.8 /validate 的响应 data）。
type ValidateResult struct {
	Valid bool `json:"valid"`
	// Errors 是阻断性错误：命中则不能保存。
	Errors []ValidateIssue `json:"errors"`
	// Warnings 是提示性风险：可以保存，但 UI MUST 展示。
	Warnings []ValidateIssue `json:"warnings"`
}

// ValidateIssue 是一条校验信息。
type ValidateIssue struct {
	// Field 是出问题的配置字段路径，如 "config.tls.chfp"。
	Field string `json:"field,omitempty"`
	// Code 是错误码（与规格书 8.4 一致），前端可据此做统一处理。
	Code int `json:"code,omitempty"`
	// Message 是中文说明，可直接展示。
	Message string `json:"message"`
	// Hint 是修复建议。
	Hint string `json:"hint,omitempty"`
}

// ValidateDeviceGroupConfig 对设备组做完整语义校验（规格书 8.8 /validate）。
//
// 校验项（MUST 全部实现）：
//   - 故障转移组 MUST 具备相同的入口权限配置 JSON（规格书 6.3）→ 42201
//   - 链式出口：不支持 UDP（42202）、不支持故障转移组（42203）
//   - SNI 白名单：子规则的 SNI 必须落在入口组 allowed_host 内（42204）
//   - 设备组节点角色必须与组类型匹配（42206）
//   - 反向组的出口节点必须是出口角色（42209）
//
// 参数 ctx 为上下文；g 为**待校验的设备组**（可以尚未落库，ID 为 0）。
// 返回首个阻断性错误；全部通过时返回 nil。
//
// 注意：本函数只做「阻断性错误」的裁决；仅给出提示的风险项由
// ValidateDeviceGroupConfigDetailed 返回（它同时给出 Warnings）。
func (s *GroupService) ValidateDeviceGroupConfig(ctx context.Context, g *model.DeviceGroup) error {
	res, err := s.ValidateDeviceGroupConfigDetailed(ctx, g)
	if err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		first := res.Errors[0]
		return response.New(first.Code, first.Message).WithDetails(response.DetailsField{
			Field: first.Field, Hint: first.Hint,
		})
	}
	return nil
}

// ValidateDeviceGroupConfigDetailed 是 ValidateDeviceGroupConfig 的完整版，
// 同时返回 Warnings（规格书 8.8 /validate 的响应体需要它）。
//
// 参数 ctx 为上下文；g 为待校验的设备组。
// 返回校验结果与「查询过程本身的错误」（数据库不可用时）。
func (s *GroupService) ValidateDeviceGroupConfigDetailed(ctx context.Context, g *model.DeviceGroup) (*ValidateResult, error) {
	res := &ValidateResult{Valid: true, Errors: []ValidateIssue{}, Warnings: []ValidateIssue{}}
	addErr := func(field string, code int, msg, hint string) {
		res.Valid = false
		res.Errors = append(res.Errors, ValidateIssue{Field: field, Code: code, Message: msg, Hint: hint})
	}
	addWarn := func(field string, code int, msg, hint string) {
		res.Warnings = append(res.Warnings, ValidateIssue{Field: field, Code: code, Message: msg, Hint: hint})
	}

	// ── 基础字段 ──
	if strings.TrimSpace(g.Name) == "" {
		addErr("name", response.CodeParamInvalid, "组名不能为空", "填写一个 1~64 字符的组名")
	}
	if g.Type != model.GroupTypeInbound && g.Type != model.GroupTypeOutbound {
		addErr("type", response.CodeParamInvalid, "组类型只能是 inbound 或 outbound", "")
		return res, nil
	}

	// 按类型解析配置。
	var (
		inCfg  InboundConfig
		outCfg OutboundConfig
	)
	if g.IsInbound() {
		cfg, err := ParseInboundConfig(g.Config)
		if err != nil {
			addErr("config", response.CodeParamInvalid,
				"入口组 config 不是合法 JSON："+err.Error(), "检查 JSON 语法")
			return res, nil
		}
		inCfg = cfg
		s.validateInboundFields(cfg, addErr, addWarn)
	} else {
		cfg, err := ParseOutboundConfig(g.Config)
		if err != nil {
			addErr("config", response.CodeParamInvalid,
				"出口组 config 不是合法 JSON："+err.Error(), "检查 JSON 语法")
			return res, nil
		}
		outCfg = cfg
		s.validateOutboundFields(cfg, addErr, addWarn)
		if !model.ValidBalance(g.Balance) {
			addErr("balance", response.CodeParamInvalid,
				fmt.Sprintf("负载均衡策略 %q 非法", g.Balance),
				"可选：least_conn / round_robin / hash_ip / weighted")
		}
		s.validateHealthCheck(g, addErr, addWarn)
	}

	// ── 节点角色匹配（规格书 8.8 / 6.3）──
	members, err := s.loadMembers(ctx, g.NodeIDs.AsUint64Slice())
	if err != nil {
		return nil, err
	}
	s.validateNodeRoles(g, members, addErr, addWarn)

	// ── 故障转移组兼容性（规格书 6.3）──
	if err := s.validateFailoverGroup(ctx, g, inCfg, members, addErr, addWarn); err != nil {
		return nil, err
	}

	// ── 链式出口约束（规格书 6.7）──
	if err := s.validateChainUsage(ctx, g, inCfg, addErr); err != nil {
		return nil, err
	}

	// ── 反向组约束（规格书 6.6）──
	if err := s.validateReverseGroup(ctx, g, inCfg, addErr); err != nil {
		return nil, err
	}

	// ── SNI 白名单（规格书 6.8）──
	if err := s.validateSubRuleSNI(ctx, g, inCfg, addErr, addWarn); err != nil {
		return nil, err
	}

	_ = outCfg // 出口组的字段级校验已在 validateOutboundFields 中完成。
	return res, nil
}

// validateInboundFields 校验入口组 config 的取值合法性。
func (s *GroupService) validateInboundFields(cfg InboundConfig,
	addErr func(string, int, string, string), addWarn func(string, int, string, string)) {

	if !ValidProtocol(cfg.Protocol) {
		addErr("config.protocol", response.CodeParamInvalid,
			fmt.Sprintf("协议 %q 非法", cfg.Protocol), "可选：ws / http / tls / direct")
	}
	if cfg.TLSInboundPolicy != TLSInboundNone &&
		cfg.TLSInboundPolicy != TLSInboundStrip &&
		cfg.TLSInboundPolicy != TLSInboundSNISplit {
		addErr("config.tls_inbound_policy", response.CodeParamInvalid,
			fmt.Sprintf("TLS 入站策略 %d 非法", cfg.TLSInboundPolicy),
			"可选：0 不处理 / 1 强制 TLS 剥离 / 2 SNI 分流")
	}
	// blocked_protocol 只能取规格书 4.3 列出的四种。
	for _, p := range cfg.BlockedProtocol {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "socks", "fet", "http", "tls":
		default:
			addErr("config.blocked_protocol", response.CodeParamInvalid,
				fmt.Sprintf("禁止的协议类型 %q 非法", p), "可选：socks / fet / http / tls")
		}
	}
	if len(cfg.BlockedHost) > 0 && len(cfg.AllowedHost) > 0 {
		// 两边都配时提醒优先级，避免用户以为白名单更大。
		addWarn("config.blocked_host", 0,
			"同时配置了白名单与黑名单，黑名单优先级更高",
			"命中黑名单的域名即使也在白名单内同样会被拒绝")
	}
	// 入口组的 TLS 是值类型，未填写时 CHFP 为空串，
	// 而 ValidChfp 本身就接受空值（表示使用 Go 默认指纹），因此无需判空。
	if !ValidChfp(cfg.TLS.CHFP) {
		addErr("config.tls.chfp", response.CodeParamInvalid,
			fmt.Sprintf("TLS 指纹 %q 非法", cfg.TLS.CHFP),
			fmt.Sprintf("可选：%s", strings.Join(chfpAllowed, " / ")))
	}
	// IPv6 优先：规格书 4.3 明确没有回退机制，MUST 给出风险提示。
	if ipv6GroupConfigured(cfg.IPv6Group) {
		addWarn("config.ipv6_group", 0,
			"已启用 IPv6 优先：connect 地址选择没有回退机制，若 IPv6 目标不可达不会自动回退 IPv4",
			"确认出口机的 IPv6 连通性后再保存")
	}
	if cfg.DisableUDP && cfg.UDPOverTCP {
		addWarn("config.udp_over_tcp", 0,
			"disable_udp 已开启，udp_over_tcp 不会生效", "两者取其一即可")
	}
	if cfg.TLSRejectEmptySNI && cfg.TLSInboundPolicy != TLSInboundSNISplit {
		addWarn("config.tls_reject_empty_sni", 0,
			"tls_reject_empty_sni 已开启但 TLS 入站策略不是 SNI 分流（2）",
			"SNI 分流场景下才需要拒绝空 SNI")
	}
}

// validateOutboundFields 校验出口组 config 的取值合法性。
func (s *GroupService) validateOutboundFields(cfg OutboundConfig,
	addErr func(string, int, string, string), addWarn func(string, int, string, string)) {

	switch strings.ToLower(strings.TrimSpace(cfg.ConnectType)) {
	case ConnectTypeDynIPv4, ConnectTypeDynIPv6, ConnectTypeStatic:
	default:
		addErr("config.connect_type", response.CodeParamInvalid,
			fmt.Sprintf("连接方式 %q 非法", cfg.ConnectType),
			"可选：dyn_ip4 / dyn_ip6 / static")
	}
	// 静态地址时 connect_address 必填且必须是合法 IP 或域名。
	if strings.EqualFold(strings.TrimSpace(cfg.ConnectType), ConnectTypeStatic) {
		if strings.TrimSpace(cfg.ConnectAddress) == "" {
			addErr("config.connect_address", response.CodeParamInvalid,
				"连接方式为 static 时连接地址必填", "填写域名或 IP")
		} else if !util.ValidIPOrHost(cfg.ConnectAddress) {
			addErr("config.connect_address", response.CodeParamInvalid,
				fmt.Sprintf("连接地址 %q 不是合法的域名或 IP", cfg.ConnectAddress),
				"支持域名（如 node.example.com）或 IP")
		}
		if !util.ValidPort(cfg.ConnectPort) {
			addErr("config.connect_port", response.CodeParamInvalid,
				fmt.Sprintf("连接端口 %d 非法", cfg.ConnectPort), "范围 1~65535")
		}
	}
	if !ValidProtocol(cfg.Protocol) {
		addErr("config.protocol", response.CodeParamInvalid,
			fmt.Sprintf("协议 %q 非法", cfg.Protocol), "可选：ws / http / tls / direct")
	}
	// TLS 是可选字段（指针），未填写时跳过指纹校验，避免空指针崩溃。
	// 用户在前端只选 ws 协议时不会提交 tls 对象，这是最常见的路径。
	if cfg.TLS != nil && !ValidChfp(cfg.TLS.CHFP) {
		addErr("config.tls.chfp", response.CodeParamInvalid,
			fmt.Sprintf("TLS 指纹 %q 非法", cfg.TLS.CHFP),
			fmt.Sprintf("可选：%s", strings.Join(chfpAllowed, " / ")))
	}
	// ws / http 协议下 host 与 path 建议填写，否则伪装特征明显。
	if (cfg.Protocol == ProtocolWS || cfg.Protocol == ProtocolHTTP) && cfg.WS.Host == "" {
		addWarn("config.ws.host", 0,
			"ws 伪装未设置 host，流量特征可能较容易被识别",
			"建议填写一个常见域名的 Host 头")
	}
}

// validateHealthCheck 校验出口组的健康检查参数。
func (s *GroupService) validateHealthCheck(g *model.DeviceGroup,
	addErr func(string, int, string, string), addWarn func(string, int, string, string)) {

	if g.HealthCheckInterval < 0 || g.HealthCheckTimeout < 0 {
		addErr("health_check_interval", response.CodeParamInvalid,
			"健康检查间隔与超时不能为负数", "")
	}
	if g.HealthCheckTimeout > 0 && g.HealthCheckInterval > 0 &&
		g.HealthCheckTimeout >= g.HealthCheckInterval {
		addWarn("health_check_timeout", 0,
			"超时时间不小于检查间隔，会造成探测任务堆积",
			"建议超时小于间隔（如间隔 10 秒、超时 3 秒）")
	}
	if g.HealthCheckFailCount < 0 || g.HealthCheckSuccCount < 0 {
		addErr("health_check_fail_count", response.CodeParamInvalid,
			"失败/恢复阈值不能为负数", "")
	}
}

// validateNodeRoles 校验组内节点角色与组类型匹配（42206）。
func (s *GroupService) validateNodeRoles(g *model.DeviceGroup, members []model.Node,
	addErr func(string, int, string, string), addWarn func(string, int, string, string)) {

	for _, n := range members {
		if g.IsInbound() && !n.IsInbound() {
			addErr("node_ids", response.CodeGroupRoleMismatch,
				fmt.Sprintf("节点 %s（%s）的角色是 %s，不能加入入口组",
					n.Name, n.PublicIPv4, n.Role),
				"入口组需要 inbound 或 both 角色的节点")
			continue
		}
		if g.IsOutbound() && !n.IsOutbound() {
			addErr("node_ids", response.CodeGroupRoleMismatch,
				fmt.Sprintf("节点 %s（%s）的角色是 %s，不能加入出口组",
					n.Name, n.PublicIPv4, n.Role),
				"出口组需要 outbound 或 both 角色的节点")
		}
	}
	if len(g.NodeIDs.AsUint64Slice()) > 0 && len(members) == 0 {
		addErr("node_ids", response.CodeGroupRoleMismatch,
			"组成员引用的节点都不存在", "节点可能已被删除，请重新选择组成员")
	}
	if g.IsOutbound() && g.Balance == model.BalanceHashIP && len(members) > 1 {
		addWarn("balance", 0,
			"hash_ip 策略下同一客户端会固定走同一出口，摘除节点会导致部分客户端重映射",
			"若希望流量更均匀，可改用 least_conn")
	}
}

// validateFailoverGroup 校验故障转移组兼容性（规格书 6.3，42201）。
//
// 规格书原文：故障转移组 MUST 具备相同的入口组权限配置 JSON，否则面板拒绝保存并提示。
//
// 实现口径：比较**入口权限相关字段**的归一化 JSON。
// 之所以不比较整份 config，是因为出口组的 config 里包含 connect_address 这类
// 「本来就该不同」的字段；权限配置指的是 4.3 里的 allowed_host / blocked_host /
// blocked_path / blocked_protocol 这几项（外加 reverse_group 会改变数据路径，也纳入比较）。
func (s *GroupService) validateFailoverGroup(ctx context.Context, g *model.DeviceGroup,
	inCfg InboundConfig, members []model.Node,
	addErr func(string, int, string, string), addWarn func(string, int, string, string)) error {

	if g.FailoverGroupID == 0 {
		return nil
	}
	if g.ID != 0 && g.FailoverGroupID == g.ID {
		addErr("failover_group_id", response.CodeFailoverGroupMismatch,
			"故障转移组不能指向自己", "选择另一个出口组")
		return nil
	}

	var fg model.DeviceGroup
	err := s.app.DB.WithContext(ctx).First(&fg, g.FailoverGroupID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		addErr("failover_group_id", response.CodeNotFound,
			fmt.Sprintf("故障转移组 %d 不存在", g.FailoverGroupID), "重新选择一个已存在的出口组")
		return nil
	}
	if err != nil {
		return response.Wrap(response.CodeInternal, err, "读取故障转移组失败")
	}
	if !fg.IsOutbound() {
		addErr("failover_group_id", response.CodeFailoverGroupMismatch,
			fmt.Sprintf("故障转移组 %s 不是出口组", fg.Name), "故障转移组必须是出口组")
		return nil
	}

	// 比较入口权限配置。
	mine := inboundPermissionFingerprint(inCfg)
	theirsRaw := s.permissionConfigOf(ctx, &fg, members)
	if mine != theirsRaw {
		addErr("failover_group_id", response.CodeFailoverGroupMismatch,
			fmt.Sprintf("故障转移组 %s 的入口权限配置与本组不一致", fg.Name),
			"故障转移组 MUST 具备相同的入口组权限配置（allowed_host / blocked_host / "+
				"blocked_path / blocked_protocol / reverse_group）")
		return nil
	}
	addWarn("failover_group_id", 0,
		"故障转移可能需要数秒才能生效（等待 TCP 超时）", "")
	return nil
}

// permissionConfigOf 取出口组对应的入口权限指纹。
//
// 出口组本身不持有入口权限配置，因此规则是：
//   - 若该出口组被某个入口组的 reverse_group 引用（反向隧道配对），
//     用那个入口组的权限配置；
//   - 否则用「该出口组 ID 对应入口组的配置」——出口组没有入口配置时，
//     用空指纹（即「全放行」）。
//
// 参数 members 为当前正在校验的组的成员，仅用于保持签名一致。
func (s *GroupService) permissionConfigOf(ctx context.Context, fg *model.DeviceGroup, members []model.Node) string {
	// 出口组自己没有权限配置，检查是否有一个入口组把它当作配对出口组：
	// 用 failover / reverse 关系推导一个稳定的指纹。
	var entrances []model.DeviceGroup
	if err := s.app.DB.WithContext(ctx).Where("type = ?", model.GroupTypeInbound).
		Find(&entrances).Error; err != nil {
		return ""
	}
	for i := range entrances {
		cfg, err := ParseInboundConfig(entrances[i].Config)
		if err != nil {
			continue
		}
		if containsUint64(cfg.ReverseGroup, fg.ID) {
			return inboundPermissionFingerprint(cfg)
		}
	}
	// 没有配对入口组：指纹为空权限配置。
	return inboundPermissionFingerprint(DefaultInboundConfig())
}

// inboundPermissionFingerprint 计算入口**权限**配置的归一化指纹。
//
// 归一化规则：域名列表排序 + 小写 + 去空白，保证「同样的权限、不同的书写顺序」
// 被判为一致（这正是规格书「相同的入口组权限配置」的本意）。
func inboundPermissionFingerprint(cfg InboundConfig) string {
	norm := func(list []string) []string {
		out := make([]string, 0, len(list))
		for _, v := range list {
			v = strings.ToLower(strings.TrimSpace(v))
			if v != "" {
				out = append(out, v)
			}
		}
		sort.Strings(out)
		return out
	}
	ids := make([]uint64, 0, len(cfg.ReverseGroup))
	ids = append(ids, cfg.ReverseGroup...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	return util.ToJSON(struct {
		AllowedHost     []string `json:"allowed_host"`
		BlockedHost     []string `json:"blocked_host"`
		BlockedPath     []string `json:"blocked_path"`
		BlockedProtocol []string `json:"blocked_protocol"`
		ReverseGroup    []uint64 `json:"reverse_group"`
	}{
		AllowedHost:     norm(cfg.AllowedHost),
		BlockedHost:     norm(cfg.BlockedHost),
		BlockedPath:     norm(cfg.BlockedPath),
		BlockedProtocol: norm(cfg.BlockedProtocol),
		ReverseGroup:    ids,
	})
}

// ValidateChainRule 校验一条**链式出口规则**的约束（规格书 6.7，42202 / 42203）。
//
// 规则层面的链式约束（MUST 由规则服务在保存规则时调用）：
//   - 链式出口不支持 UDP → 42202。判定依据是入口组的 config：
//     disable_udp 为 false 或 udp_over_tcp 为 true 时视为「启用了 UDP」；
//   - 链式出口不支持故障转移组 → 42203。链式链路里的任何一跳
//     （含最终的出口组）都不允许配置 FailoverGroupID。
//
// 参数 ctx 为上下文；rule 为待保存的规则（可以尚未落库）。
// 返回首个违规的 AppError，全部通过时返回 nil。
func (s *GroupService) ValidateChainRule(ctx context.Context, rule *model.ForwardRule) error {
	if rule == nil {
		return nil
	}
	chain := rule.ChainGroupList()
	if len(chain) == 0 {
		return nil
	}
	// 链式长度 MUST 为 2 或 3（规格书 6.7）。
	if len(chain) < 2 || len(chain) > 3 {
		return response.Field(response.CodeParamInvalid, "chain_groups", chain,
			"链式出口需要 2~3 跳（选择 2 或 3 个设备组）")
	}

	// 1. UDP 约束：检查入口组。
	var in model.DeviceGroup
	if err := s.app.DB.WithContext(ctx).First(&in, rule.InboundGroupID).Error; err == nil &&
		in.IsInbound() {
		cfg, cfgErr := ParseInboundConfig(in.Config)
		if cfgErr == nil && (!cfg.DisableUDP || cfg.UDPOverTCP) {
			return response.Field(response.CodeChainNoUDP, "config.disable_udp", cfg.DisableUDP,
				fmt.Sprintf("入口组 %s 启用了 UDP，链式转发不支持 UDP", in.Name))
		}
	}

	// 2. 故障转移组约束：检查链上的每一跳与最终出口组。
	checkIDs := append([]uint64{}, chain...)
	if rule.OutboundGroupID > 0 {
		checkIDs = append(checkIDs, rule.OutboundGroupID)
	}
	for _, gid := range checkIDs {
		var g model.DeviceGroup
		if err := s.app.DB.WithContext(ctx).First(&g, gid).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return response.Field(response.CodeNotFound, "chain_groups", gid,
					fmt.Sprintf("链式出口引用的设备组 %d 不存在", gid))
			}
			return response.Wrap(response.CodeInternal, err, "读取链式出口设备组失败")
		}
		if !g.IsOutbound() {
			// 中间跳与落地都必须是出口角色（规格书 6.7）。
			return response.Field(response.CodeGroupRoleMismatch, "chain_groups", gid,
				fmt.Sprintf("链式出口的每一跳都必须是出口组，%s 不是", g.Name))
		}
		if g.FailoverGroupID > 0 {
			return response.Field(response.CodeChainNoFailover, "failover_group_id",
				g.FailoverGroupID,
				fmt.Sprintf("链式模式下不支持故障转移组，设备组 %s 配有故障转移组", g.Name))
		}
	}
	return nil
}

// validateChainUsage 在设备组层面校验已有规则的链式约束。
//
// 与 ValidateChainRule 的区别：这里以「组内已有规则」为检查范围，
// 用于在编辑设备组（改 disable_udp 等）时提示会破坏哪些已有规则。
func (s *GroupService) validateChainUsage(ctx context.Context, g *model.DeviceGroup,
	inCfg InboundConfig, addErr func(string, int, string, string)) error {

	if !g.IsInbound() {
		return nil
	}
	var rules []model.ForwardRule
	if err := s.app.DB.WithContext(ctx).
		Where("inbound_group_id = ?", g.ID).Find(&rules).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "读取入口组规则失败")
	}

	for i := range rules {
		if len(rules[i].ChainGroupList()) == 0 {
			continue
		}
		// 链式出口不支持 UDP（42202）：入口组必须显式关闭 UDP。
		if !inCfg.DisableUDP || inCfg.UDPOverTCP {
			addErr("config.disable_udp", response.CodeChainNoUDP,
				fmt.Sprintf("规则 %s 使用链式出口，链式转发不支持 UDP，必须关闭 UDP",
					rules[i].Name),
				"链式场景下请开启 disable_udp，并在 UI 上禁用 UDP 相关开关")
		}
		// 链式出口不支持故障转移组（42203）。
		if err := s.ValidateChainRule(ctx, &rules[i]); err != nil {
			ae := response.AsAppError(err)
			addErr("chain_groups", ae.Code, ae.Msg, "")
		}
	}
	return nil
}

// validateReverseGroup 校验反向隧道约束（规格书 6.6，42209）。
//
// 规格书原文：面板 MUST 校验反向组的出口节点确实配置了 is-outbound: true。
func (s *GroupService) validateReverseGroup(ctx context.Context, g *model.DeviceGroup,
	inCfg InboundConfig, addErr func(string, int, string, string)) error {

	if !g.IsInbound() || len(inCfg.ReverseGroup) == 0 {
		return nil
	}
	for _, rgID := range inCfg.ReverseGroup {
		var rg model.DeviceGroup
		err := s.app.DB.WithContext(ctx).First(&rg, rgID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			addErr("config.reverse_group", response.CodeNotFound,
				fmt.Sprintf("反向组 %d 不存在", rgID), "重新选择反向隧道设备组")
			continue
		}
		if err != nil {
			return response.Wrap(response.CodeInternal, err, "读取反向组失败")
		}
		if !rg.IsOutbound() {
			addErr("config.reverse_group", response.CodeReverseGroupInvalid,
				fmt.Sprintf("反向组 %s 不是出口组", rg.Name), "反向组必须是出口组")
			continue
		}
		nodes, err := s.loadMembers(ctx, rg.NodeIDs.AsUint64Slice())
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if !n.IsOutbound() {
				addErr("config.reverse_group", response.CodeReverseGroupInvalid,
					fmt.Sprintf("反向组 %s 的节点 %s 角色是 %s，未配置为出口角色",
						rg.Name, n.Name, n.Role),
					"反向隧道要求出口节点 is-outbound: true（角色 outbound 或 both）")
			}
		}
	}
	return nil
}

// validateSubRuleSNI 校验 SNI 分流场景的 SNI 白名单（规格书 6.8，42204）。
//
// 规格书原文：子规则的 SNI MUST 落在 allowed_host 白名单内。
// 同时要求 SNI 分流模式下子规则必须使用固定端口（42207）。
func (s *GroupService) validateSubRuleSNI(ctx context.Context, g *model.DeviceGroup,
	inCfg InboundConfig, addErr func(string, int, string, string),
	addWarn func(string, int, string, string)) error {

	if !g.IsInbound() {
		return nil
	}
	var subs []model.ForwardRule
	if err := s.app.DB.WithContext(ctx).
		Where("inbound_group_id = ? AND is_sub_rule = ?", g.ID, true).
		Find(&subs).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "读取子规则失败")
	}
	for i := range subs {
		sub := subs[i]
		if strings.TrimSpace(sub.SNI) == "" {
			addErr("sni", response.CodeSNINotAllowed,
				fmt.Sprintf("子规则 %s 未填写 SNI", sub.Name),
				"SNI 分流模式下子规则必须指定 SNI")
			continue
		}
		if !sniAllowed(sub.SNI, inCfg.AllowedHost) {
			addErr("allowed_host", response.CodeSNINotAllowed,
				fmt.Sprintf("子规则 %s 的 SNI %s 不在入口组白名单内", sub.Name, sub.SNI),
				"请把该 SNI 加入入口组的 allowed_host，或修改子规则的 SNI")
		}
		// SNI 分流必须使用固定端口（否则无法在同一端口上按 SNI 分裂）。
		if sub.ListenPort <= 0 || sub.IsMultiPort() {
			addErr("listen_port", response.CodeFixPointRequired,
				fmt.Sprintf("子规则 %s 使用了随机端口或端口段", sub.Name),
				"SNI 分流场景必须使用固定端口（listen_port > 0 且无端口段）")
		}
	}
	if inCfg.TLSInboundPolicy == TLSInboundSNISplit && len(inCfg.AllowedHost) == 0 {
		addWarn("config.allowed_host", 0,
			"已启用 SNI 分流（tls_inbound_policy = 2）但白名单为空",
			"建议把允许的 SNI 后缀加入 allowed_host，并开启 tls_reject_empty_sni")
	}
	return nil
}

// sniAllowed 判断 SNI 是否命中入口组的 allowed_host 白名单。
//
// 空白名单 = 不限制（返回 true）；匹配使用规格书 4.3 的后缀语义
// （util.MatchAnyHost），因此 ".example.com" 能匹配 a.example.com。
func sniAllowed(sni string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	return util.MatchAnyHost(sni, allowed)
}

// loadMembers 加载组内的节点记录。
//
// 参数 ids 为节点 ID 列表；返回的节点顺序不保证与 ids 一致。
func (s *GroupService) loadMembers(ctx context.Context, ids []uint64) ([]model.Node, error) {
	if len(ids) == 0 {
		return []model.Node{}, nil
	}
	var nodes []model.Node
	if err := s.app.DB.WithContext(ctx).Where("id IN ?", ids).Find(&nodes).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "读取组成员节点失败")
	}
	if nodes == nil {
		nodes = []model.Node{}
	}
	return nodes, nil
}

// ───────────────────────── 规则分组 / 用户分组 / 节点分组 CRUD ─────────────────────────

// NameOnlyInput 是「只有名字 + 备注」的分组创建 / 更新入参。
type NameOnlyInput struct {
	Name   string `json:"name"`
	Remark string `json:"remark"`
	Sort   int    `json:"sort"`
}

// ListRuleGroups 返回规则分组列表（规格书 8.10）。
//
// 参数 ctx 为上下文。返回按 sort、id 升序排列的分组。
func (s *GroupService) ListRuleGroups(ctx context.Context) ([]model.RuleGroup, error) {
	var out []model.RuleGroup
	if err := s.app.DB.WithContext(ctx).
		Order("sort asc, id asc").Find(&out).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询规则分组失败")
	}
	if out == nil {
		out = []model.RuleGroup{}
	}
	return out, nil
}

// CreateRuleGroup 创建规则分组。
//
// 参数 ctx 为上下文；in 为入参。返回创建后的分组。
func (s *GroupService) CreateRuleGroup(ctx context.Context, in NameOnlyInput) (*model.RuleGroup, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, response.Field(response.CodeParamInvalid, "name", in.Name, "分组名不能为空")
	}
	if err := s.checkNameConflictIn(ctx, "rule_groups", name, 0); err != nil {
		return nil, err
	}
	row := &model.RuleGroup{
		Name:   util.Truncate(name, 64),
		Sort:   in.Sort,
		Remark: util.Truncate(strings.TrimSpace(in.Remark), 255),
	}
	if err := s.app.DB.WithContext(ctx).Create(row).Error; err != nil {
		return nil, s.wrapWriteErr(err, "创建规则分组失败")
	}
	// 规则分组的排序变更会影响规则列表的展示顺序，但不下发给节点，
	// 因此这里不 BumpConfigVersion（避免无意义的节点重拉配置）。
	return row, nil
}

// UpdateRuleGroup 更新规则分组。
//
// 参数 ctx 为上下文；id 为分组 ID；in 为入参。返回更新后的分组。
func (s *GroupService) UpdateRuleGroup(ctx context.Context, id uint64, in NameOnlyInput) (*model.RuleGroup, error) {
	var row model.RuleGroup
	if err := s.app.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "规则分组不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询规则分组失败")
	}
	if name := strings.TrimSpace(in.Name); name != "" && !util.EqualFoldName(name, row.Name) {
		if err := s.checkNameConflictIn(ctx, "rule_groups", name, id); err != nil {
			return nil, err
		}
		row.Name = util.Truncate(name, 64)
	}
	row.Sort = in.Sort
	if in.Remark != "" {
		row.Remark = util.Truncate(strings.TrimSpace(in.Remark), 255)
	}
	if err := s.app.DB.WithContext(ctx).Save(&row).Error; err != nil {
		return nil, s.wrapWriteErr(err, "更新规则分组失败")
	}
	return &row, nil
}

// DeleteRuleGroup 删除规则分组。
//
// 被规则引用时返回 40903。删除成功后把未分组的规则置为 rule_group_id = 0。
//
// 参数 ctx 为上下文；id 为分组 ID。返回错误（成功时为 nil）。
func (s *GroupService) DeleteRuleGroup(ctx context.Context, id uint64) error {
	var row model.RuleGroup
	if err := s.app.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return response.New(response.CodeNotFound, "规则分组不存在")
		}
		return response.Wrap(response.CodeInternal, err, "查询规则分组失败")
	}
	var n int64
	if err := s.app.DB.WithContext(ctx).Model(&model.ForwardRule{}).
		Where("rule_group_id = ?", id).Count(&n).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "统计规则分组引用失败")
	}
	if n > 0 {
		return response.New(response.CodeResourceInUse,
			fmt.Sprintf("规则分组 %s 下仍有 %d 条规则，无法删除", row.Name, n)).
			WithDetails(response.DetailsField{Field: "id", Value: id, Hint: "请先移动或删除这些规则"})
	}
	if err := s.app.DB.WithContext(ctx).Delete(&model.RuleGroup{}, id).Error; err != nil {
		return s.wrapWriteErr(err, "删除规则分组失败")
	}
	return nil
}

// ReorderRuleGroups 按提交的 ID 顺序重排规则分组的 sort 字段（规格书 8.10 /reorder）。
//
// 参数 ctx 为上下文；ids 为新的顺序。返回错误（成功时为 nil）。
func (s *GroupService) ReorderRuleGroups(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return response.Field(response.CodeParamInvalid, "ids", ids, "顺序列表不能为空")
	}
	err := s.app.DB.Tx(ctx, func(tx *gorm.DB) error {
		for i, id := range ids {
			if err := tx.Model(&model.RuleGroup{}).Where("id = ?", id).
				Update("sort", i).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return s.wrapWriteErr(err, "重排规则分组失败")
	}
	return nil
}

// ListUserGroups 返回用户分组列表。
//
// 参数 ctx 为上下文。返回按 id 升序排列的分组。
func (s *GroupService) ListUserGroups(ctx context.Context) ([]model.UserGroup, error) {
	var out []model.UserGroup
	if err := s.app.DB.WithContext(ctx).Order("id asc").Find(&out).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询用户分组失败")
	}
	if out == nil {
		out = []model.UserGroup{}
	}
	return out, nil
}

// UserGroupInput 是用户分组的创建 / 更新入参。
type UserGroupInput struct {
	Name         string   `json:"name"`
	TrafficLimit int64    `json:"traffic_limit"`
	SpeedLimit   int64    `json:"speed_limit"`
	IPLimit      int      `json:"ip_limit"`
	ConnLimit    int      `json:"conn_limit"`
	RuleGroupIDs []uint64 `json:"rule_group_ids"`
	Remark       string   `json:"remark"`
}

// CreateUserGroup 创建用户分组。
//
// 参数 ctx 为上下文；in 为入参。返回创建后的分组。
func (s *GroupService) CreateUserGroup(ctx context.Context, in UserGroupInput) (*model.UserGroup, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, response.Field(response.CodeParamInvalid, "name", in.Name, "分组名不能为空")
	}
	if err := s.checkNameConflictIn(ctx, "user_groups", name, 0); err != nil {
		return nil, err
	}
	row := &model.UserGroup{
		Name:         util.Truncate(name, 64),
		TrafficLimit: maxInt64(in.TrafficLimit, 0),
		SpeedLimit:   maxInt64(in.SpeedLimit, 0),
		IPLimit:      maxInt(in.IPLimit, 0),
		ConnLimit:    maxInt(in.ConnLimit, 0),
		RuleGroupIDs: model.FromAny(dedupeUint64(in.RuleGroupIDs)),
		Remark:       util.Truncate(strings.TrimSpace(in.Remark), 255),
	}
	if err := s.app.DB.WithContext(ctx).Create(row).Error; err != nil {
		return nil, s.wrapWriteErr(err, "创建用户分组失败")
	}
	return row, nil
}

// UpdateUserGroup 更新用户分组。
//
// 参数 ctx 为上下文；id 为分组 ID；in 为入参。返回更新后的分组。
func (s *GroupService) UpdateUserGroup(ctx context.Context, id uint64, in UserGroupInput) (*model.UserGroup, error) {
	var row model.UserGroup
	if err := s.app.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "用户分组不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询用户分组失败")
	}
	if name := strings.TrimSpace(in.Name); name != "" && !util.EqualFoldName(name, row.Name) {
		if err := s.checkNameConflictIn(ctx, "user_groups", name, id); err != nil {
			return nil, err
		}
		row.Name = util.Truncate(name, 64)
	}
	row.TrafficLimit = maxInt64(in.TrafficLimit, 0)
	row.SpeedLimit = maxInt64(in.SpeedLimit, 0)
	row.IPLimit = maxInt(in.IPLimit, 0)
	row.ConnLimit = maxInt(in.ConnLimit, 0)
	if in.RuleGroupIDs != nil {
		row.RuleGroupIDs = model.FromAny(dedupeUint64(in.RuleGroupIDs))
	}
	if in.Remark != "" {
		row.Remark = util.Truncate(strings.TrimSpace(in.Remark), 255)
	}
	if err := s.app.DB.WithContext(ctx).Save(&row).Error; err != nil {
		return nil, s.wrapWriteErr(err, "更新用户分组失败")
	}
	return &row, nil
}

// DeleteUserGroup 删除用户分组。
//
// 仍被用户引用时返回 40903（用户是「人被误删」的高风险资源，MUST 阻止）。
//
// 参数 ctx 为上下文；id 为分组 ID。返回错误（成功时为 nil）。
func (s *GroupService) DeleteUserGroup(ctx context.Context, id uint64) error {
	var row model.UserGroup
	if err := s.app.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return response.New(response.CodeNotFound, "用户分组不存在")
		}
		return response.Wrap(response.CodeInternal, err, "查询用户分组失败")
	}
	var n int64
	if err := s.app.DB.WithContext(ctx).Model(&model.User{}).
		Where("group_id = ?", id).Count(&n).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "统计用户分组引用失败")
	}
	if n > 0 {
		return response.New(response.CodeResourceInUse,
			fmt.Sprintf("用户分组 %s 下仍有 %d 个用户，无法删除", row.Name, n)).
			WithDetails(response.DetailsField{Field: "id", Value: id, Hint: "请先把这些用户移到其它分组"})
	}
	if err := s.app.DB.WithContext(ctx).Delete(&model.UserGroup{}, id).Error; err != nil {
		return s.wrapWriteErr(err, "删除用户分组失败")
	}
	return nil
}

// ListNodeGroups 返回节点分组列表（规格书 8.7）。
//
// 参数 ctx 为上下文。返回按 id 升序排列的分组。
func (s *GroupService) ListNodeGroups(ctx context.Context) ([]model.NodeGroup, error) {
	var out []model.NodeGroup
	if err := s.app.DB.WithContext(ctx).Order("id asc").Find(&out).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询节点分组失败")
	}
	if out == nil {
		out = []model.NodeGroup{}
	}
	return out, nil
}

// CreateNodeGroup 创建节点分组（规格书 8.7）。
//
// 参数 ctx 为上下文；in 为入参。返回创建后的分组。
func (s *GroupService) CreateNodeGroup(ctx context.Context, in NameOnlyInput) (*model.NodeGroup, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, response.Field(response.CodeParamInvalid, "name", in.Name, "分组名不能为空")
	}
	if err := s.checkNameConflictIn(ctx, "node_groups", name, 0); err != nil {
		return nil, err
	}
	row := &model.NodeGroup{
		Name:   util.Truncate(name, 64),
		Remark: util.Truncate(strings.TrimSpace(in.Remark), 255),
	}
	if err := s.app.DB.WithContext(ctx).Create(row).Error; err != nil {
		return nil, s.wrapWriteErr(err, "创建节点分组失败")
	}
	return row, nil
}

// UpdateNodeGroup 更新节点分组。
//
// 参数 ctx 为上下文；id 为分组 ID；in 为入参。返回更新后的分组。
func (s *GroupService) UpdateNodeGroup(ctx context.Context, id uint64, in NameOnlyInput) (*model.NodeGroup, error) {
	var row model.NodeGroup
	if err := s.app.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound, "节点分组不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询节点分组失败")
	}
	if name := strings.TrimSpace(in.Name); name != "" && !util.EqualFoldName(name, row.Name) {
		if err := s.checkNameConflictIn(ctx, "node_groups", name, id); err != nil {
			return nil, err
		}
		row.Name = util.Truncate(name, 64)
	}
	if in.Remark != "" {
		row.Remark = util.Truncate(strings.TrimSpace(in.Remark), 255)
	}
	if err := s.app.DB.WithContext(ctx).Save(&row).Error; err != nil {
		return nil, s.wrapWriteErr(err, "更新节点分组失败")
	}
	return &row, nil
}

// DeleteNodeGroup 删除节点分组（规格书 8.7）。
//
// 节点分组仅用于批量管理与筛选（规格书 6.2），不改变转发行为，
// 因此删除时只把节点上的引用清理掉，不阻止删除。
//
// 参数 ctx 为上下文；id 为分组 ID。返回错误（成功时为 nil）。
func (s *GroupService) DeleteNodeGroup(ctx context.Context, id uint64) error {
	var row model.NodeGroup
	if err := s.app.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return response.New(response.CodeNotFound, "节点分组不存在")
		}
		return response.Wrap(response.CodeInternal, err, "查询节点分组失败")
	}
	if err := s.app.DB.WithContext(ctx).Delete(&model.NodeGroup{}, id).Error; err != nil {
		return s.wrapWriteErr(err, "删除节点分组失败")
	}

	// 清理节点上的 group_ids 引用（尽力而为，失败不影响删除结果）。
	var nodes []model.Node
	if err := s.app.DB.WithContext(ctx).Find(&nodes).Error; err == nil {
		for i := range nodes {
			ids := nodes[i].GroupIDs.AsUint64Slice()
			if !containsUint64(ids, id) {
				continue
			}
			next := removeUint64(ids, id)
			_ = s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", nodes[i].ID).
				Update("group_ids", model.FromAny(next)).Error
		}
	}
	return nil
}

// ListNodeGroupNodes 返回节点分组内的节点（规格书 8.7 /:id/nodes）。
//
// 参数 ctx 为上下文；id 为节点分组 ID。返回节点列表。
func (s *GroupService) ListNodeGroupNodes(ctx context.Context, id uint64) ([]model.Node, error) {
	var nodes []model.Node
	if err := s.app.DB.WithContext(ctx).Find(&nodes).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询节点失败")
	}
	out := make([]model.Node, 0, len(nodes))
	for _, n := range nodes {
		if containsUint64(n.GroupIDs.AsUint64Slice(), id) {
			out = append(out, n)
		}
	}
	return out, nil
}

// ───────────────────────── 内部工具 ─────────────────────────

// mustGet 按 ID 读取设备组，不存在时返回 40401。
func (s *GroupService) mustGet(ctx context.Context, id uint64) (*model.DeviceGroup, error) {
	if id == 0 {
		return nil, response.New(response.CodeParamInvalid, "设备组 ID 不能为空")
	}
	var g model.DeviceGroup
	if err := s.app.DB.WithContext(ctx).First(&g, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.New(response.CodeNotFound,
				fmt.Sprintf("设备组 %d 不存在", id))
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询设备组失败")
	}
	return &g, nil
}

// checkNameConflict 检查设备组名称是否与已有组重复（EqualFold 比较，40901）。
//
// 参数 ctx 为上下文；name 为待检查名称；excludeID 为「更新时排除自身」的 ID。
func (s *GroupService) checkNameConflict(ctx context.Context, name string, excludeID uint64) error {
	q := s.app.DB.WithContext(ctx).Model(&model.DeviceGroup{})
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	var names []string
	if err := q.Pluck("name", &names).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "校验设备组名称失败")
	}
	// 规格书 4.1：MUST 在应用层做大小写归一化比较，不得依赖数据库排序规则。
	if _, conflict := util.FindNameConflict(names, name); conflict {
		return response.Field(response.CodeNameConflict, "name", name,
			fmt.Sprintf("设备组名 %s 已存在（忽略大小写）", name))
	}
	return nil
}

// checkNameConflictIn 在指定表上做名称查重（EqualFold 比较，40901）。
//
// 参数 ctx 为上下文；table 为表名（规则分组 / 用户分组 / 节点分组）；
// name 为待检查名称；excludeID 为更新时排除自身的 ID。
func (s *GroupService) checkNameConflictIn(ctx context.Context, table, name string, excludeID uint64) error {
	if !validGroupTable(table) {
		// 白名单校验：表名来自本文件内部的字面量，这里只是防御性检查。
		return response.New(response.CodeInternal, "内部错误：非法的分组表名")
	}
	q := s.app.DB.WithContext(ctx).Table(table)
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	var names []string
	if err := q.Pluck("name", &names).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "校验分组名称失败")
	}
	if _, conflict := util.FindNameConflict(names, name); conflict {
		return response.Field(response.CodeNameConflict, "name", name,
			fmt.Sprintf("名称 %s 已存在（忽略大小写）", name))
	}
	return nil
}

// validGroupTable 限制可查重的分组表名，避免表名拼接被注入。
func validGroupTable(t string) bool {
	switch t {
	case "rule_groups", "user_groups", "node_groups":
		return true
	}
	return false
}

// wrapWriteErr 把数据库错误分类为 50301（不可写）或 50001。
func (s *GroupService) wrapWriteErr(err error, msg string) *response.AppError {
	if err == nil {
		return nil
	}
	// database.WriteUnavailable 不能直接引用（本包只依赖 database 的类型），
	// 因此这里做同口径的关键字判断。
	low := strings.ToLower(err.Error())
	for _, kw := range []string{"readonly", "database is locked", "unable to open database file",
		"disk i/o", "no space left", "database or disk is full"} {
		if strings.Contains(low, kw) {
			return response.Wrap(response.CodeDBUnwritable, err, msg)
		}
	}
	return response.Wrap(response.CodeInternal, err, msg)
}

// dedupeUint64 去重并保持首次出现的顺序（顺序对出口组有意义，MUST 保留）。
func dedupeUint64(in []uint64) []uint64 {
	out := make([]uint64, 0, len(in))
	seen := make(map[uint64]struct{}, len(in))
	for _, v := range in {
		if v == 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// containsUint64 判断切片是否包含给定值。
func containsUint64(list []uint64, v uint64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// removeUint64 返回去掉指定值后的新切片。
func removeUint64(list []uint64, v uint64) []uint64 {
	out := make([]uint64, 0, len(list))
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// sameUint64Set 判断两个切片是否元素相同（忽略顺序，长度必须一致）。
func sameUint64Set(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[uint64]int, len(a))
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
		if seen[v] < 0 {
			return false
		}
	}
	return true
}

// ipv6GroupConfigured 判断 ipv6_group 是否真的启用了 IPv6 优先（含 0 = 全部）。
func ipv6GroupConfigured(group []uint64) bool {
	for _, id := range group {
		if id == 0 {
			return true
		}
	}
	// 只有非 0 的 ID 而没有 0 的写法在规格书里不是「启用 IPv6 优先」，
	// 但用户显然想表达某种 IPv6 意图，这里也提示风险。
	return len(group) > 0
}

// normalWeight 把缺失或非法的权重归一为 1（与 model.Target.NormalizedWeight 同口径）。
func normalWeight(w int) int {
	if w <= 0 {
		return 1
	}
	return w
}

// maxInt 返回较大的整数（下限保护）。
func maxInt(v, min int) int {
	if v < min {
		return min
	}
	return v
}

// maxInt64 返回较大的 int64（下限保护）。
func maxInt64(v, min int64) int64 {
	if v < min {
		return min
	}
	return v
}
