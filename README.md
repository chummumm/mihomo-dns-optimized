# DNS Route Kernel

基于 [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) 稳定版维护的独立派生内核，让 DNS 查询直接复用已有业务分流规则。**不需要 SmartDNS，也不需要再维护一份 DNS 域名策略。** 本项目不是 MetaCubeX 官方发行版。

**IP 测速只用于实际 DIRECT / Compatible 路径；走代理节点的 DNS 回答不测速。** 测速与主动预取默认关闭，可以按需开启。

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

原有普通入站 DNS 分类也保留：

| 查询如何进入 | 解析器目的地址 |
| --- | --- |
| 直接发给 `dns.listen`，或由 TUN DNS 劫持进入本地服务 | 按上述规则选择 direct / main 池 |
| 经 mixed / SOCKS / HTTP 等入站，访问指定解析器的 TCP / UDP 53 | 保留客户端指定的解析器地址，只按 QNAME 选择出口 |

后者不要求开启内置 DNS，也不依赖普通 sniffer。普通网页、SSH、非 53 流量继续原有流程。

## 快速开始

1. 从 [Releases](https://github.com/chummumm/mihomo-dns-optimized/releases) 下载对应架构的 `.gz`，核对发布页的 `SHA256SUMS`，解压并将二进制命名为 `dns-route-kernel`。
2. 保存下面的示例为 `config.yaml`，或把其中 DNS 设置并入自己的配置，保留已有节点、策略组和业务规则。
3. 校验并运行，将 DNS 客户端指向配置中的监听地址。

| 下载文件 | 适用平台 |
| --- | --- |
| `mihomo-dns-linux-amd64-<version>.gz` | Linux x86-64，`GOAMD64=v1` |
| `mihomo-dns-linux-arm64-<version>.gz` | Linux ARM64 |

两种产物均启用 `with_gvisor`、关闭 CGO。文件名沿用现有仓库发布格式。

```bash
chmod +x ./dns-route-kernel
./dns-route-kernel -t -f config.yaml
./dns-route-kernel -f config.yaml
```

下面是可独立校验的配置。DNS 地址为数值地址示例，请按网络可达性调整。`代理出口` 暂只含 DIRECT 占位项，**加入自己的节点后才会真正走代理**；接入已有配置时直接使用原来的策略组即可。

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
  nameserver: [tcp://8.8.8.8:53, tcp://8.8.4.4:53]

  # 可选：这里显式开启；不配置时测速和主动预取均关闭。
  speed-check-mode: [tcp:443, tcp:80, ping]
  speed-check-timeout: 1000
  speed-check-concurrency: 16
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

## 只有直连路径才做 IP 测速

这里优选的是 **DNS 回答中的目标 IP**，不是代理节点速度，也不是 DNS 服务器所在地。

- 仅内置 DNS 已选 DIRECT / Compatible、所选上游集合全部为自动普通 53 传输的 A / AAAA 查询参与；代理路径不参与。
- 显式 DNS 出口、非 53 / 加密上游等传输例外不做本地测速；有效缓存命中直接返回，不为这次命中重复探测。
- TCP 测握手耗时，ping 测 ICMP 往返。探测继承所选直连出口的接口 / mark，不重新匹配规则；ping 是否可用取决于平台与权限。
- 保留候选所属回答及 CNAME 链，不拼接不同服务器的回答；DNSSEC 等保护场景不改写。全部探测失败但已有有效回答时返回原回答。

测速不代表下载带宽，也不保证所有域名都更快。后台更新属于新的查询，满足条件时可以重新优选。

## 可选 DNS 设置

以下字段均放在原 `dns:` 下；顶层 `dns-rule-routing` 的默认值是 `false`。

| 测速字段 | 默认值 | 含义与单位 |
| --- | --- | --- |
| `speed-check-mode` | 空 / `[none]` | 关闭；可设 `tcp:端口`、`ping`；`none` 不能与其他项混用 |
| `speed-check-timeout` | 0 → 1000 | 毫秒；显式范围 1–5000，本轮上游收集和探测共用预算 |
| `speed-check-concurrency` | 0 → 16 | 最大 256；共享测速器的并发探测上限；每轮最多 256 个候选 IP |

| 缓存字段 | 默认值 | 含义与单位 |
| --- | --- | --- |
| `prefetch-domain` | `false` | 为近期活跃的真实 DNS 来源预取 |
| `serve-expired` | `true` | 允许返回过期缓存并触发有界后台更新 |
| `serve-expired-ttl` | 0 | 秒；0 不限过期年龄，保留上游缺省行为；604800 表示最多过期 7 天 |
| `serve-expired-reply-ttl` | 1 | 秒；过期回答交给客户端的 TTL，允许 0，不延长原记录过期时间 |

预取要求真实 DNS 入站具有有效来源，同一作用域 1 分钟至少 3 次前台请求、最近 1 分钟仍活跃，按剩余 TTL 约 80% 安排刷新。每个 resolver 最多 1024 个热点，全局最多 16 个后台任务。

来源端口也属于缓存身份；客户端频繁更换来源端口时，缓存复用和达到预取热度门槛的机会会减少。原有 `cache-algorithm` / `cache-max-size` 仍可使用，不承诺未经实测的命中率提升。

后台保留来源和已知 / 未知 PROCESS 快照，重新检查当前规则，不拿旧 socket 识别可能复用端口的新进程。清缓存、模式 / 开关变化、重载会使旧后台任务失效；缓存命中与并发合并也保留来源、子规则和实际出口边界。

## 生效条件与兼容边界

自动 QNAME 选路只在 **开关开启、Rule 模式、入站没有固定 `proxy:`** 时接管；固定出站和 Global / Direct 保留原优先级。入站 `rule:` 仍作为子规则入口，已经选定业务叶子的内部解析继承该叶子，bootstrap 独立执行。

- **不使用 FakeIP。** 内置 DNS 使用 `redir-host`；本功能与内置 FakeIP 同时启用会在校验时报错。不增加 listener 类型或专用 DNS 代理端口。
- **规则顺序不变。** IN / SRC / PROCESS、端口和网络属性仍可参与；QNAME 用于域名规则。网站目标 IP / GEOIP / ASN 尚未知，不拿解析器 IP 代替，也不为选路递归解析同一个名字。
- **自动范围是普通明文 TCP / UDP 53。** 支持常见单问题查询、EDNS 与 DNSSEC 数据传递，不验证 DNSSEC 签名；DoH / DoT / DoQ、非 53 和特殊查询保留各自原路径或明确报错。
- **显式 DNS 传输仍是例外。** `#Group` / `#DIRECT` / `#interface` 等保留旧行为；先选池，再判断该池的例外。混有显式上游时，显式分支可独立回答；未选池不会削弱自动分支的拒绝动作。
- **不另写 DNS 域名策略。** 开启时停用 `nameserver-policy`、`direct-nameserver-follow-policy`、`fallback-filter.domain/geosite`；`fallback` 的适用 IP 过滤、hosts、基础解析等保留。关闭开关并完整重载可恢复原策略。
- **respect-rules 的旧模式行为保留。** 非 Rule 模式不建立新计划；在自动范围内，QNAME 计划代替按解析器地址二次选路。

真实上游交换接入原生连接面板、规则链和流量统计，关闭连接可以中断交换；完成的短查询移出活动列表。普通 SSH 的反向域名显示不由本功能改写。完整限制见[使用说明](docs/dns-proxy.md)。

## 从旧配置迁移

1. 删除旧 `dns-proxy-port` 或专用 DNS listener 设置，添加顶层 `dns-rule-routing: true`；保留原 mixed / SOCKS / HTTP 入口。
2. 使用原生方案时开启 `dns.listen`，配置 direct / main 和独立 bootstrap，把业务分流集中到原 `rules`，不再要求 SmartDNS 前置处理。
3. 先执行 `-t` 校验，再加载完整配置。开关不支持通过 `PATCH /configs` 单独修改；`GET /configs` 可查看生效值，完整配置可通过原 `PUT /configs` 重载。

已有 SmartDNS 可以按[兼容接入模板](docs/smartdns-dns-proxy.conf)继续转发上游 53 查询，原 6053 / 6553 本地服务不自动改投；其独立缓存仍由 SmartDNS 管理。本项目不声称兼容全部 SmartDNS 功能。

## 云编译、更新与验证

[构建工作流](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/build.yml)先执行规定 Go 测试、DNS race 和同步保护测试，再构建 Linux amd64 / arm64；amd64 产物运行本地模拟服务的实际二进制端到端测试。arm64 为交叉编译，不等同于实际执行。

`main` 推送通过验证后自动发布带源提交标识和 SHA-256 的下载包；PR 只生成 Actions artifact。当前 `main` 对应构建才能更新 Latest，旧构建重跑不会覆盖新版本入口。

[稳定版同步](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/sync-upstream.yml)计划每日北京时间 04:23 检查上游正式 release，定时任务可能延迟。先合并、测试和编译，通过后普通推送；冲突、验证失败或远端分支并发变化时停止，不强推覆盖本项目修改。基线见 [UPSTREAM_VERSION](UPSTREAM_VERSION) / [UPSTREAM_COMMIT](UPSTREAM_COMMIT)。

本地验证使用项目当前工具链；CI 的 Go 版本与来源见工作流。常用命令：

```bash
bash scripts/ci-check.sh test
go build -tags with_gvisor -o dns-route-kernel
python3 scripts/test-dns-proxy.py ./dns-route-kernel
```

测试事实、ICMP / PROCESS 平台跳过项与验证范围见[实施与复查记录](docs/dns-rule-routing-validation.md)。发布成功以精确提交的 Actions / Releases 记录为准。

## 文档导航

| 文档 | 内容 |
| --- | --- |
| [配置与工作原理](docs/dns-proxy.md) | 字段、选池、测速、缓存和兼容边界 |
| [完整原生示例](docs/dns-proxy.example.yaml) | 无私人凭据、无远程 provider 的可校验模板 |
| [设计与验收约定](docs/dns-rule-routing-design.md) | 优先级、未知 IP、一次计划、生命周期 |
| [验证记录](docs/dns-rule-routing-validation.md) | 实测结果与明确限制 |
| [云编译与上游同步](docs/upstream-sync.md) | 下载、校验、发布、同步失败与预演 |
| [上游使用文档](https://wiki.metacubex.one/) | 原有节点、规则、TUN、API 等通用能力 |
| [上游 README](https://github.com/MetaCubeX/mihomo/blob/Alpha/README.md) | 原项目完整介绍和上游说明 |

## 上游、致谢与许可证

保留 MetaCubeX/mihomo 的 Git 历史，并感谢其维护者及贡献者。项目采用 [GPL-3.0](LICENSE)，派生修改不代表 MetaCubeX 官方行为或承诺。原有协议、规则、TUN、API 等能力来自上游；面板可使用 [metacubexd](https://github.com/MetaCubeX/metacubexd)。

保留上游致谢：[Dreamacro/clash](https://github.com/Dreamacro/clash)、[SagerNet/sing-box](https://github.com/SagerNet/sing-box)、[riobard/go-shadowsocks2](https://github.com/riobard/go-shadowsocks2)、[v2ray/v2ray-core](https://github.com/v2ray/v2ray-core)、[WireGuard/wireguard-go](https://github.com/WireGuard/wireguard-go)、[yaling888/clash-plus-pro](https://github.com/yaling888/clash)。

上游 README 另保留以下命名声明：

> In addition, any downstream projects not affiliated with `MetaCubeX` shall not contain the word `mihomo` in their names.
