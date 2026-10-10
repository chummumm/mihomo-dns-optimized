# DNS 分流、SmartDNS 功能迁移与发行：验证记录

本文保留原生 DNS 扩展阶段的历史验证事实。后续版本已删除普通代理入站对 TCP / UDP 53 的自动识别和逐查询路由；下面关于该旧路径的测试数量与结果不代表当前仍提供这项能力。当前入口、配置与回归要求以[使用说明](dns-proxy.md)和[设计约定](dns-rule-routing-design.md)为准，当前提交的验证结果见对应 Actions。

## 当时的实现范围

在 `c30151ec8f2541634bfcab9742b89301bcc4f05d` 原生选池版本上，本次补齐配置上游的 UDP / TCP 任意端口、DoT、DoH、HTTP/3、DoQ 自动 QNAME 路由，迁入可选 DIRECT 双栈、CNAME / TTL 控制，并扩展 37 个平台 / CPU 的发行矩阵。原生 DNS 先按业务规则选实际出口，再选 direct / main；外部代理入站仍只分类明文 TCP / UDP 53，保留原目的解析器。

该阶段本地实现、独立审查和下述集成验证均已完成。GitHub 对精确提交执行测试和完整矩阵，只有全套必需文件通过检查才发布；每个 Release 的正文、BUILDINFO 和 SHA256SUMS 可将源码和产物对应。配置与边界见[使用说明](dns-proxy.md)、[设计约定](dns-rule-routing-design.md)、[SmartDNS 迁移对照](smartdns-migration.md)。

## 独立审查与修复

| 检查点 | 结论与回归依据 |
| --- | --- |
| 原生加密上游只接受语法、未走新增规则 | 统一自动能力判断；原生 UDP/TCP 任意端口、DoT/DoH/H3/DoQ 通过首次计划连接实际配置解析器 |
| H2 / QUIC 连接跨出口复用 | 每配置上游保留最多 64 个按实际叶子、组、UDP 能力、epoch 隔离的私有协议客户端；QNAME / source-port 不作为连接键 |
| LRU 关闭仍有查询的共享客户端 | 活动引用计数阻止淘汰；全部繁忙时有界等待并服从调用取消；reset / close 取消作用域生命周期 |
| H2 拨号在关闭后迟到 | 实际测试发现 HTTP 会分离拨号与原请求取消；私有 dialer 绑定池生命周期，迟到返回的连接立即关闭 |
| 取消一个加密查询影响兄弟 | H2/H3/DoQ 按请求或 stream 取消，保留共享连接；DoT 取消关闭本次独占连接且不得放回池 |
| 面板关闭 DNS 后又自动重试 | 原生 53 与加密查询引入仅属于本次请求的关闭标记；阻止 singleflight 重试 / 后台复活，后续独立查询正常 |
| 面板长期显示首次 QNAME / 重复统计 | 逻辑查询展示当前 QNAME / source / rule；DNS-TRANSPORT 展示真实上游连接，全局线路字节只统计一次；真实 DNS 客户端的 NETWORK 在逻辑记录中保留 |
| SRC / IN / PROCESS 被域名强制覆盖 | 复用原规则顺序，真实 source / source-port / IN / process 元数据保留；只把未知业务目标 IP 清空，不拿 DNS 服务器 IP 代替 |
| 不同设备或入口混用缓存 | 自动回答 cache / singleflight 包含完整来源、池和出口；实际不同回环源 IP、相同源端口测试确认隔离，即使同为 DIRECT |
| 代理 DNS 做本地 IP 测速 | 仅实际 DIRECT / Compatible 且所选集合全为自动传输可测速；代理不探测、不额外查询另一族 |
| 双栈比较二次选组或预算翻倍 | 两族使用同一冻结计划、客户端集合、总 deadline 和共享 probe slots；默认只过滤较慢 AAAA，显式开关才允许反向偏好 |
| 短时双栈结论进入长期过期缓存 | 合成 NOERROR / NODATA 使用零 TTL；成功但不可缓存的新答案删除同 key 的旧记录，LRU / ARC 都有回归；旧 generation 无权删除新记录 |
| CNAME 压平破坏链或 DNSSEC | 仅完整唯一 A / AAAA 链，使用整链最短 TTL；断链、循环、歧义、DNAME、DO/CD/AD/签名/截断跳过 |
| 旁路模式的 DNSSEC 查询命中已改写缓存 | 独立复现确认旧 question-only 缓存会混用；启用 AnswerPolicy 后旁路也按完整 wire（排除 ID）隔离 cache / singleflight，普通→DO/CD 及并发回归通过 |
| TTL 下限延长过期回答 | 回答控制只在写缓存前应用；零 TTL 不抬高，过期命中继续使用单独的 serve-expired-reply-ttl |
| 包覆盖配置或自动重启 | deb conffiles / RPM noreplace / Arch backup；无生命周期启停脚本，包内二进制与目标编译结果一致 |
| Release 缺失目标仍更新 Latest | 从唯一 targets.json 核对完整 66 文件集合及摘要，验证 GitHub 附件 digest 后才允许精确 main 提交更新 Latest |

未发现上述审查范围内尚未解决的阻塞问题。这不是对全部网络环境、设备和协议实现无缺陷的保证。

## 该阶段已执行的集成验证

| 验证 | 已观察结果 |
| --- | --- |
| `bash scripts/ci-check.sh test` | 通过：7 项同步保护、7 项发行脚本测试；受影响 Go 包 CGO=0 / with_gvisor；DNS、DualStack、AnswerPolicy、ARC 删除等 CGO=1 race |
| 真实加密协议 | `TestDNSRuleRoutingEncrypted` 使用本地受信与不受信 TLS、HTTP/2、HTTP/3、DoT、DoQ；证书校验、实际上游目标、复用、切叶子隔离、取消兄弟存活均通过 |
| 池与关闭 | LRU 活动引用 / 容量等待 / reset / close；H2/H3/DoT/DoQ 真实迟到建连；Resolver 层面板关闭不再重试通过 |
| SmartDNS 参数 | 双栈阈值、默认保留 A、显式反向选择、DNSSEC / 失败保护、总超时与共享并发；CNAME / TTL / DO-CD 缓存与 singleflight；旧过期记录失效与 generation 保护通过 |
| 实际二进制 | 最新 with_gvisor 二进制运行 `scripts/test-dns-proxy.py`：**84 次实际 DNS 上游交换**，以及普通 TCP / UDP、面板字节 / 关闭、模式 / 固定出口 / 子规则和原生池回归通过 |
| 来源分流 | 实际 TCP / UDP 客户端、来源＋域名例外、来源 DIRECT、同源端口不同源 IP 缓存隔离、同 DIRECT 叶子隔离、block 优先级通过；本地私有原配置与优化配置另做 132 组真实规则条件求值 |
| 实际 PROCESS | 本地 OS 诊断 socket 不可用，二进制测试该项明确 SKIP；进程元数据、快照和来源 socket 定向单测 / race 通过，不等同于真实 OS 进程识别通过 |
| ICMP | 本地创建 IPv4 / IPv6 raw ICMP socket 为 EPERM，两项明确 SKIP；真实 TCP 探测、接口 / mark / TFO / 地址族与取消测试通过 |
| 用户配置与示例 | 最新二进制对用户主配置（独立屏蔽文件）、README 完整 YAML、dns-proxy.example.yaml 执行 `-t` 均通过；保留原节点区块、来源规则顺序、外部屏蔽清单逐条匹配检查通过 |
| 包格式验证 | 29 个各架构 deb / rpm / Arch fixture 包实际生成并解包，架构、版本、文件清单 / 权限、配置保护、root 归属、无启停 hooks 和二进制哈希检查通过；未在宿主机安装或启动服务 |
| 多平台发行 | 本地对多目标做真实交叉预检；最终 37 目标是否全部完成，以该源提交 Actions 及完整 Release 为准，不以配置矩阵存在代替成功构建 |
| 文档与补丁 | `git diff --check`、工作流语法和示例配置检查通过 |

用户配置与自定义域名清单只作为私有交付材料，不纳入公共仓库或发行包。

## 延续的核心约束

第一次路由决定实际 DIRECT / Compatible 或代理叶子。direct 池故障不借用 main / fallback；main / fallback、UDP 截断重试和并发候选都沿同一个计划。先选池再检查该池的显式例外；未选池不能弱化 REJECT / DROP。非 IP 查询不引入闲置 fallback 的例外。

Global / Direct、入站固定出口、子规则入口和 bootstrap 保持原优先级；已选业务叶子的内部解析不重匹配。未知业务目标 IP 通过 NOT / AND / OR、classical provider 与子规则传播，不能把未知当作“匹配失败所以 NOT 成功”。

缓存和预取保留原来源及已知 / 未知 PROCESS 快照，后台不拿旧 socket 去识别已复用端口的新进程。清缓存、模式 / 开关变化、重载关闭通过 generation、epoch、后台取消和重试入口检查阻止旧任务回填；热点和后台并发均有上限，空闲调度器释放实例。

## 历史基线

原生 direct / main 选池版本 `c30151ec8f2541634bfcab9742b89301bcc4f05d` 已在 GitHub 通过规定 Go / race 门禁、70 次二进制 DNS 交换与 Linux amd64 / arm64 构建并发布；该版本的原生自动传输尚限普通 53，本次补齐加密与其他配置端口。

更早公共分类器基线 `5633c97d` 已验证 mixed / SOCKS5 UDP / TCP / HTTP CONNECT 与 SmartDNS 48.4 接入，以及面板统计、模式切换和原始目的解析器保留。该分类器现已删除，这些记录仅用于追溯历史，不作为当前逐查询分流能力或验收结果。

## 明确限制

- SRC 规则看到的是实际到达 Mihomo 的地址；经过 DNS 转发器或 NAT 后，无法还原被隐藏的原客户端。远端 LAN 应用的 PROCESS 通常未知，不由域名推断。
- 原生规则匹配的逻辑 DNS 目标端口是 53；实际 DoH / DoT / DoQ 传输保留配置的 443 / 853 / 其他端口。真实监听端口属于 IN-PORT。
- 配置 `-t` 不会证明每个私人节点、远程 provider、设备上的既有自定义文件或实际网络均可用。未访问用户代理节点进行公网测试。
- 跨平台构建不等于每种设备上的运行验证；TUN、redir、tproxy、ICMP、PROCESS 仍取决于上游平台支持和运行权限。
- 当前普通代理入站不再分类明文 TCP / UDP 53，也不解密任意客户端 DoH / DoT / DoQ；内置 DNS 的已知 QNAME 上游仍可使用已支持的配置协议和端口。
- 传递 DNSSEC 数据不等于校验 DNSSEC 签名；速度反映探测时的握手或 ICMP 耗时，不保证下载吞吐或全部 SmartDNS 行为等价。
- 普通 SSH 的反向 IP→域名显示仍可能受原映射影响；用户配置可用窄 IP＋TCP＋22 规则确定实际出口，不能用面板显示名证明 SSH 请求了该域名。

构建记录见 [Actions](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/build.yml)，下载见 [Releases](https://github.com/chummumm/mihomo-dns-optimized/releases)；完整安装包说明见[发行文档](releases.md)。
