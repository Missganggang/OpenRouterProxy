package api

import (
	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/agent"
	"github.com/openroute/openroute/internal/api/response"
)

// registry 返回节点通信的处理器注册表（规格书 8.16）。
//
// 节点通信的全部业务逻辑位于 agent 包：面板这一侧只负责把 HTTP 请求
// 转交给它，因此本文件全部是薄转发，不写任何业务判断。
//
// 返回值可能为 nil（构造失败时），调用方需自行兜底。
func (h *Handlers) registry() *agent.Registry {
	if h.agent == nil {
		h.agent = agent.NewRegistry(h.app)
	}
	return h.agent
}

// NodeRegister 处理节点首次注册（规格书 5.1 第 5~6 步）。
//
// 认证方式为请求体中的 token，此时节点尚无会话。
func (h *Handlers) NodeRegister(c *gin.Context) {
	h.registry().RegisterHandler(c)
}

// NodeHeartbeat 处理节点心跳（规格书 8.16）。
func (h *Handlers) NodeHeartbeat(c *gin.Context) {
	h.registry().HeartbeatHandler(c)
}

// NodeConfig 拉取配置，支持增量（规格书 5.4）。
func (h *Handlers) NodeConfig(c *gin.Context) {
	h.registry().ConfigHandler(c)
}

// NodePushConfig 是面板侧主动触发一次配置推送的入口（规格书 5.4 的即时推送）。
func (h *Handlers) NodePushConfig(c *gin.Context) {
	h.registry().PushConfigHandler(c)
}

// NodeReport 接收节点上报的同步结果与流量（规格书 8.16）。
func (h *Handlers) NodeReport(c *gin.Context) {
	h.registry().ReportHandler(c)
}

// NodeTasks 返回该节点的待执行任务（规格书 8.16）。
func (h *Handlers) NodeTasks(c *gin.Context) {
	h.registry().TasksHandler(c)
}

// NodeTaskResult 接收任务执行结果（规格书 8.16）。
func (h *Handlers) NodeTaskResult(c *gin.Context) {
	h.registry().TaskResultHandler(c)
}

// InstallScript 返回节点一键安装脚本（规格书 5.1）。
//
// 无需认证：脚本不含敏感信息，token 由查询参数传入。
func (h *Handlers) InstallScript(c *gin.Context) {
	h.registry().InstallScriptHandler(c)
}

// UninstallScript 返回节点卸载脚本（规格书 5.5）。
func (h *Handlers) UninstallScript(c *gin.Context) {
	h.registry().UninstallScriptHandler(c)
}

// NodeBinary 提供节点客户端二进制下载（规格书 8.16）。
//
// 本仓库不产出预编译的节点二进制，因此明确返回 404 与可读说明，
// 而不是返回空文件让安装脚本静默失败。
func (h *Handlers) NodeBinary(c *gin.Context) {
	arch := c.Param("arch")
	switch arch {
	case "amd64", "amd64v3", "arm64":
	default:
		badRequest(c, "arch", "不支持的架构，可用：amd64、amd64v3、arm64")
		return
	}

	response.Fail(c, response.New(response.CodeNotFound,
		"当前部署未内置节点客户端二进制（arch="+arch+"）。"+
			"请自行编译节点客户端后放到静态目录，或改用离线部署：本地构建后 scp 到 /opt/openroute/"))
}
