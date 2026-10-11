package tunnel

import (
	"fmt"

	"github.com/metacubex/mihomo/component/dialer"
	C "github.com/metacubex/mihomo/constant"
)

// SharedTransport snapshots the legacy explicit/direct path too. An unresolved
// group or dialer-proxy can select a different physical exit on each dial, so
// it must keep its exchange-owned connection until the complete chain can be
// frozen. Automatic rule routes already carry such a selection snapshot.
func (d *DNSDialer) SharedTransport() (*DNSDialer, string, bool) {
	if d.plan != nil {
		leaf := d.plan.route.proxy
		if p, ok := leaf.(C.Proxy); ok {
			leaf = p.Adapter()
		}
		if !d.plan.route.udpSupportFrozen || !sharedDNSLeaf(leaf) {
			return nil, "", false
		}
		return d, d.plan.NativeTransportKey(), true
	}
	if d.proxyName == DnsRespectRules {
		return nil, "", false
	}
	copy := *d
	copy.sharedSnapshot = true
	copy.sharedInterface = dialer.DefaultInterface.Load()
	copy.sharedMark = int(dialer.DefaultRoutingMark.Load())
	if copy.proxyAdapter == nil && copy.proxyName != "" {
		if proxy := Proxies()[copy.proxyName]; proxy != nil {
			copy.proxyAdapter = proxy
		} else {
			copy.sharedInterface = copy.proxyName
			copy.proxyName = ""
		}
	}
	leaf := copy.proxyAdapter
	if p, ok := leaf.(C.Proxy); ok {
		leaf = p.Adapter()
	}
	if leaf == nil {
		return &copy, fmt.Sprintf("direct|epoch:%d|interface:%q|mark:%d", DNSRoutingEpoch(), copy.sharedInterface, copy.sharedMark), true
	}
	if !sharedDNSLeaf(leaf) {
		return nil, "", false
	}
	info := leaf.ProxyInfo()
	iface, mark := info.Interface, info.RoutingMark
	if iface == "" {
		iface = copy.sharedInterface
	}
	if mark == 0 {
		mark = copy.sharedMark
	}
	return &copy, fmt.Sprintf("explicit:%T:%p|%d|%#v|udp:%t|epoch:%d|interface:%q|mark:%d",
		leaf, leaf, leaf.Type(), info, leaf.SupportUDP(), DNSRoutingEpoch(), iface, mark), true
}

func sharedDNSLeaf(leaf C.ProxyAdapter) bool {
	if leaf.ProxyInfo().DialerProxy != "" {
		return false
	}
	switch leaf.Type() {
	case C.Relay, C.Selector, C.URLTest, C.Fallback, C.LoadBalance:
		return false
	default:
		return true
	}
}
