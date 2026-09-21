package api

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

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
// Files are built from cmd/nodeclient and shipped beside the panel.
func (h *Handlers) NodeBinary(c *gin.Context) {
	arch := c.Param("arch")
	switch arch {
	case "amd64", "amd64v3", "arm64":
	default:
		badRequest(c, "arch", "不支持的架构，可用：amd64、amd64v3、arm64")
		return
	}

	dir := h.app.Config.NodeBinaryPath
	if dir == "" {
		dir = "./node-binaries"
	}
	version := c.Query("version")
	if version != "" && version != "latest" {
		if !nodeVersionPattern.MatchString(version) {
			badRequest(c, "version", "版本号只能包含字母、数字、点、下划线和短横线，且以字母或数字开头")
			return
		}
		dir = filepath.Join(dir, version)
	}
	filename := filepath.Join(dir, arch, agent.NodeBinaryName)
	f, err := os.Open(filename)
	if err != nil {
		response.Fail(c, response.New(response.CodeNotFound,
			"该架构或版本的节点客户端尚未部署，请重新构建并部署 node-binaries（arch="+arch+"）"))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	var magic [4]byte
	if err != nil || !info.Mode().IsRegular() {
		response.Fail(c, response.New(response.CodeInternal, "节点客户端文件不可读"))
		return
	}
	if _, err = io.ReadFull(f, magic[:]); err != nil || !bytes.Equal(magic[:], []byte{0x7f, 'E', 'L', 'F'}) {
		response.Fail(c, response.New(response.CodeInternal, "节点客户端不是有效的 Linux ELF 文件，请重新部署"))
		return
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		response.Fail(c, response.New(response.CodeInternal, "节点客户端文件不可读"))
		return
	}
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", `attachment; filename="rel_nodeclient"`)
	c.Header("Cache-Control", "no-cache")
	http.ServeContent(c.Writer, c.Request, agent.NodeBinaryName, info.ModTime(), f)
}

var nodeVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
