package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"github.com/metacubex/mihomo/component/dialer"
	C "github.com/metacubex/mihomo/constant"
)

// DialDirectProbe uses the already chosen DIRECT adapter, including its local
// interface, routing mark and socket options. The numeric destination prevents
// a recursive lookup, and bypassing the tunnel avoids a second rule decision.
func (p *DNSRoutingPlan) DialDirectProbe(ctx context.Context, address netip.Addr, port uint16) (net.Conn, error) {
	leaf, err := p.directProbeAdapter()
	if err != nil {
		return nil, err
	}
	if !address.IsValid() || port == 0 {
		return nil, errors.New("DNS IP speed checks require a numeric IP and TCP port")
	}
	metadata := p.origin.Clone()
	metadata.Host, metadata.SniffHost = "", ""
	metadata.DstIP, metadata.DstPort = address.Unmap(), port
	metadata.NetWork = C.TCP
	metadata.DNSMode = C.DNSNormal
	metadata.RawDstAddr = nil
	metadata.RemoteDst = ""
	if direct, ok := leaf.(interface {
		DialContextWithOptions(context.Context, *C.Metadata, ...dialer.Option) (C.Conn, error)
	}); ok {
		// TFO returns a lazy connection before SYN/write; that is not a
		// measured TCP handshake. Disable it only for this latency probe.
		return direct.DialContextWithOptions(ctx, metadata, dialer.WithTFO(false))
	}
	if leaf.ProxyInfo().TFO {
		return nil, errors.New("DIRECT adapter cannot perform an eager speed probe")
	}
	return leaf.DialContext(ctx, metadata)
}

// DirectProbeOptions supplies the same explicit interface and mark for ICMP.
// Unset values continue to use Mihomo's global interface/mark selection.
func (p *DNSRoutingPlan) DirectProbeOptions() ([]dialer.Option, error) {
	leaf, err := p.directProbeAdapter()
	if err != nil {
		return nil, err
	}
	if direct, ok := leaf.(interface{ DialOptions() []dialer.Option }); ok {
		return direct.DialOptions(), nil
	}
	info := leaf.ProxyInfo()
	var options []dialer.Option
	if info.Interface != "" {
		options = append(options, dialer.WithInterface(info.Interface))
	}
	if info.RoutingMark != 0 {
		options = append(options, dialer.WithRoutingMark(info.RoutingMark))
	}
	return options, nil
}

// A frozen leaf normally still has adapter.Proxy bookkeeping wrappers. Remove
// only those wrappers using Adapter, never Unwrap (which can select a group).
func (p *DNSRoutingPlan) directProbeAdapter() (C.ProxyAdapter, error) {
	if p == nil || (p.Type() != C.Direct && p.Type() != C.Compatible) {
		return nil, errors.New("DNS IP speed checks require a DIRECT outbound")
	}
	leaf := p.route.proxy
	for depth := 0; depth < 16 && leaf != nil; depth++ {
		wrapper, ok := leaf.(C.Proxy)
		if !ok {
			return leaf, nil
		}
		leaf = wrapper.Adapter()
	}
	return nil, errors.New("invalid DIRECT adapter wrapper chain")
}
