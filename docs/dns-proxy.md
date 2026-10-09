# DNS 按查询域名分流

开启顶层 `dns-rule-routing: true` 后，Mihomo 的内置 DNS 先用 Question 中的域名匹配已有 `rules` / `rule-providers`，取得一次实际出站，再选择 DNS 上游池。无需 SmartDNS，也无需维护另一份 DNS 域名规则。

```yaml
mode: rule
mixed-port: 7890
dns-rule-routing: true
dns:
  enable: true
  listen: 127.0.0.1:1053
  enhanced-mode: redir-host
  default-nameserver: [192.0.2.53]
  direct-nameserver: [203.0.113.53]
  nameserver: [198.51.100.53]
```

以上均为文档保留地址，使用前须替换成可达的 DNS 服务器。建议把国内解析器放入 `direct-nameserver`，海外解析器放入 `nameserver`；程序根据实际出站类型选池，不根据这些地址推测国家或地区。完整示例见 [dns-proxy.example.yaml](dns-proxy.example.yaml)，其中保留原 mixed 入口，并显式开启可选的 IP 测速、预取和过期回答设置。

开关默认 `false`。本功能使用真实 DNS 回答；同时启用内置 FakeIP 会被配置校验拒绝，内置 DNS 应使用 `redir-host`。

## 内置 DNS 如何选择上游

`dns.listen`、TUN DNS 劫持以及适用的内部解析，共用已有 QNAME 选路核心。在 Rule 模式中，先选出实际叶子，再按下表处理：

| 选路结果 | 内置 DNS 上游与传输 |
| --- | --- |
| 实际叶子是 DIRECT 或 Compatible，且 `direct-nameserver` 非空 | 只使用直连池，通过本次选中的实际出站交换；失败不借用 nameserver / fallback |
| 实际叶子是代理节点 | 使用 `nameserver`，按原设置决定是否使用 `fallback`；自动上游均继承本次出站 |
| `direct-nameserver` 为空 | 保留原 nameserver / fallback 行为，DIRECT 查询不会凭空生成第二个池 |
| REJECT / REJECT-DROP | 自动查询在缓存前返回 REFUSED / 静默丢弃；所选集合中的显式传输例外见下文 |

判断依据是实际叶子的类型，不是组名或节点名。策略组选择了 DIRECT，就使用直连池；把代理节点命名为 DIRECT 不会把它当作直连。组选择、上游重试、main / fallback 与 UDP 截断后的 TCP 重试共用同一计划，不再次推进负载均衡。

这里补齐的是**首次收到业务查询时选 DNS 池**。原 `DirectResolver` 供已有直连出站解析目标的用途仍保留；主 resolver 复用其上游客户端，不递归调用它来重新选路。

## 两条入口的目的地址不同

| 入口 | DNS 服务器由谁指定 |
| --- | --- |
| 裸 DNS 发给 `dns.listen`，或被 TUN DNS 劫持 | Mihomo 按上面的规则结果选择 direct / main 池 |
| 客户端经 mixed / SOCKS / HTTP / 透明代理请求某个解析器的 TCP / UDP 53 | 保留客户端指定的原解析器，只按 QNAME 选择出口；不改投内置 DNS 池 |

公共入站仍只识别目标端口 **53** 上的普通明文 DNS。非 53、DoH / DoT / DoQ 和首份负载未识别为普通 DNS 的连接继续原有流程，不会因此被丢弃或解密。mixed 本身是代理协议入口；裸 DNS 应发到 `dns.listen`。

UDP 每个数据报独立选路；TCP 每个 DNS 长度帧独立选路。确认 TCP 为 DNS 后，后续畸形帧结束该连接，不改成任意字节透传。已接管连接在下一帧检查模式和开关变化，不再适用时结束连接。

## 原规则顺序与优先级

入站固定 `proxy:`、Global / Direct 模式仍优先于新增自动路由。入站仅指定 `rule:` 时，保留其子规则入口。来源、进程和入站规则没有被强制放在域名之后，仍按照原 `rules` 顺序执行。

| 规则属性 | DNS 查询中的含义 |
| --- | --- |
| 域名及域名规则集 | 当前 Question 的 QNAME |
| 网站目标 IP、GEOIP、ASN | 尚未知；不能拿解析器 IP 代替，也不为选路递归解析同一个域名 |
| SRC / IN / PROCESS / UID | 保留真实来源、入站和已有进程信息 |
| 目标端口 | 本次普通 DNS 上游的 53；本地监听 1053 等端口仍保留在 IN-PORT |
| NETWORK | 真实 DNS 客户端的 TCP / UDP；内部 lookup 没有客户端 DNS 传输时，使用该逻辑查询的上游协议 |

未知目标 IP 会通过 NOT / AND / OR、classical provider、子规则传播，不能让 `NOT(IP-CIDR,...)` 因尚未解析而误命中。来源 IP 规则仍可判断。域名叶子与规则集继续复用上游算法，共用原外层控制流；DNS 只增加未知目标字段的适配。

经本地 DNS 转发器汇聚后，PROCESS 通常只看到该转发器，无法还原最初访问网站的应用。进程识别仍受原版平台、权限和配置影响。

选中的出口不支持所需 UDP 时明确失败，不跳过命中规则或偷偷改为 DIRECT。DNS / REMATCH 回流型特殊出口目前明确报错，避免递归；PASS 等控制动作继续服从原规则顺序。

## 显式上游、基础解析与旧 DNS 设置

自动传输限于未固定出口的普通 UDP / TCP 53 上游。`#Group`、`#DIRECT`、`#interface`、显式适配器，以及 DoH / DoT / DoQ、非 53 上游，保留自身旧传输行为。`#RULES` 属于自动范围。

先选池，再判断**所选池**的例外：未用到的直连池显式上游不会削弱 main 的 REJECT；选中直连池时，也不受未用到的 main / fallback 显式配置影响。如果选中集合混有显式上游，显式分支仍可回答，即使自动分支被拒绝或失败。希望拒绝动作覆盖整个查询时，所选池应使用自动的普通 53 上游。所有候选都在自动范围之外时，保留旧行为，不运行新增选池。

节点域名、解析器自身域名使用独立 bootstrap。`proxy-server-nameserver` 继续供节点解析；未配置时，启用本功能使用独立 `default-nameserver`，不会回到待建立的业务代理中引导自己。普通出站已确定实际叶子后，其内部目标解析继承该叶子和接口 / mark，不重跑业务规则；可依据该叶子选择 DNS 池。

Global / Direct 模式不建立自动 QNAME 计划。内置 DNS 继续原 `respect-rules` 语义：`false` 不会因 Global 模式强制走 GLOBAL，`true` 仍交给原 DNSDialer。自动范围内的 `respect-rules` / `#RULES` 则由 QNAME 计划接管，不再按解析器地址进行第二次选路。

开启总开关时，以下业务域名策略停用，也不加载其专属 provider / geosite 条件：

- `nameserver-policy`
- `direct-nameserver-follow-policy`
- `fallback-filter.domain`
- `fallback-filter.geosite`

这些策略不会因切到 Global / Direct 而恢复；关闭开关并重新应用完整配置后恢复原语义。`fallback-lazy-query`、`fallback-filter.geoip/ipcidr` 继续用于实际选中的 main / fallback 路径。TXT / MX / HTTPS 等原本不用 fallback 的查询，不把闲置 fallback 算作拒绝动作的例外。

hosts、系统 hosts、回答类型控制、`proxy-server-nameserver-policy` 等原有基础能力仍保留。该功能不是 SmartDNS 全部配置或行为的兼容实现。

## 可选 IP 测速

测速只为内置 DNS 已确定的 DIRECT / Compatible 路径优选返回的 A / AAAA 地址。所选上游集合必须全部是自动的普通 53 DNS；代理路径、显式或不支持的传输跳过。默认不探测。

```yaml
dns:
  speed-check-mode: [tcp:443, tcp:80, ping]
  speed-check-timeout: 1000
  speed-check-concurrency: 16
```

每个候选按模式顺序尝试可用测法，比较成功探测的耗时。TCP 测的是握手，ping 测的是 ICMP 往返，不是下载带宽。TCP / ICMP 使用本次直连出口的接口和 mark；TCP 探测禁用延迟到首次写入才建连的 TFO 行为。ICMP 需要平台与权限支持，失败可由后续测法或原回答兜底。

从多个回答收集候选时，不拼接不同服务器的 CNAME 链；选出候选后使用其所属完整回答，只收窄对应的未签名地址记录。DNSSEC DO / CD 查询、AD 或签名回答等保护场景不改写。没有成功探测但已有有效 DNS 回答时，返回原回答；这不是因测速失败而把正常域名判为不可达。

| 设置 | 默认值与单位 |
| --- | --- |
| `speed-check-mode` | 空列表或 `[none]` 表示关闭；`none` 不能与其他模式混用；支持 `tcp:端口`、`ping` |
| `speed-check-timeout` | 毫秒；0 采用 1000，显式范围 1–5000；包含本轮上游收集和探测的共同预算 |
| `speed-check-concurrency` | 0 采用 16，最大 256；同一 checker 的并发探测受此上限约束 |
| 候选地址上限 | 每轮最多 256 个去重候选，非可配置项 |

## 缓存、预取与过期回答

自动路由先于缓存，REJECT / DROP 不会被旧成功答案绕过。缓存和并发合并按池、实际叶子、组、来源、子规则、固定出站及查询内容隔离；来源端口也参与，因此不同 socket 的命中率可能低于只按域名缓存。

| 设置 | 默认值与行为 |
| --- | --- |
| `prefetch-domain` | `false`；开启后为近期活跃的真实 DNS 来源预取 |
| `serve-expired` | `true`；允许暂时回答过期缓存并触发有界后台更新 |
| `serve-expired-ttl` | 秒；0 表示不限制过期年龄，保持上游缺省行为；例如 604800 表示最多使用过期 7 天的记录 |
| `serve-expired-reply-ttl` | 秒；默认 1，允许 0；这是返回给客户端的 TTL，不延长缓存的原始过期时间 |

预取只统计带有效来源的真实 DNS 入站。相同路由作用域 1 分钟内至少 3 次前台请求后，按剩余 TTL 的约 80% 安排刷新，且最近 1 分钟仍须有真实请求。匿名内部查询、bootstrap 和后台刷新本身不累计热度。每个 resolver 最多保留 1024 个热点，所有 resolver 共用最多 16 个后台任务，没有无限排队。

每次后台刷新作为新的逻辑查询重新检查当前规则和拒绝动作；其内部重试再共用新计划。刷新保留真实来源和已知或未知的 PROCESS 快照，不拿旧 source socket 去识别可能复用端口的新进程。模式 / 开关世代变化、清缓存、重载会使旧任务失效并取消后台工作，旧结果不得重新填回已清理的缓存。切组后的新作用域也不会继承旧作用域的持续热度。

## SmartDNS 兼容接法

现有 SmartDNS 可以继续监听 6053 / 6553，通过 mixed / SOCKS / HTTP 将普通上游 TCP / UDP 53 发给 Mihomo；见 [旧接入模板](smartdns-dns-proxy.conf)。它属于“保留原解析器地址”的过境路径，不是本页主方案的内置 DNS 选池。6053 / 6553 本身不会被当作远端 53 自动改投。

SmartDNS 命中自己的缓存时没有新请求进入 Mihomo，其缓存由 SmartDNS 管理。若改用本页的内置 DNS 方案，客户端直接使用 `dns.listen`，不需要保留这层转发器。

## 报文、面板与重载

普通 A、AAAA、HTTPS / SVCB、TXT、MX、合法 EDNS 和未知合法记录类型均可按 QNAME 选路，不验证 DNSSEC 签名。自动分类使用单问题 QUERY，校验完整报文、记录计数、OPT、响应来源、ID 和 Question。多问题、动态更新、区域传送等不作为普通查询接管；内置自动 resolver 对不支持的查询明确报错。

TCP 支持 65535 字节消息、拆分长度前缀和缓冲中的连续帧，当前逐条交换。外部 UDP 截断回答原样返回，由客户端决定重试；内置 UDP→TCP 重试保持同一计划。外部分类器有最多 128 个已识别候选 TCP 会话、256 个并发交换的上限，探测 / 单次交换上限 5 秒，已接管 TCP 空闲读取上限 60 秒；这些限制不作用于普通非 53 业务。

每次真实 DNS 上游交换接入原生连接面板，显示 QNAME、实际解析器 IP:53、来源、入站、命中规则和策略组链。面板关闭可中断交换，短查询完成后从活动列表移除。过境 DNS 不额外重复统计同一流量。普通 SSH 的反向映射或嗅探显示不在本功能修改范围内。

更改 `dns-rule-routing` 应重新加载完整配置。`GET /configs` 返回生效值，`PATCH /configs` 不接受该字段；可通过原 `PUT /configs` 完整重载。省略开关等同于关闭。旧 `dns-proxy-port` 应删除，不再新增专用 DNS listener。

实现与核验见 [设计约定](dns-rule-routing-design.md)、[验证记录](dns-rule-routing-validation.md)；构建和更新见 [上游同步说明](upstream-sync.md)。本地运行 `bash scripts/ci-check.sh test`，完整二进制检查使用 `scripts/test-dns-proxy.py`；最终结果以对应提交的 Actions 为准。
