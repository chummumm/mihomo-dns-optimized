# 云编译与跟随上游稳定版本

本分支保留 MetaCubeX/mihomo 的 Git 历史。当前基线由根目录的
`UPSTREAM_VERSION` 和 `UPSTREAM_COMMIT` 记录；首次基线为 `v1.19.32`。

## 下载编译结果

向 `main` 推送代码、提交针对 `main` 的 Pull Request，或者在 Actions 中手动运行
**Build DNS optimized**，都会测试并编译完整内核。

产物包含两个架构：

| 产物 | 适用机器 |
| --- | --- |
| `mihomo-dns-linux-amd64-<version>.gz` | Linux x86-64，使用兼容性较好的 `GOAMD64=v1` |
| `mihomo-dns-linux-arm64-<version>.gz` | Linux ARM64 |

二者均启用 `with_gvisor`，关闭 CGO。每份产物附 SHA-256 校验文件，
Actions artifact 保留 30 天。解压后需要给二进制增加执行权限。
启用专用 DNS 入口的方法见 [DNS 代理说明](dns-proxy.md)。

编译前运行专用 DNS 路由测试、配置与规则相关测试，以及本地仓库的同步保护测试。
原生 amd64 二进制还会运行本机端到端测试；这些测试使用本地模拟 DNS/SOCKS
服务，不访问用户节点。arm64 为交叉编译，不声称执行了 arm64 二进制测试。

手动运行 `Build DNS optimized` 并勾选 `publish_release`，或者推送形如
`dns-v1.19.32.1` 的版本标签，可把构建结果发布到 GitHub Releases。
发布只在本仓库进行，已有同名 release 不会被覆盖。自动上游更新成功后也会创建
以源码提交标识的 release。

## 自动跟随上游

**Sync stable upstream** 每天在北京时间 04:23 计划运行，也可以手动运行。
GitHub 的定时任务可能延迟；它查询 MetaCubeX/mihomo 的最新正式 release，
仅接受 `vX.Y.Z` 格式的稳定标签。

更新顺序：

1. 从当前 `main` 准备普通 Git merge，保留本分支修改和上游历史。
2. 保留本分支的整个 `.github/workflows` 目录及同步/编译脚本，移除这次合并
   新带入的上游工作流，避免导入上游发布或跨仓库触发任务。
3. 运行测试，并完整编译 amd64、arm64。测试、编译或代码冲突失败即停止。
4. 检查远端 `main` 是否仍是开始测试时的提交；有并发修改则停止，稍后重试。
5. 正常提交并推送合并，不使用强制推送。随后直接调用编译工作流，生成下载产物
   并发布 release。

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

完整功能测试需要 Go 工具链：

```bash
bash scripts/ci-check.sh test
```

普通调用 `scripts/upstream-sync.sh vX.Y.Z` 会在当前工作区留下待测试的 staged
合并，脚本本身从不 commit 或 push。使用它处理人工更新时，应检查、测试后再
提交；需要撤销仍在进行的合并时，使用标准 Git 的 `git merge --abort`。
