package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	C "github.com/metacubex/mihomo/constant"

	D "github.com/miekg/dns"
)

// Preserve the configured native transport's routing capabilities while
// supplying controlled complete upstream answers. The actual resolver, pool
// selection, fallback filter and routed destination probes still run normally.
type selectionTestClient struct {
	dnsClient
	exchange  func(context.Context, *D.Msg) (*D.Msg, error)
	mu        sync.Mutex
	questions []D.Question
}

func (c *selectionTestClient) Unwrap() dnsClient { return c.dnsClient }

func (c *selectionTestClient) ExchangeContext(ctx context.Context, query *D.Msg) (*D.Msg, error) {
	c.mu.Lock()
	c.questions = append(c.questions, query.Question[0])
	c.mu.Unlock()
	return c.exchange(ctx, query)
}

func (c *selectionTestClient) count(qtype uint16) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, q := range c.questions {
		if qtype == 0 || q.Qtype == qtype {
			count++
		}
	}
	return count
}

type selectionTestOutbound struct {
	*routingTestBase
	delays map[netip.Addr]time.Duration
	mu     sync.Mutex
	probes []*C.Metadata
}

func newSelectionTestOutbound() *selectionTestOutbound {
	return &selectionTestOutbound{
		routingTestBase: &routingTestBase{name: "policy-direct", kind: C.Direct},
		delays:          make(map[netip.Addr]time.Duration),
	}
}

func (a *selectionTestOutbound) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	a.mu.Lock()
	a.probes = append(a.probes, metadata.Clone())
	a.mu.Unlock()
	if delay := a.delays[metadata.DstIP]; delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	client, server := net.Pipe()
	_ = server.Close()
	return &routingTestTCPConn{ExtendedConn: N.NewExtendedConn(client), routingTestConnection: routingTestConnection{chain: C.Chain{a.Name()}}}, nil
}

func (a *selectionTestOutbound) probed(address string) bool {
	ip := netip.MustParseAddr(address)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, metadata := range a.probes {
		if metadata.DstIP == ip {
			return true
		}
	}
	return false
}

type selectionTestIPFilter map[netip.Addr]bool

func (f selectionTestIPFilter) MatchIp(ip netip.Addr) bool { return f[ip] }

type selectionTestExpiryCache struct {
	dnsCache
	writes chan time.Time
}

func (c *selectionTestExpiryCache) SetWithExpire(key string, message *D.Msg, expires time.Time) {
	c.writes <- expires
	c.dnsCache.SetWithExpire(key, message, expires)
}

func selectionTestAnswer(query *D.Msg, cname string, addresses ...string) *D.Msg {
	message := new(D.Msg).SetReply(query)
	message.RecursionAvailable = true
	owner := query.Question[0].Name
	if cname != "" {
		message.Answer = append(message.Answer, &D.CNAME{
			Hdr: D.RR_Header{Name: owner, Rrtype: D.TypeCNAME, Class: D.ClassINET, Ttl: 60}, Target: cname,
		})
		owner = cname
	}
	for _, address := range addresses {
		ip := netip.MustParseAddr(address)
		header := D.RR_Header{Name: owner, Class: D.ClassINET, Ttl: 60}
		if ip.Is4() {
			header.Rrtype = D.TypeA
			message.Answer = append(message.Answer, &D.A{Hdr: header, A: ip.AsSlice()})
		} else {
			header.Rrtype = D.TypeAAAA
			message.Answer = append(message.Answer, &D.AAAA{Hdr: header, AAAA: ip.AsSlice()})
		}
	}
	return message
}

func selectionTestConfig() Config {
	return Config{
		RuleRouting: true, IPv6: true,
		Main:              []NameServer{{Addr: "192.0.2.53:53"}},
		Fallback:          []NameServer{{Addr: "198.51.100.53:53"}},
		FallbackLazyQuery: true,
		SpeedCheck:        SpeedCheckConfig{Mode: []string{"tcp:443"}, Timeout: time.Second, Concurrency: 4},
	}
}

func TestDNSFallbackFiltersCompleteAnswerBeforeSelection(t *testing.T) {
	routingTestEnable(t)
	for _, lazy := range []bool{false, true} {
		for _, speed := range []bool{false, true} {
			t.Run(fmt.Sprintf("lazy=%v/speed=%v", lazy, speed), func(t *testing.T) {
				config := selectionTestConfig()
				config.FallbackLazyQuery = lazy
				// The fallback's own answer intentionally also matches this
				// main-only filter. The filter must not leak into fallback.
				config.FallbackIPFilter = []C.IpMatcher{selectionTestIPFilter{
					netip.MustParseAddr("192.0.2.5"):    true,
					netip.MustParseAddr("198.51.100.6"): true,
				}}
				if !speed {
					config.SpeedCheck.Mode = []string{"none"}
				}
				rs := NewResolver(config)
				defer rs.Close()
				r := rs.Resolver
				main := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
					return selectionTestAnswer(query, "main-cdn.example.", "192.0.2.4", "192.0.2.5"), nil
				}}
				fallback := &selectionTestClient{dnsClient: r.fallback[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
					return selectionTestAnswer(query, "fallback-cdn.example.", "198.51.100.6"), nil
				}}
				r.main, r.fallback = []dnsClient{main}, []dnsClient{fallback}
				leaf := newSelectionTestOutbound()
				leaf.delays[netip.MustParseAddr("192.0.2.5")] = 30 * time.Millisecond
				answer := routingTestExchange(t, r, routingTestContext(leaf, routingTestOrigin()), routingTestQuery("whole-answer.example", 77))
				ips := msgToIP(answer)
				if len(ips) != 1 || ips[0].String() != "198.51.100.6" || main.count(0) != 1 || fallback.count(0) != 1 {
					t.Fatalf("complete main answer did not trigger fallback: ips=%v main=%d fallback=%d", ips, main.count(0), fallback.count(0))
				}
				if len(answer.Answer) != 2 || answer.Answer[0].(*D.CNAME).Target != "fallback-cdn.example." {
					t.Fatalf("selected answer lost its own CNAME chain: %v", answer)
				}
				if leaf.probed("192.0.2.4") || leaf.probed("192.0.2.5") {
					t.Fatal("an address from the rejected complete main answer was probed")
				}
				if leaf.probed("198.51.100.6") != speed {
					t.Fatal("fallback probe eligibility changed")
				}
			})
		}
	}
}

func TestDNSFallbackSelectionCanUseAnotherAcceptedMainAnswer(t *testing.T) {
	routingTestEnable(t)
	config := selectionTestConfig()
	config.Main = append(config.Main, NameServer{Addr: "192.0.2.54:53"})
	config.FallbackIPFilter = []C.IpMatcher{selectionTestIPFilter{netip.MustParseAddr("192.0.2.5"): true}}
	rs := NewResolver(config)
	defer rs.Close()
	r := rs.Resolver
	dirty := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
		return selectionTestAnswer(query, "dirty.example.", "192.0.2.4", "192.0.2.5"), nil
	}}
	clean := &selectionTestClient{dnsClient: r.main[1], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
		return selectionTestAnswer(query, "clean.example.", "192.0.2.6"), nil
	}}
	fallback := &selectionTestClient{dnsClient: r.fallback[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
		return selectionTestAnswer(query, "fallback.example.", "198.51.100.6"), nil
	}}
	r.main, r.fallback = []dnsClient{dirty, clean}, []dnsClient{fallback}
	leaf := newSelectionTestOutbound()
	answer := routingTestExchange(t, r, routingTestContext(leaf, routingTestOrigin()), routingTestQuery("main-choice.example", 78))
	ips := msgToIP(answer)
	if len(ips) != 1 || ips[0].String() != "192.0.2.6" || answer.Answer[0].(*D.CNAME).Target != "clean.example." {
		t.Fatalf("selection did not retain the accepted upstream's intact answer: %v", answer)
	}
	if dirty.count(0) != 1 || clean.count(0) != 1 || fallback.count(0) != 0 || leaf.probed("192.0.2.4") || leaf.probed("192.0.2.5") {
		t.Fatalf("rejected main answer affected candidates or fallback: dirty=%d clean=%d fallback=%d", dirty.count(0), clean.count(0), fallback.count(0))
	}
}

func TestDNSFallbackFilteredAnswerDoesNotWaitForUnresponsiveMain(t *testing.T) {
	routingTestEnable(t)
	for _, delay := range []time.Duration{0, 40 * time.Millisecond} {
		t.Run(fmt.Sprintf("filtered-response-delay=%v", delay), func(t *testing.T) {
			config := selectionTestConfig()
			config.Main = append(config.Main, NameServer{Addr: "192.0.2.54:53"})
			config.SpeedCheck.Timeout = 20 * time.Millisecond
			config.FallbackIPFilter = []C.IpMatcher{selectionTestIPFilter{netip.MustParseAddr("192.0.2.5"): true}}
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			stop := make(chan struct{})
			defer close(stop)
			filtered := &selectionTestClient{dnsClient: r.main[0], exchange: func(ctx context.Context, query *D.Msg) (*D.Msg, error) {
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-timer.C:
					return selectionTestAnswer(query, "", "192.0.2.5"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}
			unresponsive := &selectionTestClient{dnsClient: r.main[1], exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-stop:
					return nil, errors.New("test finished")
				}
			}}
			fallback := &selectionTestClient{dnsClient: r.fallback[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
				return selectionTestAnswer(query, "", "198.51.100.6"), nil
			}}
			r.main, r.fallback = []dnsClient{filtered, unresponsive}, []dnsClient{fallback}
			leaf := newSelectionTestOutbound()
			ctx, cancel := context.WithTimeout(routingTestContext(leaf, routingTestOrigin()), 500*time.Millisecond)
			defer cancel()
			answer := routingTestExchange(t, r, ctx, routingTestQuery("prompt-fallback.example", 81))
			ips := msgToIP(answer)
			if len(ips) != 1 || ips[0].String() != "198.51.100.6" || filtered.count(0) != 1 || unresponsive.count(0) != 1 || fallback.count(0) != 1 {
				t.Fatalf("filtered response starved lazy fallback: ips=%v filtered=%d waiting=%d fallback=%d", ips, filtered.count(0), unresponsive.count(0), fallback.count(0))
			}
		})
	}
}

func TestDNSFallbackLifetimeIncludesWaitForOptimizedMain(t *testing.T) {
	routingTestEnable(t)
	for _, algorithm := range []string{"lru", "arc"} {
		for _, test := range []struct {
			name    string
			budget  time.Duration
			ttl     uint32
			wantTTL uint32
		}{
			{name: "expires-while-waiting", budget: 1100 * time.Millisecond, ttl: 1, wantTTL: 0},
			{name: "absolute-expiry-includes-subsecond-wait", budget: 800 * time.Millisecond, ttl: 5, wantTTL: 5},
		} {
			t.Run(algorithm+"/"+test.name, func(t *testing.T) {
				config := selectionTestConfig()
				config.CacheAlgorithm = algorithm
				config.CacheOptions = &CacheOptions{ServeExpired: false}
				config.FallbackLazyQuery = false
				config.SpeedCheck.Timeout = test.budget
				config.Main = append(config.Main, NameServer{Addr: "192.0.2.54:53"})
				config.FallbackIPFilter = []C.IpMatcher{selectionTestIPFilter{netip.MustParseAddr("192.0.2.5"): true}}
				// This fixed transport bypasses speed checking, but its early
				// response still waits for the optimized main pool's decision.
				config.Fallback[0].ProxyAdapter = newRoutingTestOutbound("fixed-fallback")
				rs := NewResolver(config)
				defer rs.Close()
				r := rs.Resolver
				cache := &selectionTestExpiryCache{dnsCache: r.cache, writes: make(chan time.Time, 1)}
				r.cache = cache
				stop := make(chan struct{})
				defer close(stop)
				filtered := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
					return selectionTestAnswer(query, "", "192.0.2.5"), nil
				}}
				unresponsive := &selectionTestClient{dnsClient: r.main[1], exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-stop:
						return nil, errors.New("test finished")
					}
				}}
				fallbackReceived := make(chan time.Time, 1)
				fallback := &selectionTestClient{dnsClient: r.fallback[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
					answer := selectionTestAnswer(query, "", "198.51.100.6")
					answer.Answer[0].Header().Ttl = test.ttl
					fallbackReceived <- time.Now()
					return answer, nil
				}}
				r.main, r.fallback = []dnsClient{filtered, unresponsive}, []dnsClient{fallback}
				leaf := newSelectionTestOutbound()
				query := routingTestQuery("fallback-lifetime.example", 82)
				ctx := routingTestContext(leaf, routingTestOrigin())
				prepared, _, err := r.prepareDNSRouting(ctx, query)
				if err != nil || !r.speedCheckEligible(prepared, r.main, query) || r.speedCheckEligible(prepared, r.fallback, query) {
					t.Fatalf("test did not select optimized main and explicit fallback: %v", err)
				}
				answer := routingTestExchange(t, r, ctx, query)
				receivedAt := <-fallbackReceived
				ips := msgToIP(answer)
				if len(ips) != 1 || ips[0].String() != "198.51.100.6" || answer.Answer[0].Header().Ttl != test.wantTTL {
					t.Fatalf("waiting for main restarted fallback's TTL: answer=%v want TTL=%d", answer, test.wantTTL)
				}
				if leaf.probed("198.51.100.6") || fallback.count(0) != 1 {
					t.Fatal("explicit fallback was probed or queried more than once")
				}
				if test.wantTTL == 0 {
					if _, _, hit := cache.GetWithExpire(r.cacheKey(prepared, query)); hit {
						t.Fatal("fallback expired during main selection entered the formal cache")
					}
					select {
					case <-cache.writes:
						t.Fatal("an exhausted fallback lifetime was renewed")
					default:
					}
					return
				}
				// Inspect the exact deadline passed to LRU/ARC before their
				// second-level rounding could hide a subsecond extension.
				expires := <-cache.writes
				want := receivedAt.Add(time.Duration(test.ttl) * time.Second)
				if expires.Before(want) || expires.After(want.Add(100*time.Millisecond)) {
					t.Fatalf("fallback cache expiry restarted on delivery: received=%s got=%s want near=%s", receivedAt, expires, want)
				}
			})
		}
	}
}

func TestDNSFallbackRetainsAcceptedDualStackNoData(t *testing.T) {
	routingTestEnable(t)
	config := selectionTestConfig()
	config.DualStack = DualStackConfig{Enabled: true, Threshold: 10 * time.Millisecond}
	rs := NewResolver(config)
	defer rs.Close()
	r := rs.Resolver
	main := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
		return dualStackTestAnswer(query), nil
	}}
	fallback := &selectionTestClient{dnsClient: r.fallback[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
		return selectionTestAnswer(query, "", "2001:db8::7"), nil
	}}
	r.main, r.fallback = []dnsClient{main}, []dnsClient{fallback}
	leaf := newSelectionTestOutbound()
	leaf.delays[netip.MustParseAddr("2001:db8::6")] = 40 * time.Millisecond
	query := new(D.Msg).SetQuestion("accepted-dualstack.example.", D.TypeAAAA)
	answer := routingTestExchange(t, r, routingTestContext(leaf, routingTestOrigin()), query)
	if len(msgToIP(answer)) != 0 || len(answer.Ns) != 1 || answer.Ns[0].Header().Rrtype != D.TypeSOA || answer.Ns[0].Header().Ttl != 0 {
		t.Fatalf("valid measured NODATA was replaced: %v", answer)
	}
	if main.count(D.TypeA) != 1 || main.count(D.TypeAAAA) != 1 || fallback.count(0) != 0 {
		t.Fatalf("measured NODATA incorrectly started fallback: A=%d AAAA=%d fallback=%d", main.count(D.TypeA), main.count(D.TypeAAAA), fallback.count(0))
	}
}

func TestDNSFallbackRejectsAuxiliaryBeforeFamilySelection(t *testing.T) {
	routingTestEnable(t)
	config := selectionTestConfig()
	config.DualStack = DualStackConfig{Enabled: true, Threshold: 10 * time.Millisecond, AllowForceAAAA: true}
	config.FallbackIPFilter = []C.IpMatcher{selectionTestIPFilter{netip.MustParseAddr("2001:db8::6"): true}}
	rs := NewResolver(config)
	defer rs.Close()
	r := rs.Resolver
	main := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
		return dualStackTestAnswer(query), nil
	}}
	fallback := &selectionTestClient{dnsClient: r.fallback[0], exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
		return nil, errors.New("fallback must not be used")
	}}
	r.main, r.fallback = []dnsClient{main}, []dnsClient{fallback}
	leaf := newSelectionTestOutbound()
	leaf.delays[netip.MustParseAddr("192.0.2.4")] = 40 * time.Millisecond
	answer := routingTestExchange(t, r, routingTestContext(leaf, routingTestOrigin()), routingTestQuery("filtered-auxiliary.example", 79))
	ips := msgToIP(answer)
	if len(ips) != 1 || ips[0].String() != "192.0.2.4" || fallback.count(0) != 0 || leaf.probed("2001:db8::6") {
		t.Fatalf("rejected auxiliary suppressed the valid family: answer=%v fallback=%d", answer, fallback.count(0))
	}
}

func TestDNSIndependentDirectPoolKeepsItsOwnFilterPolicy(t *testing.T) {
	routingTestEnable(t)
	config := selectionTestConfig()
	config.DirectServer = []NameServer{{Addr: "203.0.113.53:53"}}
	config.FallbackIPFilter = []C.IpMatcher{routingTestIPFilter{}}
	rs := NewResolver(config)
	defer rs.Close()
	r := rs.Resolver
	main := &selectionTestClient{dnsClient: r.main[0], exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
		return nil, errors.New("unselected main pool")
	}}
	fallback := &selectionTestClient{dnsClient: r.fallback[0], exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
		return nil, errors.New("unselected fallback pool")
	}}
	direct := &selectionTestClient{dnsClient: r.direct[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
		return selectionTestAnswer(query, "", "192.0.2.4"), nil
	}}
	r.main, r.fallback, r.direct = []dnsClient{main}, []dnsClient{fallback}, []dnsClient{direct}
	leaf := newSelectionTestOutbound()
	answer := routingTestExchange(t, r, routingTestContext(leaf, routingTestOrigin()), routingTestQuery("direct-filter.example", 80))
	if ips := msgToIP(answer); len(ips) != 1 || ips[0].String() != "192.0.2.4" || !leaf.probed("192.0.2.4") {
		t.Fatalf("main filter leaked into independent direct pool: %v", answer)
	}
	if main.count(0) != 0 || fallback.count(0) != 0 || direct.count(0) != 1 {
		t.Fatalf("direct exchange borrowed another pool: main=%d fallback=%d direct=%d", main.count(0), fallback.count(0), direct.count(0))
	}
}
