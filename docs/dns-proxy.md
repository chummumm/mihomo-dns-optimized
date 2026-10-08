# DNS 按查询域名分流

通过一个顶层开关，让普通 DNS 查询复用已有的 `rules` / `rule-providers` 选择出口。无需新增专用端口、listener 类型或另一套 DNS 域名规则。

```yaml
mixed-port: 7890
mode: rule
dns-rule-routing: true
```

`dns-rule-routing` 默认 `false`。可以把这一行加入现有配置，继续使用原有 mixed、SOCKS、HTTP、透明代理或 TUN 入口。外部 DNS 分类不依赖 `dns.enable`，也不依赖普通 sniffer。本功能不使用 FakeIP；启用内置 DNS 时使用 `redir-host`。同时启用内置 FakeIP 与此开关会在配置校验时报告错误。

配套文件：

- [Mihomo 最小配置](dns-proxy.example.yaml)
- [SmartDNS 接入模板](smartdns-dns-proxy.conf)
- [完整设计与验收约定](dns-rule-routing-design.md)
- [实施与复查记录](dns-rule-routing-validation.md)
- [云编译、下载与上游同步](upstream-sync.md)

## 生效条件与普通流量

| 条件 | 行为 |
| --- | --- |
| 开关关闭 | 全部沿用原有流程 |
| 入站配置固定 `proxy:` | 保留指定出站，不运行新增的 QNAME 选路 |
| Global / Direct 模式 | 保留原模式，不运行新增的 QNAME 选路 |
| Rule 模式、没有固定出站、普通明文 TCP / UDP 53 查询 | 按每个 DNS Query 的 QNAME 选择出口 |
| 入站配置 `rule:` 子规则 | 保留该规则入口，仍可逐查询选路 |
| 目标端口不是 53 | 正常执行原有转发流程 |
| 首份负载未识别为支持的普通 DNS | 原样回到原有转发流程 |

开关开启后，普通网页、SSH 等流量不会因为目标端口不是 53 而被丢弃。这也不是一个阻止 DoH / DoT / DoQ 的防火墙开关。

TCP 在确认首帧为 DNS 后，会持续按 DNS 长度帧处理。后续畸形帧会结束该 DNS 连接，不会改成任意字节透传。模式或开关发生变化时，已经接管的 TCP 连接会在处理下一条查询时重新检查；不再符合条件就结束连接，让客户端重连后执行当前模式。未收到新数据的连接仍受空闲超时限制。

## 一个 Query 如何使用已有规则

分类发生在公共 TCP / UDP 转发链中，位于反向域名映射、普通嗅探和 UDP NAT 固定出口之前。不同代理协议解包后共用同一处逻辑，不需要分别配置协议开关。

| 规则读取的信息 | DNS 查询时的含义 |
| --- | --- |
| 域名、域名规则集 | 当前 Question 的 QNAME |
| 网站目标 IP、目标 GEOIP / ASN | 尚未知，不能拿 DNS 解析器 IP 代替 |
| DNS 解析器地址 | 仅用于将这条查询发往原解析器，不作为待查网站 IP |
| 来源 IP / 端口、入站类型 / 名称 / 用户 | 保留真实查询的信息 |
| PROCESS / UID | 保留原进程识别配置和识别结果 |
| 网络、目标端口 | 实际 DNS 查询的网络信息和目标 53 端口 |
| 入站 `rule:` | 从指定子规则开始匹配 |

规则仍按现有顺序执行；来源、进程和入站规则没有新增的优先级。例如，把 `IN-NAME` 放在域名规则前面，仍可能先命中入站规则。经过 SmartDNS 汇聚后，进程通常是 SmartDNS，无法由这条 DNS 连接还原最初访问网站的应用。进程识别也取决于平台和运行权限。

网站 IP 未知会传递到 `NOT` / `AND` / `OR`、classical provider 和子规则：`NOT(IP-CIDR,...)` 不能因为尚未解析出 IP 而错误命中。来源 IP 及带 `src` 参数的规则仍可判断。为了选路，不会再次解析当前 QNAME。

域名叶子规则和域名规则集继续复用上游匹配算法。选中策略组后，使用 QNAME 选择一次实际出站，并在本次交换中固定该选择；拨号仍然发往原解析器的 IP:53，不按解析器地址重新选择负载均衡分支。

同一个 UDP socket 或 SOCKS 会话中的不同查询独立选路；同一 TCP 连接内的不同长度帧也独立选路。若选择了不支持所需 UDP 的出口，查询失败，不跳过该规则或偷偷改为 DIRECT。可以让 SmartDNS 使用普通 TCP DNS 来适配仅支持 TCP 的节点。

`REJECT` 返回 DNS REFUSED，`REJECT-DROP` 不返回回答。DNS / REMATCH 等需要再次回流的特殊出口目前不用于自动 DNS 交换，命中时明确失败，以避免递归。其他规则控制动作继续遵守原规则入口顺序。

## SmartDNS：保留 6053 / 6553，复用 mixed 7890

SmartDNS 继续作为本地 DNS 服务监听 `6053` / `6553`。它发送上游查询时，通过现有 `mixed-port: 7890` 连接真实解析器的 53 端口：

```conf
bind 127.0.0.1:6053
bind-tcp 127.0.0.1:6053
bind 127.0.0.1:6553
bind-tcp 127.0.0.1:6553

proxy-server socks5://127.0.0.1:7890 -name mihomo-dns
server-tcp 192.0.2.53:53 -proxy mihomo-dns
server-tcp 198.51.100.53:53 -proxy mihomo-dns
```

这里的 `192.0.2.53` 和 `198.51.100.53` 是文档保留地址，实际使用必须替换为可达的 DNS 解析器。示例只说明配置结构，不包含可直接使用的公网 DNS 或私人节点。

本地 `6053` / `6553` 服务连接本身不被本开关当作远端 53 查询。SmartDNS 后续送往上游的 53 查询才由公共分类器接管。原 mixed 入口的 SOCKS5 TCP / UDP、HTTP CONNECT TCP、SOCKS4/4a 等协议继续由上游入口实现；认证、LAN 范围、绑定地址仍使用各入口原有配置。

所有可能选中的出口均支持 UDP 时，可以把 `server-tcp` 改为 `server`。HTTP 代理方式可将模板中的代理地址改为 `http://127.0.0.1:7890`，配合 `server-tcp` 发送普通 DNS over TCP。HTTP CONNECT 在这里是传输 TCP DNS 的通道，不是 DoH。

如果希望查询逐域名进入此逻辑，SmartDNS 上游使用普通 UDP / TCP 53。DoH、DoT、DoQ 中的查询内容不会由普通入站解密分类。SmartDNS 自身缓存继续生效；命中其缓存时，没有新查询进入 Mihomo。

## 内置 DNS 与 TUN DNS 劫持

`dns.listen` 和 TUN 的 `dns-hijack` 继续提供本地 DNS 服务。TUN 虚拟 DNS 地址不作为公网解析器代理出去；本地服务生成的、符合范围的普通 53 上游查询共用 QNAME 选路核心。

真实 DNS 入口携带来源、入站、用户、固定出站和子规则上下文。规则中的目标端口是上游 DNS 的 `53`；若 `dns.listen` 监听 `1053`，真实本地监听端口仍记录在 `IN-PORT`。TCP / UDP DNS 客户端的入站网络信息保留。普通业务触发的内部解析没有客户端 DNS 传输，则使用该逻辑查询选定的普通 DNS 上游网络信息，不能把原网站的 TCP:443 当成 DNS 目标条件。

例如，需要内置 DNS 时可使用下面的结构，并替换文档保留地址：

```yaml
mixed-port: 7890
mode: rule
dns-rule-routing: true
dns:
  enable: true
  listen: 127.0.0.1:1053
  enhanced-mode: redir-host
  default-nameserver:
    - 192.0.2.53
  nameserver:
    - tcp://198.51.100.53:53
rules:
  - DOMAIN-SUFFIX,example.com,DIRECT
  - MATCH,DIRECT
```

开启时，以下业务域名策略停用，也不为这些停用字段加载其专属 provider / geosite 条件：

- `nameserver-policy`
- `direct-nameserver-follow-policy`
- `fallback-filter.domain`
- `fallback-filter.geosite`

这些字段在 Global / Direct 模式下也不会重新启用；关闭 `dns-rule-routing` 并重新应用完整配置后恢复原语义。业务域名的分流应统一写入原有 `rules` / `rule-providers`。

Global / Direct 模式下，内置 DNS 的传输本身保持原行为：`respect-rules: false` 不会因为切到 Global 被强制改走 GLOBAL；`respect-rules: true` 仍由原 DNSDialer 处理。自动 QNAME 路由只在 Rule 模式的适用范围内接管。

仍保留 `nameserver`、`fallback`、`fallback-lazy-query`、`fallback-filter.geoip/ipcidr`、`direct-nameserver`、`proxy-server-nameserver`、`proxy-server-nameserver-policy`、`default-nameserver`、hosts、系统 hosts、缓存和回答类型控制。自动分流范围内的普通 53 交换按 QNAME 选路，不再由 `respect-rules` 或 `#RULES` 按解析器地址进行第二次选择。显式的 DNS `#Group` / `#DIRECT` / `#interface` 仍保留其原有运输约束。

同一逻辑查询的自动 main / fallback 和截断重试共用一次选路结果；显式固定 DNS 出口仍是例外。路由作用域参与缓存与并发合并，避免不同来源、子规则或出口的查询混用结果。规则变动后不会仅因旧的成功缓存而跳过当前拒绝动作；SmartDNS 的独立缓存仍需由 SmartDNS 管理。

缓存隔离包含真实来源端口，以免 `SRC-PORT` 等规则被跨 socket 的缓存复用绕过；因此不同 socket 的内置缓存命中率可能低于只按域名缓存。SmartDNS 的缓存不受这个标识影响。

## 独立基础解析与已有出站

原请求可以用 IP 或域名指定 DNS 解析器。解析器自身是域名时，只通过独立 bootstrap 解析原解析器 Host，再把原始查询发到其真实 IP:53；不会用待查业务 QNAME 引导解析器。

节点域名、DNS 解析器域名使用独立的基础解析器。没有配置 `proxy-server-nameserver` 时，开启此功能使用独立 `default-nameserver` 路径，不能无条件退回自动业务 main resolver。即使 `dns.enable: false`，外部 DNS 分类所需的独立基础解析仍保留，但不会因此启用内置 DNS 监听服务。

普通规则为判断 IP 条件发起解析时，允许进行一次 QNAME 选路，不会重新进入需要解析同一名字的 IP 判断循环。DIRECT 等实际出站已经选定后，其后续本地目标解析继承该出站、接口和 mark，不再次按业务规则选路。

## 报文范围、回退和资源限制

自动接管的是目标端口 53 上的普通明文 UDP 消息或 TCP 长度帧，采用单问题 QUERY。校验包含完整报文消费、真实记录计数、OPT 唯一性及所属区段、响应来源和 ID / Question 关联。

| 内容 | 处理范围 |
| --- | --- |
| A、AAAA、HTTPS / SVCB、TXT、MX 等普通查询 | 支持 |
| DNSSEC 记录、DO 位、合法 EDNS 扩展、未知合法记录类型 | 不用窄类型白名单排除；不提供 DNSSEC 签名验证 |
| TCP 最大 65535 字节消息、拆分长度前缀、连续多帧 | 支持；每帧分别交换和选路 |
| TCP 客户端预先发送多条查询 | 接受缓冲中的连续帧，当前逐条处理并返回 |
| UDP 截断回答 | 外部过境查询返回原回答，由客户端决定重试；内置 resolver 的 TCP 重试保留选路结果 |
| 多问题、动态更新、AXFR / IXFR 等特殊用途 | 外部转发入口不作为普通 DNS 接管，首份报文走原流程；内置自动 resolver 拒绝不支持的查询 |
| DoH / DoT / DoQ、非 53 明文 DNS | 不自动分类；继续按原有连接规则处理 |
| 未封装为代理请求、直接发给 mixed 端口的裸 DNS | mixed 仍是代理协议入口；裸 DNS 应使用 `dns.listen` 或 SmartDNS 本地监听 |

TCP 首帧探测不消费回退所需字节；失败时保留已读内容并清除探测 deadline。确认 DNS 后，格式错误、回答不匹配或交换失败会结束 TCP DNS 连接；UDP 对应查询不返回回答，不另找未经规则选择的出口。

公共外部分类器在识别到候选 DNS 头后，限制最多 128 个完整探测或处理中的 TCP DNS 会话、256 个并发 DNS 交换；达到容量时不绕过当前选择直接回退直连。首帧探测和单次交换使用 5 秒上限，已接管 TCP 的空闲读取上限为 60 秒。DNS 分类的这些限制不作用于其他目标端口的普通业务连接。

## 配置重载与面板

配置文件中省略开关等同于 `false`。更改开关需重新加载完整配置，例如通过控制器 `PUT /configs` 的完整配置重载机制；`PATCH /configs` 不接受 `dns-rule-routing`，带此字段的 PATCH 会在修改任何设置前被拒绝。`GET /configs` 返回当前生效的布尔值。模式仍可使用原有模式切换方式更改。

从旧版本迁移时，移除旧 `dns-proxy-port`，把 SmartDNS 代理地址改成现有 mixed / SOCKS / HTTP 入口，再增加 `dns-rule-routing: true`。旧的非零专用端口配置会报告迁移错误。当前不存在专用 DNS listener 类型或每入口启用开关。

每次实际 DNS 上游交换使用原生连接 tracker，显示 QNAME、真实解析器 IP:53、来源、入站、命中规则、策略组链及流量。面板关闭操作可以取消正在进行的交换；结束的短查询会移出活动列表。过境 DNS 不再额外统计一层相同流量。

这项功能只改变所支持 DNS 查询的选路。普通 SSH 等连接的 IP 到域名显示仍受原有映射和嗅探机制影响，不能据此保证消除所有普通连接的域名显示关联。

## 源码与本地验证

公共入站分类位于 `tunnel/dns_inbound.go`，QNAME 选路与原解析器交换位于 `tunnel/dns_routing.go` 和 `tunnel/dns_proxy.go`，严格报文校验位于 `component/dnsmessage`。内置 resolver 桥接位于 `dns/routing.go`，TUN 本地服务上下文位于 `listener/sing_tun/dns.go`。

使用项目要求的 Go 工具链执行：

```bash
bash scripts/ci-check.sh test
```

完整二进制端到端检查使用 `scripts/test-dns-proxy.py`。文件名保留以兼容既有工作流，其测试对象已是公共入站和全局开关；测试使用本地模拟服务，不需要实际节点账号。具体交付的测试和云编译结果以对应提交的 Actions 记录为准。
