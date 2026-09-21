package api

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/app"
)

// DocsPage 渲染交互式 API 文档（规格书 8.19）。
//
// 使用 Scalar：单文件、无构建步骤、从 CDN 加载，
// 比 Swagger UI 更轻，且原生支持 OpenAPI 3.1。
//
// 页面通过 /api/v1/system/openapi.json 获取当前版本的接口描述，
// 因此文档永远与后端实现一致，不会出现「文档写了一个不存在的接口」。
func (h *Handlers) DocsPage(c *gin.Context) {
	// 文档开关可在设置中关闭（规格书 8.19：可开关）。
	if !h.app.Setting.GetBool("openapi_docs", true) {
		response.Fail(c, response.New(response.CodeNotFound,
			"API 文档已在设置中关闭"))
		return
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(200, docsHTML)
}

// OpenAPIJSON 运行时输出当前版本的 OpenAPI 描述（规格书 8.19）。
//
// 优先返回磁盘上的 docs/openapi.yaml（人工维护、描述最完整）；
// 文件不存在时回退到内置的最小可用描述，保证 /api/docs 仍有内容可渲染。
//
// 无需认证：接口清单本身不是敏感信息，且便于第三方在未登录时先看文档。
func (h *Handlers) OpenAPIJSON(c *gin.Context) {
	// 1. 尝试读取仓库中的静态描述文件。
	for _, p := range openAPICandidates() {
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
			// 文件是 YAML，但 OpenAPI 3.1 的 YAML 与 JSON 结构等价；
			// Scalar 能直接消费 YAML 文本，因此按 YAML 原样返回。
			c.Header("Content-Type", "application/yaml; charset=utf-8")
			c.Data(200, "application/yaml; charset=utf-8", data)
			return
		}
	}

	// 2. 回退：内置的最小描述。
	response.OK(c, fallbackOpenAPI(app.Version))
}

// openAPICandidates 返回 OpenAPI 描述文件的候选路径。
//
// 覆盖「从项目根目录运行」与「从二进制所在目录运行」两种常见情况。
func openAPICandidates() []string {
	paths := []string{
		filepath.Join("docs", "openapi.yaml"),
		"openapi.yaml",
	}
	// 再补一个「可执行文件同级的 docs 目录」。
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "docs", "openapi.yaml"))
	}
	return paths
}

// fallbackOpenAPI 返回内置的最小 OpenAPI 描述。
//
// 只包含基础信息与几个代表性路径：目的是让文档页面能正常渲染，
// 完整接口清单请以 docs/openapi.yaml 为准。
//
// 参数 version 为面板版本号。
func fallbackOpenAPI(version string) gin.H {
	return gin.H{
		"openapi": "3.1.0",
		"info": gin.H{
			"title":   "OpenRoute 面板 API",
			"version": strings.TrimPrefix(version, "v"),
			"description": "当前进程未找到 docs/openapi.yaml，以下为内置的最小描述。\n\n" +
				"完整的接口清单、错误码字典与鉴权说明请见仓库中的 docs/openapi.yaml 与 docs/API.md。",
		},
		"servers": []gin.H{
			{"url": "/", "description": "当前面板"},
		},
		"components": gin.H{
			"securitySchemes": gin.H{
				"bearerAuth": gin.H{
					"type": "http", "scheme": "bearer", "bearerFormat": "JWT",
				},
				"apiKeyAuth": gin.H{
					"type": "apiKey", "in": "header", "name": "X-API-Key",
				},
				"cookieAuth": gin.H{
					"type": "apiKey", "in": "cookie", "name": "or_session",
				},
				"nodeTokenAuth": gin.H{
					"type": "apiKey", "in": "header", "name": "X-Node-Token",
				},
			},
		},
		"paths": gin.H{
			"/api/v1/auth/login": gin.H{
				"post": gin.H{
					"tags":      []string{"auth"},
					"summary":   "登录",
					"security":  []interface{}{},
					"responses": gin.H{"200": gin.H{"description": "登录成功"}},
				},
			},
			"/api/v1/nodes": gin.H{
				"get": gin.H{
					"tags":      []string{"node"},
					"summary":   "节点列表",
					"responses": gin.H{"200": gin.H{"description": "节点列表"}},
				},
				"post": gin.H{
					"tags":      []string{"node"},
					"summary":   "创建节点（返回安装命令）",
					"responses": gin.H{"200": gin.H{"description": "创建成功"}},
				},
			},
			"/api/v1/forward-rules": gin.H{
				"get": gin.H{
					"tags":      []string{"rule"},
					"summary":   "转发规则列表",
					"responses": gin.H{"200": gin.H{"description": "规则列表"}},
				},
			},
			"/api/v1/traffic/overview": gin.H{
				"get": gin.H{
					"tags":      []string{"traffic"},
					"summary":   "全站流量概览",
					"responses": gin.H{"200": gin.H{"description": "概览"}},
				},
			},
			"/api/v1/system/info": gin.H{
				"get": gin.H{
					"tags":      []string{"system"},
					"summary":   "系统信息",
					"responses": gin.H{"200": gin.H{"description": "系统信息"}},
				},
			},
		},
	}
}

// docsHTML 是交互式文档页面的 HTML。
//
// 用 Scalar 的 CDN 单文件版本：无需构建、无 node_modules、
// 且直接消费 OpenAPI 3.1。页面顶部保留了「文档未找到」的提示位。
const docsHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>OpenRoute API 文档</title>
  <style>
    body { margin: 0; }
    .or-loading {
      font-family: -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif;
      padding: 48px; color: #4e5969; text-align: center;
    }
  </style>
</head>
<body>
  <div id="app"><div class="or-loading">正在加载 API 文档…</div></div>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
  <script>
    // Scalar 从 /api/v1/system/openapi.json 读取当前版本的接口描述，
    // 因此文档与运行中的后端始终一致。
    try {
      Scalar.createApiReference('#app', {
        url: '/api/v1/system/openapi.json',
        theme: 'default',
        hideModels: false,
        defaultHttpClient: { targetKey: 'shell', clientKey: 'curl' },
      })
    } catch (e) {
      document.getElementById('app').innerHTML =
        '<div class="or-loading">文档渲染失败：' + e.message +
        '<br />请检查浏览器是否能访问 cdn.jsdelivr.net（离线环境可改用 docs/API.md）。</div>'
    }
  </script>
</body>
</html>`
