# 从 SmartDNS 迁入原生 DNS

本项目复用 Mihomo 的 `rules` / `rule-providers` 决定业务域名出口。迁移后客户端直接使用 `dns.listen`，不再需要由 SmartDNS 的不同监听端口区分国内 / 海外查询。原 mixed / SOCKS / HTTP 等入站仍可使用；外部明文 53 DNS 分类也保留。

以下是配置能力的对照，不表示 SmartDNS 所有实现细节或 UI 插件都被复制。

## DNS 参数对照

| SmartDNS 设置 | 原生 Mihomo 对应设置 / 行为 |
| --- | --- |
| 默认上游、独立 Global 上游组 | `direct-nameserver` 与 `nameserver`；先按 QNAME 匹配业务规则，再按实际出口类型选池 |
| `server-https` / `server-tls` / QUIC 上游 | 原生 `https://` / `tls://` / `quic://`；继承本次已冻结出口，保留 TLS 证书校验 |
| `speed-check-mode tcp:443,tcp:80,ping` | `speed-check-mode: [tcp:443, tcp:80, ping]`，仅实际 DIRECT / Compatible 进行 IP 探测 |
| `response-mode fastest-ip` | 本项目开启测速后在预算内收集候选，选择成功探测耗时最小的 IP；不另增加同义 `response-mode` 字段 |
| Global `-no-speed-check` | 代理 DNS 路径始终不做本地 IP 测速 |
| `dualstack-ip-selection yes` | `dualstack-ip-selection: true`，仍需启用测速，仅作用于自动 DIRECT 查询 |
| `dualstack-ip-selection-threshold 10` | 同名字段，毫秒，默认 10，允许 0–1000 |
| `dualstack-ip-allow-force-AAAA no` | `dualstack-ip-allow-force-aaaa: false`，默认保留 IPv4 |
| Global `-no-dualstack-selection` | 代理路径不做双栈探测，也不为了本地优选额外查询另一族 |
| `prefetch-domain yes` | `prefetch-domain: true`；采用本项目有界、带真实来源的热点预取机制 |
| `serve-expired yes` | `serve-expired: true` |
| `serve-expired-ttl 604800` | 同名字段，秒，允许最多使用过期 7 天的回答 |
| `serve-expired-reply-ttl 0` | 同名字段，可显式设为 0；不延长原缓存过期点 |
| `cache-size 384000` | `cache-max-size: 384000`；该容量用于各 resolver 的缓存，非预分配相同数量的记录 |
| `cache-persist no` | 使用内存 DNS 缓存；无需新增持久化开关 |
| `rr-ttl-min 0` / `rr-ttl-max 3600` | 同名字段；0 表示不限制对应边界，单位秒 |
| `force-no-CNAME yes` | `force-no-cname: true`；压平完整、无歧义、未签名的 A / AAAA 回答链 |
| `resolv-file /etc/resolv.conf` | 建议显式配置 `default-nameserver` 与 `proxy-server-nameserver`，避免把系统 DNS 再指回本机监听而循环 |

`force-no-cname`、`dualstack-ip-allow-force-aaaa` 使用全小写的 Mihomo YAML 字段名。SmartDNS 的空格参数、`yes/no` 和混合大小写字段不能整段直接粘贴进 YAML。

### 测速与双栈的边界

`fastest-ip` 迁移的是在候选中优选最快可达 IP 的目标。本项目的可选优选预算默认 1000 毫秒；到期仍无合格 DNS 答复时，已有 DNS 查询继续使用正常解析期限，不重发整批上游。候选数与活动探测数有上限，探测满额立即跳过，不等待名额。候选来自一份已通过原版过滤的完整回答，不拼接不同上游的 CNAME 链。全部探测失败而已有有效回答时返回原回答。两者的具体等待策略并不完全相同。

双栈查询共用同一次选路、同一解析器池、一个总超时预算和共享探测并发上限。默认只在 AAAA 查询中辅助查询 A，只有两族均有可用且成功测速的结果、IPv4 快至少设定阈值时，才对 AAAA 返回 NOERROR / NODATA。开启 `dualstack-ip-allow-force-aaaa` 后才允许反向过滤 A。

这种临时过滤回答带零 TTL，不写入缓存，避免短暂的网络差异被 `serve-expired` 延续数天。辅助查询不单独填充另一族缓存。DNSSEC、某族查询失败、某族没有地址或测速全部失败时，不以失败冒充“更慢”去过滤原有效回答。此实现不复制 SmartDNS 的所有跨族缓存协同机制。

原生上游用 DoH 或 DoT，并不改变是否测速的判据：**实际 DIRECT 才可能测速，实际代理永远不做本地 IP 测速**。显式 `#Group` / `#DIRECT` 等独立传输例外不参加自动优选。

### 回答改写与 TTL 的边界

TTL 调整在写缓存前完成；零 TTL 仍保持不可缓存。过期回答使用 `serve-expired-reply-ttl`，不会重新套用 TTL 下限。节点域名和解析器 bootstrap 不继承业务回答调整。

去 CNAME 仅在同一回答中已经存在完整 A / AAAA 链时，把终点地址改写到查询名，并采用整条链的最短 TTL；不会补造地址或为隐藏 CNAME 重新解析。CNAME 类型查询、DNAME、断链、循环、歧义链及额外不相关的 Answer 记录保留原形。

DO / CD 查询以及 AD、RRSIG、SIG、TSIG 等受保护回答不进行地址优选、CNAME 压平或 TTL 改写。这是明确的保守边界，不宣称对所有回复实现无条件的 `force-no-CNAME`。本功能传递 DNSSEC 数据，不验证 DNSSEC 签名。

### 缓存与预取的边界

缓存先按完整来源及端口执行规则，再按解析器池、实际出口、来源 IP / 进程 / 入站作用域和查询内容隔离。成功的自动应答可跨临时源端口复用；在途查询仍独立管理取消。迁移相同 `cache-size` 不代表两者的内存用量或命中率相同。主动预取要求同一作用域近期至少 3 次真实查询，最多 1024 个热点 / resolver 和 16 个全局后台任务；这是防止失控和错误来源复用的边界。

Mihomo 的 `serve-expired-ttl: 0` 在本项目表示不限制过期年龄，保留本项目之前的默认行为；迁移时应明确填写需要的正数上限，不能假定它等于 SmartDNS 每个版本的零值语义。

## 域名屏蔽清单

把 SmartDNS 域名清单放进单独的普通 Mihomo domain provider 文件，主配置只用一条原生规则引用。例如主配置：

```yaml
rule-providers:
  custom_block:
    type: file
    behavior: domain
    format: yaml
    path: ./rule_provider/smartdns_block.yaml

rules:
  - RULE-SET,custom_block,REJECT
  # 后面继续原有直连、代理、规则集与 MATCH。
  - MATCH,DIRECT
```

对应 `rule_provider/smartdns_block.yaml`：

```yaml
payload:
  - '+.blocked.example'
  - '+.telemetry.example'
```

两份文件维持上述相对目录，规则清单仍只有这一份。需要维护屏蔽域名时修改规则集文件，不再同步一份 SmartDNS 域名策略。

`+.` 覆盖指定域名及其子域名。把拦截规则放到应当优先的业务规则位置，它就与其他规则共同使用 Mihomo 现成的匹配算法。自动 DNS 路由在缓存前处理 REJECT，返回 REFUSED 且不查询远端；有域名信息的业务连接同样会被该规则拒绝。SmartDNS 的 `-address #` 通常以 SOA 空回答阻断，两者 DNS 返回码不同，均不返回被阻断域名的公网地址。

这些业务规则遵守既有优先级：Global / Direct 或固定入站出口不被新增自动路由改写。不要另复制到 `nameserver-policy`；开启 `dns-rule-routing` 后该字段停用。

## 运维设置

SmartDNS 的两个监听、SOCKS 回送端口和进程用户不需要照搬。原生方案使用已有 Mihomo 服务、`dns.listen`、API 和面板；SmartDNS 的 `smartdns_ui.so`、UI 账户、历史查询数据库、日志轮转参数不作为 Mihomo DNS 配置字段迁移。

新安装包只包含通用起始配置，不包含个人节点或屏蔽清单。安装位置、配置保留及手动启停说明见[安装包文档](releases.md)。改用原生 DNS 前先执行 `mihomo -t`，确认标准 53 端口由计划中的服务监听，并让客户端使用该地址。

原 SmartDNS 参数语义参考其[官方配置表](https://pymumu.github.io/smartdns/en/configuration/)、[测速模式](https://pymumu.github.io/smartdns/en/config/check-speed-mode/)和[双栈说明](https://pymumu.github.io/smartdns/en/config/dualstack/)。实际实现与限制以本项目文档和测试为准。
