# DNS 按查询域名分流：完整设计与验收约定

## 1. 目标与配置

本设计取代旧的 `dns-proxy-port`。新增一个顶层布尔值，默认关闭：

```yaml
mixed-port: 7890
mode: rule
dns-rule-routing: true
```

不新增 listener type，不新增专用监听端口，不要求为每个入站配置开关，也不新增 DNS 规则列表。外部 DNS 查询的识别不依赖 `dns.enable` 或普通 sniffer 开关。SmartDNS 可继续监听 6053 / 6553，其代理上游改用现有 mixed / SOCKS / HTTP 入站。

本文件记录实施前审查确定的行为约定，并补充实现中确认的边界；实际验证结果见 [实施与复查记录](dns-rule-routing-validation.md)。示例不包含任何私人节点、账号或订阅。

## 2. 生效条件与优先级

公共转发入口在目标元数据合法化之后、反向域名映射、普通嗅探、UDP NAT 固定出口之前判断：

1. 开关关闭：沿用原流程。
2. 入站明确指定 `proxy:`：保留该出站，不运行 QNAME 分流。
3. Global / Direct 模式：保留原模式，不运行 QNAME 分流。
4. Rule 模式、没有固定出站、普通 TCP / UDP 53 查询：按每个 Query 选路。
5. 入站仅指定 `rule:`：继续自动分流，从对应子规则开始；找不到子规则时沿用原规则入口行为。
6. 非 53 目标，或者尚未识别为支持的普通 DNS：沿用原转发流程。

原版入站固定出站优先于模式。自动分类不改变这个优先级。`PROCESS-*`、`IN-*`、来源、端口和网络规则不获得额外优先权，仍按现有规则顺序匹配。

普通业务与 DNS 共享原有入站，旧专用端口的“非 53 一律丢弃”不再适用。TCP 在确认接管为 DNS 后继续按 DNS 帧处理，后续畸形帧不能转为任意字节透传。

## 3. 匹配信息与真实传输分离

用户最终选择的是“查询网站的目标 IP 尚未知”的语义：

| 属性 | 匹配语义 |
| --- | --- |
| 域名 | 当前 DNS Question 的 QNAME |
| 目标 IP / GEOIP / ASN | 网站尚未解析，未知；禁止为选路再次解析 QNAME |
| DNS 服务器 IP | 仅用于实际传输，不代替网站目标 IP |
| 来源 IP / 端口、IN-*、PROCESS-*、UID、网络、目标端口 | 保留真实 DNS 连接信息，按原规则顺序参与 |
| `SpecialRules` | 保留原入站指定的规则入口 |

保留实际入站类型和原版进程查找设置，不把外部查询统一标成 INNER。经 SmartDNS 汇聚后，进程通常只能反映 SmartDNS，不能还原原业务应用。

目标 IP 的“未知”必须贯穿 NOT / AND / OR、classical provider 和子规则；NOT(IP-CIDR) 不得因为未解析而误命中。来源 IP 及带 src 参数的规则不应被误当成未知的目标 IP。

域名叶子、规则集匹配算法及原外层规则顺序继续复用。共同外层处理 SpecialRules、PASS、禁用规则、缺失出站和兜底；DNS 仅提供必要的未知字段评估与失败策略。普通连接的匹配行为不变。

选中策略组后，按照 QNAME 选择一次实际出站，并固定到本次交换；真正拨号的 metadata 使用原解析器 IP:53。不能让组在拨号时按解析器 IP 再选择一次。

DNS 查询选择到不支持所需 UDP 的出口时失败，不向后落入其他规则或 DIRECT。PASS 控制动作先于 UDP 能力检查。当前 DNS 交换不支持 DNS / REMATCH 回流型特殊出口，明确失败以避免递归或不完整控制语义。

## 4. 查询识别与生命周期

### 4.1 UDP

每份数据报独立识别、选路、交换并校验回答。共享 SOCKS UDP association 或源 socket 不固定所有查询的出口。异步处理保留报文所有权，恰好释放一次；响应通过原入站 WriteBack 返回。

### 4.2 TCP

有界读取首个两字节长度帧，支持 DNS 的完整 65535 字节消息，而非依赖默认 4 KiB Peek。首帧不属于支持范围时，回放全部已读字节并清理探测 deadline，恢复原流程。确认 DNS 后逐帧处理，允许同一连接不同 QNAME 命中不同规则。

每次查询有限时和资源上限。模式 / 开关改变后，不能让已经接管的 TCP 长连接无限沿用旧的自动路由；关闭这些 DNS 连接或在逐帧处理时使用新策略，不影响普通业务连接。

### 4.3 协议范围

自动接管范围是普通明文 TCP / UDP、真实远端口 53、单问题 QUERY。继续复用严格 wire parser、完整记录计数、EDNS / OPT 检查、响应来源 / ID / Question 关联。

普通 A、AAAA、HTTPS / SVCB、TXT、MX、DNSSEC 数据、合法未知类型及 EDNS 扩展不靠窄类型白名单排除。区域传送、动态更新、多问题等特殊用途不作为普通查询接管。未接管的首份报文走原流程；已接管流中的错误不会变成任意透传。

DoH / DoT / DoQ、非 53 明文 DNS 不在本次自动接管范围。解析器自身是域名时只能使用独立基础解析，不以待查业务域名递归引导自己。

## 5. 两条入口共享一次选路

### 5.1 外部过境 DNS

mixed、SOCKS、HTTP CONNECT、redir、tproxy、TUN 普通流量和其他代理协议解包后的数据进入公共 TCP / UDP 钩子。实际解析器地址来自原请求；不交给内置 DNS 重写查询、插入 hosts 或生成 FakeIP。

### 5.2 内置服务与 TUN DNS 劫持

`dns.listen` 和 TUN 劫持继续是本地 DNS 服务。虚拟 TUN DNS 地址不能作为公网解析器通过代理发出。服务所需的普通 53 上游查询共享 QNAME 选路核心，保留入站来源、固定出站和 SpecialRules。

匹配时上游 DNS 目标端口为 53；本地监听 1053 等端口保留在 IN-PORT。实际 DNS 客户端保留其 TCP / UDP；普通业务触发的内部 lookup 使用本次逻辑查询首个符合范围的上游协议。进程查找仍使用原来源 socket 的网络和地址，不把原网站 TCP 来源端口当成 UDP socket 查询。

Global / Direct 模式下，内置 DNS 不建立新的自动路由计划，保留原 respect-rules / 显式上游的传输行为。例如 respect-rules 为 false 时，不因 Global 模式将原直连 DNS 强制改走 GLOBAL。开关开启所停用的业务策略仍保持停用。

本机 SmartDNS 的 6053 / 6553 不属于自动接管的远端 53 传输。访问本机服务维持本地路径；SmartDNS 后续通过代理发出的 53 查询再进入公共分类器。

## 6. 内部来源与防循环

内部上下文区分业务待选查询、规则求值所需解析、已有固定出口、bootstrap。它们不是新增用户配置。

* 普通规则为 IP 条件求值时，可以做一次 QNAME 路由后返回 IP，不能重入完整 IP 求值再次解析自身。
* DIRECT 等出站已经选定后，其本地目标解析继承该实际出站实例和接口 / mark，不重新进行业务自动选路。
* 节点域名与解析器自身域名使用独立基础 resolver，不通过尚未建立的代理引导自己。
* 显式 DNS `#Group` / `#DIRECT` / `#interface` 保留现有运输约束；`#RULES` 没有固定出口，在接管范围内使用 QNAME。
* 未配置 proxy-server-nameserver 时，开启状态不能无条件退回自动业务 main resolver；使用可独立运行的 default bootstrap。没有有效独立路径时明确失败，不隐藏递归。

内部 DoH / DoT / DoQ 的连接池不在此次扩展范围，不把逐域名选路错误地放到共享加密连接的首次拨号上。

## 7. DNS 配置的有效范围

开关开启时，以下业务域名策略停用，且不加载其专属 provider / geosite 条件：

* nameserver-policy
* direct-nameserver-follow-policy
* fallback-filter.domain
* fallback-filter.geosite

即使切到 Global / Direct，也不重新启用这些业务策略；关闭开关并重新应用配置后恢复原配置语义。

在被接管的业务明文 53 交换上，QNAME 路由替代 respect-rules / #RULES，不再按解析器地址第二次选路。

保留 nameserver、fallback、fallback-lazy-query、fallback-filter.geoip/ipcidr、direct-nameserver、proxy-server-nameserver、proxy-server-nameserver-policy、default-nameserver、hosts、系统 hosts、缓存及回答类型控制。

未固定出口的 main / fallback / 重试属于同一逻辑查询时继承同一选路结果。显式固定 DNS 出口仍是例外。FakeIP 不参与此功能；此项目示例继续使用真实回答及 redir-host。

路由准备只考虑本次查询实际会使用的候选上游。沿用原 isIPRequest 分类（IN A / AAAA / CNAME）决定是否纳入 fallback；TXT / MX / HTTPS 等只查询 main 时，不让未参与的 fallback 弱化 main 的 REJECT / REJECT-DROP。

## 8. 缓存、并发合并与可观测性

不能让不同来源 / 子规则 / 固定出口的查询按 Question 错误合并，也不能在需要应用新拒绝规则时直接使用旧成功缓存。实现须在保留原缓存能力的同时隔离路由作用域，或对自动路由分支采取明确的无缓存复用策略。

后台刷新与重试保留路由身份，不能用裸 context.Background 丢掉固定出口、来源或 bootstrap 标记。规则 / 出口改变后，不复用不再适用的自动查询结果。SmartDNS 自身缓存仍由 SmartDNS 管理。

缓存与 singleflight 的路由标识包含查询内容（不含事务 ID）、真实来源、子规则、固定出站以及冻结组 / 叶子的对象身份。不同来源端口也会隔离，以保留 SRC-PORT 规则语义；这可能减少不同 socket 之间的内置缓存复用。

每次真实上游交换使用原生 TCP / UDP tracker，显示 QNAME、真实解析器 IP:53、来源、入站、命中规则、策略组链和字节统计；关闭连接可取消交换。已结束短查询从实时列表移除。过境查询不重复计费。

普通 SSH 等非 DNS 的 IP 到域名映射不由本功能重写；不能把该功能宣传为解决所有普通流量的映射显示问题。

## 9. 实施与验收

实施分为配置/API迁移、公共入站、共享路由、内置 resolver 桥接、文档/云编译与独立复查。

必须验证：

1. flag × Rule/Global/Direct × 固定 proxy/子规则 的优先级矩阵；关闭时恢复原行为。
2. QNAME 域名规则及 provider 原算法，保留真实 IN / SRC / PROCESS / 端口 / 网络；目标 IP 与 NOT / 混合逻辑未知保护。
3. UDP 同会话与 TCP 同连接不同 QNAME 独立选择，组选择不重复推进，UDP 不支持不回落直连。
4. 非 53 和首负载非 DNS 字节不变，完整 TCP 帧、超时、资源释放及 mode/flag 热改。
5. 内置 dns.listen、TUN 劫持的桥接、固定出口和子规则上下文，无虚拟地址出站，无基础解析循环。
6. 停用配置真正失效，保留配置作用域正确，缓存/合并/后台刷新不跨路由混用。
7. 原生面板和取消统计正确，现有 SmartDNS SOCKS5 UDP/TCP、HTTP TCP 接入仍可用。
8. 原公共入站回归、Go 测试/竞态检查、完整二进制端到端交换、amd64/arm64 云编译。

提交后确认 GitHub Actions 实际针对交付 SHA 成功，发布产物和校验和可获取；上游同步沿用现有工作流并通过新的验证门槛。
