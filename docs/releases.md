# 预编译文件、安装包与内核更新

项目使用独立版本 **`mihomo-optimized-vX.Y.Z`**，首版为 `mihomo-optimized-v1.0.0`。`main` 的源码、依赖或构建配置变更在全部测试和完整编译成功后生成固定提交的 GitHub Release。Latest 仅在该构建仍对应当前 `main` 时更新；PR 只做合并前检查，不生成正式发布包。纯文档变更不消耗版本号，也不触发自动发布。

## 1.0.11：SmartDNS 多地址与配置对齐、DIRECT 固定目标、发布构建去重

DIRECT DNS 优选不再固定缩为一个 IPv4 / IPv6。按 SmartDNS 当前源码的接近最快条件筛选，并将完整 CNAME 路径一致的多个上游答案合并、去重、按 RTT 排序；默认最多返回 8 个，新增 `max-reply-ip-num` 可设置上限。不同 CNAME 路径、受保护的 DNSSEC 数据不混合；重复地址和链采用保守 TTL，已有缓存隔离和 256 候选探测上限继续生效。`first-ping` 的首次答复可能只有一个，后台完成及缓存可以含多个；`fastest-ip` 返回本轮比较出的多个合格地址。数量设置只作用于 DIRECT 优选路径，不保证每次凑满。

新增 `rr-ttl`、`rr-ttl-reply-max`、`force-AAAA-SOA`、`force-qtype-SOA`。固定 `rr-ttl` 优先于 min/max；回复上限只降低客户端 TTL，不缩短内部缓存。类型过滤返回本地 NOERROR/NODATA 与 SOA；qtype 支持数字、范围、逗号 / 空格分隔、YAML 行内 / 块列表，以及清空或清除指定项。业务 bootstrap 不继承这些选项。详细语义、优先级和例子见 [DNS 回答设置](dns-proxy.md#cname-与-ttl-调整)与 [SmartDNS 对照](smartdns-migration.md)。

**配置名称变更：严格采用 SmartDNS 大小写，不保留小写别名。** 正式名称为 `force-no-CNAME`、`force-AAAA-SOA`、`force-qtype-SOA`、`dualstack-ip-allow-force-AAAA`。旧配置中的小写或其他大小写变体会明确报配置错误，需改成正式名称；程序不会静默忽略，也不会自动重写用户配置。升级前可用 `mihomo -t` 校验目标版本配置。

DIRECT / Compatible 尊重客户端已指定的真实目标 IP，包括后来被 redir-host、hosts 或嗅探改写的情况；不再按域名重新解析、随机换址。域名仍用于域名规则，IP 规则复用原真实 IP，其他代理保留既有域名连接语义。域名型 SOCKS / HTTP 及 FakeIP 恢复出的域名仍需真实解析；此时启用 DNS 优选的 DIRECT 优先使用排序结果，TCP 保留有界回退，UDP 保持所选地址及 NAT 映射。内部 DNS 上游与测速探针各自使用正确的目标。代理组 `url-test` / 节点健康检查继续运行。

本轮只发布内核；面板继续使用已发布的 `0.4.8`。`1.0.10` 的统计连续性、裸内核更新保护和四类浏览器本地更新时间全部保留。

### PR 与合并后不再重复生成全套发布包

| 阶段 | 构建检查 | 正式归档 / 安装包 |
| --- | --- | --- |
| 所有 PR，包括工作流、平台、依赖变更 | 功能 / race / 网络 / systemd / 原生更新器检查，加 7 个代表目标的 `Compile check` | 不生成、不上传 |
| 合并到 `main` | 全部门禁，加 37 个目标的 `Release` 构建 | 生成一次 |
| 同次运行的发布任务 | 核对版本、源码提交、69 附件及摘要 | 直接使用上一步产物，不重新编译 |

7 个代表目标为 Linux amd64 / arm64 / 386、Windows amd64、macOS arm64、FreeBSD amd64、Android arm64-v8。纯编译检查使用相同平台工具链，校验二进制构建信息后清理临时文件；Linux amd64 还运行实际二进制 DNS 回归。PR 测试和这些代表目标仍需要编译；取消的是此前高风险 PR 的完整 37 目标发布打包和合并后再次打包的重复。

独立 tag、手动、上游同步或显式 ref 的发布入口仍使用完整矩阵。不能可靠复用不同提交的 PR 包来冒充最终源码。较少见平台的错误可能在合并后的完整矩阵中发现，但任一必需目标或验证失败都会阻止发布。

## 1.0.10：连续的 DNS 上游统计、裸内核更新保护与四类更新时间

本版本修复普通完整重载后，同一实际出口与 DNS 上游出现多行统计的问题。统计身份使用完整节点配置和 provider 来源的稳定摘要，不再随适配器对象重建或路由代次变化而拆分；重载前开始、重载后结束的交换继续正确累计。节点配置、实际接口或 mark 变化仍分别统计，未知出口不会归为 DIRECT。该身份只用于观测，DNS 缓存和连接池继续保留自己的隔离边界。详见 [DNS 上游统计](dns-observability.md)。

裸内核更新器增加以下保护，适用于直接运行二进制的 Linux / OpenWrt 等环境：

- 先检查并比较正式版本，再准备替换当前可执行文件。等版、旧版或无法可靠比较时，不再因为运行中的原文件已被移走而把版本检查报告为安装失败。
- Unix 新文件保留原有读写权限，并确保文件所有者可执行，避免继承丢失执行位的模式后无法重启。旧文件及备份权限保持原样。
- 替换和重启前运行候选内核的 `-v`，验证预期版本及操作系统 / 架构；预检有超时和输出上限。候选文件无法启动或报告不匹配时，当前内核继续运行。
- 解包后及时删除压缩包；Unix 备份优先使用硬链接，不支持时回退为经过校验的复制，减少低存储设备更新时的额外空间占用。安装前分别复核当前文件和备份的身份与内容。

SHA256、固定发行标签、架构 / 浮点 ABI 选择及既有重启事务继续生效。预检只执行版本命令，不加载用户配置。只读目录、空间不足或禁止执行的挂载仍会明确失败；本次修复不会改变目录权限或挂载设置。**已经卡在旧更新器路径或权限问题的设备，首次进入 `1.0.10` 可能仍需手动替换一次二进制**，因为运行中的旧内核尚未包含新更新逻辑。

配合面板 `0.4.8`，设置页为**内核、面板、GeoData、规则订阅四类**自动更新显示下次检查时间。GeoData 和规则订阅展开后显示每项计划，折叠时显示本类最近的计划；失败退避显示实际重试时间。日期、时间和时区来自浏览器，内核提供实际计时器的剩余时长，因此不会直接使用设备上可能错误的日历。请求耗时会消耗剩余量，显示可能保守地提前至多一次请求往返时间。关闭自动更新、本地资源和没有实际运行计时器的项目不会虚构时间。详见 [自动更新时间](dashboard-management.md)。

页面前台通过只读请求刷新调度状态，不会因为显示时间而开始安装。这里显示的是下一次自动**检查**的计划，只有检查发现变化后才执行更新。相同策略重载时 core / UI / GeoData 原定时器继续保留，规则订阅显示自己的真实定时器及失败退避。本版本不增加 YAML 配置字段。

## 1.0.9：修复接口重建时的 sysctl 瞬态错误

`mihomo-optimized-v1.0.9` 修复接口快照与 sysctl 读写之间的竞态：`utun` 等接口在快照后消失或重建时，其 `forwarding`、`rp_filter` 等逐接口项可能已经不存在。即使 `ENOENT` 被路径和操作上下文包装，也按接口瞬态缺项处理，覆盖 IPv4 / IPv6 转发与反向路径过滤的同类读写窗口，避免误判为持续的配置故障。

这项修复保留已准备的黑洞保护；后续网络事件和完整快照继续按真实接口索引及后端状态收敛，重建接口使用自己的原值。权限不足、只读文件系统、全局 sysctl 读写失败等真实故障继续报错并保持保护，不因忽略逐接口 `ENOENT` 而放行。

观察或核对故障后的首次完整应用成功会输出一次 INFO 级别的 `Network synchronization restored` 日志，并列出 TCP / UDP 各地址族实际提交的 `ready`、`protected` 或 `disabled` 状态。核对恢复时，部分后端仍可能处于保护中；`ready` 也不代表 WAN 连通性探测通过。重复 RA 和后续健康核对不会重复输出这条日志，详见[系统事件驱动](transparent-proxy-plan.md#系统事件驱动)。

## 1.0.8 的历史矩阵策略

`1.0.8` 曾把普通 PR 降为 7 个正式打包目标，但涉及平台、依赖、工作流或无法可靠判断差异时仍扩为 37 个，合并后还会完整构建。该策略已由上面的 `1.0.11` 检查 / 发布分离方案替代，所有 PR 均不再生成正式发布包。

## 1.0.6：Linux 透明代理与路由托管

`mihomo-optimized-v1.0.6` 新增默认关闭的 `transparent-proxy` YAML 配置。TCP、UDP 可分别选择 `tproxy`、`tun` 或 `"off"`；两种后端的 mark、路由表与策略优先级均可配置。本机出站与网关转发分别声明范围。健康状态依靠 Linux 的接口、地址、路由、策略规则与 nftables 事件更新，没有周期核对配置项。

路由使用显式且不重叠的优先级、完整 32 位 mark 匹配和常驻黑洞兜底。需要重建 TUN 时，在关闭旧设备前同步安装并读回验证保护；保护失败就保留旧后端。控制器重启和内核自更新会保留实际阻断规则与黑洞跨越进程替换，由新进程按 YAML 及工作资源接回。IPv4 / IPv6 后端按实际 socket、设备身份和地址分别判断就绪。

不创建状态目录、状态文件或额外的 nftables 恢复日志。运行时状态只在本进程内存中保存；正常 SIGTERM / `systemctl stop` 撤掉自有规则、集合、链、表、策略规则和路由，清理失败保留内存进度并重试。系统参数在正常停止或受控重启时按本次基线条件恢复；崩溃后的新进程不能追溯已经丢失的旧基线。

这是可选的新托管模式。启用时替代运行态的 `tun.auto-route`、`tun.auto-redirect` 和旧 `iptables` 自动接管，保留 YAML 原文。首次迁移需一次性移除原来手写的 nftables 接管表、systemd 路由增删命令与动态前缀 cron；模块不会接管或删除不属于自己的同名资源。配置示例、官方资料依据与 IPv6 边界见 [Linux 透明代理与路由托管](transparent-proxy-plan.md)。现有面板保持 `0.4.7`，无需新增开关。

该版本的 Linux CI 增加隔离网络命名空间回归：真实 nftables 安装/读回与工作资源恢复，以及正常 TUN、同步关闭、意外设备删除、单独删除查表规则和新设备恢复时的双栈报文验证。还会启动实际内核进程，验证直接 SIGTERM、控制器重启后的 SIGTERM，以及真正 `systemctl stop` 后无自有资源残留、外部对象保留且系统参数按本次基线恢复。测试环境缺少必要能力会失败，不能用跳过测试通过发布门槛。

## 文件名与固定下载入口

| 用途 | 文件名示例 |
| --- | --- |
| Linux 直接使用 / 内核自更新归档 | `mihomo-optimized-v1.0.0-linux-amd64.gz` |
| Windows 归档 | `mihomo-optimized-v1.0.0-windows-amd64.zip` |
| Debian 安装包 | `mihomo-optimized-linux-amd64.deb` |
| RPM 安装包 | `mihomo-optimized-linux-amd64.rpm` |
| Arch 安装包 | `mihomo-optimized-linux-amd64.pkg.tar.zst` |

Windows ZIP 内为 `mihomo-optimized-<target>.exe`。deb、rpm、pkg 的对外文件名固定，不带版本；例如最新 Debian amd64 包始终可从以下入口下载：

<https://github.com/chummumm/mihomo-optimized/releases/latest/download/mihomo-optimized-linux-amd64.deb>

每个 Release 仍保存该次构建的安装包；固定文件名不代表覆盖历史版本。内核更新下载带完整 tag 的 `.gz` / `.zip`，精确固定版本与 CPU 变体，不下载 native 安装包。

`SHA256SUMS` 覆盖全部 66 个二进制归档/安装包，以及 `BUILDINFO.json` 和 `version.txt`。BUILDINFO 记录完整源码提交、上游基线、Go 架构参数、工具链、CGO、编译标签及各文件大小和摘要。缺少任一必需文件、版本/提交不符或校验不一致时，发布失败。已发布 tag 和附件不覆盖；重跑也必须验证同一源码与同一文件集合。

## 支持矩阵

矩阵的机器可读来源是 [`packaging/targets.json`](../packaging/targets.json)，共 37 个平台/CPU 目标。下表中的名称用于文件名里的 `<target>`。

| 平台 | 目标 | 格式 |
| --- | --- | --- |
| Linux x86 | `linux-386`（SSE2）、`linux-386-softfloat` | `.gz`；SSE2 版另有 `.deb` / `.rpm` |
| Linux x86-64 | `linux-amd64`（v1）、`linux-amd64-v2`、`linux-amd64-v3` | `.gz` / `.deb` / `.rpm` / `.pkg.tar.zst` |
| Linux ARM | `linux-arm64`、`linux-armv5`、`linux-armv6`、`linux-armv7` | 全部 `.gz`；ARM64/v6/v7 另有 `.deb` / `.rpm`；ARM64/v7 另有 `.pkg.tar.zst` |
| Linux MIPS | `linux-mips-hardfloat`、`linux-mips-softfloat`、`linux-mipsle-hardfloat`、`linux-mipsle-softfloat`、`linux-mips64`、`linux-mips64le` | 全部 `.gz`；MIPS64LE 另有 `.deb` / `.rpm` |
| Linux 其他 CPU | `linux-loong64-abi2`、`linux-riscv64`、`linux-s390x`、`linux-ppc64le` | `.gz` / `.deb` / `.rpm` |
| Windows | `windows-386`、`windows-amd64`（v1）、`windows-amd64-v2`、`windows-amd64-v3`、`windows-arm64` | `.zip`，内含 `.exe` |
| macOS | `darwin-amd64`（v1）、`darwin-amd64-v2`、`darwin-amd64-v3`、`darwin-arm64` | `.gz` |
| FreeBSD | `freebsd-386`、`freebsd-amd64`（v1）、`freebsd-amd64-v2`、`freebsd-amd64-v3`、`freebsd-arm64` | `.gz` |
| Android | `android-386`、`android-amd64`（v1）、`android-armv7`、`android-arm64-v8` | `.gz`，NDK r29 / API 34，需匹配系统和 CPU |

`amd64` 默认保持 GOAMD64=v1；只有确认 CPU 支持对应指令集时才选择 v2/v3。ARM、MIPS 的位宽、大小端、硬浮点/软浮点也必须与设备匹配。路由器通常使用对应 `.gz` 核心，由其插件或启动脚本管理；这里的 Linux 安装包使用 systemd 路径，不适用于 OpenWrt 的 opkg/init 脚本。

构建使用上游 MetaCubeX 的 Go 1.26 工具链，均带 `with_gvisor` 标签。非 Android 目标 `CGO_ENABLED=0`，Android 使用固定 NDK `29.0.14206865` 且 `CGO_ENABLED=1`。标签表示编译选项；TUN/gVisor 等运行能力仍受上游在对应操作系统与架构上的支持限制。跨平台构建并不代表已在每种真实设备上运行测试。

这里覆盖上游常用现代平台与 CPU，但不重复上游所有下载项：不提供 Go 1.20～1.25 的旧系统兼容变体、不提供 LoongArch ABI1 专用工具链构建、不提供工具链/vendor 压缩包，也不重复 `amd64-compatible` 等别名。不要将这些 Go 1.26 文件当作旧 Windows/macOS/Linux 系统的兼容保证。

## 安装包与现有安装迁移

包管理器内部名称为 `mihomo-optimized`，声明提供并替换旧 `mihomo-dns-optimized` 和官方 `mihomo` 包。可执行文件、配置目录和服务名继续兼容现有安装：

| 用途 | 路径 |
| --- | --- |
| 可执行文件 | `/usr/bin/mihomo` |
| 配置 | `/etc/mihomo/config.yaml` |
| systemd unit | `/usr/lib/systemd/system/mihomo.service` |
| 面板 | `/etc/mihomo/ui`（此 service 的工作目录由 `-d /etc/mihomo` 指定） |
| 包内说明 / GPL 许可证 | `/usr/share/doc/mihomo-optimized/README` / `/usr/share/licenses/mihomo-optimized/LICENSE` |

先下载对应 CPU 的包与同一个 Release 的 `SHA256SUMS`，核对后安装：

```sh
sha256sum --ignore-missing --check SHA256SUMS
sudo apt install ./mihomo-optimized-linux-amd64.deb
```

RPM 使用 `sudo dnf install ./mihomo-optimized-<target>.rpm`，Arch 使用 `sudo pacman -U ./mihomo-optimized-<target>.pkg.tar.zst`。旧包和新包拥有相同运行路径，不并行安装为两套服务。按包管理器提示替换旧包；不要使用强制覆盖配置的选项。

Debian 的 conffile、RPM 的 `%config(noreplace)` 和 Arch 的 `backup` 保留用户配置。新包只提供本机 `127.0.0.1:7890` mixed 与 `MATCH,DIRECT` 的通用起始配置；DNS、自动更新默认关闭，无私人节点或凭据。放入自己的配置并校验后启动：

```sh
sudo mihomo -t -d /etc/mihomo
sudo systemctl daemon-reload
sudo systemctl enable --now mihomo
```

包没有启动、启用或重启服务的安装脚本。使用包管理器更换二进制后，自行安排 `sudo systemctl restart mihomo`；删除包前自行停止服务。内核自动更新成功后的重启由内核完成。

从 `1.0.7` 起，systemd unit 设置 `User=mihomo`。DEB / RPM / Arch 的安装和升级钩子只处理账号及文件归属：系统中没有 `mihomo` 时创建非登录系统账号，home 为 `/nonexistent` 且不创建 home，shell 为 `/usr/sbin/nologin`；已有用户直接复用，保留 UID、主组、home 与 shell。新用户复用已有同名组，组也不存在时创建系统组。

安装完成后，`/etc/mihomo` 整个默认数据目录（配置、Geo 数据、provider 缓存、面板等）和 `/usr/bin/mihomo` 都归属 `mihomo` 及该用户的主组。迁移已有资源时保留普通读写执行位与 ACL，不跟随目录中的符号链接修改外部目标；特殊 set-ID 位由 `chown` 按系统语义处理。后续由该服务生成或下载的资源自然归属运行用户。系统级目录与 unit 仍由包管理器管理；自定义到默认目录之外的资源需给予同一运行用户相应权限。

从旧包迁移时，先升级一次原生安装包，再执行 `sudo systemctl daemon-reload` 和 `sudo systemctl restart mihomo`；已启动的旧进程不会因为磁盘 unit 改动自动切换用户。可用 `systemctl show mihomo -p User -p MainPID` 及 `ps -o user,group,args -p "$(systemctl show mihomo -p MainPID --value)"` 核对实际进程。已自行配置的 systemd drop-in 仍参与 systemd 的最终配置。

### 包内版本与实际内核版本

下载文件名固定，包内仍保留包管理器要求的版本信息。新系列永久使用 epoch `1`，让独立 `1.0.0` 排在历史 `1.19.32+dns...` 之后：

| 格式 | `mihomo-optimized-v1.0.0` 的内部版本 |
| --- | --- |
| Debian | `1:1.0.0` |
| RPM | epoch `1`、version `1.0.0`、release `1` |
| Arch `.PKGINFO` 的 `pkgver` | `1:1.0.0-1` |

安装包负责首次安装、配置归属和卸载。安装后可启用内核自更新，无需每次下载新 deb/rpm/pkg。自更新只替换可执行文件，不改系统包管理器数据库，所以 `dpkg`/`rpm`/`pacman` 仍显示当初安装包的版本。实际版本以 `mihomo -v`、控制器 `/version` 或面板显示的内核版本为准。

以后重新安装某个 native 包，会安装该包携带的二进制；选择与当前运行版本一致或更高的包。配置文件仍由包管理器保护。默认 unit 的 `CAP_DAC_OVERRIDE` 等现有 capabilities 保留，让 `mihomo` 在 `/usr/bin` 中完成内核文件的原子替换；只修改文件 owner 并不能代替父目录的替换权限。配置、面板及其他默认资源通过自身 owner 权限写入。

内核自更新不会安装 systemd unit、创建系统账号或批量迁移文件归属，因此不能完成上述 `1.0.7` 的服务用户迁移。若仅手工替换二进制，启用透明代理前需要准备 `mihomo` 账号与资源权限，或在 `local.bypass-users` 显式填写已有服务用户 / UID；显式 `[]` 可关闭用户旁路。

## 项目版本与构建

`PROJECT_VERSION` 保存 `X.Y.Z`，tag、Release 标题、`version.txt` 与 `mihomo -v` 使用 `mihomo-optimized-vX.Y.Z`。`UPSTREAM_VERSION` / `UPSTREAM_COMMIT` 单独记录上游基线。三个版本分量必须是无前导零的非负 uint64；不使用预发行、构建后缀或提交次数充当正式版本。

准备新内核发行时更新项目版本。自动同步上游稳定版会先合并，并将项目 patch 加一；候选通过规定测试和 Linux amd64/arm64 构建后才普通推送，再触发完整发行矩阵。冲突、版本溢出、失败测试或远端 main 变化都会停止；不强推或覆盖 fork 的工作流与发布脚本。

已有版本不能用于不同源码。CI 在构建前与发布时检查同名 tag 的目标提交，并核对全部附件；旧版本保留原样。仅 Markdown、MDX、reStructuredText、AsciiDoc、`docs/**`、`Meta.png` 和 issue/PR 模板变化时不自动构建；与源码、`packaging/**`、依赖、脚本或工作流混合修改时照常执行。

构建脚本检查每个 native 包的架构、版本、配置保护、文件清单/权限、账号工具依赖及唯一预期安装钩子，以及其二进制与对应归档完全一致。钩子行为测试覆盖新账号、已有用户 / 主组复用、重复升级和失败传播；账号及 chown 命令被限定到测试夹具。回归还在隔离 dpkg 根目录执行旧包到新包的真实迁移，验证本地修改的配置保留与文件归属。Linux amd64 产物实际执行 `-v`、`-t` 和本地模拟上游的 DNS 端到端回归；不会安装或启动测试宿主机已有的 mihomo 服务。stop 回归以现有非 root 测试账号创建使用隔离网络命名空间的临时 systemd 单元，使用该账号持有的临时资源，并在结束时撤销。

开发者可用以下命令查看矩阵。默认返回完整 37 目标；PR profile 固定返回 7 个纯编译检查目标且不打包，不能用于缩减正式发布的必需产物集合：

```sh
python3 scripts/release-build.py matrix
python3 scripts/release-build.py matrix --profile pr
python3 scripts/release-build.py compile-check linux-amd64
```

运行 `bash scripts/ci-check.sh build <target> <绝对输出目录> mihomo-optimized-v1.0.11 <ISO时间>` 可构建单个目标；版本参数须与当前 `PROJECT_VERSION` 一致，`amd64` / `arm64` 是 Linux 别名。依赖 `dpkg-deb`、`rpm`/`rpmbuild`/`rpm2cpio`、`cpio`、`zstd`、`fakeroot`、Python 3.12 和 MetaCubeX Go；Android 还需矩阵指定的固定 NDK。

## 内核自动更新

在当前 YAML 顶层配置：

```yaml
core-auto-update: true
core-update-interval: 24 # 整数小时；默认 24，范围 1..2562047
external-ui-auto-update: true
external-ui-update-interval: 24 # 面板检查间隔，采用相同单位和范围
```

两个开关默认都是 `false`，两个间隔默认 `24`，检查无需打开面板。间隔必须写为整数小时，不接受 `24h`、`"24"`、小数或 `0`；关闭使用对应开关。保存后完整重载才应用，`mihomo -t` 不启动检查或初始面板下载。

从内核 **`mihomo-optimized-v1.0.1`** 起，开关已启用的进程每次启动 / 重启会在网络与配置就绪后立即异步检查一次，然后按间隔检查；运行中关闭改为开启也立即检查。相同策略完整重载保留原计时器，不重复检查或重置下次时间；保持开启而只改间隔时，取消旧任务并从该次重载按新间隔计时，不追加立即检查。关闭会取消对应旧任务，迟到结果不能继续安装。

启动 / 启用检查可取消地等待前一个操作释放，避免快速关闭再开启时丢失本次检查；周期检查遇忙跳过，不堆积安装请求。后台网络准备、下载与等待不阻塞配置重载。core 开关启用同时表示允许成功安装后自动重启，重启会关闭当前连接、重建内存 DNS 状态。

内核从本仓库 Latest 的 `version.txt` 读取远端版本，按数值比较当前实际项目版本，只有严格更高版本才自动安装。比如 `1.0.12 > 1.0.2`；相同版本、降级、非法版本或开发版不会触发自动替换。整个下载固定到首次确定的 tag，再读取该 tag 的 `SHA256SUMS` 与精确 `.gz` / `.zip`，Latest 在途中变化不会混用附件。

选择保持现有操作系统、CPU 指令集与浮点 ABI。清单重复/格式错误、缺少该架构、HTTP 失败、摘要错误或解压失败都保留当前内核。通过校验后先暂存；最终替换前再次核对当前策略、版本与原文件身份/摘要，配置已经改变或二进制被外部更换时放弃该任务。操作串行化，并记录已安装版本与等待重启状态，避免使用尚未重启的旧 `C.Version` 重复安装。

原文件保存在同目录 `meta-backup/`；替换后沿用现有进程重启流程。默认、`auto`、`release` 通道使用本仓库，不使用官方 alpha。兼容的 `/upgrade` API 保留给其他客户端。自内核 `1.0.4`、面板 `0.4.6` 起，面板提供统一“检查并更新”，只通过 `POST /upgrade/check` 发起一次实际更新，再通过同路径 `GET` 读取状态；内核升级排在其他资源后并自动重启，YAML 自动策略不变。详见[统一更新接口](dashboard-management.md#统一检查并更新接口)。

内核 `1.0.5` 修复 GeoData 自动调度与热重载脱节的问题：`geo-auto-update`、`geo-update-interval` 的变更会实际应用，首次失败仍保留周期检查，间隔限制为 `1`–`2562047` 整数小时以避免计时器溢出。配套面板 `0.4.7` 修复重载请求无上限等待，以及切换控制器后的节点和版本数据混用。GeoData 的到期判断与面板、内核的启动立即检查分别定义，见 [GeoData 自动更新调度](dashboard-management.md#geodata-自动更新调度)。

面板始终来自本仓库 `clash-dashboard-dist`，存于 HomeDir 下的 `ui`，跟随 `-d`。旧 `external-ui`、`external-ui-url`、`external-ui-name` 配置及命令行 `-ext-ui` 均被忽略。面板通过本地和归档的 `build-info.json` SemVer 比较更新，仅严格新版会自动替换静态资源，不重启内核或清理 DNS 数据。目录缺失或为空时，即使面板自动更新关闭，仍会后台下载初始面板；初始安装与启动检查去重。完整调度、首次安装和取消语义见[面板及内核维护说明](dashboard-management.md)，其他旧配置的保留与停用边界见 [README 迁移表](../README.md#旧配置迁移与停用范围)。

## 旧内核首次切换新系列

**包括已发布 `v1.19.32-optimized-13` 在内的旧内核，首次切换到 `mihomo-optimized-v1.0.0` 或更新的新系列内核，需要手动安装一次。** 旧二进制内置解析器只支持旧版 tag；新源码不能改变已经安装的程序。仓库重命名重定向解决的是旧链接访问，不会改变旧版解析规则。

下载并校验对应平台的新归档或 native 包，按原安装方式替换并重启，确认 `mihomo -v` 显示新系列，然后启用 YAML 自动更新。没有新增旧名 bridge Release 或复制旧资产；历史 tag 和文件不修改。

新更新器仍能读取历史 `vX.Y.Z-optimized-N` / `vX.Y.Z-dns-optimized-N` 与 `mihomo-` / `mihomo-dns-` 归档，便于显式维护操作。新系列被视为比所有旧系列更新，自动更新不会退回旧系列。历史兼容只对已安装新更新器的程序生效；`force` 不会绕过 tag 格式、归档或 SHA256 校验。
