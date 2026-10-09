package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/common/utils"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	RC "github.com/metacubex/mihomo/rules/common"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

// These fake outbound transports exercise the actual DNS wire bridge and
// resolver cache without making network requests or listening on port 53.
type routingTestBase struct {
	name string
	kind C.AdapterType
}

func (b *routingTestBase) Name() string                     { return b.name }
func (b *routingTestBase) Type() C.AdapterType              { return b.kind }
func (b *routingTestBase) Addr() string                     { return "" }
func (b *routingTestBase) SupportUDP() bool                 { return true }
func (b *routingTestBase) SupportUOT() bool                 { return false }
func (b *routingTestBase) ProxyInfo() C.ProxyInfo           { return C.ProxyInfo{} }
func (b *routingTestBase) MarshalJSON() ([]byte, error)     { return []byte("{}"), nil }
func (b *routingTestBase) Close() error                     { return nil }
func (b *routingTestBase) IsL3Protocol(*C.Metadata) bool    { return false }
func (b *routingTestBase) Unwrap(*C.Metadata, bool) C.Proxy { return nil }
func (b *routingTestBase) DialContext(context.Context, *C.Metadata) (C.Conn, error) {
	return nil, C.ErrNotSupport
}
func (b *routingTestBase) ListenPacketContext(context.Context, *C.Metadata) (C.PacketConn, error) {
	return nil, C.ErrNotSupport
}

type routingTestConnection struct{ chain C.Chain }

func (c *routingTestConnection) Chains() C.Chain                 { return c.chain }
func (c *routingTestConnection) ProviderChains() C.Chain         { return nil }
func (c *routingTestConnection) RemoteDestination() string       { return "" }
func (c *routingTestConnection) AppendToChains(a C.ProxyAdapter) { c.chain = append(c.chain, a.Name()) }

type routingTestTCPConn struct {
	N.ExtendedConn
	routingTestConnection
}
type routingTestUDPConn struct {
	N.EnhancePacketConn
	routingTestConnection
}

func (c *routingTestUDPConn) ResolveUDP(context.Context, *C.Metadata) error { return nil }

type routingTestProxyWrapper struct{ C.ProxyAdapter }

func routingTestProxy(a C.ProxyAdapter) C.Proxy                                 { return &routingTestProxyWrapper{a} }
func (p *routingTestProxyWrapper) Adapter() C.ProxyAdapter                      { return p.ProxyAdapter }
func (p *routingTestProxyWrapper) AliveForTestUrl(string) bool                  { return true }
func (p *routingTestProxyWrapper) DelayHistory() []C.DelayHistory               { return nil }
func (p *routingTestProxyWrapper) ExtraDelayHistories() map[string]C.ProxyState { return nil }
func (p *routingTestProxyWrapper) LastDelayForTestUrl(string) uint16            { return 0 }
func (p *routingTestProxyWrapper) URLTest(context.Context, string, utils.IntRanges[uint16]) (uint16, error) {
	return 0, C.ErrNotSupport
}

type routingTestCall struct {
	metadata  *C.Metadata
	bootstrap bool
	fixed     C.ProxyAdapter
}

type routingTestOutbound struct {
	*routingTestBase
	kind      atomic.Int32
	udp       atomic.Int32
	tcp       atomic.Int32
	calls     chan routingTestCall
	questions chan string
	gate      <-chan struct{}
	truncate  bool
}

func newRoutingTestOutbound(name string) *routingTestOutbound {
	a := &routingTestOutbound{
		routingTestBase: &routingTestBase{name: name, kind: C.Direct},
		calls:           make(chan routingTestCall, 128),
		questions:       make(chan string, 128),
	}
	a.kind.Store(int32(C.Direct))
	return a
}

func (a *routingTestOutbound) Type() C.AdapterType { return C.AdapterType(a.kind.Load()) }

func (a *routingTestOutbound) record(ctx context.Context, md *C.Metadata) {
	a.calls <- routingTestCall{md.Clone(), icontext.DNSBootstrap(ctx), icontext.DNSFixedOutbound(ctx)}
}

func (a *routingTestOutbound) DialContext(ctx context.Context, md *C.Metadata) (C.Conn, error) {
	a.tcp.Add(1)
	a.record(ctx, md)
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		conn := &D.Conn{Conn: server}
		query, err := conn.ReadMsg()
		if err != nil {
			return
		}
		a.questions <- query.Question[0].Name
		_ = conn.WriteMsg(routingTestAnswer(query, false))
	}()
	return &routingTestTCPConn{ExtendedConn: N.NewExtendedConn(client), routingTestConnection: routingTestConnection{chain: C.Chain{a.Name()}}}, nil
}

func (a *routingTestOutbound) ListenPacketContext(ctx context.Context, md *C.Metadata) (C.PacketConn, error) {
	a.udp.Add(1)
	a.record(ctx, md)
	pc := &routingTestPacketConn{outbound: a, packets: make(chan routingTestPacket, 1), closed: make(chan struct{})}
	return &routingTestUDPConn{EnhancePacketConn: N.NewEnhancePacketConn(pc), routingTestConnection: routingTestConnection{chain: C.Chain{a.Name()}}}, nil
}

func routingTestAnswer(query *D.Msg, truncated bool) *D.Msg {
	answer := new(D.Msg).SetReply(query)
	answer.Truncated = truncated
	if !truncated {
		answer.Answer = []D.RR{&D.A{Hdr: D.RR_Header{Name: query.Question[0].Name, Rrtype: D.TypeA, Class: D.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 99)}}
	}
	return answer
}

type routingTestPacket struct {
	wire []byte
	addr net.Addr
}

type routingTestPacketConn struct {
	outbound *routingTestOutbound
	packets  chan routingTestPacket
	closed   chan struct{}
	once     sync.Once
}

func (c *routingTestPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case packet := <-c.packets:
		return copy(p, packet.wire), packet.addr, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *routingTestPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	query := new(D.Msg)
	if err := query.Unpack(p); err != nil {
		return 0, err
	}
	c.outbound.questions <- query.Question[0].Name
	wire, err := routingTestAnswer(query, c.outbound.truncate).Pack()
	if err != nil {
		return 0, err
	}
	go func() {
		if c.outbound.gate != nil {
			select {
			case <-c.outbound.gate:
			case <-c.closed:
				return
			}
		}
		select {
		case c.packets <- routingTestPacket{wire, addr}:
		case <-c.closed:
		}
	}()
	return len(p), nil
}

func (c *routingTestPacketConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }
func (c *routingTestPacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
}
func (c *routingTestPacketConn) SetDeadline(time.Time) error      { return nil }
func (c *routingTestPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *routingTestPacketConn) SetWriteDeadline(time.Time) error { return nil }

func routingTestEnable(t *testing.T) {
	t.Helper()
	wasEnabled, mode := tunnel.DNSRuleRoutingEnabled(), tunnel.Mode()
	tunnel.SetDNSRuleRouting(true)
	tunnel.SetMode(tunnel.Rule)
	t.Cleanup(func() {
		tunnel.SetDNSRuleRouting(wasEnabled)
		tunnel.SetMode(mode)
	})
}

func routingTestResolver() *Resolver {
	return NewResolver(Config{RuleRouting: true, Main: []NameServer{{Addr: "192.0.2.53:53"}}}).Resolver
}

func routingTestOrigin() *C.Metadata {
	return &C.Metadata{
		Type: C.HTTP, NetWork: C.TCP, SrcIP: netip.MustParseAddr("192.0.2.10"), SrcPort: 41000,
		DstIP: netip.MustParseAddr("198.51.100.53"), DstPort: 53,
		InIP: netip.MustParseAddr("127.0.0.1"), InPort: 7890, InName: "mixed", InUser: "user",
		Process: "smartdns", ProcessPath: "/usr/bin/smartdns", Uid: 1000,
	}
}

func routingTestContext(a C.ProxyAdapter, md *C.Metadata) context.Context {
	ctx := icontext.WithDNSRoutingMetadata(context.Background(), md)
	return icontext.WithDNSFixedOutbound(ctx, a)
}

func routingTestQuery(name string, id uint16) *D.Msg {
	m := new(D.Msg).SetQuestion(D.Fqdn(name), D.TypeA)
	m.Id = id
	return m
}

func routingTestExchange(t *testing.T, r *Resolver, ctx context.Context, query *D.Msg) *D.Msg {
	t.Helper()
	answer, err := r.ExchangeContext(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if answer == nil || answer.Id != query.Id {
		t.Fatalf("wrong DNS response ID: query=%d answer=%+v", query.Id, answer)
	}
	return answer
}

func routingTestNextCall(t *testing.T, a *routingTestOutbound) routingTestCall {
	t.Helper()
	select {
	case call := <-a.calls:
		return call
	case <-time.After(3 * time.Second):
		t.Fatal("DNS upstream was not called")
		return routingTestCall{}
	}
}

func TestDNSRuleRoutingCacheScopeAndRealDestination(t *testing.T) {
	routingTestEnable(t)
	a, b := newRoutingTestOutbound("a"), newRoutingTestOutbound("b")
	r, md := routingTestResolver(), routingTestOrigin()
	ctx := routingTestContext(a, md)
	routingTestExchange(t, r, ctx, routingTestQuery("claude.ai", 1))
	routingTestExchange(t, r, ctx, routingTestQuery("claude.ai", 2))
	if a.udp.Load() != 1 {
		t.Fatal("identical scope did not reuse its cache")
	}
	call := routingTestNextCall(t, a)
	if call.metadata.Host != "" || call.metadata.SniffHost != "" || call.metadata.DstIP.String() != "192.0.2.53" || call.metadata.DstPort != 53 {
		t.Fatalf("business QNAME escaped into actual resolver dialing: %+v", call.metadata)
	}
	if call.metadata.SrcIP != md.SrcIP || call.metadata.SrcPort != md.SrcPort || call.metadata.InName != md.InName || call.metadata.InUser != md.InUser || call.metadata.Process != md.Process || call.metadata.NetWork != C.UDP {
		t.Fatalf("actual source or DNS transport lost: %+v", call.metadata)
	}
	md.SrcIP = netip.MustParseAddr("192.0.2.11")
	routingTestExchange(t, r, routingTestContext(a, md), routingTestQuery("claude.ai", 3))
	md.SpecialRules = "tenant-b"
	routingTestExchange(t, r, routingTestContext(a, md), routingTestQuery("claude.ai", 4))
	query := routingTestQuery("claude.ai", 5)
	query.SetEdns0(1232, true)
	routingTestExchange(t, r, routingTestContext(a, md), query)
	if a.udp.Load() != 4 {
		t.Fatalf("source/sub-rule/EDNS scopes shared answers: got %d queries", a.udp.Load())
	}
	routingTestExchange(t, r, routingTestContext(b, md), query)
	if b.udp.Load() != 1 {
		t.Fatal("different fixed outbound reused another outbound's answer")
	}
}

func TestDNSRuleRoutingRejectPrecedesCachedAnswer(t *testing.T) {
	routingTestEnable(t)
	a, r := newRoutingTestOutbound("live"), routingTestResolver()
	ctx, query := routingTestContext(a, routingTestOrigin()), routingTestQuery("blocked.example", 10)
	routingTestExchange(t, r, ctx, query)
	// Keep the same adapter identity/cache key while changing the live action.
	a.kind.Store(int32(C.Reject))
	if got := routingTestExchange(t, r, ctx, query); got.Rcode != D.RcodeRefused || len(got.Answer) != 0 {
		t.Fatalf("cached success bypassed REJECT: %v", got)
	}
	a.kind.Store(int32(C.RejectDrop))
	if _, err := r.ExchangeContext(ctx, query); !errors.Is(err, resolver.ErrDNSDrop) {
		t.Fatalf("REJECT-DROP lost its silent action: %v", err)
	}
	if a.udp.Load() != 1 {
		t.Fatal("reject action dialed an upstream")
	}
}

type routingTestGroup struct {
	*routingTestBase
	leaf    C.Proxy
	choices atomic.Int32
	seen    chan *C.Metadata
}

func (g *routingTestGroup) Unwrap(metadata *C.Metadata, _ bool) C.Proxy {
	g.choices.Add(1)
	if g.seen != nil {
		g.seen <- metadata.Clone()
	}
	return g.leaf
}

func TestDNSRuleRoutingTruncationFreezesGroupAndRetriesTCP(t *testing.T) {
	routingTestEnable(t)
	a := newRoutingTestOutbound("leaf")
	a.truncate = true
	group := &routingTestGroup{routingTestBase: &routingTestBase{name: "group", kind: C.Selector}, leaf: routingTestProxy(a)}
	got := routingTestExchange(t, routingTestResolver(), routingTestContext(group, routingTestOrigin()), routingTestQuery("claude.ai", 11))
	if got.Truncated || len(got.Answer) != 1 || a.udp.Load() != 1 || a.tcp.Load() != 1 || group.choices.Load() != 1 {
		t.Fatalf("retry changed route or did not use TCP: UDP=%d TCP=%d selections=%d reply=%v", a.udp.Load(), a.tcp.Load(), group.choices.Load(), got)
	}
}

func TestDNSRuleRoutingBootstrapAndExplicitExitExemptions(t *testing.T) {
	routingTestEnable(t)
	fixed, explicit := newRoutingTestOutbound("inherited-reject"), newRoutingTestOutbound("explicit")
	fixed.kind.Store(int32(C.Reject))
	ctx := routingTestContext(fixed, routingTestOrigin())
	ctx = context.WithValue(ctx, dnsQueryRouteKey{}, &dnsQueryRoute{key: "outer-query"})
	ns := NameServer{Addr: "192.0.2.53:53", ProxyAdapter: explicit}
	rs := NewResolver(Config{RuleRouting: true, Main: []NameServer{ns}, Default: []NameServer{ns}, ProxyServer: []NameServer{ns}})
	for _, test := range []struct {
		name string
		r    *Resolver
		boot bool
	}{{"explicit", rs.Resolver, false}, {"default", rs.BootstrapResolver, true}, {"proxy", rs.ProxyResolver, true}} {
		t.Run(test.name, func(t *testing.T) {
			got := routingTestExchange(t, test.r, ctx, routingTestQuery("node.example", 20))
			if len(got.Answer) != 1 {
				t.Fatal("inherited business REJECT overrode explicit/bootstrap resolver")
			}
			if call := routingTestNextCall(t, explicit); call.bootstrap != test.boot {
				t.Fatalf("bootstrap marker=%v, want %v", call.bootstrap, test.boot)
			}
		})
	}
	if fixed.udp.Load() != 0 {
		t.Fatal("bootstrap or explicit resolver used inherited business exit")
	}
	cleanCtx, _, err := (&Resolver{}).prepareDNSRouting(ctx, routingTestQuery("other.example", 21))
	if err != nil || queryRoute(cleanCtx) != nil {
		t.Fatal("nested resolver inherited a previous query's routing plan")
	}
}

func TestDNSRuleRoutingExplicitExitStillAnswersMixedReject(t *testing.T) {
	routingTestEnable(t)
	fixed, explicit := newRoutingTestOutbound("reject"), newRoutingTestOutbound("explicit")
	fixed.kind.Store(int32(C.Reject))
	r := NewResolver(Config{RuleRouting: true, Main: []NameServer{
		{Addr: "192.0.2.53:53"}, {Addr: "192.0.2.54:53", ProxyAdapter: explicit},
	}}).Resolver
	answer := routingTestExchange(t, r, routingTestContext(fixed, routingTestOrigin()), routingTestQuery("claude.ai", 30))
	if answer.Rcode != D.RcodeSuccess || len(answer.Answer) != 1 || explicit.udp.Load() != 1 || fixed.udp.Load() != 0 {
		t.Fatal("explicit nameserver exit did not retain its exemption")
	}
}

func TestDNSRuleRoutingSingleflightSeparatesSources(t *testing.T) {
	routingTestEnable(t)
	a, r := newRoutingTestOutbound("shared"), routingTestResolver()
	gate := make(chan struct{})
	a.gate = gate
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	results := make(chan error, 10)
	start := make(chan struct{})
	for index := 0; index < 10; index++ {
		md := routingTestOrigin()
		if index >= 8 {
			md.SpecialRules = fmt.Sprintf("tenant-%d", index)
		}
		ctx, query := routingTestContext(a, md), routingTestQuery("shared.example", uint16(index+100))
		go func() {
			<-start
			answer, err := r.ExchangeContext(ctx, query)
			if err == nil && (answer == nil || answer.Id != query.Id) {
				err = fmt.Errorf("shared query returned another caller's transaction ID")
			}
			results <- err
		}()
	}
	close(start)
	for index := 0; index < 3; index++ {
		routingTestNextCall(t, a)
	}
	release()
	for index := 0; index < 10; index++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("shared query did not complete")
		}
	}
	if a.udp.Load() != 3 {
		t.Fatalf("expected one shared + two isolated lookups, got %d", a.udp.Load())
	}
}

func TestDNSRuleRoutingStaleRefreshPreservesCancelledSource(t *testing.T) {
	routingTestEnable(t)
	a, r := newRoutingTestOutbound("refresh"), routingTestResolver()
	md := routingTestOrigin()
	md.SpecialRules = "refresh-tenant"
	ctx, cancel := context.WithCancel(routingTestContext(a, md))
	query := routingTestQuery("refresh.example", 200)
	prepared, _, err := r.prepareDNSRouting(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	r.cache.SetWithExpire(dnsCacheKey(prepared, query.Question[0]), routingTestAnswer(query, false), time.Now().Add(-time.Second))
	cancel()
	routingTestExchange(t, r, ctx, query)
	call := routingTestNextCall(t, a)
	if call.metadata.SrcIP != md.SrcIP || call.metadata.Process != md.Process || call.fixed != a || call.bootstrap {
		t.Fatalf("background refresh lost its business scope: %+v", call)
	}
	// Wait for the refresh to finish before restoring global test state.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, expire, _ := r.cache.GetWithExpire(dnsCacheKey(prepared, query.Question[0]))
		if expire.After(time.Now()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not populate cache")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDNSRuleRoutingQNAMEAndModePriority(t *testing.T) {
	routingTestEnable(t)
	oldProxies, oldProviders := tunnel.Proxies(), tunnel.Providers()
	oldRules, oldRuleProviders := tunnel.Rules(), tunnel.RuleProviders()
	t.Cleanup(func() {
		tunnel.UpdateProxies(oldProxies, oldProviders)
		tunnel.UpdateRules(oldRules, nil, oldRuleProviders)
	})
	first, philippine := newRoutingTestOutbound("first"), newRoutingTestOutbound("philippine")
	tunnel.UpdateProxies(map[string]C.Proxy{
		"DIRECT": routingTestProxy(first), "GLOBAL": routingTestProxy(first),
		"first": routingTestProxy(first), "philippine": routingTestProxy(philippine),
	}, nil)
	tunnel.UpdateRules([]C.Rule{RC.NewDomain("claude.ai", "philippine"), RC.NewMatch("first")}, nil, nil)
	r, md := routingTestResolver(), routingTestOrigin()
	ctx := icontext.WithDNSRoutingMetadata(context.Background(), md)
	routingTestExchange(t, r, ctx, routingTestQuery("claude.ai", 300))
	routingTestExchange(t, r, ctx, routingTestQuery("other.example", 301))
	if philippine.udp.Load() != 1 || first.udp.Load() != 1 {
		t.Fatal("QNAME did not choose its domain rule")
	}
	md.SpecialProxy = "first"
	routingTestExchange(t, r, icontext.WithDNSRoutingMetadata(context.Background(), md), routingTestQuery("claude.ai", 304))
	if first.udp.Load() != 2 {
		t.Fatal("inbound explicit outbound lost priority")
	}
}

func TestDNSRuleRoutingTransportScope(t *testing.T) {
	for _, test := range []struct {
		name string
		ns   NameServer
		auto bool
	}{
		{"UDP53", NameServer{Addr: "192.0.2.53:53"}, true},
		{"TCP53", NameServer{Net: "tcp", Addr: "192.0.2.53:53"}, true},
		{"rules", NameServer{Addr: "192.0.2.53:53", ProxyName: RespectRules}, true},
		{"group", NameServer{Addr: "192.0.2.53:53", ProxyName: "group"}, false},
		{"direct", NameServer{Addr: "192.0.2.53:53", ProxyName: "DIRECT"}, false},
		{"interface", NameServer{Addr: "192.0.2.53:53", ProxyName: "eth0"}, false},
		{"native6053", NameServer{Addr: "127.0.0.1:6053"}, true},
		{"native6553", NameServer{Addr: "127.0.0.1:6553"}, true},
		{"DoT53", NameServer{Net: "tls", Addr: "192.0.2.53:53"}, true},
		{"DoH", NameServer{Net: "https", Addr: "https://192.0.2.53/dns-query"}, true},
		{"DoQ", NameServer{Net: "quic", Addr: "192.0.2.53:853"}, true},
		{"explicit-DoH", NameServer{Net: "https", Addr: "https://192.0.2.53/dns-query", ProxyName: "DIRECT"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _ := dnsRoutingCapability(transform([]NameServer{test.ns}, nil)[0])
			if got != test.auto {
				t.Fatalf("automatic=%v, want %v", got, test.auto)
			}
		})
	}
}

func TestDNSRuleRoutingNonRuleModesPreserveLegacyTransport(t *testing.T) {
	routingTestEnable(t)
	oldProxies, oldProviders := tunnel.Proxies(), tunnel.Providers()
	t.Cleanup(func() { tunnel.UpdateProxies(oldProxies, oldProviders) })
	legacy, global := newRoutingTestOutbound("legacy-dialer"), newRoutingTestOutbound("global")
	tunnel.UpdateProxies(map[string]C.Proxy{"GLOBAL": routingTestProxy(global), "DIRECT": routingTestProxy(global)}, nil)
	for _, mode := range []tunnel.TunnelMode{tunnel.Global, tunnel.Direct} {
		t.Run(mode.String(), func(t *testing.T) {
			tunnel.SetMode(mode)
			r := routingTestResolver()
			c := r.main[0].(*client)
			// Substitute only the old transport dialer, keeping the configured
			// nameserver plain/unfixed. An incorrect QNAME interception would
			// bypass this sentinel and use GLOBAL/DIRECT instead.
			c.dialer = newDNSDialer(nil, legacy, "")
			ctx := icontext.WithDNSRoutingMetadata(context.Background(), routingTestOrigin())
			before := global.udp.Load()
			routingTestExchange(t, r, ctx, routingTestQuery("claude.ai", 400))
			if global.udp.Load() != before {
				t.Fatal("unfixed nameserver was forced through mode outbound")
			}
			call := routingTestNextCall(t, legacy)
			if call.metadata.Host != "" || call.metadata.DstIP.String() != "192.0.2.53" {
				t.Fatal("legacy transport did not retain resolver destination")
			}
			r = NewResolver(Config{RuleRouting: true, Main: []NameServer{{Addr: "192.0.2.53:53", ProxyName: RespectRules}}}).Resolver
			routingTestExchange(t, r, ctx, routingTestQuery("claude.ai", 401))
			if global.udp.Load() != before+1 {
				t.Fatal("legacy respect-rules stopped following mode")
			}
			call = routingTestNextCall(t, global)
			if call.metadata.Host != "" || call.metadata.SrcIP.IsValid() || call.metadata.Type != C.INNER {
				t.Fatalf("legacy respect-rules received QNAME/source metadata: %+v", call.metadata)
			}
		})
	}
}

type routingTestIPFilter struct{}

func (routingTestIPFilter) MatchIp(netip.Addr) bool { return true }

func TestDNSRuleRoutingMainFallbackShareOnePlan(t *testing.T) {
	routingTestEnable(t)
	a := newRoutingTestOutbound("fallback-leaf")
	group := &routingTestGroup{routingTestBase: &routingTestBase{name: "fallback-group", kind: C.Selector}, leaf: routingTestProxy(a)}
	r := NewResolver(Config{
		RuleRouting: true, Main: []NameServer{{Addr: "192.0.2.53:53"}},
		Fallback: []NameServer{{Addr: "192.0.2.54:53"}}, FallbackLazyQuery: true,
		FallbackIPFilter: []C.IpMatcher{routingTestIPFilter{}},
	}).Resolver
	routingTestExchange(t, r, routingTestContext(group, routingTestOrigin()), routingTestQuery("claude.ai", 500))
	first, second := routingTestNextCall(t, a), routingTestNextCall(t, a)
	if group.choices.Load() != 1 || first.metadata.DstIP.String() != "192.0.2.53" || second.metadata.DstIP.String() != "192.0.2.54" {
		t.Fatal("main/fallback did not share one route and distinct resolver destinations")
	}
}

func TestDNSRuleRoutingExplicitExitSurvivesAutomaticPlanFailure(t *testing.T) {
	routingTestEnable(t)
	fixed, explicit := newRoutingTestOutbound("unsupported"), newRoutingTestOutbound("explicit")
	fixed.kind.Store(int32(C.Dns))
	r := NewResolver(Config{RuleRouting: true, Main: []NameServer{
		{Addr: "192.0.2.53:53"}, {Addr: "192.0.2.54:53", ProxyAdapter: explicit},
	}}).Resolver
	answer := routingTestExchange(t, r, routingTestContext(fixed, routingTestOrigin()), routingTestQuery("claude.ai", 600))
	if answer.Rcode != D.RcodeSuccess || len(answer.Answer) != 1 || explicit.udp.Load() != 1 || fixed.udp.Load() != 0 {
		t.Fatal("automatic routing failure disabled an explicit nameserver exit")
	}
}

func TestDNSRuleRoutingQueryNetworkAndPort(t *testing.T) {
	routingTestEnable(t)
	for _, test := range []struct {
		name        string
		inbound     bool
		clientNet   C.NetWork
		clientPort  uint16
		upstreamNet string
		wantNet     C.NetWork
	}{
		{"website-443-uses-UDP-upstream", false, C.TCP, 443, "udp", C.UDP},
		{"website-443-uses-TCP-upstream", false, C.UDP, 443, "tcp", C.TCP},
		{"DNS-client-TCP-preserved", true, C.TCP, 1053, "udp", C.TCP},
		{"DNS-client-UDP-preserved", true, C.UDP, 1053, "tcp", C.UDP},
	} {
		t.Run(test.name, func(t *testing.T) {
			leaf := newRoutingTestOutbound("leaf")
			group := &routingTestGroup{routingTestBase: &routingTestBase{name: "group", kind: C.Selector}, leaf: routingTestProxy(leaf), seen: make(chan *C.Metadata, 1)}
			md := routingTestOrigin()
			md.NetWork, md.DstPort, md.InPort = test.clientNet, test.clientPort, 1053
			ctx := routingTestContext(group, md)
			if test.inbound {
				ctx = icontext.WithDNSRoutingInbound(ctx)
			}
			r := NewResolver(Config{RuleRouting: true, Main: []NameServer{{Addr: "192.0.2.53:53", Net: test.upstreamNet}}}).Resolver
			routingTestExchange(t, r, ctx, routingTestQuery("claude.ai", 700))
			selected := <-group.seen
			if selected.DstPort != 53 || selected.NetWork != test.wantNet || selected.InPort != 1053 || selected.SrcIP != md.SrcIP || selected.DstIP.IsValid() || selected.Host != "claude.ai" {
				t.Fatalf("wrong query matching metadata: %+v", selected)
			}
			if icontext.DNSRoutingMetadata(ctx).DstPort != test.clientPort {
				t.Fatal("route preparation mutated the caller's origin")
			}
		})
	}
}

type routingTestService struct {
	metadata *C.Metadata
	inbound  bool
	err      error
}

func (s *routingTestService) ServeMsg(ctx context.Context, query *D.Msg) (*D.Msg, error) {
	s.metadata, s.inbound = icontext.DNSRoutingMetadata(ctx), icontext.DNSRoutingInbound(ctx)
	if s.err != nil {
		return nil, s.err
	}
	return new(D.Msg).SetReply(query), nil
}

type routingTestResponseWriter struct{ msg *D.Msg }

func (w *routingTestResponseWriter) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1053}
}
func (w *routingTestResponseWriter) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 41000}
}
func (w *routingTestResponseWriter) WriteMsg(msg *D.Msg) error   { w.msg = msg; return nil }
func (w *routingTestResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *routingTestResponseWriter) Close() error                { return nil }
func (w *routingTestResponseWriter) TsigStatus() error           { return nil }
func (w *routingTestResponseWriter) TsigTimersOnly(bool)         {}
func (w *routingTestResponseWriter) Hijack()                     {}

func TestDNSRuleRoutingListenServerPreservesActualOrigin(t *testing.T) {
	for _, network := range []C.NetWork{C.TCP, C.UDP} {
		service, writer := &routingTestService{}, &routingTestResponseWriter{}
		handler := serverHandler{Server: &Server{service: service}, isUDP: network == C.UDP}
		handler.ServeDNS(writer, routingTestQuery("claude.ai", 800))
		md := service.metadata
		if !service.inbound || md == nil || md.NetWork != network || md.InName != "DNS" || md.InPort != 1053 || md.SrcIP.String() != "192.0.2.10" || md.SrcPort != 41000 || writer.msg.Id != 800 {
			t.Fatalf("dns.listen origin was not preserved: %+v", md)
		}
	}
}

func TestDNSRuleRoutingListenServerDropHasNoServfail(t *testing.T) {
	for _, network := range []C.NetWork{C.TCP, C.UDP} {
		for _, drop := range []bool{false, true} {
			service, writer := &routingTestService{err: errors.New("upstream failed")}, &routingTestResponseWriter{}
			if drop {
				service.err = fmt.Errorf("policy: %w", resolver.ErrDNSDrop)
			}
			handler := serverHandler{Server: &Server{service: service}, isUDP: network == C.UDP}
			handler.ServeDNS(writer, routingTestQuery("claude.ai", 900))
			if drop {
				if writer.msg != nil {
					t.Fatalf("intentional drop produced DNS response: %v", writer.msg)
				}
			} else if writer.msg == nil || writer.msg.Rcode != D.RcodeServerFailure {
				t.Fatal("ordinary upstream failure no longer returns SERVFAIL")
			}
		}
	}
}

func TestDNSRuleRoutingResolverHostnameUsesIndependentBootstrap(t *testing.T) {
	routingTestEnable(t)
	business, bootstrap := newRoutingTestOutbound("business"), newRoutingTestOutbound("bootstrap")
	r := NewResolver(Config{
		RuleRouting: true, Main: []NameServer{{Addr: "resolver.example:53"}},
		Default: []NameServer{{Addr: "192.0.2.53:53", ProxyAdapter: bootstrap}},
	}).Resolver
	routingTestExchange(t, r, routingTestContext(business, routingTestOrigin()), routingTestQuery("claude.ai", 1000))
	bootCall := routingTestNextCall(t, bootstrap)
	if !bootCall.bootstrap || bootCall.fixed != nil {
		t.Fatal("resolver hostname inherited its business outbound")
	}
	if qname := <-bootstrap.questions; qname != "resolver.example." {
		t.Fatalf("bootstrap was given the business domain: %q", qname)
	}
	businessCall := routingTestNextCall(t, business)
	if businessCall.metadata.Host != "" || businessCall.metadata.DstIP.String() != "192.0.2.99" || businessCall.metadata.DstPort != 53 {
		t.Fatalf("DNS server address was not bootstrapped independently: %+v", businessCall.metadata)
	}
	if qname := <-business.questions; qname != "claude.ai." {
		t.Fatalf("business DNS question changed: %q", qname)
	}
}

func TestDNSRuleRoutingUnusedFallbackDoesNotExemptNonIPReject(t *testing.T) {
	routingTestEnable(t)
	reject, unused := newRoutingTestOutbound("reject"), newRoutingTestOutbound("unused-explicit-fallback")
	r := NewResolver(Config{
		RuleRouting: true, Main: []NameServer{{Addr: "192.0.2.53:53"}},
		Fallback: []NameServer{{Addr: "192.0.2.54:53", ProxyAdapter: unused}},
	}).Resolver
	ctx := routingTestContext(reject, routingTestOrigin())
	for _, qtype := range []uint16{D.TypeTXT, D.TypeMX, D.TypeHTTPS} {
		query := new(D.Msg).SetQuestion("blocked.example.", qtype)
		reject.kind.Store(int32(C.Reject))
		if got := routingTestExchange(t, r, ctx, query); got.Rcode != D.RcodeRefused {
			t.Fatalf("unused fallback weakened REJECT for type %d: %v", qtype, got)
		}
		reject.kind.Store(int32(C.RejectDrop))
		if _, err := r.ExchangeContext(ctx, query); !errors.Is(err, resolver.ErrDNSDrop) {
			t.Fatalf("unused fallback weakened REJECT-DROP for type %d: %v", qtype, err)
		}
	}
	if reject.udp.Load() != 0 || unused.udp.Load() != 0 {
		t.Fatal("rejected non-IP query dialed an upstream")
	}
}

func TestDNSRuleRoutingUnusedFallbackDoesNotSelectGroupForNonIP(t *testing.T) {
	routingTestEnable(t)
	explicit, leaf := newRoutingTestOutbound("explicit-main"), newRoutingTestOutbound("unused-auto")
	group := &routingTestGroup{routingTestBase: &routingTestBase{name: "group", kind: C.Selector}, leaf: routingTestProxy(leaf)}
	r := NewResolver(Config{
		RuleRouting: true, Main: []NameServer{{Addr: "192.0.2.53:53", ProxyAdapter: explicit}},
		Fallback: []NameServer{{Addr: "192.0.2.54:53"}},
	}).Resolver
	routingTestExchange(t, r, routingTestContext(group, routingTestOrigin()), new(D.Msg).SetQuestion("claude.ai.", D.TypeTXT))
	if group.choices.Load() != 0 || leaf.udp.Load() != 0 || explicit.udp.Load() != 1 {
		t.Fatal("unused fallback caused an automatic group selection")
	}
}
