# OpenRoute 自研转发面板 —— 全量开发话术（AI 编码规格书）

> 本文档是交给 AI 编码助手（或开发团队）的**完整需求与实现规格**。
> 读者对象：负责从零实现 OpenRoute 的开发者 / AI Agent。
> 阅读方式：按章节顺序阅读；第 4 章「数据库」、第 6 章「功能规格」、第 8 章「API」是核心，其余为支撑。
> 版本：v1.0（个人自用精简版）

---

## 0. 术语与阅读约定

| 术语 | 含义 |
|---|---|
| 面板 / 后台（Panel / Backend） | OpenRoute 的服务端程序，Go 编写，对外只提供 HTTP API |
| 前端（Frontend） | 浏览器中运行的 WebUI，TypeScript + React，独立构建产物 |
| 节点 / 节点客户端（Node / NodeClient） | 部署在转发服务器上的 Go 程序，接受面板下发配置并执行转发 |
| 入口（Inbound） | 面向用户的一侧：用户的代理软件连到入口机器的端口 |
| 出口（Outbound） | 面向目标的一侧：数据从出口机器发往真正的目标地址 |
| 单端（Single-ended） | 入口与出口是同一台机器，没有隧道 |
| 隧道（Tunnel） | 入口与出口之间的自研 TCP 加密复用连接 |
| 隧道协议 | `ws` / `http` / `tls` 三种传输伪装方式 |
| 设备组（Device Group） | 一组配置相同的节点机器的集合，分入口组、出口组 |
| 转发规则（Forward Rule） | 一条「入口监听端口 → 目标地址」的转发定义 |
| 倍率（Multiplier） | 流量计费系数，用户实际流量 = 入口流量×入口倍率 + 出口流量×出口倍率 |
| 探针（Probe） | 节点上的状态采集与上报组件，含 WebSSH |

**本文档中的 MUST / SHOULD / MAY** 采用 RFC 2119 语义：MUST = 必须实现，SHOULD = 强烈建议，MAY = 可选。

---

## 1. 项目总览

### 1.1 一句话定位

OpenRoute 是一个**自研、可私有化、单人可维护**的 TCP/UDP 转发管理面板。它在功能面上复刻 Nyanpass 的转发能力（入口/出口设备组、隧道协议、负载均衡、故障转移、SNI 分流、倍率流量统计、节点探针与 WebSSH），同时**砍掉全部商业化模块**（支付、商城、订单、套餐、授权、域名绑定、多租户计费），并且**补齐 Nyanpass 缺失的东西**：一份稳定、版本化、可对接的公开 API。

### 1.2 设计目标（按优先级排序）

1. **能一个人跑起来**：单二进制 + SQLite + 一个配置文件，`./openroute` 双击即用，不依赖外部数据库、不依赖域名、不依赖 HTTPS 证书、不依赖任何第三方授权服务。
2. **功能不缺席**：Nyanpass 的转发相关能力全部具备，操作方式与概念命名尽量保持一致，降低迁移与学习成本。
3. **数据可迁移**：内置迁移工具，支持「Nyanpass → OpenRoute」与「OpenRoute → OpenRoute（SQLite/MySQL/PostgreSQL 互转）」两条路径，且带 dry-run、校验、回滚。
4. **接口稳定**：所有面板能力都有对应的 REST API，带版本前缀 `/api/v1/`、统一的响应包体、API Token 与 Session 双认证、错误码字典、OpenAPI 描述文件。
5. **少即是多**：不引入消息队列、不引入 Redis、不引入微服务。单进程内用 goroutine + channel 完成全部后台任务。

### 1.3 非目标（明确不做）

以下内容**明确不在 OpenRoute 范围内**，实现时不要自作主张加入：

- 任何支付网关对接（Epay / Epusdt / Bepusdt / AceTaffy / TokenPay / Cryptomus / USDT 等一律不做）
- 商城、商品、套餐、订单、自动续费、余额、充值、优惠券、每日/每月充值统计
- 授权码（License Key）、授权域名绑定、授权到期限制、云端在线校验
- 云 SaaS 托管模式、CNAME 委派、多租户隔离
- WHMCS 对接、邀请注册、分销/代理/返佣
- 强制 HTTPS、强制绑定域名、强制备案

> **实现约束**：如果 AI 在生成代码时遇到「Nyanpass 文档提到支付/授权」的内容，一律跳过，不要生成占位代码，不要生成 TODO 注释，直接不实现。

### 1.4 与 Nyanpass 的功能对照表

| Nyanpass 功能 | OpenRoute | 说明 |
|---|---|---|
| 用户管理 | ✅ 保留（简化） | 保留用户表与用户级限制；去掉注册/充值/到期计费 |
| 用户分组 / 服务器分组 | ✅ 保留 | 用于规则可见性与限速策略 |
| 流量 / 速度 / IP 数量限制 | ✅ 保留 | 入口侧强制，规则级与用户级叠加 |
| 规则管理（批量操作） | ✅ 保留 | 保留旧文本格式与新版 JSON 导入导出 |
| 规则分组 | ✅ 保留 | |
| 节点服务器管理（实时状态 / WebSSH） | ✅ 保留 | WebSSH 默认开启，可用环境变量关闭 |
| 节点服务器分组 | ✅ 保留 | |
| 规则级 / 用户级 / 全站流量统计 | ✅ 保留 | 去掉「充值统计」，保留流量统计 |
| 一键对接节点服务器 | ✅ 保留 | 面板生成一条安装命令，粘贴到节点执行 |
| 商城 / 订单 / 套餐 / 自动续费 | ❌ 移除 | |
| EPAY / USDT 充值 | ❌ 移除 | |
| 每日 / 每月全站充值统计 | ❌ 移除 | 保留流量维度统计 |
| 转发协议 ws / http / tls | ✅ 保留 | 完整保留，含 chfp 指纹 |
| 出口机器无需域名（默认公网 IP） | ✅ 保留 | |
| 指定内网 IP 实现双机专线 | ✅ 保留 | |
| 绕过隧道直接转发（入口直出） | ✅ 保留 | |
| 基于连接数的出口负载均衡 | ✅ 保留 | |
| 出口自动故障转移 | ✅ 保留 | |
| 多目标地址负载均衡 / 目标故障转移 | ✅ 保留 | |
| 负载均衡权重 `default-weight` | ✅ 保留 | |
| 反向隧道（出口→入口） | ✅ 保留 | 含 `--rev-port`、`reverse_group` |
| 链式出口（2~3 跳） | ✅ 保留 | 入口到第一跳默认开 Mux |
| SNI 分流（主/子规则） | ✅ 保留 | 含整流选项 |
| 原生 UDP / UDP over TCP | ✅ 保留 | |
| 节点探针（nezha 定制版） | ✅ 保留（自研替代） | 自研采集器，功能对齐 |
| 个性化主题 | ✅ 保留（经典 + 透明） | |
| 授权密钥找回 | ❌ 移除 | 无授权体系 |
| 稳定的公开 API 与文档 | ⭐ 新增 | Nyanpass 明确没有，OpenRoute 必做 |
| 配置版本化与回滚 | ⭐ 新增 | |
| 规则变更审计与一键回滚 | ⭐ 新增 | |
| 流量阈值告警 / Webhook | ⭐ 新增 | |
| 节点批量操作与健康度评分 | ⭐ 新增 | |
| 配置漂移检测 | ⭐ 新增 | 面板期望配置 vs 节点实际配置 |
| 单文件备份 / 恢复 | ⭐ 新增 | 一键导出全部数据为单文件 |

---

## 2. 技术栈与工程约定

### 2.1 技术选型

| 层 | 选型 | 理由 |
|---|---|---|
| 后端语言 | Go 1.22+ | 与节点客户端同语言，可共享类型定义；交叉编译出单二进制 |
| HTTP 框架 | Gin | 生态成熟，中间件齐全 |
| ORM | Gorm v2 | 支持 SQLite / MySQL / PostgreSQL 三方言；Nyanpass 同款，迁移逻辑可参考 |
| 数据库 | SQLite（默认）/ MySQL / PostgreSQL | 个人自用默认 SQLite，零运维；企业可切 MySQL/PG |
| 配置 | YAML（`config.yml`） | 与 Nyanpass 一致，便于用户迁移 |
| 定时任务 | `robfig/cron` 或原生 ticker | 不要引入分布式调度 |
| 日志 | `zap` 或 `logrus` + lumberjack 切割 | 分级、可切割、可输出 JSON |
| 前端框架 | TypeScript 5 + Vite 5 + React 18 | |
| UI 库 | Ant Design 5 | 表格/表单/图表开箱即用 |
| 状态管理 | Zustand（轻量）或 Redux Toolkit | 避免过度设计 |
| 请求层 | Axios + 统一拦截器 | 自动附带 Token、统一错误提示 |
| 图表 | `@ant-design/charts` 或 ECharts | 流量曲线、节点负载 |
| 节点客户端语言 | Go | 单二进制，systemd 托管 |
| 实时推送 | 面板→节点：gRPC 或 HTTP/2 长连；节点→面板：HTTP 上报 + 轮询兜底 | 见 5.4 |
| 协议描述 | OpenAPI 3.1（`openapi.yaml`） | 供第三方对接 |

### 2.2 后端目录结构（MUST 照此组织）

```
openroute/
├── main.go                     # 入口：解析命令行、初始化、启动
├── config.yml                  # 运行时配置（首次启动自动生成默认值）
├── config.yml.example          # 带注释的示例
├── go.mod / go.sum
├── internal/
│   ├── config/                 # 配置加载、默认值、校验
│   │   ├── config.go
│   │   └── default.go
│   ├── model/                  # Gorm 模型（数据库表结构）
│   │   ├── user.go
│   │   ├── node.go
│   │   ├── device_group.go
│   │   ├── forward_rule.go
│   │   ├── traffic.go
│   │   ├── probe.go
│   │   └── settings.go
│   ├── database/               # 连接、方言适配、自动迁移、事务封装
│   │   ├── db.go
│   │   ├── dialect.go
│   │   └── tx.go
│   ├── api/                    # HTTP 层
│   │   ├── router.go           # 路由注册总表
│   │   ├── middleware/         # 认证、鉴权、限流、审计、CORS、日志
│   │   ├── response/           # 统一响应体与错误码
│   │   └── handler/            # 各资源 handler
│   │       ├── auth.go
│   │       ├── node.go
│   │       ├── node_group.go
│   │       ├── device_group.go
│   │       ├── forward_rule.go
│   │       ├── rule_group.go
│   │       ├── user.go
│   │       ├── traffic.go
│   │       ├── probe.go
│   │       ├── terminal.go     # WebSSH
│   │       ├── settings.go
│   │       ├── migration.go
│   │       └── system.go
│   ├── service/                # 业务逻辑（handler 只做参数校验与调用）
│   │   ├── node.go
│   │   ├── rule.go
│   │   ├── sync.go             # 规则下发与同步状态机
│   │   ├── balance.go          # 负载均衡与故障转移编排
│   │   ├── traffic.go
│   │   ├── limit.go
│   │   ├── probe.go
│   │   └── auth.go
│   ├── agent/                  # 节点通信服务端
│   │   ├── register.go         # 一键对接、节点注册
│   │   ├── heartbeat.go        # 心跳与存活判定
│   │   ├── push.go             # 配置推送
│   │   └── protocol.go         # 与节点约定的消息结构
│   ├── migrate/                # 迁移子系统（见第 7 章）
│   │   ├── router.go
│   │   ├── source_nyanpass.go
│   │   ├── source_openroute.go
│   │   └── transform.go
│   ├── job/                    # 定时任务
│   │   ├── sync_rule.go
│   │   ├── collect_traffic.go
│   │   ├── cleanup.go
│   │   ├── snapshot.go
│   │   └── alert.go
│   └── util/
│       ├── crypto.go           # 密码哈希、Token 生成、节点密钥
│       ├── ip.go               # IP/CIDR/后缀匹配
│       ├── json.go
│       └── validate.go
├── public/                     # 前端构建产物（html-path 指向此处）
├── scripts/
│   ├── install_node.sh         # 节点一键安装脚本
│   ├── uninstall_node.sh
│   └── docker-compose.yml
└── docs/
    ├── API.md
    ├── openapi.yaml
    └── DEPLOY.md
```

### 2.3 编码约定

- **分层纪律**：`handler` 禁止直接操作 Gorm（除极简查询外），业务写 `service`。
- **错误处理**：业务错误使用自定义 `AppError{Code int, Msg string, HTTPStatus int}`，由统一中间件转成响应体。禁止 `panic` 透传到 HTTP 层。
- **上下文**：所有 service 方法第一个参数为 `ctx context.Context`。
- **事务**：跨表写入（如「删规则 + 清流量 + 写审计」）MUST 放在一个事务里。
- **命名**：数据库表名复数蛇形（`forward_rules`），Go 字段大驼峰，JSON 字段小驼峰。
- **注释**：导出函数 MUST 有中文注释，说明用途、参数、返回值。
- **测试**：`service` 层核心逻辑（限速计算、倍率计算、故障转移判定、迁移字段映射）MUST 有单测。

---

## 3. 后端规格：配置与启动

### 3.1 `config.yml` 完整字段

首次启动若文件不存在，MUST 自动写入下面这份默认配置（个人自用版，开箱即用）：

```yaml
# ── 数据存储 ───────────────────────────────────────────────
# 默认 SQLite，文件与本程序同目录，无需任何外部依赖
database-path: sqlite3://data.db
# MySQL 示例（如需切换）：
# database-path: "mysql://user:pass@tcp(127.0.0.1:3306)/openroute?charset=utf8mb4&parseTime=true"
# PostgreSQL 示例：
# database-path: "postgres://host=localhost user=gorm password=gorm dbname=gorm port=5432 sslmode=disable TimeZone=Asia/Shanghai"
max-open-connection: 100
max-idle-connection: 5

# ── 监听 ──────────────────────────────────────────────────
# 个人自用可直接监听 0.0.0.0，用 IP + 非标准端口访问，无需域名与 HTTPS
listen: 0.0.0.0:18888
# 若自行配置了证书可开启（非必须）
# tls-cert: ./some.crt
# tls-key: ./some.key

# ── 前端静态资源 ──────────────────────────────────────────
html-path: ./public

# ── 面板密钥（首次启动自动生成随机值，请勿外泄）─────────────
# 用于签发 Session/JWT、加密节点通信凭证
secret-key: ""

# ── 节点存活判定 ──────────────────────────────────────────
# 心跳间隔（秒）；节点每 N 秒上报一次
heartbeat-interval: 10
# 超过多少秒未上报视为离线（最低 20）
offline-node-time: 20
# 离线节点数据保留时长（秒），超期清理（最低 600）
offline-node-retention-time: 86400

# ── 接口限流 ──────────────────────────────────────────────
user-rate-limit:
  rate: 5
  limit: 5
default-rate-limit:
  rate: 5
  limit: 5

# ── 行为开关 ──────────────────────────────────────────────
disable-gzip: false
disable-queue: false
# 关闭全部定时任务（调试用）
disable-cron: false

# ── 日志 ──────────────────────────────────────────────────
log-level: info          # debug | info | warn | error
log-path: ./logs         # 目录，按天切割
log-keep-days: 14

# ── 流量统计 ──────────────────────────────────────────────
# 流量落库聚合间隔（秒）
traffic-collect-interval: 60
# 明细采样保留天数（聚合数据永久保留）
traffic-detail-keep-days: 30

# ── 探针 ──────────────────────────────────────────────────
enable-probe: true
# 探针数据保留天数
probe-keep-days: 7

# ── WebSSH ────────────────────────────────────────────────
enable-webssh: true
```

**配置校验规则（MUST 实现）**：
- `offline-node-time` < 20 时报错退出，并提示最低值。
- `offline-node-retention-time` < 600 时报错退出。
- `database-path` 无法连接时，打印可读错误（含方言、地址、错误详情），退出码 1。
- `listen` 端口被占用时，明确提示端口冲突，退出码 1。
- `secret-key` 为空时自动生成 32 字节随机值并写回配置文件。
- 任何配置项缺失时使用默认值，**不报错**（容错优先，个人自用场景）。

### 3.2 命令行接口（CLI）

后端 MUST 支持以下命令行参数（与 Nyanpass 习惯保持一致，便于老用户上手）：

| 命令 | 作用 | 备注 |
|---|---|---|
| `./openroute` | 正常启动 | |
| `./openroute -h` | 打印帮助与当前版本 | 启动失败排查第一步 |
| `./openroute -v` | 仅打印版本号后退出 | 便于脚本读取 |
| `./openroute -reset-password` | 交互式重置管理员密码 | 忘记密码时用 |
| `./openroute -clean 1` | 清理失效规则（目标为空、端口冲突、节点已删除） | 执行前打印将删除的规则清单并要求确认 |
| `./openroute -clean 2` | 清理节点负载权重（重置异常权重为默认值） | |
| `./openroute -copy-database "目标 database-path"` | 迁移/转换数据库 | **目标库会被完全覆盖且不可恢复**，必须二次确认，见 7.3 |
| `./openroute -migrate from=nyanpass,dsn=...,dry-run=true` | 从其它面板迁移数据 | 见第 7 章 |
| `./openroute -backup ./backup.zip` | 导出全量备份为单文件 | 新增 |
| `./openroute -restore ./backup.zip` | 从备份文件恢复 | 新增，恢复前自动备份当前数据 |
| `./openroute -check` | 自检：配置、数据库、端口、目录权限、节点通信 | 新增 |

**首次初始化（全新部署）**：

```bash
# 方式 A：环境变量创建管理员（推荐，与 Nyanpass 一致）
MIGRATE=1 ADMIN="admin" ./openroute

# 方式 B：不带环境变量首次启动，进入交互式引导
./openroute
# 终端提示：请输入管理员用户名 / 密码 / 确认密码，创建完成后自动继续启动
```

**升级数据库结构**：

```bash
MIGRATE=1 ./openroute    # 执行自动迁移后正常启动
```

### 3.3 启动流程（MUST 严格按序）

```
1. 解析命令行参数；若为 -h/-v/-reset-password 等立即执行并退出
2. 加载 config.yml（不存在则写默认并提示）
3. 初始化日志（按配置的 level / path）
4. 打印横幅：版本、监听地址、数据库方言、数据目录、WebUI 地址
5. 连接数据库；失败则打印可读错误并退出码 1
6. 执行 Gorm AutoMigrate（仅当 MIGRATE=1 或表结构缺失时；结构变化需打印 diff）
7. 若用户表为空 → 读取 ADMIN 环境变量或进入交互式创建管理员
8. 启动后台任务调度器（job 包）
9. 启动节点通信服务（agent 包）
10. 启动 HTTP 服务，注册路由与静态资源
11. 等待 SIGINT/SIGTERM，收到后优雅关闭：
    - 停止接受新连接
    - 等待在途请求（超时 10s）
    - 持久化节点状态与流量缓存
    - 退出
```

**启动横幅示例（MUST 打印）**：

```
  ___                  ___      _       
 / _ \ _ __   ___ _ __| _ \ ___| |_ ___ 
| | | | '_ \ / _ \ '_ \   // _ \ __/ _ \
| |_| | |_) |  __/ | | | |_\ \___/\__\___|
 \___/| .__/ \___|_| |_|___/\___||__/\___|
      |_|        OpenRoute v1.0.0

版本      : v1.0.0 (build 20260101)
数据库    : sqlite3 (./data.db)
监听地址  : 0.0.0.0:18888
WebUI     : http://127.0.0.1:18888/
日志目录  : ./logs
节点通信  : 已启动（心跳 10s，离线判定 20s）
后台任务  : 8 个已调度
```

---

## 4. 数据库规格

### 4.1 跨方言约定（MUST 严格遵守）

| 项目 | 约定 |
|---|---|
| 表名 | 小写蛇形复数：`forward_rules` |
| 主键 | `id` uint64 自增 |
| 时间字段 | `created_at` / `updated_at`，统一 UTC 存储，前端按本地时区展示 |
| JSON 字段 | SQLite/PG 用 `TEXT`，MySQL 用 `JSON` 或 `LONGTEXT`；Gorm 层用自定义 `JSONField` 类型统一读写 |
| 布尔字段 | 统一 `bool`；MySQL 存 `TINYINT(1)` |
| 大整数 | 流量用 `BIGINT`，单位 **字节** |
| 索引 | 所有外键字段、所有查询条件字段 MUST 建索引 |
| 外键约束 | **不使用数据库级外键**，由应用层维护一致性（便于迁移与分库） |

**大小写敏感警告（迁移必读）**：

- MySQL（InnoDB）默认**不区分**大小写；PostgreSQL 与 SQLite **区分**大小写。
- 影响面：用户名唯一性、节点名称匹配、规则名去重、按名称查询。
- **实现要求**：所有「按名称查重」逻辑 MUST 在应用层做 `strings.EqualFold` 归一化比较，不要依赖数据库排序规则。迁移工具 MUST 扫描并报告跨库迁移后会产生的名称冲突（见 7.4）。

### 4.2 表结构定义

#### 4.2.1 `users` —— 用户（个人自用版角色收敛为 admin / user）

```go
type User struct {
    ID            uint64    `gorm:"primaryKey"`
    Username      string    `gorm:"size:64;uniqueIndex;not null"`   // 登录名
    PasswordHash  string    `gorm:"size:128;not null"`              // bcrypt
    Nickname      string    `gorm:"size:64"`
    Role          string    `gorm:"size:16;default:user"`           // admin | user
    Status        int       `gorm:"default:1"`                      // 1 启用 0 禁用
    GroupID       uint64    `gorm:"index"`                          // 用户分组
    Token         string    `gorm:"size:64;uniqueIndex"`            // 用户级订阅/API Token

    // 流量与限制
    TrafficUsed   int64     `gorm:"default:0"`     // 已用流量（字节），按倍率折算后
    TrafficLimit  int64     `gorm:"default:0"`     // 流量上限，0 = 不限
    SpeedLimit    int64     `gorm:"default:0"`     // 限速 byte/s，0 = 不限
    IPLimit       int       `gorm:"default:0"`     // 同时在线 IP 数上限，0 = 不限
    DeviceLimit   int       `gorm:"default:0"`     // 同时在线设备数上限，0 = 不限
    ConnLimit     int       `gorm:"default:0"`     // 并发连接数上限，0 = 不限

    ExpireAt      *time.Time                        // 有效期，nil = 永久（个人自用默认 nil）
    LastLoginAt   *time.Time
    LastLoginIP   string    `gorm:"size:64"`
    Remark        string    `gorm:"size:255"`

    CreatedAt     time.Time
    UpdatedAt     time.Time
}
```

> **说明**：支付系统移除后，`users` 表只服务于「自己 + 极少数共同使用的账号」。没有注册入口、没有余额字段、没有订单关联。管理员在面板中手工建号。保留多用户是为了实现用户级限速、用户级流量统计与规则归属。

#### 4.2.2 `user_groups` —— 用户分组

```go
type UserGroup struct {
    ID          uint64 `gorm:"primaryKey"`
    Name        string `gorm:"size:64;uniqueIndex;not null"`
    // 该分组默认的流量/限速策略（用户未单独设置时继承）
    TrafficLimit int64  `gorm:"default:0"`
    SpeedLimit   int64  `gorm:"default:0"`
    IPLimit      int     `gorm:"default:0"`
    ConnLimit    int     `gorm:"default:0"`
    // 可访问的规则分组白名单，空数组 = 全部可访问
    RuleGroupIDs JSON   `gorm:"type:text"`
    Remark      string  `gorm:"size:255"`
    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

#### 4.2.3 `nodes` —— 节点服务器

```go
type Node struct {
    ID         uint64 `gorm:"primaryKey"`
    Name       string `gorm:"size:64;uniqueIndex;not null"`
    Token      string `gorm:"size:64;uniqueIndex;not null"` // 节点密钥，安装时下发
    // 角色：inbound(入口) / outbound(出口) / both(双端)
    Role       string `gorm:"size:16;default:both"`
    // 网络信息（节点上报覆盖）
    PublicIPv4 string `gorm:"size:64;index"`
    PublicIPv6 string `gorm:"size:64"`
    PrivateIP  string `gorm:"size:64"`
    // 静态连接地址（出口用；为空则用上报的公网 IP）
    ConnectHost string `gorm:"size:255"`
    IsStatic    bool   `gorm:"default:false"` // 对应配置 connect_type == "static"
    // 端口配置
    DirectPort int `gorm:"default:0"`
    WsPort     int `gorm:"default:0"`
    TlsPort    int `gorm:"default:0"`
    UdpPort    int `gorm:"default:0"`
    RevPort    int `gorm:"default:0"`
    // 反向隧道端口（入口侧监听）
    // 分组
    GroupIDs   JSON  `gorm:"type:text"`   // 所属节点分组（多选）
    // 状态
    Online     bool  `gorm:"default:false;index"`
    LastSeen   *time.Time
    Weight     int   `gorm:"default:1"`   // 负载均衡权重
    MaxConn    int   `gorm:"default:0"`   // 本机最大连接数，0 = 不限
    // 系统信息（探针上报）
    OS         string `gorm:"size:64"`
    Arch       string `gorm:"size:32"`
    KernelVer  string `gorm:"size:64"`
    ClientVer  string `gorm:"size:32"`    // 节点客户端版本
    CPUModel   string `gorm:"size:128"`
    CPUCores   int
    MemTotal   int64
    DiskTotal  int64
    BootTime   *time.Time
    // 备注
    Remark     string `gorm:"size:255"`
    CreatedAt  time.Time
    UpdatedAt  time.Time
}
```

#### 4.2.4 `node_groups` —— 节点分组

```go
type NodeGroup struct {
    ID        uint64 `gorm:"primaryKey"`
    Name      string `gorm:"size:64;uniqueIndex;not null"`
    Remark    string `gorm:"size:255"`
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

#### 4.2.5 `device_groups` —— 设备组（入口组 / 出口组）

这是**整个系统最重要的表**。设备组定义「一组机器的共同行为」：网络策略、协议、负载均衡、故障转移、SNI 策略。

```go
type DeviceGroup struct {
    ID       uint64 `gorm:"primaryKey"`
    Name     string `gorm:"size:64;uniqueIndex;not null"`
    Type     string `gorm:"size:16;not null;index"` // inbound | outbound
    // 组成员（节点 ID 数组，有序；出口组的顺序参与负载均衡初始化）
    NodeIDs  JSON `gorm:"type:text"`
    // 组的 JSON 配置，结构见 4.3 / 4.4；前端用表单编辑，后端存原文
    Config   JSON `gorm:"type:text"`
    // 出口组专用：负载均衡策略
    //   least_conn  最少连接数（默认，对齐 Nyanpass）
    //   round_robin 轮询
    //   hash_ip     源 IP 哈希
    //   weighted    按权重比例
    Balance  string `gorm:"size:16;default:least_conn"`
    // 出口组专用：健康检查
    HealthCheckEnable     bool `gorm:"default:true"`
    HealthCheckInterval   int  `gorm:"default:10"`  // 秒
    HealthCheckTimeout    int  `gorm:"default:3"`   // 秒
    HealthCheckFailCount  int  `gorm:"default:3"`   // 连续失败次数后摘除
    HealthCheckSuccCount  int  `gorm:"default:2"`   // 连续成功次数后恢复
    // 故障转移组 ID（出口组专用；本组全挂时转投该组）
    FailoverGroupID uint64 `gorm:"index"`
    Remark    string `gorm:"size:255"`
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

#### 4.2.6 `forward_rules` —— 转发规则

```go
type ForwardRule struct {
    ID         uint64 `gorm:"primaryKey"`
    Name       string `gorm:"size:128;index;not null"`
    // 归属
    UserID      uint64 `gorm:"index"`   // 归属用户；0 = 系统/管理员
    RuleGroupID uint64 `gorm:"index"`   // 规则分组
    // 入口侧
    InboundGroupID  uint64 `gorm:"index;not null"` // 入口设备组
    ListenPort      int    `gorm:"index"`          // 0 = 随机端口（新格式）
    ListenPortEnd   int    `gorm:"default:0"`       // 端口段结束（0 = 单端口）
    // 出口侧
    OutboundGroupID uint64 `gorm:"index"`          // 出口设备组；0 = 单端（入口直出）
    // 目标地址（支持多目标负载均衡）
    Targets    JSON   `gorm:"type:text"` // [{host,port,weight,status}]
    // 倍率
    InboundMultiplier  float64 `gorm:"default:1"`  // 入口倍率
    OutboundMultiplier float64 `gorm:"default:1"`  // 出口倍率
    // 限制（叠加在用户限制之上）
    SpeedLimit  int64 `gorm:"default:0"` // byte/s，0 = 不限
    ConnLimit   int   `gorm:"default:0"`
    IPLimit     int   `gorm:"default:0"`
    // 协议与高级选项（JSON，结构见 4.5）
    Options JSON `gorm:"type:text"`
    // 链式出口（2~3 跳），存设备组 ID 数组；空 = 不启用
    ChainGroups JSON `gorm:"type:text"`
    // 反向隧道
    ReverseEnable bool   `gorm:"default:false"`
    ReversePort   int    `gorm:"default:0"`
    ReverseGroupID uint64 `gorm:"default:0"`
    // 分流（SNI 分裂）
    IsSubRule  bool   `gorm:"default:false;index"` // 是否为子规则
    ParentID   uint64 `gorm:"index"`               // 主规则 ID
    SNI        string `gorm:"size:255;index"`      // 子规则匹配的 SNI
    // 状态机
    //   unsynced  未同步
    //   syncing   同步中
    //   normal    正常
    //   failed    同步失败
    SyncStatus string `gorm:"size:16;default:unsynced;index"`
    SyncError  string `gorm:"size:512"`
    SyncedAt   *time.Time
    // 流量统计（累计，明细在 traffic_logs）
    TrafficIn  int64 `gorm:"default:0"` // 入口方向累计（字节，已乘倍率）
    TrafficOut int64 `gorm:"default:0"` // 出口方向累计（字节，已乘倍率）
    Enable     bool  `gorm:"default:true;index"`
    Remark     string `gorm:"size:255"`
    CreatedAt  time.Time
    UpdatedAt  time.Time
}
```

**Targets JSON 结构**：

```json
[
  { "host": "1.2.3.4",        "port": 443,  "weight": 1, "status": "up" },
  { "host": "example.com",    "port": 80,   "weight": 3, "status": "up" },
  { "host": "10.0.0.5",       "port": 8080, "weight": 1, "status": "down" }
]
```

- `weight`：目标级负载均衡权重。
- `status`：`up` / `down`，由目标故障转移探测更新；`down` 的目标不参与分发。

#### 4.2.7 `rule_groups` —— 规则分组

```go
type RuleGroup struct {
    ID        uint64 `gorm:"primaryKey"`
    Name      string `gorm:"size:64;uniqueIndex;not null"`
    Sort      int    `gorm:"default:0"`
    Remark    string `gorm:"size:255"`
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

#### 4.2.8 `traffic_logs` —— 流量明细（聚合记录）

```go
type TrafficLog struct {
    ID        uint64 `gorm:"primaryKey"`
    // 聚合维度
    Date      string `gorm:"size:10;index"`         // 2026-01-01（UTC 日期）
    Hour      int    `gorm:"default:-1;index"`      // 0~23；-1 表示按天聚合
    UserID    uint64 `gorm:"index;default:0"`
    RuleID    uint64 `gorm:"index;default:0"`
    NodeID    uint64 `gorm:"index;default:0"`       // 产生流量的节点
    Direction string `gorm:"size:8;index"`          // in | out
    // 计量
    RawBytes  int64 `gorm:"default:0"` // 实际字节
    Bytes     int64 `gorm:"default:0"` // 乘倍率后字节
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

**唯一约束**：`(date, hour, user_id, rule_id, node_id, direction)` 建唯一索引，使用 `ON CONFLICT DO UPDATE`（SQLite/PG）或 `ON DUPLICATE KEY UPDATE`（MySQL）实现 UPSERT，避免并发写重。

> 跨方言提示：Gorm 的 `clause.OnConflict` 在三种方言下都受支持，MUST 使用它而不是手写 SQL。

#### 4.2.9 `probe_metrics` —— 探针指标

```go
type ProbeMetric struct {
    ID        uint64 `gorm:"primaryKey"`
    NodeID    uint64 `gorm:"index;not null"`
    Timestamp int64  `gorm:"index;not null"` // Unix 秒
    CPU       float64  // 使用率 0~100
    MemUsed   int64    // 字节
    MemTotal  int64
    SwapUsed  int64
    SwapTotal int64
    DiskUsed  int64
    DiskTotal int64
    NetIn     int64  // 累计入口字节（来自 COUNT_INTERFACE 指定网卡）
    NetOut    int64
    NetInSpeed  int64 // 瞬时速率 byte/s
    NetOutSpeed int64
    Load1     float64
    Load5     float64
    Load15    float64
    TcpConn   int
    UdpConn   int
    Uptime    int64
    CreatedAt time.Time
}
```

#### 4.2.10 `sessions` —— 在线会话（IP 限制 / 连接限制 / 在线用户）

```go
type Session struct {
    ID        uint64 `gorm:"primaryKey"`
    UserID    uint64 `gorm:"index"`
    RuleID    uint64 `gorm:"index"`
    NodeID    uint64 `gorm:"index"`
    ClientIP  string `gorm:"size:64;index"`
    ClientIPHash string `gorm:"size:64;index"` // 用于匿名化统计
    DeviceID  string `gorm:"size:128;index"`   // 由 UA/指纹推导，用于设备数限制
    ConnCount int    `gorm:"default:1"`
    Protocol  string `gorm:"size:16"`          // ws | http | tls | direct | udp
    StartedAt time.Time
    LastSeen  time.Time `gorm:"index"`
    Upload    int64
    Download  int64
}
```

> 会话表容量控制：超过 10 万行时按 `last_seen` 淘汰最旧的记录。会话数据**不参与计费**，仅用于限制与展示。

#### 4.2.11 `system_settings` —— 键值配置

```go
type SystemSetting struct {
    Key       string `gorm:"primaryKey;size:64"`
    Value     JSON   `gorm:"type:text"`
    UpdatedAt time.Time
}
```

用于存放：主题（`classic` / `transparent`）、站点名称、Logo、公告、默认限速、告警阈值、Webhook 地址、API 开关等。

#### 4.2.12 `audit_logs` —— 操作审计

```go
type AuditLog struct {
    ID        uint64 `gorm:"primaryKey"`
    UserID    uint64 `gorm:"index"`
    Username  string `gorm:"size:64"`
    Action    string `gorm:"size:64;index"` // create | update | delete | login | sync | migrate ...
    Resource  string `gorm:"size:64;index"` // node | rule | device_group | user ...
    ResourceID uint64 `gorm:"index"`
    Before    JSON   `gorm:"type:text"`     // 变更前快照
    After     JSON   `gorm:"type:text"`     // 变更后快照
    IP        string `gorm:"size:64"`
    UserAgent string `gorm:"size:255"`
    Result    string `gorm:"size:16"`       // success | failed
    Message   string `gorm:"size:512"`
    CreatedAt time.Time `gorm:"index"`
}
```

#### 4.2.13 `api_tokens` —— 对外 API 令牌

```go
type APIToken struct {
    ID          uint64 `gorm:"primaryKey"`
    Name        string `gorm:"size:64;not null"`
    Token       string `gorm:"size:128;uniqueIndex;not null"` // 明文仅创建时返回一次
    TokenHash   string `gorm:"size:128;index"`                 // 库中只存哈希
    Scopes      JSON   `gorm:"type:text"`                      // ["node:read","rule:write",...]
    IPWhitelist JSON   `gorm:"type:text"`                      // ["1.2.3.0/24"]
    ExpireAt    *time.Time
    LastUsedAt  *time.Time
    Enabled     bool `gorm:"default:true"`
    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

#### 4.2.14 `config_snapshots` —— 配置快照（新增，用于回滚）

```go
type ConfigSnapshot struct {
    ID        uint64 `gorm:"primaryKey"`
    Name      string `gorm:"size:128"`
    Reason    string `gorm:"size:255"`  // manual | auto_before_sync | auto_daily | before_migrate
    Payload   JSON   `gorm:"type:text"` // 全量配置：device_groups + forward_rules + nodes 关联
    Checksum  string `gorm:"size:64"`
    CreatedAt time.Time `gorm:"index"`
}
```

#### 4.2.15 `alert_rules` —— 告警规则（新增）

```go
type AlertRule struct {
    ID         uint64 `gorm:"primaryKey"`
    Name       string `gorm:"size:128"`
    // 类型：node_offline | node_cpu | node_mem | node_disk | rule_sync_failed
    //      | user_traffic_pct | rule_traffic_pct | node_traffic_pct | cert_expire
    Type       string  `gorm:"size:32;index"`
    TargetID   uint64  `gorm:"index"`      // 关联节点/用户/规则 ID，0 = 全局
    Threshold  float64 `gorm:"default:0"`  // 阈值（百分比 / 秒数）
    Duration   int     `gorm:"default:0"`  // 持续时长（秒）后才触发，防抖
    // 通知渠道
    Channels   JSON    `gorm:"type:text"`  // [{"type":"webhook","url":"..."},{"type":"telegram","token":"...","chat":"..."}]
    SilenceFor int     `gorm:"default:300"`// 静默期（秒），避免重复轰炸
    Enabled    bool    `gorm:"default:true"`
    LastFiredAt *time.Time
    CreatedAt  time.Time
    UpdatedAt  time.Time
}
```

#### 4.2.16 `alert_histories` —— 告警历史

```go
type AlertHistory struct {
    ID        uint64 `gorm:"primaryKey"`
    RuleID    uint64 `gorm:"index"`
    Level     string `gorm:"size:16"`  // info | warning | critical
    Title     string `gorm:"size:255"`
    Content   string `gorm:"type:text"`
    Resolved  bool   `gorm:"default:false;index"`
    FiredAt   time.Time `gorm:"index"`
    ResolvedAt *time.Time
}
```

### 4.3 入口组配置 JSON（结构对齐 Nyanpass，MUST 兼容）

```json
{
  "allowed_host": [],
  "blocked_host": [],
  "blocked_path": [],
  "blocked_protocol": [],
  "tls_inbound_policy": 0,
  "tls_reject_empty_sni": false,
  "disable_udp": false,
  "udp_over_tcp": false,
  "ipv6_group": [],
  "max_fail": 3,
  "fail_timout_sec": 30,
  "reverse_group": [],
  "protocol": "tls",
  "tls": {}
}
```

**字段语义（MUST 按此实现，不得改动语义）**：

| 字段 | 语义 |
|---|---|
| `allowed_host` | 允许的目标域名白名单，**后缀匹配**，如 `.qq.com` 匹配 `a.qq.com`；空数组 = 不限制 |
| `blocked_host` | 禁止的目标域名黑名单，后缀匹配；优先级高于白名单 |
| `blocked_path` | 禁止的 HTTP 路径前缀 |
| `blocked_protocol` | 禁止的入站协议类型：`socks`（SOCKS 握手）、`fet`（完全加密的 ss/vmess 等无法识别的流量）、`http`、`tls` |
| `tls_inbound_policy` | TLS 入站策略：`0` = 不处理，`1` = 强制 TLS 剥离，`2` = SNI 分流模式 |
| `tls_reject_empty_sni` | 拒绝无 SNI 的 TLS 握手 |
| `disable_udp` | 完全关闭 UDP 转发 |
| `udp_over_tcp` | UDP 走 TCP 隧道（UoT）；关闭时使用原生 UDP |
| `ipv6_group` | IPv6 优先策略：`[]` 默认；`[0]` 所有出口优先 IPv6；`[0,1,2]` 所有出口优先 IPv6，但节点 1、2 优先 IPv4 |
| `max_fail` | 出口连续失败次数阈值 |
| `fail_timout_sec` | 故障判定超时（秒） |
| `reverse_group` | 反向隧道设备组 ID 列表 |
| `protocol` | 与出口通信的协议：`ws` / `http` / `tls` |
| `tls` | 协议细节，见 4.5 |

> **`ipv6_group` 注意**：connect 地址选择**没有回退机制**。若选用 IPv6 且目标不可达，不会自动回退 IPv4。前端在选择 IPv6 优先时 MUST 弹出风险提示。

#### 连接地址优先级（MUST 严格实现此顺序）

```
1. connect_type == "static" && connect_address != ""  →  使用 connect_address + connect_port
2. connect_host != ""                                  →  使用 connect_host
3. dyn_ip4 可用                                        →  使用动态 IPv4
4. ipv6_group 配置且当前节点在组内                       →  使用动态 IPv6
5. dyn_ip6 可用                                        →  使用动态 IPv6
6. 回退                                                →  使用上报的公网 IPv4
```

### 4.4 出口组配置 JSON（MUST 兼容）

```json
{
  "connect_type": "static",
  "connect_address": "node.example.com",
  "connect_port": 2333,
  "protocol": "ws",
  "ws": {},
  "udp_over_tcp": false
}
```

- `connect_type`：`dyn_ip4`（动态 IPv4）/ `dyn_ip6`（动态 IPv6）/ `static`（静态地址）
- `connect_address`：静态地址时必填，支持域名或 IP
- `connect_port`：静态地址时的连接端口
- `protocol`：`ws` / `http` / `tls`
- `ws` / `tls`：协议细节，见 4.5

### 4.5 隧道协议配置 JSON

**`ws`（WebSocket 伪装）**

```json
{
  "ws": {
    "host": "some.host.com",
    "path": "/some/path",
    "request":  "GET / HTTP/1.5\r\n\r\n",
    "response": "HTTP/1.5 200 OK\r\n\r\n"
  }
}
```

- 不是完整 WebSocket 协议，仅做握手伪装。
- `request` / `response` 为原始 HTTP 报文模板，MUST 允许用户自定义（用于绕过特征检测）。
- `http`（FakeHTTP）协议**复用同一份 `ws` 配置**来设置 host / path，区别是没有 `Upgrade` / `Switch Protocol` 头。

**`tls`（tls_simple）**

```json
{
  "tls": {
    "sni": "some.host.com",
    "alpn": ["http/1.1"],
    "chfp": "chrome"
  }
}
```

- `chfp` 指纹可选值：`chrome`、`firefox`、`safari`、`ios`、`android`、`edge`、`360`、`qq`。
- 不填 `chfp` 则使用 Go 默认 TLS 指纹。

**规则级 TLS 入站配置（用于 SNI 分流 / 自定义证书）**

```json
{
  "tls": {
    "empty_sni": false,
    "force_empty_sni": false,
    "sni": "i0.hdslb.com",
    "alpn": ["http/1.1"],
    "cert": ["-----BEGIN CERTIFICATE-----", "..."],
    "key":  ["-----BEGIN PRIVATE KEY-----", "..."]
  }
}
```

**UDP 语义**：无论选择哪种隧道协议，若未开启 `udp_over_tcp`，UDP 一律走**原生 UDP**（28 字节 MTU 开销，简单加密）。开启后 UDP 封装进 TCP 隧道。

### 4.6 数据库初始化与迁移（结构层面）

- 首次启动且 `data.db` 不存在时，自动创建全部表并写入默认数据：
  - 一个默认用户分组「默认分组」
  - 一个默认规则分组「默认规则组」
  - 一条默认系统设置（站点名 `OpenRoute`、主题 `classic`）
- `MIGRATE=1` 时执行 `AutoMigrate`，并打印**结构变更 diff**（新增表、新增列、类型变更）。删除列 MUST NOT 自动执行，只提示。
- 提供 `schema_version` 表记录当前结构版本，升级时按版本号顺序执行变更脚本。

---

## 5. 后端规格：节点通信与同步

### 5.1 节点注册（一键对接）

**流程**：

```
1. 管理员在面板「节点管理」点击「添加节点」→ 填写节点名称、角色（入口/出口/双端）
2. 面板生成：
   - 唯一 token（32 字节随机字符串）
   - 一条安装命令：
     bash <(curl -fsSL http://<面板地址>/install.sh) -u <面板地址> -t <token>
     （若面板开启了 HTTPS，则用 https）
3. 管理员在目标机器上以 root 执行该命令
4. 安装脚本：
   - 探测系统（Debian 11+ / Ubuntu 22.04+，架构 amd64 / amd64v3 / arm64）
   - 下载节点客户端二进制到 /opt/openroute/
   - 写入 /opt/openroute/config.yml（base-url、token、is-outbound 等）
   - 注册 systemd 服务 openroute-node 并 enable
   - 启动服务
5. 节点启动后立即向面板注册接口上报自身信息：
   - 公网 IPv4 / IPv6（通过多个外部回显接口探测，失败则跳过）
   - 内网 IP、操作系统、内核、CPU 型号与核数、内存、磁盘、启动时间
   - 节点客户端版本
6. 面板收到注册：标记节点在线、记录系统信息、返回面板侧配置（监听端口、协议参数）
7. 面板页面刷新，节点显示为「在线」
```

**面板侧接口（节点调用，用 token 认证，不走管理员 Session）**：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/node/register` | 首次注册，返回节点 ID 与初始配置 |
| POST | `/api/node/heartbeat` | 每 10 秒上报一次：系统指标 + 当前运行规则摘要 + 实际监听端口 |
| GET | `/api/node/config` | 拉取全量配置（首次或重启后） |
| POST | `/api/node/report` | 上报规则同步结果、连接数、流量、错误日志 |
| GET | `/api/node/tasks` | 拉取待执行任务（升级、重启、执行命令） |
| POST | `/api/node/task-result` | 上报任务执行结果 |
| WS/SSE | `/api/node/stream` | 长连接通道，用于即时推送配置变更（可选，见 5.4） |

### 5.2 客户端 `config.yml`（节点侧）

```yaml
# 面板地址（必填）
base-url: "http://1.2.3.4:18888"
# 节点密钥（必填）
token: "xxxxxxxxxxxx"
# 是否为出口节点
is-outbound: false
# 预留 ECH 字段（当前客户端尚未实现）
use-ech: false
ech-query-name: ""

# 本机节点间隧道监听端口；0继承面板/默认值，不改变用户规则端口
direct-port: 0    # direct 隧道 TCP 端口
ws-port: 0        # ws / http 隧道 TCP 端口
tls-port: 0       # tls 隧道 TCP 端口
udp-port: 0       # 原生 UDP 隧道端口
rev-port: 0       # 反向隧道 TCP 端口

# 发布本机供其它节点连接的地址和端口，支持NAT映射
connect-host: ""            # 发布本机供其它节点连接的地址；组级static地址仍优先
connect-direct-port: 0
connect-ws-port: 0
connect-tls-port: 0
connect-udp-port: 0
connect-rev-port: 0

# 历史预留字段；当前实际权重由面板配置
default-weight: 1
```

**优先级规则（MUST 实现）**：命令行参数 `-u` / `-t` **完全覆盖** `config.yml` 中的 `base-url` / `token`。

### 5.3 节点环境变量（默认 `/opt/openroute-node/env.sh`）

| 变量 | 含义 | 版本要求 |
|---|---|---|
| `DISABLE_EXECUTE=1` | 禁用 WebSSH，并阻止远程升级 | |
| `BIND_INBOUND` | 用户规则监听的本机 IP 或网卡名，多个用逗号分隔 | |
| `TUNNEL_BIND_INBOUND` | 节点间隧道监听的单个本机裸 IP；未填继承 BIND_INBOUND | nc20260922.2 |
| `TUNNEL_BIND_OUTBOUND_4` / `_6` | 节点间隧道出站源 IP，与最终目标出站分开 | nc20260922.2 |
| `TUNNEL_INTERFACE` | Linux 节点间隧道 socket 绑定网卡（SO_BINDTODEVICE） | nc20260922.2 |
| `TUNNEL_FWMARK` | Linux 节点间隧道 socket 标记，路由规则需另配 | nc20260922.2 |
| `BIND_OUTBOUND_4` | 限定出口 IPv4 出站源地址 | 不推荐使用 |
| `BIND_OUTBOUND_6` | 限定出口 IPv6 出站源地址 | 不推荐使用 |
| `OUTBOUND_FWMARK` | 出站流量打 fwmark，配合策略路由 | 新版本 |
| `COUNT_INTERFACE` | 指定探针统计流量的网卡（如 `eth0`）；不设置时使用「智能统计」 | |
| `HEALTH_CHECK=0` | 禁用主动健康检查（降级为被动判定） | |
| `UUID` | 同一台机器跑多个实例时，为每个实例指定唯一值 | |
| `NYA_PROXY` | 节点出站代理，支持 `socks5://` / `http://` / `ss://` / `trojan://` / `tuic://` | |
| `HTTPS_PROXY` | 标准 HTTPS 代理变量 | |

**安装脚本环境变量**：

| 变量 | 含义 |
|---|---|
| `S=openroute` | 服务名（默认 `openroute`），多实例部署时用于区分 |
| `OPTIMIZE=1` | 启用系统内核网络参数优化（BBR、缓冲区、文件句柄） |
| `INSTALL_TOOLS=1` | 安装常用排查工具（`iftop`、`mtr`、`tcpdump` 等） |

### 5.4 配置下发与同步机制

**目标**：规则变更在 1 秒内到达节点，同时保证「面板短暂失联时节点继续正常运行」。

**机制（MUST 实现）**：

```
┌──────────┐  1. 规则变更（API / 前端操作）
│  面板     │ ─────────────────────────────► 写库（sync_status = unsynced）
└────┬─────┘
     │ 2. 立即推送（尽力而为）
     ├──► gRPC / HTTP2 长连推送  ──► 节点应用配置 ──► 上报结果
     │    失败或 3 秒内无响应 → 降级
     │
     │ 3. 兜底轮询（保证最终一致）
     └──► 节点每 20 秒 GET /api/node/config?since=<版本号>
          面板返回增量或全量 → 节点应用 → 上报 sync_status
```

**同步状态机（前端必须展示）**：

```
unsynced（未同步，黄色）
   │ 收到配置
   ▼
syncing（同步中，蓝色）
   ├─ 应用成功 ─► normal（正常，绿色）
   └─ 应用失败 ─► failed（同步失败，红色）+ 错误原因
```

**版本号机制**：面板维护一个全局单调递增的 `config_version`，任何规则/设备组/节点配置变更都自增。节点上报自己当前的 `config_version`，面板据此判断是否需要重新下发。

**关键约束**：

- 节点 MUST 把最后一次成功应用的配置**持久化到本地磁盘**。面板不可达时，节点继续以本地配置运行，**绝不停止转发**。
- 节点重启后优先加载本地配置，再向面板拉取最新配置。
- 若面板下发的配置解析失败，节点 MUST 保留旧配置继续运行，并上报错误，不得崩溃。

### 5.5 节点命令（运维手册，写进文档与 WebUI 提示）

```bash
# 卸载
bash /opt/openroute/openroute.uninstall.sh

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
/opt/openroute/rel_nodeclient -h

# 手动指定连接地址与端口（调试）
/opt/openroute-node/rel_nodeclient -c /opt/openroute-node/config.yml --connect-host 10.88.0.2 --ws-port 28081 --tls-port 28082
```

**面板内升级节点**：面板「节点管理」→ 选择节点 → 「升级」，节点拉取新版本二进制，替换后重启服务。若节点设置了 `DISABLE_EXECUTE=1`，升级被拒绝。

本地监听与对外NAT端口分离、CLI/YAML/面板优先级以及IPLC/IEPL完整示例，见
[`docs/PRIVATE_LINES.md`](openroute/docs/PRIVATE_LINES.md)。该指南描述实际客户端能力；
本规格书中的其它预留配置不能自动视为已实现。

### 5.6 节点故障排查清单（写进 WebUI 帮助与 `docs/`）

| 现象 | 排查步骤 |
|---|---|
| 安装失败 | 检查系统版本（Debian 11+ / Ubuntu 22.04+）、架构、是否有 root 权限、是否有外网 |
| 下载中断/重置 | 换用离线部署：本地下载二进制后 `scp` 到 `/opt/openroute/` |
| APT 报错 | 先 `apt update`；检查 `/etc/apt/sources.list`；不依赖 APT 也可运行（二进制是静态编译） |
| 节点离线 | 检查 `systemctl status openroute-node`、`journalctl -fu openroute-node`、防火墙是否放行面板端口 |
| 面板显示离线但节点在跑 | 检查节点能否访问面板地址（`curl <base-url>`）、时钟是否同步、token 是否被改 |
| 监控页面数据不更新 | 检查 `COUNT_INTERFACE` 是否指向正确网卡；「智能统计」模式下切换网卡 |
| 规则同步延迟 | 检查面板与节点之间的网络 RTT；轮询间隔 20s 属正常，即时推送失败会降级 |
| 多实例冲突 | 每实例设置唯一 `UUID`，并用 `S=` 指定不同服务名 |
| 机器 UUID 冲突 | 克隆虚拟机导致；删除 `/opt/openroute/machine-id` 后重启 |
| `too many open files` | 安装脚本 `OPTIMIZE=1` 或手动调高 `ulimit -n` 与 systemd `LimitNOFILE` |
| 垃圾用户命令 | 使用「节点管理 → 清理会话」或 `-clean` 命令 |

---

## 6. 核心功能规格

### 6.1 节点管理

**列表页字段**：名称、角色、所属分组、公网 IPv4/IPv6、连接地址、在线状态、最后心跳时间、CPU/内存/磁盘、当前连接数、实时速率、节点版本、操作。

**能力**：

- 新增 / 编辑 / 删除节点（删除前必须确认；该节点上的规则会被标记为「节点已删除」并进入待处理清单）
- 一键对接（生成安装命令，带复制按钮与二维码）
- 批量操作：批量升级、批量重启、批量执行命令、批量改分组、批量设权重、批量禁用
- 在线状态实时刷新（WebSocket 或 5 秒轮询）
- 节点详情页：基本信息、实时监控曲线（CPU/内存/网络 5 分钟粒度）、当前规则列表、日志查看、终端（WebSSH）
- **健康度评分（新增）**：综合在线率、同步失败率、CPU/内存水位、连接失败率，给出 0~100 分与红黄绿标识，用于快速发现劣质节点

**WebSSH（终端）实现要求**：

- 后端通过节点长连转发 SSH 会话，浏览器端用 `xterm.js`。
- 支持窗口尺寸同步、粘贴、断线重连。
- 每次打开终端 MUST 写入审计日志（谁、何时、连的哪台机器）。
- `enable-webssh: false` 时接口返回 403。

### 6.2 节点分组

- 分组仅用于批量管理与筛选，不改变转发行为。
- 支持按分组批量下发配置变更。
- 支持导出分组内节点为 CSV。

### 6.3 设备组（入口组 / 出口组）

**入口组能力**：

| 配置项 | 说明 |
|---|---|
| 组内节点 | 多选节点；同一入口组内多台机器共用同一套入口规则 |
| 协议 | `tls` / `ws` / `http` / `direct`（入口直出） |
| 目标域名策略 | `allowed_host` 白名单（后缀匹配）、`blocked_host` 黑名单 |
| 路径策略 | `blocked_path` 前缀黑名单 |
| 协议阻止 | `blocked_protocol`：`socks` / `fet` / `http` / `tls` |
| TLS 策略 | `tls_inbound_policy`（0/1/2）、`tls_reject_empty_sni` |
| UDP | `disable_udp`、`udp_over_tcp` |
| IPv6 | `ipv6_group` 优先策略 |
| 故障 | `max_fail`、`fail_timout_sec` |
| 反向 | `reverse_group` |
| 协议细节 | `ws` / `tls` 子配置 |

> **入口组不做面板侧负载均衡**：入口组内每台机器各自独立监听与转发，用户的多个入口地址由用户侧决定。这是与出口组的本质区别，MUST 在 UI 上明确提示。

**出口组能力**：

| 配置项 | 说明 |
|---|---|
| 组内节点 | 多选；组内做**最少连接数**负载均衡 |
| 连接方式 | `dyn_ip4` / `dyn_ip6` / `static` |
| 静态地址 | `connect_address`（域名或 IP）+ `connect_port` |
| 协议 | `ws` / `http` / `tls` |
| UDP | `udp_over_tcp` |
| 权重 | 每节点权重，默认 1 |
| 健康检查 | 开关、间隔、超时、失败阈值、恢复阈值 |
| 故障转移组 | 本组全挂时转投的目标组 |
| 组内优先级 | 节点顺序参与决策，可拖动排序 |

**「限制出口」语法（MUST 兼容 Nyanpass 写法）**：

在设备组或规则的出口配置中，「限制出口」字段接受以下写法：

| 写法 | 含义 |
|---|---|
| `禁止单端` | 禁止使用单端出口（即必须走隧道到独立出口机） |
| `1,2,3` | 仅允许使用节点 ID 为 1、2、3 的出口 |
| `1,2,3,禁止单端` | 允许 1/2/3，且禁止单端 |
| `1145141919` | 使用一个不存在的 ID，等于「只允许单端」（反向表达技巧） |
| 空 | 不限制 |

**负载均衡算法（MUST 实现 `least_conn` 为默认）**：

```
least_conn（最少连接数）：
  候选出口 = 本组内 online == true 且已通过健康检查的节点
  过滤条件：满足「限制出口」规则、未超过 MaxConn、权重 > 0
  选择 current_conn / weight 最小的节点
  并列时按权重加权随机，再按节点顺序取第一个

round_robin：按权重比例加权轮询，用平滑加权轮询算法（SWRR）

hash_ip：对客户端 IP 做一致性哈希，保证同一客户端固定出口

weighted：纯按权重随机
```

**故障转移（MUST 实现）**：

```
判定失败的条件（满足任一即计为一次失败）：
  - TCP 握手超时（超过 fail_timout_sec）
  - TCP 连接被拒绝（RST）
  - 已建立连接但在健康检查周期内无任何数据往返

连续失败次数 >= max_fail  →  摘除该出口（状态标记 down）
连续成功次数 >= health_check_succ_count  →  恢复该出口（状态标记 up）

本组所有出口均为 down  →  将新连接转投 FailoverGroupID 指定的故障转移组
                         （故障转移组 MUST 具备相同的入口组权限配置 JSON，
                           否则面板拒绝保存并提示）
故障转移可能需要数秒才能生效（等待 TCP 超时），UI MUST 提示这一点。
```

**多目标地址负载均衡（规则级）**：

- 一条规则可配置多个目标（`Targets` 数组）。
- 策略可选：`least_conn` / `round_robin` / `weighted` / `hash_ip` / `failover`（主备）。
- 目标级故障转移：某目标连续失败后标记 `down`，不参与分发；恢复后自动回到池中。
- UI 支持在一条规则中拖拽调整目标优先级。

### 6.4 转发规则

**列表页字段**：规则名、归属用户、规则分组、入口组、监听端口、出口组、目标地址、协议、倍率（入/出）、今日流量、累计流量、同步状态、启用开关、操作。

**单条规则创建表单字段**：

| 字段 | 必填 | 说明 |
|---|---|---|
| 规则名 | ✅ | 唯一性在应用层做 EqualFold 比较 |
| 归属用户 | | 默认当前用户 |
| 规则分组 | | |
| 入口设备组 | ✅ | 决定协议与入口策略 |
| 监听端口 | ✅ | `0` 表示随机端口（可能对应端口段） |
| 出口设备组 | | 留空 = 单端（入口直出） |
| 目标地址 | ✅ | 支持域名/IP，可添加多个 |
| 目标端口 | ✅ | |
| 入口倍率 | | 默认 1，范围 0 ~ 100，允许 2 位小数 |
| 出口倍率 | | 同上 |
| 速度限制 | | 单位可选 KB/s、MB/s、Gbps |
| 连接数限制 | | |
| IP 数限制 | | |
| 反向隧道 | | 开关 + 端口 + 反向组 |
| 链式出口 | | 选择 2~3 个设备组 |
| 备注 | | |

**批量操作**：

| 操作 | 说明 |
|---|---|
| 批量启用 / 禁用 | 立即下发 |
| 批量删除 | 二次确认 + 显示受影响用户 |
| 批量改分组 | |
| 批量改倍率 | 支持按百分比批量调整（如「全部 ×1.5」） |
| 批量改限制 | |
| 批量迁移出口组 | |
| 批量导出 JSON | |
| 批量导入 JSON | 见下 |

**批量文本格式（旧格式，MUST 兼容）**：

```
规则名#监听端口#目标地址#目标端口
规则名##目标地址#目标端口          ← 双井号 = 随机端口
```

**新版 JSON 导入格式**：

```json
{
  "name": "my-rule",
  "listen_port": 0,
  "targets": [{ "host": "1.2.3.4", "port": 443, "weight": 1 }],
  "inbound_group_id": 1,
  "outbound_group_id": 2,
  "inbound_multiplier": 1.5,
  "outbound_multiplier": 0.5,
  "speed_limit": 0,
  "remark": ""
}
```

- `listen_port: 0` 在支持随机端口的版本上表示「自动分配一个空闲端口」。
- 导入 MUST 支持「预览模式」：先解析全部条目，列出「新增 / 冲突 / 非法」三类结果，用户确认后再落库。

**规则同步**：

- 同步间隔 20 秒（轮询兜底），变更时立即推送（gRPC/长连）。
- 初始状态「未同步」；随后在「正常」与「同步失败」之间变化。
- 同步失败时，规则行 MUST 显示红色状态与失败原因（可展开查看节点返回的原始错误）。

**流量计费规则（MUST 精确实现）**：

```
单向流量统计。

用户产生的流量 = 入口实际流量 × 入口倍率 + 出口实际流量 × 出口倍率

示例：
  用户下载 500M、上传 100M
  该规则流量增加 = 500 + 100 = 600M
  若入口倍率 1.5、出口倍率 0.5
  则用户已用流量增加 = 600 × 1.5 + 600 × 0.5 = 900 + 300 = 1200MB
```

> 链式出口场景下，流量同时经过入口与链式出口，**两段分别按各自倍率计入**。

### 6.5 入口直出（直接转发）

**用途**：专线场景。数据不经过隧道，直接从入口机器转发到目标，开销极低。

**实现要点**：

- 在入口机器上直接建立 TCP/UDP 转发（类似 `iptables` REDIRECT + 用户态转发，或纯用户态 splice）。
- 无隧道头开销、无加密（或可选轻量加密）。
- 不做穿墙伪装（不需要）。
- MUST 支持 TCP 与 UDP。

**双机专线配置方式**：

- **简单模式**：出口机器填写入口机器的内网 IP 作为 `connect_address`，`connect_type: static`，协议选 `direct`。两台机器在同一内网或专线内。
- **复杂模式**：入口机器与出口机器分属不同网络，出口机器用 `connect_address` 指定入口的公网 IP + 映射端口，或使用反向隧道（出口主动连入口）。

### 6.6 反向隧道

**场景**：出口机器没有公网 IP，或出口机器不能被入口主动连接（NAT 后、防火墙后）。

**实现**：

- 方向反转：**出口主动连接入口**，建立隧道。
- 入口侧监听 `rev-port`（`--rev-port` 参数）。
- `reverse_group` 指定参与反向的出口设备组。
- 用户流量路径：用户 → 入口监听端口 → 反向隧道 → 出口 → 目标。
- UDP 走 UoT（UDP over TCP）。

**约束**：

- 反向隧道端口 MUST 与正向端口分离配置，避免冲突。
- 面板 MUST 校验反向组的出口节点确实配置了 `is-outbound: true`。

### 6.7 链式出口

**场景**：需要 2~3 跳中转（例如 入口 → 中转 → 落地），提升隐蔽性或解决链路质量。

**实现**：

- `ChainGroups` 存有序的设备组 ID 数组，长度 2 或 3。
- 入口到第一跳**默认开启 Mux**（连接复用）。
- **不支持 UDP 链式转发**：MUST 在 UI 上禁用链式场景下的 UDP 相关开关并说明原因。
- **不支持故障转移组**：链式模式下故障转移开关置灰并提示。
- 流量计算：**同时按入口倍率与链式出口倍率计入**（见 6.4 计费规则）。
- 链式链路的中间跳必须部署节点客户端且角色为出口。

### 6.8 SNI 分流（要求版本高于 20260302 的等价能力）

**场景**：多个用户共用一个 443 端口，按 TLS 握手中的 SNI 分发到不同规则。

**配置步骤（MUST 按此顺序）**：

```
1. 创建入口设备组（或编辑已有组）：
   - 设备组 config.tls_inbound_policy 设为 2
   - tls_reject_empty_sni 设为 true
   - allowed_host 设为 [".bilivideo.cn", ".example.com"]（SNI 白名单）
2. 管理员创建「主规则」：
   - 监听 443（固定端口）
   - uid: 0（系统归属）
   - 该主规则本身不承载用户流量，只做分发
3. 用户创建「子规则」：
   - 选择同一个入口设备组
   - 共享 TLS 端口 443
   - 填写 SNI（MUST 落在 allowed_host 白名单内）
   - 子规则由面板自动关联到主规则（ParentID）
```

**整流选项（Shaping）**：

| 选项 | 值 | 说明 |
|---|---|---|
| `Shape_RejectTLS12` | 1 | 拒绝 TLS 1.2 握手 |
| `Shape_RejectTLS` | 2 | 拒绝全部 TLS 握手 |
| `Shape_RejectHttp1` | 3 | 拒绝 HTTP/1.x |
| `Shape_RejectWs` | 4 | 拒绝 WebSocket 升级 |
| `Shape_SkipIPv6` | 2000 | 跳过 IPv6 目标 |

**兼容性警告（MUST 在 UI 上提示）**：

- 与 VLESS-Vision、VLESS-Reality、ShadowTLS **不兼容**（这些协议有自己的握手特征，会被整流规则误伤）。
- 开启 SNI 分流后，同一入口组的其它规则也受该组的 `tls_inbound_policy` 影响。

### 6.9 速度与资源限制

**MUST 遵守以下语义（与 Nyanpass 一致）**：

| 规则 | 说明 |
|---|---|
| 所有限制在**入口端**强制执行 | 出口端不做限速 |
| 每个入口**独立计算** | 入口组内两台机器各自限速，总速率是两倍 |
| 规则级限制与用户级限制**叠加** | 实际生效的是两者中更严格的那个（取 min） |
| **UDP 无法限速** | UI 上必须标注 |
| **无法限制整个入口或某个程序的总体速度** | 只能按规则/用户维度；UI 上必须标注 |

**限速实现建议**：令牌桶（`golang.org/x/time/rate`）按规则维度创建 limiter，用户维度再套一层。参考 Nyanpass 的 `nc_speed_limit` 语义。

**连接数 / IP 数限制**：

- 连接数：统计该规则/用户当前活跃连接数，超限则拒绝新连接。
- IP 数：统计该规则/用户近期（默认 5 分钟窗口）不同客户端 IP 数，超限则拒绝新 IP 的连接，已建立的连接不中断。
- 设备数：基于 `DeviceID`（由客户端 UA + 指纹推导）统计，超限拒绝。

### 6.10 流量统计

**三个维度**：

1. **规则级**：每条规则的入口流量、出口流量、今日/昨日/本月/累计、按小时的曲线。
2. **用户级**：每个用户的已用流量、剩余流量（若设置了上限）、按规则的分布饼图。
3. **全站级**：整站总流量、总连接数、在线用户数、在线节点数、按节点的流量排行、按用户/规则的流量排行。

**统计口径**：

- 原始字节与乘倍率后的字节**都要记录**，页面默认展示乘倍率后的值，可切换显示原始值。
- 时间粒度：小时（保留 30 天）、天（永久）。
- 支持导出 CSV。
- 支持按时间范围、用户、规则、节点、方向任意组合筛选。

### 6.11 探针与监控

**采集项**：CPU 使用率、内存/交换分区、磁盘、网络进出速率与累计（按 `COUNT_INTERFACE` 指定网卡或智能统计）、负载（1/5/15 分钟）、TCP/UDP 连接数、运行时长。

**展示**：

- 节点详情页：5 分钟粒度的实时曲线（CPU / 内存 / 网络）
- 全局监控页：所有节点缩略图网格 + 关键指标
- 历史查询：任意时间范围，1 小时 / 1 天 / 7 天粒度自动降采样
- 「智能统计」：未指定 `COUNT_INTERFACE` 时，自动排除 lo、docker、veth 等虚拟网卡，取流量最大的物理网卡

**保留策略**：原始探针数据保留 7 天（可配置），超期自动清理。

### 6.12 事件与告警（新增）

- **告警类型**：节点离线、CPU 超阈值、内存超阈值、磁盘超阈值、规则同步失败、用户流量百分比、规则流量百分比、节点流量百分比、证书即将过期。
- **通知渠道**：Webhook（POST JSON）、Telegram Bot、邮件（SMTP）。个人自用建议至少做 Webhook。
- **防抖**：`Duration` 持续满足条件才触发；`SilenceFor` 静默期内不重复通知。
- **告警中心页面**：当前活跃告警、历史告警、一键标记解决。

### 6.13 配置快照与回滚（新增）

- 每次「批量规则变更」「设备组变更」「迁移前」自动生成快照。
- 每天定时生成一份快照（可关闭）。
- 快照列表页支持：查看差异（与当前配置对比）、一键回滚、导出 JSON、删除。
- 回滚前 MUST 再生成一份「回滚前快照」，保证可逆。

### 6.14 配置漂移检测（新增）

- 面板记录「期望配置」，节点上报「实际配置」（监听端口、运行规则摘要、协议、进程状态）。
- 定时比对，出现差异时在节点卡片上标记「配置漂移」，并给出差异详情。
- 支持「一键纠正」（重新下发期望配置）。

### 6.15 备份与恢复（新增）

- `-backup ./backup-20260101.zip`：导出**单文件备份**，内容包含：
  - `data.db`（或 MySQL/PG 的 SQL dump）
  - `config.yml`（`secret-key` 默认脱敏，可用 `--with-secret` 保留）
  - `manifest.json`：版本号、导出时间、表行数、校验和
- `-restore ./backup-20260101.zip`：恢复前自动把当前数据备份为 `data.db.before-restore`，然后覆盖。
- WebUI 也提供「下载备份 / 上传恢复」入口。

### 6.16 个性化（保留经典 + 透明主题）

- 主题：`classic`（经典）与 `transparent`（透明）。
- 站点名称、Logo、favicon、登录页背景可自定义。
- 前端 MUST 保留完整的样式变量层（CSS Variables），便于后续替换主题。
- 面板自带的**公开 API 与文档**是对外定制的基础（见第 8 章）。

---

## 7. 迁移功能规格（重点章节）

### 7.1 迁移能力总览

| 场景 | 命令 | 说明 |
|---|---|---|
| Nyanpass → OpenRoute | `-migrate from=nyanpass` | 从 Nyanpass 的数据库读取并转换 |
| OpenRoute → OpenRoute（跨库） | `-copy-database` | SQLite ↔ MySQL ↔ PostgreSQL 互转 |
| 备份 → 恢复 | `-backup` / `-restore` | 单文件全量备份 |
| 结构升级 | `MIGRATE=1` | 自动迁移表结构 |
| 配置导入 | WebUI / API | 规则、设备组、节点配置的 JSON 导入导出 |

### 7.2 Nyanpass → OpenRoute 迁移

**输入**：Nyanpass 的数据库 DSN（支持 SQLite / MySQL / PostgreSQL）或 `data.db` 文件路径。

**命令**：

```bash
./openroute -migrate from=nyanpass,dsn="sqlite3:///path/to/nyanpass/data.db",dry-run=true
./openroute -migrate from=nyanpass,dsn="mysql://user:pass@tcp(127.0.0.1:3306)/nyanpass",dry-run=false
```

**执行阶段（MUST 分阶段，每阶段可独立重跑）**：

```
阶段 1：连接与探测
  - 连接源库，读取 schema_version，识别 Nyanpass 版本
  - 若版本过旧（缺少必要表或字段），提示先升级 Nyanpass 到较新版本再迁移
  - 打印源库概况：表数量、各表行数

阶段 2：只读扫描与字段映射
  - 逐表扫描，构建「源字段 → 目标字段」映射表
  - 无法映射的字段记录到「丢弃清单」
  - 目标缺失的字段使用默认值并记录到「填充清单」

阶段 3：冲突与风险检测
  - 名称冲突：MySQL → SQLite/PG 时，EqualFold 后重名的用户名/节点名/规则名
  - 端口冲突：同一入口组内多条规则监听同一端口
  - 节点引用缺失：规则引用的设备组/节点在源库中不存在
  - 倍率为 0 的设备组（会导致统计为 0）
  - 授权相关字段（直接丢弃，不迁移）
  - 输出「迁移预检报告」（见下）

阶段 4：dry-run 预览
  - 在内存中完成全部转换，输出：
    · 将写入的行数（按表）
    · 冲突清单与建议处理方式
    · 三条示例转换结果（before → after）
  - dry-run=true 时到此结束，不写任何数据

阶段 5：正式迁移（事务 + 幂等）
  - 全部写入放在一个事务中（SQLite 受限于单写者，分批提交但记录进度）
  - 保留源 ID 映射表：source_id → target_id，写入 migrate_id_map 表
  - 支持断点续传：失败后重跑时跳过已迁移的记录
  - 迁移完成后自动生成配置快照

阶段 6：校验
  - 行数对比（源 vs 目标，按表）
  - 抽样字段比对（随机 100 条，逐字段比）
  - 引用完整性检查（每条规则的入口组/出口组/用户都在目标库存在）
  - 流量数据总量对比（允许 ±0.1% 误差，因为倍率折算）
  - 输出「迁移报告」

阶段 7：回滚
  - `-migrate rollback=<迁移批次 ID>`：按 migrate_id_map 逆向删除本次迁移写入的数据
  - 迁移前自动备份，回滚失败时可直接用备份恢复
```

**「迁移预检报告」样例（MUST 输出此格式）**：

```
═══════════════════════════════════════════════════════
  Nyanpass → OpenRoute 迁移预检报告
═══════════════════════════════════════════════════════
源库      : sqlite3 (./nyanpass/data.db)
源版本    : Nyanpass 20260301
目标库    : sqlite3 (./data.db)
迁移模式  : DRY-RUN（不会写入任何数据）
───────────────────────────────────────────────────────
【将迁移的数据】
  用户            12 条   →  users
  用户分组         3 条   →  user_groups
  节点            28 条   →  nodes
  节点分组         4 条   →  node_groups
  设备组          16 条   →  device_groups（入口 9 / 出口 7）
  规则分组         5 条   →  rule_groups
  转发规则       214 条   →  forward_rules
  流量记录     18204 条   →  traffic_logs（按天聚合后约 900 条）
───────────────────────────────────────────────────────
【冲突与风险】
  ⚠ 名称冲突 2 处（MySQL/PG 大小写敏感）
      · 用户 "Admin" 与 "admin" 在目标库将合并 → 建议重命名为 "admin2"
      · 节点 "HK-01" 与 "hk-01" 同理
  ⚠ 端口冲突 1 处
      · 规则 "test-a" 与 "test-b" 在入口组 "HK-In" 上均监听 8443
  ⚠ 引用缺失 0 处
  ⚠ 倍率为 0 的设备组 1 个 → 统计将显示为 0（源库即为如此）
───────────────────────────────────────────────────────
【不迁移的字段（已识别并丢弃）】
  users.license_key、users.recharge_total、orders.*、products.*、
  payment_configs.*、license_domain、expire_license_at
───────────────────────────────────────────────────────
【建议操作】
  1. 迁移前先执行 -backup 备份目标库
  2. 冲突项建议在源库先改名，或使用 --rename-policy=suffix 自动加后缀
  3. 确认无误后执行：-migrate from=nyanpass,dsn="...",dry-run=false
═══════════════════════════════════════════════════════
```

**字段映射表（核心表 MUST 按下表映射）**：

| 源表 | 目标表 | 关键映射 |
|---|---|---|
| `users` | `users` | 保留用户名、密码哈希（bcrypt 格式一致则直接复用，否则标记「需重置密码」）、分组、流量、限制；丢弃授权与充值字段 |
| `user_groups` | `user_groups` | 名称、限速策略、规则分组白名单 |
| `nodes` | `nodes` | 名称、token（**MUST 重新生成**，避免与旧面板冲突）、角色、IP、端口、权重、分组、备注 |
| `node_groups` | `node_groups` | 名称 |
| `device_groups`（入口） | `device_groups` (type=inbound) | Config JSON **原样迁移**，字段结构完全一致 |
| `device_groups`（出口） | `device_groups` (type=outbound) | Config JSON 原样迁移；Balance 默认 `least_conn` |
| `rule_groups` | `rule_groups` | 名称、排序 |
| `forward_rules` | `forward_rules` | 名称、端口、目标（多目标展开为 Targets 数组）、倍率、限制、归属 |
| 流量记录 | `traffic_logs` | 按 `(date, hour, user_id, rule_id, node_id, direction)` 聚合后写入 |
| 订单/商品/支付配置 | **不迁移** | 记录到丢弃清单 |
| 授权相关 | **不迁移** | 记录到丢弃清单 |

**迁移后必须做的事（MUST 在报告中提示用户）**：

1. **节点密钥全部重新生成**，`/opt/openroute/config.yml` 里的 `token` 与旧面板不同。
2. **节点客户端不兼容**：Nyanpass 节点与 OpenRoute 面板的通信协议不同，MUST 在每台机器上重新安装 OpenRoute 节点：
   ```bash
   bash /opt/openroute/openroute.uninstall.sh      # 卸载旧节点
   # 然后在面板复制新的安装命令执行
   ```
   迁移工具 SHOULD 在节点列表页标注「待重装」状态，并提供批量导出安装命令的功能。
3. **规则需要重新下发**：迁移后所有规则状态为 `unsynced`，节点重装并上线后会自动同步。
4. 若源库使用 MySQL 且目标使用 SQLite，注意大小写敏感差异导致的名称冲突（见预检报告）。

### 7.3 跨数据库转换（`-copy-database`）

```bash
./openroute -copy-database "mysql://user:pass@tcp(127.0.0.1:3306)/openroute?charset=utf8mb4&parseTime=true"
```

**行为**：

1. 打印源库与目标库的信息（方言、地址、表数量、行数）。
2. 打印醒目的红色警告：**目标库将被完全覆盖，且无法恢复**。
3. 要求用户输入 `YES`（全大写）确认；非交互模式下需显式加 `--force`。
4. 自动在目标库执行 `AutoMigrate` 建表。
5. 按**外键依赖顺序**逐表复制（先父表后子表），每表分批（1000 行/批）：
   ```
   users → user_groups → node_groups → nodes
         → rule_groups → device_groups → forward_rules
         → traffic_logs → sessions → probe_metrics
         → system_settings → audit_logs → api_tokens
         → config_snapshots → alert_rules → alert_histories
   ```
6. 复制完成后校验每表行数与校验和，输出对比结果。
7. **类型转换注意**：
   - SQLite 的 `TEXT` JSON 字段 → MySQL 的 `JSON` 列：MUST 校验 JSON 合法性，非法值记录并跳过该行。
   - MySQL 的 `TINYINT(1)` → PG 的 `boolean`：显式转换。
   - 时间字段统一转 UTC 后写入。
   - 大整数（流量字节）在三种库都用 `BIGINT`，不会溢出。

### 7.4 边界情况处理（MUST 覆盖）

| 情况 | 处理方式 |
|---|---|
| 源库为空 | 提示「源库无数据」，不报错，退出码 0 |
| 源库版本过旧，缺少表 | 列出缺失表，提示先升级 Nyanpass，退出码 1 |
| 源库与目标库是同一个库 | 检测到后拒绝执行，退出码 1 |
| 迁移中途断电/崩溃 | 重启后检测 `migrate_id_map` 中的未完成批次，提示「继续 / 回滚 / 放弃」 |
| 磁盘空间不足 | 迁移前预估目标数据量，空间不足时拒绝执行并给出所需空间 |
| 名称冲突且用户未指定策略 | 默认 `--rename-policy=fail`（直接报错），可选 `suffix`（自动加 `_2`）或 `skip`（跳过该条） |
| 密码哈希算法不兼容 | 该用户标记 `password_reset_required = true`，首次登录强制改密 |
| 流量记录跨年跨月 | 按 UTC 日期归属，不做时区重算 |
| 规则引用不存在的设备组 | 该规则标记为「待修复」，不写入，记入报告的「跳过清单」 |

### 7.5 迁移相关数据表

```go
// migrate_batches 迁移批次
type MigrateBatch struct {
    ID          uint64 `gorm:"primaryKey"`
    Source      string `gorm:"size:32"`   // nyanpass | openroute
    SourceDSN   string `gorm:"size:512"`  // 脱敏后存储
    TargetDSN   string `gorm:"size:512"`
    Status      string `gorm:"size:16"`   // pending | running | finished | failed | rolled_back
    Stage       int                       // 当前阶段 1~7
    TotalRows   int64
    MigratedRows int64
    Report      JSON   `gorm:"type:text"` // 完整预检/迁移报告
    StartedAt   time.Time
    FinishedAt  *time.Time
    CreatedAt   time.Time
}

// migrate_id_map ID 映射（用于回滚与增量续传）
type MigrateIDMap struct {
    ID         uint64 `gorm:"primaryKey"`
    BatchID    uint64 `gorm:"index"`
    TableName  string `gorm:"size:64;index"`
    SourceID   uint64 `gorm:"index"`
    TargetID   uint64 `gorm:"index"`
    Checksum   string `gorm:"size:64"` // 该行内容的校验和，用于幂等判断
    CreatedAt  time.Time
}
```

---

## 8. API 接口设计

### 8.1 总则

- **基础路径**：`/api/v1/`（版本化，未来不兼容变更走 `/api/v2/`）
- **`/api/node/*` 除外**：节点通信接口不带版本号（节点与面板版本绑定）
- **协议**：HTTP/1.1 + JSON；`Content-Type: application/json; charset=utf-8`
- **时间格式**：RFC 3339，UTC，如 `2026-01-01T12:00:00Z`
- **流量单位**：字节（整数）
- **分页**：请求 `?page=1&page_size=20`；响应带 `pagination`
- **排序**：`?sort=created_at&order=desc`
- **筛选**：显式参数，不做通用表达式注入
- **幂等**：所有 POST 创建接口支持 `Idempotency-Key` 请求头

### 8.2 认证

| 方式 | 用途 | 请求头 |
|---|---|---|
| Session Cookie | WebUI 浏览器 | `Cookie: or_session=...`（HttpOnly、SameSite=Lax） |
| JWT Bearer | 前端 SPA / 第三方 | `Authorization: Bearer <access_token>` |
| API Token | 服务端对接、脚本 | `X-API-Key: ort_xxxxx` + 可选 `X-API-Timestamp` + `X-API-Sign` |
| 节点 Token | 节点通信 | `X-Node-Token: <node token>` |
| 设备订阅 | 客户端拉配置 | `GET /sub/<user_token>`（返回配置文件文本） |

**API Token 签名（MUST 实现）**：

```
sign = HMAC-SHA256(token_secret, method + "\n" + path + "\n" + timestamp + "\n" + body_sha256)
请求头：X-API-Key、X-API-Timestamp（Unix 秒，允许 ±300 秒偏差）、X-API-Sign
```

**权限范围（Scopes）**：

```
node:read  node:write  node:exec
group:read group:write
rule:read  rule:write
user:read  user:write
traffic:read
system:read system:write
migrate:run
backup:run
```

### 8.3 统一响应体

**成功**：

```json
{
  "code": 0,
  "message": "ok",
  "data": { },
  "request_id": "req_01HXYZ...",
  "timestamp": "2026-01-01T12:00:00Z"
}
```

**列表**：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "items": [],
    "pagination": {
      "page": 1,
      "page_size": 20,
      "total": 214,
      "total_pages": 11
    }
  },
  "request_id": "req_01HXYZ...",
  "timestamp": "2026-01-01T12:00:00Z"
}
```

**失败**：

```json
{
  "code": 40001,
  "message": "节点不存在",
  "details": {
    "field": "node_id",
    "value": 999,
    "hint": "请检查节点 ID 是否正确"
  },
  "request_id": "req_01HXYZ...",
  "timestamp": "2026-01-01T12:00:00Z"
}
```

### 8.4 错误码字典（MUST 完整定义并在前端统一处理）

| 区间 | 类别 |
|---|---|
| `0` | 成功 |
| `40000-40099` | 参数错误 |
| `40100-40199` | 认证失败 |
| `40300-40399` | 权限不足 |
| `40400-40499` | 资源不存在 |
| `40900-40999` | 资源冲突 |
| `42200-42299` | 业务校验失败 |
| `42900-42999` | 触发限流 |
| `50000-50099` | 服务器内部错误 |
| `50300-50399` | 依赖不可用（节点离线、数据库不可写等） |
| `60000-60099` | 节点通信错误 |
| `70000-70099` | 迁移相关错误 |

**常用错误码**：

| 码 | HTTP | 含义 |
|---|---|---|
| `0` | 200 | 成功 |
| `40001` | 400 | 参数缺失或格式错误 |
| `40002` | 400 | 参数值超出范围 |
| `40101` | 401 | 未登录或 Token 无效 |
| `40102` | 401 | Token 已过期 |
| `40103` | 401 | 签名校验失败 |
| `40104` | 401 | 用户名或密码错误 |
| `40301` | 403 | 无权限执行此操作 |
| `40302` | 403 | API Token 缺少所需 Scope |
| `40303` | 403 | IP 不在白名单内 |
| `40304` | 403 | WebSSH 已被禁用 |
| `40401` | 404 | 资源不存在 |
| `40901` | 409 | 名称已存在（归一化比较后重复） |
| `40902` | 409 | 端口已被占用 |
| `40903` | 409 | 该资源正在被引用，无法删除 |
| `42201` | 422 | 故障转移组必须与当前组拥有相同的入口权限配置 |
| `42202` | 422 | 链式出口不支持 UDP |
| `42203` | 422 | 链式出口不支持故障转移组 |
| `42204` | 422 | SNI 不在入口组白名单内 |
| `42205` | 422 | 倍率超出允许范围 |
| `42901` | 429 | 请求过于频繁 |
| `50001` | 500 | 内部错误 |
| `50301` | 503 | 数据库不可写 |
| `60001` | 502 | 节点离线，无法下发配置 |
| `60002` | 502 | 节点应用配置失败 |
| `60003` | 504 | 节点响应超时 |
| `70001` | 400 | 源库无法连接 |
| `70002` | 400 | 源库版本不被支持 |
| `70003` | 409 | 目标库与源库相同 |
| `70004` | 422 | 检测到无法自动处理的冲突，请先处理 |

### 8.5 认证接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/auth/login` | 登录，返回 access_token / refresh_token |
| POST | `/api/v1/auth/logout` | 登出 |
| POST | `/api/v1/auth/refresh` | 刷新 Token |
| GET | `/api/v1/auth/me` | 当前用户信息与权限 |
| POST | `/api/v1/auth/password` | 修改自己的密码 |
| GET | `/api/v1/auth/captcha` | 图形验证码（连续失败 3 次后强制） |

**登录请求**：

```json
{ "username": "admin", "password": "******", "captcha": "" }
```

**登录响应**：

```json
{
  "code": 0,
  "data": {
    "access_token": "eyJ...",
    "refresh_token": "eyJ...",
    "expires_in": 7200,
    "user": { "id": 1, "username": "admin", "role": "admin", "nickname": "管理员" }
  }
}
```

### 8.6 节点接口

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/v1/nodes` | `node:read` | 列表（支持 `?group_id=&online=&keyword=`） |
| POST | `/api/v1/nodes` | `node:write` | 创建 |
| GET | `/api/v1/nodes/:id` | `node:read` | 详情 |
| PUT | `/api/v1/nodes/:id` | `node:write` | 更新 |
| DELETE | `/api/v1/nodes/:id` | `node:write` | 删除 |
| POST | `/api/v1/nodes/:id/install-command` | `node:write` | 生成安装命令 |
| POST | `/api/v1/nodes/:id/upgrade` | `node:exec` | 升级节点客户端 |
| POST | `/api/v1/nodes/:id/restart` | `node:exec` | 重启节点服务 |
| POST | `/api/v1/nodes/:id/exec` | `node:exec` | 在节点执行命令 |
| POST | `/api/v1/nodes/:id/reset-token` | `node:write` | 重置节点密钥 |
| GET | `/api/v1/nodes/:id/metrics` | `node:read` | 探针数据（`?from=&to=&interval=`） |
| GET | `/api/v1/nodes/:id/metrics/realtime` | `node:read` | 实时指标 |
| GET | `/api/v1/nodes/:id/rules` | `rule:read` | 该节点上运行的规则 |
| GET | `/api/v1/nodes/:id/logs` | `node:read` | 节点日志（`?lines=200`） |
| GET | `/api/v1/nodes/:id/drift` | `node:read` | 配置漂移详情 |
| POST | `/api/v1/nodes/:id/drift/fix` | `node:write` | 一键纠正漂移 |
| POST | `/api/v1/nodes/batch/upgrade` | `node:exec` | 批量升级 |
| POST | `/api/v1/nodes/batch/exec` | `node:exec` | 批量执行 |
| POST | `/api/v1/nodes/batch/group` | `node:write` | 批量改分组 |
| POST | `/api/v1/nodes/batch/weight` | `node:write` | 批量改权重 |
| WS | `/api/v1/nodes/stream` | `node:read` | 节点状态实时推送 |
| WS | `/api/v1/nodes/:id/terminal` | `node:exec` | WebSSH |

**创建节点请求**：

```json
{
  "name": "HK-01",
  "role": "both",
  "group_ids": [1, 2],
  "weight": 1,
  "max_conn": 0,
  "remark": "香港入口+出口"
}
```

**创建节点响应**：

```json
{
  "code": 0,
  "data": {
    "id": 12,
    "name": "HK-01",
    "token": "nsk_9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c",
    "install_command": "bash <(curl -fsSL http://1.2.3.4:18888/install.sh) -u http://1.2.3.4:18888 -t nsk_9f8a...",
    "status": "offline"
  }
}
```

### 8.7 节点分组接口

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/api/v1/node-groups` | `group:read` |
| POST | `/api/v1/node-groups` | `group:write` |
| GET/PUT/DELETE | `/api/v1/node-groups/:id` | `group:read` / `group:write` |
| GET | `/api/v1/node-groups/:id/nodes` | `group:read` |

### 8.8 设备组接口

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/v1/device-groups` | `group:read` | `?type=inbound\|outbound` |
| POST | `/api/v1/device-groups` | `group:write` | |
| GET | `/api/v1/device-groups/:id` | `group:read` | 含解析后的 Config |
| PUT | `/api/v1/device-groups/:id` | `group:write` | |
| DELETE | `/api/v1/device-groups/:id` | `group:write` | 被规则引用时返回 `40903` |
| GET | `/api/v1/device-groups/:id/schema` | `group:read` | 返回该类型组的配置字段 schema，供前端动态渲染表单 |
| POST | `/api/v1/device-groups/:id/validate` | `group:write` | 校验配置合法性（含故障转移组兼容性） |
| GET | `/api/v1/device-groups/:id/health` | `group:read` | 组内节点健康状态与负载分布 |
| POST | `/api/v1/device-groups/:id/reorder` | `group:write` | 调整组内节点顺序 |

**创建入口组请求**：

```json
{
  "name": "HK-In",
  "type": "inbound",
  "node_ids": [12, 13],
  "config": {
    "allowed_host": [],
    "blocked_host": [],
    "blocked_path": [],
    "blocked_protocol": [],
    "tls_inbound_policy": 0,
    "tls_reject_empty_sni": false,
    "disable_udp": false,
    "udp_over_tcp": false,
    "ipv6_group": [],
    "max_fail": 3,
    "fail_timout_sec": 30,
    "reverse_group": [],
    "protocol": "tls",
    "tls": { "sni": "some.host.com", "alpn": ["http/1.1"], "chfp": "chrome" }
  }
}
```

**创建出口组请求**：

```json
{
  "name": "JP-Out",
  "type": "outbound",
  "node_ids": [21, 22, 23],
  "balance": "least_conn",
  "health_check_enable": true,
  "health_check_interval": 10,
  "health_check_timeout": 3,
  "health_check_fail_count": 3,
  "health_check_succ_count": 2,
  "failover_group_id": 8,
  "config": {
    "connect_type": "static",
    "connect_address": "jp.example.com",
    "connect_port": 2333,
    "protocol": "ws",
    "ws": { "host": "cdn.example.com", "path": "/ws" },
    "udp_over_tcp": false
  }
}
```

### 8.9 转发规则接口

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/v1/forward-rules` | `rule:read` | `?user_id=&rule_group_id=&inbound_group_id=&outbound_group_id=&sync_status=&enable=&keyword=` |
| POST | `/api/v1/forward-rules` | `rule:write` | |
| GET | `/api/v1/forward-rules/:id` | `rule:read` | |
| PUT | `/api/v1/forward-rules/:id` | `rule:write` | |
| DELETE | `/api/v1/forward-rules/:id` | `rule:write` | |
| POST | `/api/v1/forward-rules/:id/enable` | `rule:write` | |
| POST | `/api/v1/forward-rules/:id/disable` | `rule:write` | |
| POST | `/api/v1/forward-rules/:id/resync` | `rule:write` | 强制重新下发 |
| GET | `/api/v1/forward-rules/:id/traffic` | `traffic:read` | `?from=&to=&interval=hour\|day` |
| GET | `/api/v1/forward-rules/:id/sessions` | `rule:read` | 当前会话列表 |
| POST | `/api/v1/forward-rules/batch` | `rule:write` | 批量操作（动作在 body 中声明） |
| POST | `/api/v1/forward-rules/import` | `rule:write` | 导入（`?preview=true` 先预览） |
| GET | `/api/v1/forward-rules/export` | `rule:read` | 导出 JSON |
| POST | `/api/v1/forward-rules/batch-multiplier` | `rule:write` | 批量调倍率（按比例或按值） |

**创建规则请求**：

```json
{
  "name": "rule-hk-jp-01",
  "user_id": 1,
  "rule_group_id": 2,
  "inbound_group_id": 1,
  "listen_port": 8443,
  "outbound_group_id": 5,
  "targets": [
    { "host": "1.2.3.4", "port": 443, "weight": 1 },
    { "host": "5.6.7.8", "port": 443, "weight": 1 }
  ],
  "target_balance": "failover",
  "inbound_multiplier": 1.5,
  "outbound_multiplier": 0.5,
  "speed_limit": 0,
  "conn_limit": 0,
  "ip_limit": 0,
  "reverse_enable": false,
  "chain_groups": [],
  "remark": ""
}
```

**批量操作请求**：

```json
{
  "action": "set_multiplier",
  "ids": [1, 2, 3],
  "params": { "inbound_multiplier": 2.0, "outbound_multiplier": 1.0 }
}
```

`action` 可选值：`enable`、`disable`、`delete`、`set_rule_group`、`set_multiplier`、`scale_multiplier`、`set_outbound_group`、`set_limits`、`set_chain`。

**导入预览响应**：

```json
{
  "code": 0,
  "data": {
    "total": 50,
    "will_create": 46,
    "conflicts": [
      { "line": 7,  "name": "test-a", "reason": "名称已存在", "suggestion": "改名为 test-a-2" },
      { "line": 12, "name": "test-b", "reason": "端口 8443 已被占用", "suggestion": "改用随机端口" }
    ],
    "invalid": [
      { "line": 20, "raw": "bad#line", "reason": "格式错误：缺少目标端口" }
    ]
  }
}
```

### 8.10 规则分组接口

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/api/v1/rule-groups` | `rule:read` |
| POST | `/api/v1/rule-groups` | `rule:write` |
| GET/PUT/DELETE | `/api/v1/rule-groups/:id` | `rule:read` / `rule:write` |
| POST | `/api/v1/rule-groups/reorder` | `rule:write` |

### 8.11 用户接口

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/v1/users` | `user:read` | |
| POST | `/api/v1/users` | `user:write` | 手工建号 |
| GET/PUT/DELETE | `/api/v1/users/:id` | `user:read` / `user:write` | |
| POST | `/api/v1/users/:id/reset-password` | `user:write` | |
| POST | `/api/v1/users/:id/reset-token` | `user:write` | 重置订阅 Token |
| GET | `/api/v1/users/:id/traffic` | `traffic:read` | |
| GET | `/api/v1/users/:id/rules` | `user:read` | |
| POST | `/api/v1/users/:id/disable` | `user:write` | |
| GET | `/api/v1/user-groups` | `group:read` | |
| POST | `/api/v1/user-groups` | `group:write` | |
| PUT/DELETE | `/api/v1/user-groups/:id` | `group:write` | |
| GET | `/api/v1/sub/:token` | 无需认证 | 订阅：返回该用户可用的规则配置文本 |

### 8.12 流量统计接口

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/v1/traffic/overview` | `traffic:read` | 全站概览（今日/昨日/本月/累计/在线数） |
| GET | `/api/v1/traffic/timeseries` | `traffic:read` | 时间序列（`?from=&to=&interval=&group_by=user\|rule\|node\|direction`） |
| GET | `/api/v1/traffic/top` | `traffic:read` | Top N 排行（`?dimension=user\|rule\|node&limit=10`） |
| GET | `/api/v1/traffic/export` | `traffic:read` | 导出 CSV |
| GET | `/api/v1/traffic/dashboard` | `traffic:read` | 首页仪表盘聚合数据（一次请求拿全） |

### 8.13 探针与监控接口

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/api/v1/probe/overview` | `node:read` |
| GET | `/api/v1/probe/nodes/:id` | `node:read` |
| GET | `/api/v1/probe/cleanup` | `system:write` |

### 8.14 系统与设置接口

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/v1/system/info` | `system:read` | 版本、运行时长、数据库方言、节点数、规则数 |
| GET | `/api/v1/system/status` | `system:read` | 健康检查（含数据库、任务调度、节点通信） |
| GET | `/api/v1/system/version` | `system:read` | 后端与节点客户端版本清单 |
| GET | `/api/v1/settings` | `system:read` | 全部设置 |
| PUT | `/api/v1/settings` | `system:write` | 批量更新 |
| GET | `/api/v1/audit-logs` | `system:read` | 审计日志 |
| GET | `/api/v1/tasks` | `system:read` | 任务列表 |
| POST | `/api/v1/tasks/:name/run` | `system:write` | 手动触发任务 |
| GET | `/api/v1/alerts` | `system:read` | 告警规则与历史 |
| POST | `/api/v1/alerts` | `system:write` | |
| POST | `/api/v1/alerts/:id/test` | `system:write` | 测试通知渠道 |
| POST | `/api/v1/webhooks/test` | `system:write` | |
| GET | `/api/v1/snapshots` | `system:read` | 配置快照 |
| POST | `/api/v1/snapshots` | `system:write` | 手动生成快照 |
| POST | `/api/v1/snapshots/:id/rollback` | `system:write` | 回滚 |
| GET | `/api/v1/snapshots/:id/diff` | `system:read` | 与当前配置对比 |
| GET | `/api/v1/api-tokens` | `system:write` | API Token 管理 |
| POST | `/api/v1/api-tokens` | `system:write` | 创建（明文仅返回一次） |
| DELETE | `/api/v1/api-tokens/:id` | `system:write` | |

### 8.15 迁移接口

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/api/v1/migrations/precheck` | `migrate:run` | 上传/指定 DSN，返回预检报告 |
| POST | `/api/v1/migrations/run` | `migrate:run` | 执行迁移（`?dry_run=true|false`） |
| GET | `/api/v1/migrations` | `migrate:run` | 迁移批次列表 |
| GET | `/api/v1/migrations/:id` | `migrate:run` | 批次详情与报告 |
| POST | `/api/v1/migrations/:id/rollback` | `migrate:run` | 回滚该批次 |
| GET | `/api/v1/migrations/:id/progress` | `migrate:run` | 实时进度（SSE 或轮询） |
| POST | `/api/v1/backups` | `backup:run` | 生成备份 |
| GET | `/api/v1/backups` | `backup:run` | 备份列表 |
| GET | `/api/v1/backups/:id/download` | `backup:run` | 下载 |
| POST | `/api/v1/backups/:id/restore` | `backup:run` | 恢复 |

### 8.16 节点通信接口（不带 `/v1`）

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| POST | `/api/node/register` | token（body） | 注册并获取初始配置 |
| POST | `/api/node/heartbeat` | `X-Node-Token` | 心跳上报 |
| GET | `/api/node/config` | `X-Node-Token` | 拉取配置（`?version=`） |
| POST | `/api/node/report` | `X-Node-Token` | 上报同步结果与流量 |
| GET | `/api/node/tasks` | `X-Node-Token` | 拉取任务 |
| POST | `/api/node/task-result` | `X-Node-Token` | 上报任务结果 |
| GET | `/api/node/install.sh` | 无 | 安装脚本（按 token 参数模板化） |
| GET | `/api/node/binary/:arch` | 无（校验签名） | 节点二进制下载 |

**心跳请求**：

```json
{
  "node_id": 12,
  "version": "nc20260101",
  "config_version": 1024,
  "metrics": {
    "cpu": 12.5, "mem_used": 536870912, "mem_total": 2147483648,
    "disk_used": 10737418240, "disk_total": 42949672960,
    "net_in": 123456789, "net_out": 987654321,
    "net_in_speed": 1048576, "net_out_speed": 2097152,
    "load1": 0.35, "load5": 0.42, "load15": 0.38,
    "tcp_conn": 128, "udp_conn": 6,
    "uptime": 864000
  },
  "running_rules": [
    { "rule_id": 1, "port": 8443, "status": "running", "conn": 42 }
  ],
  "timestamp": 1767225600
}
```

**配置响应**：

```json
{
  "config_version": 1025,
  "full": false,
  "rules": [
    {
      "rule_id": 1,
      "name": "rule-hk-jp-01",
      "listen_port": 8443,
      "protocol": "tls",
      "targets": [{ "host": "1.2.3.4", "port": 443 }],
      "options": { "tls": { "sni": "some.host.com", "chfp": "chrome" } },
      "speed_limit": 0,
      "conn_limit": 0,
      "ip_limit": 0,
      "enable": true
    }
  ],
  "removed_rule_ids": [7, 8],
  "device_group_config": { },
  "heartbeat_interval": 10
}
```

### 8.17 速率限制与并发

| 接口类别 | 限制 |
|---|---|
| 登录 | 5 次 / 分钟 / IP；连续失败 3 次要求验证码；10 次失败锁定 15 分钟 |
| 普通读接口 | 由 `user-rate-limit` 控制（默认 5 req/s，突发 5） |
| 写接口 | 2 req/s，突发 3 |
| 批量操作 | 1 req/2s |
| 节点心跳 | 不限（节点侧固定 10s 一次） |
| 订阅接口 | 10 次 / 分钟 / Token |
| WebSSH | 每用户最多 3 个并发终端 |

### 8.18 Webhook（出站通知）

面板在以下事件发生时 POST 到用户配置的 Webhook 地址：

| 事件 | `event` 字段 |
|---|---|
| 节点上线 | `node.online` |
| 节点离线 | `node.offline` |
| 规则同步失败 | `rule.sync_failed` |
| 规则同步恢复 | `rule.sync_ok` |
| 触发告警 | `alert.fired` |
| 告警恢复 | `alert.resolved` |
| 迁移完成 | `migration.finished` |
| 备份完成 | `backup.finished` |

**请求体统一格式**：

```json
{
  "event": "node.offline",
  "timestamp": "2026-01-01T12:00:00Z",
  "data": { "node_id": 12, "node_name": "HK-01", "offline_seconds": 25 }
}
```

请求头带 `X-OpenRoute-Event` 与 `X-OpenRoute-Signature`（HMAC-SHA256，密钥为 `secret-key`）。失败重试 3 次（间隔 5s / 30s / 300s）。

### 8.19 OpenAPI 与文档

- 面板 MUST 提供 `docs/openapi.yaml`（OpenAPI 3.1）。
- 面板 MUST 在 `/api/docs` 提供 Swagger UI 或 Scalar 渲染的交互式文档（可开关）。
- 面板 MUST 提供 `docs/API.md`，包含：认证方式、快速开始（curl 示例）、全部接口清单、错误码字典、Webhook 说明、速率限制说明。
- 面板 MUST 提供 `GET /api/v1/system/openapi.json` 运行时输出当前版本的 OpenAPI 描述。

---

## 9. 前端规格

### 9.1 工程结构

```
frontend/
├── index.html
├── vite.config.ts
├── tsconfig.json
├── package.json
├── src/
│   ├── main.tsx
│   ├── App.tsx
│   ├── router/
│   │   └── index.tsx            # 路由表 + 权限守卫
│   ├── api/
│   │   ├── client.ts            # axios 实例、拦截器
│   │   ├── types.ts             # 与后端对齐的 TS 类型（可由 openapi 生成）
│   │   └── modules/             # 按资源拆分的 API 函数
│   ├── store/                   # Zustand stores
│   ├── layouts/
│   │   ├── BasicLayout.tsx      # 侧边栏 + 顶栏
│   │   └── BlankLayout.tsx      # 登录页
│   ├── pages/
│   │   ├── login/
│   │   ├── dashboard/
│   │   ├── nodes/
│   │   ├── node-groups/
│   │   ├── device-groups/
│   │   ├── forward-rules/
│   │   ├── rule-groups/
│   │   ├── users/
│   │   ├── traffic/
│   │   ├── monitor/
│   │   ├── alerts/
│   │   ├── snapshots/
│   │   ├── migration/
│   │   ├── snapshots/
│   │   └── settings/
│   ├── components/
│   │   ├── JsonEditor/          # 带 schema 校验的 JSON 编辑器
│   │   ├── DeviceGroupForm/     # 入口组/出口组动态表单
│   │   ├── TargetList/          # 多目标地址编辑器（可拖拽排序）
│   │   ├── TrafficChart/
│   │   ├── NodeCard/
│   │   ├── Terminal/            # xterm.js 封装
│   │   ├── SyncStatusTag/
│   │   └── CopyableCommand/
│   ├── hooks/
│   │   ├── useWebSocket.ts
│   │   ├── usePermission.ts
│   │   └── usePolling.ts
│   ├── locales/                 # zh-CN / en-US
│   └── styles/
│       ├── variables.css        # 主题变量（经典 / 透明）
│       └── global.css
```

### 9.2 页面清单与要点

| 页面 | 关键内容 |
|---|---|
| **登录页** | 用户名、密码、验证码（按需）、主题切换 |
| **仪表盘** | 全站流量卡片（今日/昨日/本月/累计）、在线节点数、在线用户数、活跃规则数、实时流量曲线、节点状态格子、待处理事项（同步失败规则、离线节点、活跃告警） |
| **节点管理** | 表格（可自定义列）、在线状态点、安装命令弹窗、批量操作栏、健康度评分、配置漂移标记 |
| **节点详情** | 标签页：概览 / 监控曲线 / 运行规则 / 日志 / 终端 / 配置 |
| **节点分组** | 简单 CRUD + 拖拽排序 |
| **设备组** | 列表分「入口组」「出口组」两个 Tab；创建/编辑用动态表单；入口组表单覆盖 4.3 全部字段；出口组表单覆盖 4.4 + 负载均衡 + 健康检查 + 故障转移组 |
| **转发规则** | 表格 + 高级筛选 + 批量操作栏 + 导入导出；行内显示同步状态；展开行显示目标列表与实时连接数 |
| **规则分组** | CRUD + 排序 |
| **用户管理** | CRUD、重置密码、重置 Token、查看该用户的规则与流量 |
| **流量统计** | 时间范围选择器 + 维度切换（用户/规则/节点/方向）+ 折线图 + 表格 + 导出 |
| **监控** | 所有节点缩略网格 + 关键指标 sparkline；点击进入节点详情 |
| **告警** | 告警规则 CRUD + 活跃告警 + 历史告警 |
| **快照** | 快照列表 + 差异对比（左右分栏 diff）+ 一键回滚 |
| **迁移** | 选择源（Nyanpass / OpenRoute / 备份文件）+ DSN 输入 + 预检报告展示（分区块着色）+ 逐阶段进度条 + 最终报告 |
| **设置** | 基础设置（站点名/Logo/主题）、安全设置（密码/API Token）、通知设置（Webhook/Telegram/邮件）、系统信息、审计日志 |

### 9.3 前端交互约定

- **统一错误处理**：axios 拦截器捕获 `code != 0`，用 `message.error` 展示 `message` 字段；`details.hint` 存在时拼在第二行。
- **统一 Loading**：表格用 `Table.loading`，表单提交用按钮 `loading`，页面初次加载用 `Skeleton`。
- **删除确认**：一律用 `Modal.confirm`，标题写明「确定删除 XXX？」，正文列出影响范围（如「该分组下有 12 条规则」）。
- **危险操作**：迁移、回滚、批量删除等，要求输入资源名或 `YES` 二次确认。
- **表单校验**：与后端 `422xx` 错误码对应；后端返回的 `field` 用于定位到具体表单项。
- **实时刷新**：节点状态用 WebSocket；流量曲线默认 30 秒轮询，可切换 10 秒。
- **空状态**：所有列表提供有引导性的空状态（如「还没有节点，点击右上角添加」）。
- **响应式**：最小支持 1280px 宽；低于 1280px 时侧边栏折叠。
- **主题**：CSS Variables 实现，切换 `classic` / `transparent` 时无需重新加载。
- **国际化**：所有文案走 i18n，默认 `zh-CN`，可切 `en-US`。

### 9.4 前端构建与部署

```bash
cd frontend
npm install
npm run build          # 输出到 ../public
```

后端 `html-path: ./public` 指向该目录，单个 Go 二进制即可同时提供 API 与 WebUI。

---

## 10. 部署规格（个人自用简化版）

### 10.1 部署前提（与 Nyanpass 的差异，重点）

| 项目 | Nyanpass 要求 | OpenRoute 要求 |
|---|---|---|
| HTTPS | 强制 | **不要求**。用 `http://IP:端口` 直接访问 |
| 域名 | 强制 | **不要求** |
| 授权码 | 强制 | **不要求** |
| 端口 | 仅 443/80 | **任意端口** |
| 数据库 | 需外部服务 | **默认 SQLite 文件** |
| 反向代理 | 必须 | **可选** |
| 部署方式 | 必须 docker compose | **单二进制直跑**，docker 可选 |

### 10.2 方式一：单二进制（推荐，个人自用）

```bash
# 1. 放好文件
mkdir -p /opt/openroute && cd /opt/openroute
# 放入 rel_backend（重命名为 openroute）、public/ 目录

# 2. 首次启动并创建管理员
MIGRATE=1 ADMIN="admin" ./openroute

# 3. 浏览器访问
#    http://<服务器IP>:18888/
```

**systemd 服务**：

```ini
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

### 10.3 方式二：Docker Compose

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

### 10.4 配置 HTTPS（可选，若后续需要）

面板本身不强制 HTTPS。若需要：

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
        proxy_read_timeout 3600s;   # WebSSH 需要长连接
    }
}
```

> WebSSH 与实时推送依赖 WebSocket，反代 MUST 保留 `Upgrade` / `Connection` 头。

### 10.5 升级流程

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
# 或
docker compose up -d
```

### 10.6 常见问题（写进 WebUI 帮助页）

| 现象 | 原因与处理 |
|---|---|
| 面板打不开 | 检查进程是否在跑、端口是否被占用、防火墙是否放行 |
| 502 / 连接被拒 | 后端未启动；`journalctl -fu openroute` 看日志 |
| 页面空白 | `html-path` 指向错误，或 `public/` 内容不完整 |
| 登录后立即掉线 | `secret-key` 每次启动都变了（检查配置文件是否可写） |
| 规则一直「未同步」 | 节点离线；或节点的 `base-url` / `token` 不正确 |
| CPU / 内存占用高 | 规则数量过多、日志级别为 debug、开启了全量探针；调整 `log-level` |
| 已用流量一直是 0 | 用户所属设备组倍率被设为 0；或流量采集任务被 `disable-cron` 关闭 |
| WebSSH 打不开 | `enable-webssh: false`；或节点设了 `DISABLE_EXECUTE=1`；或反代未放行 WebSocket |
| 迁移后节点全部离线 | 正常现象：节点 token 已重新生成，需卸载旧节点并重新安装 |
| 数据库文件损坏 | 用 `-restore` 恢复最近的备份；SQLite 可用 `.recover` 尝试抢救 |

---

## 11. 建议新增的功能（Nyanpass 没有的）

按实现优先级排序，前 6 项**强烈建议在 v1.0 就做**：

1. **稳定公开 API + OpenAPI 文档**（用户核心诉求）
   - 版本化路径、统一响应体、错误码字典、API Token 与签名、Webhook。
   - 提供 `openapi.yaml` 与 `/api/docs` 交互式文档。

2. **配置快照与一键回滚**
   - 每次批量变更前自动快照，出问题 10 秒内回滚。

3. **规则同步可视化与失败诊断**
   - 每条规则的同步状态、失败原因原文、按节点分组的同步总览。

4. **配置漂移检测**
   - 面板期望配置 vs 节点实际配置的差异比对，一键纠正。

5. **单文件备份 / 恢复**
   - `-backup` / `-restore`，把「数据库 + 配置 + 清单」打包成一个文件。

6. **告警与 Webhook 通知**
   - 节点离线、同步失败、流量超阈值、磁盘将满，推送到 Telegram / Webhook / 邮件。

7. **节点健康度评分**
   - 综合在线率、失败率、资源水位给出评分，快速识别劣质节点。

8. **多目标地址的图形化编辑与故障转移**
   - 拖拽排序、权重滑块、单个目标启停、实时探测状态。

9. **规则模板与快速克隆**
   - 保存常用配置为模板，一键套用到新规则。

10. **链路拓扑图**
    - 用图形展示「用户 → 入口组 → （链式） → 出口组 → 目标」，一眼看清数据路径。

11. **配置即代码（GitOps 风格）**
    - 把设备组与规则导出为 YAML，支持 `openroute -apply config.yaml` 声明式应用，适合版本化管理。

12. **规则灰度发布**
    - 新规则先在单个节点生效，观察无异常后再全量下发。

13. **流量预测与配额提醒**
    - 基于近 7 天趋势预测月末用量，超阈值提前提醒。

14. **审计日志的完整回溯**
    - 记录每次变更的 before/after 快照，支持「谁在什么时间改了什么」。

15. **只读账号与权限细分**
    - 给运维同学一个只能看不能改的账号。

16. **命令行客户端 `ortctl`**
    - 与 API 配套的 CLI，便于脚本化运维。

17. **节点自动发现**
    - 同网段节点通过广播互相发现，减少手工录入。

18. **TCP 连接质量看板**
    - 每个出口的连接建立成功率、平均握手耗时、重传率，客观衡量线路质量。

19. **订阅链接与二维码**
    - 用户拿到一个订阅地址，自动生成客户端配置（保留 Nyanpass 的做法但简化）。

20. **国际化与暗色模式**
    - 中英双语，暗色/亮色跟随系统。

---

## 12. 开发里程碑与验收标准

### 12.1 里程碑划分

| 阶段 | 周期 | 交付内容 | 验收标准 |
|---|---|---|---|
| **M1 骨架** | 1 周 | 后端启动流程、配置加载、SQLite 建表、CLI 参数、日志、统一响应体、登录认证 | `MIGRATE=1 ADMIN=admin ./openroute` 能启动，浏览器能登录看到空仪表盘 |
| **M2 节点** | 1.5 周 | 节点 CRUD、一键对接、安装脚本、心跳、探针采集、WebSSH | 添加节点 → 执行安装命令 → 节点显示在线并上报指标 → 终端能打开 |
| **M3 转发核心** | 2.5 周 | 设备组、转发规则、协议配置、同步机制、单端转发跑通 | 建一条规则 → 节点同步为「正常」→ 用 curl 验证端口转发可用 |
| **M4 隧道与高级** | 2.5 周 | ws/http/tls 隧道、出口负载均衡、故障转移、反向隧道、链式出口、SNI 分流、UDP | 入口/出口分离转发跑通；断开一个出口后流量自动转移；SNI 分流能按域名分发 |
| **M5 统计与限制** | 1.5 周 | 流量统计三口径、倍率计费、限速、连接/IP 限制、监控图表 | 流量数字与预期一致（用 6.4 的示例核对）；限速生效；IP 限制生效 |
| **M6 迁移与备份** | 1.5 周 | Nyanpass 迁移、跨库转换、备份恢复、快照回滚 | 用一份 Nyanpass 样例库完成迁移并核对行数与流量；备份能恢复到干净环境 |
| **M7 API 与文档** | 1 周 | 全部 API、API Token、Webhook、OpenAPI、文档站 | `openapi.yaml` 通过校验；用 curl 完成一次完整的「建节点→建组→建规则→查流量」流程 |
| **M8 前端完善** | 2 周 | 全部页面、主题、i18n、空状态、错误处理、响应式 | 全部页面可用；无控制台报错；主题与语言切换正常 |
| **M9 加固与发布** | 1 周 | 单测、压测、边界情况、部署文档、常见问题 | 核心 service 单测覆盖 > 70%；1000 条规则下面板响应 < 500ms |

**总计约 15 周**（单人全职）。若只做核心（M1~M5 + 简化前端），约 8 周。

### 12.2 全局验收标准（MUST 全部满足）

1. **零外部依赖启动**：在一台干净的 Debian 12 上，只放二进制与 `public/`，执行 `MIGRATE=1 ADMIN=admin ./openroute` 即可跑起来，无需数据库、域名、证书、授权。
2. **单端口访问**：一个端口同时提供 API 与 WebUI。
3. **节点一键对接**：从点击「添加节点」到节点显示在线，耗时不超过 2 分钟（含复制粘贴命令）。
4. **转发可用**：用 `curl` 通过入口端口访问目标服务，返回正确内容。
5. **面板失联不影响转发**：停掉面板进程，节点转发在 5 分钟内保持正常。
6. **规则同步及时**：修改规则后 20 秒内节点生效（正常网络）。
7. **故障转移有效**：手动关闭一个出口节点，新连接在 30 秒内转移到其它出口。
8. **流量统计准确**：按 6.4 的示例核对，误差小于 1%。
9. **迁移可回滚**：执行一次迁移后能完整回滚到迁移前状态。
10. **API 完整**：`openapi.yaml` 中定义的接口全部可用，且与实现一致（用工具校验）。
11. **无支付、无授权**：代码中不出现任何支付网关、订单、授权码、域名绑定的逻辑与依赖。
12. **文档齐全**：`docs/DEPLOY.md`、`docs/API.md`、`docs/openapi.yaml`、WebUI 内置帮助页。

---

## 13. 给 AI 编码助手的执行清单

按顺序执行，每完成一项打勾：

- [ ] **第 1 步：搭骨架**。按 2.2 建立目录结构，实现配置加载（3.1）、启动流程（3.3）、CLI（3.2）、日志、统一响应体（8.3）、错误码（8.4）。
- [ ] **第 2 步：建数据模型**。按第 4 章实现全部 Gorm 模型与自定义 `JSONField` 类型；实现跨方言的建表与 UPSERT；实现 `schema_version`。
- [ ] **第 3 步：认证与用户**。实现登录、JWT、Session、API Token（8.2）、权限中间件、用户与用户分组 CRUD（8.11）。
- [ ] **第 4 步：节点子系统**。实现节点 CRUD（8.6）、一键对接与安装脚本（5.1）、心跳与离线判定（3.1 的时间参数）、探针（6.11）、WebSSH（6.1）、节点命令（5.5）、环境变量（5.3）。
- [ ] **第 5 步：设备组**。实现入口组与出口组 CRUD（8.8），完整支持 4.3 / 4.4 的配置 JSON 全部字段，并在保存时做兼容性校验（如故障转移组权限一致、链式不支持 UDP）。
- [ ] **第 6 步：转发规则**。实现规则 CRUD、批量操作、旧文本格式与 JSON 格式的导入导出（6.4）、同步状态机（5.4）、倍率计算（6.4 的公式）。
- [ ] **第 7 步：隧道与高级转发**。ws / http / tls 协议参数（4.5）、入口直出（6.5）、反向隧道（6.6）、链式出口（6.7）、SNI 分流（6.8）、UDP/UoT、出口负载均衡与故障转移（6.3 的算法）、连接地址优先级（4.3）。
- [ ] **第 8 步：流量与限制**。流量三口径统计（6.10）、限速与连接/IP 限制（6.9，注意 UDP 与入口独立的语义）。
- [ ] **第 9 步：迁移与备份**。按第 7 章实现 Nyanpass 迁移的 7 个阶段、预检报告、跨库转换、边界处理、备份恢复。
- [ ] **第 10 步：快照、告警、漂移**。实现 6.12 / 6.13 / 6.14。
- [ ] **第 11 步：API 全量**。按第 8 章补齐全部接口，生成 `openapi.yaml`，实现 `/api/docs`。
- [ ] **第 12 步：前端**。按第 9 章实现全部页面。
- [ ] **第 13 步：部署与文档**。按第 10 章写 `docs/DEPLOY.md`，按 12.2 逐条自测。

**每次提交前自检**：

1. 有没有引入支付、订单、套餐、授权、域名绑定的代码？有则删除。
2. 新增的按名称查重逻辑，是否用了 `EqualFold` 归一化？
3. 跨库写入是否用了 Gorm 的 `clause.OnConflict` 而不是手写 SQL？
4. 节点离线时，转发逻辑是否仍能独立运行？
5. 新的 API 是否已加入 `openapi.yaml` 与错误码字典？
6. 危险操作是否有二次确认与审计日志？

---

## 14. 附录

### A. 完整设备组配置字段速查

**入口组 `config`**：

```
allowed_host            数组<string>   目标域名白名单（后缀匹配）
blocked_host            数组<string>   目标域名黑名单
blocked_path            数组<string>   路径前缀黑名单
blocked_protocol        数组<string>   socks | fet | http | tls
tls_inbound_policy      整数           0 不处理 | 1 剥离 TLS | 2 SNI 分流
tls_reject_empty_sni    布尔
disable_udp             布尔
udp_over_tcp            布尔
ipv6_group              数组<int>      [] | [0] | [0,1,2]
max_fail                整数           默认 3
fail_timout_sec         整数           默认 30
reverse_group           数组<int>      反向隧道出口组 ID
protocol                字符串         ws | http | tls | direct
ws.host / ws.path / ws.request / ws.response
tls.sni / tls.alpn / tls.chfp
```

**出口组 `config` + 表字段**：

```
connect_type            dyn_ip4 | dyn_ip6 | static
connect_address         字符串
connect_port            整数
protocol                ws | http | tls | direct
ws / tls                同上
udp_over_tcp            布尔
balance                 least_conn | round_robin | hash_ip | weighted
health_check_enable     布尔
health_check_interval   整数（秒）
health_check_timeout    整数（秒）
health_check_fail_count 整数
health_check_succ_count 整数
failover_group_id       整数
node_ids                数组<int>（有序，含权重）
```

### B. 节点环境变量速查

```
DISABLE_EXECUTE=1        禁用 WebSSH 与远程升级
BIND_INBOUND=eth0,eth1   限定用户规则监听的网卡或IP，多个用逗号分隔
BIND_OUTBOUND_4=1.2.3.4  限定出口 IPv4 源地址
BIND_OUTBOUND_6=::1      限定出口 IPv6 源地址
OUTBOUND_FWMARK=100      出站打 mark（配合策略路由）
COUNT_INTERFACE=eth0     探针统计网卡
HEALTH_CHECK=0           禁用主动健康检查
UUID=<唯一标识>           多实例部署必需
NYA_PROXY=socks5://...   节点出站代理
HTTPS_PROXY=http://...   标准代理变量
```

### C. 隧道协议配置速查

```jsonc
// ws / http（http 协议复用同一份 ws 配置，仅少了 Upgrade 头）
{ "ws": {
    "host": "some.host.com",
    "path": "/some/path",
    "request":  "GET / HTTP/1.5\r\n\r\n",
    "response": "HTTP/1.5 200 OK\r\n\r\n"
} }

// tls
{ "tls": {
    "sni": "some.host.com",
    "alpn": ["http/1.1"],
    "chfp": "chrome"      // chrome|firefox|safari|ios|android|edge|360|qq
} }
```

### D. 批量规则文本格式

```
名称#监听端口#目标地址#目标端口         单端口
名称##目标地址#目标端口                 随机端口
名称#8443-8450#1.2.3.4#443              端口段
```

### E. 术语中英对照

| 中文 | 英文 |
|---|---|
| 入口 / 出口 | inbound / outbound |
| 设备组 | device group |
| 转发规则 | forward rule |
| 隧道协议 | tunnel protocol |
| 负载均衡 | load balancing |
| 故障转移 | failover |
| 倍率 | multiplier |
| 探针 | probe |
| 分流 | SNI split |
| 整流 | shaping |
| 快照 | snapshot |
| 漂移 | drift |
| 迁移 | migration |

---

## 15. 一句话给 AI 的最终指令

> 请严格按照本文档，用 Go（Gin + Gorm）实现后端、用 TypeScript（Vite + React + Ant Design）实现前端，从零构建 OpenRoute 面板。**不实现任何支付、订单、套餐、授权码、域名绑定相关的功能**，默认使用 SQLite，用一个端口同时提供 API 与 WebUI，支持从 Nyanpass 迁移数据并可回滚，提供完整的 REST API 与 OpenAPI 文档。每完成一个模块，对照第 12 章的验收标准自测，对照第 13 章的清单打勾。
