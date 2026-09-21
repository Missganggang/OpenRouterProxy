// Package response 定义统一的 HTTP 响应包体与错误码字典。
//
// 设计目的（规格书 8.3）：所有接口返回同一个外层结构，前端只需要写一套
// 拦截器就能处理全部错误；错误码按区间分类，便于第三方对接时做分支判断。
package response

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Body 是统一响应体。
//
// 成功时 Code 为 0；失败时 Code 为错误码字典中的值，Details 携带定位信息。
// RequestID 与 Timestamp 每个响应都带，便于用户报障时对日志。
type Body struct {
	Code      int         `json:"code"`
	Message   string      `json:"message"`
	Data      interface{} `json:"data,omitempty"`
	Details   interface{} `json:"details,omitempty"`
	RequestID string      `json:"request_id"`
	Timestamp string      `json:"timestamp"`
}

// Pagination 是列表接口的分页信息。
type Pagination struct {
	Page       int   `json:"page"`
	PageSize   int   `json:"page_size"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages"`
}

// ListData 是列表接口的 data 结构。
type ListData struct {
	Items      interface{} `json:"items"`
	Pagination Pagination  `json:"pagination"`
}

// RequestIDKey 是存放在 gin.Context 中的请求 ID 键名。
const RequestIDKey = "openroute_request_id"

// Timestamp 返回响应时间戳，统一 RFC 3339 UTC 格式。
func timestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// requestID 从上下文取出请求 ID；中间件未启用时回退为空串。
func requestID(c *gin.Context) string {
	if v, ok := c.Get(RequestIDKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// OK 返回成功响应（HTTP 200，code 0）。
//
// 参数 c 为 Gin 上下文；data 为业务数据，传 nil 时 data 字段被省略。
func OK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Body{
		Code:      0,
		Message:   "ok",
		Data:      data,
		RequestID: requestID(c),
		Timestamp: timestamp(),
	})
}

// OKMessage 返回带自定义 message 的成功响应，用于「操作成功」类提示。
func OKMessage(c *gin.Context, message string, data interface{}) {
	c.JSON(http.StatusOK, Body{
		Code:      0,
		Message:   message,
		Data:      data,
		RequestID: requestID(c),
		Timestamp: timestamp(),
	})
}

// OKMsg 是 OKMessage 的简写别名。
//
// 保留两个名字是因为 handler 层大量使用 OKMsg，语义完全一致；
// 对外只暴露一个函数也可以，但别名让调用点更短、更易读。
func OKMsg(c *gin.Context, message string, data interface{}) {
	OKMessage(c, message, data)
}

// List 返回分页列表响应。
//
// 参数 items 为当前页数据（应为切片，空时输出 [] 而不是 null）；
// page/pageSize/total 用于计算 total_pages。
func List(c *gin.Context, items interface{}, page, pageSize int, total int64) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	totalPages := 0
	if total > 0 {
		totalPages = int((total + int64(pageSize) - 1) / int64(pageSize))
	}
	OK(c, ListData{
		Items: items,
		Pagination: Pagination{
			Page:       page,
			PageSize:   pageSize,
			Total:      total,
			TotalPages: totalPages,
		},
	})
}

// Fail 按 AppError 输出失败响应。
//
// 这是 handler 层唯一的失败出口，保证 HTTP 状态码与业务错误码始终配套。
func Fail(c *gin.Context, err error) {
	ae := AsAppError(err)
	c.JSON(ae.HTTPStatus, Body{
		Code:      ae.Code,
		Message:   ae.Msg,
		Details:   ae.Details,
		RequestID: requestID(c),
		Timestamp: timestamp(),
	})
}

// FailWith 直接按错误码字典构造失败响应，用于不需要额外上下文的场景。
func FailWith(c *gin.Context, code int) {
	Fail(c, New(code, ""))
}

// Abort 输出失败响应并终止后续中间件，用于中间件内拒绝请求。
func Abort(c *gin.Context, err error) {
	ae := AsAppError(err)
	c.AbortWithStatusJSON(ae.HTTPStatus, Body{
		Code:      ae.Code,
		Message:   ae.Msg,
		Details:   ae.Details,
		RequestID: requestID(c),
		Timestamp: timestamp(),
	})
}
