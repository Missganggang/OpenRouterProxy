# OpenRoute API 文档

本文档面向需要对接 OpenRoute 面板的开发者，包含认证方式、快速开始、
全部接口清单、错误码字典、Webhook 说明与速率限制说明。

- 基础路径：`/api/v1/`
- 节点通信接口例外：`/api/node/*`（不带版本号，与节点客户端版本绑定）
- 协议：HTTP/1.1 + JSON，`Content-Type: application/json; charset=utf-8`
- 时间格式：RFC 3339，UTC，如 `2026-01-01T12:00:00Z`
- 流量单位：字节（整数）
- 交互式文档：`/api/docs`（可通过设置关闭）
- 机器可读描述：`/api/v1/system/openapi.json`，静态副本 `docs/openapi.yaml`

---

## 1. 认证

OpenRoute 支持四种认证方式，按请求头自动识别（优先级从高到低）。

| 方式 | 用途 | 请求头 / Cookie |
|---|---|---|
| Session Cookie | WebUI 浏览器 | `Cookie: or_session=...`（HttpOnly、SameSite=Lax） |
| JWT Bearer | 前端 SPA / 第三方 | `Authorization: Bearer <access_token>` |
| API Token | 服务端对接、脚本 | `X-API-Key: ort_xxxxx`（可选签名） |
| 节点 Token | 节点通信 | `X-Node-Token: <node token>` |

### 1.1 登录获取令牌

```bash
curl -X POST http://127.0.0.1:18888/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"你的密码"}'
```

响应：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "access_token": "eyJ...",
    "refresh_token": "eyJ...",
    "expires_in": 7200,
    "user": { "id": 1, "username": "admin", "role": "admin", "nickname": "管理员" }
  },
  "request_id": "req_xxx",
  "timestamp": "2026-01-01T12:00:00Z"
}
```

`access_token` 有效期 2 小时，`refresh_token` 有效期 7 天。用刷新接口换新：

```bash
curl -X POST http://127.0.0.1:18888/api/v1/auth/refresh \
  -H 'Content-Type: application/json' \
  -d '{"refresh_token":"eyJ..."}'
```

### 1.2 使用 API Token

在「设置 → 安全设置 → API 令牌」创建，**明文只在创建时返回一次**。

最简用法（只带 `X-API-Key`）：

```bash
curl http://127.0.0.1:18888/api/v1/nodes \
  -H 'X-API-Key: ort_xxxxxxxx'
```

### 1.3 API Token 签名（可选但推荐）

带签名的请求额外提供完整性与防重放保护：

```
sign = HMAC-SHA256(token_secret, method + "\n" + path + "\n" + timestamp + "\n" + body_sha256)
```

- `method`：HTTP 方法，大写，如 `POST`
- `path`：请求路径，不含查询串，如 `/api/v1/nodes`
- `timestamp`：Unix 秒字符串，服务端允许 ±300 秒偏差
- `body_sha256`：请求体的 SHA256 十六进制摘要，无请求体时为空串的摘要
- `token_secret`：即 `X-API-Key` 的值本身

请求头：`X-API-Key`、`X-API-Timestamp`、`X-API-Sign`

示例（bash + openssl）：

```bash
KEY="ort_xxxxxxxx"
TS=$(date +%s)
BODY='{"name":"HK-01","role":"both"}'
BODY_HASH=$(printf '%s' "$BODY" | sha256sum | cut -d' ' -f1)
SIGN=$(printf 'POST\n/api/v1/nodes\n%s\n%s' "$TS" "$BODY_HASH" \
  | openssl dgst -sha256 -hmac "$KEY" -hex | awk '{print $2}')

curl -X POST http://127.0.0.1:18888/api/v1/nodes \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $KEY" \
  -H "X-API-Timestamp: $TS" \
  -H "X-API-Sign: $SIGN" \
  -d "$BODY"
```

### 1.4 权限范围（Scopes）

API Token 创建时勾选，支持通配（`*` 表示全部，`rule:*` 覆盖 `rule:read` 与 `rule:write`）：

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

Session Cookie 与 JWT 认证不受 Scope 限制，只受角色（admin/user）限制。

---

## 2. 统一响应体

**成功**

```json
{
  "code": 0,
  "message": "ok",
  "data": {},
  "request_id": "req_01HXYZ",
  "timestamp": "2026-01-01T12:00:00Z"
}
```

**列表**

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "items": [],
    "pagination": { "page": 1, "page_size": 20, "total": 214, "total_pages": 11 }
  },
  "request_id": "req_01HXYZ",
  "timestamp": "2026-01-01T12:00:00Z"
}
```

**失败**

```json
{
  "code": 40001,
  "message": "参数缺失或格式错误",
  "details": {
    "field": "node_id",
    "value": 999,
    "hint": "请检查节点 ID 是否正确"
  },
  "request_id": "req_01HXYZ",
  "timestamp": "2026-01-01T12:00:00Z"
}
```

`details.field` 可直接用于前端定位到具体表单项。

### 2.1 分页、排序与筛选

- 分页：`?page=1&page_size=20`（`page_size` 上限 200）
- 排序：`?sort=created_at&order=desc`（只接受白名单内的字段名）
- 筛选：显式参数，不做通用表达式注入

### 2.2 幂等

所有 POST 创建接口支持 `Idempotency-Key` 请求头。相同 key 的重复请求
不会重复创建资源，而是返回首次的结果。

---

## 3. 快速开始

一个完整的「建节点 → 建组 → 建规则 → 查流量」流程：

```bash
BASE=http://127.0.0.1:18888/api/v1
KEY=ort_xxxxxxxx
H=(-H "X-API-Key: $KEY" -H 'Content-Type: application/json')

# 1. 建节点（返回一次性 token 与安装命令）
curl -s -X POST "$BASE/nodes" "${H[@]}" \
  -d '{"name":"HK-01","role":"both"}' | jq .

# 2. 建入口设备组
curl -s -X POST "$BASE/device-groups" "${H[@]}" \
  -d '{"name":"HK-In","type":"inbound","node_ids":[1],
       "config":{"protocol":"tls","tls_inbound_policy":0,
                 "allowed_host":[],"blocked_host":[],"max_fail":3,"fail_timout_sec":30}}' | jq .

# 3. 建出口设备组
curl -s -X POST "$BASE/device-groups" "${H[@]}" \
  -d '{"name":"JP-Out","type":"outbound","node_ids":[2],
       "balance":"least_conn","health_check_enable":true,
       "config":{"connect_type":"static","connect_address":"jp.example.com",
                 "connect_port":2333,"protocol":"ws"}}' | jq .

# 4. 建转发规则
curl -s -X POST "$BASE/forward-rules" "${H[@]}" \
  -d '{"name":"rule-hk-jp-01","inbound_group_id":1,"outbound_group_id":2,
       "listen_port":8443,
       "targets":[{"host":"1.2.3.4","port":443,"weight":1}],
       "inbound_multiplier":1.5,"outbound_multiplier":0.5}' | jq .

# 5. 查流量
curl -s "$BASE/traffic/overview" "${H[@]}" | jq .
curl -s "$BASE/forward-rules/1/traffic?interval=day" "${H[@]}" | jq .
```

---

## 4. 接口清单

### 4.1 认证 `/api/v1/auth`

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/auth/login` | 登录，返回 access_token / refresh_token |
| POST | `/auth/logout` | 登出 |
| POST | `/auth/refresh` | 刷新令牌 |
| GET | `/auth/me` | 当前用户信息与权限 |
| POST | `/auth/password` | 修改自己的密码 |
| GET | `/auth/captcha` | 图形验证码（连续失败 3 次后强制） |

### 4.2 节点 `/api/v1/nodes`

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/nodes` | `node:read` | 列表（`?group_id=&online=&keyword=`） |
| POST | `/nodes` | `node:write` | 创建，返回 token 与安装命令 |
| GET | `/nodes/:id` | `node:read` | 详情 |
| PUT | `/nodes/:id` | `node:write` | 更新 |
| DELETE | `/nodes/:id` | `node:write` | 删除 |
| POST | `/nodes/:id/install-command` | `node:write` | 生成安装命令 |
| POST | `/nodes/:id/upgrade` | `node:exec` | 升级节点客户端 |
| POST | `/nodes/:id/restart` | `node:exec` | 重启节点服务 |
| POST | `/nodes/:id/exec` | `node:exec` | 在节点执行命令 |
| POST | `/nodes/:id/reset-token` | `node:write` | 重置节点密钥 |
| GET | `/nodes/:id/metrics` | `node:read` | 探针数据（`?from=&to=&interval=`） |
| GET | `/nodes/:id/metrics/realtime` | `node:read` | 实时指标 |
| GET | `/nodes/:id/rules` | `rule:read` | 该节点上运行的规则 |
| GET | `/nodes/:id/logs` | `node:read` | 节点日志（`?lines=200`） |
| GET | `/nodes/:id/drift` | `node:read` | 配置漂移详情 |
| POST | `/nodes/:id/drift/fix` | `node:write` | 一键纠正漂移 |
| WS | `/nodes/stream` | `node:read` | 节点状态实时推送 |
| WS | `/nodes/:id/terminal` | `node:exec` | WebSSH |
| POST | `/nodes/batch/upgrade` | `node:exec` | 批量升级 |
| POST | `/nodes/batch/exec` | `node:exec` | 批量执行 |
| POST | `/nodes/batch/group` | `node:write` | 批量改分组 |
| POST | `/nodes/batch/weight` | `node:write` | 批量改权重 |

### 4.3 节点分组 `/api/v1/node-groups`

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/node-groups` | `group:read` |
| POST | `/node-groups` | `group:write` |
| GET | `/node-groups/:id` | `group:read` |
| PUT | `/node-groups/:id` | `group:write` |
| DELETE | `/node-groups/:id` | `group:write` |
| GET | `/node-groups/:id/nodes` | `group:read` |
| GET | `/node-groups/:id/export` | `group:read` |

### 4.4 设备组 `/api/v1/device-groups`

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/device-groups` | `group:read` | `?type=inbound\|outbound` |
| POST | `/device-groups` | `group:write` | |
| GET | `/device-groups/:id` | `group:read` | 含解析后的 Config |
| PUT | `/device-groups/:id` | `group:write` | |
| DELETE | `/device-groups/:id` | `group:write` | 被引用时返回 `40903` |
| GET | `/device-groups/:id/schema` | `group:read` | 配置字段 schema |
| GET | `/device-groups-schema` | `group:read` | 按 `?type=` 取 schema |
| POST | `/device-groups/:id/validate` | `group:write` | 校验配置合法性 |
| GET | `/device-groups/:id/health` | `group:read` | 组内健康状态与负载分布 |
| POST | `/device-groups/:id/reorder` | `group:write` | 调整组内节点顺序 |

### 4.5 转发规则 `/api/v1/forward-rules`

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/forward-rules` | `rule:read` | 支持多条件筛选 |
| POST | `/forward-rules` | `rule:write` | |
| GET | `/forward-rules/:id` | `rule:read` | |
| PUT | `/forward-rules/:id` | `rule:write` | |
| DELETE | `/forward-rules/:id` | `rule:write` | |
| POST | `/forward-rules/:id/enable` | `rule:write` | |
| POST | `/forward-rules/:id/disable` | `rule:write` | |
| POST | `/forward-rules/:id/resync` | `rule:write` | 强制重新下发 |
| GET | `/forward-rules/:id/traffic` | `traffic:read` | `?from=&to=&interval=hour\|day` |
| GET | `/forward-rules/:id/sessions` | `rule:read` | 当前会话列表 |
| POST | `/forward-rules/batch` | `rule:write` | 批量操作 |
| POST | `/forward-rules/import` | `rule:write` | `?preview=true` 先预览 |
| GET | `/forward-rules/export` | `rule:read` | 导出 JSON |
| POST | `/forward-rules/batch-multiplier` | `rule:write` | 批量调倍率 |

批量操作的 `action` 可选值：
`enable`、`disable`、`delete`、`set_rule_group`、`set_multiplier`、
`scale_multiplier`、`set_outbound_group`、`set_limits`、`set_chain`。

### 4.6 规则分组 `/api/v1/rule-groups`

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/rule-groups` | `rule:read` |
| POST | `/rule-groups` | `rule:write` |
| GET/PUT/DELETE | `/rule-groups/:id` | `rule:read` / `rule:write` |
| POST | `/rule-groups/reorder` | `rule:write` |

### 4.7 用户与用户分组 `/api/v1/users` `/api/v1/user-groups`

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/users` | `user:read` | |
| POST | `/users` | `user:write` | 手工建号 |
| GET/PUT/DELETE | `/users/:id` | `user:read` / `user:write` | |
| POST | `/users/:id/reset-password` | `user:write` | 返回一次性明文 |
| POST | `/users/:id/reset-token` | `user:write` | 重置订阅 Token |
| GET | `/users/:id/traffic` | `traffic:read` | |
| GET | `/users/:id/rules` | `user:read` | |
| POST | `/users/:id/disable` | `user:write` | |
| GET | `/user-groups` | `group:read` | |
| POST | `/user-groups` | `group:write` | |
| GET/PUT/DELETE | `/user-groups/:id` | `group:read` / `group:write` | |
| GET | `/sub/:token` | 无需认证 | 订阅：返回该用户的规则配置文本 |

### 4.8 流量统计 `/api/v1/traffic`

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/traffic/overview` | `traffic:read` | 今日/昨日/本月/累计 + 在线数 |
| GET | `/traffic/timeseries` | `traffic:read` | `?from=&to=&interval=&group_by=` |
| GET | `/traffic/top` | `traffic:read` | `?dimension=user\|rule\|node&limit=10` |
| GET | `/traffic/export` | `traffic:read` | 导出 CSV |
| GET | `/traffic/dashboard` | `traffic:read` | 首页仪表盘聚合数据 |

### 4.9 探针与监控 `/api/v1/probe`

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/probe/overview` | `node:read` |
| GET | `/probe/nodes/:id` | `node:read` |
| GET | `/probe/cleanup` | `system:write` |

### 4.10 系统与设置 `/api/v1`

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/system/info` | `system:read` | 版本、运行时长、方言、节点数、规则数 |
| GET | `/system/status` | `system:read` | 健康检查 |
| GET | `/system/version` | `system:read` | 版本清单 |
| GET | `/system/errors` | `system:read` | 完整错误码字典 |
| GET | `/system/openapi.json` | 无需认证 | 运行时 OpenAPI 描述 |
| GET/PUT | `/settings` | `system:read` / `system:write` | |
| GET | `/audit-logs` | `system:read` | 审计日志 |
| GET | `/tasks` | `system:read` | 任务列表 |
| POST | `/tasks/:name/run` | `system:write` | 手动触发任务 |
| GET/POST | `/alerts` | `system:read` / `system:write` | 告警规则 |
| GET | `/alerts/history` | `system:read` | 告警历史 |
| POST | `/alerts/:id/test` | `system:write` | 测试通知渠道 |
| POST | `/alerts/:id/resolve` | `system:write` | 标记已解决 |
| POST | `/webhooks/test` | `system:write` | 测试 Webhook |
| GET/POST | `/snapshots` | `system:read` / `system:write` | 配置快照 |
| GET | `/snapshots/:id/diff` | `system:read` | 与当前配置对比 |
| POST | `/snapshots/:id/rollback` | `system:write` | 回滚 |
| GET/POST | `/api-tokens` | `system:write` | API Token 管理 |
| DELETE | `/api-tokens/:id` | `system:write` | |

### 4.11 迁移 `/api/v1/migrations`

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/migrations/precheck` | `migrate:run` | 返回预检报告，不写数据 |
| POST | `/migrations/run` | `migrate:run` | `?dry_run=true\|false` |
| GET | `/migrations` | `migrate:run` | 批次列表 |
| GET | `/migrations/:id` | `migrate:run` | 批次详情与报告 |
| GET | `/migrations/:id/progress` | `migrate:run` | 实时进度 |
| POST | `/migrations/:id/rollback` | `migrate:run` | 回滚该批次 |
| POST | `/backups` | `backup:run` | 生成备份 |
| GET | `/backups` | `backup:run` | 备份列表 |
| GET | `/backups/:id/download` | `backup:run` | 下载 |
| POST | `/backups/:id/restore` | `backup:run` | 恢复 |

### 4.12 节点通信 `/api/node`（不带 `/v1`）

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| POST | `/api/node/register` | token（body） | 注册并获取初始配置 |
| POST | `/api/node/heartbeat` | `X-Node-Token` | 心跳上报 |
| GET | `/api/node/config` | `X-Node-Token` | 拉取配置（`?version=`） |
| POST | `/api/node/report` | `X-Node-Token` | 上报同步结果与流量 |
| GET | `/api/node/tasks` | `X-Node-Token` | 拉取任务 |
| POST | `/api/node/task-result` | `X-Node-Token` | 上报任务结果 |
| GET | `/api/node/stream` | `X-Node-Token` | 长连接推送 |
| GET | `/install.sh` | 无 | 安装脚本 |
| GET | `/uninstall.sh` | 无 | 卸载脚本 |
| GET | `/api/node/binary/:arch` | 无 | 节点二进制下载 |

**心跳请求示例**

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

---

## 5. 错误码字典

### 5.1 区间划分

| 区间 | 类别 |
|---|---|
| `0` | 成功 |
| `40000`-`40099` | 参数错误 |
| `40100`-`40199` | 认证失败 |
| `40300`-`40399` | 权限不足 |
| `40400`-`40499` | 资源不存在 |
| `40900`-`40999` | 资源冲突 |
| `42200`-`42299` | 业务校验失败 |
| `42900`-`42999` | 触发限流 |
| `50000`-`50099` | 服务器内部错误 |
| `50300`-`50399` | 依赖不可用 |
| `60000`-`60099` | 节点通信错误 |
| `70000`-`70099` | 迁移相关错误 |

运行时可通过 `GET /api/v1/system/errors` 获取当前版本的完整字典。

### 5.2 常用错误码

| 码 | HTTP | 含义 |
|---|---|---|
| `0` | 200 | 成功 |
| `40001` | 400 | 参数缺失或格式错误 |
| `40002` | 400 | 参数值超出范围 |
| `40101` | 401 | 未登录或 Token 无效 |
| `40102` | 401 | Token 已过期 |
| `40103` | 401 | 签名校验失败 |
| `40104` | 401 | 用户名或密码错误 |
| `40105` | 401 | 需要验证码 |
| `40106` | 401 | 验证码错误 |
| `40107` | 401 | 账号已锁定 |
| `40108` | 401 | 账号已禁用 |
| `40301` | 403 | 无权限执行此操作 |
| `40302` | 403 | API Token 缺少所需 Scope |
| `40303` | 403 | IP 不在白名单内 |
| `40304` | 403 | WebSSH 已被禁用 |
| `40401` | 404 | 资源不存在 |
| `40901` | 409 | 名称已存在（归一化比较后重复） |
| `40902` | 409 | 端口已被占用 |
| `40903` | 409 | 该资源正在被引用，无法删除 |
| `40904` | 409 | 当前状态不允许该操作 |
| `42201` | 422 | 故障转移组必须与当前组拥有相同的入口权限配置 |
| `42202` | 422 | 链式出口不支持 UDP |
| `42203` | 422 | 链式出口不支持故障转移组 |
| `42204` | 422 | SNI 不在入口组白名单内 |
| `42205` | 422 | 倍率超出允许范围 |
| `42206` | 422 | 节点角色与设备组类型不匹配 |
| `42207` | 422 | 该场景必须使用固定端口 |
| `42208` | 422 | 至少需要一个有效目标 |
| `42209` | 422 | 反向组的出口节点未配置为出口角色 |
| `42210` | 422 | 该场景不支持 UDP |
| `42901` | 429 | 请求过于频繁 |
| `50001` | 500 | 内部错误 |
| `50301` | 503 | 数据库不可写 |
| `60001` | 502 | 节点离线，无法下发配置 |
| `60002` | 502 | 节点应用配置失败 |
| `60003` | 504 | 节点响应超时 |
| `60004` | 401 | 节点密钥无效 |
| `70001` | 400 | 源库无法连接 |
| `70002` | 400 | 源库版本不被支持 |
| `70003` | 409 | 目标库与源库相同 |
| `70004` | 422 | 检测到无法自动处理的冲突，请先处理 |
| `70005` | 500 | 备份恢复失败 |

---

## 6. Webhook（出站通知）

面板在以下事件发生时 POST 到配置的 Webhook 地址：

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

请求体统一格式：

```json
{
  "event": "node.offline",
  "timestamp": "2026-01-01T12:00:00Z",
  "data": { "node_id": 12, "node_name": "HK-01", "offline_seconds": 25 }
}
```

请求头：

- `X-OpenRoute-Event`：事件名
- `X-OpenRoute-Signature`：`HMAC-SHA256(config.yml 的 secret-key, 请求体原文)` 的十六进制摘要

校验示例（Python）：

```python
import hmac, hashlib

def verify(secret: str, raw_body: bytes, signature: str) -> bool:
    want = hmac.new(secret.encode(), raw_body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, signature)
```

失败重试 3 次，间隔 5s / 30s / 300s。

---

## 7. 速率限制

| 接口类别 | 限制 |
|---|---|
| 登录 | 5 次 / 分钟 / IP；连续失败 3 次要求验证码；10 次失败锁定 15 分钟 |
| 普通读接口 | 由 `user-rate-limit` 控制（默认 5 req/s，突发 5） |
| 写接口 | 2 req/s，突发 3 |
| 批量操作 | 1 req / 2s |
| 节点心跳 | 不限（节点侧固定 10s 一次） |
| 订阅接口 | 10 次 / 分钟 / Token |
| WebSSH | 每用户最多 3 个并发终端 |

触发限流时返回 `42901`，响应头带 `Retry-After`。

---

## 8. 流量计费口径

单向流量统计，倍率折算公式：

```
用户产生的流量 = 入口实际流量 × 入口倍率 + 出口实际流量 × 出口倍率
```

示例：用户下载 500M、上传 100M，入口倍率 1.5、出口倍率 0.5

```
规则流量增加 = 500 + 100 = 600M
用户已用流量增加 = 600 × 1.5 + 600 × 0.5 = 900 + 300 = 1200MB
```

链式出口场景下，流量同时经过入口与链式出口，两段分别按各自倍率计入。

页面的默认展示值是**乘倍率后**的字节数，可切换显示原始值。
API 的 `raw_bytes` 字段始终是实际字节，`bytes` 是乘倍率后的值。
