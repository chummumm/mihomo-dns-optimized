package dns

import (
	"net/netip"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
)

func TestDNSOptimizationResolverRunsProbesOnlyForAutomaticDirectPool(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("chosen")
	config := nativeRoutingTestConfig()
	config.SpeedCheck = SpeedCheckConfig{Mode: []string{"tcp:443"}, Timeout: time.Second, Concurrency: 2}
	rs := NewResolver(config)
	defer rs.Close()
	r := rs.Resolver
	if r.speedChecker == nil || r.speedChecker != rs.DirectResolver.speedChecker {
		t.Fatal("resolver pools did not share their configured probe limit")
	}
	ctx := routingTestContext(leaf, routingTestOrigin())
	query := routingTestQuery("speed.example", 1)
	routingTestExchange(t, r, ctx, query)
	nativeRoutingTestDestination(t, leaf, "198.51.100.53")
	probe := routingTestNextCall(t, leaf).metadata
	if probe.DstIP != netip.MustParseAddr("192.0.2.99") || probe.DstPort != 443 || probe.Host != "" {
		t.Fatalf("speed probe did not use the returned literal IP: %+v", probe)
	}
	if leaf.tcp.Load() != 1 || leaf.udp.Load() != 1 {
		t.Fatal("DIRECT resolver did not perform exactly one DNS query and probe")
	}
	query.Id++
	routingTestExchange(t, r, ctx, query)
	if leaf.tcp.Load() != 1 || leaf.udp.Load() != 1 {
		t.Fatal("cache hit repeated the DNS query or IP probes")
	}
	leaf.kind.Store(int32(C.Socks5))
	query.Id++
	routingTestExchange(t, r, ctx, query)
	nativeRoutingTestDestination(t, leaf, "192.0.2.53")
	if leaf.tcp.Load() != 1 || leaf.udp.Load() != 2 {
		t.Fatal("proxy pool reused the direct cache or performed IP probes")
	}
}

func TestDNSOptimizationExplicitTransportDoesNotProbe(t *testing.T) {
	routingTestEnable(t)
	leaf, explicit := newRoutingTestOutbound("chosen"), newRoutingTestOutbound("fixed-DNS")
	config := nativeRoutingTestConfig()
	config.DirectServer[0].ProxyAdapter = explicit
	config.SpeedCheck = SpeedCheckConfig{Mode: []string{"tcp:443"}, Timeout: time.Second}
	rs := NewResolver(config)
	defer rs.Close()
	routingTestExchange(t, rs.Resolver, routingTestContext(leaf, routingTestOrigin()), routingTestQuery("explicit.example", 2))
	nativeRoutingTestDestination(t, explicit, "198.51.100.53")
	if leaf.tcp.Load() != 0 || explicit.tcp.Load() != 0 || leaf.udp.Load() != 0 {
		t.Fatal("explicit DNS transport triggered automatic DIRECT IP probes")
	}
}
