package tunnel

import (
	"fmt"

	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/dnsstats"
	C "github.com/metacubex/mihomo/constant"
)

// dnsObservedOutbound groups the same configured leaf and effective socket
// policy across reloads. Cache and transport keys retain their separate object
// and epoch boundaries; those lifetimes must not split process statistics.
// Opaque selectors and an unfrozen dialer-proxy cannot prove the actual exit.
func dnsObservedOutbound(proxy C.ProxyAdapter) dnsstats.Outbound {
	if proxy == nil {
		return dnsstats.Outbound{}
	}
	if wrapper, ok := proxy.(C.Proxy); ok {
		proxy = wrapper.Adapter()
	}
	if proxy == nil {
		return dnsstats.Outbound{}
	}
	info := proxy.ProxyInfo()
	switch proxy.Type() {
	case C.Relay, C.Selector, C.URLTest, C.Fallback, C.LoadBalance, C.Reject, C.RejectDrop, C.Dns, C.Rematch, C.Pass, C.PassRule:
		return dnsstats.Outbound{}
	}
	if info.DialerProxy != "" {
		return dnsstats.Outbound{}
	}
	iface, mark := info.Interface, info.RoutingMark
	if iface == "" {
		iface = dialer.DefaultInterface.Load()
	}
	if mark == 0 {
		mark = int(dialer.DefaultRoutingMark.Load())
	}
	kind := "proxy"
	if proxy.Type() == C.Direct || proxy.Type() == C.Compatible {
		kind = "direct"
	}
	identity := ""
	if source, ok := proxy.(interface{ DNSStatsIdentity() string }); ok {
		identity = source.DNSStatsIdentity()
	}
	if identity == "" {
		// An externally constructed adapter may hide credentials or dialer policy.
		// Its display name/address cannot establish equivalence with another one.
		identity = fmt.Sprintf("instance:%T:%p", proxy, proxy)
	}
	return dnsstats.Outbound{
		Key:  fmt.Sprintf("native-leaf:%q|%d|%#v|interface:%q|mark:%d", identity, proxy.Type(), info, iface, mark),
		Name: proxy.Name(), Type: kind,
	}
}

func (p *DNSRoutingPlan) ObservabilityOutbound() dnsstats.Outbound {
	if p == nil {
		return dnsstats.Outbound{}
	}
	return dnsObservedOutbound(p.route.proxy)
}

// A transport bound to a frozen plan has a known exit even on a reused
// connection. An explicit terminal adapter is also known. Looking up a dynamic
// configured name here would make a second, potentially different selection.
func (d *DNSDialer) ObservabilityOutbound() dnsstats.Outbound {
	if d == nil {
		return dnsstats.Outbound{}
	}
	if d.plan != nil {
		return d.plan.ObservabilityOutbound()
	}
	return dnsObservedOutbound(d.proxyAdapter)
}
