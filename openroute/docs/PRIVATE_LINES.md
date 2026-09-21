# 内网、IPLC / IEPL 与多网卡配置

适用于 `nc20260922.2` 及之后的节点客户端。先更新面板，再更新参与链路的所有节点。
IPLC / IEPL 对本程序而言是节点之间的一条 IP 通路，不需要另一种转发协议。
运营商交付的 VLAN、二层接入、IP、网关、路由和 MTU 必须先在系统中配置好；
面板不会开通线路、创建 VLAN、建立网桥或自动改写服务器路由。

## 先区分四个地址

| 配置 | 含义 | 双机专线示例 |
|---|---|---|
| `-u` / `base-url` | 节点访问面板的管理地址 | `https://panel.example.com`，可以走公网 |
| `--connect-host` / `connect-host` | **发布本机地址**，供其它节点连接本机 | 落地 B 填 B 的 `10.88.0.2`，不是入口 A 的 IP |
| `TUNNEL_BIND_INBOUND` | 本机隧道监听绑定地址，必须是本机拥有的 IP | B 填 `10.88.0.2` |
| `TUNNEL_BIND_OUTBOUND_4` | 本机主动建立隧道时的源 IP | A 填 A 的 `10.88.0.1` |

`connect-host` 不负责把 IP 加到网卡，也不指定源 IP。它只接受裸 IPv4、裸 IPv6 或
域名，不接受 `https://`、`:端口`、CIDR、`0.0.0.0`、`::` 或带 zone 的 IPv6。
连接端口单独配置。IP/域名选中后，连接失败不会改用这个节点的公网地址；
**配置了其它出口或故障转移组时，仍可能改选那个出口**。纯专线业务应确保所有
候选节点和各跳均有专线路径，并配合网卡绑定及系统路由，避免配置公网备用出口。

本地客户端 `connect-host` 只用于节点互联，不改变终端用户的订阅地址。
历史面板字段「节点→连接地址」同时可能作为入口订阅地址：入口机需要同时保留
公网订阅与专线互联时，优先用本地 `--connect-host`，不要把面板该字段改成内网地址。

## 双机专线：入口 A → 落地 B

```text
用户 → A 公网IP:规则端口
         A eth1:10.88.0.1 ── IPLC/IEPL ── B eth1:10.88.0.2 → B 公网出口 → 目标
                       节点间隧道
A、B → HTTPS 面板：独立的管理连接，可走公网
```

例子假定两端专线已能互通，网卡名都是 `eth1`，没有 NAT。请替换成实际地址和网卡。

1. 在面板创建 A、B 节点；A 加入入口设备组，B 加入出口设备组。
2. B 的安装命令追加 `--connect-host 10.88.0.2`。A 也可发布自己的专线 IP，便于反向连接。
3. 出口组连接类型用 `dyn_ip4`，不填写组级静态地址。尽管类型名称是动态 IPv4，
   显式 `connect-host` 优先，因此仍会连接 `10.88.0.2`。多个出口可各自发布不同 IP。
4. 入口组、出口组选择一致的隧道协议，例如 `direct` 或 `tls`；转发规则选择入口组、出口组和真实目标。
   协议配置按入口组 → 出口组 → 规则 `options` 顺序覆盖，以最后明确指定的值为准。
   是否需要 TLS 取决于线路可信程度；“专线”本身不等于应用数据已经加密。
5. 根据所选协议，允许 A 访问 B 的对应隧道端口；允许用户访问 A 的规则监听端口。

在 A 首次安装：

```bash
TUNNEL_BIND_INBOUND=10.88.0.1 \
TUNNEL_BIND_OUTBOUND_4=10.88.0.1 TUNNEL_INTERFACE=eth1 \
bash <(curl -fsSL https://panel.example.com/install.sh) \
  -u https://panel.example.com -t 'A的节点密钥' --connect-host 10.88.0.1
```

在 B 首次安装：

```bash
TUNNEL_BIND_INBOUND=10.88.0.2 \
TUNNEL_BIND_OUTBOUND_4=10.88.0.2 TUNNEL_INTERFACE=eth1 \
bash <(curl -fsSL https://panel.example.com/install.sh) \
  -u https://panel.example.com -t 'B的节点密钥' --connect-host 10.88.0.2
```

已有安装会保留 `env.sh`，命令前临时设置的环境变量不会覆盖该文件。已有节点应编辑
`/opt/openroute-node/env.sh`，分别填写本机的 IP，然后执行 `systemctl restart openroute-node`。
本地 `config.yml` 的未知字段、注释和未显式覆盖的参数在重新安装时保留。
如果原安装使用自定义 `-s`（或 `S`），重装必须沿用同一服务名，否则会安装另一个实例。

落地 B 通常不要设置 `BIND_OUTBOUND_4=10.88.0.2`：它限制的是访问**最终目标**的源地址，
可能使本应走公网出口的连接也使用专线地址。隧道专用变量解决了这两类出站的区分。

单机专线的另一种交付方式：服务器本身已经能经运营商路由访问目标，此时可使用
不选出口组的单端直出规则。该模式没有节点间隧道，走最终目标的出站策略；
`TUNNEL_*` 不会替它选择线路，应使用系统路由，必要时使用 `BIND_OUTBOUND_*` / `OUTBOUND_FWMARK`。

## 参数、默认值和优先级

下表的长参数同时支持节点二进制和安装脚本；`--名称 值` 与 `--名称=值` 均可使用。
YAML 键与长参数去掉 `--` 后相同。二进制直接启动时的参数仅对该次进程生效，
安装脚本会将显式参数持久化到 `config.yml`。

| 参数 / YAML 键 | 默认值 | 含义 |
|---|---|---|
| `connect-host` | 空字符串 | 发布本机可被其它节点访问的地址；空表示使用面板地址配置 |
| `direct-port` | `0` | 本机 direct 隧道 TCP 监听端口；继承面板，最终默认 `28080` |
| `ws-port` | `0` | 本机 WS / HTTP 隧道 TCP 监听端口；最终默认 `28081` |
| `tls-port` | `0` | 本机 TLS 隧道 TCP 监听端口；最终默认 `28082` |
| `udp-port` | `0` | 本机原生 UDP 隧道监听端口；最终默认 `28083` |
| `rev-port` | `0` | 本机反向隧道 TCP 监听端口；最终默认 `28084`；规则显式 `reverse_port` 仍有独立语义 |
| `connect-direct-port` | `0` | 其它节点连接本机 direct 隧道的端口；0 跟随实际监听端口 |
| `connect-ws-port` | `0` | WS / HTTP 对外连接端口；0 跟随实际监听端口 |
| `connect-tls-port` | `0` | TLS 对外连接端口；0 跟随实际监听端口 |
| `connect-udp-port` | `0` | 原生 UDP 对外连接端口；0 跟随实际监听端口 |
| `connect-rev-port` | `0` | 反向隧道对外连接端口；0 跟随实际监听端口 |

端口必须为十进制的 `0` 或 `1～65535`，不接受十六进制；前导零不改变进制。这里的端口均为**节点间隧道端口**，不改变转发规则的
用户访问端口。新客户端在注册和心跳时上报这组配置，改变后面板会更新配置版本并通知对端。
面板显示的客户端上报值与管理员填写值分开保存，清空本地覆盖不会删除管理员设置。

优先级：

- 本地配置：显式 CLI 参数（包括空字符串和 0）→ YAML → 空 / 0。
- 对端地址：设备组 `static + connect_address` → 客户端 `connect-host` → 面板节点连接地址
  → 原有静态内网 / 动态 IPv4 / IPv6 地址策略。多网卡场景不要依赖自动探测的第一个内网 IP。
- 本机监听端口：客户端非零端口 → 面板节点非零端口 → 上表默认端口。
- 对端 TCP 端口：出口组显式静态 `connect_port` → 客户端对应 `connect-*-port` → 实际监听端口。
  原生 UDP 与反向端口独立，不被普通出口组的 TCP 静态端口覆盖。

组级静态地址会作用于该组每个节点，适用于明确的一对一地址映射；不能把多个不同出口
放入同组后，给全组填写某一台机器的 IP 来代替逐节点地址。

清除本地覆盖并继续使用面板配置：

```bash
bash <(curl -fsSL https://panel.example.com/install.sh) \
  -u https://panel.example.com -t '节点密钥' --connect-host='' --direct-port=0
```

节点二进制辅助参数：`-c/--config` 指定 YAML，`-u/-t` 覆盖面板和密钥，`--check` 校验后退出，
`--version` 查看版本。`--write-config 目标路径` 会合并 `-c` 文件与显式参数，原子写入目标并退出，
不会连接面板；安装脚本使用它保留本地设置。`--is-outbound` 仅用于这种写配置模式的历史字段，
实际入口 / 出口角色仍由面板控制。

## 专线环境变量

配置文件位置：`/opt/openroute-node/env.sh`；自定义 `-s` 服务名时目录相应变化。
每行写 `变量="值"`，修改后重启服务。`BIND_INBOUND` 支持本机 IP、网卡名以及逗号分隔的列表；
网卡名会展开成本机该网卡上的 IP。`TUNNEL_BIND_INBOUND` 显式设置时仅接受**一个裸 IP**，
不接受网卡名、域名或逗号列表；未设置时继承完整的规则监听地址列表。

| 环境变量 | 默认 / 继承 | 作用范围 |
|---|---|---|
| `BIND_INBOUND` | `0.0.0.0` | 用户访问规则的监听 IP；保持公网规则需要的可达性 |
| `TUNNEL_BIND_INBOUND` | 未填继承 `BIND_INBOUND` 解析出的全部地址 | 本机所有节点间隧道的 TCP / UDP / 反向监听 IP |
| `TUNNEL_BIND_OUTBOUND_4` | 见下方隔离规则 | 建立 IPv4 隧道的本机源 IPv4 |
| `TUNNEL_BIND_OUTBOUND_6` | 见下方隔离规则 | 建立 IPv6 隧道的本机源 IPv6 |
| `TUNNEL_INTERFACE` | 空，不限制网卡 | Linux 上用 `SO_BINDTODEVICE` 限定隧道网卡，例 `eth1` |
| `TUNNEL_FWMARK` | 见下方隔离规则 | Linux 隧道 socket 的 `SO_MARK`，用于策略路由；支持 `100`、`0x64`、`0` |
| `BIND_OUTBOUND_4` / `_6` | 空，系统选源 | 最终目标连接的源 IP，包括该路径的健康检查 |
| `OUTBOUND_FWMARK` | 空，不打标 | 最终目标 socket 标记，配合系统 `ip rule` / `ip route` |
| `COUNT_INTERFACE` | 原有自动统计 | 探针统计网卡，例如 `eth1`；它只影响统计，不决定转发路线 |

四个隧道出站变量 `TUNNEL_BIND_OUTBOUND_4`、`TUNNEL_BIND_OUTBOUND_6`、`TUNNEL_FWMARK`、
`TUNNEL_INTERFACE` **任一非空时使用独立策略**，其它未填项采用系统默认，不继承旧出站配置。
四项全空时，为兼容旧部署，隧道继承 `BIND_OUTBOUND_4/6` 与 `OUTBOUND_FWMARK`。
可用 `TUNNEL_FWMARK=0` 显式启用独立策略并取消旧标记。单独设置 `TUNNEL_BIND_INBOUND`
不会开启出站策略隔离。

隧道策略覆盖普通隧道、链式各跳、反向主动连接及节点间探测；监听 socket 同样应用
隧道网卡 / mark，UDP 回复使用相同监听 socket。域名按实际解析出的 IPv4 / IPv6 选择源地址。
指定不存在的网卡、无法绑定的本机 IP 或缺少 socket 权限时会报错，不会悄悄移除限制重试。
`--check` 校验格式、网卡与 socket 权限；实际 IP 可绑定性、端口占用和线路连通性仍需启动后验证。
本机网络接口约束不代替路由配置；专线目的网段仍应有明确路由。

`TUNNEL_INTERFACE`、`TUNNEL_FWMARK`、`OUTBOUND_FWMARK` 需要 Linux；通常使用安装服务默认
授予的 root / 网络能力。面板 HTTPS 控制连接不使用这组数据面绑定参数。
域名解析使用系统 DNS 设置，DNS 查询本身不受 `TUNNEL_*` 约束；严格限定专线路径时可直接
发布专线 IP，或另行配置内网 DNS 和系统路由。mark 的合法范围为无符号 32 位整数，
建议使用普通十进制或 `0x` 十六进制写法，避免带前导零的进制歧义。

## NAT、反向连接和链式

**NAT 映射**：假设 B 本机监听 `10.88.0.2:28080`，运营商将 `10.99.0.2:40080/TCP`
映射到它，UDP 则用 `40083 → 28083`。B 的 YAML：

```yaml
connect-host: "10.99.0.2"
direct-port: 28080
connect-direct-port: 40080
udp-port: 28083
connect-udp-port: 40083
```

B 的 `TUNNEL_BIND_INBOUND` 仍填本机 `10.88.0.2`，不要绑定运营商的映射 IP。
端口映射必须在运营商或 NAT 设备中实际存在；填参数不会创建映射。

**反向连接**：当只能由 B 主动连接 A 时，A 发布自己的专线 IP，A 放行反向端口，
B 可达 A；在规则中开启反向模式并选择相应组。B 连接 A 公布的 `connect-rev-port`
（为 0 时跟随 A 的 `rev-port`），不是让 B
把自己的 `connect-host` 填成 A 的 IP。反向支持四种 TCP 外壳，UDP 使用 UoT。
规则单独设置 `reverse_port` 时该端口会优先用于此反向规则，需要对应的可达监听 / NAT 映射。
例如 A 的反向 NAT 映射为 `40084 → 28084/TCP`，则 A 配置 `rev-port: 28084`、
`connect-rev-port: 40084`，规则 `reverse_port` 保持 0，避免覆盖该映射。

**链式**：每一跳都配置下一跳可达的专线地址；`chain_groups` 表示完整出口路径。
目前链式仅支持 TCP，不能组合反向或组级故障转移。具有多个出口的组要逐一验证路径。

## 验证与排错

```bash
# 在 A 上查看本机地址、网卡和到 B 的实际路由
ip -br address
ip route get 10.88.0.2 from 10.88.0.1
# 如果使用 TUNNEL_FWMARK=100，同时检查带标记的路线
ip route get 10.88.0.2 from 10.88.0.1 mark 100
ip rule show
ip route show table all

# 在 B 上确认节点服务和监听
systemctl status openroute-node
journalctl -u openroute-node -n 80 --no-pager
ss -lntup

# 连通性测试；TCP能连接仅说明端口通，不代表规则认证和目标都成功
nc -vz 10.88.0.2 28080
# 专线允许ICMP时测试1500 MTU；失败也可能是ICMP被过滤
ping -c 3 -M do -s 1472 10.88.0.2
```

系统应显示去 B 的路由经专线网卡；使用 fwmark 时还应检查对应策略路由表。
直接二层交付与经网关交付的路由写法不同，应使用运营商给定的网段 / 下一跳，
不要把缺省公网路由整体替换成专线来“试一试”。

最终使用真实转发规则访问目标，并查看两端日志和流量；只 ping 通或端口打开不足以
证明整条转发可用。专线 MTU 较低时应调整系统 MTU / MSS；原生 UDP 会受分片与丢包影响。
UoT 可用于只放行 TCP 的链路，但存在 TCP 队头阻塞，并非所有专线都更适合开启。

## 对照依据与后续值得增加的能力

Nyanpass 公开文档：[节点命令](https://nyanpass.pages.dev/reference/nc_command/)、
[连接地址](https://nyanpass.pages.dev/reference/connect/)、
[专线配置](https://nyanpass.pages.dev/reference/zx/)、
[环境变量](https://nyanpass.pages.dev/reference/nc_environment/)。这些公开文档支持上述
“发布本机地址、静态地址优先、系统路由另行配置”的语义，不等于对所有最新版本二进制的兼容认证。

本次补齐参数和专线路由选择后，仍值得后续单独实现的项目：

- 在面板直接展示各跳所选 IP、端口、网卡、RTT、丢包和 MTU 的诊断工具；目前需用节点日志和系统工具。
- 一台节点面向不同入口的地址 / 路由配置档案，以及更直观的多专线拓扑编排；当前主要按节点及设备组配置。
- 单独的用户访问地址字段，彻底拆分旧面板连接地址与订阅地址的历史复用。
- 标准外部代理接入 `NYA_PROXY`、ECH 等规格中预留但尚未实现的能力；不要将预留 YAML 字段视为已支持。
- UDP 速率整形和跨入口严格共享配额；当前 TCP 限速与每入口限制、配额同步延迟的边界仍有效。

这些是当前项目的明确缺口和自用场景建议，不表示 Nyanpass 每项都有对应实现。支付系统不在范围内。
