# 预编译文件与安装包

`main` 包含源码、依赖或构建配置变更时，测试和完整编译成功后生成固定提交的 GitHub Release。下载页的 **Latest** 仅在该构建仍对应当前 `main` 时更新。PR 只产生 Actions artifacts；每天的稳定版上游同步先经过测试和 Linux amd64/arm64 构建，再触发同一套完整发行矩阵。

仅修改 Markdown、MDX、reStructuredText、AsciiDoc、`docs/**`、展示图片 `Meta.png` 或 GitHub issue/PR 说明模板时，push 和 PR 不启动自动核心构建、性能验证或发布。`docs/**` 包含文档配图和说明用配置示例；实际打包配置 `packaging/**`、源码、依赖、脚本和工作流仍触发构建。文档与这些文件混合修改时照常执行。显式手动构建、发行标签和上游稳定版同步调用保留原有行为。

从 `v1.19.32-optimized-9` 起，所有归档和安装包统一使用 `mihomo-<target>-<version>`，包含平台、CPU 和版本，例如：

```text
mihomo-linux-amd64-v1.19.32-optimized-9.gz
mihomo-linux-arm64-v1.19.32-optimized-9.deb
mihomo-windows-amd64-v1.19.32-optimized-9.zip
```

Windows ZIP 内的执行文件相应为 `mihomo-<target>.exe`。新 Release 只发布这一套文件名，不同时附加 `mihomo-dns-` 旧名副本。已经发布的 v8 及更早版本保留原始文件和校验和；本次改名使用新的 v9 修订号，不覆盖既有 Release。

`SHA256SUMS` 覆盖全部 66 个二进制归档/安装包，以及 `BUILDINFO.json` 和 `version.txt`。`BUILDINFO.json` 记录完整提交号、各目标的 Go 架构参数、工具链、CGO、编译标签以及每个文件的大小和 SHA-256。每个目标缺少文件或校验不一致时，整个 Release 发布失败。已有同名 Release 的文件不被覆盖；重新发布时也必须验证其文件集合和摘要一致。

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

## Debian / Ubuntu 安装

安装包内部名称继续保留 `mihomo-dns-optimized`，声明提供并替换 `mihomo`。仅发行文件名去掉 `dns`，包管理器仍将新版识别为同一个包的升级，原有配置和服务路径继续由原包管理。它与官方 `mihomo` 包使用同一路径，不作为第二套服务并存。切换前备份现有配置，并确认下载的 CPU 版本：

```sh
# 在保存下载文件的目录执行；将占位符替换为实际文件名。
sha256sum --ignore-missing --check SHA256SUMS
sudo apt install ./mihomo-linux-amd64-<version>.deb
```

安装后的固定路径：

| 用途 | 路径 |
| --- | --- |
| 可执行文件 | `/usr/bin/mihomo` |
| 配置 | `/etc/mihomo/config.yaml` |
| systemd unit | `/usr/lib/systemd/system/mihomo.service` |
| 包内说明 / GPL 许可证 | `/usr/share/doc/mihomo-dns-optimized/README` / `/usr/share/licenses/mihomo-dns-optimized/LICENSE` |

包内仅提供通用起始配置：监听本机 `127.0.0.1:7890`、`MATCH,DIRECT`，内置 DNS 与 `dns-rule-routing` 关闭，无 FakeIP、订阅或私人凭据。请将自己的配置放入 `/etc/mihomo/config.yaml`，检查成功后再显式启动：

```sh
sudo mihomo -t -d /etc/mihomo
sudo systemctl daemon-reload
sudo systemctl enable --now mihomo
```

安装包没有启动、启用或重启服务的安装脚本。升级已有运行中的服务后，需要自己安排 `sudo systemctl restart mihomo`。删除包也不会替用户管理当前进程，应先自行停止服务。

Debian 使用 conffile；RPM 使用 `%config(noreplace)`；Arch 使用 `backup` 标记。升级会保留用户修改的配置，冲突时由相应包管理器提示或保存 `.dpkg-dist` / `.rpmnew` / `.pacnew` 等候选文件。不要使用强制覆盖配置的包管理器选项。包内无个人配置、节点、API 密钥或认证信息。

安装包内部版本包含上游版本、UTC 提交时间和数字修订号，例如 `1.19.32+dns.20261010120000.9`。Debian、RPM 和 Arch 原有的包名称及版本序列均保留，使文件改名不影响已有包的升级顺序；具体下载文件名仍与 Release 版本一致。

## RPM / Arch 与直接使用核心

RPM 系统用 `sudo dnf install ./mihomo-<target>-<version>.rpm`，Arch 系统用 `sudo pacman -U ./mihomo-<target>-<version>.pkg.tar.zst`。两者有相同的路径、配置保留和手动启动行为。替换官方包时应阅读包管理器的冲突/替换提示。

直接使用归档时，解压对应文件即可；Linux/macOS/FreeBSD/Android 的 `.gz` 是单个可执行文件，解压后需要 `chmod +x`。Windows `.zip` 内为 `.exe`。使用自己的数据目录运行，例如 `./mihomo -d /path/to/config-dir`。

构建脚本会检查每个包的架构、版本、配置保留标记、无生命周期脚本、文件清单/权限，以及包内二进制与该目标编译结果完全一致。Linux amd64 还实际执行 `-v`、通用配置 `-t` 和本地 DNS 端到端测试。安装包没有在测试宿主机上执行安装或启动服务。

开发者可运行 `python3 scripts/release-build.py matrix` 查看 Actions 矩阵，或通过 `bash scripts/ci-check.sh build <target> <绝对输出目录> <版本> <ISO时间>` 复现一个目标；`amd64` / `arm64` 仍是对应 Linux 目标的简写。测试/打包依赖 `dpkg-deb`、`rpm`/`rpmbuild`/`rpm2cpio`、`cpio`、`zstd`、Python 3.12 和 Go；Android 还需上述固定 NDK，并设置 `ANDROID_NDK_HOME`。

## 数字修订版本

新发行版本从 `v1.19.32-optimized-8` 开始，tag、Release 标题、`version.txt` 与 `mihomo -v` 使用相同版本。版本由源码中的 `UPSTREAM_VERSION` 和 `OPTIMIZED_REVISION` 共同确定，不再按提交次数推算。每次准备新的内核发行时递增修订号；纯文档更新不改编号，也不会触发构建或自动创建 Release。自动同步上游稳定版时，准备合并的脚本会将修订号加一并加入同一份待测试提交，候选构建与正式构建读取相同版本文件。

已有修订号不能用于不同源码：CI 在完整平台构建前检查同名标签的目标提交，发布时再次检查标签和附件。已经发布的旧命名标签保留原样。提交 SHA 保留于 `BUILDINFO.json` 和提交记录，便于核对和重现构建。

包管理器版本保留 UTC 构建时间前缀以保证从已有带哈希版本正常升级，其末尾改为数字修订号。安装包不会自动重启服务，升级后仍需校验配置并手动重启。

## 内核一键更新

本版本的核心 `/upgrade` 使用 **chummumm/mihomo-dns-optimized** 的正式 Release。默认、`auto` 和 `release` 通道都先读取本仓库 Latest 的 `version.txt`，再将校验和及对应架构归档的下载固定到该版本标签；下载过程中 Latest 改变不会混用版本。未提供本仓库 alpha 发行，显式请求 `alpha` 会提示使用 `release`，不会跳转官方仓库。

更新保留当前平台和 CPU 变体，包括 amd64 v1/v2/v3、386 softfloat、ARM、MIPS 浮点 ABI、LoongArch ABI2 和 Android 文件名。不存在对应发行目标时明确失败。新版更新器同时识别 `vX.Y.Z-optimized-N` 和旧 `vX.Y.Z-dns-optimized-N`，按上游版本和数字修订号比较；未设置 `force` 时不会降级。下载使用发行元数据给出的原始标签，不通过改写版本字符串猜测下载地址。

从 v9 起，新更新器只在固定标签的 `SHA256SUMS` 中选择当前目标、同一版本的精确归档名：优先 `mihomo-<target>-<version>.gz` / `.zip`；只有清单没有新名、且包含合法旧名时，才读取历史 `mihomo-dns-<target>-<version>` 归档。两种名称都存在时始终选新名；清单重复或格式错误直接失败。新名归档的 HTTP、SHA256 或解压验证失败后，不再尝试旧名，也不跳转其他版本或官方源。

归档必须通过本版本 `SHA256SUMS` 校验，解压到独立临时目录后才替换内核；HTTP 错误、版本格式错误、缺失校验、摘要不符或无效归档不会覆盖当前文件。原内核保存在同目录 `meta-backup/`。一键更新成功后仍沿用现有接口的重启行为，配置文件不被替换。

**已经安装的 `v1.19.32-optimized-8` 及更早内核，首次升级 v9 需要手动安装一次。** 两次命名迁移涉及不同限制：

| 已安装内核 | 首次升级 v9 的限制 |
| --- | --- |
| `v1.19.32-optimized-8` | 能识别新版本号，但编译内置的更新器仍要求 `mihomo-dns-` 资产名；新清单没有该名称，因缺少对应校验而停止，保留原内核。 |
| `v1.19.32-dns-optimized-N`、`v1.19.32-dns.2` 等旧版本 | 旧版本解析规则和旧资产名均已编译进文件，不能依靠新源码改变其行为。 |
| 已安装本仓库 v9 或后续新版 | 使用新更新器，从本仓库固定版本的校验和清单选择资产，可继续通过原入口升级。 |

新更新器兼容历史文件名，只对已经安装新代码的内核生效。`force` 不会绕过版本格式或 SHA256 校验。为保持新 Release 只有一套清晰的文件名，本项目采用手动安装 v9 的一次迁移，不增加旧名副本或修改已发布的 v8。

首次迁移时，下载并校验对应平台的新归档或安装包，按原来的安装方式替换内核并重启，用 `mihomo -v` 确认已运行 `v1.19.32-optimized-9` 或后续新版本。此后直接使用核心的用户可继续通过面板的内核更新入口升级；用 deb/rpm/pkg 安装的用户建议继续通过相应安装包升级，以保持包管理器记录一致。
