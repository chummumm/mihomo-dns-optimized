package dns

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	RC "github.com/metacubex/mihomo/rules/common"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

func cachePrefetchTestControl(t *testing.T, options *CacheOptions) (*Resolver, *cacheControl, *routingTestOutbound) {
	t.Helper()
	routingTestEnable(t)
	oldProxies, oldProviders := tunnel.Proxies(), tunnel.Providers()
	oldRules, oldRuleProviders := tunnel.Rules(), tunnel.RuleProviders()
	t.Cleanup(func() {
		tunnel.UpdateProxies(oldProxies, oldProviders)
		tunnel.UpdateRules(oldRules, nil, oldRuleProviders)
	})
	direct := newRoutingTestOutbound("actual-direct")
	tunnel.UpdateProxies(map[string]C.Proxy{"DIRECT": routingTestProxy(direct)}, nil)
	tunnel.UpdateRules([]C.Rule{RC.NewMatch("DIRECT")}, nil, nil)
	r, c := cacheTestControl(t, options)
	r.ruleRouting = true
	r.main = transform([]NameServer{{Addr: "192.0.2.53:53"}}, nil)
	return r, c, direct
}

func cachePrefetchTestClock(c *cacheControl) *atomic.Int64 {
	clock := new(atomic.Int64)
	clock.Store(time.Now().Truncate(time.Second).Add(2 * time.Second).UnixNano())
	c.now = func() time.Time { return time.Unix(0, clock.Load()) }
	return clock
}

func cachePrefetchTestPrepare(t *testing.T, c *cacheControl, query *D.Msg, origin *C.Metadata) (context.Context, string) {
	t.Helper()
	ctx := c.Context(icontext.WithDNSRoutingInbound(icontext.WithDNSRoutingMetadata(context.Background(), origin)))
	ctx, answer, err := c.r.prepareDNSRouting(ctx, query)
	if err != nil || answer != nil {
		t.Fatalf("prepare prefetch fixture: answer=%v err=%v", answer, err)
	}
	return ctx, c.r.cacheKey(ctx, query)
}

// The simulated scheduler advances hours while this test takes milliseconds.
// Supply an exact answer expiration through the production receipt-lifetime
// channel; a generous wire TTL prevents the wall clock from shortening it.
func cachePrefetchTestStore(t *testing.T, c *cacheControl, ctx context.Context, query *D.Msg, ttl time.Duration) *D.Msg {
	t.Helper()
	expires := c.now().Add(ttl)
	wireTTL := uint32(time.Until(expires)/time.Second) + 2
	answer := cacheTestReply(query, wireTTL)
	ctx = withDNSAnswerLifetimes(ctx)
	lifetimes := ctx.Value(dnsAnswerLifetimesKey{}).(*dnsAnswerLifetimes)
	lifetimes.expires = map[*D.Msg]time.Time{answer: expires}
	if !c.Store(ctx, c.r.cacheKey(ctx, query), query.Question[0], answer) {
		t.Error("current generation could not store its prefetch fixture")
	}
	return answer
}

func cachePrefetchTestEntry(t *testing.T, c *cacheControl, key string) cacheHotEntry {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.hot[key]
	if entry == nil {
		t.Fatal("successful DIRECT demand did not create a prefetch candidate")
	}
	return *entry
}

func TestCacheControlPrefetchSingleSuccessfulQueryAcrossTTL(t *testing.T) {
	for _, ttl := range []time.Duration{300 * time.Second, time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			_, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true})
			clock := cachePrefetchTestClock(c)
			base := c.now()
			query := routingTestQuery("ttl-client.example", 40)
			ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
			ctx = c.Observe(ctx, query, key)
			if len(c.hot) != 0 || c.scheduler != nil {
				t.Fatal("a miss created a pending global entry before any successful reply")
			}
			cachePrefetchTestStore(t, c, ctx, query, ttl)
			entry := cachePrefetchTestEntry(t, c, key)
			wantDelay := ttl / 2
			if entry.heat != 3 || entry.next.Sub(base) != wantDelay {
				t.Fatalf("first real success did not grant finite TTL-based prefetch: heat=%d next=%s", entry.heat, entry.next.Sub(base))
			}
			var calls atomic.Int32
			c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
				calls.Add(1)
				return cachePrefetchTestStore(t, c, ctx, request, ttl), nil
			}
			// A client that respects its TTL is silent for much longer than one
			// minute. Its one request must still reach the scheduled refresh.
			clock.Store(base.Add(time.Minute).UnixNano())
			c.tick(c.now())
			if calls.Load() != 0 || len(c.hot) != 1 {
				t.Fatal("the old one-minute hotness window is still active")
			}
			for attempt := int32(1); attempt <= 2; attempt++ {
				entry = cachePrefetchTestEntry(t, c, key)
				clock.Store(entry.next.Add(-time.Nanosecond).UnixNano())
				c.tick(c.now())
				if calls.Load() != attempt-1 {
					t.Fatal("prefetch ran before the current cache deadline")
				}
				clock.Store(entry.next.UnixNano())
				c.tick(c.now())
				cacheWaitIdle(t, c)
				if calls.Load() != attempt {
					t.Fatalf("prefetch attempts=%d, want %d", calls.Load(), attempt)
				}
				if attempt == 1 {
					next := cachePrefetchTestEntry(t, c, key)
					if next.heat != 2 || next.next.Sub(c.now()) != wantDelay {
						t.Fatalf("background work added heat or ignored its new TTL: %+v", next)
					}
				}
			}
			clock.Store(c.now().Add(10 * ttl).UnixNano())
			c.tick(c.now())
			if calls.Load() != 2 || len(c.hot) != 0 || c.hotBytes != 0 || len(c.ready) != 0 {
				t.Fatal("background work indefinitely kept an unused domain hot")
			}
		})
	}
}

func TestCacheControlPrefetchHitKeepsDeadlineAndFiniteHeat(t *testing.T) {
	_, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true})
	clock := cachePrefetchTestClock(c)
	query := routingTestQuery("frequent-client.example", 41)
	ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
	ctx = c.Observe(ctx, query, key)
	cachePrefetchTestStore(t, c, ctx, query, 300*time.Second)
	initial := cachePrefetchTestEntry(t, c, key)
	clock.Store(c.now().Add(100 * time.Second).UnixNano())
	for i := 0; i < 100; i++ {
		c.Observe(ctx, query, key)
	}
	entry := cachePrefetchTestEntry(t, c, key)
	if !entry.next.Equal(initial.next) || entry.heat != 3 || entry.demand != 101 {
		t.Fatalf("repeated hits postponed the deadline or accumulated unbounded credit: heat=%d demand=%d next=%v", entry.heat, entry.demand, entry.next)
	}
	background := context.WithValue(ctx, cacheBackgroundKey{}, true)
	c.Observe(background, query, key)
	if got := cachePrefetchTestEntry(t, c, key); got.heat != entry.heat || got.demand != entry.demand {
		t.Fatal("background work counted as new client demand")
	}
}

func TestCacheControlPrefetchOnlyCacheableSuccessActivates(t *testing.T) {
	_, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true})
	for _, outcome := range []string{"failed", "no-message", "zero-ttl", "nxdomain", "servfail"} {
		t.Run(outcome, func(t *testing.T) {
			query := routingTestQuery(outcome+".example", 42)
			ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
			ctx = c.Observe(ctx, query, key)
			switch outcome {
			case "no-message":
				c.Store(ctx, key, query.Question[0], nil)
			case "zero-ttl":
				c.Store(ctx, key, query.Question[0], cacheTestReply(query, 0))
			case "nxdomain", "servfail":
				answer := cacheTestReply(query, 60)
				answer.Rcode = D.RcodeNameError
				if outcome == "servfail" {
					answer.Rcode = D.RcodeServerFailure
				}
				c.Store(ctx, key, query.Question[0], answer)
			}
			if len(c.hot) != 0 || len(c.ready) != 0 || c.hotBytes != 0 || c.scheduler != nil {
				t.Fatal("failed/noncacheable foreground work retained an unscheduled pending entry")
			}
		})
	}
}

func TestCacheControlPrefetchFreshTimingIgnoresExpiredOptions(t *testing.T) {
	for _, test := range []struct {
		name    string
		options CacheOptions
		ttl     time.Duration
		first   time.Duration
		next    time.Duration
	}{
		{"stale-disabled", CacheOptions{Prefetch: true}, 300 * time.Second, 150 * time.Second, 150 * time.Second},
		{"stale-disabled-with-retention", CacheOptions{Prefetch: true, ServeExpiredTTL: 24 * time.Hour}, 300 * time.Second, 150 * time.Second, 150 * time.Second},
		{"short-ttl", CacheOptions{Prefetch: true}, 4 * time.Second, 2 * time.Second, 2 * time.Second},
		{"minimum-period", CacheOptions{Prefetch: true, ServeExpired: true}, time.Second, time.Second, time.Second},
		{"stale-unlimited", CacheOptions{Prefetch: true, ServeExpired: true}, 300 * time.Second, 150 * time.Second, 150 * time.Second},
		{"stale-four-hours", CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredTTL: 4 * time.Hour}, time.Hour, 30 * time.Minute, 30 * time.Minute},
		{"stale-one-minute", CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredTTL: time.Minute}, 300 * time.Second, 150 * time.Second, 150 * time.Second},
		{"expired-period-shorter", CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredPrefetchTime: time.Second}, 300 * time.Second, 150 * time.Second, 150 * time.Second},
		{"expired-period-longer", CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredPrefetchTime: 24 * time.Hour}, 300 * time.Second, 150 * time.Second, 150 * time.Second},
		{"unused-expired-period", CacheOptions{Prefetch: true, ServeExpiredPrefetchTime: time.Minute}, 300 * time.Second, 150 * time.Second, 150 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, c, _ := cachePrefetchTestControl(t, &test.options)
			clock := cachePrefetchTestClock(c)
			query := routingTestQuery("timing.example", 43)
			ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
			ctx = c.Observe(ctx, query, key)
			cachePrefetchTestStore(t, c, ctx, query, test.ttl)
			entry := cachePrefetchTestEntry(t, c, key)
			if got := entry.next.Sub(c.now()); got != test.first {
				t.Fatalf("first delay=%s, want %s", got, test.first)
			}
			var calls atomic.Int32
			c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
				calls.Add(1)
				return cachePrefetchTestStore(t, c, ctx, request, test.ttl), nil
			}
			clock.Store(entry.next.UnixNano())
			c.tick(c.now())
			cacheWaitIdle(t, c)
			if calls.Load() != 1 {
				t.Fatal("due prefetch did not run")
			}
			entry = cachePrefetchTestEntry(t, c, key)
			if got := entry.next.Sub(c.now()); got != test.next {
				t.Fatalf("subsequent delay=%s, want %s", got, test.next)
			}
		})
	}
}

func TestCacheControlPrefetchLargeTTLDoesNotRefreshImmediately(t *testing.T) {
	r, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true})
	clock := cachePrefetchTestClock(c)
	query := routingTestQuery("long-ttl.example", 44)
	ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
	ttl := time.Duration(uint64(math.MaxUint32)) * time.Second
	r.cache.SetWithExpire(key, cacheTestReply(query, math.MaxUint32), c.now().Add(ttl))
	c.Observe(ctx, query, key)
	entry := cachePrefetchTestEntry(t, c, key)
	if entry.next.Sub(c.now()) != ttl/2 {
		t.Fatal("valid large DNS TTL overflowed its refresh deadline")
	}
	var calls atomic.Int32
	c.fetch = func(context.Context, *D.Msg) (*D.Msg, error) {
		calls.Add(1)
		return nil, nil
	}
	clock.Store(c.now().Add(time.Minute).UnixNano())
	c.tick(c.now())
	if calls.Load() != 0 || len(c.hot) != 1 {
		t.Fatal("a long TTL triggered immediate work or lost its TTL-based eligibility")
	}
}

func TestCacheControlPrefetchFailureBackoffDoesNotRenewRetention(t *testing.T) {
	r, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredTTL: time.Minute, ServeExpiredPrefetchTime: 24 * time.Hour})
	clock := cachePrefetchTestClock(c)
	query := routingTestQuery("unavailable.example", 45)
	ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
	ctx = c.Observe(ctx, query, key)
	cachePrefetchTestStore(t, c, ctx, query, 300*time.Second)
	_, originalExpiry, _ := r.cache.GetWithExpire(key)
	var calls atomic.Int32
	c.fetch = func(context.Context, *D.Msg) (*D.Msg, error) {
		calls.Add(1)
		return nil, errors.New("upstream unavailable")
	}
	entry := cachePrefetchTestEntry(t, c, key)
	// A delayed scheduler finds this fresh-origin task after expiration. Its
	// failed attempt must keep the short retry rather than wait the stale period.
	clock.Store(entry.expires.Add(time.Second).UnixNano())
	c.tick(c.now())
	cacheWaitIdle(t, c)
	entry = cachePrefetchTestEntry(t, c, key)
	if delay := entry.next.Sub(c.now()); delay < 5*time.Second || delay >= 6*time.Second || entry.heat != 2 {
		t.Fatalf("failed refresh did not receive finite, jittered retry: next=%s heat=%d", delay, entry.heat)
	}
	c.tick(c.now().Add(time.Second))
	if calls.Load() != 1 {
		t.Fatal("failed upstream created an immediate retry loop")
	}
	clock.Store(entry.next.UnixNano())
	c.tick(c.now())
	cacheWaitIdle(t, c)
	_, actualExpiry, exists := r.cache.GetWithExpire(key)
	if calls.Load() != 2 || len(c.hot) != 0 || !exists || !actualExpiry.Equal(originalExpiry) {
		t.Fatal("failed background work self-renewed heat or extended cached validity")
	}
	if valid, _ := c.ServeStale(actualExpiry, originalExpiry.Add(time.Minute+time.Nanosecond)); valid {
		t.Fatal("failed refresh extended stale-answer retention")
	}
}

func TestCacheControlPrefetchSnapshotsAndSourcesStaySeparate(t *testing.T) {
	_, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true})
	cachePrefetchTestClock(c)
	query := routingTestQuery("same-domain.example", 46)
	first, second := routingTestOrigin(), routingTestOrigin()
	second.SrcIP = netip.MustParseAddr("192.0.2.11")
	firstCtx, firstKey := cachePrefetchTestPrepare(t, c, query, first)
	secondCtx, secondKey := cachePrefetchTestPrepare(t, c, query, second)
	if firstKey == secondKey {
		t.Fatal("source-specific cache scopes were merged")
	}
	type unrelatedRequestState struct{}
	firstCtx = context.WithValue(firstCtx, unrelatedRequestState{}, make([]byte, 1<<20))
	firstCtx = c.Observe(firstCtx, query, firstKey)
	cachePrefetchTestStore(t, c, firstCtx, query, 300*time.Second)
	secondCtx = c.Observe(secondCtx, query, secondKey)
	cachePrefetchTestStore(t, c, secondCtx, query, 300*time.Second)
	if len(c.hot) != 2 {
		t.Fatal("distinct real sources did not get independent finite candidates")
	}
	entry := cachePrefetchTestEntry(t, c, firstKey)
	snapshotCtx := entry.request.context(c)
	md := icontext.DNSRoutingMetadata(snapshotCtx)
	if snapshotCtx.Value(unrelatedRequestState{}) != nil || !icontext.DNSProcessSnapshot(snapshotCtx) || md == nil || md.SrcIP != first.SrcIP || md.SrcPort != first.SrcPort || md.Process != first.Process {
		t.Fatal("candidate retained its parent request or lost its source/process snapshot")
	}
}

func TestCacheControlPrefetchBudgetScalesBeyondFixed1024(t *testing.T) {
	r, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true})
	r.cache = Config{CacheMaxSize: 384000}.newCache()
	cachePrefetchTestClock(c)
	firstKey := ""
	for i := 0; i < 18000; i++ {
		query := routingTestQuery(fmt.Sprintf("budget-%d.example", i), 47)
		ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
		if i == 0 {
			firstKey = key
		}
		ctx = c.Observe(ctx, query, key)
		cachePrefetchTestStore(t, c, ctx, query, time.Hour)
	}
	if len(c.hot) < 10000 || c.hotBytes > cachePrefetchBudget || c.hotBytes <= cachePrefetchBudget-cachePrefetchEntryLimit || len(c.ready) != len(c.hot) || c.lru.Len() != len(c.hot) {
		t.Fatalf("candidate budget did not replace the fixed 1024 limit: count=%d bytes=%d", len(c.hot), c.hotBytes)
	}
	if c.hot[firstKey] != nil {
		t.Fatal("budget pressure did not evict the oldest unrequested candidate")
	}
	query := routingTestQuery("oversized-source.example", 48)
	origin := routingTestOrigin()
	origin.ProcessPath = strings.Repeat("x", cachePrefetchEntryLimit)
	ctx, key := cachePrefetchTestPrepare(t, c, query, origin)
	ctx = c.Observe(ctx, query, key)
	cachePrefetchTestStore(t, c, ctx, query, time.Hour)
	if c.hot[key] != nil {
		t.Fatal("an oversized source snapshot exceeded the per-entry budget")
	}
	c.Clear()
	if c.hotBytes != 0 || len(c.hot) != 0 || len(c.ready) != 0 || c.lru.Len() != 0 {
		t.Fatal("clear retained candidate memory or queue references")
	}
}

type cachePrefetchTestPolicy struct{ clients []dnsClient }

func (p cachePrefetchTestPolicy) Match(string) []dnsClient { return p.clients }

func TestCacheControlPrefetchDirectGateUsesEffectivePoolAndLeaf(t *testing.T) {
	for _, test := range []struct {
		name  string
		kind  C.AdapterType
		qtype uint16
		setup func(*Resolver)
		want  bool
	}{
		{"automatic-direct", C.Direct, D.TypeA, nil, true},
		{"compatible", C.Compatible, D.TypeA, nil, true},
		{"proxy-named-DIRECT", C.Socks5, D.TypeA, nil, false},
		{"mixed-main", C.Direct, D.TypeA, func(r *Resolver) {
			r.main = append(r.main, transform([]NameServer{{Addr: "192.0.2.54:53", ProxyName: "forced"}}, nil)...)
		}, false},
		{"explicit-policy", C.Direct, D.TypeA, func(r *Resolver) {
			r.policy = []dnsPolicy{cachePrefetchTestPolicy{transform([]NameServer{{Addr: "192.0.2.54:53", ProxyName: "forced"}}, nil)}}
		}, false},
		{"explicit-fallback", C.Direct, D.TypeA, func(r *Resolver) {
			r.fallback = transform([]NameServer{{Addr: "192.0.2.54:53", ProxyName: "forced"}}, nil)
		}, false},
		{"unused-explicit-fallback-for-TXT", C.Direct, D.TypeTXT, func(r *Resolver) {
			r.fallback = transform([]NameServer{{Addr: "192.0.2.54:53", ProxyName: "forced"}}, nil)
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, c, leaf := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true})
			leaf.kind.Store(int32(test.kind))
			if test.setup != nil {
				test.setup(r)
			}
			query := new(D.Msg).SetQuestion("pool-proof.example.", test.qtype)
			ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
			if got := r.directDNSQueryEligible(ctx, query); got != test.want {
				t.Fatalf("effective DIRECT proof=%t, want %t", got, test.want)
			}
			ctx = c.Observe(ctx, query, key)
			answer := fakeIPRoutingTestReply(query)
			c.Store(ctx, key, query.Question[0], answer)
			if active := len(c.hot) == 1; active != test.want {
				t.Fatalf("prefetch admission ignored the actual leaf/pool: active=%t want=%t", active, test.want)
			}
		})
	}
}

func TestCacheControlPrefetchInternalAndAnonymousQueriesStayOnDemand(t *testing.T) {
	_, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true})
	query := routingTestQuery("infrastructure.example", 49)
	for _, test := range []struct {
		name string
		ctx  context.Context
	}{
		{"anonymous", icontext.WithDNSRoutingInbound(context.Background())},
		{"internal-target", icontext.WithDNSRoutingMetadata(context.Background(), routingTestOrigin())},
		{"bootstrap", icontext.WithDNSBootstrap(icontext.WithDNSRoutingInbound(icontext.WithDNSRoutingMetadata(context.Background(), routingTestOrigin())))},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := c.Context(test.ctx)
			ctx, _, err := c.r.prepareDNSRouting(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			key := c.r.cacheKey(ctx, query)
			ctx = c.Observe(ctx, query, key)
			c.Store(ctx, key, query.Question[0], cacheTestReply(query, 600))
			if len(c.hot) != 0 {
				t.Fatal("infrastructure/anonymous lookup activated scheduled prefetch")
			}
		})
	}
}

func TestCacheControlPrefetchRechecksRouteAndDiscardsOldScope(t *testing.T) {
	for _, nextKind := range []C.AdapterType{C.Direct, C.Socks5, C.Reject} {
		t.Run(nextKind.String(), func(t *testing.T) {
			_, c, first := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true})
			clock := cachePrefetchTestClock(c)
			second := newRoutingTestOutbound("changed-leaf")
			second.kind.Store(int32(nextKind))
			tunnel.UpdateProxies(map[string]C.Proxy{"DIRECT": routingTestProxy(first), "changed": routingTestProxy(second)}, nil)
			query := routingTestQuery("new-policy.example", 50)
			ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
			ctx = c.Observe(ctx, query, key)
			cachePrefetchTestStore(t, c, ctx, query, 300*time.Second)
			entry := cachePrefetchTestEntry(t, c, key)
			var calls atomic.Int32
			c.fetch = func(context.Context, *D.Msg) (*D.Msg, error) {
				calls.Add(1)
				return nil, nil
			}
			tunnel.UpdateRules([]C.Rule{RC.NewMatch("changed")}, nil, nil)
			clock.Store(entry.next.UnixNano())
			c.tick(c.now())
			cacheWaitIdle(t, c)
			if calls.Load() != 0 || len(c.hot) != 0 {
				t.Fatal("old scope's heat drove a query through a newly selected route")
			}
		})
	}
}

func TestCacheControlPrefetchRechecksLeafTypeEvenWithSameName(t *testing.T) {
	_, c, leaf := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true})
	clock := cachePrefetchTestClock(c)
	query := routingTestQuery("changed-type.example", 51)
	ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
	ctx = c.Observe(ctx, query, key)
	cachePrefetchTestStore(t, c, ctx, query, 300*time.Second)
	entry := cachePrefetchTestEntry(t, c, key)
	leaf.kind.Store(int32(C.Socks5))
	var calls atomic.Int32
	c.fetch = func(context.Context, *D.Msg) (*D.Msg, error) {
		calls.Add(1)
		return nil, nil
	}
	clock.Store(entry.next.UnixNano())
	c.tick(c.now())
	cacheWaitIdle(t, c)
	if calls.Load() != 0 || len(c.hot) != 0 {
		t.Fatal("a proxy retained scheduled DIRECT eligibility through its display name")
	}
}

func TestCacheControlStaleRefreshStillUsesProxyRoute(t *testing.T) {
	_, c, leaf := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true})
	leaf.kind.Store(int32(C.Socks5))
	query := routingTestQuery("proxy-stale.example", 52)
	ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
	var calls atomic.Int32
	c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
		if route := queryRoute(ctx); route == nil || route.plan.Type() != C.Socks5 {
			t.Error("on-demand refresh lost the selected proxy route")
		}
		calls.Add(1)
		answer := cacheTestReply(request, 600)
		c.Store(ctx, c.r.cacheKey(ctx, request), request.Question[0], answer)
		return answer, nil
	}
	c.Refresh(ctx, query, key)
	cacheWaitIdle(t, c)
	if calls.Load() != 1 || len(c.hot) != 0 {
		t.Fatal("DIRECT-only prefetch changed Mihomo's proxy stale-refresh path")
	}
}

func cacheTestOccupyBackgroundSlots(t *testing.T, count int) func(int) {
	t.Helper()
	for i := 0; i < count; i++ {
		select {
		case cacheBackgroundSlots <- struct{}{}:
		default:
			t.Fatal("test fixture found a leaked background reservation")
		}
	}
	remaining := count
	release := func(count int) {
		if count > remaining {
			t.Fatal("test released an unowned background reservation")
		}
		for i := 0; i < count; i++ {
			<-cacheBackgroundSlots
		}
		remaining -= count
	}
	t.Cleanup(func() { release(remaining) })
	return release
}

func TestCacheControlPrefetchDeadlineOrderAndFullSlotsPreserveHeat(t *testing.T) {
	_, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true})
	clock := cachePrefetchTestClock(c)
	keys := make(map[string]string)
	for _, test := range []struct {
		name string
		ttl  time.Duration
	}{{"later.example", 600 * time.Second}, {"earlier.example", 300 * time.Second}} {
		query := routingTestQuery(test.name, 53)
		ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
		ctx = c.Observe(ctx, query, key)
		cachePrefetchTestStore(t, c, ctx, query, test.ttl)
		keys[D.Fqdn(test.name)] = key
	}
	releaseSlots := cacheTestOccupyBackgroundSlots(t, 16)
	clock.Store(c.now().Add(500 * time.Second).UnixNano())
	c.tick(c.now())
	for _, key := range keys {
		if entry := cachePrefetchTestEntry(t, c, key); entry.heat != 3 || entry.running {
			t.Fatal("full-slot admission consumed heat or removed queued work")
		}
	}
	started, finish := make(chan string, 2), make(chan struct{})
	c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
		started <- request.Question[0].Name
		select {
		case <-finish:
			return cachePrefetchTestStore(t, c, ctx, request, 600*time.Second), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	releaseSlots(1)
	c.tick(c.now())
	select {
	case name := <-started:
		if name != "earlier.example." {
			t.Fatalf("newer candidate bypassed an older deadline: %s", name)
		}
	case <-time.After(time.Second):
		t.Fatal("free slot did not admit overdue work")
	}
	if entry := cachePrefetchTestEntry(t, c, keys["later.example."]); entry.heat != 3 {
		t.Fatal("waiting candidate lost heat while another key owned the slot")
	}
	close(finish)
	cacheWaitIdle(t, c)
	c.tick(c.now())
	cacheWaitIdle(t, c)
	select {
	case name := <-started:
		if name != "later.example." {
			t.Fatalf("oldest remaining deadline was not admitted: %s", name)
		}
	default:
		t.Fatal("slot pressure permanently discarded the later candidate")
	}
}

func TestCacheControlPrefetchStopsIdleSchedulerAndIgnoresRetiredTicks(t *testing.T) {
	_, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true})
	clock := cachePrefetchTestClock(c)
	query := routingTestQuery("scheduler-lifetime.example", 54)
	ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
	ctx = c.Observe(ctx, query, key)
	cachePrefetchTestStore(t, c, ctx, query, 300*time.Second)
	first := cacheTestScheduler(t, c)
	c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
		return cachePrefetchTestStore(t, c, ctx, request, 300*time.Second), nil
	}
	for i := 0; i < 2; i++ {
		entry := cachePrefetchTestEntry(t, c, key)
		clock.Store(entry.next.UnixNano())
		c.tickScheduler(first, c.now())
		cacheWaitIdle(t, c)
	}
	cacheWaitSchedulerStopped(t, c, first)
	c.Observe(ctx, query, key)
	second := cacheTestScheduler(t, c)
	if second == first {
		t.Fatal("new real demand reused a retiring scheduler")
	}
	c.tickScheduler(first, c.now().Add(24*time.Hour))
	if cacheTestScheduler(t, c) != second || len(c.hot) != 1 {
		t.Fatal("a retired ticker mutated its replacement's candidates")
	}
}

func TestCacheControlPrefetchDropsEvictedAnswers(t *testing.T) {
	r, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true})
	clock := cachePrefetchTestClock(c)
	query := routingTestQuery("evicted.example", 55)
	ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
	ctx = c.Observe(ctx, query, key)
	cachePrefetchTestStore(t, c, ctx, query, 300*time.Second)
	entry := cachePrefetchTestEntry(t, c, key)
	r.cache.Delete(key)
	var calls atomic.Int32
	c.fetch = func(context.Context, *D.Msg) (*D.Msg, error) {
		calls.Add(1)
		return nil, nil
	}
	clock.Store(entry.next.UnixNano())
	c.tick(c.now())
	if calls.Load() != 0 || len(c.hot) != 0 || c.hotBytes != 0 {
		t.Fatal("prefetch resurrected an answer evicted from the actual cache")
	}
}

func TestCacheControlPrefetchDemandBelongsToItsQuestionAndResolver(t *testing.T) {
	_, first, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true})
	query := routingTestQuery("demand-owner.example", 74)
	ctx, key := cachePrefetchTestPrepare(t, first, query, routingTestOrigin())
	ctx = first.Observe(ctx, query, key)
	otherQuestion := query.Copy()
	otherQuestion.Question[0].Qtype = D.TypeAAAA
	otherContext, reply, err := first.r.prepareDNSRouting(ctx, otherQuestion)
	if err != nil || reply != nil {
		t.Fatalf("prepare sibling question: reply=%v err=%v", reply, err)
	}
	otherKey := first.r.cacheKey(otherContext, otherQuestion)
	otherAnswer := new(D.Msg).SetReply(otherQuestion)
	otherAnswer.Answer = []D.RR{&D.AAAA{Hdr: D.RR_Header{Name: otherQuestion.Question[0].Name, Rrtype: D.TypeAAAA, Class: D.ClassINET, Ttl: 60}, AAAA: netip.MustParseAddr("2001:db8::1").AsSlice()}}
	if !first.Store(otherContext, otherKey, otherQuestion.Question[0], otherAnswer) {
		t.Fatal("auxiliary answer did not enter the cache")
	}
	_, second := cacheTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true})
	if !second.Store(second.Context(ctx), key, query.Question[0], cacheTestReply(query, 60)) {
		t.Fatal("second resolver fixture could not store an answer")
	}
	if len(first.hot) != 0 || len(second.hot) != 0 {
		t.Fatal("a sibling question or resolver inherited the original client's prefetch demand")
	}
	if !first.Store(ctx, key, query.Question[0], cacheTestReply(query, 60)) {
		t.Fatal("original answer could not enter the cache")
	}
	if got := cachePrefetchTestEntry(t, first, key); got.heat != 3 || got.demand != 1 {
		t.Fatal("an unrelated write consumed the original question's finite demand")
	}
}
