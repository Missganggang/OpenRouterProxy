package api

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/middleware"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/job"
	"github.com/openroute/openroute/internal/util"
)

// scheduler 返回当前进程注册的后台任务调度器。
//
// 通过 Server 在构造时注入，避免 api 包反向依赖 main 包。
// 未注入时返回 nil，调用方需自行处理。
func (h *Handlers) scheduler() *job.Scheduler {
	return h.jobs
}

// jobIsNotFound 判断任务错误是否为「任务不存在」。
func jobIsNotFound(err error) bool {
	return job.IsNotFound(err)
}

// parseTimeQuery 解析 RFC3339 时间查询参数。
//
// 参数 name 为参数名；返回解析结果与是否成功。
// 未提供时返回零值与 false（调用方据此跳过该筛选条件）。
func parseTimeQuery(c *gin.Context, name string) (time.Time, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// 兼容浏览器 date-picker 常见的「只有日期」写法。
		if t2, err2 := time.Parse("2006-01-02", raw); err2 == nil {
			return t2.UTC(), true
		}
		return time.Time{}, false
	}
	return t.UTC(), true
}

// timeRange 解析 from / to 区间，并为缺失的边界填默认值。
//
// 参数 defFrom / defTo 为缺省区间（相对当前时间的时长）。
// 返回起止时间，保证 from <= to。
func timeRange(c *gin.Context, defFrom, defTo time.Duration) (time.Time, time.Time) {
	now := time.Now().UTC()
	from, okFrom := parseTimeQuery(c, "from")
	to, okTo := parseTimeQuery(c, "to")

	if !okFrom {
		from = now.Add(-defFrom)
	}
	if !okTo {
		to = now.Add(defTo)
	}
	// 顺序颠倒时交换，避免生成负数区间的查询。
	if from.After(to) {
		from, to = to, from
	}
	return from, to
}

// bytesMode 解析「显示原始值 / 乘倍率值」的开关（规格书 6.10 统计口径）。
func bytesMode(c *gin.Context) app.BytesMode {
	if raw := strings.TrimSpace(c.Query("raw")); raw == "true" || raw == "1" {
		return app.BytesModeRaw
	}
	return app.BytesModeScaled
}

// itoa 是无依赖的整数转字符串，避免在每个 handler 里写 strconv。
func itoa(v int) string { return strconv.Itoa(v) }

// utoa 是无依赖的 uint64 转字符串。
func utoa(v uint64) string { return strconv.FormatUint(v, 10) }

// parseIDList 解析请求体中的 ID 列表，去重并保持顺序。
func parseIDList(ids []uint64) []uint64 {
	if len(ids) == 0 {
		return []uint64{}
	}
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// validScope 校验 API Token 的权限范围是否合法（规格书 8.2）。
//
// 支持通配：`*` 与「资源级通配」如 `rule:*`。
func validScope(s string) bool {
	if s == "*" {
		return true
	}
	// 资源级通配：rule:* / node:* 等。
	if strings.HasSuffix(s, ":*") {
		prefix := strings.TrimSuffix(s, ":*")
		for _, full := range middleware.AllScopes() {
			if strings.HasPrefix(full, prefix+":") {
				return true
			}
		}
		return false
	}
	for _, full := range middleware.AllScopes() {
		if full == s {
			return true
		}
	}
	return false
}

// normalizeCIDR 校验并规范化一个 IP 或 CIDR 字符串。
//
// 单个 IP 会被补上掩码长度（IPv4 /32、IPv6 /128）。
// 返回规范化结果与是否合法。
func normalizeCIDR(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if strings.Contains(s, "/") {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			return "", false
		}
		return ip.String() + "/" + maskLen(n), true
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "", false
	}
	if ip.To4() != nil {
		return ip.String() + "/32", true
	}
	return ip.String() + "/128", true
}

// maskLen 返回网段的掩码长度字符串。
func maskLen(n *net.IPNet) string {
	ones, _ := n.Mask.Size()
	return strconv.Itoa(ones)
}

// newAPIToken 生成明文 API Token。
func newAPIToken() (string, error) {
	return util.GenerateAPIToken()
}

// hashToken 计算 Token 的落库哈希。
func hashToken(plain string) string {
	return util.SHA256Hex(plain)
}

// baseURL 推导面板的可访问地址，用于生成节点的安装命令与订阅链接。
//
// 优先使用请求头中的 Host（用户从哪个地址访问面板，
// 节点大概也能从那个地址访问到），并依据 X-Forwarded-Proto 判断协议。
func baseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := c.Request.Host
	if host == "" {
		host = "127.0.0.1:18888"
	}
	return scheme + "://" + host
}

// userCanAccess 判断当前用户是否有权操作指定归属的资源。
//
// 管理员可操作全部；普通用户只能操作自己的资源。
func userCanAccess(c *gin.Context, ownerID uint64) bool {
	if middleware.IsAdmin(c) {
		return true
	}
	return middleware.CurrentUserID(c) == ownerID
}

// csvHeader 设置 CSV 下载的响应头。
func csvHeader(c *gin.Context, filename string) {
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
}

// attachmentHeader 设置二进制附件的响应头。
func attachmentHeader(c *gin.Context, filename, contentType string) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
}

// dbOf 是 h.gormDBOf 的包级便捷版本，供不喜欢方法调用的场合使用。
func dbOf(h *Handlers, c *gin.Context) *gorm.DB {
	return h.app.DB.WithContext(c.Request.Context())
}
