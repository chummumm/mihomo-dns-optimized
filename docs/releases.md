# 预编译文件与安装包

每次 `main` 测试和完整编译成功后都会生成固定提交的 GitHub Release。下载页的 **Latest** 仅在该构建仍对应当前 `main` 时更新。PR 只产生 Actions artifacts；每天的稳定版上游同步先经过测试和 Linux amd64/arm64 构建，再触发同一套完整发行矩阵。

所有文件名包含平台、CPU 和版本，例如：

```text
mihomo-dns-linux-amd64-v1.19.32-dns.<数字修订号>.gz
mihomo-dns-linux-amd64-v1.19.32-dns.<数字修订号>.deb
mihomo-dns-windows-amd64-v1.19.32-dns.<数字修订号>.zip
```

`SHA256SUMS` 覆盖全部 66 个二进制归档/安装包及 `BUILDINFO.json`。`BUILDINFO.json` 记录完整提交号、各目标的 Go 架构参数、工具链、CGO、编译标签以及每个文件的大小和 SHA-256。每个目标缺少文件或校验不一致时，整个 Release 发布失败。已有同名 Release 的文件不被覆盖；重新发布时也必须验证其文件集合和摘要一致。

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

安装包名为 `mihomo-dns-optimized`，声明提供并替换 `mihomo`；它与官方 `mihomo` 包使用同一路径，不作为第二套服务并存。切换前备份现有配置，并确认下载的 CPU 版本：

```sh
# 在保存下载文件的目录执行；将占位符替换为实际文件名。
sha256sum --ignore-missing --check SHA256SUMS
sudo apt install ./mihomo-dns-linux-amd64-<version>.deb
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

安装包内部版本包含上游版本、UTC 提交时间和提交号，例如 `1.19.32+dns.20261009020000.123456789abc`，正常的新提交能按时间升级；具体文件名仍与 Release 的源码版本一致。

## RPM / Arch 与直接使用核心

RPM 系统用 `sudo dnf install ./mihomo-dns-<target>-<version>.rpm`，Arch 系统用 `sudo pacman -U ./mihomo-dns-<target>-<version>.pkg.tar.zst`。两者有相同的路径、配置保留和手动启动行为。替换官方包时应阅读包管理器的冲突/替换提示。

直接使用归档时，解压对应文件即可；Linux/macOS/FreeBSD/Android 的 `.gz` 是单个可执行文件，解压后需要 `chmod +x`。Windows `.zip` 内为 `.exe`。使用自己的数据目录运行，例如 `./mihomo -d /path/to/config-dir`。

构建脚本会检查每个包的架构、版本、配置保留标记、无生命周期脚本、文件清单/权限，以及包内二进制与该目标编译结果完全一致。Linux amd64 还实际执行 `-v`、通用配置 `-t` 和本地 DNS 端到端测试。安装包没有在测试宿主机上执行安装或启动服务。

开发者可运行 `python3 scripts/release-build.py matrix` 查看 Actions 矩阵，或通过 `bash scripts/ci-check.sh build <target> <绝对输出目录> <版本> <ISO时间>` 复现一个目标；`amd64` / `arm64` 仍是对应 Linux 目标的简写。测试/打包依赖 `dpkg-deb`、`rpm`/`rpmbuild`/`rpm2cpio`、`cpio`、`zstd`、Python 3.12 和 Go；Android 还需上述固定 NDK，并设置 `ANDROID_NDK_HOME`。

## 数字修订版本

新发行版本形式为 `v1.19.32-dns.数字`，对应 tag 为 `dns-v1.19.32.数字`。数字由 `DNS_RELEASE_BASE` 之后的 first-parent 提交数量确定，因此可复现、随主线推进递增，并不要求每次发行连续编号。上游稳定版更新仍由原同步流程处理。提交 SHA 不再放入二进制版本或发行文件名，保留于 `BUILDINFO.json` 和提交记录。

包管理器版本保留 UTC 构建时间前缀以保证从已有带哈希版本正常升级，其末尾改为数字修订号。安装包不会自动重启服务，升级后仍需校验配置并手动重启。
