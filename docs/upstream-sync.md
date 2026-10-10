# 云编译与跟随上游稳定版本

本分支保留 MetaCubeX/mihomo 的 Git 历史。当前基线由根目录的
`UPSTREAM_VERSION` 和 `UPSTREAM_COMMIT` 记录；首次基线为 `v1.19.32`。

## 下载编译结果

向 `main` 推送代码、提交针对 `main` 的 Pull Request，或者在 Actions 中手动运行
**Build DNS optimized**，都会测试并编译完整内核。

产物覆盖 Linux、Windows、macOS、FreeBSD、Android 共 **37 个平台 / CPU 目标**。
其中 12 个 Linux 目标另提供 `.deb` / `.rpm`，5 个目标提供 `.pkg.tar.zst`；
全部共 66 个归档和安装包，另附 `BUILDINFO.json`、`version.txt` 和 `SHA256SUMS`。
完整 CPU、安装路径与兼容范围见[预编译文件与安装包](releases.md)，矩阵唯一来源为
[`packaging/targets.json`](../packaging/targets.json)。

各目标均使用 `with_gvisor` 编译标签；Android 使用固定 NDK r29 / API 34 和 CGO，
其他目标关闭 CGO。标签不代表每个平台都具备同样的 TUN 运行能力。
Actions artifact 保留 30 天。归档解压后需按平台给二进制增加执行权限。
启用全局 `dns-rule-routing` 的方法见 [DNS 按查询域名分流说明](dns-proxy.md)。

编译前运行 DNS 分类与逐查询路由测试、配置与规则相关测试、DNS 相关竞态检查、安装包检查，以及本地仓库的同步保护测试。
原生 amd64 二进制还会运行本机端到端测试；这些测试使用本地模拟 DNS/SOCKS
服务，不访问用户节点。其余平台为交叉编译，不声称已在相应真实设备执行。

推送 `main` 后，测试和完整 37 目标的编译全部成功，且 66 个必需归档 / 安装包的
文件集合、版本和摘要校验均通过，才自动把构建结果发布到
GitHub Releases，提供持久下载。Pull Request 只测试和上传 Actions artifact。
手动运行 `Build DNS optimized` 并勾选 `publish_release`、推送形如
`v1.19.32-optimized-8` 的版本标签，以及自动上游更新成功后，也会发布 release。
发布只在本仓库进行，已有同名 release 的附件不会被覆盖；重跑也必须核对既有
Release 的完整附件集合及 GitHub 返回的 SHA-256 摘要。构建前和发布时都会确认
同名标签仍指向本次源码提交，轻量标签和附注标签均会解引用后核对。

内核版本使用 `UPSTREAM_VERSION` 与 `OPTIMIZED_REVISION`，不再从 Git 提交数量
生成修订号。准备下一次代码发行时递增 `OPTIMIZED_REVISION`；仅修改文档不会
触发构建或发版，也不会消耗修订号。旧版内核首次切换到新命名的安装要求见
[内核一键更新](releases.md#内核一键更新)。

发布任务按仓库串行执行。新 release 创建时先不标记 Latest，附件上传后重新读取
当前 `main` 的提交；只有它仍等于这次的构建提交，才更新 Latest。较旧提交的构建
仍保留下载，但不会因完成较晚或重跑而覆盖新版本的 Latest 链接。

## 自动跟随上游

**Sync stable upstream** 每天在北京时间 04:23 计划运行，也可以手动运行。
GitHub 的定时任务可能延迟；它查询 MetaCubeX/mihomo 的最新正式 release，
仅接受 `vX.Y.Z` 格式的稳定标签。

更新顺序：

1. 从当前 `main` 准备普通 Git merge，保留本分支修改和上游历史。
2. 保留本分支的整个 `.github/workflows` 目录、同步 / 编译脚本、`packaging/` 和
   `OPTIMIZED_REVISION`，移除新带入的上游工作流。随后将本分支修订号加一，
   与上游版本元数据一起加入待测试的合并，候选构建和提交后的正式构建读取同一版本。
3. 运行测试和安装包检查，并完整编译 Linux amd64、arm64。测试、编译或代码冲突失败即停止。
4. 检查远端 `main` 是否仍是开始测试时的提交；有并发修改则停止，稍后重试。
5. 正常提交并推送合并，不使用强制推送。随后直接调用编译工作流，生成下载产物
   并发布 release。推送前门禁是这两个 Linux 目标，发布门禁仍要求完整 37 目标全部通过；
   若其他架构在完整矩阵中失败，不生成新的成功 Release，Latest 保留已有可下载版本。

工作流使用 GitHub 自动提供的 `GITHUB_TOKEN`，不需要 PAT 或其他仓库 secrets。
普通 CI 的测试和编译 job 只有读取源码权限；同步及发布 release 的 job 才声明
`contents: write`。如果仓库分支规则阻止 Actions 直接更新 `main`，同步会报告失败，
不会绕过分支规则。普通编译仍可运行。

GitHub 不会因 `GITHUB_TOKEN` 推送自动触发另一个 push 工作流，因此同步任务明确
复用编译工作流，不依赖这种递归触发。参考
[GitHub 工作流触发说明](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow)。

发生冲突或上游变更要求调整本分支时，Actions 日志和运行摘要会报告失败，`main`
保留最后通过测试的版本。需要人工合并冲突或更新兼容性后，后续同步继续运行。
工具链暂跟随当前上游的 Go 1.26；上游未来升级工具链而导致验证失败时，也会停止
更新，等待调整配置。

## 预演与本地检查

在 Actions 手动运行 `Sync stable upstream` 时勾选 `dry_run`，会准备合并并完成
测试和两种架构的编译，整个运行不推送代码、不发布 release。

本地只检查合并是否干净，可以在工作区无未提交修改时运行：

```bash
bash scripts/upstream-sync.sh --dry-run v1.19.33
```

把版本替换成实际存在的稳定标签。此命令在临时 worktree 中预演合并，完成后
删除临时 worktree；它不编译、不改变原分支或工作区、不推送远端。

本地同步保护测试不依赖公网：

```bash
python3 scripts/test-upstream-sync.py
```

完整功能测试需要 Go 工具链和打包检查依赖（`dpkg-deb`、RPM 工具、`cpio`、`zstd`、Python 3.12）：

```bash
bash scripts/ci-check.sh test
```

普通调用 `scripts/upstream-sync.sh vX.Y.Z` 会在当前工作区留下待测试的 staged
合并，脚本本身从不 commit 或 push。使用它处理人工更新时，应检查、测试后再
提交；需要撤销仍在进行的合并时，使用标准 Git 的 `git merge --abort`。
