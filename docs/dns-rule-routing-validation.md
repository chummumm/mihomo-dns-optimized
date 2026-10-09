# DNS 原生选池、优化与缓存：实施与复查记录

## 本轮范围与状态

本轮在已存在的 `dns-rule-routing` 和公共 53 分类基础上，补齐内置 DNS **首次按业务 QNAME 选实际出站，再选 direct / main 池**，并增加可选直连 IP 测速、预取和过期回答控制。原公共入站继续保留客户端请求的解析器地址；无需 SmartDNS，不使用 FakeIP。

设计与边界以 [设计约定](dns-rule-routing-design.md) 为准，配置见 [使用说明](dns-proxy.md) 与 [原生 DNS 示例](dns-proxy.example.yaml)。本轮已完成实现、独立复审和下述本地验证；云构建会在精确源提交上重新执行规定门禁，成功后才发布。

## 本轮审查结论

| 检查点 | 实现约束及回归依据 |
| --- | --- |
| 先选实际出口，再选解析器池 | 主 resolver 复用现有 direct 客户端；DIRECT / Compatible 走 direct，代理走 main，名称不代替类型 |
| 直连失败串到海外池 | direct 计划的 fallback 为空，不递归 DirectResolver；故障回归确认不查询 main / fallback |
| 未选池显式上游弱化拒绝 | 先冻结所选池，再只检查其实际参与集合；无关 direct 显式配置不能豁免 main REJECT / DROP |
| 重试或缓存重新选择负载均衡 | 每次逻辑查询一份 plan；main / fallback / TC 共用；缓存身份含池、上游和叶子 |
| 已有固定出站与 bootstrap | SpecialProxy 旁路新增选池；业务已选 leaf 可选池但不重匹配；基础解析不继承业务计划 |
| 测速破坏回答语义 | 保留一个完整回答的 CNAME 链，仅收窄未签名地址记录；DNSSEC 等保护场景跳过，全部探测失败返回已有原回答 |
| 测速丢失接口 / mark 或误测 TFO | 探测沿实际直连适配器；数字 IP 不解析；TCP 测真实握手，ICMP 继承 socket 策略 |
| 后台查旧 socket 得到另一个进程 | 保存已知或未知 PROCESS 快照，后台不重查旧 source tuple，但重新匹配当前业务规则 |
| 清理后旧查询重新写缓存 | generation 隔离后台、singleflight 和写回；clear / mode / flag / reload / close 取消旧任务 |
| 预取无限扩张或自激活 | 真实来源和前台热度门槛、每 resolver 1024 热点、全局 16 后台任务、无无限排队；新作用域需重新获得真实请求 |
| 遗留前台 singleflight 重试绕过清理 | 实际网络函数和重试入口均检查活动代；阻塞首请求后 Close / Clear / mode 往返的确定性测试确认没有第二次交换 |
| 关闭本地 DNS 后遗留隐藏 bootstrap | executor 保留完整 resolver 集合所有权，在更新规则和节点前关闭全部实例，包括仅暴露 proxy handle 的情形 |
| 私有 resolver 被空闲 ticker 永久持有 | stale 刷新结束或热度消退后停止调度器；新需求安全重启，旧 tick 不影响新实例；执行中仍检测策略变化 |
| 测试 TCP 空闲端口被已有 UDP relay 占用 | 本地 fixture 同时检查 TCP / UDP 绑定；双协议 DNS stub 直接接收预绑定 socket，首次 DNS 查询不通过重试掩盖失败 |

## 已执行的本轮核心验证

以下为 native 选池核心落地后的定向执行记录，随后已执行最终集成门禁。

| 命令 | 已观察结果 |
| --- | --- |
| `go test ./dns -run '^TestDNSRuleRoutingNative' -count=1` | 通过；7 个 native 用例 |
| `go test -race ./dns -run '^(TestDNSRuleRouting\|TestDNSRouting)' -count=1` | 通过；新增 native 与原 QNAME 桥接回归 |

Native 用例覆盖真实 QNAME 规则、A / AAAA / 非 IP 查询、DIRECT / Compatible / 代理实际类型、误导性节点名称、相同作用域缓存、池切换隔离、原 fallback、直连故障不串池、UDP→TCP 冻结、未选显式池不弱化 REJECT / DROP、选中显式池本身的例外、SpecialProxy、非 Rule 模式与原 DirectResolver 用途。

## 本轮最终集成记录

| 项目 | 状态 |
| --- | --- |
| 规定 Go 门禁、上游同步保护与 race | `bash scripts/ci-check.sh test` 通过：7 项同步保护测试，受影响组件 CGO=0 / `with_gvisor` 测试，以及 DNS 相关 CGO=1 race 门禁 |
| 测速专项 | DIRECT 普通 53 与显式传输边界、实际本地 TCP 握手、真实包装 Direct 的 TFO / 关闭端口 / 取消 / 地址族、CNAME、DNSSEC、全失败回退、总超时和并发上限通过 |
| ICMP 平台能力 | 新增 IPv4 / IPv6 回环实测；本地容器创建 raw ICMP socket 返回 EPERM，两项明确 SKIP，未把该结果记为真实 echo/reply 收发通过 |
| 缓存专项 | 热度与过期 TTL、来源 / PROCESS 快照、重新匹配规则、缓存分区、Clear / Close / mode 取消、旧代重试拒绝、完整 resolver 所有权、idle stop / restart 与 retired tick 回归通过；CacheControl race 通过 |
| 原生 DNS 与公共入口实际二进制 | `python3 scripts/test-dns-proxy.py <binary>`：70 次实际 DNS 交换，以及普通 TCP / UDP、来源规则、面板统计、API 关闭和重载；真实 PROCESS 因诊断 socket 不可用明确 SKIP |
| 示例 YAML 与差异检查 | 本轮 amd64 二进制对 README 的完整 YAML 和 `docs/dns-proxy.example.yaml` 执行 `-t` 均通过；`git diff --check` 通过 |
| Linux amd64 / arm64 构建 | 两种架构均以 Go 1.26、CGO=0、`with_gvisor` 构建成功；amd64 已实际执行，arm64 为交叉编译 |
| 精确源提交与云产物 | 同一源提交的 push 工作流重新测试并构建两个架构；Release 正文自动写入完整 SHA，tag 含 SHA 前 12 位，`SHA256SUMS` 对应两个 `.gz` 产物；实际状态可由下方 Actions / Releases 链接核对 |

独立复审发现的 TFO 提前返回、代理包装隐藏探测配置、IPv4 / IPv6 约束丢失、隐藏 bootstrap 未关闭、空闲 ticker 持有解析器和旧 singleflight 重试问题均已修正并补充回归；最终独立复审未发现未解决的 P0 / P1。上述结论说明已检查的范围，不等同于所有网络环境和平台均无缺陷。

## 先前版本基线

此前公共分类器版本（本轮开始时 HEAD `5633c97d`）已完成 Go / race、真实二进制公共与 native 桥接、普通流量、面板统计及关闭，以及 SmartDNS 48.4 SOCKS5 UDP / TCP、HTTP CONNECT TCP 接入验证。这些构成本轮回归基线，不证明本轮新增选池、测速或缓存功能已经通过最终发布验证。

先前审查还修正了 matcher 阻塞解析与重载读锁问题、切组后的 UDP 能力冻结、非 IP 查询闲置 fallback 弱化拒绝、内置 DROP 变 SERVFAIL、TCP 短写及原生初始化期间测试过早连接的问题。启动测试先确认普通 SOCKS TCP 回声可用，再要求首条 DNS 一次成功，不用重试掩盖首条 DNS 失败。

## 验证边界

- 文档地址和本地模拟出站用于确定性测试，不依赖私人节点凭据；配置 `-t` 通过不代表文档保留地址可实际联网。
- 真实 PROCESS 查询和 ICMP 取决于平台、权限与容器能力；任何跳过应在最终记录注明，不以已注入元数据的单测替代真实系统能力声明。
- TUN 的本地服务上下文 / relay 测试不等同于在所有系统完成真实内核 TUN、redir、tproxy 部署。
- arm64 交叉编译成功不等同于执行 arm64 二进制。
- 自动分类限普通单问题明文 TCP / UDP 53；显式上游和加密 / 非 53 传输保留各自原行为。DNSSEC 数据可传递，但本功能不验证其签名。
- IP 测速反映探测条件下的握手或 ICMP 耗时，不保证业务下载速度、全部域名加速或 SmartDNS 全量兼容。缓存按来源端口隔离，没有未经测量的命中率或吞吐提升承诺。
- 普通 SSH 的 IP 到域名显示关联仍受原映射与嗅探影响。

最终构建见 [GitHub Actions](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/build.yml)，产物见 [Releases](https://github.com/chummumm/mihomo-dns-optimized/releases)。发布记录应标明精确源提交和 SHA-256，使源码、测试和下载文件可对应。
