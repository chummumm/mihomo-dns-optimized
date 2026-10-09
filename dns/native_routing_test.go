package dns

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	RC "github.com/metacubex/mihomo/rules/common"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

func nativeRoutingTestConfig() Config {
	return Config{
		RuleRouting:  true,
		Main:         []NameServer{{Addr: "192.0.2.53:53"}},
		DirectServer: []NameServer{{Addr: "198.51.100.53:53"}},
	}
}

func nativeRoutingTestDestination(t *testing.T, outbound *routingTestOutbound, want string) {
	t.Helper()
	call := routingTestNextCall(t, outbound)
	if call.metadata.DstIP != netip.MustParseAddr(want) || call.metadata.DstPort != 53 || call.metadata.Host != "" {
		t.Fatalf("wrong actual DNS resolver destination: %+v, want %s:53", call.metadata, want)
	}
}

func TestDNSRuleRoutingNativeFirstQuestionUsesActualLeafPool(t *testing.T) {
	routingTestEnable(t)
	oldProxies, oldProviders := tunnel.Proxies(), tunnel.Providers()
	oldRules, oldRuleProviders := tunnel.Rules(), tunnel.RuleProviders()
	t.Cleanup(func() {
		tunnel.UpdateProxies(oldProxies, oldProviders)
		tunnel.UpdateRules(oldRules, nil, oldRuleProviders)
	})
	direct, compatible, proxy := newRoutingTestOutbound("domestic"), newRoutingTestOutbound("compatible"), newRoutingTestOutbound("DIRECT")
	compatible.kind.Store(int32(C.Compatible))
	proxy.kind.Store(int32(C.Socks5)) // A display name is not the outbound's type.
	tunnel.UpdateProxies(map[string]C.Proxy{
		"DIRECT": routingTestProxy(direct), "domestic": routingTestProxy(direct),
		"compatible": routingTestProxy(compatible), "overseas": routingTestProxy(proxy),
	}, nil)
	tunnel.UpdateRules([]C.Rule{
		RC.NewDomain("domestic.example", "domestic"),
		RC.NewDomain("compatible.example", "compatible"),
		RC.NewMatch("overseas"),
	}, nil, nil)
	r := NewResolver(nativeRoutingTestConfig()).Resolver
	ctx := icontext.WithDNSRoutingInbound(icontext.WithDNSRoutingMetadata(context.Background(), routingTestOrigin()))
	for _, test := range []struct {
		name  string
		qtype uint16
		leaf  *routingTestOutbound
		want  string
	}{
		{"domestic.example", D.TypeTXT, direct, "198.51.100.53"},
		{"compatible.example", D.TypeAAAA, compatible, "198.51.100.53"},
		{"claude.ai", D.TypeA, proxy, "192.0.2.53"},
	} {
		query := new(D.Msg).SetQuestion(D.Fqdn(test.name), test.qtype)
		routingTestExchange(t, r, ctx, query)
		nativeRoutingTestDestination(t, test.leaf, test.want)
		query.Id++
		routingTestExchange(t, r, ctx, query)
		if test.leaf.udp.Load() != 1 {
			t.Fatal("same selected pool did not reuse its scoped answer")
		}
	}
}

func TestDNSRuleRoutingNativePoolScopeAndFallbackIsolation(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("live-leaf")
	config := nativeRoutingTestConfig()
	config.Fallback = []NameServer{{Addr: "192.0.2.54:53"}}
	config.FallbackIPFilter = []C.IpMatcher{routingTestIPFilter{}}
	config.FallbackLazyQuery = true
	r := NewResolver(config).Resolver
	ctx, query := routingTestContext(leaf, routingTestOrigin()), routingTestQuery("same.example", 2)
	routingTestExchange(t, r, ctx, query)
	nativeRoutingTestDestination(t, leaf, "198.51.100.53")
	if leaf.udp.Load() != 1 {
		t.Fatal("direct pool queried main/fallback despite its isolated selection")
	}
	// Keep the same adapter/cache identity. Pool identity still prevents a
	// direct answer from being reused after the actual route becomes proxied.
	leaf.kind.Store(int32(C.Socks5))
	routingTestExchange(t, r, ctx, query)
	nativeRoutingTestDestination(t, leaf, "192.0.2.53")
	nativeRoutingTestDestination(t, leaf, "192.0.2.54")
	if leaf.udp.Load() != 3 {
		t.Fatal("proxy pool reused the direct cache or lost its normal fallback")
	}
}

func TestDNSRuleRoutingNativeDirectTruncationKeepsPlanAndPool(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("direct-leaf")
	leaf.truncate = true
	group := &routingTestGroup{routingTestBase: &routingTestBase{name: "selection", kind: C.Selector}, leaf: routingTestProxy(leaf)}
	r := NewResolver(nativeRoutingTestConfig()).Resolver
	routingTestExchange(t, r, routingTestContext(group, routingTestOrigin()), routingTestQuery("tcp.example", 3))
	nativeRoutingTestDestination(t, leaf, "198.51.100.53")
	nativeRoutingTestDestination(t, leaf, "198.51.100.53")
	if group.choices.Load() != 1 || leaf.udp.Load() != 1 || leaf.tcp.Load() != 1 {
		t.Fatal("TCP retry reselected the group or changed the direct pool")
	}
}

func TestDNSRuleRoutingNativeUnselectedExplicitPoolCannotWeakenReject(t *testing.T) {
	routingTestEnable(t)
	leaf, explicit := newRoutingTestOutbound("reject"), newRoutingTestOutbound("unused-direct")
	config := nativeRoutingTestConfig()
	config.DirectServer[0].ProxyAdapter = explicit
	r := NewResolver(config).Resolver
	ctx := routingTestContext(leaf, routingTestOrigin())
	for _, qtype := range []uint16{D.TypeA, D.TypeAAAA, D.TypeTXT, D.TypeHTTPS} {
		query := new(D.Msg).SetQuestion("blocked.example.", qtype)
		leaf.kind.Store(int32(C.Reject))
		if answer := routingTestExchange(t, r, ctx, query); answer.Rcode != D.RcodeRefused {
			t.Fatalf("unused direct pool weakened REJECT: %v", answer)
		}
		leaf.kind.Store(int32(C.RejectDrop))
		if _, err := r.ExchangeContext(ctx, query); !errors.Is(err, resolver.ErrDNSDrop) {
			t.Fatalf("unused direct pool weakened REJECT-DROP: %v", err)
		}
	}
	if explicit.udp.Load() != 0 || leaf.udp.Load() != 0 {
		t.Fatal("rejection contacted a resolver")
	}
}

func TestDNSRuleRoutingNativeExplicitTransportBelongsToSelectedPool(t *testing.T) {
	routingTestEnable(t)
	for _, selectedExplicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "unused-main", true: "selected-direct"}[selectedExplicit], func(t *testing.T) {
			leaf, explicit := newRoutingTestOutbound("direct"), newRoutingTestOutbound("explicit")
			config := nativeRoutingTestConfig()
			if selectedExplicit {
				config.DirectServer[0].ProxyAdapter = explicit
			} else {
				config.Main[0].ProxyAdapter = explicit
				config.Fallback = []NameServer{{Addr: "192.0.2.54:53", ProxyAdapter: explicit}}
			}
			r := NewResolver(config).Resolver
			routingTestExchange(t, r, routingTestContext(leaf, routingTestOrigin()), routingTestQuery("domestic.example", 4))
			called, unused := leaf, explicit
			if selectedExplicit {
				called, unused = explicit, leaf
			}
			nativeRoutingTestDestination(t, called, "198.51.100.53")
			if called.udp.Load() != 1 || unused.udp.Load() != 0 {
				t.Fatal("explicit transport changed the chosen pool or lost its own outbound")
			}
		})
	}
}

type nativeRoutingTestFailure struct{ *routingTestOutbound }

func (a *nativeRoutingTestFailure) ListenPacketContext(ctx context.Context, md *C.Metadata) (C.PacketConn, error) {
	a.udp.Add(1)
	a.record(ctx, md)
	return nil, errors.New("direct DNS transport failed")
}

func TestDNSRuleRoutingNativeDirectFailureDoesNotBorrowOtherPools(t *testing.T) {
	routingTestEnable(t)
	leaf := &nativeRoutingTestFailure{newRoutingTestOutbound("failed-direct")}
	config := nativeRoutingTestConfig()
	config.Fallback = []NameServer{{Addr: "192.0.2.54:53"}}
	config.FallbackIPFilter = []C.IpMatcher{routingTestIPFilter{}}
	r := NewResolver(config).Resolver
	query := routingTestQuery("failure.example", 5)
	ctx, immediate, err := r.prepareDNSRouting(routingTestContext(leaf, routingTestOrigin()), query)
	if err != nil || immediate != nil {
		t.Fatalf("route preparation failed: %v, %v", immediate, err)
	}
	if _, err := r.ipExchange(ctx, query); err == nil {
		t.Fatal("direct DNS failure was silently replaced by another pool")
	}
	nativeRoutingTestDestination(t, leaf.routingTestOutbound, "198.51.100.53")
	if leaf.udp.Load() != 1 {
		t.Fatal("direct failure queried main/fallback")
	}
}

func TestDNSRuleRoutingNativePriorityAndLegacyDirectResolver(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("fixed-direct")
	rs := NewResolver(nativeRoutingTestConfig())
	md := routingTestOrigin()
	md.SpecialProxy = "fixed-inbound"
	routingTestExchange(t, rs.Resolver, routingTestContext(leaf, md), routingTestQuery("fixed.example", 6))
	nativeRoutingTestDestination(t, leaf, "192.0.2.53")
	// The separate resolver still has its original direct-server purpose. It
	// does not recursively classify itself into another primary resolver pool.
	if len(rs.DirectResolver.direct) != 0 {
		t.Fatal("DirectResolver recursively acquired a native direct pool")
	}
	routingTestExchange(t, rs.DirectResolver, routingTestContext(leaf, routingTestOrigin()), routingTestQuery("legacy-direct.example", 7))
	nativeRoutingTestDestination(t, leaf, "198.51.100.53")
	for _, mode := range []tunnel.TunnelMode{tunnel.Global, tunnel.Direct} {
		tunnel.SetMode(mode)
		ctx, _, err := rs.Resolver.prepareDNSRouting(routingTestContext(leaf, routingTestOrigin()), routingTestQuery("mode.example", 8))
		main, _ := rs.Resolver.dnsQueryServers(ctx)
		if err != nil || queryRoute(ctx) != nil || main[0] != rs.Resolver.main[0] {
			t.Fatalf("mode %s activated native DNS pool selection", mode)
		}
	}
}
