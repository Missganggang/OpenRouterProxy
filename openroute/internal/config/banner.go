package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BannerInfo 是启动横幅需要展示的信息。
type BannerInfo struct {
	Version         string
	BuildStamp      string
	DatabaseDisplay string
	Listen          string
	WebUI           string
	LogPath         string
	HeartbeatSec    int
	OfflineSec      int
	JobCount        int
	HTMLPath        string
}

// ASCII 艺术字标题（规格书 3.3 要求 MUST 打印）。
const bannerArt = `
  ___                  ___      _
 / _ \ _ __   ___ _ __| _ \ ___| |_ ___
| | | | '_ \ / _ \ '_ \   // _ \ __/ _ \
| |_| | |_) |  __/ | | | |_\ \___/\__\___|
 \___/| .__/ \___|_| |_|___/\___||__/\___|
      |_|        OpenRoute`

// PrintBanner 打印启动横幅。
//
// 输出内容：ASCII 标题、版本与构建时间、数据库方言、监听地址、
// WebUI 地址、日志目录、节点通信参数、已调度的后台任务数。
//
// 参数 info 为横幅信息；返回打印错误（通常无需处理）。
func PrintBanner(info BannerInfo) error {
	var b strings.Builder

	b.WriteString(bannerArt)
	fmt.Fprintf(&b, " %s\n\n", info.Version)

	// 字段对齐：标签统一按「显示宽度」补齐到 12 列再输出。
	//
	// 不能直接用 %-10s：中文字符在终端占两列，用 fmt 的宽度参数会按字节/字符数
	// 补齐，导致「版本」「数据库」「监听地址」这些长短不一的标签右侧参差不齐。
	row := func(label, value string) {
		fmt.Fprintf(&b, "%s: %s\n", padDisplay(label, 12), value)
	}

	version := info.Version
	if info.BuildStamp != "" {
		version = fmt.Sprintf("%s (build %s)", info.Version, info.BuildStamp)
	}
	row("版本", version)
	row("数据库", info.DatabaseDisplay)
	row("监听地址", info.Listen)
	row("WebUI", info.WebUI)
	row("日志目录", info.LogPath)
	row("节点通信", fmt.Sprintf("已启动（心跳 %ds，离线判定 %ds）", info.HeartbeatSec, info.OfflineSec))
	row("后台任务", fmt.Sprintf("%d 个已调度", info.JobCount))

	// html-path 缺失时给出明确提示，避免用户看到空白页却不知原因。
	if info.HTMLPath != "" {
		if _, err := os.Stat(filepath.Join(info.HTMLPath, "index.html")); err != nil {
			b.WriteString("\n")
			row("提示", fmt.Sprintf("未找到前端资源 %s/index.html，"+
				"WebUI 暂不可用；请先执行 cd frontend && npm run build", info.HTMLPath))
		}
	}

	b.WriteString("\n")
	_, err := os.Stdout.WriteString(b.String())
	return err
}

// padDisplay 把字符串按「终端显示宽度」右侧补空格到 width 列。
//
// 规则：ASCII 与半角字符占 1 列，CJK 及全角符号占 2 列。
// 只处理横幅里实际会出现的字符范围，不做完整的 Unicode 宽度表。
//
// 参数 s 为原字符串；width 为目标显示宽度。
// 返回补齐后的字符串（原串已超过宽度时原样返回）。
func padDisplay(s string, width int) string {
	w := 0
	for _, r := range s {
		w += runeDisplayWidth(r)
	}
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// runeDisplayWidth 返回单个字符的终端显示宽度。
//
// 覆盖中日韩文字、全角标点与常见全角符号；其余按 1 列计。
func runeDisplayWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 韩文字母
		r >= 0x2E80 && r <= 0xA4CF,   // CJK 部首、假名、中文
		r >= 0xAC00 && r <= 0xD7A3,   // 韩文音节
		r >= 0xF900 && r <= 0xFAFF,   // CJK 兼容表意文字
		r >= 0xFE30 && r <= 0xFE6F,   // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60,   // 全角 ASCII
		r >= 0xFFE0 && r <= 0xFFE6,   // 全角符号
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展
		return 2
	}
	return 1
}

// WebUIURL 根据监听地址推导一个便于点击访问的 WebUI 地址。
//
// 监听 0.0.0.0 / :: 时替换为 127.0.0.1，因为 0.0.0.0 不是可访问地址。
//
// 参数 tlsEnabled 为 true 时用 https 前缀：启用 TLS 后仍打印 http:// 会让人
// 直接复制一个打不开的地址（浏览器会以明文请求 TLS 端口而失败）。
// 443 是 https 的默认端口，此时省略端口号，输出更接近用户实际输入的地址。
func WebUIURL(listen string, tlsEnabled bool) string {
	host, port := splitHostPort(listen)
	switch host {
	case "0.0.0.0", "::", "[::]", "":
		host = "127.0.0.1"
	}

	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}

	if tlsEnabled && port == "443" {
		return fmt.Sprintf("%s://%s/", scheme, host)
	}
	return fmt.Sprintf("%s://%s:%s/", scheme, host, port)
}

// splitHostPort 拆分监听地址，port 缺失时默认 18888。
func splitHostPort(addr string) (string, string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "127.0.0.1", "18888"
	}
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, "18888"
	}
	host := addr[:idx]
	port := addr[idx+1:]
	if port == "" {
		port = "18888"
	}
	return host, port
}

// BuildStampNow 返回默认的构建时间戳，格式 yyyymmdd。
//
// 正式发布时通过 -ldflags 注入固定值，保证同一版本的横幅一致。
func BuildStampNow() string {
	return time.Now().UTC().Format("20060102")
}
