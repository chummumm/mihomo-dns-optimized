package dns

import (
	"context"
	"reflect"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	D "github.com/miekg/dns"
)

func TestDNSAnswerLifetimeAgesAllOrdinaryRecordsAndPreservesOPT(t *testing.T) {
	query := routingTestQuery("lifetime.example", 1)
	answer := speedCheckTestReply(t, query,
		"lifetime.example. 10 IN CNAME address.example.",
		"address.example. 20 IN A 192.0.2.1",
		"address.example. 0 IN A 192.0.2.2")
	answer.Ns = []D.RR{speedCheckTestRR(t, "example. 30 IN NS ns.example.")}
	answer.Extra = []D.RR{speedCheckTestRR(t, "ns.example. 40 IN A 192.0.2.53")}
	answer.SetEdns0(1232, true)
	answer.IsEdns0().SetVersion(1)
	answer.IsEdns0().Option = []D.EDNS0{&D.EDNS0_COOKIE{Code: D.EDNS0COOKIE, Cookie: "0011223344556677"}}
	before := answer.Copy()
	start := time.Unix(1000, 0)
	aged := ageDNSAnswerAt(answer, start, start.Add(12500*time.Millisecond))
	want := []uint32{0, 8, 0, 18, 28}
	var got []uint32
	for _, records := range [][]D.RR{aged.Answer, aged.Ns, aged.Extra} {
		for _, rr := range records {
			if rr.Header().Rrtype != D.TypeOPT {
				got = append(got, rr.Header().Ttl)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordinary TTLs were not aged by one common elapsed interval: got %v want %v", got, want)
	}
	if !reflect.DeepEqual(aged.IsEdns0(), before.IsEdns0()) || !speedCheckTestEqual(answer, before) {
		t.Fatal("aging mutated EDNS state or the original upstream response")
	}
	if got := ageDNSAnswerAt(answer, start, start.Add(-time.Second)); got != answer {
		t.Fatal("a backwards clock changed the answer or extended its TTL")
	}
}

func TestDNSAnswerLifetimeTracksChosenCopyWithoutResettingReceipt(t *testing.T) {
	ctx := withDNSAnswerLifetimes(context.Background())
	query := routingTestQuery("lifetime.example", 1)
	first := cacheTestReply(query, 10)
	chosen := cacheTestReply(query, 10)
	start := time.Now()
	recordDNSAnswerReceived(ctx, first, start.Add(-5500*time.Millisecond))
	recordDNSAnswerReceived(ctx, chosen, start.Add(-2500*time.Millisecond))
	// The wrapper and result collector see the same message again later.
	// This handoff must retain the original receipt instead of granting 10s.
	recordDNSAnswerReceived(ctx, chosen, start)
	copy := chosen.Copy()
	inheritDNSAnswerLifetime(ctx, copy, chosen)
	aged := ageDNSAnswerAfterPolicy(ctx, copy, copy)
	if ttl := aged.Answer[0].Header().Ttl; ttl != 8 {
		t.Fatalf("chosen answer lost its own receipt or used another upstream's: %d", ttl)
	}
	if ttl := chosen.Answer[0].Header().Ttl; ttl != 10 {
		t.Fatal("aging the chosen copy modified its raw candidate")
	}
	unrelated := withDNSAnswerLifetimes(ctx)
	if got := ageDNSAnswerAfterPolicy(unrelated, copy, copy); got != copy {
		t.Fatal("a new resolver invocation inherited another exchange's lifetime table")
	}
}

func TestDNSAnswerLifetimeAppliesTTLPolicyBeforeElapsedTime(t *testing.T) {
	for _, test := range []struct {
		name   string
		ttl    uint32
		policy AnswerPolicy
		age    time.Duration
		want   uint32
	}{
		{"minimum-still-counts-wait", 1, AnswerPolicy{RRTTLMin: 10}, 2500 * time.Millisecond, 8},
		{"maximum-still-counts-wait", 60, AnswerPolicy{RRTTLMax: 10}, 2500 * time.Millisecond, 8},
		{"minimum-cannot-revive-exhausted-policy-lifetime", 1, AnswerPolicy{RRTTLMin: 3}, 3500 * time.Millisecond, 0},
		{"zero-remains-zero", 0, AnswerPolicy{RRTTLMin: 10}, 2500 * time.Millisecond, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := routingTestQuery("lifetime.example", 1)
			client := newCandidateCacheTestClient(func(ctx context.Context, q *D.Msg, _ int32) *D.Msg {
				answer := cacheTestReply(q, test.ttl)
				recordDNSAnswerReceived(ctx, answer, time.Now().Add(-test.age))
				return answer
			})
			r := NewResolverFromClient(client)
			r.answerPolicy = test.policy
			defer r.Close()
			answer, err := r.ExchangeContext(context.Background(), query)
			if err != nil || answer == nil {
				t.Fatalf("resolver exchange failed: %v, %v", answer, err)
			}
			if ttl := answer.Answer[0].Header().Ttl; ttl != test.want {
				t.Fatalf("policy reset the receipt lifetime: got TTL %d want %d", ttl, test.want)
			}
			key := r.cacheKey(context.Background(), query)
			_, _, cached := r.cache.GetWithExpire(key)
			if cached != (test.want > 0) {
				t.Fatalf("cacheability did not follow the aged policy lifetime: cached=%t ttl=%d", cached, test.want)
			}
		})
	}
}

func TestDNSAnswerLifetimeKeepsAbsoluteCacheExpiryAtSubsecondBoundaries(t *testing.T) {
	for _, algorithm := range []string{"lru", "arc"} {
		for _, ruleRouting := range []bool{false, true} {
			t.Run(algorithm+map[bool]string{false: "/legacy", true: "/native"}[ruleRouting], func(t *testing.T) {
				for _, test := range []struct {
					name   string
					policy AnswerPolicy
					reply  func(*D.Msg) *D.Msg
					ttl    uint32
				}{
					{"positive", AnswerPolicy{}, func(q *D.Msg) *D.Msg { return cacheTestReply(q, 1) }, 1},
					{"minimum", AnswerPolicy{RRTTLMin: 10}, func(q *D.Msg) *D.Msg { return cacheTestReply(q, 1) }, 10},
					{"maximum", AnswerPolicy{RRTTLMax: 10}, func(q *D.Msg) *D.Msg { return cacheTestReply(q, 60) }, 10},
					{"cname-minimum", AnswerPolicy{}, func(q *D.Msg) *D.Msg {
						return speedCheckTestReply(t, q, "lifetime.example. 1 IN CNAME address.example.", "address.example. 30 IN A 192.0.2.1")
					}, 1},
					{"negative-soa", AnswerPolicy{}, func(q *D.Msg) *D.Msg {
						answer := new(D.Msg)
						answer.SetRcode(q, D.RcodeNameError)
						answer.Ns = []D.RR{speedCheckTestRR(t, "example. 1 IN SOA ns.example. hostmaster.example. 1 30 30 30 1")}
						return answer
					}, 1},
					{"server-failure", AnswerPolicy{}, func(q *D.Msg) *D.Msg {
						return new(D.Msg).SetRcode(q, D.RcodeServerFailure)
					}, serverFailureCacheTTL},
				} {
					t.Run(test.name, func(t *testing.T) {
						query := routingTestQuery("lifetime.example", 1)
						r := &Resolver{ruleRouting: ruleRouting, cache: Config{RuleRouting: ruleRouting, CacheAlgorithm: algorithm}.newCache()}
						r.cacheControl = newCacheControl(r, &CacheOptions{ServeExpired: false})
						defer r.Close()
						ctx := withDNSAnswerLifetimes(r.cacheControl.Context(context.Background()))
						receivedAt := time.Now().Add(-800 * time.Millisecond)
						original := test.reply(query)
						recordDNSAnswerReceived(ctx, original, receivedAt)
						answer := ageDNSAnswerAfterPolicy(ctx, original, test.policy.apply(query, original))
						if ttl := dnsMessageCacheTTL(answer); ttl != test.ttl {
							t.Fatalf("subsecond aging changed whole-second wire TTL: got %d want %d", ttl, test.ttl)
						}
						const key = "absolute-expiry"
						if !r.cacheControl.Store(ctx, key, query.Question[0], answer) {
							t.Fatal("current generation could not cache a valid reply")
						}
						_, expires, hit := r.cache.GetWithExpire(key)
						want := receivedAt.Add(time.Duration(test.ttl) * time.Second)
						// LRU and ARC serialize deadlines as Unix seconds. Their
						// existing rounding may shorten, but must never extend,
						// the original receipt-based expiration.
						if !hit || expires.After(want) || expires.Before(want.Truncate(time.Second)) {
							t.Fatalf("cache restarted the fractional lifetime: got %s (hit %t), upper bound %s", expires, hit, want)
						}
						// Copying, re-registering, or reapplying a policy cannot move
						// this already-recorded deadline forward.
						copy := answer.Copy()
						inheritDNSAnswerLifetime(ctx, copy, answer)
						recordDNSAnswerReceived(ctx, copy, time.Now())
						copy = ageDNSAnswerAfterPolicy(ctx, copy, copy)
						if got := dnsAnswerExpires(ctx, copy); !got.Equal(want) {
							t.Fatalf("a repeated handoff extended the absolute expiry: got %s want %s", got, want)
						}
					})
				}
			})
		}
	}
}

func TestDNSAnswerLifetimeDoesNotAlterDNSSECSignatureMetadata(t *testing.T) {
	query := routingTestQuery("lifetime.example", 1)
	answer := cacheTestReply(query, 60)
	signature := &D.RRSIG{
		Hdr:         D.RR_Header{Name: query.Question[0].Name, Rrtype: D.TypeRRSIG, Class: D.ClassINET, Ttl: 60},
		TypeCovered: D.TypeA, Algorithm: D.RSASHA256, Labels: 2, OrigTtl: 60,
		Expiration: 2000000060, Inception: 2000000000, KeyTag: 1234,
		SignerName: "example.", Signature: "AA==",
	}
	answer.Answer = append(answer.Answer, signature)
	answer.AuthenticatedData = true
	ctx := withDNSAnswerLifetimes(context.Background())
	recordDNSAnswerReceived(ctx, answer, time.Now().Add(-2500*time.Millisecond))
	policy := AnswerPolicy{RRTTLMin: 600}
	aged := ageDNSAnswerAfterPolicy(ctx, answer, policy.apply(query, answer))
	got := aged.Answer[1].(*D.RRSIG)
	if got.Hdr.Ttl != 58 || !aged.AuthenticatedData || signature.Hdr.Ttl != 60 {
		t.Fatal("DNSSEC answer lost its original lifetime, validation flag, or copy ownership")
	}
	// DNSSEC uses OrigTtl while verifying a signature. Only the ordinary RR
	// header TTL ages; signature lifetime, signer, and bytes remain intact.
	copy := *got
	copy.Hdr.Ttl = signature.Hdr.Ttl
	if !reflect.DeepEqual(&copy, signature) {
		t.Fatal("aging changed signed DNSSEC metadata")
	}
}

func TestDNSAnswerLifetimePreservesWholeMessageSignatures(t *testing.T) {
	for _, signature := range []D.RR{
		&D.TSIG{Hdr: D.RR_Header{Name: "key.example.", Rrtype: D.TypeTSIG, Class: D.ClassANY}, Algorithm: D.HmacSHA256, TimeSigned: 2000000000, Fudge: 300},
		&D.SIG{RRSIG: D.RRSIG{Hdr: D.RR_Header{Name: ".", Rrtype: D.TypeSIG, Class: D.ClassANY}, TypeCovered: 0, Algorithm: D.RSASHA256, SignerName: "key.example.", Signature: "AA=="}},
	} {
		t.Run(D.TypeToString[signature.Header().Rrtype], func(t *testing.T) {
			query := routingTestQuery("lifetime.example", 1)
			answer := cacheTestReply(query, 60)
			answer.Extra = []D.RR{signature}
			before := answer.Copy()
			receivedAt := time.Unix(1000, 0)
			aged := ageDNSAnswerAt(answer, receivedAt, receivedAt.Add(2500*time.Millisecond))
			if !speedCheckTestEqual(aged, before) {
				t.Fatal("aging rewrote fields authenticated by a whole-message signature")
			}
		})
	}
}

func TestDNSAnswerLifetimeExpiredBeforeStoreDeletesOlderAnswer(t *testing.T) {
	query := routingTestQuery("lifetime.example", 1)
	cache := Config{}.newCache()
	const key = "expired-before-store"
	putMsgToCache(cache, key, query.Question[0], cacheTestReply(query, 60))
	// The reply still has a positive whole-second TTL, but its absolute
	// deadline passed between selection and the generation-aware store.
	putMsgToCacheWithExpiry(cache, key, query.Question[0], cacheTestReply(query, 1), time.Now().Add(-time.Millisecond))
	if _, _, hit := cache.GetWithExpire(key); hit {
		t.Fatal("an exhausted fresh reply retained or replaced an older cache entry")
	}
}

func TestDNSCandidateLifetimeIsAgedOnceFromOriginalReceipt(t *testing.T) {
	ctx, checker := candidateCacheTestScope(t)
	ctx = withDNSAnswerLifetimes(ctx)
	query := routingTestQuery("cache.example", 1)
	receivedAt := time.Now().Add(-500 * time.Millisecond)
	client := newCandidateCacheTestClient(func(ctx context.Context, q *D.Msg, _ int32) *D.Msg {
		answer := cacheTestReply(q, 60)
		recordDNSAnswerReceived(ctx, answer, receivedAt)
		return answer
	})
	first := candidateCacheTestExchange(t, ctx, checker, []dnsClient{client}, query)
	query.Id++
	second := candidateCacheTestExchange(t, ctx, checker, []dnsClient{client}, query)
	if client.calls.Load() != 1 || second.Answer[0].Header().Ttl != 60 {
		t.Fatal("raw candidate reuse prematurely aged the TTL or lost the cache hit")
	}
	lifetimes := ctx.Value(dnsAnswerLifetimesKey{}).(*dnsAnswerLifetimes)
	lifetimes.mu.Lock()
	firstAt, secondAt := lifetimes.received[first], lifetimes.received[second]
	lifetimes.mu.Unlock()
	if !firstAt.Equal(receivedAt) || !secondAt.Equal(receivedAt) {
		t.Fatal("candidate reuse reset the original upstream receipt")
	}
	aged := ageDNSAnswerAt(second, secondAt, receivedAt.Add(2500*time.Millisecond))
	if ttl := aged.Answer[0].Header().Ttl; ttl != 58 {
		t.Fatalf("candidate lifetime was counted twice: %d", ttl)
	}
}

type answerLifetimeTestOutbound struct {
	*routingTestOutbound
	delay time.Duration
}

func (a *answerLifetimeTestOutbound) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	timer := time.NewTimer(a.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return a.routingTestOutbound.DialContext(ctx, metadata)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDNSResolverDoesNotRestartTTLAfterActualSpeedWait(t *testing.T) {
	for _, algorithm := range []string{"lru", "arc"} {
		t.Run(algorithm, func(t *testing.T) {
			routingTestEnable(t)
			r := NewResolver(Config{
				RuleRouting: true, Main: []NameServer{{Addr: "192.0.2.53:53"}},
				CacheAlgorithm: algorithm, CacheOptions: &CacheOptions{ServeExpired: false},
				SpeedCheck: SpeedCheckConfig{Mode: []string{"tcp:443"}, Timeout: 2 * time.Second, Concurrency: 1},
			}).Resolver
			defer r.Close()
			client := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
				return cacheTestReply(q, 1)
			})
			r.main = []dnsClient{client}
			leaf := &answerLifetimeTestOutbound{routingTestOutbound: newRoutingTestOutbound("lifetime-" + algorithm), delay: 1100 * time.Millisecond}
			ctx := routingTestContext(leaf, routingTestOrigin())
			query := routingTestQuery("lifetime.example", 1)
			answer := routingTestExchange(t, r, ctx, query)
			if ttl := answer.Answer[0].Header().Ttl; ttl != 0 {
				t.Fatalf("actual probe wait granted a new TTL: got %d, want 0", ttl)
			}
			prepared, _, err := r.prepareDNSRouting(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, hit := r.cache.GetWithExpire(r.cacheKey(prepared, query)); hit {
				t.Fatal("a response exhausted during speed checking entered the fresh cache")
			}
			query.Id++
			if second := routingTestExchange(t, r, ctx, query); second.Rcode != D.RcodeSuccess || client.calls.Load() != 2 {
				t.Fatalf("next transaction reused expired reply instead of querying upstream: calls=%d answer=%v", client.calls.Load(), second)
			}
			if leaf.tcp.Load() == 0 {
				t.Fatalf("test did not exercise %s speed checking", algorithm)
			}
		})
	}
}
