# DNS 按查询域名分流：设计与验收约定

## 目标和配置边界

顶层 `dns-rule-routing` 默认关闭，开启后复用已有业务 `rules` / `rule-providers`。内置 DNS 需要真实解析时取得一次 QNAME 路由计划，以实际叶子选择 `direct-nameserver` 或 `nameserver`；hosts 与 FakeIP 先按原有顺序处理。原生自动上游包括配置的 UDP / TCP 任意端口、DoT、DoH（HTTP/2 / HTTP/3）和 DoQ；普通代理入站不再自动识别或接管目标 53 的 DNS。无需 SmartDNS、独立 DNS 域名策略表或专用监听类型。

用户配置和示例见 [使用说明](dns-proxy.md)、[原生 DNS 示例](dns-proxy.example.yaml)。本功能只提供文档描述的选路、IP 优选和缓存控制，不声称完整实现 SmartDNS 的配置、插件或所有行为。

## 1. 入口和优先级

| 场景 | 行为 |
| --- | --- |
| 开关关闭 | 原版 DNS 与普通转发行为 |
| hosts / FakeIP 本地回答 | 优先返回，不创建真实 DNS 路由计划；FakeIP 过滤后和其他未本地处理的查询继续进入真实解析 |
| Global / Direct 模式 | 不创建新增 QNAME 计划；原 DNSDialer / respect-rules 行为保留 |
| 普通代理入站，包括固定 `proxy:` | 仍按普通连接规则或固定出站转发，不检查载荷中的 DNS Question |
| 内置解析来源带 SpecialProxy | 新增选池旁路，保留旧固定出站处理 |
| 仅指定入站 `rule:` | 保留 SpecialRules 作为规则入口 |
| 已决定业务实际叶子的内部 lookup | 沿用该叶子，可据此选池，不重新匹配业务规则 |
| 节点或解析器域名 bootstrap | 独立基础解析，禁止继承业务 QNAME 计划 |

普通代理入站的 TCP / UDP 53 与其他端口一样，使用原域名映射、可选协议嗅探、规则匹配和转发流程；没有专用 DNS 首帧等待或逐查询路由入口。`dns.listen` 与显式 TUN DNS 劫持继续使用内置 DNS。

## 2. 匹配信息

域名取自 DNS Question。真实 IN / SRC / PROCESS / UID、入站规则入口、DNS 网络和目标端口照原规则顺序参与，域名规则没有强制优先权。真实 DNS 客户端保留 TCP / UDP；无客户端 DNS 传输的内部 lookup 使用首个可自动路由上游的网络建立逻辑查询。进程查找使用原来源 socket 的网络和地址，不能把网站 TCP:443 当成 DNS 目标条件。监听端口记录为 IN-PORT；原生查询供规则判断的 DST-PORT 固定为逻辑 53，实际 DoH 443、DoT 853 或自定义端口只用于传输。

SRC-IP 是入口实际取得的来源。客户端直接访问 DNS 监听或入口保留了 TUN / 代理来源元数据时，设备来源可参与匹配；经过 DNS 转发器重新查询或 NAT 汇聚后，只能匹配当前可见的转发器来源，不能还原原设备或应用。前置 SRC-IP 规则可先于域名规则命中。不同可见来源及端口的回答缓存隔离，但缓存隔离不能补回入口已丢失的身份。

网站 DstIP / GEOIP / ASN 尚未知，解析器 IP 只用于实际传输。禁止为了判定这类规则再次解析同一 QNAME。未知值通过 NOT / AND / OR、classical provider 和子规则传播，防止 NOT(IP-CIDR) 因缺失地址而误命中；来源 IP 等可判定属性正常参与。

域名叶子和 provider 继续用上游算法，共享原 match 外层控制流。PASS、PASS-RULE、禁用规则、命中计数、缺失出站及兜底遵循原语义；PASS 先于 UDP 能力检查。选中不支持所需 UDP 的出口时失败，不跳规则或退 DIRECT。DNS / REMATCH 回流型出口明确失败，避免递归。

## 3. 原生 DNS 的一次计划

### 内置 DNS

在 hosts / FakeIP 未直接回答时进入真实 resolver，在真实答案缓存读取和 singleflight 合并之前创建计划，解开策略组得到实际叶子。计划及组级 UDP 能力冻结于本次逻辑请求。

| 实际叶子 | 选择的客户端集合 |
| --- | --- |
| DIRECT / Compatible 且 direct 非空 | direct；fallback 必须为空 |
| 代理叶子 | main，加上此查询类型实际允许的 fallback |
| direct 为空 | 保留原 main / fallback |
| REJECT / DROP 或自动计划错误 | 以 main / 实际 fallback 的显式例外决定原有兼容处理 |

`Resolver.direct` 引用现有 DirectResolver 的上游客户端，主 resolver 不递归调用 DirectResolver.ExchangeContext。原 DirectResolver 仍供已选直连出站解析目标。类型依据是实际适配器，不是组名或显示名称。

先确定池，再判断所选集合是否含自动或显式传输。未选池不能禁用另一池的自动路由，也不能削弱其拒绝动作。所选集合内 `#Group` / `#DIRECT` / `#interface`、显式适配器和未接入的特殊解析器保留原 transport；`#RULES` 及未固定出口的原生明文、加密客户端使用冻结计划，配置非 53 端口不构成例外。所有候选均非自动时跳过新增选池。纯自动分支的 REJECT 不覆盖用户显式传输的原豁免；混合所选集合中的显式分支仍可按原方式回答。

直连查询失败不得改用代理 main / fallback。代理查询保留 fallback-lazy-query 和 IP 过滤行为。同一逻辑查询的重试、并发 main / fallback、UDP→TCP 均不得再次推进选组；非 IP 查询不纳入原本不会使用的 fallback。

### 普通代理转发与显式劫持

通过普通代理访问解析器时，目标地址来自客户端代理请求，Mihomo 不读取其中的 DNS Question，不进行逐包 / 逐帧 QNAME 路由，也不改投原生 direct / main 池。该连接与其他普通代理流量使用相同的解析、匹配和转发行为。

显式 TUN DNS 劫持保持本地 DNS 服务身份，进入内置解析器，不能把虚拟 TUN DNS 地址当作公网解析器发送。取消普通入站自动识别不影响这一显式入口。

## 4. DNS 设置与基础解析

开启时停用 nameserver-policy、direct-nameserver-follow-policy、fallback-filter.domain / geosite，也不加载这些停用字段专属 provider / geosite。Global / Direct 不恢复这些字段；关闭开关并完整重载后恢复原配置。

保留 nameserver、direct-nameserver、实际 main 路径的 fallback / IP 过滤、hosts、系统 hosts、缓存及回答类型控制。自动原生交换用 QNAME 计划代替按解析器地址执行的 respect-rules / #RULES 二次选择。基础 TLS / HTTP / QUIC 客户端保留解析器的服务器名、证书验证和协议设置，不把业务 QNAME 用作实际拨号目标或 TLS 名称。

proxy-server-nameserver 及其 policy 保持节点解析作用。未配置时使用独立 default bootstrap，不能退回正在建立的业务代理。FakeIP 可与内置 DNS 自动路由同时开启，配置层不关闭任一功能，也不改变 fake-ip-filter 的黑名单、白名单或规则模式。normal 与 redir-host 都可使用原生优化，区别在于是否保留真实 IP 的历史域名映射。

FakeIP 直接合成 A / AAAA 时使用 fake-ip-ttl，不进入真实答案缓存、TTL 调整、IP 测速、双栈优选或预取。应使用虚拟地址却没有有效 FakeIPv6 池的 AAAA，以及未被过滤的 HTTPS / SVCB，仍本地返回空答案；其他未本地处理的类型继续真实解析。真实查询才执行 DNS 阶段 REJECT / DROP。FakeIP 后续业务连接恢复域名并正常匹配规则；内部拨号解析直接使用真实 resolver，不返回虚拟地址，已选业务出站与 bootstrap 隔离保持原约定。

## 5. 可选 IP 优选

从 `1.0.13` 起，省略 `speed-check-mode` 或设为 `auto` / `[auto]` 时采用自动策略；写了该字段但为空、`null`、`""` 或 `[]` 则关闭，不再接受 `none`。顶层 `dns-rule-routing` 默认仍为 false。测速只对已确定的 DIRECT / Compatible、全自动上游集合中的 A / AAAA 查询启用；代理、显式传输及签名保护场景跳过。加密协议和配置非 53 端口不单独禁止测速，实际代理叶子不发本地目标地址探测。

`auto` 独占自动策略，按 443、80、ping 的顺序调度；前两项在实际探测时优先使用真实 SYN RTT，发包前明确不具备平台或原始套接字能力 / 权限才选择普通 connect。真实发包后的超时、网络错误、取消，以及资源不足、接口 / mark 设置失败，不产生第二轮 connect。选择沿用同一个期限、并发名额与出口。已有显式 TCP connect、TCP SYN 指定端口与 ICMP 列表保持原含义。

探测使用已选直连适配器的接口 / mark；数字 IP 避免递归解析，普通 TCP connect 强制实际握手，不把 TFO 的延迟建连当成低延迟。多种测法约每 100 ms 推进，允许有界重叠。总预算包含上游回答收集与探测；缺省 1000 ms，允许 1–5000 ms，0 表示缺省。并发缺省 16、最大 256，每轮去重候选最多 256。

选择最快地址所属的完整 CNAME 路径，汇集相同路径多个上游的近最快终点地址，按 RTT 排序并去重；`max-reply-ip-num` 优选时默认 8，显式设置也限制普通 A / AAAA 与静态 hosts 答复。不同链不混合，重复地址及链 TTL 取保守值并从最早贡献来源接收时间老化。DO / CD 查询及 AD / RRSIG / SIG / TSIG 等保护回答不改写；查询 AD 能力位不单独禁止未签名答案优化。全部探测失败时，允许采用同一路径下多个独立有效上游重复返回的地址兜底；无共识则保留首个有效回答顺序并执行数量上限。没有有效回答时报告真实解析失败。测速不保证应用吞吐提升。

### 双栈比较

`dualstack-ip-selection` 默认 false，仅在上述 DIRECT 测速已启用时生效；阈值默认 10 ms，范围 0–1000 ms，可显式设 0。`dualstack-ip-allow-force-AAAA` 默认 false，此时仅 AAAA 查询辅助查询 A；显式开启后，A 也可辅助查询 AAAA。

优先复用同来源、实际出口和池中有效的另一族缓存与测量；缺失时前台才发起辅助查询，使用同一冻结叶子、上游池、总超时和探测并发限额，不递归调用 resolver、不重新选组。后台刷新不额外补查另一族。两族成功的正常答案分别缓存，辅助结果不覆盖另一族已有的新鲜答案或正在完成的答复。两族均有有效测速且另一族快到阈值时，可对请求族暂时返回 NOERROR / NODATA；请求族所有有效候选有真实超时或不可达证据、另一族成功时也可偏向可达族。无地址、签名保护、取消、资源不足、未发起探测、权限和未知错误不构成网络不可达证据。代理不做辅助查询。

合成 NODATA 的 SOA TTL / MINIMUM 均为 0，只用于当次客户端答复，不缓存，也不删除或覆盖正常正向答案。缓存中的有界测速摘要绑定该答案身份和过期点；过期、替换、清理或作用域变化后，旧比较证据不再参与选择。

## 6. CNAME 和 TTL 回答策略

`force-no-CNAME` 默认 false；四个 `rr-ttl*` 字段默认 0。`rr-ttl` 非零时固定 TTL 并优先于 min/max；`rr-ttl-reply-max` 仅在答复输出时限制客户端 TTL，不改变内部缓存寿命。只压平同一回答中完整、无歧义的未签名 IN A / AAAA 链，将现有终点地址归属到查询名，TTL 取调整后链中最小值；不补造地址或发起新的 CNAME 解析。显式 CNAME 查询、DNAME、断链、循环、冲突和不相关 Answer 记录不压平。

DO / CD 查询、AD / RRSIG / SIG / TSIG 保护、截断或不匹配回答跳过整个回答调整。OPT 的 TTL 字段是标志位，不作为寿命修改；TTL 最大值也限制 SOA MINIMUM，零 TTL 始终为零。只在新回答写缓存前调整一次，不对缓存命中或过期回答重复套用最小 TTL，不继承到节点 / 解析器 bootstrap。普通代理转发的 DNS 不经过此回答调整。

这组设置独立于 DIRECT 测速：启用后也可作用于代理、显式上游或非 Rule 模式的内置业务回答。无自动路由计划时仍将完整查询内容加入缓存 / singleflight 键，避免受保护查询复用此前已改写的普通回答；不验证 DNSSEC 签名。

`force-AAAA-SOA`、`force-qtype-SOA` 默认关闭，显式启用时返回本地 NOERROR/NODATA 与 SOA，基础解析器不继承。AAAA 强制策略在 hosts 前，qtype 策略在静态 hosts 后、FakeIP 前；真实 resolver 内仍先处理路由拒绝，再在缓存前处理本地类型策略。以上带记录类型的字段及 `dualstack-ip-allow-force-AAAA` 严格采用 SmartDNS 大小写，不接受小写别名。

真实客户端目标 IP 在入站时保留；DIRECT 使用该地址，域名型与 FakeIP 请求才解析。域名解析获得多个优选地址时，DIRECT 使用有序 TCP 尝试和 UDP 首选地址，保留有界回退；节点 URL 测试不受影响。内部 DNS 的元数据转换清除业务固定 IP，避免上游或探针连到客户端目标。

## 7. 缓存和后台任务

作用域包含 Question / EDNS 等查询内容（事务 ID 除外）、池和上游集合、实际组 / 叶子身份、来源、进程、认证、入站、子规则和固定出站。每次先用真实来源端口执行当前规则并检查动作，不能用旧成功回答绕过 REJECT / DROP。仅成功且全部自动的路由允许同作用域的回答跨临时源端口复用；在途 singleflight 仍按来源端口隔离，保留独立取消语义。物理连接池合并同叶子不会放宽这些正式答案缓存作用域。

`serve-expired` 缺省 true；`serve-expired-ttl` 以秒计，0 不限制过期年龄；`serve-expired-reply-ttl` 缺省 1 秒，可设 0。过期回答 TTL 不能延长原记录的实际过期点。允许过期回答时触发有界后台更新；关闭或超过允许年龄时前台重新查询。

`prefetch-domain` 缺省 false。开启后，仅实际 DIRECT / Compatible 的自动上游池和有效真实 DNS 来源可以激活定时预取；首次成功缓存即可，不再要求一分钟三次或最近一分钟仍有请求。新鲜缓存统一按当前剩余 TTL 的 50% 安排、最小一秒，不提供额外的预取模式选择器。

`serve-expired-prefetch-time` 仅控制已过期且仍可使用候选的定时重查。间隔 `P` 为显式正整数秒数；0 / 省略时取 `serve-expired-ttl / 2`，自动推导上限 8 小时、下限一秒，过期年龄不限时为 8 小时。过期候选首次期限按原到期点加 `P`，该期限已过时最快一秒后尝试；新鲜成功回答恢复 50% 预取。已有到期任务、失败退避和客户端按需刷新不会因 `P` 延后。

前台命中补充有限热度且不延后已有期限。没有新需求时最多两次定时尝试，失败也消耗次数；热度耗尽后不会因过期重查间隔无限续期。每 resolver 使用 16 MiB 候选记账预算，全局共用 16 个活动刷新任务；详细计时与资源边界见[缓存与预取](dns-proxy.md#缓存预取与过期回答)。

客户端命中过期缓存后的按需刷新立即提交、优先并使用有限队列，不等待过期重查间隔，仍保留代理等原本允许的路径，不受 DIRECT 定时预取限制。两种后台刷新都不额外发起另一族 DNS 查询。

后台是新的逻辑请求，重新准备计划和拒绝检查，不能永远使用旧组 / 规则。其来源和 PROCESS 已知/未知结果从原请求快照继承，不重新查已可能被复用的旧 source socket。后台本身不累计热度，新作用域需真实需求重新激活。

收到有效 NOERROR / NXDOMAIN 的新上游回答且有效 TTL 为零时，删除同作用域旧缓存；双栈选择临时合成的客户端 SOA 不进入这条写回路径。删除与写入同受 generation 检查，旧世代的零 TTL 结果不能删除新世代条目；失去缓存条目的热点也须移除。`cache-algorithm` / `cache-max-size` 仍控制各 resolver 的原有缓存，并非预分配全部容量。

清缓存、重载、模式 / 开关 epoch 变化取消旧后台工作；generation 同时隔离 singleflight 和写回，旧查询不能重新填入已清缓存。前台也不能加入会被调度器取消的后台 singleflight。退出时关闭调度器与任务。

## 8. 协议、连接复用和可观测性

内置 DNS 自动范围是单问题普通 QUERY，原生协议与端口范围见本文开头。严格校验报文消费、记录计数、OPT、响应来源 / ID / Question；普通记录类型、EDNS 和 DNSSEC 数据不靠窄类型白名单排除，但不验证签名。区域传送、更新、多问题不在自动范围。

普通入站已删除 DNS 首帧探测、逐帧交换和相应的外部连接 / 交换限额。原生 DNS 的未命中工作、缓存、探测和后台刷新仍按各自边界管理，不将测速名额满视为 DNS 查询失败。

原生加密及非 53 上游为每个配置客户端维护最多 64 个传输作用域，不等于 64 条 socket。`NativeTransportKey` 仅用于该连接池：普通已确定叶子使用实际适配器对象身份，去除选组路径，保留出口参数、冻结的 UDP 约束、查询时有效接口 / routing mark 和路由 epoch，不含 QNAME、来源 IP 或来源端口。不同规则 / 组选择同一实际叶子可复用 DoT、HTTP/2、HTTP/3、DoQ 持久连接；节点名称不能证明身份相同，同名替换对象不得继承旧连接。每个上游客户端继续隔离地址、端口、协议和 TLS 设置。

含 `dialer-proxy` 或不透明 Relay / 未展开组的路径保守使用原完整路由作用域，不扩大其复用范围。全局接口 / mark 进入查询时的 key，不构成连接重建或接口自动发现的事务快照，底层仍遵循原拨号生命周期。正式答案缓存、singleflight 与测速沿用原 `TransportKey` / `CacheKey`，不跟随物理连接身份放宽。仅淘汰空闲作用域；全部在用时等待空位或请求超时，不能关闭仍承载兄弟查询的连接。重载关闭旧池。

这条原生共享传输路径将 tracker 分为每次逻辑查询和实际连接两层。逻辑层记录当前 QNAME、真实来源、入站、规则、策略链和 DNS 负载计数，不带 `dns: true`；物理层的入站标签为 DNS-TRANSPORT，记录真实解析器及配置端口，全局流量只累加真实线路字节一次。只有实际解析器上游使用专用 DNS tracker，在加入连接管理器前设置 `dns: true`；面板据此显示 DNS 类型和最终节点，隐藏匹配规则。API 保留各自的网络类型及完整节点链，逻辑记录保留查询来源与规则，物理连接不继承业务查询身份。普通代理流量不会仅因目标端口 53 被标记为 DNS。逻辑查询完成后移出活动列表，底层共享连接可继续存在。面板手动关闭逻辑查询只取消该查询，不关闭 HTTP/2 / QUIC 兄弟流，且禁止被该查询的重试或后台刷新重新发起。

普通原生 TCP / UDP 53 也在实际出口稳定时共享传输，单个查询取消只取消自身；需要独占的事务保留独立交换。连接上报使用另建的 `DNS-TRANSPORT` 元数据，只记录真实解析器 IP / 端口和上游网络，不再克隆客户端来源、QNAME、认证、入站、进程或业务命中规则。DNS 观测中的查询来源独立保留。通过普通代理的 DNS 只展示普通连接，不再生成逐查询记录。回答缓存按来源隔离与物理连接复用互不冲突。完整连接资源边界见 [DNS 性能说明](dns-performance.md)。

## 9. 最小验收矩阵

| 维度 | 必须确认 |
| --- | --- |
| 优先级 | flag × Rule/Global/Direct × fixed proxy / SpecialRules；关闭恢复原流程；hosts / FakeIP 本地应答优先 |
| FakeIP 组合 | A / AAAA 合成与反向映射、缺少 IPv6 池空答、三种 filter 模式的真实查询、CNAME/HTTPS/SVCB/TXT/MX、DNS REJECT 时机、真实缓存/测速/预取及 TTL 隔离 |
| 首次选池 | QNAME→DIRECT/Compatible/direct；QNAME→代理/main；名字不能代替类型；direct 空沿用 main |
| 冻结 | TC、main/fallback、重试及并发单次选组；直连失败不串池；切组分离缓存 |
| 规则与来源 | 原域名算法、真实 IN/SRC/PROCESS、来源规则优先级、转发器来源不冒充原设备、未知目标 IP 逻辑、PASS/禁用/命中计数 |
| 原生协议 | UDP/TCP 自定义端口、真实 TLS/HTTP2/HTTP3/DoQ、证书不可信拒绝；逻辑 53 与实际端口分离 |
| 传输例外 | selected pool 显式保留；未选池不削弱 REJECT；bootstrap 无递归；Global / fixed 沿旧优先级 |
| 测速 | 缺省 auto 与显式空值关闭、显式模式保留、真实 SYN / TCP / ICMP、能力回退与发包后不重试、多回答与 CNAME、DNSSEC 跳过、全失败、接口/mark/TFO、预算与并发上限 |
| 双栈 | 默认保留 A、显式双向比较、阈值边界、同叶子与共享预算、代理无辅助查询、另一族失败不误过滤、零 TTL NODATA |
| 回答调整 | 完整 CNAME 链与异常链、DNSSEC/OPT/零 TTL 保护、负缓存上限、缓存命中不重复抬高 TTL、显式/Global 完整查询隔离 |
| 缓存 | 不同来源/叶子隔离、过期上限/返回 TTL、热点条件、快照进程、重新匹配规则、零 TTL 删除旧正向、clear/mode/reload/close 取消及防旧世代写入/删除 |
| 连接池与面板 | 同实际叶子跨规则/组/QNAME/来源复用，正式答案组作用域不放宽；同名新对象隔离；UDP/接口/mark/epoch 边界；动态链保守隔离；64 作用域和活动保护；跨组取消单流不伤兄弟；逻辑手动关闭不重试；显式 DNS 标记、最终节点显示与完整 API；真实字节只计一次；重载关闭 |
| 普通入口与显式劫持 | mixed / SOCKS / HTTP 的 DNS 载荷按普通连接转发，不按 QNAME 选路；普通非 DNS 不变；dns.listen 与显式 TUN 劫持保留上下文及原生选池 |
| 交付 | Go 测试及 race、配置 -t、真实二进制、amd64/arm64 构建、精确提交 Actions |

验证事实与平台限制单独记录在 [实施与复查记录](dns-rule-routing-validation.md)。
