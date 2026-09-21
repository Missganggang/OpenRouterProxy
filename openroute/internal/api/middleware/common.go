package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/util"
)

// RequestID 返回生成请求 ID 的中间件。
//
// 若请求已带 X-Request-ID 则沿用（便于跨系统串联日志），
// 否则生成一个新的。请求 ID 会写入响应头与响应体。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if rid == "" {
			rid = "req_" + util.MustRandomString(24)
		}
		c.Set(response.RequestIDKey, rid)
		c.Header("X-Request-ID", rid)
		c.Next()
	}
}

// Recovery 返回 panic 恢复中间件。
//
// 规格书 2.3 要求禁止 panic 透传到 HTTP 层，
// 因此这里捕获后记录完整堆栈，只向客户端返回 50001。
func Recovery(log *zap.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, recovered interface{}) {
		log.Error("请求处理发生 panic",
			zap.Any("panic", recovered),
			zap.String("path", c.Request.URL.Path),
			zap.String("method", c.Request.Method),
			zap.String("request_id", GetRequestID(c)),
		)
		response.Abort(c, response.New(response.CodeInternal, ""))
	})
}

// AccessLog 返回访问日志中间件。
//
// 只记录写操作与错误响应，避免读接口刷满日志。
// 个人自用场景下这个策略能显著降低日志噪音。
func AccessLog(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		isWrite := c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead
		// 只记录：写操作、非 2xx 响应。
		if !isWrite && status < 400 {
			return
		}

		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", status),
			zap.Duration("latency", time.Since(start)),
			zap.String("ip", util.ClientIP(c.Request)),
			zap.String("request_id", GetRequestID(c)),
		}
		if uid, un, _, ok := CurrentUser(c); ok && uid > 0 {
			fields = append(fields, zap.Uint64("user_id", uid), zap.String("username", un))
		}
		if len(c.Errors) > 0 {
			fields = append(fields, zap.String("errors", c.Errors.String()))
		}

		if status >= 500 {
			log.Error("请求失败", fields...)
		} else if status >= 400 {
			log.Warn("请求被拒绝", fields...)
		} else {
			log.Info("写操作", fields...)
		}
	}
}

// CORS 返回跨域中间件。
//
// 个人自用面板通常与前端同源（单端口同时提供 API 与 WebUI），
// 但开发时前端跑在 Vite 的 5173 端口，因此需要放行本地来源。
//
// 安全说明：这里不使用 `*` 通配，而是回显请求的 Origin 并允许携带凭证，
// 否则浏览器会拒绝带 Cookie 的跨域请求。
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers",
				"Content-Type, Authorization, X-API-Key, X-API-Timestamp, X-API-Sign, "+
					"X-Node-Token, X-Request-ID, Idempotency-Key")
			c.Header("Access-Control-Expose-Headers", "X-Request-ID")
			c.Header("Access-Control-Max-Age", "86400")
		}

		// 预检请求直接结束，不进入业务处理。
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// SecurityHeaders 返回基础安全响应头中间件。
//
// 面板默认不强制 HTTPS（规格书 10.1），因此这里只设置
// 与传输无关的安全头，避免误配导致面板无法访问。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "SAMEORIGIN")
		c.Header("Referrer-Policy", "no-referrer")
		c.Next()
	}
}

// RateLimitConfig 是一类接口的限流参数。
type RateLimitConfig struct {
	// Rate 是每秒允许的请求数。
	Rate float64
	// Burst 是突发容量。
	Burst int
	// KeyFunc 决定限流维度，默认按客户端 IP。
	KeyFunc func(*gin.Context) string
}

// RateLimiter 是进程内的令牌桶限流器。
//
// 设计取舍：规格书要求不引入 Redis，因此用进程内 map + 定期清理实现。
// 单实例部署下语义正确；多实例部署时每个实例各自计数（个人自用场景可接受）。
type RateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*limiterEntry
	rate     rate.Limit
	burst    int
	// lastGC 记录上次清理时间，避免每次请求都扫描整个 map。
	lastGC time.Time
}

// limiterEntry 是单个维度的限流器与其最近使用时间。
type limiterEntry struct {
	limiter *rate.Limiter
	seen    time.Time
}

// NewRateLimiter 构造一个限流器。
//
// 参数 perSecond 为每秒允许请求数；burst 为突发容量。
// 两者非法（<= 0）时回退到「5 req/s，突发 5」的默认值。
func NewRateLimiter(perSecond float64, burst int) *RateLimiter {
	if perSecond <= 0 {
		perSecond = 5
	}
	if burst <= 0 {
		burst = 5
	}
	return &RateLimiter{
		limiters: make(map[string]*limiterEntry),
		rate:     rate.Limit(perSecond),
		burst:    burst,
		lastGC:   time.Now(),
	}
}

// Middleware 返回该限流器的 gin 中间件。
//
// 参数 keyFunc 为空时按客户端 IP 限流。
func (rl *RateLimiter) Middleware(keyFunc func(*gin.Context) string) gin.HandlerFunc {
	if keyFunc == nil {
		keyFunc = func(c *gin.Context) string { return util.ClientIP(c.Request) }
	}
	return func(c *gin.Context) {
		key := keyFunc(c)
		if key == "" {
			// 取不到维度时不做限流，避免误伤全部请求。
			c.Next()
			return
		}
		if !rl.allow(key) {
			c.Header("Retry-After", strconv.Itoa(1))
			response.Abort(c, response.New(response.CodeRateLimited, ""))
			return
		}
		c.Next()
	}
}

// allow 判断指定维度是否放行本次请求，并顺带做惰性清理。
func (rl *RateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	// 惰性清理：每分钟最多清理一次，避免高频请求下反复遍历。
	if now.Sub(rl.lastGC) > time.Minute {
		for k, e := range rl.limiters {
			// 十分钟未使用的限流器可以直接丢弃。
			if now.Sub(e.seen) > 10*time.Minute {
				delete(rl.limiters, k)
			}
		}
		rl.lastGC = now
	}

	e, ok := rl.limiters[key]
	if !ok {
		e = &limiterEntry{limiter: rate.NewLimiter(rl.rate, rl.burst)}
		rl.limiters[key] = e
	}
	e.seen = now
	return e.limiter.Allow()
}

// LoginRateLimit 返回登录接口专用的限流中间件。
//
// 规格书 8.17：登录 5 次/分钟/IP。
// 更细的失败计数与锁定逻辑在 AuthService 中（需要落库），
// 这里只做最外层的暴力破解拦截。
func LoginRateLimit() gin.HandlerFunc {
	// 5 次/分钟 = 0.0833 req/s，突发 5。
	rl := NewRateLimiter(5.0/60.0, 5)
	return rl.Middleware(nil)
}

// SubscribeRateLimit 返回订阅接口专用的限流中间件。
//
// 规格书 8.17：订阅 10 次/分钟/Token。
func SubscribeRateLimit() gin.HandlerFunc {
	rl := NewRateLimiter(10.0/60.0, 10)
	return rl.Middleware(func(c *gin.Context) string {
		// 按 URL 中的 token 限流，取不到时回退到 IP。
		token := c.Param("token")
		if token == "" {
			return util.ClientIP(c.Request)
		}
		return "sub:" + token
	})
}

// WriteRateLimit 返回写接口专用的限流中间件。
//
// 规格书 8.17：写接口 2 req/s，突发 3。
// 按「用户 + IP」限流，避免同一 NAT 后的多个用户互相影响。
func WriteRateLimit() gin.HandlerFunc {
	rl := NewRateLimiter(2, 3)
	return rl.Middleware(func(c *gin.Context) string {
		if uid := CurrentUserID(c); uid > 0 {
			return "u:" + strconv.FormatUint(uid, 10)
		}
		return "ip:" + util.ClientIP(c.Request)
	})
}

// BatchRateLimit 返回批量操作专用的限流中间件。
//
// 规格书 8.17：批量操作 1 req/2s。
func BatchRateLimit() gin.HandlerFunc {
	rl := NewRateLimiter(0.5, 1)
	return rl.Middleware(func(c *gin.Context) string {
		if uid := CurrentUserID(c); uid > 0 {
			return "b:" + strconv.FormatUint(uid, 10)
		}
		return "bip:" + util.ClientIP(c.Request)
	})
}

// Gzip 返回 gzip 压缩中间件，可由配置关闭（规格书 3.1 的 disable-gzip）。
//
// 参数 disabled 为 true 时返回一个空中间件，保证路由注册代码无需分支。
// 压缩级别取 5：在压缩率与 CPU 开销之间折中，面板的 JSON 响应压缩收益明显。
func Gzip(disabled bool) gin.HandlerFunc {
	if disabled {
		return func(c *gin.Context) { c.Next() }
	}
	return gzip.Gzip(gzip.DefaultCompression, gzip.WithExcludedPaths([]string{"/api/node/stream"}))
}

// MaxBodySize 限制请求体大小，防止超大请求耗尽内存。
//
// 参数 maxBytes 为上限；超限时返回 40001。
func MaxBodySize(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > maxBytes {
			response.Abort(c, response.Field(response.CodeParamInvalid, "body", c.Request.ContentLength,
				"请求体过大，上限 "+strconv.FormatInt(maxBytes/1024/1024, 10)+"MB"))
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}
