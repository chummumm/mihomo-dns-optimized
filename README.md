# DNS Route Kernel

基于 [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) 稳定版维护的独立派生项目，通过一个全局开关，让普通 DNS 查询按查询域名复用现有 `rules` / `rule-providers` 分流，无需另外维护一份 DNS 域名规则。

```yaml
mixed-port: 7890
mode: rule
dns-rule-routing: true
```

`dns-rule-routing` 默认 `false`。开启后，在 Rule 模式且入站没有固定 `proxy:` 时，普通入站中发往解析器 `53` 端口的明文 TCP / UDP DNS 会逐条查询选路。同一 TCP 连接或 UDP 会话里的不同域名可以走不同出口。**非 53 流量和首份负载未识别为普通 DNS 的流量继续走原流程。** 不增加监听端口或 listener 类型，Global / Direct 模式和固定出站保留原行为。

SmartDNS 可以继续监听 `6053` / `6553`，通过现有 `mixed-port: 7890` 发送普通 DNS 上游查询。内置 `dns.listen` 和 TUN DNS 劫持继续提供本地 DNS 服务，其符合范围的上游查询共享相同选路核心。外部 DNS 识别不要求启用内置 DNS，也不依赖普通 sniffer。此功能不使用 FakeIP；启用内置 DNS 时使用 `redir-host`。

匹配时，域名来自当前 QNAME；网站目标 IP 尚未知，不能拿解析器 IP 代替。来源、入站、进程、端口和网络信息仍按真实查询保留，并按原规则顺序参与匹配。选中策略组后固定本次查询的实际出口，再向原解析器发送 DNS；不会为选路再次解析待查域名。

报文解析复用项目已有的 `miekg/dns`，并检查完整长度、记录计数、OPT 结构及响应关联。支持普通 HTTPS / SVCB 查询、DNSSEC 数据及 EDNS 扩展；DoH / DoT / DoQ 不属于本功能的自动分类范围，仍按原有连接处理。正在交换的查询接入原版连接面板和流量统计；短查询可能在两次面板刷新之间结束。

- [配置与工作原理](docs/dns-proxy.md) · [完整行为约定](docs/dns-rule-routing-design.md)
- [Mihomo 最小配置](docs/dns-proxy.example.yaml) · [SmartDNS 接入示例](docs/smartdns-dns-proxy.conf)
- [云编译](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/build.yml) · [下载发布](https://github.com/chummumm/mihomo-dns-optimized/releases)
- [稳定版上游同步](https://github.com/chummumm/mihomo-dns-optimized/actions/workflows/sync-upstream.yml)
- [编译产物、发布与同步说明](docs/upstream-sync.md)

GitHub Actions 构建 Linux amd64（v1 指令集）和 arm64，包含 `with_gvisor`。`main` 更新且验证成功后自动发布下载包。每日北京时间约 04:23 检查上游稳定版，先合并、测试并编译，再更新本仓库。冲突、测试失败或分支并发变更会停止同步，不覆盖本仓库改动。

以下保留上游项目说明、致谢和许可声明。此派生项目与 MetaCubeX 团队无隶属关系。

---

<h1 align="center">
  <img src="Meta.png" alt="Meta Kennel" width="200">
  <br>Meta Kernel<br>
</h1>

<h3 align="center">Another Mihomo Kernel.</h3>

<p align="center">
  <a href="https://goreportcard.com/report/github.com/MetaCubeX/mihomo">
    <img src="https://goreportcard.com/badge/github.com/MetaCubeX/mihomo?style=flat-square">
  </a>
  <img src="https://img.shields.io/github/go-mod/go-version/MetaCubeX/mihomo/Alpha?style=flat-square">
  <a href="https://github.com/MetaCubeX/mihomo/releases">
    <img src="https://img.shields.io/github/release/MetaCubeX/mihomo/all.svg?style=flat-square">
  </a>
  <a href="https://github.com/MetaCubeX/mihomo">
    <img src="https://img.shields.io/badge/release-Meta-00b4f0?style=flat-square">
  </a>
</p>

## Features

- Local HTTP/HTTPS/SOCKS server with authentication support
- VMess, VLESS, Shadowsocks, Trojan, Snell, TUIC, Hysteria protocol support
- Built-in DNS server that aims to minimize DNS pollution attack impact, supports DoH/DoT upstream and fake IP.
- Rules based off domains, GEOIP, IPCIDR or Process to forward packets to different nodes
- Remote groups allow users to implement powerful rules. Supports automatic fallback, load balancing or auto select node
  based off latency
- Remote providers, allowing users to get node lists remotely instead of hard-coding in config
- Netfilter TCP redirecting. Deploy Mihomo on your Internet gateway with `iptables`.
- Comprehensive HTTP RESTful API controller

## Dashboard

A web dashboard with first-class support for this project has been created; it can be checked out at [metacubexd](https://github.com/MetaCubeX/metacubexd).

## Configration example

Configuration example is located at [/docs/config.yaml](https://github.com/MetaCubeX/mihomo/blob/Alpha/docs/config.yaml).

## Docs

Documentation can be found in [mihomo Docs](https://wiki.metacubex.one/).

## For development

Requirements:
[Go 1.20 or newer](https://go.dev/dl/)

Build mihomo:

```shell
git clone https://github.com/MetaCubeX/mihomo.git
cd mihomo && go mod download
go build
```

Set go proxy if a connection to GitHub is not possible:

```shell
go env -w GOPROXY=https://goproxy.io,direct
```

Build with gvisor tun stack:

```shell
go build -tags with_gvisor
```

### IPTABLES configuration

Work on Linux OS which supported `iptables`

```yaml
# Enable the TPROXY listener
tproxy-port: 9898

iptables:
  enable: true # default is false
  inbound-interface: eth0 # detect the inbound interface, default is 'lo'
```

## Debugging

Check [wiki](https://wiki.metacubex.one/api/#debug) to get an instruction on using debug
API.

## Credits

- [Dreamacro/clash](https://github.com/Dreamacro/clash)
- [SagerNet/sing-box](https://github.com/SagerNet/sing-box)
- [riobard/go-shadowsocks2](https://github.com/riobard/go-shadowsocks2)
- [v2ray/v2ray-core](https://github.com/v2ray/v2ray-core)
- [WireGuard/wireguard-go](https://github.com/WireGuard/wireguard-go)
- [yaling888/clash-plus-pro](https://github.com/yaling888/clash)

## License

This software is released under the GPL-3.0 license.

**In addition, any downstream projects not affiliated with `MetaCubeX` shall not contain the word `mihomo` in their names.**
