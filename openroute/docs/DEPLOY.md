# OpenRoute 部署文档

本文档覆盖从零部署、升级、备份恢复与常见问题排查。
面向个人自用场景，全部步骤都刻意保持简单：**不需要域名、证书、授权码或外部数据库**。

---

## 1. 部署前提

| 项目 | Nyanpass 要求 | OpenRoute 要求 |
|---|---|---|
| HTTPS | 强制 | **不要求**，用 `http://IP:端口` 直接访问 |
| 域名 | 强制 | **不要求** |
| 授权码 | 强制 | **不要求** |
| 端口 | 仅 443/80 | **任意端口** |
| 数据库 | 需外部服务 | **默认 SQLite 文件**，零运维 |
| 反向代理 | 必须 | **可选** |
| 部署方式 | 必须 docker compose | **单二进制直跑**，docker 可选 |

**运行环境**：Linux（amd64 / arm64）、macOS、Windows 均可。
SQLite 使用纯 Go 驱动，**不需要 CGO、不需要 gcc**，因此单个二进制即可运行。

---

## 2. 从源码构建

### 2.1 构建后端

```bash
# 需要 Go 1.22+（本项目在 1.26 上开发验证）
git clone <仓库地址> openroute && cd openroute
go build -o openroute .
```

交叉编译到 Linux 服务器（在本地开发机执行）：

```bash
# amd64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o openroute-linux-amd64 .
# arm64（如 Oracle ARM 实例、树莓派）
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o openroute-linux-arm64 .
```

> `CGO_ENABLED=0` 是关键：SQLite 走纯 Go 实现，因此不需要为目标平台准备交叉编译工具链。

### 2.2 构建前端

```bash
cd frontend
npm install
npm run build      # 产物输出到 ../public（由 vite.config.ts 指定）
```

构建产物会直接落到后端的 `html-path`（默认 `./public`），
因此**一个端口同时提供 API 与 WebUI**。

---

## 3. 方式一：单二进制（推荐，个人自用）

### 3.1 首次部署

```bash
# 1. 放好文件
mkdir -p /opt/openroute && cd /opt/openroute
# 把 openroute 二进制与 public/ 目录放进来
chmod +x openroute

# 2. 首次启动并创建管理员
MIGRATE=1 ADMIN="admin" ADMIN_PASSWORD="你的密码" ./openroute

# 3. 浏览器访问
#    http://<服务器IP>:18888/
```

首次启动会自动完成：

1. 生成 `config.yml`（含自动生成的随机 `secret-key`）
2. 创建 `data.db` 与全部表结构
3. 写入默认数据（默认用户分组、默认规则组、站点设置）
4. 创建管理员账号

**也可以不传 `ADMIN_PASSWORD`**，此时会进入交互式引导，在终端里输入用户名与密码。

### 3.2 systemd 服务

```ini
# /etc/systemd/system/openroute.service
[Unit]
Description=OpenRoute Panel
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/openroute
ExecStart=/opt/openroute/openroute
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now openroute
systemctl status openroute
journalctl -fu openroute
```

### 3.3 开放防火墙端口

```bash
# 面板端口
ufw allow 18888/tcp
# 转发规则用到的端口段（按需）
ufw allow 8443:8500/tcp
ufw allow 8443:8500/udp
```

---

## 4. 方式二：Docker Compose

```yaml
# docker-compose.yml
services:
  openroute:
    image: openroute/panel:latest     # 或用 build: . 本地构建
    container_name: openroute
    restart: unless-stopped
    ports:
      - "18888:18888"
    volumes:
      - ./data:/app/data
      - ./config.yml:/app/config.yml
      - ./logs:/app/logs
    environment:
      - TZ=Asia/Shanghai
    ulimits:
      nofile:
        soft: 1048576
        hard: 1048576
```

```bash
docker compose up -d
docker compose logs -f
```

---

## 5. 配置 HTTPS（可选）

面板本身不强制 HTTPS。若确实需要，用 Nginx 反代：

```nginx
server {
    listen 443 ssl;
    server_name panel.example.com;

    ssl_certificate     /etc/nginx/certs/fullchain.pem;
    ssl_certificate_key /etc/nginx/certs/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:18888;
        proxy_http_version 1.1;
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host       $host;
        proxy_set_header X-Real-IP  $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 3600s;   # WebSSH 需要长连接
    }
}
```

> **WebSSH 与实时推送依赖 WebSocket**，反代 MUST 保留 `Upgrade` 与 `Connection` 头，
> 否则节点状态不会实时刷新、终端打不开。

---

## 6. 节点部署（一键对接）

1. 面板「节点管理」→「添加节点」，填写名称与角色（入口/出口/双端）。
2. 面板弹出安装命令（带复制按钮与二维码）：

   ```bash
   bash <(curl -fsSL http://<面板地址>/install.sh) -u <面板地址> -t <节点token>
   ```

3. 在目标机器上以 **root** 执行该命令。安装脚本会：
   - 探测系统（Debian 11+ / Ubuntu 22.04+）与架构（amd64 / amd64v3 / arm64）
   - 下载节点客户端到 `/opt/openroute-node/`
   - 写入 `/opt/openroute-node/config.yml`（`base-url`、`token`、`is-outbound` 等）
   - 注册并启用 systemd 服务 `openroute-node`，随后启动
4. 节点启动后会立即注册并上报系统信息，面板页面刷新后显示「在线」。

**客户端需随面板一起构建部署。** 构建方式与当前功能范围见 [NODECLIENT.md](NODECLIENT.md)。

### 6.1 安装脚本环境变量

```bash
S=openroute           # 服务名（默认 openroute），多实例部署时用于区分
OPTIMIZE=1            # 启用内核网络参数优化（BBR、缓冲区、文件句柄）
INSTALL_TOOLS=1       # 安装常用排查工具（iftop、mtr、tcpdump 等）
DISABLE_EXECUTE=1     # 禁用 WebSSH 与远程升级
BIND_INBOUND=0.0.0.0  # 用户规则监听IP；也支持网卡名及逗号分隔的列表
TUNNEL_BIND_INBOUND=10.88.0.1 # 本机隧道监听IP，与用户规则监听分开
TUNNEL_BIND_OUTBOUND_4=10.88.0.1 # 本机建立IPv4隧道的源IP
TUNNEL_INTERFACE=eth1 # Linux上约束隧道使用的网卡，仍需系统路由
COUNT_INTERFACE=eth0  # 指定探针统计流量的网卡
UUID=<唯一值>          # 同一台机器跑多个实例时为每个实例指定唯一值
```

专线参数、继承规则、现有安装的 env.sh 保留行为，以及双机 / NAT / 反向完整示例见
[专线配置指南](PRIVATE_LINES.md)。`--connect-host` 发布本机可达地址，不是填写对端地址。

示例（优化内核 + 指定统计网卡）：

```bash
OPTIMIZE=1 COUNT_INTERFACE=eth0 \
  bash <(curl -fsSL http://1.2.3.4:18888/install.sh) -u http://1.2.3.4:18888 -t nsk_xxx
```

### 6.2 节点运维命令

```bash
# 卸载
bash /opt/openroute-node/openroute.uninstall.sh

# 服务控制
systemctl status  openroute-node
systemctl start   openroute-node
systemctl stop    openroute-node
systemctl restart openroute-node
systemctl enable  openroute-node
systemctl disable openroute-node

# 实时日志
journalctl -fu openroute-node

# 查看版本
/opt/openroute-node/rel_nodeclient -h

# 检查节点本地配置
/opt/openroute-node/rel_nodeclient -c /opt/openroute-node/config.yml -check
```

---

## 7. 从 Nyanpass 迁移

> **迁移前务必先备份目标库。** 迁移工具也会自动备份，但双保险更稳妥。

```bash
# 1. 备份当前数据（可选但强烈建议）
./openroute -backup ./backup-before-migrate.zip

# 2. 预检：只读扫描，不写入任何数据
./openroute -migrate from=nyanpass,dsn="sqlite3:///path/to/nyanpass/data.db",dry-run=true

# 3. 查看预检报告中的冲突项，确认无误后正式迁移
./openroute -migrate from=nyanpass,dsn="sqlite3:///path/to/nyanpass/data.db",dry-run=false

# 4. 若结果不符合预期，回滚该批次
./openroute -migrate rollback=<批次ID>
```

跨数据库转换（SQLite ↔ MySQL ↔ PostgreSQL）：

```bash
./openroute -copy-database "mysql://user:pass@tcp(127.0.0.1:3306)/openroute?charset=utf8mb4&parseTime=true"
# 目标库会被完全覆盖且无法恢复，必须输入 YES 确认
```

**迁移后必做事项**（预检报告中也会提示）：

1. **节点密钥全部重新生成**：`/opt/openroute/config.yml` 里的 `token` 与旧面板不同。
2. **节点客户端不兼容**：必须在每台机器上卸载旧节点并重新安装：

   ```bash
   bash /opt/openroute/openroute.uninstall.sh
   # 然后在面板复制新的安装命令执行
   ```

3. **规则需要重新下发**：迁移后所有规则状态为 `unsynced`，节点重装上线后会自动同步。
4. **大小写敏感差异**：若源库是 MySQL 而目标是 SQLite/PostgreSQL，
   注意名称唯一性由应用层归一化维护，预检报告会列出冲突项。

---

## 8. 备份与恢复

```bash
# 导出全量备份为单个 zip（含 data.db、config.yml、manifest.json）
./openroute -backup ./backup-20260101.zip

# 保留 secret-key（默认脱敏）
./openroute -backup ./backup.zip -with-secret

# 从备份恢复（恢复前自动保存当前数据为 data.db.before-restore）
./openroute -restore ./backup-20260101.zip
```

WebUI 的「迁移 → 备份」页也提供「生成备份 / 下载 / 上传恢复」入口。

---

## 9. 升级流程

```bash
# 1. 备份（务必）
./openroute -backup ./backup-before-upgrade.zip

# 2. 停止服务
systemctl stop openroute

# 3. 替换二进制与前端产物
#    覆盖 openroute 与 public/

# 4. 迁移数据库结构
MIGRATE=1 ./openroute      # 观察输出的结构 diff，确认无误后 Ctrl+C
#    或直接以 MIGRATE=1 启动

# 5. 启动服务
systemctl start openroute
```

> `MIGRATE=1` 会打印结构变更 diff（新增表、新增列、类型变更）。
> **删除列不会自动执行**，只会在 diff 中提示，需要人工处理。

---

## 10. 自检与维护命令

```bash
./openroute -check              # 自检：配置、数据库、端口、目录权限、前端资源
./openroute -reset-password     # 交互式重置管理员密码
./openroute -clean 1            # 清理失效规则（执行前打印清单并要求确认）
./openroute -clean 2            # 重置异常的节点权重
./openroute -v                  # 仅打印版本号
./openroute -h                  # 帮助
```

**忘记密码时**：`./openroute -reset-password`，按提示选择账号并设置新密码。

---

## 11. 常见问题

| 现象 | 原因与处理 |
|---|---|
| 面板打不开 | 检查进程是否在跑（`systemctl status openroute`）、端口是否被占用、防火墙是否放行 |
| 502 / 连接被拒 | 后端未启动；`journalctl -fu openroute` 看日志 |
| 页面空白 | `html-path` 指向错误，或 `public/` 内容不完整；页面会给出明确提示 |
| 登录后立即掉线 | `secret-key` 每次启动都变了（检查 `config.yml` 是否可写） |
| 规则一直「未同步」 | 节点离线；或节点的 `base-url` / `token` 不正确 |
| CPU / 内存占用高 | 规则数量过多、`log-level` 为 debug、开启了全量探针；调整 `log-level` |
| 已用流量一直是 0 | 设备组倍率被设为 0；或流量采集任务被 `disable-cron` 关闭 |
| WebSSH 打不开 | `enable-webssh: false`；或节点设了 `DISABLE_EXECUTE=1`；或反代未放行 WebSocket |
| 迁移后节点全部离线 | 正常现象：节点 token 已重新生成，需卸载旧节点并重新安装 |
| 数据库文件损坏 | 用 `-restore` 恢复最近的备份；SQLite 可用 `.recover` 尝试抢救 |
| 启动报 `offline-node-time` 过低 | 该值最低 20 秒，低于此值会拒绝启动（防止节点状态抖动） |
| 启动报端口冲突 | 换 `config.yml` 的 `listen` 端口，或停掉占用该端口的进程 |

### 11.1 节点侧排查

| 现象 | 排查步骤 |
|---|---|
| 安装失败 | 检查系统版本（Debian 11+ / Ubuntu 22.04+）、架构、是否有 root 权限、是否有外网 |
| 下载中断/重置 | 改用离线部署：本地下载二进制后 `scp` 到 `/opt/openroute/` |
| APT 报错 | 先 `apt update`；检查 `/etc/apt/sources.list`；不依赖 APT 也可运行（二进制是静态编译） |
| 节点离线 | 检查 `systemctl status openroute-node`、`journalctl -fu openroute-node`、防火墙是否放行面板端口 |
| 面板显示离线但节点在跑 | 检查节点能否访问面板（`curl <base-url>`）、时钟是否同步、token 是否被改 |
| 监控页面数据不更新 | 检查 `COUNT_INTERFACE` 是否指向正确网卡 |
| 规则同步延迟 | 检查面板与节点之间的网络 RTT；轮询间隔 20s 属正常 |
| 多实例冲突 | 每实例设置唯一 `UUID`，并用 `S=` 指定不同服务名 |
| 机器 UUID 冲突 | 克隆虚拟机导致；删除 `/opt/openroute/machine-id` 后重启 |
| `too many open files` | 用 `OPTIMIZE=1` 安装，或手动调高 `ulimit -n` 与 systemd 的 `LimitNOFILE` |

---

## 12. 目录结构说明

部署后目录形态：

```
/opt/openroute/
├── openroute              # 主程序（单二进制）
├── config.yml             # 运行时配置（首次启动自动生成）
├── data.db                # SQLite 数据库
├── data.db-wal            # SQLite WAL（运行时自动维护）
├── public/                # 前端构建产物
├── logs/
│   └── openroute.log      # 按天切割的日志
└── backups/               # 命令行与 WebUI 生成的备份
```

**只需备份 `data.db` 与 `config.yml` 两个文件即可完整恢复**，
或用 `-backup` 把它们打包成单个 zip。

---

## 13. 接口与文档

| 内容 | 位置 |
|---|---|
| 交互式 API 文档 | `http://<面板地址>/api/docs` |
| OpenAPI 3.1 描述（运行时） | `GET /api/v1/system/openapi.json` |
| OpenAPI 3.1 描述（静态） | `docs/openapi.yaml` |
| API 使用说明 | `docs/API.md` |
| 错误码字典（运行时） | `GET /api/v1/system/errors` |
| 健康检查 | `GET /api/v1/health`、`GET /api/v1/system/status` |
