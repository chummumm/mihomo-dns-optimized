package tunnel

import (
	"context"
	"net/netip"
	"testing"

	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
)

type directDestinationMapper struct{ resolver.Enhancer }

func (directDestinationMapper) IsFakeIP(ip netip.Addr) bool {
	return ip == netip.MustParseAddr("198.18.0.1")
}

type directDestinationForbiddenResolver struct{ resolver.Resolver }

func (directDestinationForbiddenResolver) Invalid() bool { return true }
func (directDestinationForbiddenResolver) LookupIP(context.Context, string) ([]netip.Addr, error) {
	panic("a client-specified IP must not be resolved again while matching rules")
}
func (r directDestinationForbiddenResolver) LookupIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	return r.LookupIP(ctx, host)
}

func TestDirectDestinationIPRulesReuseClientIPWithoutChangingProxyTarget(t *testing.T) {
	dnsProxyTestState(t)
	old := resolver.DefaultResolver
	resolver.DefaultResolver = directDestinationForbiddenResolver{}
	t.Cleanup(func() { resolver.DefaultResolver = old })
	proxies["proxy"] = newDNSProxyTestProxy(newDNSProxyTestBase("proxy", C.Socks5, true))
	rules = []C.Rule{dnsProxyTestRule(t, "IP-CIDR,192.0.2.7/32,proxy"), dnsProxyTestRule(t, "MATCH,DIRECT")}
	md := &C.Metadata{Host: "sniffed.example", SniffHost: "sniffed.example", InboundDstIP: netip.MustParseAddr("192.0.2.7"), DstPort: 443, NetWork: C.TCP}
	p, _, err := resolveMetadata(md)
	if err != nil || p.Name() != "proxy" {
		t.Fatalf("IP rule did not use client's destination: %v %v", p, err)
	}
	if md.DstIP.IsValid() || md.Host != "sniffed.example" || md.SniffHost != "sniffed.example" {
		t.Fatalf("IP rule matching changed the proxy's hostname destination: %+v", md)
	}
	rules = []C.Rule{dnsProxyTestRule(t, "DOMAIN,sniffed.example,proxy"), dnsProxyTestRule(t, "MATCH,DIRECT")}
	p, _, err = resolveMetadata(md)
	if err != nil || p.Name() != "proxy" {
		t.Fatalf("sniffed domain stopped participating in rules: %v %v", p, err)
	}
}

func TestDirectDestinationCapturesRealInboundBeforeSniffing(t *testing.T) {
	old := resolver.DefaultHostMapper
	resolver.DefaultHostMapper = directDestinationMapper{}
	t.Cleanup(func() { resolver.DefaultHostMapper = old })
	for _, test := range []struct {
		name, host, ip string
		want           bool
	}{
		{"real IP", "", "192.0.2.7", true},
		{"literal host", "2001:db8::7", "", true},
		{"FakeIP", "", "198.18.0.1", false},
		{"domain only", "domain.example", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			md := &C.Metadata{Host: test.host}
			if test.ip != "" {
				md.DstIP = netip.MustParseAddr(test.ip)
			}
			fixMetadata(md)
			if md.InboundDstIP.IsValid() != test.want {
				t.Fatalf("incorrect original destination: %+v", md)
			}
			if test.want && md.InboundDstIP != md.DstIP {
				t.Fatal("captured IP differs from client target")
			}
		})
	}
}
