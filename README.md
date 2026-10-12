# mihomo-optimized

基于 [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) 稳定版维护的独立派生内核，让 DNS 查询直接复用已有业务分流规则。**不需要 SmartDNS，也不需要再维护一份 DNS 域名策略。** 本项目不是 MetaCubeX 官方发行版。

**原生 DNS 上游支持 UDP、TCP、DoH、DoT、DoQ 和 DoH 的 HTTP/3。IP 测速与双栈优选只用于实际 DIRECT / Compatible 路径，走代理节点的 DNS 回答不测速。** 测速、双栈优选与主动预取默认关闭，可以按需开启。

[下载 Releases](https://github.com/chummumm/mihomo-optimized/releases) · [构建状态](https://github.com/chummumm/mihomo-optimized/actions/workflows/build.yml) · [完整配置说明](docs/dns-proxy.md) · [验证记录](docs/dns-rule-routing-validation.md)

**配置入口：** [完整配置示例](#完整配置示例) · [面板与内核自动更新](#面板与内核自动更新) · [DNS 可选字段](#可选-dns-设置) · [共享连接例外](#共享-dns-连接与单个上游例外) · [旧配置迁移](#旧配置迁移与停用范围)

## 解决什么问题

原生 DNS 需要真实解析时，用 Question 中的域名（QNAME）匹配已有 `rules` / `rule-providers`，选出实际出站，再选择 DNS 上游。hosts 与 FakeIP 的本地回答优先完成，不进入这一步：

| 实际选中的出站 | 内置 DNS 使用的上游 |
| --- | --- |
| DIRECT / Compatible | `direct-nameserver`；通常填写国内可达的解析器 |
| 代理节点 | `nameserver`，以及按原设置适用的 `fallback` |
| REJECT / REJECT-DROP | 进入真实解析的自动查询在读取缓存前拒绝 / 静默丢弃；显式传输例外见下文 |

判断依据是**策略组最终选择的实际出口类型**，组名和节点名不参与这个判断。组切到 DIRECT 就走直连池，选择代理节点就走代理池。`direct-nameserver` 为空时，保留原 `nameserver` / `fallback` 行为。

一次逻辑查询只选一次实际出口；上游重试、main / fallback 和 UDP 截断后的 TCP 重试沿用它。直连池失败不会偷偷借用代理池。缓存按解析器池、实际出口、来源等作用域隔离，切组不会混用另一个出口的回答。

DNS 分流通过内置解析器提供，普通代理连接保持原转发行为：

| 查询如何进入 | 解析器目的地址 |
| --- | --- |
| 直接发给 `dns.listen`，或由 TUN DNS 劫持进入本地服务 | 按上述规则选择 direct / main 池 |
| 经 mixed / SOCKS / HTTP 等入站，访问指定解析器的 TCP / UDP 53 | 作为普通代理流量转发，按连接目标和原规则选择出口，不读取 DNS Question 分流 |

普通代理入站不再自动识别和接管目标 53 的 DNS 报文。需要按查询域名选择 DNS 出口时，让客户端使用 `dns.listen`，或显式配置 TUN DNS 劫持；普通 SOCKS / HTTP 请求直接携带的目标域名仍可正常匹配业务规则。

## DNS 路径性能优化

UDP、TCP 和 DoT 上游在实际出口固定时共享连接，多个查询通过独立请求 ID 分发，不再每次普通查询都重新建连。单个查询超时或取消只取消自身；ID 在同一连接整个生命周期内不重复使用，旧回复不会串到后续查询。共享仍按出口、路由代次、接口和 mark 隔离；动态或无法冻结的出口保留独占方式。DoH/HTTP3/DoQ 继续使用原有多路复用。

开启 `dns-rule-routing` 时，真实 DNS 解析的规则执行使用派生视图和 classical 纯域名连续段索引，仍按原业务规则顺序判断 SRC / PROCESS / IN。成功的自动查询应答可以在同一来源、进程和入站作用域内跨临时源端口复用；每个进入真实解析的请求仍先匹配当前规则，在途查询保持独立取消语义。DIRECT 探测使用共享有界任务池、同出口目标 IP 合并及短期结果复用，代理结果不进行本地探测。

新增的 DNS 面板数据由本机控制器提供，查询记录、趋势和上游统计只保存在进程内存中，不写数据库或日志文件，也不发送遥测。无需维护第二份分流规则；观测不会改变 DNS 回答和出口选择。优化范围、缓存安全边界及测试方式见 [DNS 性能说明](docs/dns-performance.md)。

正式版本采用 **`mihomo-optimized-vX.Y.Z`**，当前为 `mihomo-optimized-v1.0.5`。tag、发行标题、`version.txt` 和运行内核版本一致；`PROJECT_VERSION` 记录独立项目版本，上游 mihomo 的版本与提交另存于 `UPSTREAM_VERSION` / `UPSTREAM_COMMIT`。源码哈希保留在 BUILDINFO 中。**旧版 `v1.19.32-optimized-N` 及更早内核首次切换到新系列，需要手动安装一次**；新系列随后可通过 YAML 启用内核后台检查和更新，详见[更新说明](docs/releases.md#内核自动更新)。

## DNS 状态与原版面板

本仓库的 [Clash Dashboard 分支](https://github.com/chummumm/mihomo-optimized/tree/clash-dashboard) 在原有面板上提供 DNS 概览、查询记录与上游统计，并保留嗅探域名显示兼容。连接页的“全部”与各 IP 从同一份保留记录计算数量；活跃连接完整保留，“保留关闭连接”最多保留最近 5000 条关闭记录。长表格按可见区域渲染，刷新时保持阅读位置。

连接页的 `DNS` 类型仅用于内核本机 resolver 发起的真实上游 DNS transport，由核心明确返回 `dns: true`；合成记录和客户端查询记录不携带这个标记。自内核 v10 起，普通 UDP / TCP 53 上游连接与原生共享传输一致，使用 `DNS-TRANSPORT` 入站标签和真实解析器地址，不再沿用客户端来源、查询域名或业务命中规则；选路、缓存及内存中的 DNS 查询记录继续保留真实来源。面板 0.3.2 无需更新即可显示修正后的信息，DNS 连接归入 `mihomo`，节点显示最终选择的出站；连接 API 仍保留完整节点链。面板同时提供移动端连接页、开始时间显示、重载入口和测速 URL 设置。

DNS 观测默认随 `dns.enable` 开启，可用 `dns.observability: false` 关闭。最多保留 4096 条查询明细，并受 8 MiB 记账预算和 24 小时保留期限约束；趋势使用固定分钟桶，上游统计最多保留 128 个独立身份，超出部分归入合计。普通完整重载保留数据，关闭观测会释放数据，重新开启或重启内核从零开始。面板不会将 DNS 数据写入浏览器本地存储。

自内核 `1.0.4` 配合面板 `0.4.6` 起，查询记录支持“第 N / M 页 · 共 K 条”、首页 / 上一页 / 下一页 / 末页和直接跳页。内核在同一保留窗口中返回筛选总数、历史上界和每页游标，前端只保存当前页与有界游标列表。新日志不混入正在浏览的历史，淘汰后总数和末页随之修正；旧内核保留前后翻页并明确显示总数未知。

查询次数、实际上游交换次数和连接数采用不同口径，不能直接相加；NXDOMAIN 是 DNS 回答，竞争中取消的上游交换单独统计。完整字段、内存边界、安装方式和验证方法见 [DNS 状态展示](docs/dns-observability.md)。

面板保留当前 YAML 文件编辑、保存冲突检测和未保存草稿保护；路径、下载 URL 和自动更新策略均不在设置页填写。自面板 `0.4.6` 配合内核 `1.0.4` 起，设置页提供一个“检查并更新”入口，真实更新当前配置中的规则集、GeoData、面板及内核，内核最后升级并重启。编辑器沿用原有 `external-controller` / `secret` 鉴权，不需要新增配置开关；草稿不写浏览器存储。日志过滤等级与手动节点测速 URL 是浏览器偏好，没有新增对应的 YAML 字段。

自面板 `0.4.2` 起，编辑器主按钮为**“保存并重新加载”**，Ctrl-S / Cmd-S 也执行同一操作：先保存当前文件，再完整校验并重载，使可热重载的配置生效。组合操作使用 `force=true`，包括更新 HTTP / SOCKS / mixed 端口、绑定地址及 `allow-lan`；控制器地址和 `secret` 等启动设置仍需重启内核。保存成功而重载失败时，会保留已保存内容和新修订标识，并明确提示重载未成功或结果无法确认，不自动回滚文件。详细行为见[配置编辑说明](docs/dashboard-management.md#编辑当前配置文件)。

面板 `0.4.7` 为重载请求设置 30 秒等待上限，超时后可以退出或重试，已保存文件继续保留；超时不代表内核一定没有完成重载。重载成功后的设置刷新在后台进行。节点、策略组、代理集、规则与内核版本按控制器实例隔离，切换控制器后不显示旧控制器的数据，也不接受其迟到响应。

自内核 `mihomo-optimized-v1.0.2` 配合面板 `0.4.4` 起，读取配置时返回能否安全保存的 `writable` 状态。可读但不可保存的文件直接以只读方式展示，仍可选择和复制；保存期间权限变化会保留草稿并明确提示。检测使用内核运行进程的权限，不修改文件属主或权限。手机编辑器全屏显示，打开及重新读取文件从开头显示。

面板来源固定为本仓库 `clash-dashboard-dist`，目录固定为内核工作目录下的 `./ui`（跟随 `-d`），可通过控制器地址的 `/ui/` 打开。路径、下载来源和旧配置的处理见[迁移表](#旧配置迁移与停用范围)。

## 统一检查并更新

设置页的“检查并更新”向内核发送空请求体的 **`POST /upgrade/check`**。内核立即启动实际检查，发现变化即更新；随后通过同一路径的 `GET` 读取任务进度。规则集和 GeoData 使用当前 YAML 配置的来源，UI 和内核使用本仓库固定来源。自动更新开关关闭时，仍可主动执行这一次更新。

面板为每次点击生成独立请求标识并绑定当前内核实例；浏览器重发同一次请求时复用标识，内核重启后拒绝旧实例的提交。内核保留最近 128 个已接受标识，响应丢失后面板只读取状态并核对请求身份，不把别的终端的任务当作本次执行成功。接口字段与兼容调用方式见[统一更新接口](docs/dashboard-management.md#统一检查并更新接口)。

| 类别 | 判断及处理 |
| --- | --- |
| HTTP rule-provider | 重新请求已配置来源；内容变化时校验、原子保存并替换生效规则；本地 / 内联规则显示跳过 |
| GeoData | 按当前规则、DNS 和已生效规则集的实际依赖选择数据库；内容变化时校验并替换，清理对应缓存或切换数据库读者 |
| 面板 | 真实复验远端版本，仅严格新版安装；受管 `./ui` 为空时可首次安装 |
| 内核 | 真实检查正式发行版本，仅严格新版升级；等待其他项目结束后执行，成功安装后重启 |

面板不直连 GitHub 查询，也不传入更新 URL 或路径。各项独立返回结果，一项失败不阻止其余检查；配置重载会取消旧配置尚未提交的任务。任务在内核运行，关闭浏览器不取消任务。内核重启会中断连接并重建内存 DNS 状态，面板尝试重连且不主动刷新编辑页面。接口、状态和故障语义见[统一更新接口](docs/dashboard-management.md#统一检查并更新接口)。

这个入口不新增 YAML 配置键，沿用已经生效的配置：

| 类别 | 来源及自动策略字段 |
| --- | --- |
| 规则集 | `rule-providers.<名称>.url` / `proxy` / `header`；自动周期为 `interval`（秒） |
| GeoData | `geox-url`；自动策略为 `geo-auto-update` / `geo-update-interval`（小时），仅处理实际引用的数据族 |
| 面板 | 来源和 `./ui` 固定；`external-ui-auto-update` / `external-ui-update-interval`（小时） |
| 内核 | 本仓库正式发行版；`core-auto-update` / `core-update-interval`（小时） |

## 面板与内核自动更新

这四项放在 YAML **顶层，与 `dns:` 同级**。示例显式开启两类自动更新；只需要其中一类时，将另一开关设为 `false`：

```yaml
external-ui-auto-update: true
external-ui-update-interval: 24
core-auto-update: true
core-update-interval: 24
```

| 顶层字段 | 默认值 | 作用 |
| --- | --- | --- |
| `external-ui-auto-update` | `false` | 自动检查并安装版本更高的面板 |
| `external-ui-update-interval` | `24` | 面板的检查间隔，单位为整数小时 |
| `core-auto-update` | `false` | 自动检查并安装版本更高的内核，成功后自动重启 |
| `core-update-interval` | `24` | 内核的检查间隔，单位为整数小时 |

两个间隔都只接受 **`1`–`2562047` 的整数小时**。写 `24`，不要写 `24h`、`"24"`、小数或 `0`；关闭检查请使用对应布尔开关。面板 `0.4.2` 的“保存并重新加载”会应用这四个字段；直接修改磁盘 YAML 或只调用保存接口后，仍需完整重载才改变运行中的策略。

从内核 **`1.0.1`** 起，调度行为如下，面板与内核分别遵守自己的开关和间隔：

| 发生的操作 | 已开启的自动更新如何执行 |
| --- | --- |
| 进程启动或重启 | 网络与配置应用完成后，立即异步检查一次，之后按间隔检查 |
| 运行中从 `false` 改为 `true` 并完整重载 | 立即异步检查一次，之后按间隔检查 |
| 使用相同开关、间隔完整重载 | 沿用原定时器，不重复立即检查，也不重置下次检查时间 |
| 保持开启，仅修改间隔并完整重载 | 取消旧任务，从该次重载按新间隔计时，不追加一次立即检查 |
| 改为 `false` 并完整重载 | 取消该类旧任务，停止后续自动检查和安装 |

自动安装以**版本号严格变大**为条件，不以文件变化或摘要变化代替版本判断。面板比较远端与本地 `build-info.json` 中的版本；内核比较当前实际版本与本仓库正式 release 版本。版本相同、远端较旧或无法可靠比较时均跳过；下载校验值用于验证文件完整性。面板安装不重启内核，内核安装完成会自动重启。

**首次面板安装单独处理：** 受管 `./ui` 目录不存在或为空时，内核会自动下载安装初始面板，即使 `external-ui-auto-update: false`；目录已有内容后，该开关才控制是否自动升级。启动检查、初始安装和后续检查由内核协调，不需要打开浏览器触发。配置检查命令 `mihomo -t` 只校验，不启动这些任务。完整行为、故障处理与版本边界见[面板及内核维护说明](docs/dashboard-management.md)。

### GeoData 自动更新

GeoData 沿用以下顶层字段和当前 `geox-url` 来源，只检查当前规则、DNS 与规则集实际使用的数据库：

```yaml
geo-auto-update: true
geo-update-interval: 24 # 整数小时，默认 24，范围 1..2562047
```

从内核 `1.0.5` 起，完整重载会实际应用 GeoData 自动更新的开关和间隔：关闭会取消旧自动任务，开启会根据已使用数据库的更新时间决定是否立即检查；相同策略保留原计时，修改间隔后按新间隔计时。启动、启用时发现缺失或过期的数据会尝试更新，首次检查失败仍保留后续定期检查。间隔使用与面板、内核一致的整数小时范围，停止更新使用 `geo-auto-update: false`。具体行为见 [GeoData 自动更新调度](docs/dashboard-management.md#geodata-自动更新调度)。

Linux 的 TCP / UDP 独立接管、事件驱动路由恢复与可配置标记 / 路由表另有[透明代理托管方案](docs/transparent-proxy-plan.md)。**这是设计文档，尚未实现；当前版本不要添加其中的 `transparent-proxy` 字段，也不能依靠它恢复手写路由。**

## 快速开始

1. 从 [Releases](https://github.com/chummumm/mihomo-optimized/releases) 下载对应系统和 CPU 的文件，核对 `SHA256SUMS`；直接运行可选 `.gz` / Windows `.zip`，Linux 也提供安装包。
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

Debian / Ubuntu 例如使用 `mihomo-optimized-linux-amd64.deb`：

```bash
sudo apt install ./mihomo-optimized-linux-amd64.deb
# 将自己的配置放入 /etc/mihomo/config.yaml 后再校验和启动。
sudo mihomo -t -d /etc/mihomo
sudo systemctl daemon-reload
sudo systemctl enable --now mihomo
```

安装包使用 `/usr/bin/mihomo`、`/etc/mihomo/config.yaml` 和 `mihomo.service`，通过包管理器保护已有配置，不自动启动服务。包名改为 `mihomo-optimized`，声明替换旧 `mihomo-dns-optimized` 和官方 `mihomo`。deb/rpm/pkg 下载文件名固定、不带版本；包内部保留合法版本元数据。安装后可由内核自更新，无需每次重新安装包；系统包管理器记录的仍是当初安装包版本，当前实际版本请看 `mihomo -v`。通用起始配置只有本机 mixed 和 DIRECT；私人节点与规则需自行提供。

直接使用压缩核心时，解压并命名为 `mihomo-optimized` 后运行：

```bash
chmod +x ./mihomo-optimized
./mihomo-optimized -t -f config.yaml
./mihomo-optimized -f config.yaml
```

### 完整配置示例

下面列全本项目新增或调整的主要可配置项，并包含运行所需的基础设置。此例显式开启自动更新、DIRECT 测速、双栈优选与预取；它们的默认值及关闭方式见后面的字段表。直连池使用普通 DNS，代理池使用 Cloudflare DoH，请按网络可达性调整。`代理出口` 暂只含 DIRECT 占位项，**加入自己的节点后才会真正走代理**；接入已有配置时保留原节点和策略组，将字段并入现有 YAML，不要重复新增第二个 `dns:` 块。

```yaml
mode: rule
mixed-port: 7890
allow-lan: false
ipv6: true  # 双栈优选同时需要此项与 dns.ipv6 为 true。
external-controller: 127.0.0.1:9090
secret: "replace-with-your-controller-secret"
dns-rule-routing: true

# YAML 顶层：此例显式开启；默认均为 false，间隔单位为整数小时。
external-ui-auto-update: true
external-ui-update-interval: 24
core-auto-update: true
core-update-interval: 24

dns:
  enable: true
  observability: true  # false 关闭并释放 DNS 观测数据。
  listen: 127.0.0.1:1053
  enhanced-mode: redir-host
  ipv6: true
  respect-rules: false

  # 只排除真实 IP 到历史域名的反查映射，不屏蔽 DNS 回答或业务连接。
  redir-host-filter:
    - time.example.org
    - +.example.net

  # 节点和解析器自身域名的独立基础解析，避免启动循环。
  default-nameserver: [223.5.5.5, 119.29.29.29]
  proxy-server-nameserver: [223.5.5.5, 119.29.29.29]
  direct-nameserver: [223.5.5.5, 119.29.29.29]
  nameserver:
    - https://cloudflare-dns.com/dns-query
    - https://1.1.1.1/dns-query

  # 可选：这里显式开启；仅实际 DIRECT 优选 IP 与双栈。
  speed-check-mode: [tcp:443, tcp:80, ping]
  response-mode: first-ping
  speed-check-timeout: 1000
  speed-check-concurrency: 16
  dualstack-ip-selection: true
  dualstack-ip-selection-threshold: 10
  dualstack-ip-allow-force-aaaa: false

  # 缓存和预取；以下 TTL 均为秒。
  prefetch-domain: true
  serve-expired: true
  serve-expired-ttl: 604800
  serve-expired-reply-ttl: 1

  # 可选回答调整；此处保留默认关闭。需要时可将 CNAME 项设为 true。
  force-no-cname: false
  rr-ttl-min: 0  # 0 不设下限，例如 60；零 TTL 和保护场景仍保留。
  rr-ttl-max: 0  # 0 不设上限，例如 86400；非零时不得小于 rr-ttl-min。

proxy-groups:
  - name: 代理出口
    type: select
    proxies: [DIRECT]  # 添加你自己的节点名称。

rules:
  - DOMAIN-SUFFIX,example.com,DIRECT
  - DOMAIN-SUFFIX,example.net,代理出口
  - MATCH,DIRECT
```

示例监听本机 DNS `1053`、mixed `7890` 和控制器 `9090`；替换 `secret` 为自己的控制器密钥后，可访问 `http://127.0.0.1:9090/ui/`。需要标准 DNS 端口或其他设备接入时，调整监听与原有访问范围配置。另有[包含文档代理节点的完整示例](docs/dns-proxy.example.yaml)，其中保留地址须替换后才能实际联网。

`enhanced-mode: redir-host` 返回真实 IP，并保留 IP 到历史查询域名的映射，供只有 IP 的连接辅助匹配。若不需要这层反查，可改为 `enhanced-mode: normal`；正常 DNS 解析、正向回答缓存和本项目的原生优化仍保留。透明代理若拿不到域名，需要依赖嗅探或其他规则；模式区别与可选 TLS / HTTP 全端口嗅探示例见[使用说明](docs/dns-proxy.md#enhanced-mode-与域名嗅探)。

### FakeIP 与 DNS 分流同时使用

从内核 `mihomo-optimized-v1.0.3` 起，可同时开启 FakeIP 和 DNS 分流。将下面字段并入自己的配置，保留已有节点、规则与其他 DNS 设置：

```yaml
dns-rule-routing: true
dns:
  enable: true
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  fake-ip-filter-mode: blacklist
  fake-ip-filter:
    - '+.lan'
    - time.example.org
  fake-ip-ttl: 1
  default-nameserver: [223.5.5.5, 119.29.29.29]
  direct-nameserver: [223.5.5.5, 119.29.29.29]
  nameserver: [https://cloudflare-dns.com/dns-query]
```

处理顺序沿用 **hosts → FakeIP → 真实 DNS 解析**。应该返回虚拟地址的查询直接合成，使用 `fake-ip-ttl`，不会为了 DNS 分流先查询真实 IP，也不会参与真实答案缓存、IP 测速或预取。`fake-ip-filter` 要求真实解析的域名，以及 TXT / MX 等未由 FakeIP 本地处理的记录，继续按已有规则选 DNS 上游和出口。

FakeIP 的 IPv6 与 HTTPS / SVCB 处理也保持原行为：应使用虚拟地址的 AAAA 查询在 IPv6 未启用或没有 FakeIPv6 地址池时返回空答案；未被过滤的 HTTPS / SVCB 返回空答案。FakeIP 已合成的 DNS 回答不执行 DNS 阶段的 REJECT，后续访问虚拟地址时仍恢复域名并按业务规则拒绝连接；要求真实解析的查询仍可在 DNS 阶段拒绝。具体组合见[FakeIP 优先级说明](docs/dns-proxy.md#fakeip-与真实查询的优先级)。

## 只有直连路径才做 IP 测速

这里优选的是 **DNS 回答中的目标 IP**，不是代理节点速度，也不是 DNS 服务器所在地。

- 仅内置 DNS 已选 DIRECT / Compatible、所选上游集合全部支持自动传输的 A / AAAA 查询参与；代理路径不参与。原生 DoH / DoT / DoQ 及自定义端口同样按实际出口判定。
- 显式 DNS 出口等传输例外不做本地测速；有效缓存命中直接返回，不为这次命中重复探测。
- TCP 测握手耗时，ping 测 ICMP 往返。探测继承所选直连出口的接口 / mark，不重新匹配规则；ping 是否可用取决于平台与权限。
- 保留候选所属回答及 CNAME 链，不拼接不同服务器的回答；DNSSEC 等保护场景不改写。全部探测失败但已有有效回答时返回原回答。

测速不代表下载带宽，也不保证所有域名都更快。预取和过期缓存刷新属于新的查询，满足条件时可以重新优选。

开启测速后，可用 `response-mode` 选择首次回答的时机。默认 `first-ping` 在候选 IP 首次探测成功后回答；`fastest-ip` 在本轮预算内充分比较候选；`fastest-response` 在首个通过当前策略检查的 DNS 回答到达时回答，不等待 IP 探测。提前回答后，剩余比较仅在有后台名额时继续，沿用本轮期限，结果可以更新缓存。缓存命中直接回答，代理路径和关闭测速的配置保持原解析行为。具体边界见[响应模式](docs/dns-proxy.md#响应模式)。

可选双栈优选复用这次已选直连出口和同一个超时预算。默认保留 IPv4；当 AAAA 查询同时取得两族可测速的回答，且 IPv4 比 IPv6 快至少所设阈值时，返回零 TTL 的 NOERROR / NODATA。代理查询不另查另一族、不探测；测速失败或受 DNSSEC 保护时保留原回答。该临时优选结论不缓存，避免被过期缓存长期延续。

## 可选 DNS 设置

以下字段均放在原 `dns:` 下；顶层 `dns-rule-routing` 的默认值是 `false`。

| 观测与映射字段 | 默认值 | 生效条件与关闭方式 |
| --- | --- | --- |
| `observability` | `true` | 随 `dns.enable` 收集纯内存数据；设 `false` 并完整重载后停止收集并释放数据 |
| `redir-host-filter` | `[]` | 仅在 `enhanced-mode: redir-host` 下排除反查映射；空列表不排除域名，完整重载生效 |

`redir-host-filter: []` 可在 `enhanced-mode: redir-host` 下排除指定域名的真实 IP 反查映射，支持精确域名和 `+.example.com` 等域名通配符。正常 DNS 回答、入站明确携带的域名和嗅探域名不受影响。名单使用已有域名索引，完整重载后新名单约束也适用于旧映射；详见[映射黑名单](docs/dns-proxy.md#redir-host-映射黑名单)。

| 测速字段 | 默认值 | 含义与单位 |
| --- | --- | --- |
| `speed-check-mode` | 空 / `[none]` | 关闭；可设 `tcp:端口`、`ping`；`none` 不能与其他项混用 |
| `response-mode` | `first-ping` | 开启测速时的首次回答策略；可选 `first-ping`、`fastest-ip`、`fastest-response` |
| `speed-check-timeout` | 0 → 1000 | 毫秒；显式范围 1–5000，只限制本轮可选优选等待；无有效答复时，已发出的 DNS 继续使用正常解析期限 |
| `speed-check-concurrency` | 0 → 16 | 最大 256；只限制活动 IP 探测，满额立即跳过新探测，不排队，不限制 DNS 上游请求；每轮最多 256 个候选 IP |
| `dualstack-ip-selection` | `false` | 开启 DIRECT 双栈优选；仍需启用测速 |
| `dualstack-ip-selection-threshold` | 10 | 毫秒；0–1000，另一族至少快多少才过滤当前族 |
| `dualstack-ip-allow-force-aaaa` | `false` | 是否允许反向过滤 A、只保留更快的 IPv6；默认保留 IPv4 |

关闭测速可用 `speed-check-mode: [none]`；双栈优选还要求全局 `ipv6` 和 `dns.ipv6` 同时开启，任一 IPv6 开关关闭时不会进行双栈筛选。只想停止双栈优选可设 `dualstack-ip-selection: false`，保留原单族测速。

| 缓存字段 | 默认值 | 含义与单位 |
| --- | --- | --- |
| `prefetch-domain` | `false` | 为近期活跃的真实 DNS 来源预取 |
| `serve-expired` | `true` | 允许返回过期缓存并触发有界后台更新 |
| `serve-expired-ttl` | 0 | 秒；0 不限过期年龄，保留上游缺省行为；604800 表示最多过期 7 天 |
| `serve-expired-reply-ttl` | 1 | 秒；过期回答交给客户端的 TTL，允许 0，不延长原记录过期时间 |

`prefetch-domain: false` 关闭主动预取，`serve-expired: false` 停止返回过期缓存；正常未过期缓存仍可使用。这两个开关互不替代。

预取要求真实 DNS 入站具有有效来源，同一作用域 1 分钟至少 3 次前台请求、最近 1 分钟仍活跃，按剩余 TTL 约 80% 安排刷新。每个 resolver 最多 1024 个热点，全局最多 16 个后台任务。

满足自动选路条件的正式答案缓存和预取热度可跨临时来源端口复用；在途查询合并仍按来源端口隔离。原有 `cache-algorithm` / `cache-max-size` 仍可使用，不承诺未经实测的命中率提升。

后台保留来源和已知 / 未知 PROCESS 快照，重新检查当前规则，不拿旧 socket 识别可能复用端口的新进程。清缓存、模式 / 开关变化、重载会使旧后台任务失效；缓存命中与并发合并也保留来源、子规则和实际出口边界。

| 回答调整字段 | 默认值 | 行为 |
| --- | --- | --- |
| `force-no-cname` | `false` | 将完整、无歧义的未签名 A / AAAA CNAME 链压平到查询名；不会补造 IP 或重新分流 CNAME |
| `rr-ttl-min` | 0 | 秒；0 不设下限，保留零 TTL 的不可缓存回答 |
| `rr-ttl-max` | 0 | 秒；0 不设上限，限制正常回答及其缓存寿命 |

回答调整在写缓存前完成；过期回答仍使用 `serve-expired-reply-ttl`，不会被 TTL 下限延长。CNAME 类型查询、不完整 / 歧义链、DNSSEC 等保护场景保留原回答。节点和解析器 bootstrap 不继承这些业务回答调整。

`rr-ttl-min` / `rr-ttl-max` 接受 `0`–`4294967295` 秒；非零上限不能小于下限。例如把完整示例中的两项改为 `60` 和 `86400`，将适用正常回答的 TTL 约束在一分钟至一天，原本不可缓存的零 TTL 答案仍不被延长。`force-no-cname: true` 单独启用 CNAME 压平；不需要时保持 `false`，TTL 约束仍可独立使用。

### 共享 DNS 连接与单个上游例外

满足安全复用条件的 UDP、TCP 和 DoT 上游**默认共享连接**，不需要新增全局开关。普通 UDP / TCP 53、自定义端口和基础解析路径都包含在支持范围内；实际出口、接口、mark 和配置代次仍隔离，不能可靠冻结的动态出口、TSIG 等非普通查询保留独占方式。UDP 使用共享 socket，TCP / DoT 使用持久连接并允许多个查询同时在途；单个请求取消不会关闭兄弟查询所用的连接。

某个上游需要停止复用时，在**那个上游地址**后追加 `#disable-reuse=true`。下面分别展示 UDP、TCP 和 DoT 的写法，可独立校验；并入已有配置时只替换需要调整的对应上游，`default-nameserver`、`proxy-server-nameserver`、`direct-nameserver`、`nameserver` 和 `fallback` 的各个地址分别生效：

```yaml
dns:
  enable: true
  listen: 127.0.0.1:1053
  enhanced-mode: normal
  default-nameserver: [223.5.5.5]
  nameserver:
    - "udp://223.5.5.5:53#disable-reuse=true"
    - "tcp://119.29.29.29:53#disable-reuse=true"
    - "tls://dns.alidns.com:853#disable-reuse=true"
```

该参数只关闭被标注 UDP / TCP / DoT 上游的连接复用，不关闭 DNS 回答缓存、QNAME 分流、测速或观测，也不为上游指定固定出口。删除该参数即可恢复默认复用；DoH 的 HTTP/2 / HTTP/3 以及 DoQ 保留原有复用机制，不受这个参数控制。共享池的并发、等待、连接和空闲回收上限由内核固定管理，**没有新增池大小、每连接并发数或超时的 YAML 键**；资源边界与取消处理见 [DNS 性能说明](docs/dns-performance.md)。

## 旧配置迁移与停用范围

下面只列本项目功能对应的旧字段和行为。**永久忽略和条件停用是不同处理**，不应把它们一并删除或当成全局失效：

| 旧字段或设置 | 当前处理 | 迁移方式 / 恢复条件 |
| --- | --- | --- |
| 顶层 `external-ui`、`external-ui-url`、`external-ui-name` | 始终忽略；关闭自动更新也不会恢复旧目录或来源 | 面板固定使用工作目录的 `ui` 和本仓库 `clash-dashboard-dist`；工作目录由 `-d` 指定 |
| 命令行 `-ext-ui`，含其旧环境变量 `CLASH_OVERRIDE_EXTERNAL_UI_DIR` | 接受旧入口但忽略路径值 | 改用 `-d` 选择内核工作目录，面板始终放在其中的 `ui` |
| 旧浏览器保存的面板 / 内核自动更新偏好 | 自面板 `0.4.0` 起不再生效，更新控件与浏览器更新计时器已移除 | 将策略迁移为上面的四个 YAML 顶层字段；在 YAML 编辑器“保存并重新加载”，或手工修改文件后完整重载 |
| `dns.nameserver-policy` | 仅在 `dns-rule-routing: true` 时停用 | 改用原业务 `rules`；设 `dns-rule-routing: false` 并完整重载后，原 DNS 策略恢复 |
| `dns.direct-nameserver-follow-policy` | 仅在 `dns-rule-routing: true` 时停用 | 同上；未开启本项目路由时保留原行为 |
| `dns.fallback-filter.domain` / `dns.fallback-filter.geosite` | 仅在 `dns-rule-routing: true` 时停用对应域名筛选 | 同上；`fallback` 及仍适用的 IP 过滤保留 |

`default-nameserver`、`proxy-server-nameserver`、`proxy-server-nameserver-policy`、hosts、缓存以及可适用的 fallback IP 过滤没有被这张表中的域名策略停用一并移除。`respect-rules` 也没有永久失效：自动 QNAME 计划覆盖其范围内的按解析器地址二次选路，其他原有模式保持原行为。

## 生效条件与兼容边界

自动 QNAME 选路只在 **开关开启、Rule 模式、入站没有固定 `proxy:`** 时接管；固定出站和 Global / Direct 保留原优先级。入站 `rule:` 仍作为子规则入口，已经选定业务叶子的内部解析继承该叶子，bootstrap 独立执行。

- **FakeIP 与真实解析各自生效。** 内置 DNS 可使用 `normal`、`redir-host` 或 `fake-ip`。hosts 和 FakeIP 本地回答优先；需要真实查询时才进入 DNS 分流。监听仍使用 `dns.listen` 或显式 TUN DNS 劫持。
- **规则顺序不变。** IN / SRC / PROCESS、端口和网络属性仍可参与；QNAME 用于域名规则。网站目标 IP / GEOIP / ASN 尚未知，不拿解析器 IP 代替，也不为选路递归解析同一个名字。
- **按 QNAME 分流的入口是内置 DNS。** 内置 DNS 已知 QNAME，配置的 UDP / TCP 任意端口及 DoH / DoT / DoQ / H3 均可自动选池和选出口。普通代理入站不自动接管明文 DNS，也不解密客户端 DoH；显式 TUN DNS 劫持仍可将请求送入内置 DNS。
- **显式 DNS 传输仍是例外。** `#Group` / `#DIRECT` / `#interface` 等保留旧行为；先选池，再判断该池的例外。混有显式上游时，显式分支可独立回答；未选池不会削弱自动分支的拒绝动作。
- **不另写 DNS 域名策略。** 开启时停用 `nameserver-policy`、`direct-nameserver-follow-policy`、`fallback-filter.domain/geosite`；`fallback` 的适用 IP 过滤、hosts、基础解析等保留。关闭开关并完整重载可恢复原策略。
- **respect-rules 的旧模式行为保留。** 非 Rule 模式不建立新计划；在自动范围内，QNAME 计划代替按解析器地址二次选路。

可按路由作用域复用的原生上游使用冻结的出口身份，切换叶子后使用相应连接池；每个配置上游最多保留 64 个路由作用域客户端，只淘汰空闲项，重载会取消并关闭旧池。这里的 `64` 是作用域客户端数，**不是全局 socket 数量上限**。QNAME 和来源端口不进入连接池键；回答缓存和在途查询仍采用各自更严格的作用域，避免连接共享变成答案串用。

面板按次展示 QNAME、来源、规则和策略链，底层长连接标记为 `DNS-TRANSPORT`、显示真实解析器，不沿用首个域名冒充后续请求。逻辑查询计数展示 DNS 负载，底层只统计一次真实线路流量；关闭一条逻辑查询不关闭共享连接上的其他查询。普通 SSH 的反向域名显示不由本功能改写。完整限制见[使用说明](docs/dns-proxy.md)。

## 云编译、更新与验证

[构建工作流](https://github.com/chummumm/mihomo-optimized/actions/workflows/build.yml)先执行规定 Go 测试、DNS race、同步保护与安装包检查，再构建完整 37 目标矩阵；Linux amd64 产物运行本地模拟服务的实际二进制端到端测试。其他目标为交叉编译，不等同于在各设备实际执行。所有必需归档和安装包齐全、摘要一致后才发布。

`main` 的源码、依赖或构建配置变更通过验证后自动发布带源提交标识和 SHA-256 的下载包；PR 只生成 Actions artifact。纯文档更新跳过自动构建和发布，具体路径与显式发布入口见[发行文档](docs/releases.md)。当前 `main` 对应构建才能更新 Latest，旧构建重跑不会覆盖新版本入口。

[稳定版同步](https://github.com/chummumm/mihomo-optimized/actions/workflows/sync-upstream.yml)计划每日北京时间 04:23 检查上游正式 release，定时任务可能延迟。先合并、测试和编译，通过后普通推送；冲突、验证失败或远端分支并发变化时停止，不强推覆盖本项目修改。基线见 [UPSTREAM_VERSION](UPSTREAM_VERSION) / [UPSTREAM_COMMIT](UPSTREAM_COMMIT)。

本地验证使用项目当前工具链；CI 的 Go 版本与来源见工作流。常用命令：

```bash
bash scripts/ci-check.sh test
go build -tags with_gvisor -o mihomo-optimized
python3 scripts/test-dns-proxy.py ./mihomo-optimized
python3 scripts/test-dns-observability.py ./mihomo-optimized
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
