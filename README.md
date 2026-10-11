# mihomo-optimized：流光面板与 DNS 状态

mihomo-optimized 的配套网络控制台，基于原版 Clash Dashboard 维护，采用“流光”界面，保留纯内存 DNS 状态展示、连接列表刷新与计数修复。侧栏、代理、连接、日志、规则、DNS 和设置使用同一套面板、读数与青蓝流光视觉，支持系统深浅色和减少动态效果偏好。连接列表、主机名排序和连接详情统一按 `host → sniffHost → destinationIP` 取值；原目标域名优先，旧内核没有 `sniffHost` 时仍正常工作。

连接展示不会写回连接元数据，不会改变实际连接目标或 DNS 分流。使用 `enhanced-mode: normal` 和 `override-destination: false` 时，已经嗅探到的域名也能显示；未获得任何域名的连接仍显示 IP。

自 `0.4.1` 起，代理节点、按钮、卡片、面板和导航等常驻区域不再使用背景模糊，通用面板底色在深浅主题中均为 90% 不透明度；页面入场保留透明度与位移，新连接行入场保留透明度，二者均去掉模糊。下拉菜单、提示消息和模态窗口等覆盖内容的浮层仍保留原有背景模糊。连接表继续按可见区域渲染，代理节点卡片仍按原列表渲染。

[内核完整配置示例](https://github.com/chummumm/mihomo-optimized#完整配置示例) · [YAML 自动更新设置](#自动更新配置) · [旧配置迁移说明](https://github.com/chummumm/mihomo-optimized#旧配置迁移与停用范围)

## 代理与实时流量拓扑

代理页顶部的流量拓扑按“来源 → 策略组 → 出口节点”画出当前活动连接，连线粗细和流动光点随实时速率变化，悬停可查看完整名称并突出该节点的链路，点击策略组或出口节点会跳到对应卡片。拓扑只读复用连接页同一个 `/connections` 推送，仅在拓扑展开时订阅，收起后立即断开；聚合只在内存中进行，不写回连接数据。来源、策略组和出口各最多显示前几项，其余合并为“其他”。拓扑滚出视口或页面进入后台时停止绘制并回收旧节点、连线和粒子，重新可见后恢复动效；长名称使用二分截断和有限测量缓存。策略组通过共享索引解析当前出口，支持深层嵌套并检测循环，不按节点名称前缀猜测拒绝类型。

自面板 `0.4.3` 起，拓扑将标题和连线缓存到离屏图层，卡片、状态点与流动粒子复用预绘制图像；流量、布局、悬停、主题或字体变化时更新对应图层和图像。节点排序移动、链路流光、测速反馈及悬停点击交互继续保留；不支持 `OffscreenCanvas` 时使用普通 Canvas 回退。拓扑不可见时释放连线图层，卸载时释放全部绘图缓存。

策略组卡片以圆环显示当前出口延迟，切换后名称滑入、选中外框滑到新节点；节点卡片显示最近延迟走势，测速中有扫描光带，结果返回时按状态色闪亮一次。全部测速时按钮显示进度。

## 连接列表

“全部（N）”、`mihomo`、未知来源和各设备 IP 从同一份保留记录计算。每条连接恰好进入一个来源分组，分组数量直接取其筛选结果长度；一次线性聚合建立索引，切换来源无需逐个设备重复扫描或排序整份记录。真实内置 DNS 上游连接归 `mihomo`，包括保留客户端来源 IP 的上游连接；其余 `Inner` 且没有有效客户端 IP 的记录也归 `mihomo`。带有效来源 IP 的客户端逻辑查询仍归对应设备，普通连接缺失来源时归未知来源。

`mihomo` 有连接时固定在“全部”后面的第二项，不随设备出现顺序或实时刷新移动。数量为 `0` 的来源分组不保留，归零时播放原有退出动画后移除；退场中的按钮立即隐藏旧计数并停止交互。当前选中的分组归零后自动回到“全部”。计数直接读取现有来源索引，不额外扫描连接记录；分组入场、位置调整、选中高亮和退出动效均保留。

开启“保留关闭连接”时，各组和全部都包含尚未淘汰的关闭记录；关闭历史最多保留 5000 条，活跃连接完整计数。面板收到快照后在同一帧更新，兼容旧内核空快照中的 `connections: null`。切换控制器后恢复到全部。

长列表使用已有的 `react-window` 按可见区域渲染。刷新时通过连接 ID 保持阅读位置，停在最底部时保持底部；列宽、横向滚动和固定主机名列继续可用。连接 WebSocket 不再额外缓存 200 份完整快照，关闭记录独立维护，避免每次刷新重新判定全部历史的状态。

连接页统计的是浏览器当前保留集合，不是内核启动以来的累计连接数。关闭页面或刷新浏览器后不会从磁盘恢复历史。

页面顶部的上下行速率由相邻两次快照的累计流量差计算，数字直接显示当前值，不再滚动过渡；实时走势图及其他流光、分组与卡片动画保留。最近 60 秒走势只在本页内存中保留；来源列表的圆环表示各来源占当前实时流量的比例。这些读数只读取同一推送，不改变连接记录、计数或筛选。

DNS 连接在列表和详情中统一显示类型 `DNS`，节点链只显示实际选定的最终节点（直连显示 `DIRECT`），不显示匹配规则和代理组。仅当内核明确返回 `dns: true` 时才识别为 DNS，不根据目的端口、入口类型或入口名称推测；普通 SOCKS/HTTP 到目标 53 的连接与没有该标记的逻辑查询保持原有显示。原始连接元数据、完整节点链和规则仍保留在控制 API 中，显示简化不会改变解析或路由。DNS 查询记录页中的 A、AAAA、HTTPS 等记录类型保持不变。

## 移动界面与手动操作

移动端导航可进入连接页，详情宽度适应窄屏，保留进程列和现有虚拟滚动。详情显示按当前面板语言与浏览器时区格式化的连接开始时间，列表相对时间的提示也包含完整时间。

设置页增加“重新加载配置”，点击后重载当前控制器正在使用的配置路径，显示成功或错误结果。请求进行中禁止重复点击，切换控制器或离开设置页后取消旧请求的界面回调。链接通过 `host`、`port`、`secret` 或 `protocol` 指定控制器时，设置页显示实际请求地址与链接指定提示，不提供无效的已保存控制器切换；显式 HTTPS 协议不会降为 HTTP。

手动节点测速可在设置页保存 HTTP/HTTPS URL，默认 `https://www.gstatic.com/generate_204`，可一键恢复。`PASS` 等内置策略不参与测速；同一节点卡片已有请求时不重复发起，切换控制器或离页会取消旧请求，旧结果不会写入另一个控制器的同名节点。本地延迟历史最多保留 10 条。ClashX 原生测速和代理集健康检查沿用各自设置。

测速 URL 属于原有浏览器界面偏好设置；DNS 查询和连接记录仍只存在内存。提示消息使用 React 18 根正确卸载，并清理定时器；关闭动画未触发时也会释放消息节点。

## 配置编辑

设置页的“编辑配置文件”读取当前控制器正在使用的 YAML 文件，支持行号、光标位置和 Ctrl-S / Cmd-S。使用本仓库自 `mihomo-optimized-v1.0.0` 起提供的 `/configs/file` API，并沿用控制器鉴权。使用原有的 `external-controller` / `secret` 配置连接面板即可，无需新增配置编辑开关。

自面板 **`0.4.2`** 起，主按钮为“保存并重新加载”，Ctrl-S / Cmd-S 执行相同操作。面板先保存文件，成功后再请求内核完整校验和重载，只有两步都成功才显示完成。编辑入口使用铅笔图标，保存操作使用磁盘图标，文件路径旁使用文件图标；图形及悬停、点击动效采用流光设计，并遵循减少动态效果偏好。

组合操作使用 `PUT /configs?force=true`，使 HTTP / SOCKS / mixed 等监听端口、绑定地址和 `allow-lan` 随配置重载应用。控制器地址、控制器密钥等启动设置仍需重启内核后生效。原有独立“重新加载配置”入口保持原行为。

保存仍检查单个 YAML 文档及基础字段类型，`PUT /configs/file` 接口自身只负责写入；读取或调用该接口本身不会重载。若文件已保存但完整校验或重载失败，编辑器保留新的文件内容及修订标识，明确提示保存结果，允许修改后再次保存，或点击“重试重新加载”。重试会先重新检查修订标识，避免忽略其他程序的文件修改；内容相同时内核不重复写文件。网络中断时不会声称运行配置未改变，也不会自动回滚已保存文件。重载成功后的设置刷新失败不影响成功提示。

编辑器草稿与修订标识仅在内存中，关闭或切换控制器即释放，不写浏览器存储或日志。保存使用修订冲突检查，文件已被其他程序改动时保留草稿并提示重新读取，不覆盖新版文件；未保存即关闭会要求确认。单文件上限 4 MiB，行号只渲染可见部分，光标通过行起点索引二分定位。

内核仅允许编辑当前路径的普通 UTF-8 文件，不接受任意文件路径；加密文件及包含符号链接的路径暂不支持。写入采用同目录临时文件与替换，保留文件权限，无法安全保存时保留原文件。API 响应使用 `no-store`，错误不回显配置片段。嵌入模式禁用此接口。

## 日志等级

日志页的等级选择器控制面板的日志订阅过滤。`Default` 跟随内核当前全局等级；选择 `Debug`、`Info`、`Warn`、`Error` 或 `Silent` 时使用指定等级。即使本二开内核全局设为 `silent`，仍可在面板选择 `Debug` 查看调试日志。

切换只更新 `/logs?level=...` 的日志订阅，不修改内核全局配置、不重载配置，也不清 DNS 答案缓存、候选缓存、查询历史或统计，不中断现有代理连接。`Silent` 关闭面板日志订阅，不建立无用的静默连接；已显示的记录保留。面板最近日志和后台日志缓冲各最多保留 200 条，仅保存在内存中；浏览器持久化的只有等级等界面偏好。日志自动滚动或下拉列表自身滚动不会关闭正在选择的菜单。

“暂停”只冻结显示：订阅照常进行，期间到达的新日志最多暂存 200 条并显示数量，“继续”后接到末尾并滚到最新。“清空”以点击时刻为界清空当前标签页的记录；退场动画期间新到的日志继续保留，离开再回到日志页也不会重新显示已清空的记录；不会断开订阅、修改内核或写入浏览器存储。

## DNS 概览、记录和上游

DNS 页由本二开内核的 `/dns/observability` API 提供数据：

| 页面 | 显示内容 |
| --- | --- |
| 概览 | 最近 24 小时查询量、QPS、缓存命中率、新鲜/过期缓存命中数、错误、平均耗时和估算 P95、分钟趋势、热门域名/客户端 |
| 查询记录 | 原始查询名、客户端、类型、入口、结果、返回码、耗时、有限答案摘要；精确过滤与 50/100 条游标翻页 |
| 上游 | 按解析器与实际出站统计交换尝试、成功、错误、取消、超时、失败返回码、成功平均耗时与已完成成功率 |

缓存命中率为同一窗口的 `(新鲜缓存命中 + 过期缓存命中) / 已完成客户端 DNS 查询总数`；hosts、Fake IP、测速候选缓存不计入正式 DNS 缓存命中。查询明细被淘汰后不会影响进程累计和 24 小时统计。QPS 使用内核返回的最近一个完整分钟；P95 是对数直方图上界估算。热门域名和客户端的范围是当前保留明细，页面会明确标注。

**DNS 数据只在内存中。** 内核最多保留 4096 条明细，限制为最长 24 小时并受 8 MiB 记账预算约束；趋势使用固定分钟桶，上游保留最多 128 个独立身份和一个溢出合计。记账预算不是内核整个进程 RSS 上限。

新内核提供成功、错误和取消各自的平均耗时：主耗时列仅统计成功，错误与取消下方分别显示其平均等待时间。成功次数为零时显示“—”。旧内核只提供混合均值时，表头明确显示“综合平均耗时”。实际出站由内核上报，未知时显示未归属，不根据规则、端口或解析器地址猜测 DIRECT。

浏览器只保留当前页与正在显示的有限聚合，不向 `localStorage`、`IndexedDB` 或 PWA 缓存写 DNS 数据。查询记录默认暂停自动刷新，手动开启实时后也不会累积已看过的页面；离页、隐藏标签页或切换控制器会取消对应请求。常规 reload 保留内核观测，关闭观测释放数据，重新开启或重启从零开始。

旧内核没有这些 API 时，DNS 页会明确提示不支持并停止轮询，其他页面继续工作。默认随 `dns.enable` 收集，可设 `dns.observability: false` 并完整重载关闭。协议和后端边界见 [DNS 状态文档](https://github.com/chummumm/mihomo-optimized/blob/main/docs/dns-observability.md)。

## 安装和更新

面板版本使用纯 `x.y.z` 格式（当前 `0.4.3`），产品与构建元数据名称统一为 **`mihomo-optimized`**。请搭配本仓库 `mihomo-optimized-v1.0.0` 或后续内核使用；下面的启动时自动检查行为从内核 `mihomo-optimized-v1.0.1` 起提供。

源码维护在本仓库的 [`clash-dashboard`](https://github.com/chummumm/mihomo-optimized/tree/clash-dashboard) 分支。构建后的静态文件发布到固定的 [`clash-dashboard-dist`](https://github.com/chummumm/mihomo-optimized/tree/clash-dashboard-dist) 分支。内核固定从本仓库获取面板，并管理核心工作目录下的 **`./ui`**（跟随 `-d`）；在控制器地址的 `/ui/` 打开面板即可。旧 `external-ui`、`external-ui-url`、`external-ui-name` 和命令行 `-ext-ui` 均被忽略，不改变目录或下载来源。

自面板 `0.4.0` 起，不再提供面板路径、下载 URL、面板或内核的手动检查、安装、自动更新开关、间隔和更新状态区域。相关 Provider、API 调用、计时器和事件处理已删除；打开面板、切换控制器、重新联网和时间推移均不会发起 `/upgrade` 请求。旧版浏览器保存的自动更新偏好不会生效。内核的配置仍可通过 YAML 编辑器读取、修改，并使用“保存并重新加载”应用。

### 自动更新配置

把下面四项放在内核 YAML **顶层，与 `dns:` 同级**；示例显式开启面板和内核自动更新，可按需只开启其中一项：

```yaml
external-ui-auto-update: true
external-ui-update-interval: 24
core-auto-update: true
core-update-interval: 24
```

两个开关均默认 `false`，两个检查间隔均默认 `24`，单位是**整数小时**，有效范围为 `1`–`2562047`。写 `24`，不要写 `24h`、`"24"`、小数或 `0`。设置 `false` 并完整重载可停止对应的自动更新；“保存并重新加载”成功后策略随重载生效。直接修改磁盘 YAML 或仅调用保存接口仍需另行重载。

从内核 `1.0.1` 起，已开启的项目在进程启动或重启、网络与配置应用完成后，立即异步检查一次，之后按各自间隔检查。运行中从关闭改为开启也立即检查；相同策略的完整重载不会重复检查或重置计时器；保持开启而只修改间隔时，从该次重载按新间隔计时，不追加一次立即检查。

面板通过远端和本地 `build-info.json` 的版本号判断，内核通过当前实际版本与本仓库正式 release 版本判断；只有远端版本严格更高时才自动安装，相同、旧版或无法可靠比较的版本均跳过。面板安装完成不重启内核，内核升级完成会自动重启。首次运行时，若受管 `./ui` 目录不存在或为空，内核仍会自动下载初始面板，即使 `external-ui-auto-update` 为 `false`；此后已安装面板是否自动升级才由该开关控制。

DNS 新增与调整字段、完整可复制 YAML、共享连接的 `#disable-reuse=true` 例外，以及永久忽略和仅在 `dns-rule-routing` 开启时停用的旧字段，集中见[内核 README 配置入口](https://github.com/chummumm/mihomo-optimized#完整配置示例)和[迁移表](https://github.com/chummumm/mihomo-optimized#旧配置迁移与停用范围)。日志等级过滤与手动节点测速 URL 仍是浏览器偏好，没有新增对应 YAML 键。

浏览器继续使用当前站点的 service worker 缓存静态资源。它只处理网页资源，不发起内核或面板安装请求；面板不会因后台安装主动刷新正在编辑的页面。编辑器原有未保存离页保护、关闭确认及冲突草稿保留均继续有效。若浏览器保留旧资源，可重新打开页面或使用浏览器刷新。

静态目录中的 `build-info.json` 标明源码提交与 UI 版本，`SHA256SUMS` 提供各文件校验值。独立 UI 工作流不创建内核发行版；其构建 artifact 提供 `mihomo-optimized-ui.zip` 及校验值。

## 构建

使用 Node.js 22、pnpm 8.15.9 和仓库中的锁文件：

```bash
corepack pnpm@8.15.9 install --frozen-lockfile
corepack pnpm@8.15.9 test:unit
corepack pnpm@8.15.9 build
corepack pnpm@8.15.9 exec playwright install chromium
corepack pnpm@8.15.9 test:browser
python3 scripts/package-ui.py --source-sha "$(git rev-parse HEAD)" --repository chummumm/mihomo-optimized
```

UI 工作流只处理 `clash-dashboard` 分支的代码变更及指向该分支的 PR。文档修改不触发构建，PR 不发布静态文件。浏览器回归使用本机模拟控制器，不连接生产设备；发布前验证来源计数与筛选、底部刷新、关闭历史上限、滚动位置、DNS API 兼容与内存生命周期，以及移动界面、重载、测速和控制器切换边界。

日志回归另验证全局 `silent` 下的等级选择、鼠标与键盘操作、菜单滚动、200 条内存上限，以及切换等级不会发送配置修改或缓存清理请求。配置编辑回归覆盖先保存后重载、两个阶段防重复、部分成功与重试、冲突保留草稿、控制器切换、错误兼容及图标显示。更新移除回归验证两种语言均没有更新控件，多个页面、控制器切换、联网事件和长时间运行均不发起更新请求，同时验证 YAML 保存重载与草稿保护仍可用。

## 原项目与许可

基于原版 Dreamacro/clash-dashboard 提交 `9a32d9d163ad233141c384ba365c6ef18c58cb94`，从保留完整历史的 [源码备份](https://github.com/chmod777john/clash-dashboard) 导入。保留原作者署名与 MIT 许可，以下为原项目说明。

界面字体 Sora / Martian Mono 使用 SIL OFL，内联 SVG 图标来自 Phosphor Icons，使用 MIT 许可。字体与图标均随静态文件提供，运行时无需请求外部字体或图标服务；对应许可保留在 `public/licenses/`，构建后包含在 `dist/licenses/`。

移动连接入口、开始时间、配置重载与 PASS 不测速等功能参考 OpenClash 使用的 [ayanamist/clash-dashboard `my` 分支](https://github.com/ayanamist/clash-dashboard/tree/5bb32ccb2cc7bbeafb543360af9094a6f2a60b7c)。按本仓库的 React 18、纯内存观测与虚拟滚动实现整合，保留进程信息与双语界面。

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
