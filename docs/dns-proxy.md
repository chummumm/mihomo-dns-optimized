# DNS 专用混合代理端口

这个入口让 SmartDNS 发出的普通 DNS 查询，按查询中的域名（QNAME）复用 Mihomo 已有的分流规则选择出口。配置仍然只维护原来的 `rules`、`sub-rules` 和 `rule-providers`，不需要生成配置，也不需要另一份按域名划分的 DNS 策略表。

该功能独立于 Mihomo 内置 DNS 的增强模式，不要求 FakeIP。使用真实 IP 时，原有 `enhanced-mode: redir-host` 可以保留。

## 1. Mihomo 只增加一个顶层端口

在原有配置里增加 `dns-proxy-port`，它和原版 `mixed-port` 同级：

```yaml
mixed-port: 7890
dns-proxy-port: 7853
```

与 `mixed-port` 一样，配置非零端口即启用；设为 `0` 或省略即关闭。没有额外的 `enable`、TCP 或 UDP 开关，也不需要在 `listeners` 中声明新 `type`。上一版的 `listeners: type: dns-proxy` 配置已移除，请替换成这个顶层字段。

`7853` 是本地代理入口的端口，TCP 和 UDP 共用这个数字；**被代理的 DNS 服务器目标端口必须为 `53`**。入口不直接接受裸 UDP DNS，SmartDNS 必须通过 SOCKS5 或 HTTP CONNECT 连接它。SOCKS4/4a TCP、SOCKS5 TCP / UDP、HTTP CONNECT TCP 均默认支持。

| 配置项 | 行为 |
| --- | --- |
| `dns-proxy-port` | 新增的顶层代理端口，与 `mixed-port` 采用相同的启用和关闭方式 |
| `allow-lan` / `bind-address` | 复用全局设置；`allow-lan: false` 时仅监听 `127.0.0.1`，启用 LAN 后按 `bind-address` 绑定 |
| `authentication` | 复用全局 `用户名:密码` 列表，与普通 mixed 共用 |
| `skip-auth-prefixes` | 复用全局免认证来源网段 |
| `lan-allowed-ips` / `lan-disallowed-ips` | 复用全局来源访问控制 |
| `inbound-tfo` / `inbound-mptcp` | 复用全局入站套接字设置，与 mixed 一致 |

需要认证时直接使用原有全局字段，例如：

```yaml
authentication:
  - "smartdns:replace-with-your-own-password"
```

不为这个入口另设 `users`、`rule` 或 `proxy`。它始终复用主分流规则。SOCKS4 的 USERID 认证方式也沿用上游语义；带密码的认证请使用 SOCKS5 或 HTTP CONNECT。

控制 API 的 `GET /configs` 会返回 `dns-proxy-port`，`PATCH /configs` 支持修改、关闭和重新启用该端口。配置文件中的端口变更沿用 mixed 的重载语义：`PUT /configs?force=true` 会更新入站，非强制重载保留当前端口。修改共享的 `allow-lan` / `bind-address` 时，普通 mixed 和 DNS 专用端口都会跟随更新。

## 2. SmartDNS 指向这个入口

SOCKS5 同时支持 UDP DNS 和 TCP DNS。选中的节点如果不支持 UDP，使用 `server-tcp`：

```conf
proxy-server socks5://127.0.0.1:7853 -name mihomo-dns

server-tcp 1.1.1.1:53 -proxy mihomo-dns
server-tcp 8.8.8.8:53 -proxy mihomo-dns
```

需要 UDP，且分流可能选中的出口都支持 UDP 时，可以使用：

```conf
server 1.1.1.1:53 -proxy mihomo-dns
server 8.8.8.8:53 -proxy mihomo-dns
```

也可以使用 HTTP 代理，但它只承载 TCP DNS：

```conf
proxy-server http://127.0.0.1:7853 -name mihomo-dns-http
server-tcp 1.1.1.1:53 -proxy mihomo-dns-http
```

有认证时，代理 URL 使用 `socks5://用户名:密码@127.0.0.1:7853` 或对应的 HTTP URL；特殊字符按 URL 规则转义。

原来的 SmartDNS `6053`、`6553` 监听端口可以继续保留。本功能不依赖这两个端口的分组名称。需要由 Mihomo 按域名决定出口的上游，统一加上指向新入口的 `-proxy mihomo-dns`。

从这条链路移除 `server-https` / `server-tls` / `server-quic` 等加密 DNS 上游。专用入口只处理发往 `IP:53` 的普通 DNS；把 DoH 改到端口 `53` 也不会被当成 DNS 转发。

[SmartDNS 最小接入示例](smartdns-dns-proxy.conf) 保留了 `6053` 和 `6553` 两个本地监听端口。该示例只展示接入，不包含私人节点或账号。

## 3. 分流规则保持原样

例如已有一个名为 `菲律宾` 的策略组，规则仍然直接写在原来的列表里：

```yaml
rules:
  - DOMAIN-SUFFIX,claude.ai,菲律宾
  - DOMAIN-SUFFIX,anthropic.com,菲律宾
  # 其余规则保持原有顺序
  - MATCH,你的默认策略组
```

上面是规则片段，不是完整的 Anthropic 域名列表。已经使用 Anthropic 规则集时，保留原来的 `RULE-SET,Anthropic,菲律宾` 即可，无须再抄一份域名到 SmartDNS。

每条查询都会重新匹配规则，策略组也读取当前选择。即使 SmartDNS 使用同一个 UDP 会话、同一个长时间保持的 TCP 连接，查询不同域名仍然可以走不同出口。修改策略组选择后，下一条新查询生效；正在交换的查询继续使用已选定的节点。

### 哪些规则参与匹配

| 规则 | DNS 查询阶段的处理 |
| --- | --- |
| `DOMAIN`、`DOMAIN-SUFFIX`、`DOMAIN-KEYWORD`、`DOMAIN-REGEX`、`DOMAIN-WILDCARD` | 按 QNAME 匹配 |
| `GEOSITE` | 复用已加载的域名数据库 |
| `RULE-SET`，`behavior: domain` | 复用已加载的域名规则集 |
| `RULE-SET`，`behavior: classical` | 计算其中能用域名判断的部分 |
| `AND`、`OR`、`NOT`、`SUB-RULE` | 只有能根据域名确定结果时才参与 |
| `MATCH` | 正常作为最后的兜底规则 |
| IP、GEOIP、进程、来源、端口、网络、入站等条件 | 无法代表原始业务连接，跳过 |

这里需要区分两个地址：查询的域名决定**出口**；SmartDNS 指定的解析器 IP 决定**向谁请求 DNS**。例如 SmartDNS 查询 `1.1.1.1:53`，QNAME 命中 `菲律宾`，就通过该组当前节点向 `1.1.1.1:53` 查询。不会把请求错误地发到 `claude.ai:53`，也不会把 `1.1.1.1` 当作网站 IP 去匹配 GEOIP。

DNS 查询还未拿到网站 IP，所以无法保证按 IP、进程或来源进行的业务分流在此阶段也得到同一结果。混合逻辑采用“已知真 / 已知假 / 无法判断”，例如 `NOT(IP-CIDR,...)` 不会因为没有网站 IP 而被误判为真。

`mode: rule` 按上述规则处理；`mode: global` 使用 `GLOBAL` 的当前选择；`mode: direct` 使用 `DIRECT`。

## 4. 节点域名解析需要独立的启动路径

如果代理节点的 `server` 是域名，建立节点连接之前需要先解析这个域名。这一步不能再次进入“必须先连接这个节点”的 SmartDNS 代理路径。

可以在现有 Mihomo DNS 块保留一个专供节点域名使用、能够直接访问的解析器：

```yaml
dns:
  enable: true
  enhanced-mode: redir-host
  respect-rules: false
  nameserver:
    - 127.0.0.1:6053
  proxy-server-nameserver:
    - '223.5.5.5#DIRECT'
    - '119.29.29.29#DIRECT'
```

这是固定的启动解析路径，无须随着业务域名规则增删。如果已有独立的本地直连 DNS，也可以继续使用它。节点直接使用 IP 时，不需要为它做域名解析。

专用入口本身仅接受字面量 IPv4 / IPv6 解析器地址，拒绝解析器主机名，以免为解析器地址再触发一层循环解析。SOCKS4 使用 IPv4；SOCKS4a 的地址字段也须填写字面量 IP。这里限制的是解析器的目标地址，DNS 报文中的待查询域名照常用于匹配规则。

## 5. 拒绝行为及边界

- SOCKS4/4a、SOCKS5 CONNECT 和 HTTP CONNECT 的目标不是 `53`：返回协议错误并关闭，不拨号。
- SOCKS5 UDP 数据报的目标不是 `53`：静默丢弃，不拨号。
- 普通 HTTP 请求、DoH、DoT、DoQ、SOCKS BIND、分片 SOCKS5 UDP、非单播解析器地址：拒绝。
- DNS 必须是有效的普通单问题查询；响应包冒充查询、多问题、尾随垃圾数据、AXFR / IXFR 等不转发。
- `REJECT` 返回 DNS `REFUSED`；`REJECT-DROP` 丢弃查询。TCP 查询被丢弃或交换失败时关闭该连接。
- 选中的出口不支持 UDP：该次 UDP 交换失败，**不会自动改为直连**。SmartDNS 可以改用 `server-tcp`。
- `DNS`、`rematch` 等需要重新进入路由的特殊出口不用于该链路；普通节点、直连和常用选择组正常使用。

策略组没有可用条目时，仍遵循该组的 `empty-fallback` 配置；上游默认的 `COMPATIBLE` 是直连。需要空组也拒绝请求时，把相应组的 `empty-fallback` 显式设为 `REJECT`。

DNS 查询内容（包括原始事务 ID、大小写和 EDNS 信息）原样发给解析器，返回包检查事务 ID、问题和来源。这里只决定这条 DNS 请求的出口，不改写 DNS 回答，也不移除 SmartDNS 自己的缓存、测速或过滤逻辑。

每个监听实例最多 128 个客户端会话、256 个并发查询；单次查询及握手超时 5 秒，空闲会话超时 60 秒。TCP 连接上的查询按帧顺序处理，每次查询独立建立上游交换，避免把不同域名固定到同一出口。策略组的定时健康检查仍然生效；此专用路径直接使用一次选定的节点，不调用策略组包裹拨号层的成功 / 失败回调。

SOCKS5 UDP 同时提供与 mixed 一样的同端口 UDP 入口，以及标准 UDP ASSOCIATE 协商的临时转发端口。两条路径都逐报文校验目标和 DNS 内容，再逐查询选择出口。

设置全局认证后，没有认证信息的同端口 UDP 数据报只允许 `skip-auth-prefixes` 指定的来源；其他来源应先完成 SOCKS5 认证，并使用 ASSOCIATE 返回的转发地址。UDP 来源访问控制同样生效。临时转发端口绑定到控制连接，连接关闭时清理会话；跨容器或其他主机访问时，需要能访问协商得到的地址。

## 6. 验证与排查

### 面板连接列表

DNS 上游交换接入原版连接统计，可通过现有面板的连接页或 `/connections` API 查看。每条正在交换的 DNS 查询显示查询域名（QNAME）、解析器 `IP:53`、SmartDNS 来源、入站名称 `DEFAULT-DNS-PROXY`、TCP / UDP 协议、命中规则、策略组和节点链，以及上传 / 下载流量。可以按入站名称识别这些 DNS 连接。

面板的关闭操作会中断选中的 DNS 上游交换。TCP 查询中断后，相应入站 TCP 连接也会结束；UDP 关闭只影响选中的查询。完成、失败或取消的交换会从活动连接列表移除，流量累计保留。TCP 统计包括 DNS 的两字节长度前缀，UDP 统计 DNS 载荷，与实际交换一致。

这里显示的是每次上游 DNS 交换。空闲的 SmartDNS TCP 控制连接或 UDP ASSOCIATE 不单独占一条活动记录。同一入站连接中的不同域名各自对应自己的规则和出口；没有把整个 SmartDNS 长连接固定显示为某个域名。

DNS 查询通常很短，原版面板按间隔读取活动连接快照，可能来不及显示已经完成的查询；历史记录能保留哪些条目取决于面板自身。内核不把已结束的查询伪装成活动连接。需要逐条排查时，可查看下面的 debug 日志；SmartDNS 缓存命中不会产生新的上游交换。

### 配置检查和测试

先使用新二进制检查自己的配置：

```sh
./mihomo -t -f /path/to/config.yaml
```

把 `log-level` 临时设为 `debug`，可以看到带 `[DNS proxy]` 前缀的域名、解析器和出口选择日志。SmartDNS 缓存命中时不会向上游发新查询，因此不会出现对应的新入口日志。

本仓库包含真实二进制的本地端到端测试：

```sh
go build -tags with_gvisor -o /tmp/dns-route-kernel .
python3 scripts/test-dns-proxy.py /tmp/dns-route-kernel
```

测试通过两个本地模拟 SOCKS5 出口，验证 HTTP CONNECT、SOCKS4/4a、SOCKS5 TCP、两种 UDP 入口的多域名分流、IPv4 / IPv6 解析器、选择组实时切换，以及非 `53`、非法 DNS 和分片数据包拒绝；同时检查顶层端口的读取、关闭、重开和热重载。面板验证直接读取 `/connections`，检查 TCP / UDP 的查询域名、解析器、来源、嵌套组链、流量累计和关闭操作，并确认完成后移除活动记录。使用保留的测试 IP，不需要公网 DNS，也不占用特权端口。

另外已用官方 SmartDNS `Release48.4`（`1.2026.08.05-0921`）进行实际客户端联调：带认证的 SOCKS5 UDP、SOCKS5 TCP、HTTP CONNECT TCP 三种方式均通过连续两域名分流验证。入口兼容 SmartDNS 在未指定客户端 IP 的 UDP ASSOCIATE 请求中仍填写解析器端口的行为，随后按真实首包固定 UDP 源端口。

## 实现位置与参考

- [DNS 入口](../listener/dnsproxy/listener.go)
- [每条查询的匹配及交换](../tunnel/dns_proxy.go)
- [顶层配置](../config/config.go)
- [端口管理](../listener/listener.go)
- [SmartDNS 官方代理配置](https://pymumu.github.io/smartdns/config/proxy/)
- [SmartDNS 官方配置选项](https://pymumu.github.io/smartdns/configuration/)
- [Mihomo DNS 文档](https://wiki.metacubex.one/config/dns/)
