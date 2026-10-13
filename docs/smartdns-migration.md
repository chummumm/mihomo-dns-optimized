# 从 SmartDNS 迁入原生 DNS

本项目复用 Mihomo 的 `rules` / `rule-providers` 决定业务域名出口。迁移后客户端直接使用 `dns.listen`，不再需要由 SmartDNS 的不同监听端口区分国内 / 海外查询。原 mixed / SOCKS / HTTP 等入站仍可用于普通代理转发，但已取消外部明文 53 DNS 的自动识别与逐查询 QNAME 分流。

以下是配置能力的对照，不表示 SmartDNS 所有实现细节或 UI 插件都被复制。

## DNS 参数对照

| SmartDNS 设置 | 原生 Mihomo 对应设置 / 行为 |
| --- | --- |
| 默认上游、独立 Global 上游组 | `direct-nameserver` 与 `nameserver`；先按 QNAME 匹配业务规则，再按实际出口类型选池 |
| `server-https` / `server-tls` / QUIC 上游 | 原生 `https://` / `tls://` / `quic://`；继承本次已冻结出口，保留 TLS 证书校验 |
| `speed-check-mode tcp:443,tcp:80,ping` | `speed-check-mode: [tcp:443, tcp:80, ping]`，仅实际 DIRECT / Compatible 进行 IP 探测 |
| `speed-check-mode tcp-syn:443,ping` | `speed-check-mode: [tcp-syn:443, ping]`；Linux 真实 SYN 探测，需要原始套接字权限 |
| 默认测速选择 | 本项目从 `1.0.13` 起，省略 `speed-check-mode` 或写 `auto` / `[auto]` 使用自动策略；这不是 SmartDNS 配置键的逐字兼容，具体默认与边界见下文 |
| `response-mode fastest-ip` | `response-mode: fastest-ip`；也支持 `first-ping`、`fastest-response`，开启测速后按所选时机回答 |
| `max-reply-ip-num 8` | `max-reply-ip-num: 8`；优选时默认 8；显式设置也限制普通 A/AAAA 和静态 hosts，范围 1–2147483647；省略时不裁剪未优选答案 |
| Global `-no-speed-check` | 代理 DNS 路径始终不做本地 IP 测速 |
| `dualstack-ip-selection yes` | `dualstack-ip-selection: true`，仍需启用测速，仅作用于自动 DIRECT 查询 |
| `dualstack-ip-selection-threshold 10` | 同名字段，毫秒，默认 10，允许 0–1000 |
| `dualstack-ip-allow-force-AAAA no` | `dualstack-ip-allow-force-AAAA: false`，默认保留 IPv4 |
| Global `-no-dualstack-selection` | 代理路径不做双栈探测，也不为了本地优选额外查询另一族 |
| `prefetch-domain yes` | `prefetch-domain: true`；仅 DIRECT，首次真实查询即可激活；统一按剩余 TTL 约 80% 提前刷新 |
| `serve-expired yes` | `serve-expired: true` |
| `serve-expired-ttl 604800` | 同名字段，秒，允许最多使用过期 7 天的回答 |
| `serve-expired-reply-ttl 0` | 同名字段，可显式设为 0；不延长原缓存过期点 |
| `cache-size 384000` | `cache-max-size: 384000`；该容量用于各 resolver 的缓存，非预分配相同数量的记录 |
| `cache-persist no` | 使用内存 DNS 缓存；无需新增持久化开关 |
| `rr-ttl 300` | `rr-ttl: 300`；固定正常回答 TTL，优先于 min/max，默认 0 不固定 |
| `rr-ttl-min 0` / `rr-ttl-max 3600` | 同名字段；0 表示不限制对应边界，单位秒 |
| `rr-ttl-reply-max 60` | `rr-ttl-reply-max: 60`；只限制客户端收到的 TTL，内部缓存寿命保留 |
| `force-AAAA-SOA yes` | `force-AAAA-SOA: true`；AAAA 返回本地 NOERROR/NODATA 与 SOA，并移除可改写 HTTPS 回答的 IPv6 提示 |
| `force-qtype-SOA 65 128-256` | `force-qtype-SOA: [65, "128-256"]`；同样支持标量、块列表和清除语法 |
| `force-no-CNAME yes` | `force-no-CNAME: true`；压平完整、无歧义、未签名的 A / AAAA 回答链 |
| `resolv-file /etc/resolv.conf` | 建议显式配置 `default-nameserver` 与 `proxy-server-nameserver`，避免把系统 DNS 再指回本机监听而循环 |

`1.0.11` 起，`force-no-CNAME`、`force-AAAA-SOA`、`force-qtype-SOA`、`dualstack-ip-allow-force-AAAA` 严格采用 SmartDNS 大小写，不接受小写别名；旧写法会明确报配置错误。仍需按 YAML 语法书写键和值，例如 SmartDNS 的 `yes/no` 改为 `true/false`，数字列表使用合法 YAML。

### 测速与双栈的边界

从 `1.0.13` 起，省略 `speed-check-mode`、`speed-check-mode: auto` 和 `speed-check-mode: [auto]` 等价。自动策略按 443、80、ping 的顺序调度，TCP 项优先使用真实 SYN 的 RTT；在发包前确认平台或原始套接字能力 / 权限不可用时，才改用普通 connect。选择在实际探测时完成，不依赖启动时权限检查和默认列表初始化的先后顺序。真实 SYN 发包后的超时、网络失败、取消，以及资源不足、接口 / mark 设置错误，均不追加另一轮 connect；预算、并发限制和 DIRECT 选路保持一致。

`auto` 是独占的自动策略，不能加端口或与其他测法混写；显式 `tcp`、`tcp-syn`、`ping` 列表仍按原来的测法执行。写了测速字段但留空，或设为 `null`、`""`、`[]` 时关闭；旧 `none` 不再接受，不能直接照搬。已开启 `dns-rule-routing` 且省略测速字段的配置，升级后会对合格的 DIRECT 自动上游池启用此策略；顶层 `dns-rule-routing` 默认仍为 `false`，自动策略不会自行启用 DNS 选路。这里明确约定本项目的默认行为，不假定 SmartDNS 各版本未配置时一定选择 SYN。

`fastest-ip` 迁移的是在候选中优选最快可达 IP 的目标。本项目保留原有可选优选总预算，默认 1000 毫秒；不会把 `first-ping` 暗中改成 SmartDNS 的 200 毫秒预算。到期仍无合格 DNS 答复时，已有 DNS 查询继续使用正常解析期限，不重发整批上游。多种测法约每 100 毫秒推进下一阶段，可重叠但共用实际探测并发上限；满额时本轮释放不再等待的旧阶段探针，再尝试后续测法，不无限排队。

普通 `tcp` 成功建连或收到连接拒绝都可测 RTT；`tcp-syn` 构造真实 TCP SYN，只接受源 / 目的地址、端口、ACK 和校验和匹配的 SYN-ACK 或 RST-ACK，不完成应用连接。裸 `tcp` / `tcp-syn` 使用 80 端口。显式 SYN 当前仅支持 Linux，其他平台显式配置会报错；缺少运行权限时按测法失败处理，不伪装成 connect。跨平台部署需要自动选取可用 TCP 测法时，可使用上面的 `auto`。

候选可来自多个已通过原版过滤的完整回答；只合并完整 CNAME 路径一致的终点地址，重复地址和链 TTL 取保守值。筛选采用 SmartDNS 当前源码的 0.1 ms 精度与近最快条件，按 RTT 排序并限制数量；全部探测失败时，同一路径多个独立有效上游重复返回的地址可作为兜底。没有这种共识时保留首个有效回答及其顺序，这是保留 Mihomo 有效答复的选择，不伪造测速结果。初次 `first-ping` 可能只有一个地址，后台完成或缓存命中才返回多个。

`first-ping` / `fastest-response` 取得后台名额并继续本轮比较时，提前发布的普通正向答案只给客户端最多 3 秒 TTL；内部缓存仍保留完整有效期。实际剩余 TTL 与 `rr-ttl-reply-max` 可以进一步降低此值，零 TTL 和受保护答案不延长。未取得后台名额、最后结果和普通缓存命中不套用提前答复规则。

双栈查询共用同一次选路、同一解析器池、一个总超时预算和共享探测并发上限。默认只允许在 AAAA 查询中辅助查询 A：优先复用同来源、实际出口及池中有效的另一族缓存和测量；缺失时才由前台补查。两族均成功测速、IPv4 快至少设定阈值时，对 AAAA 返回 NOERROR / NODATA；IPv6 全部有效候选有实际探测超时或不可达证据、IPv4 可达时也可如此。开启 `dualstack-ip-allow-force-AAAA` 后才允许反向过滤 A。

两族成功取得的正常答案分别填入正式缓存；辅助结果不覆盖另一族已有的新鲜答案或正在完成的结果。临时过滤回答带零 TTL，不覆盖正向缓存，避免短暂网络差异被 `serve-expired` 延续数天。缓存命中可利用仍有效、与该答案完全对应的两族测量继续比较；另一族过期、被替换或清除后保留当前族正常答案。后台预取和按需过期刷新只查询自身记录，不额外补查另一族。DNSSEC、解析错误、无地址、取消、资源忙、未实际发出探测、缺少权限及未知错误都不充当网络不可达证据。

原生上游用 DoH 或 DoT，并不改变是否测速的判据：**实际 DIRECT 才可能测速，实际代理永远不做本地 IP 测速**。显式 `#Group` / `#DIRECT` 等独立传输例外不参加自动优选。

### 回答改写与 TTL 的边界

`rr-ttl` 非零时优先于 min/max，在写缓存前应用一次；零 TTL 仍保持不可缓存。`rr-ttl-reply-max` 在输出答复时另行限制客户端 TTL，内部缓存不受此上限影响。过期回答使用 `serve-expired-reply-ttl`，不会重新套用 TTL 下限。节点域名和解析器 bootstrap 不继承业务回答调整。

去 CNAME 仅在同一回答中已经存在完整 A / AAAA 链时，把终点地址改写到查询名，并采用整条链的最短 TTL；不会补造地址或为隐藏 CNAME 重新解析。CNAME 类型查询、DNAME、断链、循环、歧义链及额外不相关的 Answer 记录保留原形。

对于本地 hosts 别名，没有远端 CNAME 链的 NODATA 可以隐藏本地别名；NXDOMAIN 及含有远端别名链的负回答保留完整身份。这样不会把别名目标不存在错误地变成别名本身不存在。负缓存使用 SOA 头部 TTL、SOA.MINIMUM 和 CNAME 路径 TTL 的最小值，按原接收时间扣除等待耗时。

DO / CD 查询以及 AD、RRSIG、SIG、TSIG 等受保护回答不进行地址优选、CNAME 压平或 TTL 改写。查询单独设置 AD 仅表示客户端能力，不阻止未签名答案优化；本地合成答案不会把查询 AD 原样冒充为已认证答案。这是明确的保守边界，不宣称对所有回复实现无条件的 `force-no-CNAME`。本功能传递 DNSSEC 数据，不验证 DNSSEC 签名。

专用 `force-AAAA-SOA` 会去除可安全改写的 HTTPS ServiceMode 答案中 `ipv6hint` 及其 `mandatory` 引用；其他参数保留。泛用 `force-qtype-SOA: [28]` 只控制 AAAA 问题，不隐含这项 HTTPS 处理。

类型过滤、YAML 列表与清除语法、hosts / FakeIP 优先级见[查询类型过滤](dns-proxy.md#查询类型过滤)。DIRECT 已有客户端真实 IP 时直接连接该 IP；只有域名或 FakeIP 才需要解析，避免客户端已选地址被内核重新替换。

### 缓存与预取的边界

缓存先按完整来源及端口执行规则，再按解析器池、实际出口、来源 IP / 进程 / 入站作用域和查询内容隔离。成功的自动应答可跨临时源端口复用；在途查询仍独立管理取消。迁移相同 `cache-size` 不代表两者的内存用量或命中率相同。

从 `1.0.12` 起，定时预取严格限于实际 DIRECT / Compatible 的自动上游池，只由带有效来源的真实 DNS 入站激活；首次成功缓存即可，不再要求一分钟三次访问，也不因一分钟无新访问就失去资格。真实需求补充有限热度，后台自身不补热；没有新需求时最多两次定时尝试。每次执行前重新选路，发现代理、拒绝或作用域改变后丢弃旧候选。匿名、内部、bootstrap 和 FakeIP 合成不会激活任务。

从 `1.0.13` 起只保留统一的提前刷新策略，移除 `prefetch-mode` 选择器及其附属的 `serve-expired-prefetch-time`；配置中存在这两个旧键会明确报迁移错误，应删除后通过 `prefetch-domain` 控制预取。首次成功缓存后，按剩余 TTL 的约 80% 安排刷新，最小一秒；命中缓存不推迟原定时刻。这样不会因为过期缓存保留期很长而推迟新鲜记录的预取，也无需另设一个 SmartDNS 原版没有的模式字段。SmartDNS 的 `serve-expired-prefetch-time` 不再作为本项目支持的配置迁入，不赋予它新的语义。

本项目不复制没有新请求仍无限维持冷域名的行为。预取候选使用 16 MiB / resolver 记账预算，通常约 1.2 万至 1.6 万个普通短查询，不再固定 1024 项；按到期先后调度，共用 16 个活动后台刷新槽位。按需过期刷新优先，有限排队且不阻塞前台。

**预取的 DIRECT 限制只针对定时任务。** 客户端命中过期答案后，原来的按需刷新继续对包括代理在内的正常路径生效；关闭预取不关闭该行为。`serve-expired`、缓存容量、双栈和各 TTL 设置的默认值保留，主动预取默认关闭；`1.0.13` 的测速默认改为上述 `auto`，不会连带开启预取或双栈优选。代理组 `url-test`、节点健康检查及其他代理路径的原生 `tcp-concurrent` 行为继续保留。

Mihomo 的 `serve-expired-ttl: 0` 在本项目表示不限制过期年龄，保留本项目之前的默认行为；迁移时应明确填写需要的正数上限，不能假定它等于 SmartDNS 每个版本的零值语义。

## 域名屏蔽清单

把 SmartDNS 域名清单放进单独的普通 Mihomo domain provider 文件，主配置只用一条原生规则引用。例如主配置：

```yaml
rule-providers:
  custom_block:
    type: file
    behavior: domain
    format: yaml
    path: ./rule_provider/smartdns_block.yaml

rules:
  - RULE-SET,custom_block,REJECT
  # 后面继续原有直连、代理、规则集与 MATCH。
  - MATCH,DIRECT
```

对应 `rule_provider/smartdns_block.yaml`：

```yaml
payload:
  - '+.blocked.example'
  - '+.telemetry.example'
```

两份文件维持上述相对目录，规则清单仍只有这一份。需要维护屏蔽域名时修改规则集文件，不再同步一份 SmartDNS 域名策略。

`+.` 覆盖指定域名及其子域名。把拦截规则放到应当优先的业务规则位置，它就与其他规则共同使用 Mihomo 现成的匹配算法。自动 DNS 路由在缓存前处理 REJECT，返回 REFUSED 且不查询远端；有域名信息的业务连接同样会被该规则拒绝。SmartDNS 的 `-address #` 通常以 SOA 空回答阻断，两者 DNS 返回码不同，均不返回被阻断域名的公网地址。

这些业务规则遵守既有优先级：Global / Direct 或固定入站出口不被新增自动路由改写。不要另复制到 `nameserver-policy`；开启 `dns-rule-routing` 后该字段停用。

## 运维设置

SmartDNS 的两个监听、SOCKS 回送端口和进程用户不需要照搬。原生方案使用已有 Mihomo 服务、`dns.listen`、API 和面板；SmartDNS 的 `smartdns_ui.so`、UI 账户、历史查询数据库、日志轮转参数不作为 Mihomo DNS 配置字段迁移。

旧的“SmartDNS 经 SOCKS / HTTP 查询外部解析器”接法只按解析器连接目标和普通代理规则转发，不再读取 DNS Question，也不使用内置 DNS 的缓存或 IP 优选。需要保留 SmartDNS 作为前置缓存时，可以将它的上游改为 Mihomo 的 `dns.listen`；此时 Mihomo 看到的来源是 SmartDNS。需要按原设备来源分流时，应让客户端直接访问 Mihomo DNS，或使用保留客户端元数据的显式 TUN DNS 劫持。

新安装包只包含通用起始配置，不包含个人节点或屏蔽清单。安装位置、配置保留及手动启停说明见[安装包文档](releases.md)。改用原生 DNS 前先执行 `mihomo -t`，确认标准 53 端口由计划中的服务监听，并让客户端使用该地址。

原 SmartDNS 参数语义参考其[官方配置表](https://pymumu.github.io/smartdns/en/configuration/)、[缓存与预取](https://pymumu.github.io/smartdns/config/cache/)、[测速模式](https://pymumu.github.io/smartdns/en/config/check-speed-mode/)和[双栈说明](https://pymumu.github.io/smartdns/en/config/dualstack/)。本次配置、预取及多地址行为还核对了官方源码提交 [`dfc9ef4`](https://github.com/pymumu/smartdns/tree/dfc9ef4bd947891178924c06070fbccc3ea370ba)，包括 `dns_server/context.c`、`dns_server/rules.c`、`dns_conf/qtype_soa.c`、缓存计时及默认值定义；示例注释中的返回数量 1–16 不是当前源码上限。实际实现与限制以本项目文档和测试为准。
