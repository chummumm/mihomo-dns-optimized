package dns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

func TestSpeedCheckMultiAddressAcrossCompatibleUpstreams(t *testing.T) {
	for _, qtype := range []uint16{D.TypeA, D.TypeAAAA} {
		for _, cname := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/cname=%t", qtype, cname), func(t *testing.T) {
				query := new(D.Msg).SetQuestion("example.", qtype)
				addresses := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}
				if qtype == D.TypeAAAA {
					addresses = []string{"2001:db8::1", "2001:db8::2", "2001:db8::3"}
				}
				checker := speedCheckTestChecker(t, time.Second, 4)
				checker.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
					for i, address := range addresses {
						if ip.String() == address {
							return time.Duration(3-i) * time.Millisecond, nil
						}
					}
					return 0, errors.New("unexpected address")
				}
				var clients []dnsClient
				for _, address := range addresses {
					owner := ""
					if cname {
						owner = "cdn.example."
					}
					clients = append(clients, speedCheckTestClient{message: selectionTestAnswer(query, owner, address)})
				}
				answer, _, err := checker.Exchange(context.Background(), clients, query)
				want := []netip.Addr{netip.MustParseAddr(addresses[2]), netip.MustParseAddr(addresses[1]), netip.MustParseAddr(addresses[0])}
				if err != nil || !reflect.DeepEqual(msgToIP(answer), want) {
					t.Fatalf("compatible singleton upstreams did not form ranked multi-address reply: %v, %v", answer, err)
				}
				if cname && (len(answer.Answer) != 4 || answer.Answer[0].Header().Rrtype != D.TypeCNAME) {
					t.Fatalf("merged answer lost the CNAME path: %v", answer)
				}
			})
		}
	}
}

func TestSpeedCheckMultiAddressLimitsAndFiltering(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	var records []string
	for i := 20; i > 0; i-- {
		records = append(records, fmt.Sprintf("example. 60 IN A 192.0.2.%d", i))
	}
	for _, limit := range []int{0, 1, 3, 16, 32} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			checker := speedCheckTestChecker(t, time.Second, 32)
			if limit != 0 {
				checker.maxReplyIPNum = limit
			}
			checker.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
				return time.Duration(ip.As4()[3]) * 100 * time.Microsecond, nil
			}
			answer, _, err := checker.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: speedCheckTestReply(t, query, records...)}}, query)
			want := limit
			if want == 0 {
				want = 8
			}
			if want > 20 {
				want = 20
			}
			if err != nil || len(msgToIP(answer)) != want {
				t.Fatalf("count is not bounded by configured maximum and available fast IPs: %v, %v", answer, err)
			}
			for i, ip := range msgToIP(answer) {
				if ip.As4()[3] != byte(i+1) {
					t.Fatalf("reply was not fastest-first: %v", msgToIP(answer))
				}
			}
		})
	}
}

func TestSpeedCheckMultiAddressSmartDNSNearFastThresholds(t *testing.T) {
	for _, test := range []struct {
		best, candidate time.Duration
		want            bool
	}{
		{time.Millisecond, 9900 * time.Microsecond, true},
		{time.Millisecond, 10 * time.Millisecond, false},
		{20 * time.Millisecond, 24900 * time.Microsecond, true},
		{20 * time.Millisecond, 25 * time.Millisecond, false},
		{100 * time.Millisecond, 110500 * time.Microsecond, true},
		{100 * time.Millisecond, 110600 * time.Microsecond, false},
		{0, 0, true},
	} {
		if got := speedCheckNearFastest(test.best, test.candidate); got != test.want {
			t.Errorf("best=%s candidate=%s: near-fast=%t, want %t", test.best, test.candidate, got, test.want)
		}
	}
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	checker := speedCheckTestChecker(t, time.Second, 4)
	checker.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
		switch ip.As4()[3] {
		case 1:
			return 20 * time.Millisecond, nil
		case 2:
			return 24 * time.Millisecond, nil
		case 3:
			return 25 * time.Millisecond, nil
		default:
			return 0, errors.New("unreachable")
		}
	}
	answer, _, err := checker.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: selectionTestAnswer(query, "", "192.0.2.4", "192.0.2.3", "192.0.2.2", "192.0.2.1")}}, query)
	if err != nil || !reflect.DeepEqual(msgToIP(answer), []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}) {
		t.Fatalf("slow or unreachable address entered preferred reply: %v, %v", answer, err)
	}
}

func TestSpeedCheckMultiAddressDoesNotMixDistinctOrAmbiguousChains(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	winning := speedCheckTestReply(t, query,
		"example. 60 IN CNAME first.example.", "first.example. 60 IN CNAME final.example.", "final.example. 60 IN A 192.0.2.1")
	compatible := speedCheckTestReply(t, query,
		"EXAMPLE. 60 IN CNAME FIRST.EXAMPLE.", "FIRST.EXAMPLE. 60 IN CNAME FINAL.EXAMPLE.", "FINAL.EXAMPLE. 60 IN A 192.0.2.2")
	incompatible := speedCheckTestReply(t, query,
		"example. 60 IN CNAME other.example.", "other.example. 60 IN CNAME final.example.", "final.example. 60 IN A 192.0.2.3")
	ambiguous := speedCheckTestReply(t, query,
		"example. 60 IN CNAME first.example.", "example. 60 IN CNAME other.example.", "first.example. 60 IN CNAME final.example.", "final.example. 60 IN A 192.0.2.4")
	checker := speedCheckTestChecker(t, time.Second, 4)
	checker.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
		if ip.As4()[3] == 4 {
			t.Error("ambiguous CNAME chain was probed")
		}
		return time.Duration(ip.As4()[3]) * time.Millisecond, nil
	}
	answer, _, err := checker.Exchange(context.Background(), []dnsClient{
		speedCheckTestClient{message: winning}, speedCheckTestClient{message: compatible}, speedCheckTestClient{message: incompatible}, speedCheckTestClient{message: ambiguous},
	}, query)
	if err != nil || !reflect.DeepEqual(msgToIP(answer), []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}) {
		t.Fatalf("different paths sharing a terminal name were merged: %v, %v", answer, err)
	}
	if answer.Answer[0].(*D.CNAME).Target != "first.example." || answer.Answer[1].(*D.CNAME).Target != "final.example." {
		t.Fatalf("the winning complete CNAME chain changed: %v", answer)
	}
}

func TestSpeedCheckMultiAddressPreservesConservativeSourceLifetimes(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	winning := speedCheckTestReply(t, query, "example. 60 IN CNAME cdn.example.", "cdn.example. 120 IN A 192.0.2.1")
	older := speedCheckTestReply(t, query,
		"example. 20 IN CNAME cdn.example.", "cdn.example. 5 IN A 192.0.2.1", "cdn.example. 30 IN A 192.0.2.2")
	beforeWinning, beforeOlder := winning.Copy(), older.Copy()
	ctx := withDNSAnswerLifetimes(context.Background())
	oldReceipt := time.Now().Add(-2500 * time.Millisecond)
	recordDNSAnswerReceived(ctx, winning, oldReceipt.Add(2*time.Second))
	recordDNSAnswerReceived(ctx, older, oldReceipt)
	best := speedCheckResult{candidate: speedCheckCandidates(winning, query.Question[0])[0], rtt: time.Millisecond}
	answer := speedCheckAnswer(ctx, best, query.Question[0], []*D.Msg{winning, older}, map[netip.Addr]time.Duration{
		netip.MustParseAddr("192.0.2.1"): time.Millisecond, netip.MustParseAddr("192.0.2.2"): 2 * time.Millisecond,
	}, 8)
	if got := []uint32{answer.Answer[0].Header().Ttl, answer.Answer[1].Header().Ttl, answer.Answer[2].Header().Ttl}; !reflect.DeepEqual(got, []uint32{20, 5, 30}) {
		t.Fatalf("merged chain/address TTLs extended duplicate source lifetimes: %v", got)
	}
	if !dnsAnswerReceivedAt(ctx, answer).Equal(oldReceipt) {
		t.Fatal("merged answer lost its oldest contributing receipt")
	}
	adjusted := (AnswerPolicy{RRTTLMin: 10}).apply(query, answer)
	aged := ageDNSAnswerAfterPolicy(ctx, answer, adjusted)
	if got := []uint32{aged.Answer[0].Header().Ttl, aged.Answer[1].Header().Ttl, aged.Answer[2].Header().Ttl}; !reflect.DeepEqual(got, []uint32{18, 8, 28}) {
		t.Fatalf("TTL minimum revived time already spent collecting candidates: %v", got)
	}
	if !dnsAnswerExpires(ctx, aged).Equal(oldReceipt.Add(10 * time.Second)) {
		t.Fatal("merged cache expiry lost the source's subsecond lifetime")
	}
	if !speedCheckTestEqual(winning, beforeWinning) || !speedCheckTestEqual(older, beforeOlder) {
		t.Fatal("merging/aging changed an upstream's raw candidate response")
	}
}

func TestSpeedCheckMultiAddressEarlyAndCompletedResponseModes(t *testing.T) {
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse} {
		t.Run(mode, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("example.", D.TypeA)
			checker := speedCheckTestChecker(t, time.Second, 4)
			checker.responseMode, checker.maxReplyIPNum = mode, 2
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			checker.probe = func(ctx context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
				if ip.As4()[3] != 1 {
					select {
					case <-release:
					case <-ctx.Done():
						return 0, ctx.Err()
					}
				}
				return time.Duration(ip.As4()[3]) * time.Millisecond, nil
			}
			early := make(chan *D.Msg, 1)
			ctx := withDNSEarlyResponse(context.Background(), func(answer *D.Msg) bool { early <- answer; return true })
			completed := make(chan result, 1)
			go func() {
				answer, _, err := checker.Exchange(ctx, []dnsClient{speedCheckTestClient{message: selectionTestAnswer(query, "", "192.0.2.3", "192.0.2.1", "192.0.2.2")}}, query)
				completed <- result{answer, err}
			}()
			if mode != ResponseModeFastestIP {
				answer := responseModeReceive(t, early)
				want := 1
				if mode == ResponseModeFastestResponse {
					want = 2
					if msgToIP(answer)[0].String() != "192.0.2.3" {
						t.Fatal("unmeasured earliest reply did not keep upstream order")
					}
				}
				if len(msgToIP(answer)) != want {
					t.Fatalf("initial response mode count=%d, want %d", len(msgToIP(answer)), want)
				}
			}
			releaseOnce.Do(func() { close(release) })
			final := responseModeReceive(t, completed)
			if final.Error != nil || !reflect.DeepEqual(msgToIP(final.Msg), []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}) {
				t.Fatalf("completed mode did not retain multiple preferred addresses: %v, %v", final.Msg, final.Error)
			}
		})
	}
}

func TestSpeedCheckMultiAddressFallbackCountDoesNotInventProbeSuccess(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	checker := speedCheckTestChecker(t, time.Second, 4)
	checker.maxReplyIPNum = 2
	checker.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		return 0, errors.New("all addresses unmeasured")
	}
	original := selectionTestAnswer(query, "", "192.0.2.3", "192.0.2.2", "192.0.2.3", "192.0.2.1")
	original.Answer[2].Header().Ttl = 5
	before := original.Copy()
	answer, _, err := checker.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: original}}, query)
	if err != nil || !reflect.DeepEqual(msgToIP(answer), []netip.Addr{netip.MustParseAddr("192.0.2.3"), netip.MustParseAddr("192.0.2.2")}) {
		t.Fatalf("failed probes lost bounded upstream fallback order: %v, %v", answer, err)
	}
	if answer.Answer[0].Header().Ttl != 5 || !speedCheckTestEqual(original, before) {
		t.Fatal("fallback deduplication extended a duplicate TTL or mutated the raw response")
	}
	protected := original.Copy()
	protected.AuthenticatedData = true
	answer, _, err = checker.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: protected}}, query)
	if err != nil || !speedCheckTestEqual(answer, protected) {
		t.Fatalf("reply count changed an authenticated RRset: %v, %v", answer, err)
	}
}

func TestDNSResponseModeMultiAddressCachePreservesSelection(t *testing.T) {
	routingTestEnable(t)
	config := selectionTestConfig()
	config.Fallback = nil
	config.Main = append(config.Main, NameServer{Addr: "192.0.2.54:53"})
	rs := NewResolver(config)
	defer rs.Close()
	r := rs.Resolver
	first := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
		return selectionTestAnswer(q, "cdn.example.", "192.0.2.1"), nil
	}}
	second := &selectionTestClient{dnsClient: r.main[1], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
		return selectionTestAnswer(q, "cdn.example.", "192.0.2.2"), nil
	}}
	r.main = []dnsClient{first, second}
	query := routingTestQuery("cached-multiple.example", 100)
	ctx := routingTestContext(newSelectionTestOutbound(), routingTestOrigin())
	answer := routingTestExchange(t, r, ctx, query)
	if len(msgToIP(answer)) != 2 {
		t.Fatalf("resolver narrowed compatible singleton upstreams: %v", answer)
	}
	query.Id++
	cached := routingTestExchange(t, r, ctx, query)
	if !reflect.DeepEqual(msgToIP(cached), msgToIP(answer)) || first.count(0) != 1 || second.count(0) != 1 {
		t.Fatalf("cache lost ranked multi-address answer or restarted upstreams: %v, counts=%d/%d", cached, first.count(0), second.count(0))
	}
}

func TestSpeedCheckMultiAddressOrderedLookupExcludesUnrelatedAnswers(t *testing.T) {
	routingTestEnable(t)
	for _, qtype := range []uint16{D.TypeA, D.TypeAAAA} {
		t.Run(D.TypeToString[qtype], func(t *testing.T) {
			config := selectionTestConfig()
			config.Fallback = nil
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			addresses := []string{"192.0.2.1", "192.0.2.2"}
			unrelated := "unrelated.example. 60 IN A 192.0.2.99"
			if qtype == D.TypeAAAA {
				addresses = []string{"2001:db8::1", "2001:db8::2"}
				unrelated = "unrelated.example. 60 IN AAAA 2001:db8::99"
			}
			client := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				answer := selectionTestAnswer(q, "cdn.example.", addresses...)
				answer.Answer = append([]D.RR{speedCheckTestRR(t, unrelated)}, answer.Answer...)
				return answer, nil
			}}
			r.main = []dnsClient{client}
			ctx := routingTestContext(newSelectionTestOutbound(), routingTestOrigin())
			var previous []netip.Addr
			for i := 0; i < 2; i++ {
				var got []netip.Addr
				var err error
				if qtype == D.TypeA {
					got, err = r.LookupIPv4(ctx, "ordered-lookup.example")
				} else {
					got, err = r.LookupIPv6(ctx, "ordered-lookup.example")
				}
				if err != nil || len(got) != 2 {
					t.Fatalf("ordered lookup used an unrelated Answer address: %v, %v", got, err)
				}
				for _, address := range got {
					if address.String() != addresses[0] && address.String() != addresses[1] {
						t.Fatalf("ordered lookup returned an unrelated owner: %v", got)
					}
				}
				if previous != nil && !reflect.DeepEqual(previous, got) {
					t.Fatalf("cached lookup lost preferred terminal order: %v, %v", previous, got)
				}
				previous = got
			}
			query := new(D.Msg).SetQuestion("ordered-lookup.example.", qtype)
			wire := routingTestExchange(t, r, ctx, query)
			if len(msgToIP(wire)) != 3 || wire.Answer[0].Header().Name != "unrelated.example." || client.count(0) != 1 {
				t.Fatalf("ordered extraction changed wire records or bypassed cache: %v, calls=%d", wire, client.count(0))
			}
		})
	}
}
