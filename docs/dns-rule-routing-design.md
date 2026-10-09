# DNS 按查询域名分流：设计与验收约定

## 目标和配置边界

顶层 `dns-rule-routing` 默认关闭，开启后复用已有业务 `rules` / `rule-providers`。内置 DNS 首次收到业务查询时取得一次 QNAME 路由计划，以实际叶子选择 `direct-nameserver` 或 `nameserver`。公共代理入站的普通 53 DNS 分类继续保留。无需 SmartDNS、独立 DNS 域名策略表、专用监听类型或 `dns-proxy-port`。

用户配置和示例见 [使用说明](dns-proxy.md)、[原生 DNS 示例](dns-proxy.example.yaml)。本功能只提供文档描述的选路、IP 优选和缓存控制，不声称完整实现 SmartDNS 的配置、插件或所有行为。

## 1. 入口和优先级

| 场景 | 行为 |
| --- | --- |
| 开关关闭 | 原版 DNS 与普通转发行为 |
| Global / Direct 模式 | 不创建新增 QNAME 计划；原 DNSDialer / respect-rules 行为保留 |
| 公共入站固定 `proxy:` | 不接管该连接的 QNAME 路由，仍服从固定出站 |
| 内置解析来源带 SpecialProxy | 新增选池旁路，保留旧固定出站处理 |
| 仅指定入站 `rule:` | 保留 SpecialRules 作为规则入口 |
| 已决定业务实际叶子的内部 lookup | 沿用该叶子，可据此选池，不重新匹配业务规则 |
| 节点或解析器域名 bootstrap | 独立基础解析，禁止继承业务 QNAME 计划 |

普通网页、SSH 和非 53 流量不会被此开关丢弃。公共分类位于反向域名映射、普通嗅探及 UDP NAT 固定出口之前，协议入口解包后共用，不单独实现每种代理握手。

## 2. 匹配信息

域名取自 DNS Question。真实 IN / SRC / PROCESS / UID、入站规则入口、DNS 网络和目标端口照原规则顺序参与，域名规则没有强制优先权。真实 DNS 客户端保留 TCP / UDP；内部 lookup 的 DNS 网络使用逻辑查询的上游协议。进程查找使用原来源 socket 的网络和地址，不能把网站 TCP:443 当成 DNS 目标条件。监听端口记录为 IN-PORT，普通 DNS 目标端口为 53。

网站 DstIP / GEOIP / ASN 尚未知，解析器 IP 只用于实际传输。禁止为了判定这类规则再次解析同一 QNAME。未知值通过 NOT / AND / OR、classical provider 和子规则传播，防止 NOT(IP-CIDR) 因缺失地址而误命中；来源 IP 等可判定属性正常参与。

域名叶子和 provider 继续用上游算法，共享原 match 外层控制流。PASS、PASS-RULE、禁用规则、命中计数、缺失出站及兜底遵循原语义；PASS 先于 UDP 能力检查。选中不支持所需 UDP 的出口时失败，不跳规则或退 DIRECT。DNS / REMATCH 回流型出口明确失败，避免递归。

## 3. 一次计划与两种目的地址

### 内置 DNS

在缓存读取和 singleflight 合并之前创建计划，解开策略组得到实际叶子。计划及组级 UDP 能力冻结于本次逻辑请求。

| 实际叶子 | 选择的客户端集合 |
| --- | --- |
| DIRECT / Compatible 且 direct 非空 | direct；fallback 必须为空 |
| 代理叶子 | main，加上此查询类型实际允许的 fallback |
| direct 为空 | 保留原 main / fallback |
| REJECT / DROP 或自动计划错误 | 以 main / 实际 fallback 的显式例外决定原有兼容处理 |

`Resolver.direct` 引用现有 DirectResolver 的上游客户端，主 resolver 不递归调用 DirectResolver.ExchangeContext。原 DirectResolver 仍供已选直连出站解析目标。类型依据是实际适配器，不是组名或显示名称。

先确定池，再判断所选集合是否含自动 53 或显式传输。未选池不能禁用另一池的自动路由，也不能削弱其拒绝动作。所选集合内 `#Group` / `#DIRECT` / `#interface`、显式适配器、非 53 和加密 DNS 保留原 transport；自动客户端用冻结计划。所有候选均非自动时跳过新增选池。为纯自动分支定义的 REJECT 不能擅自覆盖用户显式传输的原豁免。

直连查询失败不得改用代理 main / fallback。代理查询保留 fallback-lazy-query 和 IP 过滤行为。同一逻辑查询的重试、并发 main / fallback、UDP→TCP 均不得再次推进选组；非 IP 查询不纳入原本不会使用的 fallback。

### 公共入站过境 DNS

目的解析器来自客户端请求，选路只决定出口，不能改写为 native direct / main 池，也不能把 QNAME 写入实际拨号 Host。原解析器 Host 如需解析，只能通过独立 bootstrap 获得原解析器 IP:53。

UDP 每包独立选路；TCP 每长度帧独立选路。公共路径不插入内置 hosts 或生成 FakeIP。TUN DNS 劫持则保持本地 DNS 服务身份，不能把虚拟 TUN DNS 地址当作公网解析器发送。

## 4. DNS 设置与基础解析

开启时停用 nameserver-policy、direct-nameserver-follow-policy、fallback-filter.domain / geosite，也不加载这些停用字段专属 provider / geosite。Global / Direct 不恢复这些字段；关闭开关并完整重载后恢复原配置。

保留 nameserver、direct-nameserver、实际 main 路径的 fallback / IP 过滤、hosts、系统 hosts、缓存及回答类型控制。自动普通 53 交换用 QNAME 计划代替按解析器地址执行的 respect-rules / #RULES 二次选择。

proxy-server-nameserver 及其 policy 保持节点解析作用。未配置时使用独立 default bootstrap，不能退回正在建立的业务代理；即使关闭内置监听，公共分类所需基础解析仍可独立运行。FakeIP 与启用的内置 DNS 自动路由配置互斥。

## 5. 可选 IP 优选

`speed-check-mode` 默认空 / none。只对已确定的 DIRECT / Compatible、自动普通 53 上游集合中的 A / AAAA 查询启用；代理、显式 / 非 53 传输以及签名保护场景跳过。

模式支持 TCP 指定端口与 ICMP，按配置顺序尝试。探测使用已选直连适配器的接口 / mark；数字 IP 避免递归解析，TCP 强制实际握手，不把 TFO 的延迟建连当成低延迟。总预算包含上游回答收集与探测；缺省 1000 ms，允许 1–5000 ms，0 表示缺省。并发缺省 16、最大 256，每轮去重候选最多 256。

保留候选所属回答和 CNAME 链，仅收窄同一未签名地址 RRset，不拼接多个回答。DO / CD 查询及 AD / RRSIG / SIG / TSIG 等保护回答不改写。全部探测失败而已有有效回答时返回原回答；没有有效回答时报告真实解析失败。测速不保证应用吞吐提升。

## 6. 缓存和后台任务

作用域包含 Question / EDNS 等查询内容（事务 ID 除外）、池和上游集合、实际组 / 叶子身份、来源含端口、子规则和固定出站。先检查当前规则动作，再命中缓存，不能用旧成功回答绕过 REJECT / DROP。来源端口隔离可能降低跨 socket 命中率。

`serve-expired` 缺省 true；`serve-expired-ttl` 以秒计，0 不限制过期年龄；`serve-expired-reply-ttl` 缺省 1 秒，可设 0。过期回答 TTL 不能延长原记录的实际过期点。允许过期回答时触发有界后台更新；关闭或超过允许年龄时前台重新查询。

`prefetch-domain` 缺省 false。仅真实 DNS 入站且来源有效的前台请求累计热度；每个作用域 1 分钟至少 3 次、最近 1 分钟仍有真实请求，按剩余 TTL 约 80% 安排刷新。每 resolver 最多 1024 个热点，全局 16 个后台任务，无无限队列。

后台是新的逻辑请求，重新准备计划和拒绝检查，不能永远使用旧组 / 规则。其来源和 PROCESS 已知/未知结果从原请求快照继承，不重新查已可能被复用的旧 source socket。后台本身不累计热度，新作用域需真实需求重新激活。

清缓存、重载、模式 / 开关 epoch 变化取消旧后台工作；generation 同时隔离 singleflight 和写回，旧查询不能重新填入已清缓存。前台也不能加入会被调度器取消的后台 singleflight。退出时关闭调度器与任务。

## 7. 协议、资源和可观测性

范围是单问题普通 QUERY、明文 TCP / UDP 53。严格校验报文消费、记录计数、OPT、响应来源 / ID / Question；普通记录类型、EDNS 和 DNSSEC 数据不靠窄类型白名单排除，但不验证签名。区域传送、更新、多问题不在自动范围。

TCP 首帧探测可处理 65535 字节，未识别时保留全部字节并清 deadline 回原流程；确认后逐帧处理，畸形帧不得变为任意透传。公共上限为 128 个已识别候选 TCP 会话、256 个并发交换；探测 / 单次交换 5 秒，已接管 TCP 空闲 60 秒。模式或开关变化在下一帧重新检查。

真实上游交换用原生 tracker，展示 QNAME、真实解析器 IP:53、来源、规则、完整策略组链和字节计数。面板关闭中断交换，完成后移出活动列表，过境流量不重复统计。普通 SSH 的反向域名显示不在本轮修改范围。

## 8. 最小验收矩阵

| 维度 | 必须确认 |
| --- | --- |
| 优先级 | flag × Rule/Global/Direct × fixed proxy / SpecialRules；关闭恢复原流程 |
| 首次选池 | QNAME→DIRECT/Compatible/direct；QNAME→代理/main；名字不能代替类型；direct 空沿用 main |
| 冻结 | TC、main/fallback、重试及并发单次选组；直连失败不串池；切组分离缓存 |
| 规则 | 原域名算法、真实 IN/SRC/PROCESS、未知目标 IP 逻辑、PASS/禁用/命中计数 |
| 传输例外 | selected pool 显式保留；未选池不削弱 REJECT；bootstrap 无递归；非 53 原行为 |
| 测速 | 多回答与 CNAME、DNSSEC 跳过、全失败、真实 TCP/ICMP、接口/mark/TFO、预算与并发上限 |
| 缓存 | 过期上限/返回 TTL、热点条件、快照进程、重新匹配规则、clear/mode/reload/close 取消和防旧写回 |
| 公共入口 | 同 socket / TCP 多帧逐查询、普通非 DNS 不变、完整帧、原目的地址不改池、TUN 上下文 |
| 交付 | Go 测试及 race、配置 -t、真实二进制、amd64/arm64 构建、精确提交 Actions |

验证事实与平台限制单独记录在 [实施与复查记录](dns-rule-routing-validation.md)。
