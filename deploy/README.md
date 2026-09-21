# 部署说明

把 OpenRoute 部署到远端服务器。凭据不写进脚本，而是通过命令行参数传入。

## 目标环境

| 项目 | 值 |
|---|---|
| 服务器 | `38.76.177.65`（Debian 12, amd64, 2 核 3.6G） |
| 域名 | `x.aarcx.com`（A 记录已指向该 IP） |
| 面板地址 | `https://x.aarcx.com/` |
| 证书 | `/root/cert/x.aarcx.com/{fullchain.pem,privkey.pem}`（acme.sh 管理） |
| 数据目录 | `/opt/openroute/`（`data.db` / `config.yml` / `logs/`） |
| 服务 | `systemctl {status\|restart\|stop} openroute`（已开机自启） |
| 管理员凭据 | 服务器上的 `/etc/openroute.env`（权限 600） |

## 为什么用自研的 sshexec

本机有两个现成的远程执行方案，但在**这台机器上都不可用**：

- **Windows 自带的 `ssh.exe`**：不支持用参数传密码
  （`-pw` 是 PuTTY 的语法，`ssh.exe` 会报 `Bad port 'w'`），
  只能交互输入，无法脚本化。
- **WSL + sshpass**：本机 WSL 的网络模式（`Mirrored`）初始化失败，
  WSL 内 `Network is unreachable`，连不上外网。

因此仓库里带了一个极小的 Go 工具 `sshexec`（基于 `x/crypto/ssh`），
Go 的网络栈在本机工作正常，可以用参数提供密码，也支持密钥。

```bash
cd ..                      # 到仓库根目录
cd sshexec && go build -o sshexec.exe .
```

## 部署流程

四个步骤必须按顺序执行（后一步依赖前一步的结果）。

### 1. 交叉编译并打包

```powershell
cd openroute
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -trimpath -ldflags "-s -w -X 'github.com/openroute/openroute/internal/app.BuildStamp=20260921'" -o ..\deploy\openroute .
Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED

cd ..
tar -czf deploy\openroute-deploy.tar.gz -C deploy openroute install.sh -C openroute public
```

> 前端要先构建：`cd openroute/frontend && node node_modules/vite/bin/vite.js build`
> （产物输出到 `openroute/public`）。

### 2. 上传

```powershell
$SS = "$PWD\sshexec\sshexec.exe"
& $SS run    -host 38.76.177.65 -user root -pass '密码' -cmd "rm -rf /tmp/openroute-deploy && mkdir -p /tmp/openroute-deploy"
& $SS upload -host 38.76.177.65 -user root -pass '密码' -local "$PWD\deploy\openroute-deploy.tar.gz" -remote /tmp/openroute-deploy.tar.gz
& $SS run    -host 38.76.177.65 -user root -pass '密码' -cmd "tar -xzf /tmp/openroute-deploy.tar.gz -C /tmp/openroute-deploy"
```

### 3. 安装

```powershell
& $SS exec -host 38.76.177.65 -user root -pass '密码' -script "$PWD\deploy\install.sh"
```

### 4. 验证

```powershell
# 确认前端 bundle 已更新
curl.exe -s https://x.aarcx.com/ | Select-String "index-"

# 浏览器级回归（真实登录 + 跨过故障点持续观察）
node scripts\browser\dashboard-watch.mjs https://x.aarcx.com admin '密码'
node scripts\browser\smoke.mjs https://x.aarcx.com admin '密码'
```

## install.sh 做了什么

在服务器上以 root 执行，**幂等**（可重复运行）：

1. **前置检查** —— 二进制是 Linux ELF、证书存在且未过期
2. **管理员凭据** —— 首次生成随机密码写入 `/etc/openroute.env`（已存在则复用）
3. **释放文件** —— 二进制装到 `/opt/openroute/`，前端覆盖 `public/`
4. **`config.yml`** —— 监听 443 + TLS、随机 `secret-key`、权限 600（**已存在则保留**）
5. **systemd 服务** —— 开机自启、`LimitNOFILE=1048576`、基础加固
6. **初始化数据库** —— 先停服务（否则 443 被占用会失败）→ 建表建管理员 → 拉回服务
7. **自检** —— 端口、本地 HTTPS、证书链
8. **证书续期钩子** —— acme.sh 续期后自动重启面板加载新证书

重复执行**不会**覆盖已有的 `config.yml` 与 `data.db`，也不会重置已有密码。

## 重要说明

### 面板直接终止 TLS

面板监听 443 并自行加载证书（`tls-cert` / `tls-key`），**没有 Nginx**。
少一层组件，代价是要保证证书路径对面板进程可读。

节点对接命令里的面板地址由面板依据请求自动推导：
通过 `https://x.aarcx.com` 访问时，生成的安装命令就是 https 的。

### 证书续期

证书由 **acme.sh** 管理（standalone 模式，用 80 端口做 HTTP-01 校验），
cron 每 6 小时检查一次。安装脚本已挂 `--reloadcmd`：续期成功后自动重启 `openroute`，
否则面板会一直用内存里的旧证书，浏览器仍报过期。

**80 端口平时没有进程监听，属正常现象** —— acme.sh 只在续期的几秒内临时占用它。

```bash
# 手工续期（建议在到期前一个月手工跑一次确认可行）
/root/.acme.sh/acme.sh --renew -d x.aarcx.com --force
```

### 与 x-ui 共存

服务器上运行着 x-ui（`x-ui.service`，监听 2096 等端口），本部署**未改动它**。

## 文件

| 文件 | 用途 |
|---|---|
| `install.sh` | 服务器端安装脚本（上面第 3 步执行的就是它） |
| `openroute` | 交叉编译出的 Linux/amd64 二进制（构建产物） |
| `openroute-deploy.tar.gz` | 部署包：二进制 + `install.sh` + 前端产物 |
| `../sshexec/` | 自研 SSH 执行/上传工具（见上文「为什么用自研的 sshexec」） |
| `../scripts/browser/` | 浏览器级回归测试（真实渲染校验） |

## 故障排查

```bash
# 服务状态与日志
systemctl status openroute
journalctl -fu openroute

# 面板自检（配置、数据库、端口、目录权限）
/opt/openroute/openroute -check

# 端口占用
ss -tlnp | grep ':443 '

# 配置与凭据
cat /opt/openroute/config.yml
cat /etc/openroute.env

# 忘记密码
systemctl stop openroute
cd /opt/openroute && ./openroute -reset-password
systemctl start openroute
```

### 页面白屏

先看浏览器控制台（F12）的报错。前端已内置错误边界：
渲染异常时会显示可读的错误与组件调用栈，而不是一片空白。

最常见的成因是**前端类型与后端字段口径不一致**（例如把统计周期对象当数字渲染）。
排查方式是对照真实响应校准类型：

```bash
bash openroute/scripts/dump_api.sh https://x.aarcx.com admin '密码'
```
