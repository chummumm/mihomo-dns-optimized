# DNS 全局开关：实施与复查记录

## 交付范围

本轮把旧专用 `dns-proxy-port` 改为顶层 `dns-rule-routing` 布尔开关，默认关闭。设计、实现、交叉复查和二进制验证围绕 [完整行为约定](dns-rule-routing-design.md) 进行。源码基线为根目录记录的 Mihomo `v1.19.32`；具体派生版本、提交与构建时间由每次 GitHub Actions 发布记录标识。

公共入口只接管 Rule 模式、未固定出站的普通明文 TCP / UDP 53 查询；普通业务、Global / Direct、固定入口按约定保留原路径。内置 DNS 和 TUN DNS 劫持保持本地服务身份，通过上游交换桥接同一 QNAME 路由核心。协议解析和域名匹配复用项目既有实现，不新增 DNS 域名规则列表，不使用 FakeIP。

## 审查中发现并修正的问题

| 问题 | 修正及验证依据 |
| --- | --- |
| 仅解析候选配置就临时改变在线 Mode / FindProcessMode | 临时配置只改变解析所需设置，不改变运行模式；回归验证失败解析和解析期间真实 API 模式变更不会被回滚覆盖 |
| 关闭内置 DNS 后，外部解析器 Host 缺少独立引导 | 开关开启时仍保留 default / proxy bootstrap，业务 DNS 服务继续关闭；配置测试和 Host:53 二进制交换验证 |
| 普通 IP 规则触发内部 DNS 时，配置重载可能等待重入的读锁 | 仅在阻塞解析期间释放公共 matcher 的读锁，返回后重新取得；测试在解析阻塞期间实际更新规则和出站 |
| 切组可能改变同一次 DNS 重试的出口或 UDP 能力判断 | 冻结组、实际叶子和 UDP 能力；后续 main / fallback / TCP 截断重试不重选，保留组级 disable-udp |
| 内置 DNS 在 Global 模式中被新增逻辑强行改走 GLOBAL | 非 Rule 模式不创建自动计划；分别验证 respect-rules 开关下的原 DNSDialer 行为 |
| 本地 DNS 1053 或原网站 TCP:443 进入 DNS 规则元数据 | 上游目标端口匹配 53，IN-PORT 保留本地监听；真实 DNS 网络与内部 lookup 网络分开，进程查找继续使用原 source socket |
| 内置 REJECT-DROP 被错误转为 SERVFAIL | 使用明确的 drop 标记；DNS listen 与 TUN relay 的 TCP / UDP 均不为被丢弃查询生成回答 |
| 非 IP 查询误把闲置 fallback 算作实际候选，弱化拒绝动作 | 按原 isIPRequest 分支选择候选；TXT / MX / HTTPS 不受未使用的 fallback 影响 |
| TCP 下游短写可能截断 DNS 长度帧 | 复用已有完整写入 helper；最大帧与流水线测试使用每次最多写 3 字节的连接 |
| 旧 dedicated listener 留下无用握手扩展 | 删除专用 listener 及专属测试，将 SOCKS4/5 握手源文件恢复到上游基线；普通 mixed 实现继续复用上游 |
| 云端第一条 HTTP CONNECT 测试早于内核完成启动 | 用阻塞本地 provider 初始化的实验复现 200 后 EOF：原版先打开监听，再加载 provider / profile 并进入 Running；测试改为先完成普通 SOCKS TCP:443 回声证明转发就绪，随后第一条 DNS 仍要求一次成功，不用重试掩盖 DNS 问题 |

交叉复查同时核对了业务 DNS 策略停用、bootstrap 例外、缓存 / singleflight 范围、固定出站传递、未知目标 IP 的逻辑传播，以及失败是否意外改走 DIRECT。正式配置路径没有保留被停用的业务 nameserver-policy；未为不可达的策略组合增加额外算法。

## 本地验证

以下记录来自本轮实现后的实际执行；CI 会对提交重新执行测试，发布结果以对应 Actions 的精确提交为准。

| 验证 | 结果与覆盖 |
| --- | --- |
| `bash scripts/ci-check.sh test` | 通过；包括 7 项本地 Git 上游同步保护测试、相关 Go 包测试以及 DNS 专项 race 检查 |
| 完整 Linux amd64 / arm64 内核 | 两个架构的 `with_gvisor`、CGO 关闭构建通过；arm64 为交叉编译 |
| `scripts/test-dns-proxy.py` 真实二进制 | 通过；60 次实际 DNS 上游交换，另验证普通 TCP / UDP 转发和拒绝动作 |
| 内置 resolver 专项 | 缓存、来源 / 子规则 / EDNS / 固定出口隔离、事务 ID、取消与后台刷新、main / fallback / TC、bootstrap 和模式矩阵通过 |
| SmartDNS 48.4 实际程序 | SOCKS5 UDP、SOCKS5 TCP、HTTP CONNECT TCP 三种方式均通过；使用认证的原 mixed 入口，两个域名分别到达两条本地模拟出站 |
| 示例配置与差异检查 | 最小 YAML 的 `-t` 校验、`git diff --check` 通过 |

真实二进制测试使用本地模拟 SOCKS / DNS 出站及文档保留的目标地址，不依赖用户账号或外部 DNS 服务。覆盖 HTTP CONNECT 首帧预发送、SOCKS4/4a/5、IPv4 / IPv6、解析器域名独立引导、同连接逐查询分流、选择器切换、IN / SRC / 端口规则、固定出站和子规则、REJECT / REJECT-DROP、非 DNS 原字节转发、响应 ID / Question 校验、PASS 控制、Global 切换和完整配置重载。

内置 DNS 的二进制检查覆盖 TCP / UDP 本地监听、QNAME 规则、真实 IN-PORT / 来源 / 网络、停用旧 nameserver-policy、选择器变化后的缓存隔离、事务 ID、拒绝动作和原生连接面板。外部 tracker 另验证精确流量统计、完成清理和面板关闭取消。

## 验证边界

- 当前本地容器禁止原版进程查询所需的诊断 socket，返回 `socket: operation not permitted`。真实进程查询的这一项明确记为跳过；原版无法识别进程时的后续规则行为及已识别进程元数据有测试。云端若平台允许则执行真实进程识别断言。
- TUN 已验证三种入口的本地 DNS 服务上下文及 TCP / UDP relay，不将其等同于已在所有操作系统、真实内核 TUN / tproxy / redir 网络配置中完成部署测试。其他代理协议依靠公共解包后的转发钩子接入，并未逐一连接实际远端协议服务器。
- Linux arm64 由云端交叉编译；没有在本地声称执行 arm64 机器码。
- 自动分类限于普通单问题明文 TCP / UDP 53。DoH / DoT / DoQ、非 53 和显式 DNS 出口仍沿用原路径。外部不支持的首份负载可以回到原流程；内置自动 resolver 对不支持的查询明确拒绝。
- 本功能不验证 DNSSEC 签名，不替代 DNS 防污染服务器，也不保证消除普通 SSH 连接原有的 IP 到域名显示关联。
- 缓存按来源端口等身份隔离，可能减少不同 socket 间的内置缓存复用；没有作未经测量的吞吐或延迟提升承诺。

## 云编译与后续维护

[构建工作流](../.github/workflows/build.yml) 对提交执行相同测试，构建 Linux amd64 / arm64 并发布带 SHA-256 的压缩包。amd64 构建会执行真实二进制端到端脚本；arm64 进行交叉编译。上游同步先通过同一测试与两种架构构建，再进行普通 merge 提交；代码冲突、测试失败或 main 并发变化时停止。

具体运行见 [GitHub Actions](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/build.yml)，下载见 [Releases](https://github.com/chummumm/mihomo-dns-optimized/releases)。发布名称包含源提交前缀，可将下载文件与源码及测试记录对应起来。
