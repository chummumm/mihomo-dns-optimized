# DNS 专用 SOCKS5 / HTTP CONNECT 入口

这个入口让 SmartDNS 发出的普通 DNS 查询，按查询中的域名（QNAME）复用 Mihomo 已有的分流规则选择出口。配置仍然只维护原来的 `rules`、`sub-rules` 和 `rule-providers`，不需要生成配置，也不需要另一份按域名划分的 DNS 策略表。

该功能独立于 Mihomo 内置 DNS 的增强模式，不要求 FakeIP。使用真实 IP 时，原有 `enhanced-mode: redir-host` 可以保留。

## 1. Mihomo 只增加一个监听块

把下面这一项加入已有的 `listeners` 列表；如果已有 `listeners:`，不要再增加第二个同名顶层键。

```yaml
listeners:
  - name: smartdns-in
    type: dns-proxy
    listen: 127.0.0.1
    port: 7853
    enable: true
```

`7853` 是本地代理入口的端口；**被代理的 DNS 服务器目标端口必须为 `53`**。入口不直接接受裸 UDP DNS，SmartDNS 必须通过 SOCKS5 或 HTTP CONNECT 连接它。

| 配置项 | 行为 |
| --- | --- |
| `enable` | 声明此监听块时默认 `true`；设置 `false` 或删除此块即可关闭 |
| `listen` | 默认 `127.0.0.1`，可以显式指定其他本地监听 IP |
| `port` | 代理监听端口，示例为 `7853` |
| `users` | 可选，使用 `username` / `password`；省略时继承全局代理认证，显式 `[]` 表示该入口不认证 |

可选认证示例：

```yaml
    users:
      - username: smartdns
        password: replace-with-your-own-password
```

专用入口总是使用主分流规则。`listener` 级别的 `rule:` / `proxy:` 覆写不适用于此入口，配置时会报错。已有普通 SOCKS、HTTP、mixed、TUN 等入口照常使用。

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

专用入口本身仅接受字面量 IPv4 / IPv6 解析器地址，拒绝解析器主机名，以免为解析器地址再触发一层循环解析。

## 5. 拒绝行为及边界

- SOCKS5 CONNECT 和 HTTP CONNECT 的目标不是 `53`：返回协议错误并关闭，不拨号。
- SOCKS5 UDP 数据报的目标不是 `53`：静默丢弃，不拨号。
- 普通 HTTP 请求、DoH、DoT、DoQ、SOCKS BIND、分片 SOCKS5 UDP、非单播解析器地址：拒绝。
- DNS 必须是有效的普通单问题查询；响应包冒充查询、多问题、尾随垃圾数据、AXFR / IXFR 等不转发。
- `REJECT` 返回 DNS `REFUSED`；`REJECT-DROP` 丢弃查询。TCP 查询被丢弃或交换失败时关闭该连接。
- 选中的出口不支持 UDP：该次 UDP 交换失败，**不会自动改为直连**。SmartDNS 可以改用 `server-tcp`。
- `DNS`、`rematch` 等需要重新进入路由的特殊出口不用于该链路；普通节点、直连和常用选择组正常使用。

策略组没有可用条目时，仍遵循该组的 `empty-fallback` 配置；上游默认的 `COMPATIBLE` 是直连。需要空组也拒绝请求时，把相应组的 `empty-fallback` 显式设为 `REJECT`。

DNS 查询内容（包括原始事务 ID、大小写和 EDNS 信息）原样发给解析器，返回包检查事务 ID、问题和来源。这里只决定这条 DNS 请求的出口，不改写 DNS 回答，也不移除 SmartDNS 自己的缓存、测速或过滤逻辑。

每个监听实例最多 128 个客户端会话、256 个并发查询；单次查询及握手超时 5 秒，空闲会话超时 60 秒。TCP 连接上的查询按帧顺序处理，每次查询独立建立上游交换，避免把不同域名固定到同一出口。策略组的定时健康检查仍然生效；此专用路径直接使用一次选定的节点，不调用策略组包裹拨号层的成功 / 失败回调。

SOCKS5 UDP 使用标准 UDP ASSOCIATE，为每个控制连接分配临时 UDP 转发端口，控制连接关闭时立即清理会话。同机 SmartDNS 可以直接使用；跨容器或其他主机访问时还需要能访问协商得到的 UDP 转发地址。

## 6. 验证与排查

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

测试通过两个本地模拟 SOCKS5 出口，验证 HTTP CONNECT、SOCKS5 TCP、同一 UDP 会话的多域名分流、IPv4 / IPv6 解析器、选择组实时切换，以及非 `53`、非法 DNS 和分片数据包拒绝。使用保留的测试 IP，不需要公网 DNS，也不占用特权端口。

## 实现位置与参考

- [DNS 入口](../listener/dnsproxy/listener.go)
- [每条查询的匹配及交换](../tunnel/dns_proxy.go)
- [监听配置](../listener/inbound/dns_proxy.go)
- [SmartDNS 官方代理配置](https://pymumu.github.io/smartdns/config/proxy/)
- [SmartDNS 官方配置选项](https://pymumu.github.io/smartdns/configuration/)
- [Mihomo DNS 文档](https://wiki.metacubex.one/config/dns/)
