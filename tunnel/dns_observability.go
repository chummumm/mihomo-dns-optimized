package tunnel

import (
	"fmt"

	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/dnsstats"
	C "github.com/metacubex/mihomo/constant"
)

// dnsObservedOutbound uses the same physical leaf, effective socket policy and
// replacement epoch as native transport reuse, without rule/group identities.
// Opaque selectors and an unfrozen dialer-proxy cannot prove the actual exit.
func dnsObservedOutbound(proxy C.ProxyAdapter, epoch uint64) dnsstats.Outbound {
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
	return dnsstats.Outbound{
		Key:  fmt.Sprintf("native-leaf:%T:%p|%d|%#v|epoch:%d|interface:%q|mark:%d", proxy, proxy, proxy.Type(), info, epoch, iface, mark),
		Name: proxy.Name(), Type: kind,
	}
}

func (p *DNSRoutingPlan) ObservabilityOutbound() dnsstats.Outbound {
	if p == nil {
		return dnsstats.Outbound{}
	}
	return dnsObservedOutbound(p.route.proxy, p.epoch)
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
	return dnsObservedOutbound(d.proxyAdapter, DNSRoutingEpoch())
}
