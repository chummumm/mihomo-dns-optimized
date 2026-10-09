package dns

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

type candidateCacheTestClient struct {
	dnsClient
	calls atomic.Int32
	reply func(context.Context, *D.Msg, int32) *D.Msg
}

func newCandidateCacheTestClient(reply func(context.Context, *D.Msg, int32) *D.Msg) *candidateCacheTestClient {
	return &candidateCacheTestClient{
		dnsClient: newClient("192.0.2.53:53", nil, "udp", nil, nil, ""),
		reply:     reply,
	}
}

func (c *candidateCacheTestClient) Unwrap() dnsClient { return c.dnsClient }
func (c *candidateCacheTestClient) ExchangeContext(ctx context.Context, query *D.Msg) (*D.Msg, error) {
	return c.reply(ctx, query, c.calls.Add(1)), nil
}

func candidateCacheTestScope(t *testing.T) (context.Context, *directSpeedChecker) {
	t.Helper()
	routingTestEnable(t)
	r := routingTestResolver()
	t.Cleanup(r.Close)
	ctx, _, err := r.prepareDNSRouting(routingTestContext(newRoutingTestOutbound("candidate-cache"), routingTestOrigin()), routingTestQuery("cache.example", 1))
	if err != nil {
		t.Fatal(err)
	}
	return ctx, speedCheckTestChecker(t, time.Second, 2)
}

func candidateCacheTestExchange(t *testing.T, ctx context.Context, checker *directSpeedChecker, clients []dnsClient, query *D.Msg) *D.Msg {
	t.Helper()
	wrapped := checker.shareCandidateClients(ctx, clients)
	answer, err := wrapped[0].ExchangeContext(ctx, query)
	if err != nil || answer == nil {
		t.Fatalf("candidate exchange failed: %v, %v", answer, err)
	}
	if answer.Id != query.Id {
		t.Fatalf("candidate returned transaction ID %d for %d", answer.Id, query.Id)
	}
	return answer
}

func TestDNSCandidateCacheSeparatesUpstreamInstancesAndSelectedSets(t *testing.T) {
	ctx, checker := candidateCacheTestScope(t)
	query := routingTestQuery("cache.example", 1)
	main := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
		return speedCheckTestReply(t, q, "cache.example. 60 IN A 192.0.2.1")
	})
	fallback := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
		return speedCheckTestReply(t, q, "cache.example. 60 IN A 192.0.2.2")
	})
	// Both clients deliberately report the same address. Their actual configured
	// instances and their selected lists, rather than list index, own replies.
	for _, test := range []struct {
		clients []dnsClient
		want    string
	}{
		{[]dnsClient{main}, "192.0.2.1"},
		{[]dnsClient{fallback}, "192.0.2.2"},
		{[]dnsClient{main}, "192.0.2.1"},
		{[]dnsClient{fallback}, "192.0.2.2"},
	} {
		query.Id++
		answer := candidateCacheTestExchange(t, ctx, checker, test.clients, query)
		if ips := msgToIP(answer); len(ips) != 1 || ips[0].String() != test.want {
			t.Fatalf("upstream identity collision: got %v, want %s", ips, test.want)
		}
	}
	if main.calls.Load() != 1 || fallback.calls.Load() != 1 {
		t.Fatalf("ordinary repeated candidates did not reuse their own replies: main=%d fallback=%d", main.calls.Load(), fallback.calls.Load())
	}
	candidateCacheTestExchange(t, ctx, checker, []dnsClient{main, fallback}, query)
	if main.calls.Load() != 2 {
		t.Fatal("a different selected upstream set borrowed the first set's cache entry")
	}
}

func TestDNSCandidateCacheIncludesECSAndDisableTypeWrappers(t *testing.T) {
	ctx, checker := candidateCacheTestScope(t)
	query := routingTestQuery("cache.example", 1)
	raw := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
		ip := "192.0.2.1"
		if opt := q.IsEdns0(); opt != nil {
			for _, option := range opt.Option {
				if ecs, ok := option.(*D.EDNS0_SUBNET); ok && ecs.Address.String() == "198.51.100.0" {
					ip = "192.0.2.2"
				}
			}
		}
		return speedCheckTestReply(t, q, "cache.example. 60 IN A "+ip)
	})
	first := clientWithEdns0Subnet{dnsClient: raw, ecsPrefix: netip.MustParsePrefix("192.0.2.0/24")}
	second := clientWithEdns0Subnet{dnsClient: raw, ecsPrefix: netip.MustParsePrefix("198.51.100.0/24")}
	for _, test := range []struct {
		client dnsClient
		want   string
	}{{first, "192.0.2.1"}, {second, "192.0.2.2"}, {first, "192.0.2.1"}} {
		answer := candidateCacheTestExchange(t, ctx, checker, []dnsClient{test.client}, query)
		if ips := msgToIP(answer); len(ips) != 1 || ips[0].String() != test.want {
			t.Fatalf("ECS wrapper cache collision: got %v, want %s", ips, test.want)
		}
	}
	if raw.calls.Load() != 2 {
		t.Fatalf("ECS wrappers lost valid reuse: %d exchanges", raw.calls.Load())
	}
	overridden := first
	overridden.ecsOverride = true
	firstID, _ := dnsCandidateServerIdentity(first)
	overrideID, _ := dnsCandidateServerIdentity(overridden)
	if firstID == overrideID {
		t.Fatal("ECS override semantics are missing from the identity")
	}

	raw = newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
		return speedCheckTestReply(t, q, "cache.example. 60 IN A 192.0.2.1", "cache.example. 60 IN AAAA 2001:db8::1")
	})
	for _, test := range []struct {
		disabled uint16
		want     int
	}{{D.TypeAAAA, 1}, {D.TypeHTTPS, 2}, {D.TypeAAAA, 1}} {
		client := clientWithDisableTypes{dnsClient: raw, disableTypes: map[uint16]struct{}{test.disabled: {}}}
		answer := candidateCacheTestExchange(t, ctx, checker, []dnsClient{client}, query)
		if len(answer.Answer) != test.want {
			t.Fatalf("disable-type wrapper cache collision: got %d answers, want %d", len(answer.Answer), test.want)
		}
	}
	if raw.calls.Load() != 2 {
		t.Fatalf("disable-type wrappers lost valid reuse: %d exchanges", raw.calls.Load())
	}
}

type candidateCacheValueClient struct {
	dnsClient
	properties map[string]string
}

func (c candidateCacheValueClient) Unwrap() dnsClient { return c.dnsClient }

func TestDNSCandidateCacheUnknownValueClientDoesNotPanicOrShare(t *testing.T) {
	ctx, checker := candidateCacheTestScope(t)
	client := candidateCacheValueClient{
		dnsClient:  newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg { return cacheTestReply(q, 60) }),
		properties: map[string]string{"scope": "unproven"},
	}
	if _, ok := dnsCandidateServerIdentity(client); ok {
		t.Fatal("an uncomparable value client's identity was guessed")
	}
	if _, ok := checker.shareCandidateClients(ctx, []dnsClient{client})[0].(*dnsCandidateClient); ok {
		t.Fatal("unproven identity enabled candidate reuse")
	}
	candidateCacheTestExchange(t, ctx, checker, []dnsClient{client}, routingTestQuery("cache.example", 1))
}

func TestDNSCandidateCacheDoesNotReplayEDNSTransactionState(t *testing.T) {
	for _, queryOPT := range []bool{false, true} {
		t.Run(fmt.Sprintf("query-opt-%t", queryOPT), func(t *testing.T) {
			ctx, checker := candidateCacheTestScope(t)
			query := routingTestQuery("cache.example", 1)
			if queryOPT {
				query.SetEdns0(1232, false)
			}
			client := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, call int32) *D.Msg {
				answer := cacheTestReply(q, 1)
				answer.SetEdns0(1232, false)
				opt := answer.IsEdns0()
				opt.SetDo()
				opt.SetVersion(1)
				opt.Option = []D.EDNS0{&D.EDNS0_COOKIE{Code: D.EDNS0COOKIE, Cookie: fmt.Sprintf("0011223344556677%016x", call)}}
				return answer
			})
			var previous string
			for i := 0; i < 2; i++ {
				query.Id++
				answer := candidateCacheTestExchange(t, ctx, checker, []dnsClient{client}, query)
				opt := answer.IsEdns0()
				if opt == nil || !opt.Do() || opt.Version() != 1 || opt.UDPSize() != 1232 {
					t.Fatalf("live EDNS control fields changed: %v", opt)
				}
				cookie := opt.Option[0].(*D.EDNS0_COOKIE).Cookie
				if cookie == previous {
					t.Fatal("replayed the previous upstream transaction's server cookie")
				}
				previous = cookie
			}
			if client.calls.Load() != 2 {
				t.Fatal("EDNS response entered the optional candidate cache")
			}
		})
	}
	cache := newDNSCandidateCache()
	query := routingTestQuery("cache.example", 1)
	query.SetEdns0(1232, false)
	cache.store("query-only-opt", 0, query, cacheTestReply(query, 60))
	if answer, _ := cache.get("query-only-opt"); answer != nil {
		t.Fatal("a transaction using EDNS was cached even when its reply omitted OPT")
	}
}

func TestDNSCachedTTLUpdatesNeverTouchOPT(t *testing.T) {
	for _, control := range []uint32{0, 0x8000, 0x00010000, 0x01000000, 0x01018040} {
		for _, remaining := range []uint32{0, 9, 20} {
			t.Run(fmt.Sprintf("opt-%08x/remaining-%d", control, remaining), func(t *testing.T) {
				opt := &D.OPT{Hdr: D.RR_Header{Name: ".", Rrtype: D.TypeOPT, Class: 1232, Ttl: control},
					Option: []D.EDNS0{&D.EDNS0_COOKIE{Code: D.EDNS0COOKIE, Cookie: "0011223344556677"}}}
				before := D.Copy(opt)
				record := speedCheckTestRR(t, "cache.example. 10 IN A 192.0.2.1")
				records := []D.RR{opt, record}
				if ttl := minimalTTL(records); ttl != 10 {
					t.Fatalf("OPT participated in minimum TTL: %d", ttl)
				}
				updateTTL(records, remaining)
				if !reflect.DeepEqual(opt, before) {
					t.Fatalf("OPT was aged as a normal RR: before=%v after=%v", before, opt)
				}
				want := remaining
				if want == 0 {
					want = 1 // existing positive fresh-cache floor
				}
				if want > 10 {
					want = 10
				}
				if record.Header().Ttl != want {
					t.Fatalf("ordinary RR TTL got %d, want %d", record.Header().Ttl, want)
				}
				updateTTL([]D.RR{opt}, 0)
				if !reflect.DeepEqual(opt, before) {
					t.Fatal("OPT-only section changed at zero remaining TTL")
				}
			})
		}
	}
}
