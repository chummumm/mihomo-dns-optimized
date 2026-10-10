# DNS Route Kernel

基于 [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) 稳定版维护的独立派生内核，让 DNS 查询直接复用已有业务分流规则。**不需要 SmartDNS，也不需要再维护一份 DNS 域名策略。** 本项目不是 MetaCubeX 官方发行版。

**原生 DNS 上游支持 UDP、TCP、DoH、DoT、DoQ 和 DoH 的 HTTP/3。IP 测速与双栈优选只用于实际 DIRECT / Compatible 路径，走代理节点的 DNS 回答不测速。** 测速、双栈优选与主动预取默认关闭，可以按需开启。

[下载 Releases](https://github.com/chummumm/mihomo-dns-optimized/releases) · [构建状态](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/build.yml) · [完整配置说明](docs/dns-proxy.md) · [验证记录](docs/dns-rule-routing-validation.md)

## 解决什么问题

原生 DNS 收到查询后，用 Question 中的域名（QNAME）匹配已有 `rules` / `rule-providers`，选出实际出站，再选择 DNS 上游：

| 实际选中的出站 | 内置 DNS 使用的上游 |
| --- | --- |
| DIRECT / Compatible | `direct-nameserver`；通常填写国内可达的解析器 |
| 代理节点 | `nameserver`，以及按原设置适用的 `fallback` |
| REJECT / REJECT-DROP | 自动查询在读取缓存前拒绝 / 静默丢弃；显式传输例外见下文 |

判断依据是**策略组最终选择的实际出口类型**，组名和节点名不参与这个判断。组切到 DIRECT 就走直连池，选择代理节点就走代理池。`direct-nameserver` 为空时，保留原 `nameserver` / `fallback` 行为。

一次逻辑查询只选一次实际出口；上游重试、main / fallback 和 UDP 截断后的 TCP 重试沿用它。直连池失败不会偷偷借用代理池。缓存按解析器池、实际出口、来源等作用域隔离，切组不会混用另一个出口的回答。

DNS 分流通过内置解析器提供，普通代理连接保持原转发行为：

| 查询如何进入 | 解析器目的地址 |
| --- | --- |
| 直接发给 `dns.listen`，或由 TUN DNS 劫持进入本地服务 | 按上述规则选择 direct / main 池 |
| 经 mixed / SOCKS / HTTP 等入站，访问指定解析器的 TCP / UDP 53 | 作为普通代理流量转发，按连接目标和原规则选择出口，不读取 DNS Question 分流 |

普通代理入站不再自动识别和接管目标 53 的 DNS 报文。需要按查询域名选择 DNS 出口时，让客户端使用 `dns.listen`，或显式配置 TUN DNS 劫持；普通 SOCKS / HTTP 请求直接携带的目标域名仍可正常匹配业务规则。

## DNS 路径性能优化

开启 `dns-rule-routing` 时，DNS 规则执行使用派生视图和 classical 纯域名连续段索引，仍按原业务规则顺序判断 SRC / PROCESS / IN。成功的自动查询应答可以在同一来源、进程和入站作用域内跨临时源端口复用；每个请求仍先匹配当前规则，在途查询保持独立取消语义。DIRECT 探测使用共享有界任务池、同出口目标 IP 合并及短期结果复用，代理结果不进行本地探测。

新增的 DNS 面板数据由本机控制器提供，查询记录、趋势和上游统计只保存在进程内存中，不写数据库或日志文件，也不发送遥测。无需维护第二份分流规则；观测不会改变 DNS 回答和出口选择。优化范围、缓存安全边界及测试方式见 [DNS 性能说明](docs/dns-performance.md)。正式发布使用 `v1.19.32-dns-optimized-1` 形式，末尾为数字修订号；tag、发行标题与内核版本一致，源码哈希保留在 BUILDINFO 中供核对。内核一键更新使用本二开仓库的正式发行并验证 SHA256，不会回装官方内核，见[更新说明](docs/releases.md#内核一键更新)。

## DNS 状态与原版面板

本仓库的 [Clash Dashboard 分支](https://github.com/chummumm/mihomo-dns-optimized/tree/clash-dashboard) 在原有面板上提供 DNS 概览、查询记录与上游统计，并保留嗅探域名显示兼容。连接页的“全部”与各 IP 从同一份保留记录计算数量；活跃连接完整保留，“保留关闭连接”最多保留最近 5000 条关闭记录。长表格按可见区域渲染，刷新时保持阅读位置。

DNS 观测默认随 `dns.enable` 开启，可用 `dns.observability: false` 关闭。最多保留 4096 条查询明细，并受 8 MiB 记账预算和 24 小时保留期限约束；趋势使用固定分钟桶，上游统计最多保留 128 个独立身份，超出部分归入合计。普通完整重载保留数据，关闭观测会释放数据，重新开启或重启内核从零开始。面板不会将 DNS 数据写入浏览器本地存储。

查询次数、实际上游交换次数和连接数采用不同口径，不能直接相加；NXDOMAIN 是 DNS 回答，竞争中取消的上游交换单独统计。完整字段、内存边界、安装方式和验证方法见 [DNS 状态展示](docs/dns-observability.md)。

## 快速开始

1. 从 [Releases](https://github.com/chummumm/mihomo-dns-optimized/releases) 下载对应系统和 CPU 的文件，核对 `SHA256SUMS`；直接运行可选 `.gz` / Windows `.zip`，Linux 也提供安装包。
2. 保存下面的示例为 `config.yaml`，或把其中 DNS 设置并入自己的配置，保留已有节点、策略组和业务规则。
3. 校验并运行，将 DNS 客户端指向配置中的监听地址。

| 平台 | 下载格式与范围 |
| --- | --- |
| Linux | 19 个 CPU 目标的 `.gz`；12 个目标另有 `.deb` / `.rpm`，5 个目标另有 `.pkg.tar.zst` |
| Windows | x86、x86-64 v1/v2/v3、ARM64 的 `.zip` |
| macOS | x86-64 v1/v2/v3、Apple Silicon 的 `.gz` |
| FreeBSD | x86、x86-64 v1/v2/v3、ARM64 的 `.gz` |
| Android | x86、x86-64、ARMv7、ARM64 的 `.gz`，NDK r29 / API 34 |

完整矩阵为 **37 个平台 / CPU 目标、66 个二进制归档和安装包**，另附 `BUILDINFO.json` 与 `SHA256SUMS`。`linux-amd64` 保持 GOAMD64=v1；v2/v3 单独提供。当前不重复上游旧 Go 工具链、LoongArch ABI1 等兼容变体。完整 CPU、最低兼容边界和安装说明见[预编译文件与安装包](docs/releases.md)。

Debian / Ubuntu 例如使用 `mihomo-dns-linux-amd64-<version>.deb`：

```bash
sudo apt install ./mihomo-dns-linux-amd64-<version>.deb
# 将自己的配置放入 /etc/mihomo/config.yaml 后再校验和启动。
sudo mihomo -t -d /etc/mihomo
sudo systemctl daemon-reload
sudo systemctl enable --now mihomo
```

安装包使用 `/usr/bin/mihomo`、`/etc/mihomo/config.yaml` 和 `mihomo.service`，通过包管理器保护已有配置，不自动启动服务。包名为 `mihomo-dns-optimized`，声明替换官方 `mihomo` 包。通用起始配置只有本机 mixed 和 DIRECT；私人节点与规则需自行提供。

直接使用压缩核心时，解压并命名为 `dns-route-kernel` 后运行：

```bash
chmod +x ./dns-route-kernel
./dns-route-kernel -t -f config.yaml
./dns-route-kernel -f config.yaml
```

下面是可独立校验的配置，直连池使用普通 DNS，代理池使用 Cloudflare DoH；请按网络可达性调整。`代理出口` 暂只含 DIRECT 占位项，**加入自己的节点后才会真正走代理**；接入已有配置时直接使用原来的策略组即可。

```yaml
mode: rule
mixed-port: 7890
allow-lan: false
dns-rule-routing: true

dns:
  enable: true
  listen: 127.0.0.1:1053
  enhanced-mode: redir-host
  ipv6: true
  respect-rules: false

  # 节点和解析器自身域名的独立基础解析，避免启动循环。
  default-nameserver: [223.5.5.5, 119.29.29.29]
  proxy-server-nameserver: [223.5.5.5, 119.29.29.29]
  direct-nameserver: [223.5.5.5, 119.29.29.29]
  nameserver:
    - https://cloudflare-dns.com/dns-query
    - https://1.1.1.1/dns-query

  # 可选：这里显式开启；仅实际 DIRECT 优选 IP 与双栈。
  speed-check-mode: [tcp:443, tcp:80, ping]
  speed-check-timeout: 1000
  speed-check-concurrency: 16
  dualstack-ip-selection: true
  dualstack-ip-selection-threshold: 10
  dualstack-ip-allow-force-aaaa: false
  prefetch-domain: true
  serve-expired: true
  serve-expired-ttl: 604800
  serve-expired-reply-ttl: 1

proxy-groups:
  - name: 代理出口
    type: select
    proxies: [DIRECT]  # 添加你自己的节点名称。

rules:
  - DOMAIN-SUFFIX,example.com,DIRECT
  - DOMAIN-SUFFIX,example.net,代理出口
  - MATCH,DIRECT
```

示例监听本机 `1053`，原 mixed 仍为 `7890`。需要标准 DNS 端口或其他设备接入时，调整监听与原有访问范围配置。另有[包含文档代理节点的完整示例](docs/dns-proxy.example.yaml)，其中保留地址须替换后才能实际联网。

`enhanced-mode: redir-host` 返回真实 IP，并保留 IP 到历史查询域名的映射，供只有 IP 的连接辅助匹配。若不需要这层反查，可改为 `enhanced-mode: normal`；正常 DNS 解析、正向回答缓存和本项目的原生优化仍保留。透明代理若拿不到域名，需要依赖嗅探或其他规则；模式区别与可选 TLS / HTTP 全端口嗅探示例见[使用说明](docs/dns-proxy.md#enhanced-mode-与域名嗅探)。

## 只有直连路径才做 IP 测速

这里优选的是 **DNS 回答中的目标 IP**，不是代理节点速度，也不是 DNS 服务器所在地。

- 仅内置 DNS 已选 DIRECT / Compatible、所选上游集合全部支持自动传输的 A / AAAA 查询参与；代理路径不参与。原生 DoH / DoT / DoQ 及自定义端口同样按实际出口判定。
- 显式 DNS 出口等传输例外不做本地测速；有效缓存命中直接返回，不为这次命中重复探测。
- TCP 测握手耗时，ping 测 ICMP 往返。探测继承所选直连出口的接口 / mark，不重新匹配规则；ping 是否可用取决于平台与权限。
- 保留候选所属回答及 CNAME 链，不拼接不同服务器的回答；DNSSEC 等保护场景不改写。全部探测失败但已有有效回答时返回原回答。

测速不代表下载带宽，也不保证所有域名都更快。后台更新属于新的查询，满足条件时可以重新优选。

可选双栈优选复用这次已选直连出口和同一个超时预算。默认保留 IPv4；当 AAAA 查询同时取得两族可测速的回答，且 IPv4 比 IPv6 快至少所设阈值时，返回零 TTL 的 NOERROR / NODATA。代理查询不另查另一族、不探测；测速失败或受 DNSSEC 保护时保留原回答。该临时优选结论不缓存，避免被过期缓存长期延续。

## 可选 DNS 设置

以下字段均放在原 `dns:` 下；顶层 `dns-rule-routing` 的默认值是 `false`。

`redir-host-filter: []` 可在 `enhanced-mode: redir-host` 下排除指定域名的真实 IP 反查映射，支持精确域名和 `+.example.com` 等域名通配符。正常 DNS 回答、入站明确携带的域名和嗅探域名不受影响。名单使用已有域名索引，完整重载后新名单约束也适用于旧映射；详见[映射黑名单](docs/dns-proxy.md#redir-host-映射黑名单)。

| 测速字段 | 默认值 | 含义与单位 |
| --- | --- | --- |
| `speed-check-mode` | 空 / `[none]` | 关闭；可设 `tcp:端口`、`ping`；`none` 不能与其他项混用 |
| `speed-check-timeout` | 0 → 1000 | 毫秒；显式范围 1–5000，只限制本轮可选优选等待；无有效答复时，已发出的 DNS 继续使用正常解析期限 |
| `speed-check-concurrency` | 0 → 16 | 最大 256；只限制活动 IP 探测，满额立即跳过新探测，不排队，不限制 DNS 上游请求；每轮最多 256 个候选 IP |
| `dualstack-ip-selection` | `false` | 开启 DIRECT 双栈优选；仍需启用测速 |
| `dualstack-ip-selection-threshold` | 10 | 毫秒；0–1000，另一族至少快多少才过滤当前族 |
| `dualstack-ip-allow-force-aaaa` | `false` | 是否允许反向过滤 A、只保留更快的 IPv6；默认保留 IPv4 |

| 缓存字段 | 默认值 | 含义与单位 |
| --- | --- | --- |
| `prefetch-domain` | `false` | 为近期活跃的真实 DNS 来源预取 |
| `serve-expired` | `true` | 允许返回过期缓存并触发有界后台更新 |
| `serve-expired-ttl` | 0 | 秒；0 不限过期年龄，保留上游缺省行为；604800 表示最多过期 7 天 |
| `serve-expired-reply-ttl` | 1 | 秒；过期回答交给客户端的 TTL，允许 0，不延长原记录过期时间 |

预取要求真实 DNS 入站具有有效来源，同一作用域 1 分钟至少 3 次前台请求、最近 1 分钟仍活跃，按剩余 TTL 约 80% 安排刷新。每个 resolver 最多 1024 个热点，全局最多 16 个后台任务。

满足自动选路条件的正式答案缓存和预取热度可跨临时来源端口复用；在途查询合并仍按来源端口隔离。原有 `cache-algorithm` / `cache-max-size` 仍可使用，不承诺未经实测的命中率提升。

后台保留来源和已知 / 未知 PROCESS 快照，重新检查当前规则，不拿旧 socket 识别可能复用端口的新进程。清缓存、模式 / 开关变化、重载会使旧后台任务失效；缓存命中与并发合并也保留来源、子规则和实际出口边界。

| 回答调整字段 | 默认值 | 行为 |
| --- | --- | --- |
| `force-no-cname` | `false` | 将完整、无歧义的未签名 A / AAAA CNAME 链压平到查询名；不会补造 IP 或重新分流 CNAME |
| `rr-ttl-min` | 0 | 秒；0 不设下限，保留零 TTL 的不可缓存回答 |
| `rr-ttl-max` | 0 | 秒；0 不设上限，限制正常回答及其缓存寿命 |

回答调整在写缓存前完成；过期回答仍使用 `serve-expired-reply-ttl`，不会被 TTL 下限延长。CNAME 类型查询、不完整 / 歧义链、DNSSEC 等保护场景保留原回答。节点和解析器 bootstrap 不继承这些业务回答调整。

## 生效条件与兼容边界

自动 QNAME 选路只在 **开关开启、Rule 模式、入站没有固定 `proxy:`** 时接管；固定出站和 Global / Direct 保留原优先级。入站 `rule:` 仍作为子规则入口，已经选定业务叶子的内部解析继承该叶子，bootstrap 独立执行。

- **使用真实 DNS 回答。** 内置 DNS 可使用 `normal` 或 `redir-host`；本功能与内置 FakeIP 同时启用会在校验时报错。不增加 listener 类型或专用 DNS 代理端口。
- **规则顺序不变。** IN / SRC / PROCESS、端口和网络属性仍可参与；QNAME 用于域名规则。网站目标 IP / GEOIP / ASN 尚未知，不拿解析器 IP 代替，也不为选路递归解析同一个名字。
- **按 QNAME 分流的入口是内置 DNS。** 内置 DNS 已知 QNAME，配置的 UDP / TCP 任意端口及 DoH / DoT / DoQ / H3 均可自动选池和选出口。普通代理入站不自动接管明文 DNS，也不解密客户端 DoH；显式 TUN DNS 劫持仍可将请求送入内置 DNS。
- **显式 DNS 传输仍是例外。** `#Group` / `#DIRECT` / `#interface` 等保留旧行为；先选池，再判断该池的例外。混有显式上游时，显式分支可独立回答；未选池不会削弱自动分支的拒绝动作。
- **不另写 DNS 域名策略。** 开启时停用 `nameserver-policy`、`direct-nameserver-follow-policy`、`fallback-filter.domain/geosite`；`fallback` 的适用 IP 过滤、hosts、基础解析等保留。关闭开关并完整重载可恢复原策略。
- **respect-rules 的旧模式行为保留。** 非 Rule 模式不建立新计划；在自动范围内，QNAME 计划代替按解析器地址二次选路。

加密上游按冻结的出口身份复用连接，切换叶子后使用相应连接池；每个配置上游最多保留 64 个作用域，只淘汰空闲项，重载会取消并关闭旧池。QNAME 和来源端口不进入连接池键，保留 H2 / H3 / DoQ 的复用；它们仍进入更严格的回答缓存身份。

面板按次展示 QNAME、来源、规则和策略链，底层长连接标记为 `DNS-TRANSPORT`、显示真实解析器，不沿用首个域名冒充后续请求。逻辑查询计数展示 DNS 负载，底层只统计一次真实线路流量；关闭一条逻辑查询不关闭共享连接上的其他查询。普通 SSH 的反向域名显示不由本功能改写。完整限制见[使用说明](docs/dns-proxy.md)。

## 云编译、更新与验证

[构建工作流](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/build.yml)先执行规定 Go 测试、DNS race、同步保护与安装包检查，再构建完整 37 目标矩阵；Linux amd64 产物运行本地模拟服务的实际二进制端到端测试。其他目标为交叉编译，不等同于在各设备实际执行。所有必需归档和安装包齐全、摘要一致后才发布。

`main` 的源码、依赖或构建配置变更通过验证后自动发布带源提交标识和 SHA-256 的下载包；PR 只生成 Actions artifact。纯文档更新跳过自动构建和发布，具体路径与显式发布入口见[发行文档](docs/releases.md)。当前 `main` 对应构建才能更新 Latest，旧构建重跑不会覆盖新版本入口。

[稳定版同步](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/sync-upstream.yml)计划每日北京时间 04:23 检查上游正式 release，定时任务可能延迟。先合并、测试和编译，通过后普通推送；冲突、验证失败或远端分支并发变化时停止，不强推覆盖本项目修改。基线见 [UPSTREAM_VERSION](UPSTREAM_VERSION) / [UPSTREAM_COMMIT](UPSTREAM_COMMIT)。

本地验证使用项目当前工具链；CI 的 Go 版本与来源见工作流。常用命令：

```bash
bash scripts/ci-check.sh test
go build -tags with_gvisor -o dns-route-kernel
python3 scripts/test-dns-proxy.py ./dns-route-kernel
python3 scripts/test-dns-observability.py ./dns-route-kernel
```

测试事实、ICMP / PROCESS 平台跳过项与验证范围见[实施与复查记录](docs/dns-rule-routing-validation.md)。发布成功以精确提交的 Actions / Releases 记录为准。

## 文档导航

| 文档 | 内容 |
| --- | --- |
| [配置与工作原理](docs/dns-proxy.md) | 字段、选池、测速、缓存和兼容边界 |
| [完整原生示例](docs/dns-proxy.example.yaml) | 无私人凭据、无远程 provider 的可校验模板 |
| [设计与验收约定](docs/dns-rule-routing-design.md) | 优先级、未知 IP、一次计划、生命周期 |
| [验证记录](docs/dns-rule-routing-validation.md) | 实测结果与明确限制 |
| [DNS 状态展示](docs/dns-observability.md) | 纯内存概览、查询明细、上游统计、面板与计数口径 |
| [SmartDNS 迁移对照](docs/smartdns-migration.md) | 字段对应、双栈与缓存差异、域名屏蔽迁移 |
| [预编译文件与安装包](docs/releases.md) | 37 目标矩阵、deb / rpm / Arch 安装与配置保护 |
| [云编译与上游同步](docs/upstream-sync.md) | 下载、校验、发布、同步失败与预演 |
| [上游使用文档](https://wiki.metacubex.one/) | 原有节点、规则、TUN、API 等通用能力 |
| [上游 README](https://github.com/MetaCubeX/mihomo/blob/Alpha/README.md) | 原项目完整介绍和上游说明 |

## 上游、致谢与许可证

保留 MetaCubeX/mihomo 的 Git 历史，并感谢其维护者及贡献者。项目采用 [GPL-3.0](LICENSE)，派生修改不代表 MetaCubeX 官方行为或承诺。原有协议、规则、TUN、API 等能力来自上游；面板可使用 [metacubexd](https://github.com/MetaCubeX/metacubexd)。

保留上游致谢：[Dreamacro/clash](https://github.com/Dreamacro/clash)、[SagerNet/sing-box](https://github.com/SagerNet/sing-box)、[riobard/go-shadowsocks2](https://github.com/riobard/go-shadowsocks2)、[v2ray/v2ray-core](https://github.com/v2ray/v2ray-core)、[WireGuard/wireguard-go](https://github.com/WireGuard/wireguard-go)、[yaling888/clash-plus-pro](https://github.com/yaling888/clash)。

上游 README 另保留以下命名声明：

> In addition, any downstream projects not affiliated with `MetaCubeX` shall not contain the word `mihomo` in their names.
