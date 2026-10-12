package outbound

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/loopback"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
)

type Direct struct {
	*Base
	loopBack *loopback.Detector
}

type DirectOption struct {
	BasicOption
	Name string `proxy:"name"`
}

// DialContext implements C.ProxyAdapter
func (d *Direct) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	return d.DialContextWithOptions(ctx, metadata)
}

// DialContextWithOptions preserves this DIRECT adapter's socket policy while
// allowing a caller such as an IP latency probe to require an eager connection.
func (d *Direct) DialContextWithOptions(ctx context.Context, metadata *C.Metadata, extra ...dialer.Option) (C.Conn, error) {
	ctx = icontext.WithDNSFixedOutbound(ctx, d)
	address := metadata.RemoteAddress()
	checkMetadata := metadata
	if ip := directDestinationIP(ctx, metadata); ip.IsValid() {
		address = net.JoinHostPort(ip.String(), strconv.Itoa(int(metadata.DstPort)))
		checkMetadata = metadata.Clone()
		checkMetadata.DstIP = ip
	}
	if err := d.loopBack.CheckConn(checkMetadata); err != nil {
		return nil, err
	}
	opts := d.DialOptions()
	opts = append(opts, dialer.WithResolver(resolver.DirectHostResolver),
		dialer.WithPreferResolverOrder(!icontext.DNSBootstrap(ctx)))
	opts = append(opts, extra...)
	c, err := dialer.DialContext(ctx, "tcp", address, opts...)
	if err != nil {
		return nil, err
	}
	return d.loopBack.NewConn(NewConn(c, d)), nil
}

// ListenPacketContext implements C.ProxyAdapter
func (d *Direct) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	if err := d.loopBack.CheckPacketConn(metadata); err != nil {
		return nil, err
	}
	if err := d.ResolveUDP(ctx, metadata); err != nil {
		return nil, err
	}
	pc, err := dialer.NewDialer(d.DialOptions()...).ListenPacket(ctx, "udp", "", metadata.AddrPort())
	if err != nil {
		return nil, err
	}
	return d.loopBack.NewPacketConn(NewPacketConn(pc, d)), nil
}

func (d *Direct) ResolveUDP(ctx context.Context, metadata *C.Metadata) error {
	ctx = icontext.WithDNSFixedOutbound(ctx, d)
	if ip := directDestinationIP(ctx, metadata); ip.IsValid() {
		metadata.DstIP = ip
		return nil
	}
	if metadata.Host != "" {
		ip, err := d.resolveIP(ctx, metadata.Host)
		if err != nil {
			return fmt.Errorf("can't resolve ip: %w", err)
		}
		metadata.DstIP = ip
	}
	return nil
}

func directDestinationIP(ctx context.Context, metadata *C.Metadata) netip.Addr {
	if !icontext.DNSBootstrap(ctx) && metadata.InboundDstIP.IsValid() {
		return metadata.InboundDstIP.Unmap()
	}
	// An IP filled in while matching GEOIP rules for a domain-only request is
	// not the client's fixed destination. Resolve that domain through DIRECT's
	// own resolver; literal/internal destinations already have no hostname.
	if metadata.Host == "" && metadata.DstIP.IsValid() && !resolver.IsFakeIP(metadata.DstIP) {
		return metadata.DstIP.Unmap()
	}
	return netip.Addr{}
}

func (d *Direct) resolveIP(ctx context.Context, host string) (netip.Addr, error) {
	r := resolver.DirectHostResolver
	if icontext.DNSBootstrap(ctx) || !resolver.PreferIPOrder(r) {
		return resolveIPWithResolver(ctx, host, d.prefer, r)
	}
	var ips []netip.Addr
	var err error
	switch d.prefer {
	case C.IPv4Only:
		ips, err = resolver.LookupIPv4WithResolver(ctx, host, r)
	case C.IPv6Only:
		ips, err = resolver.LookupIPv6WithResolver(ctx, host, r)
	default:
		ips, err = resolver.LookupIPWithResolver(ctx, host, r)
	}
	if err != nil {
		return netip.Addr{}, err
	}
	if len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("%w: %s", resolver.ErrIPNotFound, host)
	}
	// Honor the configured family preference, then take its highest-ranked IP.
	// Unlike TCP, a UDP socket has no handshake with which to race candidates.
	wantIPv6 := d.prefer == C.IPv6Prefer || d.prefer == C.IPv6Only
	for _, ip := range ips {
		if ip.Is6() == wantIPv6 {
			return ip, nil
		}
	}
	return ips[0], nil
}

func (d *Direct) IsL3Protocol(metadata *C.Metadata) bool {
	return true // tell DNSDialer don't send domain to DialContext, avoid lookback to DefaultResolver
}

func NewDirectWithOption(option DirectOption) *Direct {
	return &Direct{
		Base: NewBase(BaseOption{
			Name:         option.Name,
			Type:         C.Direct,
			ProviderName: option.ProviderName,
			UDP:          true,
			TFO:          option.TFO,
			MPTCP:        option.MPTCP,
			Interface:    option.Interface,
			RoutingMark:  option.RoutingMark,
			Prefer:       option.IPVersion,
		}),
		loopBack: loopback.NewDetector(),
	}
}

func NewDirect() *Direct {
	return &Direct{
		Base: NewBase(BaseOption{
			Name:   "DIRECT",
			Type:   C.Direct,
			UDP:    true,
			Prefer: C.DualStack,
		}),
		loopBack: loopback.NewDetector(),
	}
}

func NewCompatible() *Direct {
	return &Direct{
		Base: NewBase(BaseOption{
			Name:   "COMPATIBLE",
			Type:   C.Compatible,
			UDP:    true,
			Prefer: C.DualStack,
		}),
		loopBack: loopback.NewDetector(),
	}
}
