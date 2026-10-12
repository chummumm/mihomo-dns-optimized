package outbound

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
)

type directDestinationResolver struct {
	resolver.Resolver
	ips   []netip.Addr
	calls int
}

func (r *directDestinationResolver) Invalid() bool       { return true }
func (r *directDestinationResolver) PreferIPOrder() bool { return true }
func (r *directDestinationResolver) LookupIP(context.Context, string) ([]netip.Addr, error) {
	r.calls++
	return append([]netip.Addr(nil), r.ips...), nil
}
func (r *directDestinationResolver) LookupIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	ips, err := r.LookupIP(ctx, host)
	v4, _ := resolver.SortationAddr(ips)
	return v4, err
}
func (r *directDestinationResolver) LookupIPv6(ctx context.Context, host string) ([]netip.Addr, error) {
	ips, err := r.LookupIP(ctx, host)
	_, v6 := resolver.SortationAddr(ips)
	return v6, err
}

func installDirectDestinationResolver(t *testing.T) *directDestinationResolver {
	t.Helper()
	old, disabled := resolver.DirectHostResolver, resolver.DisableIPv6
	r := &directDestinationResolver{ips: []netip.Addr{
		netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11"),
		netip.MustParseAddr("2001:db8::10"), netip.MustParseAddr("2001:db8::11"),
	}}
	resolver.DirectHostResolver, resolver.DisableIPv6 = r, false
	t.Cleanup(func() { resolver.DirectHostResolver, resolver.DisableIPv6 = old, disabled })
	return r
}

func TestDirectDestinationTCPKeepsClientIPWithoutDNS(t *testing.T) {
	r := installDirectDestinationResolver(t)
	for _, test := range []struct {
		name         string
		ip, original netip.Addr
		want         string
	}{
		{"IP with domain", netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("192.0.2.7"), "192.0.2.7:443"},
		{"sniffer replaced destination", netip.Addr{}, netip.MustParseAddr("192.0.2.8"), "192.0.2.8:443"},
		{"hosts changed destination", netip.MustParseAddr("192.0.2.9"), netip.MustParseAddr("192.0.2.8"), "192.0.2.8:443"},
		{"IPv6 client", netip.MustParseAddr("2001:db8::7"), netip.MustParseAddr("2001:db8::7"), "[2001:db8::7]:443"},
	} {
		t.Run(test.name, func(t *testing.T) {
			md := &C.Metadata{Host: "sniffed.example", SniffHost: "sniffed.example", DstIP: test.ip, InboundDstIP: test.original, DstPort: 443, NetWork: C.TCP}
			before := *md
			client, peer := net.Pipe()
			defer peer.Close()
			conn, err := NewDirect().DialContextWithOptions(context.Background(), md, dialer.WithNetDialer(dialer.NetDialerFunc(func(_ context.Context, _, address string) (net.Conn, error) {
				if address != test.want {
					return nil, errors.New("client destination was replaced: " + address)
				}
				return client, nil
			})))
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			if r.calls != 0 || md.DstIP != before.DstIP || md.Host != before.Host {
				t.Fatalf("DIRECT re-resolved or mutated the caller: calls=%d metadata=%+v", r.calls, md)
			}
		})
	}
}

func TestDirectDestinationUDPKeepsClientIPWithSeparateResolver(t *testing.T) {
	r := installDirectDestinationResolver(t)
	md := &C.Metadata{Host: "sniffed.example", DstIP: netip.MustParseAddr("192.0.2.9"), InboundDstIP: netip.MustParseAddr("192.0.2.7"), DstPort: 443, NetWork: C.UDP}
	if err := NewDirect().ResolveUDP(context.Background(), md); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 || md.DstIP.String() != "192.0.2.7" {
		t.Fatalf("client UDP IP was replaced: %+v calls=%d", md, r.calls)
	}
}

func TestDirectDestinationDomainUDPUsesOrderedPreferredFamily(t *testing.T) {
	installDirectDestinationResolver(t)
	for _, test := range []struct {
		prefer C.DNSPrefer
		want   string
	}{
		{C.DualStack, "192.0.2.10"}, {C.IPv4Only, "192.0.2.10"},
		{C.IPv6Only, "2001:db8::10"}, {C.IPv6Prefer, "2001:db8::10"},
	} {
		// A GEOIP rule may already have resolved this domain with the default
		// resolver. That is not the client's chosen real IP and must not
		// replace the DIRECT resolver's preferred result.
		md := &C.Metadata{Host: "domain.example", DstIP: netip.MustParseAddr("203.0.113.77"), DstPort: 443, NetWork: C.UDP, DNSMode: C.DNSFakeIP}
		d := NewDirectWithOption(DirectOption{Name: "DIRECT", BasicOption: BasicOption{IPVersion: test.prefer}})
		if err := d.ResolveUDP(context.Background(), md); err != nil {
			t.Fatal(err)
		}
		if md.DstIP.String() != test.want {
			t.Fatalf("family %v discarded DNS order: %s", test.prefer, md.DstIP)
		}
	}
}

func TestDirectDestinationDNSContextCannotReplaceUpstreamIP(t *testing.T) {
	business := &C.Metadata{InboundDstIP: netip.MustParseAddr("192.0.2.7"), Host: "site.example"}
	ctx := icontext.WithDNSRoutingMetadata(context.Background(), business)
	upstream := icontext.DNSRoutingMetadata(ctx)
	upstream.Host, upstream.DstIP = "", netip.MustParseAddr("192.0.2.53")
	if got := directDestinationIP(ctx, upstream); got != upstream.DstIP {
		t.Fatalf("business IP replaced DNS upstream: %s", got)
	}
	if !business.InboundDstIP.IsValid() {
		t.Fatal("DNS setup mutated business metadata")
	}
	upstream.InboundDstIP = business.InboundDstIP
	if got := directDestinationIP(icontext.WithDNSBootstrap(ctx), upstream); got != upstream.DstIP {
		t.Fatalf("bootstrap used business IP: %s", got)
	}
}
