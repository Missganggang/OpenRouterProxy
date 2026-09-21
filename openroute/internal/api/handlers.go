package api

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/openroute/openroute/internal/agent"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/job"
)

// Handlers 汇总全部 HTTP handler。
//
// 所有 handler 都是本类型的方法，共享 app、logger 与任务调度器依赖，
// 避免在每个 handler 里重复传参。
type Handlers struct {
	app  *app.App
	log  *zap.Logger
	jobs *job.Scheduler
	// agent 是节点通信的处理器注册表，延迟构造（首次用到时才创建）。
	agent *agent.Registry
	// terminalCounter 统计每个用户当前打开的 WebSSH 终端数（规格书 8.17）。
	terminalCounter counter
}

// NewHandlers 构造 handler 集合。
//
// 参数 deps 为路由层依赖；任务调度器可能为空（CLI 模式下不启动调度），
// 相关 handler 会自行处理 nil 情况。
func NewHandlers(deps Deps) *Handlers {
	return &Handlers{app: deps.App, log: deps.Log, jobs: deps.Jobs}
}

// App 返回内部持有的依赖，供需要直接访问 service 的 handler 使用。
func (h *Handlers) App() *app.App { return h.app }

// -------------------- 通用参数解析 --------------------

// pathID 解析路径中的 `:id` 参数。
//
// 返回 0 与 false 表示参数缺失或不是合法正整数，
// 调用方应据此返回 40001。
func pathID(c *gin.Context, name string) (uint64, bool) {
	raw := c.Param(name)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// queryInt 读取整数查询参数，缺失或非法时返回默认值。
func queryInt(c *gin.Context, name string, def int) int {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

// queryUint64 读取 uint64 查询参数，缺失或非法时返回默认值。
func queryUint64(c *gin.Context, name string, def uint64) uint64 {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

// queryBool 读取布尔查询参数。
//
// 支持 true/false/1/0/yes/no（大小写不敏感）。
// 参数缺失时返回 nil，便于区分「未传」与「传了 false」。
func queryBool(c *gin.Context, name string) *bool {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil
	}
	switch strings.ToLower(raw) {
	case "true", "1", "yes", "y":
		v := true
		return &v
	case "false", "0", "no", "n":
		v := false
		return &v
	default:
		return nil
	}
}

// pageParams 解析分页参数并做边界收敛。
//
// page 最小 1；page_size 范围 1~200，默认 20。
// 上限 200 是为了避免单次请求拉取过多数据拖慢面板。
func pageParams(c *gin.Context) (page, pageSize int) {
	page = queryInt(c, "page", 1)
	pageSize = queryInt(c, "page_size", 20)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}
	return page, pageSize
}

// sortParams 解析排序参数。
//
// 参数 allowed 为允许排序的字段白名单——这是防止 SQL 注入的关键：
// 只有白名单内的字段名才会被拼进 ORDER BY。
// 返回排序字段与方向（asc / desc）。
func sortParams(c *gin.Context, allowed map[string]bool, defField, defOrder string) (string, string) {
	field := strings.TrimSpace(c.Query("sort"))
	if field == "" || !allowed[field] {
		field = defField
	}
	order := strings.ToLower(strings.TrimSpace(c.Query("order")))
	if order != "asc" && order != "desc" {
		order = defOrder
	}
	if order == "" {
		order = "desc"
	}
	return field, order
}

// badRequest 返回参数错误响应，便于 handler 内一行处理。
func badRequest(c *gin.Context, field, hint string) {
	response.Fail(c, response.Field(response.CodeParamInvalid, field, nil, hint))
}
