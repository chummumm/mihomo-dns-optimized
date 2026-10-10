# Clash Dashboard：DNS 状态与连接显示增强

在现有原版 Clash Dashboard 上维护，增加纯内存 DNS 状态展示，并修复连接列表刷新与计数。没有迁移到另一套面板。连接列表、主机名排序和连接详情统一按 `host → sniffHost → destinationIP` 取值；原目标域名优先，旧内核没有 `sniffHost` 时仍正常工作。

修改只影响显示，不会写回连接元数据，不会改变实际连接目标或 DNS 分流。使用 `enhanced-mode: normal` 和 `override-destination: false` 时，已经嗅探到的域名也能显示；未获得任何域名的连接仍显示 IP。

## 连接列表

“全部（N）”和各 IP 数字从同一份保留记录计算。开启“保留关闭连接”时，两者都包含尚未淘汰的关闭记录；关闭历史最多保留 5000 条，活跃连接完整计数。面板收到快照后在同一帧更新，兼容旧内核空快照中的 `connections: null`，所选设备消失后恢复显示全部。

长列表使用已有的 `react-window` 按可见区域渲染。刷新时通过连接 ID 保持阅读位置，停在最底部时保持底部；列宽、横向滚动和固定主机名列继续可用。连接 WebSocket 不再额外缓存 200 份完整快照，关闭记录独立维护，避免每次刷新重新判定全部历史的状态。

连接页统计的是浏览器当前保留集合，不是内核启动以来的累计连接数。关闭页面或刷新浏览器后不会从磁盘恢复历史。

DNS 连接在列表和详情中统一显示类型 `DNS`，节点链只显示实际选定的最终节点（直连显示 `DIRECT`），不显示匹配规则和代理组。识别优先使用内核的明确 DNS 标记，并兼容旧版专用 DNS 入口标记；普通 SOCKS/HTTP 到目标 53 的连接保持原有显示，不会被当成内核 DNS 查询。原始连接元数据、完整节点链和规则仍保留在控制 API 中，显示简化不会改变解析或路由。DNS 查询记录页中的 A、AAAA、HTTPS 等记录类型保持不变。

## DNS 概览、记录和上游

DNS 页由本二开内核的 `/dns/observability` API 提供数据：

| 页面 | 显示内容 |
| --- | --- |
| 概览 | 最近 24 小时查询量、QPS、缓存命中率、新鲜/过期缓存命中数、错误、平均耗时和估算 P95、分钟趋势、热门域名/客户端 |
| 查询记录 | 原始查询名、客户端、类型、入口、结果、返回码、耗时、有限答案摘要；精确过滤与 50/100 条游标翻页 |
| 上游 | 实际交换尝试、成功、错误、取消、超时、失败返回码、平均耗时与已完成成功率 |

缓存命中率为同一窗口的 `(新鲜缓存命中 + 过期缓存命中) / 已完成客户端 DNS 查询总数`；hosts、Fake IP、测速候选缓存不计入正式 DNS 缓存命中。查询明细被淘汰后不会影响进程累计和 24 小时统计。QPS 使用内核返回的最近一个完整分钟；P95 是对数直方图上界估算。热门域名和客户端的范围是当前保留明细，页面会明确标注。

**DNS 数据只在内存中。** 内核最多保留 4096 条明细，限制为最长 24 小时并受 8 MiB 记账预算约束；趋势使用固定分钟桶，上游保留最多 128 个独立身份和一个溢出合计。记账预算不是内核整个进程 RSS 上限。

浏览器只保留当前页与正在显示的有限聚合，不向 `localStorage`、`IndexedDB` 或 PWA 缓存写 DNS 数据。查询记录默认暂停自动刷新，手动开启实时后也不会累积已看过的页面；离页、隐藏标签页或切换控制器会取消对应请求。常规 reload 保留内核观测，关闭观测释放数据，重新开启或重启从零开始。

旧内核没有这些 API 时，DNS 页会明确提示不支持并停止轮询，其他页面继续工作。默认随 `dns.enable` 收集，可设 `dns.observability: false` 并完整重载关闭。协议和后端边界见 [DNS 状态文档](https://github.com/chummumm/mihomo-dns-optimized/blob/main/docs/dns-observability.md)。

## 安装和更新

源码维护在本仓库的 [`clash-dashboard`](https://github.com/chummumm/mihomo-dns-optimized/tree/clash-dashboard) 分支。构建后的静态文件维护在 [`clash-dashboard-dist`](https://github.com/chummumm/mihomo-dns-optimized/tree/clash-dashboard-dist) 分支，使用独立 UI 工作流，不创建内核发行版。

下载可直接安装的 [UI ZIP](https://github.com/chummumm/mihomo-dns-optimized/archive/refs/heads/clash-dashboard-dist.zip)。解压后，把唯一顶层目录中的 `index.html`、`assets` 等文件放入现有 `external-ui` 目录；先备份旧 UI 文件。

也可以让 Mihomo 使用这个更新地址，保留你当前的 `external-ui` 路径：

```yaml
external-ui-url: "https://github.com/chummumm/mihomo-dns-optimized/archive/refs/heads/clash-dashboard-dist.zip"
```

重新加载配置后，执行 Mihomo 的 UI 更新操作（`POST /upgrade/ui`，使用现有控制器鉴权）。**已有 UI 目录非空时，单纯重启内核不会自动替换旧面板。** 这套原版 UI 自身没有新增更新按钮，也可以直接使用上面的 ZIP 替换文件。

更新后重新打开或强制刷新面板。若浏览器仍缓存旧版，可在开发者工具的 Application → Service Workers 中注销旧 worker 后刷新，无需清除保存控制器设置的 localStorage。新构建使用自动接管更新的 worker，PWA 从当前 UI 路径启动。

静态目录中的 `build-info.json` 标明源码提交与 UI 版本，`SHA256SUMS` 提供各文件校验值。GitHub Actions 的安装包 artifact 另含根目录直接可用的 `clash-dashboard-ui.zip` 及其校验值。

## 构建

使用 Node.js 22、pnpm 8.15.9 和仓库中的锁文件：

```bash
corepack pnpm@8.15.9 install --frozen-lockfile
corepack pnpm@8.15.9 test:unit
corepack pnpm@8.15.9 build
corepack pnpm@8.15.9 exec playwright install chromium
corepack pnpm@8.15.9 test:browser
python3 scripts/package-ui.py --source-sha "$(git rev-parse HEAD)" --repository chummumm/mihomo-dns-optimized
```

UI 工作流只处理 `clash-dashboard` 分支的代码变更及指向该分支的 PR。文档修改不触发构建，PR 不发布静态文件。浏览器回归使用本机模拟控制器，不连接生产设备；发布前验证计数、底部刷新、关闭历史上限、滚动位置、DNS API 兼容和内存数据生命周期。

## 原项目与许可

基于原版 Dreamacro/clash-dashboard 提交 `9a32d9d163ad233141c384ba365c6ef18c58cb94`，从保留完整历史的 [源码备份](https://github.com/chmod777john/clash-dashboard) 导入。保留原作者署名与 MIT 许可，以下为原项目说明。

<h1 align="center">
    <img src="https://github.com/Dreamacro/clash/raw/master/docs/logo.png" alt="Clash" width="200">
    <br>
    Clash Dashboard
    <br>
</h1>

<h4 align="center">Web Dashboard for Clash, now host on ClashX</h4>

<p align="center">
    <a href="https://github.com/Dreamacro/clash-dashboard/actions">
        <img src="https://img.shields.io/github/actions/workflow/status/Dreamacro/clash-dashboard/ghpages.yml?branch=master&style=flat-square" alt="Github Actions">
    </a>
</p>

## Features

  - All ClashX configurations
  - Manage Proxies
  - Manage Proxy Groups
  - Realtime Log

## Progress

See [Projects](https://github.com/Dreamacro/clash-dashboard/projects)

### Start develop with ClashX(Dev Mode)

You can setup your local development environment with [the contribution guide](CONTRIBUTION.md).

```bash
# Enable ClashX with Dev Mode
defaults write com.west2online.ClashX kEnableDashboard -bool YES

# Set dashboard entry
defaults write com.west2online.ClashX webviewUrl "http://localhost:8080/"

# Reset dashboard entry
defaults delete com.west2online.ClashX webviewUrl
```

### Development Env

This command will start Clash Dashboard at `http://localhost:8080/`

```bash
$ pnpm start
```

### Build for production

```bash
$ pnpm build
```

## Contributors

<!-- ALL-CONTRIBUTORS-LIST:START - Do not remove or modify this section -->
<!-- prettier-ignore -->
| [<img src="https://avatars2.githubusercontent.com/u/3380894?v=4" width="100px;"/><br /><sub><b>Jason Chen</b></sub>](https://ijason.cc)<br />[🎨](#design-jas0ncn "Design") [💻](https://github.com/Dreamacro/clash-dashboard/commits?author=jas0ncn "Code") [🐛](https://github.com/Dreamacro/clash-dashboard/issues?q=author%3Ajas0ncn "Bug reports") [🤔](#ideas-jas0ncn "Ideas, Planning, & Feedback") [👀](#review-jas0ncn "Reviewed Pull Requests") [🌍](#translation-jas0ncn "Translation") | [<img src="https://avatars1.githubusercontent.com/u/8615343?v=4" width="100px;"/><br /><sub><b>Dreamacro</b></sub>](https://github.com/Dreamacro)<br />[💻](https://github.com/Dreamacro/clash-dashboard/commits?author=Dreamacro "Code") [🐛](https://github.com/Dreamacro/clash-dashboard/issues?q=author%3ADreamacro "Bug reports") [🤔](#ideas-Dreamacro "Ideas, Planning, & Feedback") [👀](#review-Dreamacro "Reviewed Pull Requests") [🌍](#translation-Dreamacro "Translation") [📦](#platform-Dreamacro "Packaging/porting to new platform") | [<img src="https://avatars1.githubusercontent.com/u/12679581?v=4" width="100px;"/><br /><sub><b>chs97</b></sub>](http://www.hs97.cn)<br />[💻](https://github.com/Dreamacro/clash-dashboard/commits?author=chs97 "Code") [🐛](https://github.com/Dreamacro/clash-dashboard/issues?q=author%3Achs97 "Bug reports") [👀](#review-chs97 "Reviewed Pull Requests") | [<img src="https://avatars3.githubusercontent.com/u/11733500?v=4" width="100px;"/><br /><sub><b>Yicheng</b></sub>](https://github.com/yichengchen)<br />[🤔](#ideas-yichengchen "Ideas, Planning, & Feedback") [📦](#platform-yichengchen "Packaging/porting to new platform") |
| :---: | :---: | :---: | :---: |
<!-- ALL-CONTRIBUTORS-LIST:END -->

## LICENSE

MIT License
