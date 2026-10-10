# DNS 按查询域名分流

开启顶层 `dns-rule-routing: true` 后，Mihomo 的内置 DNS 先用 Question 中的域名匹配已有 `rules` / `rule-providers`，取得一次实际出站，再选择 DNS 上游池。无需 SmartDNS，也无需维护另一份 DNS 域名规则。

原生上游支持配置的 UDP / TCP 任意端口、DoT、DoH、DoH 的 HTTP/3 和 DoQ。**IP 测速与双栈优选只用于实际 DIRECT / Compatible，代理路径不做本地 IP 探测。** 普通代理入站不再自动识别和接管目标 53 的 DNS 报文；需要按查询域名分流时，使用内置 DNS 监听或显式 DNS 劫持。

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

开关默认 `false`。本功能使用真实 DNS 回答；同时启用内置 FakeIP 会被配置校验拒绝，内置 DNS 可使用 `normal` 或 `redir-host`。

## enhanced-mode 与域名嗅探

`dns.enhanced-mode` 控制 DNS 回答与域名映射方式，不是顶层 Rule / Global / Direct 的分流模式，也不是 DNS 缓存开关。

| 模式 | 行为 |
| --- | --- |
| `normal` | 返回真实 DNS 回答，不建立真实 IP 到历史查询域名的反查映射 |
| `redir-host` | 返回真实 DNS 回答，并维护真实 IP 到域名的映射，供缺少目标域名的连接辅助匹配 |
| `fake-ip` | 分配虚拟地址并在连接时恢复域名；当前不能与已启用内置 DNS 的 `dns-rule-routing` 同时使用 |

`normal` 仍保留正常解析、正向回答缓存、QNAME 选池和按配置启用的 DIRECT 测速等优化。SOCKS / HTTP 代理握手已经提供目标域名时，域名规则照常匹配。TUN / TProxy / redir 等入口只提供真实 IP 时，若没有成功嗅探到域名，就需依靠 IP、来源、进程、入站或兜底规则。真实 IP 映射可能因多个域名共用同一 IP 而产生歧义，不能将历史映射视为该连接真实请求域名的保证。

HTTP / TLS 嗅探可以配置覆盖全部目标端口，无需修改协议默认值。下面是搭配 `normal` 使用的可选配置片段；按需并入已有配置，不必替换其他 DNS 和嗅探设置：

```yaml
dns:
  enhanced-mode: normal

sniffer:
  enable: true
  parse-pure-ip: true
  override-destination: false
  sniff:
    TLS:
      ports: ["1-65535"]
      override-destination: false
    HTTP:
      ports: ["1-65535"]
      override-destination: false
```

`parse-pure-ip` 允许对缺少域名的连接嗅探；`normal` 下不需要为历史映射设置 `force-dns-mapping`。`override-destination: false` 让本次嗅探结果辅助路由，不主动把实际目的地址改成嗅探出的域名；它不是关闭历史 DNS 映射的开关。协议级设置优先于全局设置，合并配置时，已有 HTTP / TLS 子项中的 `override-destination: true` 也须删除或改成 `false`。

全端口表示在符合嗅探条件的 TCP 连接上识别 HTTP Host / TLS ClientHello 中可见的服务器名，不主动扫描端口，也不解密 TLS 应用数据。已有明确域名的代理请求不会仅因端口范围扩大就被强制再次嗅探。没有可见域名、ECH 隐藏真实名称、非 HTTP / TLS 或嗅探失败的连接，不能保证恢复域名。范围扩大后，某些等待服务端先发数据的协议可能多出首包等待；可按需要缩小端口或配置跳过地址。QUIC 属于独立的 UDP 嗅探设置，不由这两个 TCP 端口范围开启。

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

未指定固定出口的下列原生上游均能继承这次计划；示例地址仅用于说明格式：

| 上游协议 | 配置形式示例 | 实际传输 |
| --- | --- | --- |
| UDP | `198.51.100.53:5353` | UDP，使用配置端口 |
| TCP | `tcp://198.51.100.53:5353` | TCP，使用配置端口 |
| DoT | `tls://resolver.example:853` | TLS / TCP |
| DoH | `https://resolver.example/dns-query` | HTTPS / TCP，可复用 HTTP/2 |
| DoH HTTP/3 | `https://resolver.example/dns-query#h3=true` | HTTP/3 / QUIC / UDP |
| DoQ | `quic://resolver.example:853` | QUIC / UDP |

TLS 的服务器名、证书校验和 HTTP 协议仍由对应客户端处理；QNAME 选路不会自动关闭证书校验，也不会把业务域名当成解析器的 TLS 名称或拨号目标。实际需要 UDP 的传输仍要求选中的节点支持 UDP；可根据节点能力选择 TCP、DoT 或普通 DoH。

## 内置 DNS 与普通代理转发

| 入口 | DNS 服务器由谁指定 |
| --- | --- |
| 裸 DNS 发给 `dns.listen`，或被 TUN DNS 劫持 | Mihomo 按上面的规则结果选择 direct / main 池 |
| 客户端经 mixed / SOCKS / HTTP / 透明代理请求某个解析器的 TCP / UDP 53，且未配置显式 DNS 劫持 | 按原普通连接规则转发，不检查 DNS Question，不执行逐查询 QNAME 分流或改投内置 DNS 池 |

目标端口 53 不再触发专用 DNS 首帧识别、逐包 / 逐帧选路或专用连接限额。普通 DNS、DoH / DoT / DoQ 与其他流量按普通代理流程处理。mixed 本身是代理协议入口；裸 DNS 应发到 `dns.listen`。

显式配置的 TUN DNS 劫持仍把请求交给内置解析器；普通 SOCKS / HTTP 请求在代理握手中直接提供目标域名时，也仍按该连接的域名匹配业务规则。这两种能力不依赖已删除的端口 53 自动识别。

## 原规则顺序与优先级

入站固定 `proxy:`、Global / Direct 模式仍优先于新增自动路由。入站仅指定 `rule:` 时，保留其子规则入口。来源、进程和入站规则没有被强制放在域名之后，仍按照原 `rules` 顺序执行。

| 规则属性 | DNS 查询中的含义 |
| --- | --- |
| 域名及域名规则集 | 当前 Question 的 QNAME |
| 网站目标 IP、GEOIP、ASN | 尚未知；不能拿解析器 IP 代替，也不为选路递归解析同一个域名 |
| SRC / IN / PROCESS / UID | 保留真实来源、入站和已有进程信息 |
| DST-PORT | 原生查询匹配规则时使用逻辑 DNS 端口 53；实际 DoH 的 443、DoT 的 853 或自定义端口用于传输，不替换这个规则条件；本地监听端口保留在 IN-PORT |
| NETWORK | 真实 DNS 客户端的 TCP / UDP；内部 lookup 没有客户端 DNS 传输时，按首个可自动路由上游的协议建立逻辑查询，选池和重试不再次改变规则决定 |

未知目标 IP 会通过 NOT / AND / OR、classical provider、子规则传播，不能让 `NOT(IP-CIDR,...)` 因尚未解析而误命中。来源 IP 规则仍可判断。域名叶子与规则集继续复用上游算法，共用原外层控制流；DNS 只增加未知目标字段的适配。

### SRC-IP 看见哪个客户端

`SRC-IP-CIDR`、`SRC-IP-SUFFIX` 等匹配 **Mihomo 实际收到的来源**，不会从 QNAME 推测访问者。客户端直接访问 `dns.listen` 时使用该连接或数据报的来源；TUN DNS 劫持传递其保留的客户端元数据。普通代理连接的内部解析只使用调用方实际传入的元数据，不从过境 DNS 报文提取域名或来源。

如果路由器、SmartDNS、dnsmasq 或其他转发器重新发起查询，Mihomo 通常只看见转发器的 IP 和来源端口，PROCESS 也通常属于转发器。NAT 抹去的来源、未传入的原应用信息不会被这个开关恢复。需要按每台设备分流首次 DNS 查询时，应让查询带着真实客户端来源到达 Mihomo。进程识别仍受平台、权限和原有设置影响。

规则按既有顺序执行。例如下面的来源规则在前，该来源访问 `example.net` 也先命中 DIRECT：

```yaml
rules:
  - SRC-IP-CIDR,192.0.2.10/32,DIRECT
  - DOMAIN-SUFFIX,example.net,Proxy
  - MATCH,DIRECT
```

需要某个域名或屏蔽列表对这些设备同样优先时，将相应业务规则放到来源规则之前。已经固定出口的入站和非 Rule 模式仍保持上文的优先级。

自动路由先用真实来源及端口执行完整规则，再决定应答缓存是否可复用。成功且全部自动的查询，可以在同一来源 IP、进程、认证和入站作用域内跨临时源端口复用相同出口、上游池和查询内容的应答；不同来源、规则结果或出口仍隔离。源端口影响规则时仍然生效，不会先读缓存绕过规则。在途查询保持源端口隔离，面板取消不会误伤其他源端口。来源已汇聚成同一转发器时，只能使用目前可见的信息。详见 [性能与安全边界](dns-performance.md)。

选中的出口不支持所需 UDP 时明确失败，不跳过命中规则或偷偷改为 DIRECT。DNS / REMATCH 回流型特殊出口目前明确报错，避免递归；PASS 等控制动作继续服从原规则顺序。

## 显式上游、基础解析与旧 DNS 设置

原生 UDP / TCP、DoT、DoH / HTTP/3 和 DoQ 上游，只要没有固定出口，都属于自动范围，配置端口不构成例外。`#RULES` 也属于自动范围。`#Group`、`#DIRECT`、`#interface` 或显式适配器保留自身原有传输行为；其他未接入自动路由的特殊解析器也保留旧行为。

先选池，再判断**所选池**的例外：未用到的直连池显式上游不会削弱 main 的 REJECT；选中直连池时，也不受未用到的 main / fallback 显式配置影响。如果选中集合混有显式上游，显式分支仍可回答，即使自动分支被拒绝或失败。希望拒绝动作覆盖整个查询时，所选池应全部使用自动上游，可以使用明文或加密协议。所有候选都在自动范围之外时，保留旧行为，不运行新增选池。

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

测速只为内置 DNS 已确定的 DIRECT / Compatible 路径优选返回的 A / AAAA 地址。所选上游集合必须全部属于自动范围；使用 DoH、DoT、DoQ 或非 53 端口不会单独禁止测速。代理路径、显式或未接入自动路由的传输跳过。默认不探测。

```yaml
dns:
  speed-check-mode: [tcp:443, tcp:80, ping]
  speed-check-timeout: 1000
  speed-check-concurrency: 16
```

每个候选按模式顺序尝试可用测法，比较成功探测的耗时。多个查询共用有界工作池；同实际直连出口及套接字策略下的相同 IP 探测可合并，成功结果短期复用。每个等待者独立接收结果，包括双栈优选所需的 RTT。TCP 测的是握手，ping 测的是 ICMP 往返，不是下载带宽。TCP / ICMP 使用本次直连出口的接口和 mark；TCP 探测禁用延迟到首次写入才建连的 TFO 行为。ICMP 需要平台与权限支持，失败可由后续测法或原回答兜底。

从多个回答收集候选时，不拼接不同服务器的 CNAME 链；选出候选后使用其所属完整回答，只收窄对应的未签名地址记录。DNSSEC DO / CD 查询、AD 或签名回答等保护场景不改写。没有成功探测但已有有效 DNS 回答时，返回原回答；这不是因测速失败而把正常域名判为不可达。

| 设置 | 默认值与单位 |
| --- | --- |
| `speed-check-mode` | 空列表或 `[none]` 表示关闭；`none` 不能与其他模式混用；支持 `tcp:端口`、`ping` |
| `speed-check-timeout` | 毫秒；0 采用 1000，显式范围 1–5000；限制可选优选等待，不缩短正常 DNS 解析期限 |
| `speed-check-concurrency` | 0 采用 16，最大 256；只限制活动 IP 探测，满额立即跳过新探测，不排队，不限制 DNS 上游并发 |
| 候选地址上限 | 每轮最多 256 个去重候选，非可配置项 |

## 可选双栈优选

双栈优选还要求上面的 DIRECT 测速已经启用，配置字段如下：

| 设置 | 默认值与行为 |
| --- | --- |
| `dualstack-ip-selection` | `false`；开启 A / AAAA 族间比较 |
| `dualstack-ip-selection-threshold` | 毫秒；默认 10，范围 0–1000，可显式设 0 |
| `dualstack-ip-allow-force-aaaa` | `false`；默认保留 IPv4，仅显式开启后才允许优先 IPv6 而过滤 A |

默认在 AAAA 查询中辅助查询同域名的 A。两族复用已经冻结的出口、同一上游池、一个优选等待期限和共享探测并发上限；正常 DNS 查询保留自己的解析期限，不重新选组，也不通过主 resolver 递归查询。只有两族均有有效地址和成功测速结果，且 IPv4 比 IPv6 快至少指定阈值，才对原 AAAA 返回 NOERROR / NODATA。开启 `dualstack-ip-allow-force-aaaa` 才允许对 A 做对称处理。全局 IPv6 或 `dns.ipv6` 关闭时，跨族优选不生效，不会用无法交付给客户端的 AAAA 抑制 A；已有配置仍可正常加载。

代理路径不发这种辅助查询。DNSSEC 保护、某族解析错误、没有地址或某族探测全部失败时，不把失败当成较慢而过滤原有效回答。辅助结果不单独写入另一族缓存。

优选合成的 NODATA 使用零 TTL SOA，不进入缓存。如果新回答替代了该作用域内的旧正向缓存，旧条目也会失效，避免继续用数天的过期 AAAA 答案或把短时偏好固化成长期负缓存。

## CNAME 与 TTL 调整

| 设置 | 默认值与行为 |
| --- | --- |
| `force-no-cname` | `false`；压平完整、无歧义的未签名 A / AAAA CNAME 链 |
| `rr-ttl-min` | 秒；0 不设下限，零 TTL 始终保持零 |
| `rr-ttl-max` | 秒；0 不设上限；非零时须不小于配置的最小值 |

去 CNAME 只使用同一回答里已存在的终点地址，将记录归属名改成查询名，并采用该回答链的最短 TTL；不补造 IP、不重新查询 CNAME 目标，也不因此改变出口。显式 CNAME 查询、DNAME、断链、循环、歧义链及额外不相关的 Answer 记录保留原形。

DO / CD 查询以及 AD、RRSIG、SIG、TSIG 等受保护回答不做这类改写，截断和不匹配回答也跳过。TTL 调整不改 EDNS OPT 的标志位，最大值同时限制 SOA 的负缓存寿命。调整在写缓存前完成一次；缓存命中和过期回答不再次套用最小 TTL。节点和解析器自身的基础解析不继承这些业务设置。

这些是独立的可选回答设置，不要求实际出口为 DIRECT；只有测速和双栈比较限于 DIRECT。即使在显式上游或非 Rule 模式下启用回答调整，缓存仍区分 DO / CD、EDNS 等查询差异，避免把已改写结果交给受保护的请求。本功能不验证 DNSSEC 签名。

## 缓存、预取与过期回答

自动路由先于缓存，REJECT / DROP 不会被旧成功答案绕过。正式应答缓存保留查询内容、来源 IP、进程、认证、入站、子规则、实际出口及上游池等作用域；选路成功且所选上游全部属于自动范围时，可跨临时源端口复用。在途合并仍按源端口隔离，并区分缓存世代及前台 / 后台任务。

| 设置 | 默认值与行为 |
| --- | --- |
| `prefetch-domain` | `false`；开启后为近期活跃的真实 DNS 来源预取 |
| `serve-expired` | `true`；允许暂时回答过期缓存并触发有界后台更新 |
| `serve-expired-ttl` | 秒；0 表示不限制过期年龄，保持上游缺省行为；例如 604800 表示最多使用过期 7 天的记录 |
| `serve-expired-reply-ttl` | 秒；默认 1，允许 0；这是返回给客户端的 TTL，不延长缓存的原始过期时间 |
| `cache-algorithm` / `cache-max-size` | 保留原配置能力；容量用于各 resolver 的缓存，不代表预分配相同数量的记录 |

预取只统计带有效来源的真实 DNS 入站。相同路由作用域 1 分钟内至少 3 次前台请求后，按剩余 TTL 的约 80% 安排刷新，且最近 1 分钟仍须有真实请求。匿名内部查询、bootstrap 和后台刷新本身不累计热度。每个 resolver 最多保留 1024 个热点，所有 resolver 共用最多 16 个后台任务，没有无限排队。

每次后台刷新作为新的逻辑查询重新检查当前规则和拒绝动作；其内部重试再共用新计划。刷新保留真实来源和已知或未知的 PROCESS 快照，不拿旧 source socket 去识别可能复用端口的新进程。模式 / 开关世代变化、清缓存、重载会使旧任务失效并取消后台工作，旧结果不得重新填回已清理的缓存。切组后的新作用域也不会继承旧作用域的持续热度。

成功取得零 TTL 的新回答时会移除该作用域的旧缓存，而不只是跳过本次写入。删除和写入都受同一世代检查保护；已经失效的旧任务不能删除新世代的缓存。

## 独立域名屏蔽文件

屏蔽清单可以直接作为普通 file provider，由同一份 `rules` 引用：

```yaml
rule-providers:
  custom_block:
    type: file
    behavior: domain
    format: yaml
    path: ./rule_provider/custom_block.yaml
rules:
  - RULE-SET,custom_block,REJECT
  - MATCH,DIRECT
```

对应的 `rule_provider/custom_block.yaml` 单独维护：

```yaml
payload:
  - '+.blocked.example'
  - '+.telemetry.example'
```

`+.` 匹配域名本身及其子域名，不扩大到父域。将这条 REJECT 放到需要优先拦截的位置；自动 DNS 在缓存前返回 REFUSED，有域名信息的业务连接也受同一规则约束。无需再复制成 `nameserver-policy`。固定入站出口和非 Rule 模式的优先级仍保留。

## SmartDNS 旧接法迁移

现有 SmartDNS 可以继续监听 6053 / 6553，通过 mixed / SOCKS / HTTP 访问上游解析器；见 [普通代理转发模板](smartdns-dns-proxy.conf)。当前这只是普通代理转发，Mihomo 按解析器连接目标和原规则选择出口，不再根据每个 DNS Question 的域名选路，也不会使用内置 DNS 的候选缓存或测速。

需要继续复用业务域名规则选择 DNS 上游和出口时，迁移到本页的内置 DNS 方案，让客户端直接使用 `dns.listen`；或者让保留的转发器把查询发到该监听，此时 Mihomo 看到的来源是转发器。SmartDNS 命中自己的缓存时没有新请求进入 Mihomo，其缓存由 SmartDNS 管理。

## 报文、面板与重载

内置 DNS 的普通 A、AAAA、HTTPS / SVCB、TXT、MX、合法 EDNS 和未知合法记录类型均可按 QNAME 选路，不验证 DNSSEC 签名。自动解析使用单问题 QUERY，校验完整报文、记录计数、OPT、响应来源、ID 和 Question。内置自动 resolver 对多问题、动态更新、区域传送等不支持的查询明确报错。

内置 UDP→TCP 截断重试保持同一计划。已删除的普通入站分类器不再设置 DNS 首帧等待、专用 TCP 会话上限或外部 DNS 交换上限；原生解析器自己的有界工作额度仍保留，见[性能说明](dns-performance.md)。

原生加密及非 53 上游使用按实际出口隔离的协议客户端。每个配置上游的连接池最多保留 **64 个作用域**；这不是 64 条 socket 的承诺。键包含叶子与组的身份、出口约束和路由世代，**不包含 QNAME 或来源端口**，因此同一出口的不同查询可以复用 HTTP/2、HTTP/3、DoQ 等连接。切换叶子不会复用旧叶子的连接；只回收空闲作用域，全部在用时等待空位或超时。重载会取消和关闭旧池。普通 53 路径继续使用原来的逐交换传输。

连接池复用与回答缓存隔离是两件事：不同来源可以共享同一条安全的解析器连接，仍各自按规则选路并使用自己的缓存和逻辑查询记录。

面板将共享原生传输分成两层：逻辑查询显示本次 QNAME、来源、入站、命中规则与策略链，计数展示 DNS 负载；底层长连接标记 `DNS-TRANSPORT`，显示真实解析器和实际端口，不用首个业务域名冒充后续查询。真实线路字节由底层统计一次，逻辑计数不再叠加到全局流量。

关闭一条这样的逻辑查询会中断该交换，不关闭同一 HTTP/2 或 QUIC 连接上的其他查询，也不触发该查询的自动重试或后台刷新。正常完成后逻辑记录离开活动列表，共享传输可以继续存在。经普通代理转发的 DNS 显示为普通连接，不再生成逐查询 QNAME 记录。普通 SSH 的反向映射或嗅探显示不在本功能修改范围内。

更改 `dns-rule-routing` 应重新加载完整配置。`GET /configs` 返回生效值，`PATCH /configs` 不接受该字段；可通过原 `PUT /configs` 完整重载。省略开关等同于关闭。旧 `dns-proxy-port` 应删除，不再新增专用 DNS listener。

实现与核验见 [设计约定](dns-rule-routing-design.md)、[验证记录](dns-rule-routing-validation.md)；构建和更新见 [上游同步说明](upstream-sync.md)。本地运行 `bash scripts/ci-check.sh test`，完整二进制检查使用 `scripts/test-dns-proxy.py`；最终结果以对应提交的 Actions 为准。
