package util

import (
	"fmt"
	"strings"
)

// 速率单位换算常量。前端限速输入框支持 KB/s、MB/s、Gbps。
const (
	KB = 1024
	MB = 1024 * KB
	GB = 1024 * MB
	// Gbps 是网络速率单位，1 Gbps = 1000^3 bit/s = 125000000 byte/s
	GbpsInBytesPerSec int64 = 1000 * 1000 * 1000 / 8
)

// ParseSpeed 把「数值 + 单位」解析为 byte/s。
//
// 支持的单位：B/s（或空）、KB/s、MB/s、GB/s、Gbps、Mbps、Kbps。
// 大小写不敏感，允许 `kb/s`、`KB`、`MBps` 等写法。
//
// 参数 value 为数值（允许小数）；unit 为单位字符串。
// 返回换算后的 byte/s，以及单位非法时的错误。
func ParseSpeed(value float64, unit string) (int64, error) {
	if value < 0 {
		return 0, fmt.Errorf("速率不能为负数")
	}
	u := strings.ToLower(strings.TrimSpace(unit))
	u = strings.ReplaceAll(u, "/s", "")
	u = strings.TrimSpace(u)

	// Gbps 是「比特」单位（网络速率），与 GB/s（字节）不是同一个量级，
	// 必须单独处理，不能与其他 g 前缀混在一起。
	if u == "gbps" {
		return int64(value * float64(GbpsInBytesPerSec)), nil
	}
	if u == "mbps" {
		return int64(value * float64(MB) / 8), nil
	}
	if u == "kbps" {
		return int64(value * float64(KB) / 8), nil
	}
	if u == "bps" {
		return int64(value / 8), nil
	}

	var factor float64
	switch u {
	case "", "b":
		factor = 1
	case "k", "kb":
		factor = KB
	case "m", "mb":
		factor = MB
	case "g", "gb":
		factor = GB
	default:
		return 0, fmt.Errorf("不支持的速率单位 %q，可用：B/s、KB/s、MB/s、GB/s、Kbps、Mbps、Gbps", unit)
	}
	return int64(value * factor), nil
}

// FormatBytes 把字节数格式化为人类可读字符串（带单位）。
//
// 用于日志与纯文本报告；WebUI 由前端自行格式化，不依赖本函数。
func FormatBytes(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	out := fmt.Sprintf("%.2f %s", v, units[i])
	if neg {
		return "-" + out
	}
	return out
}

// EqualFoldName 是「按名称查重」的统一入口。
//
// 规格书 4.1 的跨方言要求：MySQL 默认不区分大小写，而 PostgreSQL 与 SQLite 区分。
// 为保证三种库行为一致，所有按名称查重的逻辑 MUST 在应用层做
// 大小写归一化比较，不得依赖数据库排序规则。
func EqualFoldName(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// FindNameConflict 在已有名称列表中查找与 name 归一化后重复的项。
//
// 参数 names 为已存在的名称集合（键为名称）；name 为待检查名称。
// 返回冲突的原始名称与是否冲突。excludeID 用于「更新时排除自身」的场景，
// 由调用方在构造 names 时排除。
func FindNameConflict(names []string, name string) (string, bool) {
	for _, n := range names {
		if EqualFoldName(n, name) {
			return n, true
		}
	}
	return "", false
}

// DedupeNamesFold 对名称列表做归一化去重，返回重复项清单。
//
// 供迁移预检报告使用：报告「跨库迁移后会产生的名称冲突」。
func DedupeNamesFold(names []string) map[string][]string {
	groups := make(map[string][]string)
	for _, n := range names {
		key := strings.ToLower(strings.TrimSpace(n))
		groups[key] = append(groups[key], n)
	}
	// 只保留真正发生冲突的组。
	for k, v := range groups {
		if len(v) < 2 {
			delete(groups, k)
		}
	}
	return groups
}

// InRange 判断整数是否落在 [min, max] 闭区间内。
func InRange(v, min, max int) bool {
	return v >= min && v <= max
}

// InRangeFloat 判断浮点数是否落在 [min, max] 闭区间内（含边界容差）。
func InRangeFloat(v, min, max float64) bool {
	const epsilon = 1e-9
	return v >= min-epsilon && v <= max+epsilon
}

// 倍率的允许范围（规格书 6.4：默认 1，范围 0 ~ 100，允许 2 位小数）。
const (
	MultiplierMin = 0.0
	MultiplierMax = 100.0
)

// ValidMultiplier 校验倍率是否在允许范围内且小数位不超过两位。
func ValidMultiplier(v float64) error {
	if !InRangeFloat(v, MultiplierMin, MultiplierMax) {
		return fmt.Errorf("倍率 %.4f 超出允许范围 %.0f ~ %.0f", v, MultiplierMin, MultiplierMax)
	}
	// 两位小数校验：放大 100 倍后应为整数（允许浮点误差）。
	scaled := v * 100
	if scaled-float64(int64(scaled+0.5)) > 1e-6 || float64(int64(scaled+0.5))-scaled > 1e-6 {
		return fmt.Errorf("倍率 %.4f 小数位超过两位", v)
	}
	return nil
}

// ValidPort 校验端口号是否在合法范围内（1~65535）。
func ValidPort(p int) bool { return p >= 1 && p <= 65535 }

// ParsePortRange 解析端口段写法，如 `8443-8450`。
//
// 返回起始端口、结束端口与错误；单端口写法（如 `8443`）返回 end = 0。
func ParsePortRange(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, fmt.Errorf("端口不能为空")
	}
	if !strings.Contains(s, "-") {
		var p int
		if _, err := fmt.Sscanf(s, "%d", &p); err != nil {
			return 0, 0, fmt.Errorf("端口 %q 不是合法数字", s)
		}
		if !ValidPort(p) {
			return 0, 0, fmt.Errorf("端口 %d 超出范围 1~65535", p)
		}
		return p, 0, nil
	}

	parts := strings.SplitN(s, "-", 2)
	var start, end int
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &start); err != nil {
		return 0, 0, fmt.Errorf("端口段起始值 %q 不是合法数字", parts[0])
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &end); err != nil {
		return 0, 0, fmt.Errorf("端口段结束值 %q 不是合法数字", parts[1])
	}
	if !ValidPort(start) || !ValidPort(end) {
		return 0, 0, fmt.Errorf("端口段 %d-%d 超出范围 1~65535", start, end)
	}
	if end < start {
		return 0, 0, fmt.Errorf("端口段 %d-%d 的结束值小于起始值", start, end)
	}
	return start, end, nil
}

// Truncate 按最大长度截断字符串，用于写入有长度限制的数据库列前做保护。
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	// 按字节截断可能切断 UTF-8 字符，回退到最后一个完整字符边界。
	cut := max
	for cut > 0 && !isUTF8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// isUTF8Start 判断字节是否为 UTF-8 字符的首字节。
func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }
