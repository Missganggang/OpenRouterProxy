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
	NodeInstallDir = "/opt/openroute-node"
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
		"Service": service,
	}
	var b strings.Builder
	if err := uninstallTemplate.Execute(&b, data); err != nil {
		return "#!/usr/bin/env bash\necho \"卸载脚本渲染失败\" >&2\nexit 1\n"
	}
	return b.String()
}

// InstallScriptHandler 处理安装脚本下载（GET /api/node/install.sh，规格书 8.16）。
//
// 该接口不需要认证。匿名请求只生成通用脚本，节点密钥由管理员
// 通过 bash 的 -t 参数传入，绝不从数据库挑选或公开节点密钥。
//
// 查询参数（全部可选，见下方说明）：
//
//	-u 面板地址（可选，缺省用请求本身推导出的地址）
//	-t 节点密钥（可选，缺省时在运行脚本时提供）
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
// （curl 拿到 400，`-f` 直接以错误退出）。
//
// 查询参数提供默认值，运行脚本时的命令行参数优先。
func (r *Registry) InstallScriptHandler(c *gin.Context) {
	base := firstNonEmpty(c.Query("u"), c.Query("url"), c.Query("base_url"))
	token := firstNonEmpty(c.Query("t"), c.Query("token"))

	// 面板地址缺省时，从本次请求推导：面板给谁下发脚本，
	// 谁就应该能访问到这个面板地址。
	if strings.TrimSpace(base) == "" {
		base = requestBaseURL(c)
	}

	if strings.TrimSpace(token) == "" {
		r.writeInstallScript(c, base, "")
		return
	}

	// 显式给了密钥：校验它确实存在，避免把无主的脚本发给攻击者做探测。
	_, err := r.nodeByToken(c.Request.Context(), strings.TrimSpace(token))
	if err != nil {
		ae := response.AsAppError(err)
		c.String(ae.HTTPStatus, "%s\n", ae.Msg)
		return
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
	c.Header("Cache-Control", "no-store")
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

// shellQuote prevents template values from becoming shell syntax.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// uninstallBody is shared by downloaded and locally generated uninstallers.
// Deriving the directory from a validated service prevents deleting the panel.
const uninstallBody = `
while getopts ":s:h" option; do
  case "$option" in
    s) SERVICE_NAME="$OPTARG" ;;
    h) echo "用法：bash uninstall.sh [-s 服务名]"; exit 0 ;;
    *) echo "卸载参数无效" >&2; exit 1 ;;
  esac
done
shift "$((OPTIND - 1))"
[ "$#" -eq 0 ] || { echo "不支持的位置参数" >&2; exit 1; }
[[ "$SERVICE_NAME" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$ ]] || { echo "服务名无效" >&2; exit 1; }
INSTALL_DIR="/opt/${SERVICE_NAME}-node"
SERVICE_UNIT="${SERVICE_NAME}-node"
SERVICE_FILE="/etc/systemd/system/${SERVICE_UNIT}.service"
[ "$(id -u)" = "0" ] || { echo "请以 root 身份运行卸载脚本" >&2; exit 1; }
systemctl show-environment >/dev/null 2>&1 || { echo "systemd 未运行" >&2; exit 1; }
echo "[OpenRoute] 停止并禁用服务 ${SERVICE_UNIT} ..."
systemctl stop "${SERVICE_UNIT}" >/dev/null 2>&1 || true
systemctl disable "${SERVICE_UNIT}" >/dev/null 2>&1 || true
if [ -f "${SERVICE_FILE}" ]; then
  rm -f -- "${SERVICE_FILE}"
  systemctl daemon-reload
  systemctl reset-failed "${SERVICE_UNIT}" >/dev/null 2>&1 || true
fi
answer=""
read -r -p "是否删除安装目录 ${INSTALL_DIR}（含配置与日志）？[y/N] " answer || true
case "${answer}" in
  y|Y|yes|YES)
    [ ! -L "${INSTALL_DIR}" ] || { echo "拒绝删除符号链接目录" >&2; exit 1; }
    rm -rf -- "${INSTALL_DIR}"
    ;;
  *) echo "[OpenRoute] 已保留 ${INSTALL_DIR}。" ;;
esac
echo "[OpenRoute] 卸载完成。"
`

var installTemplate = template.Must(template.New("install").Funcs(template.FuncMap{
	"sh": shellQuote,
}).Parse(`#!/usr/bin/env bash
# OpenRoute 节点安装脚本。运行参数覆盖面板提供的默认值。
set -euo pipefail
umask 077

PANEL_URL={{sh .BaseURL}}
NODE_TOKEN={{sh .Token}}
SERVICE_NAME={{sh .Service}}
SERVICE_NAME="${S:-${SERVICE_NAME}}"
ARCH_HINT={{sh .Arch}}
NODE_VERSION={{sh .Version}}
IS_OUTBOUND={{.IsOutbound}}

info() { printf '[OpenRoute] %s\n' "$*"; }
warn() { printf '[OpenRoute] 警告：%s\n' "$*" >&2; }
fail() { printf '[OpenRoute] 安装失败：%s\n' "$*" >&2; exit 1; }
usage() {
  echo "用法：bash install.sh -u 面板地址 -t 节点密钥 [-s 服务名] [-o 0|1] [-a auto|amd64|amd64v3|arm64] [-v 版本]"
}
while getopts ":u:t:s:o:a:v:h" option; do
  case "$option" in
    u) PANEL_URL="$OPTARG" ;;
    t) NODE_TOKEN="$OPTARG" ;;
    s) SERVICE_NAME="$OPTARG" ;;
    o) IS_OUTBOUND="$OPTARG" ;;
    a) ARCH_HINT="$OPTARG" ;;
    v) NODE_VERSION="$OPTARG" ;;
    h) usage; exit 0 ;;
    :) fail "选项 -${OPTARG} 缺少参数" ;;
    *) usage >&2; fail "未知选项 -${OPTARG}" ;;
  esac
done
shift "$((OPTIND - 1))"
[ "$#" -eq 0 ] || fail "不支持的位置参数：$*"

single_line() { [[ "$1" != *$'\n'* && "$1" != *$'\r'* ]]; }
[ -n "$PANEL_URL" ] || fail "缺少面板地址，请使用 -u"
[ -n "$NODE_TOKEN" ] || fail "缺少节点密钥，请从面板复制安装命令（-t）"
single_line "$NODE_TOKEN" || fail "节点密钥不能包含换行"
[[ "$PANEL_URL" != *[[:space:]]* ]] || fail "面板地址不能包含空白字符"
case "$PANEL_URL" in
  http://*|https://*) ;;
  *://*) fail "面板地址仅支持 http:// 或 https://" ;;
  *) PANEL_URL="http://${PANEL_URL}" ;;
esac
PANEL_URL="${PANEL_URL%/}"
[[ "$SERVICE_NAME" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$ ]] || fail "服务名仅支持 1 至 64 个字母、数字、下划线或连字符，且必须以字母或数字开头"
[[ "$NODE_VERSION" =~ ^[a-zA-Z0-9._+-]*$ ]] || fail "版本号包含不支持的字符"
case "$IS_OUTBOUND" in
  1|true|yes|on) IS_OUTBOUND=true ;;
  0|false|no|off) IS_OUTBOUND=false ;;
  *) fail "-o 必须为 0 或 1" ;;
esac
case "$ARCH_HINT" in auto|amd64|amd64v3|arm64) ;; *) fail "不支持的架构 ${ARCH_HINT}" ;; esac

# 面板通常位于 /opt/openroute；节点及各个实例使用独立目录。
INSTALL_DIR="/opt/${SERVICE_NAME}-node"
BINARY_PATH="${INSTALL_DIR}/{{.BinaryName}}"
CONFIG_PATH="${INSTALL_DIR}/{{.ConfigFile}}"
ENV_PATH="${INSTALL_DIR}/{{.EnvFile}}"
MACHINE_ID_PATH="${INSTALL_DIR}/{{.MachineID}}"
SERVICE_UNIT="${SERVICE_NAME}-node"
SERVICE_FILE="/etc/systemd/system/${SERVICE_UNIT}.service"
UNINSTALL_PATH="${INSTALL_DIR}/{{.Uninstall}}"

[ "$(id -u)" = "0" ] || fail "请以 root 身份运行"
[ -f /etc/os-release ] || fail "无法识别系统发行版（缺少 /etc/os-release）"
. /etc/os-release
case "${ID:-unknown}" in
  debian)
    [ "${VERSION_ID%%.*}" -ge {{.MinDebian}} ] || fail "请使用 Debian {{.MinDebian}} 或更高版本"
    ;;
  ubuntu)
    [ "${VERSION_ID%%.*}" -ge {{.MinUbuntu}} ] || fail "请使用 Ubuntu {{.MinUbuntu}}.04 或更高版本"
    ;;
  *) warn "当前系统未在支持列表内，继续安装" ;;
esac

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) fail "不支持的 CPU 架构，仅支持 amd64 / amd64v3 / arm64" ;;
  esac
}
# 自动选择通用 amd64；显式 -a amd64v3 可供确认支持完整 v3 指令集的机器使用。
TARGET_ARCH="$ARCH_HINT"
[ "$TARGET_ARCH" != auto ] || TARGET_ARCH="$(detect_arch)"
info "系统 ${PRETTY_NAME:-${ID:-unknown}}，架构 ${TARGET_ARCH}"
command -v curl >/dev/null 2>&1 || fail "缺少 curl，请先执行：apt update && apt install -y curl"
command -v systemctl >/dev/null 2>&1 || fail "缺少 systemctl，本脚本需要 systemd"
systemctl show-environment >/dev/null 2>&1 || fail "systemd 未运行，无法托管节点服务"

if [ "${INSTALL_TOOLS:-0}" = 1 ]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq || warn "apt update 失败"
  apt-get install -y -qq iftop mtr-tiny tcpdump net-tools dnsutils curl || warn "部分排查工具安装失败"
fi

[ ! -L "$INSTALL_DIR" ] || fail "拒绝使用符号链接安装目录 ${INSTALL_DIR}"
info "准备安装目录 ${INSTALL_DIR}"
mkdir -p -- "$INSTALL_DIR"
chmod 0700 "$INSTALL_DIR"
TMP_BINARY=""
TMP_CONFIG=""
TMP_ENV=""
TMP_SERVICE=""
cleanup() { rm -f -- "$TMP_BINARY" "$TMP_CONFIG" "$TMP_ENV" "$TMP_SERVICE"; }
trap cleanup EXIT
TMP_BINARY="$(mktemp "${INSTALL_DIR}/.rel_nodeclient.XXXXXX")"
TMP_CONFIG="$(mktemp "${INSTALL_DIR}/.config.XXXXXX")"
BINARY_URL="${PANEL_URL}/api/node/binary/${TARGET_ARCH}"
[ -z "$NODE_VERSION" ] || BINARY_URL="${BINARY_URL}?version=${NODE_VERSION}"
info "从面板下载节点客户端：${BINARY_URL}"
curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 15 -o "$TMP_BINARY" "$BINARY_URL" || fail "下载节点客户端失败。请确认面板可达且已部署 ${TARGET_ARCH} 客户端；旧客户端和配置已保留。"
[ -s "$TMP_BINARY" ] || fail "下载的客户端为空"
chmod 0755 "$TMP_BINARY"
"$TMP_BINARY" -h >/dev/null 2>&1 || fail "客户端自检失败，可能下载内容错误或 CPU 架构不匹配；旧客户端和配置已保留"

MACHINE_UUID="${UUID:-}"
if [ -z "$MACHINE_UUID" ] && [ -s "$MACHINE_ID_PATH" ]; then
  MACHINE_UUID="$(cat "$MACHINE_ID_PATH")"
fi
if [ -z "$MACHINE_UUID" ]; then
  if [ -r /proc/sys/kernel/random/uuid ]; then
    MACHINE_UUID="$(cat /proc/sys/kernel/random/uuid)"
  else
    MACHINE_UUID="$(date +%s%N)"
  fi
fi
single_line "$MACHINE_UUID" || fail "UUID 不能包含换行"
yaml_quote() {
  local value="$1"
  value="${value//\'/\'\'}"
  printf "'%s'" "$value"
}
cat > "$TMP_CONFIG" <<YAML
# OpenRoute 节点配置；修改后重启节点服务。
base-url: $(yaml_quote "$PANEL_URL")
token: $(yaml_quote "$NODE_TOKEN")
is-outbound: ${IS_OUTBOUND}
use-ech: false
ech-query-name: ""
direct-port: 0
ws-port: 0
tls-port: 0
udp-port: 0
rev-port: 0
connect-host: ""
connect-direct-port: 0
connect-ws-port: 0
connect-tls-port: 0
connect-udp-port: 0
connect-rev-port: 0
default-weight: 1
machine-id: $(yaml_quote "$MACHINE_UUID")
YAML
"$TMP_BINARY" -c "$TMP_CONFIG" -check || fail "配置自检失败；旧客户端和配置已保留"

# 已存在的环境文件保留运维人员的自定义值。
if [ ! -e "$ENV_PATH" ]; then
  TMP_ENV="$(mktemp "${INSTALL_DIR}/.env.XXXXXX")"
  printf '# OpenRoute 节点环境变量；修改后重启 %s\n' "$SERVICE_UNIT" > "$TMP_ENV"
  for env_name in DISABLE_EXECUTE BIND_INBOUND BIND_OUTBOUND_4 BIND_OUTBOUND_6 OUTBOUND_FWMARK COUNT_INTERFACE HEALTH_CHECK UUID; do
    env_value="${!env_name-}"
    if [ -n "$env_value" ]; then
      single_line "$env_value" || fail "${env_name} 不能包含换行"
      env_value="${env_value//\\/\\\\}"
      env_value="${env_value//\"/\\\"}"
      env_value="${env_value//\$/\\\$}"
      env_value="${env_value//$'\x60'/\\$'\x60'}"
      printf '%s="%s"\n' "$env_name" "$env_value" >> "$TMP_ENV"
    fi
  done
  mv -f -- "$TMP_ENV" "$ENV_PATH"
else
  info "保留已有环境变量文件 ${ENV_PATH}"
fi

mv -f -- "$TMP_BINARY" "$BINARY_PATH"
mv -f -- "$TMP_CONFIG" "$CONFIG_PATH"
printf '%s\n' "$MACHINE_UUID" > "$MACHINE_ID_PATH"
chmod 0600 "$CONFIG_PATH" "$ENV_PATH" "$MACHINE_ID_PATH"

# 本地卸载脚本绑定命令行解析后的服务实例。
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\nSERVICE_NAME=%q\n' "$SERVICE_NAME"
  cat <<'UNINSTALL_SCRIPT'
` + uninstallBody + `
UNINSTALL_SCRIPT
} > "$UNINSTALL_PATH"
chmod 0755 "$UNINSTALL_PATH"

TMP_SERVICE="$(mktemp /etc/systemd/system/.openroute-node.XXXXXX)"
cat > "$TMP_SERVICE" <<UNIT
[Unit]
Description=OpenRoute Node Client (${SERVICE_NAME})
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-${ENV_PATH}
Environment=OPENROUTE_SYSTEMD_UNIT=${SERVICE_UNIT}.service
WorkingDirectory=${INSTALL_DIR}
ExecStart=${BINARY_PATH} -c ${CONFIG_PATH}
Restart=always
RestartSec=3
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
ProtectSystem=false
PrivateTmp=false

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "$TMP_SERVICE"
mv -f -- "$TMP_SERVICE" "$SERVICE_FILE"
systemctl daemon-reload || fail "systemd daemon-reload 失败"
systemctl enable "$SERVICE_UNIT" >/dev/null 2>&1 || fail "无法启用服务 ${SERVICE_UNIT}"
systemctl restart "$SERVICE_UNIT" || fail "无法启动服务 ${SERVICE_UNIT}"
sleep 2
systemctl is-active --quiet "$SERVICE_UNIT" || fail "服务未能正常运行，请执行：journalctl -u ${SERVICE_UNIT} -n 100 --no-pager"

if [ "${OPTIMIZE:-0}" = 1 ]; then
  cat > /etc/sysctl.d/99-openroute.conf <<SYSCTL
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
  sysctl -p /etc/sysctl.d/99-openroute.conf >/dev/null 2>&1 || warn "部分内核优化参数写入失败"
fi

cat <<DONE

[OpenRoute] 节点安装完成，服务已启动：${SERVICE_UNIT}
面板地址：${PANEL_URL}
安装目录：${INSTALL_DIR}
配置文件：${CONFIG_PATH}
环境变量：${ENV_PATH}
实例标识：${MACHINE_UUID}
查看状态：systemctl status ${SERVICE_UNIT}
查看日志：journalctl -fu ${SERVICE_UNIT}
查看版本：${BINARY_PATH} -version
卸载节点：bash ${UNINSTALL_PATH}
请在面板确认节点注册与心跳状态。
DONE
`))

var uninstallTemplate = template.Must(template.New("uninstall").Funcs(template.FuncMap{
	"sh": shellQuote,
}).Parse("#!/usr/bin/env bash\nset -euo pipefail\nSERVICE_NAME={{sh .Service}}\n" + uninstallBody))
