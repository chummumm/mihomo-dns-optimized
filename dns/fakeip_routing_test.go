package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/mihomo/component/fakeip"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/component/trie"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	RC "github.com/metacubex/mihomo/rules/common"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

func fakeIPRoutingTestEnhancer(t *testing.T) EnhancerConfig {
	t.Helper()
	pools := make([]*fakeip.Pool, 2)
	for i, prefix := range []string{"198.18.0.1/16", "fdfe:dcba:9876::1/64"} {
		pool, err := fakeip.New(fakeip.Options{IPNet: netip.MustParsePrefix(prefix), Size: 16})
		if err != nil {
			t.Fatal(err)
		}
		pools[i] = pool
	}
	return EnhancerConfig{IPv6: true, EnhancedMode: C.DNSFakeIP, FakeIPPool: pools[0], FakeIPPool6: pools[1], FakeIPSkipper: &fakeip.Skipper{}, FakeIPTTL: 17}
}

func fakeIPRoutingTestReply(query *D.Msg) *D.Msg {
	answer := new(D.Msg).SetReply(query)
	header := D.RR_Header{Name: query.Question[0].Name, Rrtype: query.Question[0].Qtype, Class: D.ClassINET, Ttl: 60}
	var record D.RR
	switch header.Rrtype {
	case D.TypeA:
		record = &D.A{Hdr: header, A: net.IPv4(192, 0, 2, 99)}
	case D.TypeAAAA:
		record = &D.AAAA{Hdr: header, AAAA: net.ParseIP("2001:db8::99")}
	case D.TypeCNAME:
		record = &D.CNAME{Hdr: header, Target: "alias-target.example."}
	case D.TypeTXT:
		record = &D.TXT{Hdr: header, Txt: []string{"real-upstream"}}
	case D.TypeMX:
		record = &D.MX{Hdr: header, Preference: 10, Mx: "mail.example."}
	case D.TypeHTTPS:
		record = &D.HTTPS{SVCB: D.SVCB{Hdr: header, Priority: 1, Target: "."}}
	case D.TypeSVCB:
		record = &D.SVCB{Hdr: header, Priority: 1, Target: "."}
	}
	if record != nil {
		answer.Answer = []D.RR{record}
	}
	return answer
}

func fakeIPRoutingTestServe(t *testing.T, service *Service, ctx context.Context, name string, qtype uint16) *D.Msg {
	t.Helper()
	query := new(D.Msg).SetQuestion(D.Fqdn(name), qtype)
	answer, err := service.ServeMsg(ctx, query)
	if err != nil || answer == nil || answer.Id != query.Id || answer.Rcode != D.RcodeSuccess {
		t.Fatalf("serve %s %s: answer=%v, err=%v", name, D.TypeToString[qtype], answer, err)
	}
	return answer
}

func TestDNSRuleRoutingFakeIPLocalAnswersPrecedeResolver(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("direct")
	leaf.answer = fakeIPRoutingTestReply
	cfg := nativeRoutingTestConfig()
	cfg.IPv6 = true
	cfg.SpeedCheck = SpeedCheckConfig{Mode: []string{"tcp:443"}, Timeout: time.Second, ResponseMode: ResponseModeFastestIP}
	cfg.AnswerPolicy = AnswerPolicy{ForceNoCNAME: true, RRTTLMin: 300, RRTTLMax: 600}
	cfg.DualStack = DualStackConfig{Enabled: true}
	cfg.CacheOptions = &CacheOptions{Prefetch: true}
	rs := NewResolver(cfg)
	t.Cleanup(rs.Close)
	r := rs.Resolver
	manager := dnsstats.New()
	ctx := dnsstats.WithManager(icontext.WithDNSRoutingInbound(routingTestContext(leaf, routingTestOrigin())), manager)
	query := new(D.Msg).SetQuestion("cached.example.", D.TypeA)
	warm := routingTestExchange(t, r, ctx, query)
	if ips := msgToIP(warm); len(ips) != 1 || ips[0].String() != "192.0.2.99" || warm.Answer[0].Header().Ttl != 300 {
		t.Fatalf("real resolver did not warm its adjusted answer: %v", warm)
	}
	queries, probes := leaf.queryCount.Load(), leaf.tcp.Load()
	if queries != 1 || probes != 1 {
		t.Fatalf("fixture did not exercise real DNS and DIRECT probing: queries=%d probes=%d", queries, probes)
	}
	r.cacheControl.mu.Lock()
	initialHeat, initialCandidates := 0, len(r.cacheControl.hot)
	var initialDemand uint64
	for _, hot := range r.cacheControl.hot {
		initialHeat += int(hot.heat)
		initialDemand += hot.demand
	}
	r.cacheControl.mu.Unlock()
	if initialCandidates != 1 || initialHeat != cachePrefetchInitialHeat || initialDemand != 1 {
		t.Fatalf("successful real DIRECT answer did not activate one finite prefetch candidate: count=%d heat=%d demand=%d", initialCandidates, initialHeat, initialDemand)
	}
	enhancer := fakeIPRoutingTestEnhancer(t)
	mapper := NewEnhancer(enhancer)
	service := NewService(r, mapper)
	for i := 0; i < 3; i++ {
		answer := fakeIPRoutingTestServe(t, service, ctx, "cached.example", D.TypeA)
		ips := msgToIP(answer)
		if len(ips) != 1 || !enhancer.FakeIPPool.IPNet().Contains(ips[0]) || answer.Answer[0].Header().Ttl != 17 {
			t.Fatalf("real cache or TTL policy overrode FakeIP: %v", answer)
		}
		if host, ok := mapper.FindHostByIP(ips[0]); !ok || host != "cached.example" {
			t.Fatal("FakeIP answer lost the domain needed by a later business connection")
		}
	}
	answer6 := fakeIPRoutingTestServe(t, service, ctx, "v6.example", D.TypeAAAA)
	if ips := msgToIP(answer6); len(ips) != 1 || !enhancer.FakeIPPool6.IPNet().Contains(ips[0]) || answer6.Answer[0].Header().Ttl != 17 {
		t.Fatalf("real IPv6 selection or TTL policy overrode FakeIPv6: %v", answer6)
	}
	for _, qtype := range []uint16{D.TypeHTTPS, D.TypeSVCB} {
		if answer := fakeIPRoutingTestServe(t, service, ctx, "service.example", qtype); len(answer.Answer) != 0 {
			t.Fatalf("FakeIP must retain its local empty %s response: %v", D.TypeToString[qtype], answer)
		}
	}
	if leaf.queryCount.Load() != queries || leaf.tcp.Load() != probes {
		t.Fatal("FakeIP/local answers queried or probed an upstream")
	}
	r.cacheControl.mu.Lock()
	finalHeat, finalCandidates := 0, len(r.cacheControl.hot)
	var finalDemand uint64
	for _, hot := range r.cacheControl.hot {
		finalHeat += int(hot.heat)
		finalDemand += hot.demand
	}
	r.cacheControl.mu.Unlock()
	if finalCandidates != initialCandidates || finalHeat != initialHeat || finalDemand != initialDemand {
		t.Fatal("FakeIP answers contributed to real-answer prefetch activity")
	}
	if totals := manager.Stats().Totals; totals.Queries != 6 || totals.FakeIP != 4 || totals.Local != 2 || totals.CacheFresh != 0 || totals.Upstream != 0 {
		t.Fatalf("local FakeIP queries were misclassified: %+v", totals)
	}
	// Internal lookups still use real answers even while the client service
	// synthesizes FakeIP; its separate answer cache must remain intact.
	if got := routingTestExchange(t, r, ctx, query); msgToIP(got)[0].String() != "192.0.2.99" || leaf.queryCount.Load() != queries {
		t.Fatal("FakeIP modified the real resolver cache or internal lookup path")
	}
}

func TestDNSRuleRoutingFakeIPPreservesHostsAndIPv6Policy(t *testing.T) {
	routingTestEnable(t)
	oldHosts := resolver.DefaultHosts
	t.Cleanup(func() { resolver.DefaultHosts = oldHosts })
	hosts := trie.New[resolver.HostValue]()
	if err := hosts.Insert("hosts.example", resolver.HostValue{IPs: []netip.Addr{netip.MustParseAddr("192.0.2.7")}}); err != nil {
		t.Fatal(err)
	}
	if err := hosts.Insert("alias.example", resolver.HostValue{IsDomain: true, Domain: "target.example"}); err != nil {
		t.Fatal(err)
	}
	resolver.DefaultHosts = resolver.NewHosts(hosts)
	leaf := newRoutingTestOutbound("reject")
	leaf.kind.Store(int32(C.Reject))
	rs := NewResolver(nativeRoutingTestConfig())
	t.Cleanup(rs.Close)
	ctx := routingTestContext(leaf, routingTestOrigin())
	enhancer := fakeIPRoutingTestEnhancer(t)
	enhancer.UseHosts = true
	mapper := NewEnhancer(enhancer)
	service := NewService(rs.Resolver, mapper)
	answer := fakeIPRoutingTestServe(t, service, ctx, "hosts.example", D.TypeA)
	if ips := msgToIP(answer); len(ips) != 1 || ips[0].String() != "192.0.2.7" || answer.Answer[0].Header().Ttl != 10 {
		t.Fatalf("FakeIP or DNS REJECT displaced a hosts answer: %v", answer)
	}
	alias := fakeIPRoutingTestServe(t, service, ctx, "alias.example", D.TypeA)
	if len(alias.Answer) != 2 || alias.Answer[0].Header().Rrtype != D.TypeCNAME || alias.Question[0].Name != "alias.example." {
		t.Fatalf("hosts alias lost its client question or CNAME: %v", alias)
	}
	if ips := msgToIP(alias); len(ips) != 1 || !enhancer.FakeIPPool.IPNet().Contains(ips[0]) {
		t.Fatalf("hosts alias did not follow the existing FakeIP path: %v", alias)
	} else if host, ok := mapper.FindHostByIP(ips[0]); !ok || host != "target.example" {
		t.Fatal("hosts alias FakeIP lost its target-domain mapping")
	}
	for _, disabled := range []string{"ipv6", "pool"} {
		t.Run(disabled, func(t *testing.T) {
			limited := fakeIPRoutingTestEnhancer(t)
			if disabled == "ipv6" {
				limited.IPv6 = false
			} else {
				limited.FakeIPPool6 = nil
			}
			s := NewService(rs.Resolver, NewEnhancer(limited))
			if answer := fakeIPRoutingTestServe(t, s, ctx, "disabled-v6.example", D.TypeAAAA); len(answer.Answer) != 0 {
				t.Fatalf("missing FakeIPv6 support escaped into real resolution: %v", answer)
			}
		})
	}
	if leaf.queryCount.Load() != 0 || leaf.udp.Load() != 0 || leaf.tcp.Load() != 0 {
		t.Fatal("hosts or local empty AAAA unexpectedly reached upstream routing")
	}
}

func TestDNSRuleRoutingFakeIPFilterRoutesRealQueries(t *testing.T) {
	routingTestEnable(t)
	oldProxies, oldProviders := tunnel.Proxies(), tunnel.Providers()
	oldRules, oldRuleProviders := tunnel.Rules(), tunnel.RuleProviders()
	t.Cleanup(func() {
		tunnel.UpdateProxies(oldProxies, oldProviders)
		tunnel.UpdateRules(oldRules, nil, oldRuleProviders)
	})
	direct, proxy := newRoutingTestOutbound("direct"), newRoutingTestOutbound("overseas")
	direct.answer, proxy.answer = fakeIPRoutingTestReply, fakeIPRoutingTestReply
	proxy.kind.Store(int32(C.Socks5))
	tunnel.UpdateProxies(map[string]C.Proxy{"DIRECT": routingTestProxy(direct), "overseas": routingTestProxy(proxy)}, nil)
	tunnel.UpdateRules([]C.Rule{RC.NewDomain("direct.example", "DIRECT"), RC.NewMatch("overseas")}, nil, nil)
	ctx := icontext.WithDNSRoutingInbound(icontext.WithDNSRoutingMetadata(context.Background(), routingTestOrigin()))
	for _, mode := range []C.FilterMode{C.FilterBlackList, C.FilterWhiteList, C.FilterRule} {
		t.Run(mode.String(), func(t *testing.T) {
			enhancer := fakeIPRoutingTestEnhancer(t)
			skipper := &fakeip.Skipper{Mode: mode}
			switch mode {
			case C.FilterBlackList:
				skipper.Host = []C.DomainMatcher{redirHostTestFilter(t, "direct.example", "proxy.example")}
			case C.FilterWhiteList:
				skipper.Host = []C.DomainMatcher{redirHostTestFilter(t, "fake.example")}
			case C.FilterRule:
				skipper.Rules = []C.Rule{RC.NewDomain("direct.example", fakeip.UseRealIP), RC.NewDomain("proxy.example", fakeip.UseRealIP), RC.NewMatch(fakeip.UseFakeIP)}
			}
			enhancer.FakeIPSkipper = skipper
			cfg := nativeRoutingTestConfig()
			cfg.IPv6 = true
			rs := NewResolver(cfg)
			t.Cleanup(rs.Close)
			service := NewService(rs.Resolver, NewEnhancer(enhancer))
			for _, test := range []struct {
				name        string
				typeCode    uint16
				leaf        *routingTestOutbound
				destination string
			}{
				{"direct.example", D.TypeA, direct, "198.51.100.53"},
				{"direct.example", D.TypeAAAA, direct, "198.51.100.53"},
				{"direct.example", D.TypeHTTPS, direct, "198.51.100.53"},
				{"direct.example", D.TypeSVCB, direct, "198.51.100.53"},
				{"proxy.example", D.TypeA, proxy, "192.0.2.53"},
				{"fake.example", D.TypeCNAME, proxy, "192.0.2.53"},
				{"fake.example", D.TypeTXT, proxy, "192.0.2.53"},
				{"fake.example", D.TypeMX, proxy, "192.0.2.53"},
			} {
				before := test.leaf.queryCount.Load()
				answer := fakeIPRoutingTestServe(t, service, ctx, test.name, test.typeCode)
				if len(answer.Answer) != 1 || answer.Answer[0].Header().Rrtype != test.typeCode || test.leaf.queryCount.Load() != before+1 {
					t.Fatalf("filtered/non-address query did not reach its real resolver: %v", answer)
				}
				// Connections may be shared across questions, so inspect the
				// frozen route's pool separately from new physical dial events.
				prepared, _, err := rs.Resolver.prepareDNSRouting(ctx, new(D.Msg).SetQuestion(D.Fqdn(test.name), test.typeCode))
				if err != nil {
					t.Fatal(err)
				}
				main, _ := rs.Resolver.dnsQueryServers(prepared)
				if len(main) != 1 || main[0].Address() != "udp://"+test.destination+":53" {
					t.Fatalf("wrong real resolver pool: %v, want %s", main, test.destination)
				}
			}
			fake := fakeIPRoutingTestServe(t, service, ctx, "fake.example", D.TypeA)
			if ips := msgToIP(fake); len(ips) != 1 || !enhancer.FakeIPPool.IPNet().Contains(ips[0]) {
				t.Fatalf("filter failed to preserve the FakeIP branch: %v", fake)
			}
		})
	}
	// Both selected pools were reached through the actual DNS wire bridge.
	nativeRoutingTestDestination(t, direct, "198.51.100.53")
	nativeRoutingTestDestination(t, proxy, "192.0.2.53")
}

func TestDNSRuleRoutingFakeIPPrecedesRejectButRealQueriesRemainBlocked(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("blocked")
	rs := NewResolver(nativeRoutingTestConfig())
	t.Cleanup(rs.Close)
	enhancer := fakeIPRoutingTestEnhancer(t)
	enhancer.FakeIPSkipper.Host = []C.DomainMatcher{redirHostTestFilter(t, "real.example")}
	service := NewService(rs.Resolver, NewEnhancer(enhancer))
	ctx := routingTestContext(leaf, routingTestOrigin())
	for _, kind := range []C.AdapterType{C.Reject, C.RejectDrop} {
		leaf.kind.Store(int32(kind))
		fakeIPRoutingTestServe(t, service, ctx, "fake.example", D.TypeA)
		fakeIPRoutingTestServe(t, service, ctx, "fake.example", D.TypeAAAA)
		for _, test := range []struct {
			name     string
			typeCode uint16
		}{{"real.example", D.TypeA}, {"real.example", D.TypeHTTPS}, {"fake.example", D.TypeTXT}} {
			answer, err := service.ServeMsg(ctx, new(D.Msg).SetQuestion(D.Fqdn(test.name), test.typeCode))
			if kind == C.RejectDrop {
				if !errors.Is(err, resolver.ErrDNSDrop) {
					t.Fatalf("real query lost REJECT-DROP: %v, %v", answer, err)
				}
			} else if err != nil || answer == nil || answer.Rcode != D.RcodeRefused {
				t.Fatalf("real query lost REJECT: %v, %v", answer, err)
			}
		}
	}
	if leaf.udp.Load() != 0 || leaf.tcp.Load() != 0 {
		t.Fatal("FakeIP or rejected real queries contacted an upstream")
	}
}
