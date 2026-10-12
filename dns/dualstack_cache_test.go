package dns

import (
	"context"
	"net/netip"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

func dualStackCacheFixture(t *testing.T) (*Resolver, *selectionTestClient, context.Context) {
	t.Helper()
	routingTestEnable(t)
	config := selectionTestConfig()
	config.Fallback = nil
	config.CacheAlgorithm = "arc"
	config.DualStack = DualStackConfig{Enabled: true, Threshold: 10 * time.Millisecond}
	r := NewResolver(config).Resolver
	t.Cleanup(r.Close)
	client := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, query *D.Msg) (*D.Msg, error) {
		if query.Question[0].Qtype == D.TypeA {
			return selectionTestAnswer(query, "", "192.0.2.4", "192.0.2.5"), nil
		}
		return selectionTestAnswer(query, "", "2001:db8::6"), nil
	}}
	r.main = []dnsClient{client}
	leaf := newSelectionTestOutbound()
	leaf.delays[netip.MustParseAddr("2001:db8::6")] = 40 * time.Millisecond
	return r, client, routingTestContext(leaf, routingTestOrigin())
}

func dualStackCacheContext(t *testing.T, r *Resolver, ctx context.Context, query *D.Msg) (context.Context, string) {
	t.Helper()
	ctx = r.cacheControl.Context(ctx)
	ctx, local, err := r.prepareDNSRouting(ctx, query)
	if err != nil || local != nil {
		t.Fatalf("prepare route: %v %v", local, err)
	}
	return ctx, r.cacheKey(ctx, query)
}

func TestDualStackCachesPositiveFamiliesAndReusesEvidence(t *testing.T) {
	r, upstream, ctx := dualStackCacheFixture(t)
	query := new(D.Msg).SetQuestion("cached-family.example.", D.TypeAAAA)
	answer, err := r.ExchangeContext(ctx, query)
	if err != nil || len(answer.Answer) != 0 || len(answer.Ns) != 1 {
		t.Fatalf("expected measured family preference: %v %v", answer, err)
	}
	for _, qtype := range []uint16{D.TypeA, D.TypeAAAA} {
		q := new(D.Msg).SetQuestion(query.Question[0].Name, qtype)
		_, key := dualStackCacheContext(t, r, ctx, q)
		cached, expires, ok := r.cache.GetWithExpire(key)
		if !ok || len(msgToIP(cached)) == 0 || time.Until(expires) < 58*time.Second {
			t.Fatalf("family %d did not retain its positive full-TTL cache entry: %v %v", qtype, cached, expires)
		}
		got, err := r.ExchangeContext(ctx, q)
		want := 0
		if qtype == D.TypeA {
			want = 2
		}
		if err != nil || len(msgToIP(got)) != want {
			t.Fatalf("family cache reuse changed preference: %v %v", got, err)
		}
	}
	if upstream.count(D.TypeA) != 1 || upstream.count(D.TypeAAAA) != 1 {
		t.Fatalf("warm families queried upstream again: A=%d AAAA=%d", upstream.count(D.TypeA), upstream.count(D.TypeAAAA))
	}
	// An expired/evicted other-family answer cannot keep suppressing valid IPv6.
	other := new(D.Msg).SetQuestion(query.Question[0].Name, D.TypeA)
	_, otherKey := dualStackCacheContext(t, r, ctx, other)
	r.cache.Delete(otherKey)
	answer, err = r.ExchangeContext(ctx, query)
	if err != nil || len(msgToIP(answer)) != 1 || upstream.count(0) != 2 {
		t.Fatalf("missing opposite cache kept a stale preference or started network work: %v %v", answer, err)
	}
}

func TestDualStackBackgroundReusesCacheWithoutAuxiliaryQueries(t *testing.T) {
	r, upstream, ctx := dualStackCacheFixture(t)
	query := new(D.Msg).SetQuestion("background-family.example.", D.TypeAAAA)
	if _, err := r.ExchangeContext(ctx, query); err != nil {
		t.Fatal(err)
	}
	_, key := dualStackCacheContext(t, r, ctx, query)
	r.cache.Delete(key)
	r.speedChecker.candidates.Clear() // require a new upstream exchange, not one-second raw reuse
	background := context.WithValue(ctx, cacheBackgroundKey{}, true)
	if _, err := r.ExchangeContext(background, query); err != nil {
		t.Fatal(err)
	}
	if upstream.count(D.TypeA) != 1 || upstream.count(D.TypeAAAA) != 2 {
		t.Fatalf("background refresh created auxiliary DNS traffic: A=%d AAAA=%d", upstream.count(D.TypeA), upstream.count(D.TypeAAAA))
	}
	r.ClearCache()
	if _, err := r.ExchangeContext(background, query); err != nil {
		t.Fatal(err)
	}
	if upstream.count(D.TypeA) != 1 || upstream.count(D.TypeAAAA) != 3 {
		t.Fatalf("cold background refresh queried the other family: A=%d AAAA=%d", upstream.count(D.TypeA), upstream.count(D.TypeAAAA))
	}
}

func TestDualStackCacheInvalidatesUnmeasuredReplacementAndOldGeneration(t *testing.T) {
	r, upstream, ctx := dualStackCacheFixture(t)
	query := new(D.Msg).SetQuestion("replaced-family.example.", D.TypeAAAA)
	if _, err := r.ExchangeContext(ctx, query); err != nil {
		t.Fatal(err)
	}
	prepared, key := dualStackCacheContext(t, r, ctx, query)
	if !r.cacheControl.Store(prepared, key, query.Question[0], dualStackTestAnswer(query)) {
		t.Fatal("replacement was not accepted")
	}
	answer, err := r.ExchangeContext(ctx, query)
	if err != nil || len(msgToIP(answer)) != 1 || upstream.count(0) != 2 {
		t.Fatalf("old speed sample outlived an unmeasured replacement: %v %v", answer, err)
	}
	r.ClearCache()
	if r.cacheControl.Store(prepared, key, query.Question[0], dualStackTestAnswer(query)) {
		t.Fatal("old generation refilled cleared family cache")
	}
	r.cacheControl.mu.Lock()
	retained := r.cacheControl.dualStackSamples != nil
	r.cacheControl.mu.Unlock()
	if retained {
		t.Fatal("clear retained family measurements")
	}
}

func TestDualStackADQueryMayUseUnsignedAnswers(t *testing.T) {
	r, upstream, ctx := dualStackCacheFixture(t)
	query := new(D.Msg).SetQuestion("ad-capability.example.", D.TypeAAAA)
	query.AuthenticatedData = true
	answer, err := r.ExchangeContext(ctx, query)
	if err != nil || len(answer.Answer) != 0 || upstream.count(D.TypeA) != 1 || upstream.count(D.TypeAAAA) != 1 {
		t.Fatalf("query AD capability incorrectly disabled unsigned optimization: %v %v", answer, err)
	}
}

func TestDualStackCachedPreferenceMatchesTheReadSnapshot(t *testing.T) {
	r, _, ctx := dualStackCacheFixture(t)
	query := new(D.Msg).SetQuestion("cache-snapshot.example.", D.TypeAAAA)
	if _, err := r.ExchangeContext(ctx, query); err != nil {
		t.Fatal(err)
	}
	prepared, key := dualStackCacheContext(t, r, ctx, query)
	old, expires, _ := getMsgFromCache(r.cache, key)
	old.Id = query.Id
	// Another query finishes with a different measured IPv6 address after
	// this caller read the old answer but before it checks family preference.
	replacement := selectionTestAnswer(query, "", "2001:db8::7")
	prepared = r.withDualStackAnswers(prepared)
	recordDNSDualStackSelection(prepared, replacement, &dnsDualStackSelection{
		positive: replacement, pool: dnsDualStackPool(r.main),
		measurements: &dualStackMeasurements{rtts: map[netip.Addr]time.Duration{netip.MustParseAddr("2001:db8::7"): 50 * time.Millisecond}},
	})
	if !r.cacheControl.Store(prepared, key, query.Question[0], replacement) {
		t.Fatal("new sample was not stored")
	}
	if got := r.preferCachedDualStack(prepared, query, old, key, expires); len(msgToIP(got)) != 1 {
		t.Fatal("new address measurement suppressed a different cached snapshot")
	}
	old.AuthenticatedData = true
	if got := r.preferCachedDualStack(prepared, query, old, key, expires); got != old || !got.AuthenticatedData {
		t.Fatal("cache preference modified an authenticated reply")
	}
}
