package agent

import (
	"net/http"
	"strings"
	"text/template"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
)

// 本文件负责生成节点侧的一键安装脚本与卸载脚本，
// 以及面板生成节点 config.yml 时使用的结构体（规格书 5.1 / 5.2 / 5.3）。

// 安装脚本的部署路径与命名约定（规格书 5.1 / 5.5）。
const (
	// NodeInstallDir 节点安装目录。
	NodeInstallDir = "/opt/openroute"
	// NodeBinaryName 节点客户端二进制文件名（与规格书 5.5 的运维手册一致）。
	NodeBinaryName = "rel_nodeclient"
	// NodeServiceNameDefault 默认 systemd 服务名前缀。
	NodeServiceNameDefault = "openroute"
	// NodeConfigFileName 节点配置文件。
	NodeConfigFileName = "config.yml"
	// NodeEnvFileName 环境变量文件（规格书 5.3）。
	NodeEnvFileName = "env.sh"
	// NodeMachineIDFileName 实例唯一标识文件（规格书 5.6 的多实例冲突排查）。
	NodeMachineIDFileName = "machine-id"
	// NodeLocalConfigFileName 节点本地持久化的最后一份可用配置。
	NodeLocalConfigFileName = "config.json"
	// NodeUninstallFileName 卸载脚本文件名（规格书 5.5）。
	NodeUninstallFileName = "openroute.uninstall.sh"
)

// 支持的发行版与架构（规格书 5.1）。
const (
	// 最低要求：Debian 11+ / Ubuntu 22.04+。
	nodeMinDebianVersion = 11
	nodeMinUbuntuMajor   = 22
)

// InstallOptions 是渲染安装脚本时的参数。
//
// 对应规格书 8.16 的 GET /api/node/install.sh：
// 面板按请求参数（-u / -t / -s / -o）把这些值模板化进脚本。
type InstallOptions struct {
	// BaseURL 面板地址，必填，形如 http://1.2.3.4:18888。
	BaseURL string
	// Token 节点密钥，必填。
	Token string
	// ServiceName 服务名，对应规格书 5.3 的 S= 环境变量，默认 openroute。
	ServiceName string
	// IsOutbound 是否出口节点，写入 config.yml 的 is-outbound。
	IsOutbound bool
	// Arch 目标架构：amd64 / amd64v3 / arm64；留空表示脚本自动探测。
	Arch string
	// Version 节点客户端版本号，用于拼下载地址；留空表示走 latest。
	Version string
}

// NodeConfig 是节点侧 config.yml 的结构（规格书 5.2）。
//
// 面板在 5.2 的「优先级规则」下只负责给出初始值：
// 命令行 -u / -t 会完全覆盖 base-url / token。
type NodeConfig struct {
	// 面板地址（必填）。
	BaseURL string `yaml:"base-url" json:"base-url"`
	// 节点密钥（必填）。
	Token string `yaml:"token" json:"token"`
	// 是否为出口节点。
	IsOutbound bool `yaml:"is-outbound" json:"is-outbound"`
	// 是否启用 ECH。
	UseECH       bool   `yaml:"use-ech" json:"use-ech"`
	ECHQueryName string `yaml:"ech-query-name" json:"ech-query-name"`

	// 本地监听端口（入口节点使用）。
	DirectPort int `yaml:"direct-port" json:"direct-port"` // 入口直出（无隧道）
	WsPort     int `yaml:"ws-port" json:"ws-port"`         // ws / http 隧道
	TlsPort    int `yaml:"tls-port" json:"tls-port"`       // tls 隧道
	UdpPort    int `yaml:"udp-port" json:"udp-port"`       // 原生 UDP
	RevPort    int `yaml:"rev-port" json:"rev-port"`       // 反向隧道

	// 主动连接地址（出口节点使用；覆盖面板下发的连接地址）。
	ConnectHost       string `yaml:"connect-host" json:"connect-host"`
	ConnectDirectPort int    `yaml:"connect-direct-port" json:"connect-direct-port"`
	ConnectWsPort     int    `yaml:"connect-ws-port" json:"connect-ws-port"`
	ConnectTlsPort    int    `yaml:"connect-tls-port" json:"connect-tls-port"`
	ConnectUdpPort    int    `yaml:"connect-udp-port" json:"connect-udp-port"`
	ConnectRevPort    int    `yaml:"connect-rev-port" json:"connect-rev-port"`

	// 负载均衡权重（仅出口节点，默认 1）。
	DefaultWeight int `yaml:"default-weight" json:"default-weight"`
}

// NodeEnv 是节点环境变量的结构，用于生成 env.sh（规格书 5.3）。
//
// 只列出安装阶段会写默认值的变量；其余（如 NYA_PROXY）由用户手工追加，
// 面板不主动写入，避免覆盖运维人员的现场配置。
type NodeEnv struct {
	// DisableExecute 为 1 时禁用 WebSSH 并阻止远程升级。
	DisableExecute bool
	// BindInbound 限定入口监听绑定的网卡/地址，多个用逗号分隔。
	BindInbound string
	// BindOutbound4 / BindOutbound6 限定出口出站源地址。
	BindOutbound4 string
	BindOutbound6 string
	// OutboundFwmark 出站流量打 fwmark，配合策略路由。
	OutboundFwmark string
	// CountInterface 指定探针统计流量的网卡；留空表示「智能统计」。
	CountInterface string
	// HealthCheck 为 false 时禁用主动健康检查。
	HealthCheck bool
	// UUID 多实例部署时的实例唯一值。
	UUID string
}

// NewNodeConfig 按节点信息构造一份节点侧 config.yml 结构（规格书 5.2）。
//
// 参数 baseURL 为面板地址；token 为节点密钥；n 为节点（可为 nil，表示只生成骨架）。
// 返回可直接序列化为 YAML 的配置。
func NewNodeConfig(baseURL, token string, n *model.Node) NodeConfig {
	cfg := NodeConfig{
		BaseURL:       normalizeBase(baseURL),
		Token:         token,
		UseECH:        false,
		ECHQueryName:  "",
		DefaultWeight: 1,
	}
	if n != nil {
		// 出口节点由角色推导；both 角色同时启用出口能力。
		cfg.IsOutbound = n.IsOutbound()
		cfg.DefaultWeight = n.Weight
		if cfg.DefaultWeight <= 0 {
			cfg.DefaultWeight = 1
		}
		// 端口沿用节点行上已有的配置，便于「重新生成 config.yml」时保留现场设置。
		cfg.DirectPort = n.DirectPort
		cfg.WsPort = n.WsPort
		cfg.TlsPort = n.TlsPort
		cfg.UdpPort = n.UdpPort
		cfg.RevPort = n.RevPort
		cfg.ConnectHost = n.ConnectHost
	}
	return cfg
}

// InstallScript 渲染安装脚本正文（规格书 5.1）。
//
// 参数 opt 为安装参数；返回可直接被 bash 执行的脚本文本。
// 脚本是自包含的：探测系统与架构、下载二进制、写配置与环境变量、
// 注册 systemd 服务并启动，任何一步失败都会打印可读原因并退出非零。
func InstallScript(opt InstallOptions) string {
	service := strings.TrimSpace(opt.ServiceName)
	if service == "" {
		service = NodeServiceNameDefault
	}
	arch := strings.TrimSpace(opt.Arch)
	if arch == "" {
		arch = "auto"
	}
	data := map[string]interface{}{
		"BaseURL":     normalizeBase(opt.BaseURL),
		"Token":       opt.Token,
		"Service":     service,
		"ServiceUnit": service + "-node",
		"InstallDir":  NodeInstallDir,
		"BinaryName":  NodeBinaryName,
		"ConfigFile":  NodeConfigFileName,
		"EnvFile":     NodeEnvFileName,
		"MachineID":   NodeMachineIDFileName,
		"LocalConfig": NodeLocalConfigFileName,
		"Uninstall":   NodeUninstallFileName,
		"IsOutbound":  boolToInt(opt.IsOutbound),
		"Arch":        arch,
		"Version":     strings.TrimSpace(opt.Version),
		"MinDebian":   nodeMinDebianVersion,
		"MinUbuntu":   nodeMinUbuntuMajor,
	}
	var b strings.Builder
	if err := installTemplate.Execute(&b, data); err != nil {
		// 模板是编译期常量，渲染失败只可能是数据缺失，这里回退到最小脚本。
		return "#!/usr/bin/env bash\necho \"安装脚本渲染失败，请联系面板管理员\" >&2\nexit 1\n"
	}
	return b.String()
}

// UninstallScript 渲染卸载脚本正文（规格书 5.5）。
//
// 参数 serviceName 为服务名前缀（与环境变量 S= 一致），留空取默认值。
// 返回脚本文本：停止并禁用服务、卸载 unit、可选择删除安装目录。
func UninstallScript(serviceName string) string {
	service := strings.TrimSpace(serviceName)
	if service == "" {
		service = NodeServiceNameDefault
	}
	data := map[string]interface{}{
		"Service":     service,
		"ServiceUnit": service + "-node",
		"InstallDir":  NodeInstallDir,
		"EnvFile":     NodeEnvFileName,
	}
	var b strings.Builder
	if err := uninstallTemplate.Execute(&b, data); err != nil {
		return "#!/usr/bin/env bash\necho \"卸载脚本渲染失败\" >&2\nexit 1\n"
	}
	return b.String()
}

// InstallScriptHandler 处理安装脚本下载（GET /api/node/install.sh，规格书 8.16）。
//
// 该接口不需要认证：脚本本身只包含面板地址与节点密钥两个参数，
// 密钥必须由管理员从面板复制，泄露风险与安装命令本身一致。
//
// 查询参数（全部可选，见下方说明）：
//
//	-u 面板地址（可选，缺省用请求本身推导出的地址）
//	-t 节点密钥（可选，缺省取该面板上唯一 / 最近的节点）
//	-s 服务名（可选，默认 openroute）
//	-o 是否出口节点（可选，1 / true）
//	-a 架构（可选，amd64 / amd64v3 / arm64）
//	-v 节点客户端版本（可选）
//
// 为什么参数是可选的：面板生成的安装命令形如
//
//	bash <(curl -fsSL https://面板/install.sh) -u https://面板 -t nsk_xxx
//
// 这里的 `-u` / `-t` 是传给 **bash** 的，curl 请求的是**不带任何查询串**的
// `/install.sh`。因此如果本接口强制要求查询参数，面板自己生成的命令永远跑不通
//（curl 拿到 400，`-f` 直接以错误退出）。
//
// 脚本正文里的面板地址与密钥在「生成命令时」就已经渲染进去了，
// 查询参数只用于覆盖，不作为必需项。
func (r *Registry) InstallScriptHandler(c *gin.Context) {
	base := firstNonEmpty(c.Query("u"), c.Query("url"), c.Query("base_url"))
	token := firstNonEmpty(c.Query("t"), c.Query("token"))

	// 面板地址缺省时，从本次请求推导：面板给谁下发脚本，
	// 谁就应该能访问到这个面板地址。
	if strings.TrimSpace(base) == "" {
		base = requestBaseURL(c)
	}

	// 密钥缺省时，退回「该面板上下发安装脚本最合适的那个节点」。
	//
	// 说明：这里不再因为缺参数而 400。裸请求 /install.sh 本身不包含任何鉴权信息，
	// 它只是取一份把地址与密钥都内嵌好的脚本——而这两个值本来就等同于
	// 「管理员从面板复制过去的安装命令」，泄露风险与之持平。
	// 若面板上确实没有任何节点，才明确报错，告诉用户先去创建节点。
	if strings.TrimSpace(token) == "" {
		node, err := r.defaultInstallNode(c.Request.Context())
		if err != nil {
			c.String(http.StatusBadRequest,
				"当前面板还没有任何节点，无法生成安装脚本。\n"+
					"请先在面板「节点管理」中点击「添加节点」，再复制生成的安装命令到目标机器执行。\n")
			return
		}
		r.writeInstallScript(c, base, node.Token)
		return
	}

	// 显式给了密钥：校验它确实存在，避免把无主的脚本发给攻击者做探测。
	node, err := r.nodeByToken(c.Request.Context(), strings.TrimSpace(token))
	if err != nil {
		ae := response.AsAppError(err)
		c.String(ae.HTTPStatus, "%s\n", ae.Msg)
		return
	}

	// 密钥有效时，若调用方没指定地址，优先用该节点注册时记录的连接地址，
	// 其次才是从请求推导——两者通常一致。
	if strings.TrimSpace(base) == "" {
		base = firstNonEmpty(node.ConnectHost, requestBaseURL(c))
	}

	r.writeInstallScript(c, base, token)
}

// writeInstallScript 渲染并写出安装脚本。
//
// 参数 base 为面板地址；token 为节点密钥。
// 侧效应：写出 HTTP 响应。
func (r *Registry) writeInstallScript(c *gin.Context, base, token string) {
	opt := InstallOptions{
		BaseURL:     strings.TrimSpace(base),
		Token:       strings.TrimSpace(token),
		ServiceName: firstNonEmpty(c.Query("s"), c.Query("service")),
		IsOutbound:  isTruthy(firstNonEmpty(c.Query("o"), c.Query("outbound"))),
		Arch:        firstNonEmpty(c.Query("a"), c.Query("arch")),
		Version:     firstNonEmpty(c.Query("v"), c.Query("version")),
	}

	// 返回纯文本，前端用 curl -fsSL 直接管道给 bash。
	c.Header("Content-Type", "text/x-shellscript; charset=utf-8")
	c.Header("Content-Disposition", `inline; filename="install.sh"`)
	c.String(http.StatusOK, "%s", InstallScript(opt))
}

// UninstallScriptHandler 处理卸载脚本下载（GET /api/node/uninstall.sh）。
//
// 与安装脚本一样无需认证：脚本内容不含任何密钥。
func (r *Registry) UninstallScriptHandler(c *gin.Context) {
	r.app.Log.Debug("下发节点卸载脚本", zap.String("service", c.Query("s")))
	c.Header("Content-Type", "text/x-shellscript; charset=utf-8")
	c.Header("Content-Disposition", `inline; filename="uninstall_node.sh"`)
	c.String(http.StatusOK, "%s", UninstallScript(firstNonEmpty(c.Query("s"), c.Query("service"))))
}

// requestBaseURL 从本次请求推导出面板的对外地址。
//
// 取请求的 Host 与协议：面板给谁下发脚本，谁就应该能从同一个地址访问它。
// 反向代理场景下以 X-Forwarded-Proto 为准（面板默认直接终结 TLS，
// 此时 c.Request.TLS 也不为空，两者结果一致）。
//
// 参数 c 为 Gin 上下文；返回形如 https://panel.example.com 的地址
// （不带末尾斜杠，与 InstallScript 的 normalizeBase 口径一致）。
func requestBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := strings.TrimSpace(c.Request.Host)
	if host == "" {
		// 极端情况下（例如经由某些代理转发）Host 为空，退回监听地址，
		// 避免渲染出一个 `http:///install.sh` 这种不可用的命令。
		host = "127.0.0.1"
	}
	return scheme + "://" + strings.TrimRight(host, "/")
}

// defaultInstallNode 挑选「下发安装脚本时默认使用的节点」。
//
// 用于裸请求 /install.sh（不带 -t）的场景：此时调用方没有指定节点，
// 选最近创建的一个——通常是管理员刚刚在面板上点「添加节点」建出来的那个。
//
// 参数 ctx 为上下文；返回节点与错误（面板上没有任何节点时返回错误）。
func (r *Registry) defaultInstallNode(ctx context.Context) (*model.Node, error) {
	var n model.Node
	if err := r.app.DB.WithContext(ctx).Order("id DESC").First(&n).Error; err != nil {
		return nil, err
	}
	return &n, nil
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// normalizeBase 去掉末尾斜杠并补齐协议头。
func normalizeBase(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimRight(u, "/")
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	return u
}

// boolToInt 把布尔值转成 0 / 1，便于嵌入 shell 脚本。
func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// isTruthy 判断查询参数是否表示真值。
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

// firstNonEmpty 返回首个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// installTemplate 是安装脚本模板（规格书 5.1 + 5.3）。
//
// 保持「先探测、再下载、后注册」的顺序：任何一步失败都不会留下
// 半装状态（下载到临时文件，校验通过才 mv 到目标位置）。
var installTemplate = template.Must(template.New("install").Parse(`#!/usr/bin/env bash
#
# OpenRoute 节点一键安装脚本（由面板按 token 模板化生成）
#
# 用法：
#   bash <(curl -fsSL {{.BaseURL}}/install.sh) -u {{.BaseURL}} -t <token>
#
# 支持的环境变量：
#   S=openroute        服务名（默认 openroute），多实例部署时用于区分
#   OPTIMIZE=1         启用内核网络参数优化（BBR、缓冲区、文件句柄）
#   INSTALL_TOOLS=1    安装常用排查工具（iftop / mtr / tcpdump 等）
#   DISABLE_EXECUTE=1  禁用 WebSSH 与远程升级
#   BIND_INBOUND=...   限定入口监听绑定的网卡/地址
#   COUNT_INTERFACE=.. 指定探针统计流量的网卡
#   UUID=...           多实例部署时的实例唯一值
#
set -euo pipefail

# ── 参数 ────────────────────────────────────────────────────────────────
PANEL_URL="{{.BaseURL}}"
NODE_TOKEN="{{.Token}}"
SERVICE_NAME="${S:-{{.Service}}}"
ARCH_HINT="{{.Arch}}"
NODE_VERSION="{{.Version}}"
IS_OUTBOUND="{{.IsOutbound}}"

INSTALL_DIR="{{.InstallDir}}"
BINARY_PATH="${INSTALL_DIR}/{{.BinaryName}}"
CONFIG_PATH="${INSTALL_DIR}/{{.ConfigFile}}"
ENV_PATH="${INSTALL_DIR}/{{.EnvFile}}"
LOCAL_CONFIG_PATH="${INSTALL_DIR}/{{.LocalConfig}}"
MACHINE_ID_PATH="${INSTALL_DIR}/{{.MachineID}}"
SERVICE_UNIT="{{.ServiceUnit}}"
SERVICE_FILE="/etc/systemd/system/${SERVICE_UNIT}.service"
UNINSTALL_PATH="${INSTALL_DIR}/{{.Uninstall}}"

# ── 输出工具 ────────────────────────────────────────────────────────────
RED='\033[31m'; GREEN='\033[32m'; YELLOW='\033[33m'; BLUE='\033[36m'; RESET='\033[0m'

info()  { printf "${GREEN}[OpenRoute]${RESET} %s\n" "$*"; }
warn()  { printf "${YELLOW}[OpenRoute]${RESET} %s\n" "$*"; }
step()  { printf "${BLUE}[OpenRoute]${RESET} %s\n" "$*"; }
fail()  { printf "${RED}[OpenRoute] 安装失败：%s${RESET}\n" "$*" >&2; exit 1; }

# ── 1. 前置检查：root / 系统版本 / 架构 ─────────────────────────────────
if [ "$(id -u)" != "0" ]; then
  fail "请以 root 身份运行（当前用户不是 root）"
fi

if [ ! -f /etc/os-release ]; then
  fail "无法识别系统发行版（缺少 /etc/os-release），仅支持 Debian {{.MinDebian}}+ / Ubuntu {{.MinUbuntu}}.04+"
fi
. /etc/os-release
OS_ID="${ID:-unknown}"

case "${OS_ID}" in
  debian)
    if [ "${VERSION_ID%%.*}" -lt {{.MinDebian}} ] 2>/dev/null; then
      fail "Debian ${VERSION_ID} 版本过低，请使用 Debian {{.MinDebian}} 或更高版本"
    fi
    ;;
  ubuntu)
    if [ "${VERSION_ID%%.*}" -lt {{.MinUbuntu}} ] 2>/dev/null; then
      fail "Ubuntu ${VERSION_ID} 版本过低，请使用 Ubuntu {{.MinUbuntu}}.04 或更高版本"
    fi
    ;;
  *)
    warn "当前系统为 ${OS_ID}，未在官方支持列表内（Debian {{.MinDebian}}+ / Ubuntu {{.MinUbuntu}}.04+），继续安装但可能不可用"
    ;;
esac

# 架构探测：x86_64 默认使用 amd64，若 CPU 支持 x86-64-v3 指令集则升级为 amd64v3。
detect_arch() {
  local machine
  machine="$(uname -m)"
  case "${machine}" in
    x86_64|amd64)
      if grep -qE '(^| )(avx2|bmi2)( |$)' /proc/cpuinfo 2>/dev/null; then
        echo "amd64v3"
      else
        echo "amd64"
      fi
      ;;
    aarch64|arm64)
      echo "arm64"
      ;;
    *)
      fail "不支持的架构 ${machine}，仅支持 amd64 / amd64v3 / arm64"
      ;;
  esac
}

if [ "${ARCH_HINT}" = "amd64" ] || [ "${ARCH_HINT}" = "amd64v3" ] || [ "${ARCH_HINT}" = "arm64" ]; then
  TARGET_ARCH="${ARCH_HINT}"
else
  TARGET_ARCH="$(detect_arch)"
fi
info "系统 ${PRETTY_NAME:-${OS_ID}}，架构 ${TARGET_ARCH}"

# ── 2. 依赖检查 ────────────────────────────────────────────────────────
# 只要 curl / systemctl 存在即可运行；不依赖 APT 也能工作（二进制是静态编译）。
command -v curl >/dev/null 2>&1 || fail "缺少 curl，请先执行：apt update && apt install -y curl"
if ! command -v systemctl >/dev/null 2>&1; then
  fail "未检测到 systemd（systemctl 不存在），本脚本仅支持 systemd 托管的系统"
fi

if [ "${INSTALL_TOOLS:-0}" = "1" ]; then
  step "安装常用排查工具 ..."
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq || warn "apt update 失败，跳过工具安装"
  apt-get install -y -qq iftop mtr-tiny tcpdump net-tools dnsutils curl || warn "部分工具安装失败，不影响节点运行"
fi

# ── 3. 下载二进制 ──────────────────────────────────────────────────────
step "准备安装目录 ${INSTALL_DIR}"
mkdir -p "${INSTALL_DIR}"

if [ -n "${NODE_VERSION}" ]; then
  BINARY_URL="${PANEL_URL}/api/node/binary/${TARGET_ARCH}?version=${NODE_VERSION}"
else
  BINARY_URL="${PANEL_URL}/api/node/binary/${TARGET_ARCH}"
fi

TMP_BINARY="$(mktemp "${INSTALL_DIR}/.rel_nodeclient.XXXXXX")"
trap 'rm -f "${TMP_BINARY}"' EXIT

info "从面板下载节点客户端：${BINARY_URL}"
if ! curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 15 -o "${TMP_BINARY}" "${BINARY_URL}"; then
  rm -f "${TMP_BINARY}"
  fail "下载节点客户端失败。
  排查建议：
    1. 确认面板地址可达：curl -fsSL ${PANEL_URL}
    2. 确认 token 有效：面板「节点管理」中该节点仍存在
    3. 若面板开启了 HTTPS，请把 -u 参数改为 https:// 开头
    4. 网络受限时可用离线部署：本地下载二进制后 scp 到 ${BINARY_PATH}"
fi
chmod 0755 "${TMP_BINARY}"

# 简单有效性校验：能打印帮助即视为可执行文件。
if ! "${TMP_BINARY}" -h >/dev/null 2>&1; then
  warn "二进制自检未通过（可能是架构不匹配），仍继续安装"
fi
mv -f "${TMP_BINARY}" "${BINARY_PATH}"
trap - EXIT
info "节点客户端已安装到 ${BINARY_PATH}"

# ── 4. 写入 config.yml ─────────────────────────────────────────────────
step "生成配置文件 ${CONFIG_PATH}"

# 实例唯一标识：多实例部署时用 UUID 覆盖，否则由面板下发的 token 派生。
if [ -n "${UUID:-}" ]; then
  printf '%s' "${UUID}" > "${MACHINE_ID_PATH}"
elif [ ! -s "${MACHINE_ID_PATH}" ]; then
  if [ -r /proc/sys/kernel/random/uuid ]; then
    cat /proc/sys/kernel/random/uuid > "${MACHINE_ID_PATH}"
  else
    date +%s%N > "${MACHINE_ID_PATH}"
  fi
fi
MACHINE_UUID="$(cat "${MACHINE_ID_PATH}" 2>/dev/null || echo "")"

cat > "${CONFIG_PATH}" <<YAML
# OpenRoute 节点客户端配置（由安装脚本生成，可手工修改后重启服务生效）
# 面板地址（必填）
base-url: "${PANEL_URL}"
# 节点密钥（必填）
token: "${NODE_TOKEN}"
# 是否为出口节点
is-outbound: $( [ "${IS_OUTBOUND}" = "1" ] && echo true || echo false )
# 是否启用 ECH
use-ech: false
ech-query-name: ""

# 本地监听端口（入口节点使用；0 = 由面板下发时指定）
direct-port: 0
ws-port: 0
tls-port: 0
udp-port: 0
rev-port: 0

# 主动连接地址（出口节点使用；留空表示使用面板下发的连接地址）
connect-host: ""
connect-direct-port: 0
connect-ws-port: 0
connect-tls-port: 0
connect-udp-port: 0
connect-rev-port: 0

# 负载均衡权重（仅出口节点，默认 1）
default-weight: 1

# 实例标识（多实例部署时由 UUID 环境变量指定）
machine-id: "${MACHINE_UUID}"
YAML
chmod 0600 "${CONFIG_PATH}"

# ── 5. 写入环境变量 env.sh（规格书 5.3）───────────────────────────────
step "生成环境变量文件 ${ENV_PATH}"
cat > "${ENV_PATH}" <<SH
# OpenRoute 节点环境变量（由安装脚本生成）
# 修改后执行：systemctl restart ${SERVICE_UNIT}

# 禁用 WebSSH 与远程升级（1 = 禁用）
# DISABLE_EXECUTE=1

# 限定入口监听绑定的网卡/地址，多个用逗号分隔
$([ -n "${BIND_INBOUND:-}" ] && echo "BIND_INBOUND=${BIND_INBOUND}" || echo "# BIND_INBOUND=0.0.0.0")

# 限制出口出站源地址（不推荐使用）
# BIND_OUTBOUND_4=
# BIND_OUTBOUND_6=

# 出站流量打 fwmark，配合策略路由
# OUTBOUND_FWMARK=

# 探针统计流量的网卡；不设置时使用「智能统计」
$([ -n "${COUNT_INTERFACE:-}" ] && echo "COUNT_INTERFACE=${COUNT_INTERFACE}" || echo "# COUNT_INTERFACE=eth0")

# 主动健康检查（0 = 禁用，降级为被动判定）
# HEALTH_CHECK=0

# 多实例部署时的实例唯一值
$([ -n "${UUID:-}" ] && echo "UUID=${UUID}" || echo "# UUID=")

# 服务名（多实例部署时用于区分 unit）
# S=${SERVICE_NAME}
SH
chmod 0600 "${ENV_PATH}"

# ── 6. 生成卸载脚本 ────────────────────────────────────────────────────
cat > "${UNINSTALL_PATH}" <<'UNINSTALL_SCRIPT'
#!/usr/bin/env bash
#
# OpenRoute 节点卸载脚本（安装时内嵌生成）
#
# 用法：bash /opt/openroute/openroute.uninstall.sh
#
set -uo pipefail

if [ "$(id -u)" != "0" ]; then
  echo "请以 root 身份运行卸载脚本" >&2
  exit 1
fi

SERVICE_UNIT="${SERVICE_UNIT:-{{.ServiceUnit}}}"
INSTALL_DIR="{{.InstallDir}}"
SERVICE_FILE="/etc/systemd/system/${SERVICE_UNIT}.service"

echo "[OpenRoute] 停止并禁用服务 ${SERVICE_UNIT} ..."
systemctl stop    "${SERVICE_UNIT}" >/dev/null 2>&1 || true
systemctl disable "${SERVICE_UNIT}" >/dev/null 2>&1 || true

if [ -f "${SERVICE_FILE}" ]; then
  rm -f "${SERVICE_FILE}"
  systemctl daemon-reload
  systemctl reset-failed "${SERVICE_UNIT}" >/dev/null 2>&1 || true
fi

read -r -p "是否删除安装目录 ${INSTALL_DIR}（含配置与日志）？[y/N] " answer
case "${answer}" in
  y|Y|yes|YES)
    echo "[OpenRoute] 删除 ${INSTALL_DIR} ..."
    rm -rf "${INSTALL_DIR}"
    ;;
  *)
    echo "[OpenRoute] 已保留 ${INSTALL_DIR}，可手动删除。"
    ;;
esac

echo "[OpenRoute] 卸载完成。"
exit 0
UNINSTALL_SCRIPT
chmod 0755 "${UNINSTALL_PATH}"
info "卸载脚本：bash ${UNINSTALL_PATH}"

# ── 7. 注册并启用 systemd 服务 ─────────────────────────────────────────
step "注册 systemd 服务 ${SERVICE_UNIT}"

NOFILE_LIMIT=1048576
cat > "${SERVICE_FILE}" <<UNIT
[Unit]
Description=OpenRoute Node Client (${SERVICE_NAME})
Documentation=${PANEL_URL}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-${ENV_PATH}
WorkingDirectory=${INSTALL_DIR}
ExecStart=${BINARY_PATH} -u ${PANEL_URL} -t ${NODE_TOKEN}
Restart=always
RestartSec=3
LimitNOFILE=${NOFILE_LIMIT}
# 网络转发所需的能力；不使用完整 root 之外的特权
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
# 转发场景需要放开内核网络参数
ProtectSystem=false
PrivateTmp=false

[Install]
WantedBy=multi-user.target
UNIT

# 启动前做一次配置语法自检，避免服务反复重启。
"${BINARY_PATH}" -c "${CONFIG_PATH}" -check >/dev/null 2>&1 || warn "配置文件自检未通过，请检查 ${CONFIG_PATH}"

systemctl daemon-reload
systemctl enable "${SERVICE_UNIT}" >/dev/null 2>&1 || warn "enable 失败，请检查 systemd"
systemctl restart "${SERVICE_UNIT}"

sleep 2
if systemctl is-active --quiet "${SERVICE_UNIT}"; then
  info "服务已启动：${SERVICE_UNIT}"
else
  warn "服务未能正常启动，请执行：journalctl -fu ${SERVICE_UNIT}"
fi

# ── 8. 可选：内核网络参数优化 ──────────────────────────────────────────
if [ "${OPTIMIZE:-0}" = "1" ]; then
  step "启用内核网络参数优化 ..."
  cat > /etc/sysctl.d/99-openroute.conf <<SYSCTL
# OpenRoute 节点网络优化
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
net.core.rmem_max = 33554432
net.core.wmem_max = 33554432
net.ipv4.tcp_rmem = 4096 87380 33554432
net.ipv4.tcp_wmem = 4096 65536 33554432
net.ipv4.tcp_fastopen = 3
net.ipv4.tcp_slow_start_after_idle = 0
net.ipv4.ip_local_port_range = 10240 65000
net.ipv4.tcp_tw_reuse = 1
net.ipv4.tcp_fin_timeout = 15
net.ipv4.tcp_max_syn_backlog = 8192
net.core.somaxconn = 8192
fs.file-max = 1048576
SYSCTL
  if sysctl -p /etc/sysctl.d/99-openroute.conf >/dev/null 2>&1; then
    info "内核参数已优化（BBR / 缓冲区 / 文件句柄）"
  else
    warn "部分内核参数写入失败（内核不支持 BBR 时可忽略）"
  fi
fi

# ── 9. 完成 ────────────────────────────────────────────────────────────
cat <<DONE

============================================================
 OpenRoute 节点安装完成
------------------------------------------------------------
 面板地址   : ${PANEL_URL}
 安装目录   : ${INSTALL_DIR}
 配置文件   : ${CONFIG_PATH}
 环境变量   : ${ENV_PATH}
 服务名     : ${SERVICE_UNIT}
 实例标识   : ${MACHINE_UUID}

 常用命令：
   查看状态   systemctl status ${SERVICE_UNIT}
   重启服务   systemctl restart ${SERVICE_UNIT}
   实时日志   journalctl -fu ${SERVICE_UNIT}
   查看版本   ${BINARY_PATH} -h
   卸载       bash ${UNINSTALL_PATH}

 提示：面板「节点管理」页面刷新后，本机应显示为「在线」。
============================================================
DONE

exit 0
`))

// uninstallTemplate 是独立下发的卸载脚本模板（规格书 5.5）。
var uninstallTemplate = template.Must(template.New("uninstall").Parse(`#!/usr/bin/env bash
#
# OpenRoute 节点卸载脚本
#
# 用法：bash /opt/openroute/openroute.uninstall.sh
#
set -uo pipefail

SERVICE_UNIT="{{.ServiceUnit}}"
INSTALL_DIR="{{.InstallDir}}"
SERVICE_FILE="/etc/systemd/system/${SERVICE_UNIT}.service"

if [ "$(id -u)" != "0" ]; then
  echo "请以 root 身份运行卸载脚本" >&2
  exit 1
fi

echo "[OpenRoute] 停止并禁用服务 ${SERVICE_UNIT} ..."
systemctl stop    "${SERVICE_UNIT}" >/dev/null 2>&1 || true
systemctl disable "${SERVICE_UNIT}" >/dev/null 2>&1 || true

if [ -f "${SERVICE_FILE}" ]; then
  rm -f "${SERVICE_FILE}"
  systemctl daemon-reload
  systemctl reset-failed "${SERVICE_UNIT}" >/dev/null 2>&1 || true
fi

read -r -p "是否删除安装目录 ${INSTALL_DIR}（含配置与日志）？[y/N] " answer
case "${answer}" in
  y|Y|yes|YES)
    echo "[OpenRoute] 删除 ${INSTALL_DIR} ..."
    rm -rf "${INSTALL_DIR}"
    ;;
  *)
    echo "[OpenRoute] 已保留 ${INSTALL_DIR}，可手动删除。"
    ;;
esac

echo "[OpenRoute] 卸载完成。"
exit 0
`))
