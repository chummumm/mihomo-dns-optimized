package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/process"
	R "github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
	icontext "github.com/metacubex/mihomo/context"
	RRules "github.com/metacubex/mihomo/rules"
	RP "github.com/metacubex/mihomo/rules/provider"
)

func TestDNSRoutingActualAttributesFollowRuleOrder(t *testing.T) {
	dnsProxyTestState(t)
	origin := dnsProxyTestResolver(C.UDP)
	origin.InName, origin.ProcessPath, origin.DSCP = "dns-client", "/usr/bin/smartdns", 8
	for _, condition := range []string{
		"IN-NAME,dns-client", "IN-TYPE,SOCKS5", "IN-USER,smartdns",
		"SRC-IP-CIDR,127.0.0.0/8", "SRC-PORT,53000", "DST-PORT,53", "IN-PORT,7894", "NETWORK,udp",
		"PROCESS-NAME,smartdns", "PROCESS-PATH,/usr/bin/smartdns", "PROCESS-NAME-REGEX,^smartdns$", "DSCP,8",
	} {
		t.Run(condition, func(t *testing.T) {
			attribute := dnsProxyTestRule(t, condition+",AWS")
			domain := dnsProxyTestRule(t, "DOMAIN,a.example,DMIT")
			for _, domainFirst := range []bool{false, true} {
				rules = []C.Rule{
					dnsProxyTestRule(t, "IP-CIDR,8.8.8.8/32,wrong"),
					dnsProxyTestRule(t, "NOT,((IP-CIDR,10.0.0.0/8)),wrong"), attribute, domain,
				}
				want := "AWS"
				if domainFirst {
					rules[2], rules[3], want = domain, attribute, "DMIT"
				}
				_, err := exchangeDNSProxy(context.Background(), dnsProxyTestQuery(t, "A.Example."), origin, selectDNSProxy,
					func(_ context.Context, query []byte, destination *C.Metadata, route dnsProxyRoute) ([]byte, error) {
						if route.proxy.Name() != want || destination.Host != "" || destination.DstIP != origin.DstIP {
							t.Fatalf("wrong rule order or resolver target: route=%s destination=%+v", route.proxy.Name(), destination)
						}
						return dnsProxyTestReply(t, query), nil
					})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDNSRoutingSourceRuleSetsAndUnknownDestination(t *testing.T) {
	dnsProxyTestState(t)
	ruleProviders["source-ip"] = RP.NewInlineProvider("source-ip", P.IPCIDR, []string{"127.0.0.0/8"}, RRules.ParseRule)
	ruleProviders["source-classical"] = RP.NewInlineProvider("source-classical", P.Classical, []string{"IP-CIDR,127.0.0.0/8"}, RRules.ParseRule)
	ruleProviders["destination-classical"] = RP.NewInlineProvider("destination-classical", P.Classical, []string{"SRC-IP-CIDR,8.8.8.8/32"}, RRules.ParseRule)
	metadata := dnsRoutingMetadata("a.example", dnsProxyTestResolver(C.UDP))
	before := metadata.Clone()
	for _, test := range []struct {
		rule         string
		known, match bool
	}{
		{"RULE-SET,source-ip,AWS", false, false},
		{"RULE-SET,source-ip,AWS,src", true, true},
		{"RULE-SET,source-classical,AWS,src", true, true},
		{"RULE-SET,destination-classical,AWS,src", false, false},
		{"NOT,((RULE-SET,destination-classical,src)),AWS", false, false},
		{"NOT,((SRC-IP-CIDR,10.0.0.0/8)),AWS", true, true},
	} {
		result := matchDNSProxyRule(dnsProxyTestRule(t, test.rule), metadata, 0)
		if result.known != test.known || result.match != test.match {
			t.Errorf("%s: got %+v, want known=%v match=%v", test.rule, result, test.known, test.match)
		}
	}
	if !reflect.DeepEqual(before, metadata) {
		t.Fatal("RULE-SET src mutated the caller metadata")
	}
}

func TestDNSRoutingSpecialRuleEntryAndUnsupportedActions(t *testing.T) {
	dnsProxyTestState(t)
	rules = []C.Rule{dnsProxyTestRule(t, "MATCH,wrong")}
	subRules["dns-entry"] = []C.Rule{dnsProxyTestRule(t, "DOMAIN,a.example,AWS"), dnsProxyTestRule(t, "MATCH,DMIT")}
	metadata := dnsRoutingMetadata("a.example", dnsProxyTestResolver(C.TCP))
	metadata.SpecialRules = "dns-entry"
	route, err := selectDNSProxy(metadata)
	if err != nil || route.proxy.Name() != "AWS" {
		t.Fatalf("inbound rule entry was lost: %+v, %v", route, err)
	}
	metadata.SpecialProxy = "Philippines"
	route, err = selectDNSProxy(metadata)
	if err != nil || route.proxy.Name() != "Philippines" {
		t.Fatalf("explicit inbound target lost priority: %+v, %v", route, err)
	}
	metadata.SpecialProxy = ""
	for _, kind := range []C.AdapterType{C.Dns, C.Rematch} {
		proxies["action"] = newDNSProxyTestProxy(newDNSProxyTestBase("action", kind, true))
		subRules["dns-entry"] = []C.Rule{dnsProxyTestRule(t, "MATCH,action")}
		if _, err := selectDNSProxy(metadata); err == nil {
			t.Errorf("DNS routing accepted recursive action %s", kind)
		}
	}
}

type dnsRoutingBootstrapResolver struct {
	R.Resolver
	lookup func(context.Context, string) ([]netip.Addr, error)
}

func (r *dnsRoutingBootstrapResolver) Invalid() bool { return true }
func (r *dnsRoutingBootstrapResolver) LookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	return r.lookup(ctx, host)
}
func (r *dnsRoutingBootstrapResolver) LookupIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	return r.lookup(ctx, host)
}
func (r *dnsRoutingBootstrapResolver) LookupIPv6(ctx context.Context, host string) ([]netip.Addr, error) {
	return r.lookup(ctx, host)
}

func TestDNSRoutingResolverHostnameUsesSeparateBootstrap(t *testing.T) {
	dnsProxyTestState(t)
	old := R.ProxyServerHostResolver
	t.Cleanup(func() { R.ProxyServerHostResolver = old })
	lookups := 0
	R.ProxyServerHostResolver = &dnsRoutingBootstrapResolver{lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
		lookups++
		if host != "resolver.example" || !icontext.DNSBootstrap(ctx) {
			t.Fatalf("wrong bootstrap context or question: host=%s bootstrap=%v", host, icontext.DNSBootstrap(ctx))
		}
		return []netip.Addr{netip.MustParseAddr("203.0.113.53")}, nil
	}}
	origin := dnsProxyTestResolver(C.TCP)
	origin.Host, origin.DstIP = "resolver.example", netip.Addr{}
	rules = []C.Rule{dnsProxyTestRule(t, "DOMAIN,a.example,AWS"), dnsProxyTestRule(t, "MATCH,wrong")}
	_, err := exchangeDNSProxy(context.Background(), dnsProxyTestQuery(t, "a.example."), origin, selectDNSProxy,
		func(_ context.Context, query []byte, destination *C.Metadata, route dnsProxyRoute) ([]byte, error) {
			if route.proxy.Name() != "AWS" || destination.Host != "" || destination.RemoteAddress() != "203.0.113.53:53" {
				t.Fatalf("website and resolver addresses were mixed: route=%s destination=%+v", route.proxy.Name(), destination)
			}
			return dnsProxyTestReply(t, query), nil
		})
	if err != nil || lookups != 1 || origin.DstIP.IsValid() || origin.Host != "resolver.example" {
		t.Fatalf("bootstrap failed or mutated original destination: calls=%d origin=%+v err=%v", lookups, origin, err)
	}
}

func TestDNSRoutingPlanFreezesRouteAndChecksActualTransport(t *testing.T) {
	dnsProxyTestState(t)
	sentinel := errors.New("test dial completed")
	dials := 0
	leaf := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("tcp-only", C.Http, false)}
	leaf.dial = func(context.Context, *C.Metadata) (C.Conn, error) { dials++; return nil, sentinel }
	proxies["tcp-only"] = newDNSProxyTestProxy(leaf)
	rules = []C.Rule{dnsProxyTestRule(t, "NETWORK,udp,tcp-only"), dnsProxyTestRule(t, "MATCH,wrong")}
	origin := dnsProxyTestResolver(C.UDP)
	plan, err := PrepareDNSRouting(context.Background(), "a.example.", origin)
	if err != nil || plan.Type() != C.Http {
		t.Fatalf("client UDP incorrectly ruled out a TCP upstream: %v", err)
	}
	rules = []C.Rule{dnsProxyTestRule(t, "MATCH,wrong")}
	query := dnsProxyTestQuery(t, "a.example.")
	if _, err := plan.Exchange(context.Background(), query, dnsProxyTestResolver(C.TCP)); !errors.Is(err, sentinel) || dials != 1 {
		t.Fatalf("plan did not preserve its frozen outbound: dials=%d, err=%v", dials, err)
	}
	if _, err := plan.Exchange(context.Background(), query, origin); err == nil || dials != 1 {
		t.Fatalf("unsupported UDP did not fail closed: dials=%d, err=%v", dials, err)
	}
	if _, err := plan.Exchange(context.Background(), dnsProxyTestQuery(t, "other.example."), origin); err == nil {
		t.Fatal("routing plan accepted another question")
	}
	if _, err := PrepareDNSRouting(icontext.WithDNSBootstrap(context.Background()), "a.example", origin); err == nil {
		t.Fatal("bootstrap reentered query routing")
	}
	fixed, err := PrepareDNSRouting(icontext.WithDNSFixedOutbound(context.Background(), leaf), "a.example", origin)
	if err != nil || fixed.Type() != C.Http || fixed.CacheKey() == "" {
		t.Fatalf("fixed outbound was rematched: %+v, %v", fixed, err)
	}
}

func TestDNSRoutingNestedResolutionAllowsConfigurationReload(t *testing.T) {
	dnsProxyTestState(t)
	rules = []C.Rule{
		dnsProxyTestRule(t, "IP-CIDR,203.0.113.10/32,AWS"),
		dnsProxyTestRule(t, "DOMAIN,a.example,DMIT"),
	}
	metadata := dnsRoutingMetadata("a.example", dnsProxyTestResolver(C.TCP))
	reloadedRules := []C.Rule{dnsProxyTestRule(t, "DOMAIN,a.example,DMIT")}
	reloadedProxies := make(map[string]C.Proxy, len(proxies))
	for name, proxy := range proxies {
		reloadedProxies[name] = proxy
	}
	reloadedProxies["AWS"] = newDNSProxyTestProxy(newDNSProxyTestBase("AWS-reloaded", C.Direct, true))
	updated := make(chan struct{})
	var nestedError error
	helper := C.RuleMatchHelper{ResolveIP: func() {
		// A reload must be able to complete while a normal IP rule awaits
		// DNS, and the DNS query must then enter the same rule matcher.
		currentSubRules, currentProviders, currentProxyProviders := subRules, ruleProviders, providers
		go func() {
			UpdateRules(reloadedRules, currentSubRules, currentProviders)
			UpdateProxies(reloadedProxies, currentProxyProviders)
			close(updated)
		}()
		select {
		case <-updated:
		case <-time.After(time.Second):
			nestedError = errors.New("configuration reload blocked by DNS lookup")
			return
		}
		route, err := selectDNSProxy(dnsRoutingMetadata("a.example", metadata))
		if err != nil {
			nestedError = err
		} else if route.proxy.Name() != "DMIT" {
			nestedError = errors.New("nested DNS query used unresolved website IP")
		}
		metadata.DstIP = netip.MustParseAddr("203.0.113.10")
	}}
	proxy, _, err := match(metadata, helper)
	<-updated
	if nestedError != nil || err != nil || proxy.Name() != "AWS-reloaded" {
		t.Fatalf("nested DNS/reload failed: proxy=%s, nested=%v, match=%v", proxy.Name(), nestedError, err)
	}
}

func TestDNSRoutingPlanFreezesGroupUDPCapability(t *testing.T) {
	dnsProxyTestState(t)
	sentinel := errors.New("frozen UDP leaf used")
	dials := 0
	leaf := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("leaf", C.Socks5, true)}
	leaf.packet = func(context.Context, *C.Metadata) (C.PacketConn, error) { dials++; return nil, sentinel }
	group := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("group", C.Selector, true)}
	group.choose = func(*C.Metadata, bool) C.Proxy { return newDNSProxyTestProxy(leaf) }
	proxies["group"] = newDNSProxyTestProxy(group)
	rules = []C.Rule{dnsProxyTestRule(t, "MATCH,group")}
	origin := dnsProxyTestResolver(C.TCP)
	query := dnsProxyTestQuery(t, "a.example.")
	plan, err := PrepareDNSRouting(context.Background(), "a.example.", origin)
	if err != nil {
		t.Fatal(err)
	}
	// A selector can now point at an unsupported node, but this plan retains
	// both its original leaf and the original group capability.
	group.udp = false
	group.choose = func(*C.Metadata, bool) C.Proxy { return proxies["wrong"] }
	if _, err := plan.Exchange(context.Background(), query, dnsProxyTestResolver(C.UDP)); !errors.Is(err, sentinel) || dials != 1 {
		t.Fatalf("live group changed a frozen UDP plan: dials=%d err=%v", dials, err)
	}
	blocked, err := PrepareDNSRouting(context.Background(), "a.example.", origin)
	if err != nil {
		t.Fatal(err)
	}
	group.udp = true
	if _, err := blocked.Exchange(context.Background(), query, dnsProxyTestResolver(C.UDP)); err == nil || dials != 1 {
		t.Fatalf("frozen group disable-udp was lost: dials=%d err=%v", dials, err)
	}
}

func TestDNSRoutingProcessLookupUsesOriginalSocketNetwork(t *testing.T) {
	dnsProxyTestState(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	source := connection.LocalAddr().(*net.TCPAddr).AddrPort()
	_, path, err := process.FindProcessName("tcp", source.Addr(), int(source.Port()))
	if err != nil || path == "" {
		t.Skipf("native process lookup unavailable: %v", err)
	}
	application := &C.Metadata{NetWork: C.TCP, Type: C.SOCKS5, SrcIP: source.Addr(), SrcPort: source.Port(), Host: "a.example", DstPort: 443}
	query := application.Clone()
	query.NetWork, query.DstPort = C.UDP, 53
	rules = []C.Rule{
		dnsProxyTestRule(t, "AND,((NETWORK,udp),(DST-PORT,53),(PROCESS-NAME,"+filepath.Base(path)+")),AWS"),
		dnsProxyTestRule(t, "MATCH,wrong"),
	}
	for _, findMode := range []process.FindProcessMode{process.FindProcessStrict, process.FindProcessAlways} {
		SetFindProcessMode(findMode)
		plan, err := PrepareDNSRouting(icontext.WithDNSRoutingMetadata(context.Background(), application), "a.example", query)
		if err != nil {
			t.Fatal(err)
		}
		if plan.route.proxy.Name() != "AWS" || plan.origin.ProcessPath != path {
			t.Fatalf("mode %s looked up the DNS transport instead of the source socket: route=%s process=%q", findMode, plan.route.proxy.Name(), plan.origin.ProcessPath)
		}
	}
	if application.Process != "" || query.Process != "" {
		t.Fatal("process lookup mutated caller-owned origin metadata")
	}
}
