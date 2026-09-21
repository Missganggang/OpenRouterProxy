# 节点客户端

仓库的 `cmd/nodeclient` 是与本面板 HTTP 协议配套的节点程序。
面板程序本身以及其它项目的节点二进制都不能代替它。

## 构建和下载

在仓库根目录运行 `./deploy/build.ps1`（PowerShell），构建面板与 Linux
amd64、amd64v3、arm64 三种节点。将 `deploy/node-binaries/` 随面板一起部署到
面板工作目录下的 `node-binaries/`，或通过面板配置 `node-binary-path` 指定目录。

文件布局为 `node-binaries/<arch>/rel_nodeclient`，下载地址为
`/api/node/binary/<arch>`。如需固定版本，额外放置
`node-binaries/<version>/<arch>/rel_nodeclient`，再传 `?version=<version>`。
缺失文件会返回明确的 404；不支持的架构或非法版本参数返回 400。

Linux 下也可直接构建：

```bash
cd openroute
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 \
  go build -trimpath -o node-binaries/amd64/rel_nodeclient ./cmd/nodeclient
```

## 安装与运行

在面板创建节点后，复制该节点的安装命令到目标服务器执行。
公开 `/install.sh` 只包含通用脚本，不会查询或内嵌现有节点密钥。
`-u`、`-t` 参数由目标机上的脚本解析。默认安装目录为
`/opt/openroute-node`，服务为 `openroute-node`，与面板目录隔离。

客户端注册后定期发送心跳、拉取配置并报告应用结果。已下载的配置保存在
节点目录，面板暂时不可达时可以恢复本地配置；配置与密钥属于敏感文件。

```bash
systemctl status openroute-node
journalctl -u openroute-node -n 50 --no-pager
/opt/openroute-node/rel_nodeclient -version
/opt/openroute-node/rel_nodeclient -c /opt/openroute-node/config.yml -check
```

## 当前能力范围

此客户端提供注册、系统指标、心跳、配置同步与基础 direct TCP/UDP 转发。
尚不支持 WS/HTTP/TLS 隧道、入口与出口组之间的隧道协作、反向/链式隧道、
SNI 分流以及远程升级/命令执行。未支持的转发配置会报告失败原因，
不会把规则标成已成功运行。面板中出现某个配置项不代表客户端已经实现它。

系统探针会上报真实网络速率；规则流量增量目前尚未在面板端接入累计落库，
因此规则累计流量图表不能作为本版本的流量核对依据。
