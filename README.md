# Clash Dashboard：Mihomo 嗅探域名显示兼容版

保留原版 Clash Dashboard 的界面，补齐 Mihomo 的 `sniffHost` 显示支持。连接列表、主机名排序和连接详情统一按 `host → sniffHost → destinationIP` 取值；原目标域名优先，旧内核没有 `sniffHost` 时仍正常工作。

修改只影响显示，不会写回连接元数据，不会改变实际连接目标或 DNS 分流。使用 `enhanced-mode: normal` 和 `override-destination: false` 时，已经嗅探到的域名也能显示；未获得任何域名的连接仍显示 IP。

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
corepack pnpm@8.15.9 build
python3 scripts/package-ui.py --source-sha "$(git rev-parse HEAD)" --repository chummumm/mihomo-dns-optimized
```

UI 工作流只处理 `clash-dashboard` 分支的代码变更及指向该分支的 PR。文档修改不触发构建，PR 不发布静态文件。

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
