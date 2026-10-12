# 预编译文件、安装包与内核更新

项目使用独立版本 **`mihomo-optimized-vX.Y.Z`**，首版为 `mihomo-optimized-v1.0.0`。`main` 的源码、依赖或构建配置变更在全部测试和完整编译成功后生成固定提交的 GitHub Release。Latest 仅在该构建仍对应当前 `main` 时更新；PR 只生成 Actions artifacts。纯文档变更不消耗版本号，也不触发自动发布。

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

### 包内版本与实际内核版本

下载文件名固定，包内仍保留包管理器要求的版本信息。新系列永久使用 epoch `1`，让独立 `1.0.0` 排在历史 `1.19.32+dns...` 之后：

| 格式 | `mihomo-optimized-v1.0.0` 的内部版本 |
| --- | --- |
| Debian | `1:1.0.0` |
| RPM | epoch `1`、version `1.0.0`、release `1` |
| Arch `.PKGINFO` 的 `pkgver` | `1:1.0.0-1` |

安装包负责首次安装、配置归属和卸载。安装后可启用内核自更新，无需每次下载新 deb/rpm/pkg。自更新只替换可执行文件，不改系统包管理器数据库，所以 `dpkg`/`rpm`/`pacman` 仍显示当初安装包的版本。实际版本以 `mihomo -v`、控制器 `/version` 或面板显示的内核版本为准。

以后重新安装某个 native 包，会安装该包携带的二进制；选择与当前运行版本一致或更高的包。配置文件仍由包管理器保护。自动更新需要运行用户有权写入二进制所在目录；默认 systemd unit 沿用现有 root 运行方式。

## 项目版本与构建

`PROJECT_VERSION` 保存 `X.Y.Z`，tag、Release 标题、`version.txt` 与 `mihomo -v` 使用 `mihomo-optimized-vX.Y.Z`。`UPSTREAM_VERSION` / `UPSTREAM_COMMIT` 单独记录上游基线。三个版本分量必须是无前导零的非负 uint64；不使用预发行、构建后缀或提交次数充当正式版本。

准备新内核发行时更新项目版本。自动同步上游稳定版会先合并，并将项目 patch 加一；候选通过规定测试和 Linux amd64/arm64 构建后才普通推送，再触发完整发行矩阵。冲突、版本溢出、失败测试或远端 main 变化都会停止；不强推或覆盖 fork 的工作流与发布脚本。

已有版本不能用于不同源码。CI 在构建前与发布时检查同名 tag 的目标提交，并核对全部附件；旧版本保留原样。仅 Markdown、MDX、reStructuredText、AsciiDoc、`docs/**`、`Meta.png` 和 issue/PR 模板变化时不自动构建；与源码、`packaging/**`、依赖、脚本或工作流混合修改时照常执行。

构建脚本检查每个 native 包的架构、版本、配置保护、文件清单/权限、无安装脚本，以及其二进制与对应归档完全一致。回归还在隔离 dpkg 根目录执行旧包到新包的真实迁移，验证本地修改的配置保留与文件归属。Linux amd64 产物实际执行 `-v`、`-t` 和本地模拟上游的 DNS 端到端回归；不会安装到测试宿主机或启动宿主服务。

开发者可运行 `python3 scripts/release-build.py matrix` 查看 37 目标矩阵，或 `bash scripts/ci-check.sh build <target> <绝对输出目录> mihomo-optimized-v1.0.5 <ISO时间>` 构建单个目标；版本参数须与当前 `PROJECT_VERSION` 一致，`amd64` / `arm64` 是 Linux 别名。依赖 `dpkg-deb`、`rpm`/`rpmbuild`/`rpm2cpio`、`cpio`、`zstd`、`fakeroot`、Python 3.12 和 MetaCubeX Go；Android 还需矩阵指定的固定 NDK。

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
