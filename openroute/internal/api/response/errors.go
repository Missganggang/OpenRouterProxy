package response

import (
	"errors"
	"fmt"
	"net/http"
)

// 错误码区间划分（规格书 8.4）。
//
//	0            成功
//	40000-40099  参数错误
//	40100-40199  认证失败
//	40300-40399  权限不足
//	40400-40499  资源不存在
//	40900-40999  资源冲突
//	42200-42299  业务校验失败
//	42900-42999  触发限流
//	50000-50099  服务器内部错误
//	50300-50399  依赖不可用
//	60000-60099  节点通信错误
//	70000-70099  迁移相关错误
const (
	// 成功
	CodeOK = 0

	// 40000-40099 参数错误
	CodeParamInvalid    = 40001 // 参数缺失或格式错误
	CodeParamOutOfRange = 40002 // 参数值超出范围

	// 40100-40199 认证失败
	CodeUnauthorized    = 40101 // 未登录或 Token 无效
	CodeTokenExpired    = 40102 // Token 已过期
	CodeSignatureFailed = 40103 // 签名校验失败
	CodeBadCredentials  = 40104 // 用户名或密码错误
	CodeCaptchaRequired = 40105 // 需要验证码
	CodeCaptchaWrong    = 40106 // 验证码错误
	CodeAccountLocked   = 40107 // 账号锁定
	CodeAccountDisabled = 40108 // 账号已禁用

	// 40300-40399 权限不足
	CodeForbidden      = 40301 // 无权限执行此操作
	CodeScopeMissing   = 40302 // API Token 缺少所需 Scope
	CodeIPNotAllowed   = 40303 // IP 不在白名单内
	CodeWebSSHDisabled = 40304 // WebSSH 已被禁用

	// 40400-40499 资源不存在
	CodeNotFound = 40401 // 资源不存在

	// 40900-40999 资源冲突
	CodeNameConflict  = 40901 // 名称已存在（归一化比较后重复）
	CodePortConflict  = 40902 // 端口已被占用
	CodeResourceInUse = 40903 // 该资源正在被引用，无法删除
	CodeStateConflict = 40904 // 当前状态不允许该操作

	// 42200-42299 业务校验失败
	CodeFailoverGroupMismatch = 42201 // 故障转移组必须与当前组拥有相同的入口权限配置
	CodeChainNoUDP            = 42202 // 链式出口不支持 UDP
	CodeChainNoFailover       = 42203 // 链式出口不支持故障转移组
	CodeSNINotAllowed         = 42204 // SNI 不在入口组白名单内
	CodeMultiplierRange       = 42205 // 倍率超出允许范围
	CodeGroupRoleMismatch     = 42206 // 节点角色与设备组类型不匹配
	CodeFixPointRequired      = 42207 // 该场景必须使用固定端口
	CodeTargetRequired        = 42208 // 至少需要一个有效目标
	CodeReverseGroupInvalid   = 42209 // 反向组的出口节点未配置为出口角色
	CodeUDPNotSupported       = 42210 // 该场景不支持 UDP

	// 42900-42999 触发限流
	CodeRateLimited = 42901 // 请求过于频繁

	// 50000-50099 服务器内部错误
	CodeInternal = 50001 // 内部错误

	// 50300-50399 依赖不可用
	CodeDBUnwritable = 50301 // 数据库不可写

	// 60000-60099 节点通信错误
	CodeNodeOffline      = 60001 // 节点离线，无法下发配置
	CodeNodeApplyFail    = 60002 // 节点应用配置失败
	CodeNodeTimeout      = 60003 // 节点响应超时
	CodeNodeTokenInvalid = 60004 // 节点密钥无效

	// 70000-70099 迁移相关错误
	CodeSourceUnreachable  = 70001 // 源库无法连接
	CodeSourceUnsupported  = 70002 // 源库版本不被支持
	CodeSameDatabase       = 70003 // 目标库与源库相同
	CodeUnresolvedConflict = 70004 // 检测到无法自动处理的冲突，请先处理
	CodeRestoreFailed      = 70005 // 备份恢复失败
)

// errorMeta 描述错误码的默认信息。
type errorMeta struct {
	Msg  string
	HTTP int
}

// errorDict 是错误码字典，MUST 与规格书 8.4 的表格逐条对应。
//
// 新增错误码时必须同时在此登记，否则默认信息会退化为「未知错误」。
var errorDict = map[int]errorMeta{
	CodeOK: {"ok", http.StatusOK},

	CodeParamInvalid:    {"参数缺失或格式错误", http.StatusBadRequest},
	CodeParamOutOfRange: {"参数值超出范围", http.StatusBadRequest},

	CodeUnauthorized:    {"未登录或 Token 无效", http.StatusUnauthorized},
	CodeTokenExpired:    {"Token 已过期", http.StatusUnauthorized},
	CodeSignatureFailed: {"签名校验失败", http.StatusUnauthorized},
	CodeBadCredentials:  {"用户名或密码错误", http.StatusUnauthorized},
	CodeCaptchaRequired: {"连续失败次数过多，需要验证码", http.StatusUnauthorized},
	CodeCaptchaWrong:    {"验证码错误", http.StatusUnauthorized},
	CodeAccountLocked:   {"账号已被锁定，请稍后重试", http.StatusUnauthorized},
	CodeAccountDisabled: {"账号已被禁用", http.StatusUnauthorized},

	CodeForbidden:      {"无权限执行此操作", http.StatusForbidden},
	CodeScopeMissing:   {"API Token 缺少所需 Scope", http.StatusForbidden},
	CodeIPNotAllowed:   {"IP 不在白名单内", http.StatusForbidden},
	CodeWebSSHDisabled: {"WebSSH 已被禁用", http.StatusForbidden},

	CodeNotFound: {"资源不存在", http.StatusNotFound},

	CodeNameConflict:  {"名称已存在（归一化比较后重复）", http.StatusConflict},
	CodePortConflict:  {"端口已被占用", http.StatusConflict},
	CodeResourceInUse: {"该资源正在被引用，无法删除", http.StatusConflict},
	CodeStateConflict: {"当前状态不允许该操作", http.StatusConflict},

	CodeFailoverGroupMismatch: {"故障转移组必须与当前组拥有相同的入口权限配置", http.StatusUnprocessableEntity},
	CodeChainNoUDP:            {"链式出口不支持 UDP", http.StatusUnprocessableEntity},
	CodeChainNoFailover:       {"链式出口不支持故障转移组", http.StatusUnprocessableEntity},
	CodeSNINotAllowed:         {"SNI 不在入口组白名单内", http.StatusUnprocessableEntity},
	CodeMultiplierRange:       {"倍率超出允许范围", http.StatusUnprocessableEntity},
	CodeGroupRoleMismatch:     {"节点角色与设备组类型不匹配", http.StatusUnprocessableEntity},
	CodeFixPointRequired:      {"该场景必须使用固定端口", http.StatusUnprocessableEntity},
	CodeTargetRequired:        {"至少需要一个有效目标", http.StatusUnprocessableEntity},
	CodeReverseGroupInvalid:   {"反向组的出口节点未配置为出口角色", http.StatusUnprocessableEntity},
	CodeUDPNotSupported:       {"该场景不支持 UDP", http.StatusUnprocessableEntity},

	CodeRateLimited: {"请求过于频繁", http.StatusTooManyRequests},

	CodeInternal: {"内部错误", http.StatusInternalServerError},

	CodeDBUnwritable: {"数据库不可写", http.StatusServiceUnavailable},

	CodeNodeOffline:      {"节点离线，无法下发配置", http.StatusBadGateway},
	CodeNodeApplyFail:    {"节点应用配置失败", http.StatusBadGateway},
	CodeNodeTimeout:      {"节点响应超时", http.StatusGatewayTimeout},
	CodeNodeTokenInvalid: {"节点密钥无效", http.StatusUnauthorized},

	CodeSourceUnreachable:  {"源库无法连接", http.StatusBadRequest},
	CodeSourceUnsupported:  {"源库版本不被支持", http.StatusBadRequest},
	CodeSameDatabase:       {"目标库与源库相同", http.StatusConflict},
	CodeUnresolvedConflict: {"检测到无法自动处理的冲突，请先处理", http.StatusUnprocessableEntity},
	CodeRestoreFailed:      {"备份恢复失败", http.StatusInternalServerError},
}

// AppError 是贯穿 handler / service 的业务错误类型。
//
// 规格书 2.3 要求：业务错误使用自定义 AppError，由统一中间件转成响应体，
// 禁止 panic 透传到 HTTP 层。
type AppError struct {
	Code       int         `json:"code"`
	Msg        string      `json:"message"`
	HTTPStatus int         `json:"-"`
	Details    interface{} `json:"details,omitempty"`
	// Err 保存底层原始错误，只写日志，不返回给客户端。
	Err error `json:"-"`
}

// Error 实现 error 接口。
func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("[%d] %s: %v", e.Code, e.Msg, e.Err)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Msg)
}

// Unwrap 支持 errors.Is / errors.As 向下追溯原始错误。
func (e *AppError) Unwrap() error { return e.Err }

// New 按错误码字典构造 AppError。
//
// 参数 code 为已登记的错误码；msg 为空时使用字典中的默认信息，
// 非空时覆盖默认信息（用于补充具体上下文，如「节点 HK-01 不存在」）。
func New(code int, msg string) *AppError {
	meta, ok := errorDict[code]
	if !ok {
		meta = errorMeta{Msg: "未知错误", HTTP: http.StatusInternalServerError}
	}
	if msg == "" {
		msg = meta.Msg
	}
	return &AppError{Code: code, Msg: msg, HTTPStatus: meta.HTTP}
}

// WithDetails 附加结构化定位信息（如 field / value / hint），便于前端定位表单项。
func (e *AppError) WithDetails(details interface{}) *AppError {
	e.Details = details
	return e
}

// WithErr 记录底层错误用于日志，并保持错误码不变。
func (e *AppError) WithErr(err error) *AppError {
	e.Err = err
	return e
}

// Wrap 用底层错误构造 AppError，日志里能看到根因，客户端只看到友好信息。
func Wrap(code int, err error, msg string) *AppError {
	e := New(code, msg)
	e.Err = err
	return e
}

// DetailsField 是常用的详情结构，与规格书 8.3 的失败响应示例一致。
type DetailsField struct {
	Field string      `json:"field,omitempty"`
	Value interface{} `json:"value,omitempty"`
	Hint  string      `json:"hint,omitempty"`
}

// Field 构造字段级校验错误，供表单定位使用。
func Field(code int, field string, value interface{}, hint string) *AppError {
	return New(code, "").WithDetails(DetailsField{Field: field, Value: value, Hint: hint})
}

// AsAppError 把任意 error 归一化为 *AppError。
//
// 非 AppError 的错误统一视为内部错误，避免把数据库原始错误泄露给客户端。
func AsAppError(err error) *AppError {
	if err == nil {
		return New(CodeOK, "")
	}
	var ae *AppError
	if errors.As(err, &ae) {
		return ae
	}
	// 未包装的普通错误统一转成 50001，原始信息写入 Err 供日志使用。
	return Wrap(CodeInternal, err, "")
}

// HTTPStatusOf 返回错误码对应的 HTTP 状态码，未知码返回 500。
func HTTPStatusOf(code int) int {
	if meta, ok := errorDict[code]; ok {
		return meta.HTTP
	}
	return http.StatusInternalServerError
}

// MessageOf 返回错误码的默认中文信息，未知码返回「未知错误」。
func MessageOf(code int) string {
	if meta, ok := errorDict[code]; ok {
		return meta.Msg
	}
	return "未知错误"
}

// Dict 导出完整错误码字典，供 /api/v1/system/errors 与 docs 使用。
func Dict() map[int]errorMeta {
	out := make(map[int]errorMeta, len(errorDict))
	for k, v := range errorDict {
		out[k] = v
	}
	return out
}

// DictEntry 是错误码字典的可序列化条目。
type DictEntry struct {
	Code int    `json:"code"`
	Msg  string `json:"message"`
	HTTP int    `json:"http_status"`
}

// DictList 返回按错误码升序排列的字典条目，便于生成文档。
func DictList() []DictEntry {
	out := make([]DictEntry, 0, len(errorDict))
	for code, meta := range errorDict {
		out = append(out, DictEntry{Code: code, Msg: meta.Msg, HTTP: meta.HTTP})
	}
	// 简单插入排序，字典规模很小（几十条），无需引入 sort 包的比较器。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Code < out[j-1].Code; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
