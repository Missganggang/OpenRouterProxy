package util

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// MatchHostSuffix 判断目标域名是否命中后缀规则（规格书 4.3 的 allowed_host / blocked_host 语义）。
//
// 语义（与 Nyanpass 一致，MUST 严格保持）：
//
//	".qq.com"  匹配 a.qq.com、b.qq.com，但不匹配 qq.com 本身
//	"qq.com"   匹配 qq.com 与 a.qq.com（不带点号视为「含自身」的后缀）
//	"*"        匹配任意域名
//	空规则列表 由调用方决定语义（白名单为空 = 不限制）
//
// 参数 host 为目标主机名或 IP；rule 为单条规则。
func MatchHostSuffix(host, rule string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	rule = strings.ToLower(strings.TrimSpace(rule))
	if rule == "" {
		return false
	}
	if rule == "*" {
		return true
	}

	// 带点号前缀：只做严格后缀匹配，不匹配自身。
	if strings.HasPrefix(rule, ".") {
		return strings.HasSuffix(host, rule)
	}

	// 不带点号：匹配自身或任意层级子域。
	if host == rule {
		return true
	}
	return strings.HasSuffix(host, "."+rule)
}

// MatchAnyHost 判断 host 是否命中规则列表中的任意一条。
func MatchAnyHost(host string, rules []string) bool {
	for _, r := range rules {
		if MatchHostSuffix(host, r) {
			return true
		}
	}
	return false
}

// CheckHostPolicy 按入口组的白名单/黑名单裁决目标域名。
//
// 规则（规格书 4.3）：黑名单优先级高于白名单。
// 返回 allowed 表示是否放行，reason 为拒绝原因（用于日志与错误提示）。
func CheckHostPolicy(host string, allowed, blocked []string) (bool, string) {
	if len(blocked) > 0 && MatchAnyHost(host, blocked) {
		return false, fmt.Sprintf("目标域名 %s 命中入口组黑名单", host)
	}
	if len(allowed) > 0 && !MatchAnyHost(host, allowed) {
		return false, fmt.Sprintf("目标域名 %s 不在入口组白名单内", host)
	}
	return true, ""
}

// MatchPathPrefix 判断 HTTP 路径是否命中黑名单前缀（规格书 4.3 的 blocked_path）。
//
// 语义为「前缀匹配」，因此 "/admin" 会拦截 "/admin" 与 "/admin/x"。
func MatchPathPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// MatchProtocol 判断入站协议类型是否被禁止（规格书 4.3 的 blocked_protocol）。
//
// 协议类型取值：socks、fet、http、tls。
func MatchProtocol(proto string, blocked []string) bool {
	proto = strings.ToLower(strings.TrimSpace(proto))
	for _, b := range blocked {
		if strings.EqualFold(strings.TrimSpace(b), proto) {
			return true
		}
	}
	return false
}

// ParseCIDRList 把 CIDR 字符串列表解析为网段列表。
//
// 单个 IP 会按 /32（IPv4）或 /128（IPv6）处理，便于用户直接填 IP。
// 解析失败的条目会被跳过，不返回错误——白名单场景下「宁可漏放行」。
func ParseCIDRList(list []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				continue
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			s = fmt.Sprintf("%s/%d", s, bits)
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// IPInCIDRList 判断 IP 是否落在任一网段内。
//
// 列表为空时返回 true（白名单未配置 = 不限制）。
func IPInCIDRList(ipStr string, nets []*net.IPNet) bool {
	if len(nets) == 0 {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP 从请求中提取真实客户端 IP。
//
// 取值优先级（与规格书 8.17 的限流维度一致）：
//  1. X-Forwarded-For 的最左值（最早的代理记录）
//  2. X-Real-IP
//  3. 直连的 RemoteAddr
//
// 说明：个人自用场景下，用户常在反向代理后运行面板；
// 若面板直接暴露公网，这些头可被伪造，但本面板的 IP 限制用于
// 「防同好误用」而非对抗性安全，因此接受该权衡。
func ClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if v := strings.TrimSpace(parts[0]); v != "" {
			return normalizeIP(v)
		}
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return normalizeIP(real)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return normalizeIP(r.RemoteAddr)
	}
	return normalizeIP(host)
}

// normalizeIP 去掉 IPv6 映射前缀（::ffff:1.2.3.4 → 1.2.3.4），
// 保证同一客户端在双栈环境下得到同一个 IP 字符串。
func normalizeIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "::ffff:") {
		s = strings.TrimPrefix(s, "::ffff:")
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return s
}

// HashIP 对 IP 做加盐哈希，用于匿名化统计（规格书 4.2.10 的 ClientIPHash）。
//
// 加盐后无法反查真实 IP，但同一 IP 在同一密钥下哈希稳定，可做去重统计。
func HashIP(ip, salt string) string {
	if ip == "" {
		return ""
	}
	return SHA256Hex(ip + "|" + salt)
}

// ValidIPOrHost 校验字符串是否为合法的 IP 或域名。
//
// 用于节点地址、目标地址的表单校验。
func ValidIPOrHost(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	return ValidDomain(s)
}

// ValidDomain 校验是否为合法域名。
//
// 规则：总长 1~253，标签由字母/数字/连字符组成，
// 不能以连字符开头或结尾，顶层域至少 2 个字符。
// 同时接受 localhost 这种无点号的单标签主机名（内网专线场景常用）。
func ValidDomain(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 253 {
		return false
	}
	// 允许纯数字主机名（某些内网环境用），但不允许空标签。
	labels := strings.Split(s, ".")
	if len(labels) == 1 {
		// 单标签：localhost 或内网短名
		return validLabel(labels[0]) && len(labels[0]) > 0
	}
	for i, l := range labels {
		if !validLabel(l) {
			return false
		}
		// 顶层域不允许纯数字（避免把 IPv4 误判为域名，IPv4 已在前面处理）
		if i == len(labels)-1 {
			if len(l) < 2 {
				return false
			}
			allDigit := true
			for _, c := range l {
				if c < '0' || c > '9' {
					allDigit = false
					break
				}
			}
			if allDigit {
				return false
			}
		}
	}
	return true
}

// validLabel 校验单个域名标签。
func validLabel(l string) bool {
	if l == "" || len(l) > 63 {
		return false
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, c := range l {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}
