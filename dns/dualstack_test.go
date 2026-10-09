package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"

	D "github.com/miekg/dns"
)

type dualStackTestClient struct {
	exchange func(context.Context, *D.Msg) (*D.Msg, error)
	mu       sync.Mutex
	queries  []D.Question
}

func (c *dualStackTestClient) ExchangeContext(ctx context.Context, query *D.Msg) (*D.Msg, error) {
	c.mu.Lock()
	c.queries = append(c.queries, query.Question[0])
	c.mu.Unlock()
	return c.exchange(ctx, query)
}

func (c *dualStackTestClient) Address() string  { return "test://dualstack" }
func (c *dualStackTestClient) ResetConnection() {}
func (c *dualStackTestClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queries)
}

func dualStackTestAnswer(query *D.Msg) *D.Msg {
	response := new(D.Msg).SetReply(query)
	response.RecursionAvailable = true
	header := D.RR_Header{Name: query.Question[0].Name, Rrtype: query.Question[0].Qtype, Class: D.ClassINET, Ttl: 60}
	switch query.Question[0].Qtype {
	case D.TypeA:
		response.Answer = []D.RR{&D.A{Hdr: header, A: net.ParseIP("192.0.2.4")}}
	case D.TypeAAAA:
		response.Answer = []D.RR{&D.AAAA{Hdr: header, AAAA: net.ParseIP("2001:db8::6")}}
	}
	return response
}

func TestDualStackConfiguration(t *testing.T) {
	for _, duration := range []time.Duration{0, 10 * time.Millisecond, time.Second} {
		if err := ValidateDualStackConfig(DualStackConfig{Threshold: duration}); err != nil {
			t.Fatal(err)
		}
	}
	for _, duration := range []time.Duration{-time.Millisecond, time.Second + time.Millisecond} {
		if err := ValidateDualStackConfig(DualStackConfig{Threshold: duration}); err == nil {
			t.Fatalf("accepted out-of-range dual-stack threshold %v", duration)
		}
	}
}

func TestDualStackThresholdAndFailurePreservation(t *testing.T) {
	for _, test := range []struct {
		name       string
		qtype      uint16
		v4, v6     time.Duration
		threshold  time.Duration
		forceAAAA  bool
		v4Failure  string
		v6Failure  string
		wantNoData bool
	}{
		{name: "IPv4 faster", v4: 5, v6: 30, threshold: 10, wantNoData: true},
		{name: "inclusive threshold", v4: 5, v6: 15, threshold: 10, wantNoData: true},
		{name: "below threshold", v4: 5, v6: 14, threshold: 10},
		{name: "IPv6 faster", v4: 30, v6: 5, threshold: 10},
		{name: "explicit zero threshold", v4: 5, v6: 6, wantNoData: true},
		{name: "opposite family probe failure", v4Failure: "probe", v6: 30, threshold: 10},
		{name: "primary family probe failure", v4: 5, v6Failure: "probe", threshold: 10},
		{name: "all probes fail", v4Failure: "probe", v6Failure: "probe", threshold: 10},
		{name: "opposite lookup failure", v4Failure: "lookup", v6: 30, threshold: 10},
		{name: "opposite NXDOMAIN", v4Failure: "nxdomain", v6: 30, threshold: 10},
		{name: "opposite NODATA", v4Failure: "nodata", v6: 30, threshold: 10},
		{name: "signed opposite answer", v4Failure: "signed", v6: 30, threshold: 10},
		{name: "signed primary answer", v4: 5, v6Failure: "signed", threshold: 10},
		{name: "A remains available by default", qtype: D.TypeA, v4: 30, v6: 5, threshold: 10},
		{name: "explicit IPv6 only preference", qtype: D.TypeA, v4: 30, v6: 5, threshold: 10, forceAAAA: true, wantNoData: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			qtype := test.qtype
			if qtype == 0 {
				qtype = D.TypeAAAA
			}
			query := new(D.Msg).SetQuestion("dual.example.", qtype)
			query.Id = 72
			client := &dualStackTestClient{exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
				failure := test.v4Failure
				if query.Question[0].Qtype == D.TypeAAAA {
					failure = test.v6Failure
				}
				answer := dualStackTestAnswer(query)
				switch failure {
				case "lookup":
					return nil, errors.New("opposite resolver failed")
				case "nxdomain":
					answer.Rcode, answer.Answer = D.RcodeNameError, nil
				case "nodata":
					answer.Answer = nil
				case "signed":
					answer.AuthenticatedData = true
				}
				return answer, nil
			}}
			probe := func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
				rtt, failure := test.v4, test.v4Failure
				if ip.Is6() {
					rtt, failure = test.v6, test.v6Failure
				}
				if failure == "probe" {
					return 0, errors.New("unreachable")
				}
				return rtt * time.Millisecond, nil
			}
			checker := speedCheckTestChecker(t, time.Second, 2)
			answer, cache, err := checker.ExchangeDualStackWithProbe(context.Background(), []dnsClient{client}, query, probe,
				DualStackConfig{Enabled: true, Threshold: test.threshold * time.Millisecond, AllowForceAAAA: test.forceAAAA})
			if err != nil || answer == nil || answer.Id != query.Id || answer.Rcode != D.RcodeSuccess {
				t.Fatalf("lost primary DNS answer: %v, %v", answer, err)
			}
			if test.wantNoData {
				if len(answer.Answer) != 0 || len(answer.Ns) != 1 || answer.Ns[0].Header().Rrtype != D.TypeSOA || cache {
					t.Fatalf("expected transient NODATA: cache=%v, answer=%v", cache, answer)
				}
				soa := answer.Ns[0].(*D.SOA)
				if soa.Hdr.Ttl != 0 || soa.Minttl != 0 {
					t.Fatal("measured address-family preference became cacheable")
				}
				// ipExchange's older API drops the cache flag. Zero TTL must also
				// prevent storage after an independent answer policy is applied.
				adjusted := (AnswerPolicy{RRTTLMin: 300}).apply(query, answer)
				store := Config{}.newCache()
				putMsgToCache(store, "dualstack", query.Question[0], adjusted)
				if _, _, hit := store.GetWithExpire("dualstack"); hit {
					t.Fatal("synthetic NODATA was retained for serve-expired")
				}
			} else if len(answer.Answer) != 1 || answer.Answer[0].Header().Rrtype != qtype {
				t.Fatalf("a failure or insufficient speed difference filtered a valid address: %v", answer)
			}
			if qtype == D.TypeA && !test.forceAAAA && client.count() != 1 {
				t.Fatal("ordinary A query unnecessarily queried AAAA")
			}
		})
	}
}

func TestDualStackProtectedQuestionsAndDisabledModeDoNotQueryOtherFamily(t *testing.T) {
	for _, mode := range []string{"disabled", "DO", "CD", "AD", "TSIG", "HTTPS"} {
		t.Run(mode, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("protected.example.", D.TypeAAAA)
			switch mode {
			case "DO":
				query.SetEdns0(1232, true)
			case "CD":
				query.CheckingDisabled = true
			case "AD":
				query.AuthenticatedData = true
			case "TSIG":
				query.SetTsig("key.example.", D.HmacSHA256, 300, time.Now().Unix())
			case "HTTPS":
				query.Question[0].Qtype = D.TypeHTTPS
			}
			client := &dualStackTestClient{exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) { return dualStackTestAnswer(q), nil }}
			var probes atomic.Int32
			checker := speedCheckTestChecker(t, time.Second, 2)
			_, _, err := checker.ExchangeDualStackWithProbe(context.Background(), []dnsClient{client}, query,
				func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
					probes.Add(1)
					return time.Millisecond, nil
				},
				DualStackConfig{Enabled: mode != "disabled", Threshold: 10 * time.Millisecond})
			if err != nil || client.count() != 1 || (mode != "disabled" && probes.Load() != 0) {
				t.Fatalf("protected/disabled query started auxiliary work: queries=%d probes=%d err=%v", client.count(), probes.Load(), err)
			}
		})
	}
}

func TestDualStackBothFamiliesShareBudgetAndProbeLimit(t *testing.T) {
	query := new(D.Msg).SetQuestion("budget.example.", D.TypeAAAA)
	checker := speedCheckTestChecker(t, 40*time.Millisecond, 2)
	client := &dualStackTestClient{exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
		if q.Question[0].Qtype == D.TypeA {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return dualStackTestAnswer(q), nil
	}}
	start := time.Now()
	answer, _, err := checker.ExchangeDualStackWithProbe(context.Background(), []dnsClient{client}, query,
		func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) { return time.Millisecond, nil },
		DualStackConfig{Enabled: true, Threshold: 10 * time.Millisecond})
	if err != nil || len(answer.Answer) != 1 || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("auxiliary failure exceeded one budget or lost primary answer: %v, %v, %v", time.Since(start), answer, err)
	}

	checker = speedCheckTestChecker(t, time.Second, 2)
	client = &dualStackTestClient{exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
		answer := dualStackTestAnswer(q)
		for i := 1; i < 3; i++ {
			rr := D.Copy(answer.Answer[0])
			if a, ok := rr.(*D.A); ok {
				a.A = net.IPv4(192, 0, 2, byte(4+i))
			} else {
				ip := net.ParseIP("2001:db8::6")
				ip[15] += byte(i)
				rr.(*D.AAAA).AAAA = ip
			}
			answer.Answer = append(answer.Answer, rr)
		}
		return answer, nil
	}}
	var active, maximum, probes atomic.Int32
	probe := func(ctx context.Context, _ netip.Addr, _ speedCheckMode) (time.Duration, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		probes.Add(1)
		timer := time.NewTimer(2 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return time.Millisecond, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	_, _, err = checker.ExchangeDualStackWithProbe(context.Background(), []dnsClient{client}, query, probe,
		DualStackConfig{Enabled: true, Threshold: 10 * time.Millisecond})
	if err != nil || maximum.Load() > 2 || probes.Load() != 6 {
		t.Fatalf("families did not share the probe limiter: maximum=%d probes=%d err=%v", maximum.Load(), probes.Load(), err)
	}
}

func TestDualStackAuxiliaryKeepsFrozenRouteAndOriginalQuestion(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("direct-leaf")
	group := &routingTestGroup{routingTestBase: &routingTestBase{name: "selection", kind: C.Selector}, leaf: routingTestProxy(leaf)}
	r := NewResolver(nativeRoutingTestConfig()).Resolver
	t.Cleanup(r.Close)
	query := new(D.Msg).SetQuestion("frozen.example.", D.TypeAAAA)
	before := query.Copy()
	ctx, _, err := r.prepareDNSRouting(routingTestContext(group, routingTestOrigin()), query)
	if err != nil || queryRoute(ctx) == nil {
		t.Fatal("could not prepare a direct routing plan", err)
	}
	route := queryRoute(ctx)
	client := &dualStackTestClient{exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
		if queryRoute(ctx) != route || q.Question[0].Name != query.Question[0].Name {
			return nil, errors.New("auxiliary query selected another route or domain")
		}
		return dualStackTestAnswer(q), nil
	}}
	checker := speedCheckTestChecker(t, time.Second, 2)
	_, _, err = checker.ExchangeDualStackWithProbe(ctx, []dnsClient{client}, query,
		func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) { return time.Millisecond, nil },
		DualStackConfig{Enabled: true, Threshold: 10 * time.Millisecond})
	if err != nil || client.count() != 2 || group.choices.Load() != 1 || !speedCheckTestEqual(query, before) {
		t.Fatalf("auxiliary routing changed the plan/question: choices=%d queries=%d err=%v", group.choices.Load(), client.count(), err)
	}
}

func TestDualStackProxyQueriesDoNotProbeOrQueryOtherFamily(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("proxy-leaf")
	leaf.kind.Store(int32(C.Socks5))
	config := nativeRoutingTestConfig()
	config.SpeedCheck = SpeedCheckConfig{Mode: []string{"tcp:443"}, Timeout: time.Second, Concurrency: 2}
	config.DualStack = DualStackConfig{Enabled: true, Threshold: 10 * time.Millisecond}
	r := NewResolver(config).Resolver
	t.Cleanup(r.Close)
	query := new(D.Msg).SetQuestion("proxied.example.", D.TypeAAAA)
	if _, err := r.ExchangeContext(routingTestContext(leaf, routingTestOrigin()), query); err != nil {
		t.Fatal(err)
	}
	if leaf.udp.Load() != 1 || leaf.tcp.Load() != 0 {
		t.Fatalf("proxy DNS started dual-stack/probe work: udp=%d tcp=%d", leaf.udp.Load(), leaf.tcp.Load())
	}
}
