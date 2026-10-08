# DNS Route Kernel

基于 [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) 稳定版维护的独立派生项目，增加与 `mixed-port` 同级的 **DNS 专用混合代理端口 `dns-proxy-port`**。

SmartDNS 经此入口发送到 `IP:53` 的普通 DNS 查询，会读取每条查询的域名，复用现有 `rules` / `rule-providers` 选择出口。同一 TCP 连接或 UDP 会话中的不同域名也独立分流；其他目标端口在拨号前拒绝。无需 FakeIP，无需另写一份 DNS 域名规则。

```yaml
mixed-port: 7890
dns-proxy-port: 7853
```

配置非零端口即启用，设为 `0` 或省略即关闭，与 `mixed-port` 一致。HTTP CONNECT、SOCKS4/4a、SOCKS5 TCP 和 UDP 均默认支持，无额外协议开关；认证、LAN 访问、绑定地址等沿用原版全局配置。该端口只接受发往解析器字面量 IP 的 `53` 端口的有效 DNS。

正在交换的 DNS 查询接入原版面板连接列表与流量统计，展示查询域名、解析器、规则和出口链，并支持关闭。查询完成后移出活动列表；短查询可能在面板两次刷新之间结束。

- [配置与工作原理](docs/dns-proxy.md)
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
