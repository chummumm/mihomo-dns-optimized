package dns

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"

	D "github.com/miekg/dns"
)

func TestCacheControlExpiredPrefetchAnchorsAtExpiration(t *testing.T) {
	for _, test := range []struct {
		name      string
		retention time.Duration
		period    time.Duration
		age       time.Duration
		wait      time.Duration
	}{
		{"unlimited-default", 0, 0, time.Minute, 8*time.Hour - time.Minute},
		{"half-retention", 4 * time.Hour, 0, 30 * time.Minute, 90 * time.Minute},
		{"capped-default", 48 * time.Hour, 0, time.Hour, 7 * time.Hour},
		{"explicit-period", time.Hour, time.Minute, 20 * time.Second, 40 * time.Second},
		{"explicit-over-default-cap", 48 * time.Hour, 12 * time.Hour, time.Hour, 11 * time.Hour},
		{"already-due", time.Hour, 30 * time.Second, time.Minute, time.Second},
		{"minimum-default", 2 * time.Second, 0, 0, time.Second},
		{"minimum-explicit", time.Hour, time.Millisecond, 0, time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, c, _ := cachePrefetchTestControl(t, &CacheOptions{
				Prefetch: true, ServeExpired: true, ServeExpiredTTL: test.retention, ServeExpiredPrefetchTime: test.period,
			})
			clock := cachePrefetchTestClock(c)
			query := routingTestQuery("expired-timing.example", 70)
			ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
			expires := c.now().Add(-test.age)
			r.cache.SetWithExpire(key, cacheTestReply(query, 300), expires)
			c.Observe(ctx, query, key)
			entry := cachePrefetchTestEntry(t, c, key)
			if wait := entry.next.Sub(c.now()); wait != test.wait || !entry.expires.Equal(expires) {
				t.Fatalf("stale candidate lost its expiration anchor: wait=%v want=%v expires=%v want=%v", wait, test.wait, entry.expires, expires)
			}
			clock.Store(c.now().Add(500 * time.Millisecond).UnixNano())
			for i := 0; i < 10; i++ {
				c.Observe(ctx, query, key)
			}
			if updated := cachePrefetchTestEntry(t, c, key); !updated.next.Equal(entry.next) || updated.heat != cachePrefetchInitialHeat {
				t.Fatalf("new stale hits postponed the expiration-based deadline: before=%v after=%v heat=%v", entry.next, updated.next, updated.heat)
			}
		})
	}
}

func TestCacheControlExpiredPrefetchSuccessReturnsToFreshHalfTTL(t *testing.T) {
	r, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredPrefetchTime: time.Minute})
	clock := cachePrefetchTestClock(c)
	query := routingTestQuery("expired-to-fresh.example", 71)
	ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
	r.cache.SetWithExpire(key, cacheTestReply(query, 300), c.now().Add(-20*time.Second))
	c.Observe(ctx, query, key)
	entry := cachePrefetchTestEntry(t, c, key)
	if entry.next.Sub(c.now()) != 40*time.Second {
		t.Fatal("first stale refresh did not use the remaining expired interval")
	}
	var calls atomic.Int32
	c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
		calls.Add(1)
		return cachePrefetchTestStore(t, c, ctx, request, 300*time.Second), nil
	}
	clock.Store(entry.next.UnixNano())
	c.tick(c.now())
	cacheWaitIdle(t, c)
	entry = cachePrefetchTestEntry(t, c, key)
	if calls.Load() != 1 || entry.heat != 2 || entry.next.Sub(c.now()) != 150*time.Second || entry.expires.Sub(c.now()) != 300*time.Second {
		t.Fatalf("fresh success remained on the expired interval or changed cache validity: calls=%d entry=%+v", calls.Load(), entry)
	}
	clock.Store(entry.next.UnixNano())
	c.tick(c.now())
	cacheWaitIdle(t, c)
	if calls.Load() != 2 || len(c.hot) != 0 {
		t.Fatal("expired interval introduced an unlimited cold-domain refresh loop")
	}
}

func TestCacheControlExpiredPeriodDoesNotPostponeOverdueFreshWork(t *testing.T) {
	_, c, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredPrefetchTime: 24 * time.Hour})
	clock := cachePrefetchTestClock(c)
	query := routingTestQuery("overdue-fresh.example", 72)
	ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
	ctx = c.Observe(ctx, query, key)
	cachePrefetchTestStore(t, c, ctx, query, 300*time.Second)
	entry := cachePrefetchTestEntry(t, c, key)
	clock.Store(entry.expires.Add(time.Second).UnixNano())
	c.Observe(ctx, query, key)
	if updated := cachePrefetchTestEntry(t, c, key); !updated.next.Equal(entry.next) {
		t.Fatal("crossing expiration postponed an already scheduled fresh refresh")
	}
	var calls atomic.Int32
	c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
		calls.Add(1)
		return cachePrefetchTestStore(t, c, ctx, request, 300*time.Second), nil
	}
	c.tick(c.now())
	cacheWaitIdle(t, c)
	if calls.Load() != 1 {
		t.Fatal("overdue fresh work waited for the expired-only interval")
	}
}

func TestCacheControlOnDemandStaleRefreshDoesNotWaitForExpiredPeriod(t *testing.T) {
	for _, kind := range []C.AdapterType{C.Direct, C.Socks5} {
		t.Run(kind.String(), func(t *testing.T) {
			r, c, leaf := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredPrefetchTime: 24 * time.Hour})
			leaf.kind.Store(int32(kind))
			cachePrefetchTestClock(c)
			query := routingTestQuery("request-stale.example", 73)
			ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
			r.cache.SetWithExpire(key, cacheTestReply(query, 300), c.now().Add(-time.Second))
			ctx = c.Observe(ctx, query, key)
			var calls atomic.Int32
			c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
				if route := queryRoute(ctx); route == nil || route.plan.Type() != kind {
					t.Error("on-demand stale refresh changed the selected outbound")
				}
				calls.Add(1)
				return cachePrefetchTestStore(t, c, ctx, request, 300*time.Second), nil
			}
			// There is no scheduler tick or simulated passage of the 24h period.
			c.Refresh(ctx, query, key)
			cacheWaitIdle(t, c)
			if calls.Load() != 1 {
				t.Fatal("client-triggered stale refresh waited for the scheduled period")
			}
			if kind == C.Direct {
				entry := cachePrefetchTestEntry(t, c, key)
				if entry.next.Sub(c.now()) != 150*time.Second || entry.heat != cachePrefetchInitialHeat {
					t.Fatal("fresh on-demand success kept stale timing or consumed scheduled credit")
				}
			} else if len(c.hot) != 0 {
				t.Fatal("a proxy stale hit activated scheduled DIRECT prefetch")
			}
		})
	}
}

func TestCacheControlExpiredPeriodDoesNotOverrideEligibility(t *testing.T) {
	for _, test := range []struct {
		name    string
		options CacheOptions
	}{
		{"prefetch-disabled", CacheOptions{ServeExpired: true, ServeExpiredPrefetchTime: time.Second}},
		{"stale-disabled", CacheOptions{Prefetch: true, ServeExpiredPrefetchTime: time.Second}},
		{"stale-retention-ended", CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredTTL: time.Second, ServeExpiredPrefetchTime: time.Second}},
		{"negative-programmatic-period", CacheOptions{Prefetch: true, ServeExpired: true, ServeExpiredPrefetchTime: -time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, c, _ := cachePrefetchTestControl(t, &test.options)
			cachePrefetchTestClock(c)
			query := routingTestQuery("expired-ineligible.example", 74)
			ctx, key := cachePrefetchTestPrepare(t, c, query, routingTestOrigin())
			r.cache.SetWithExpire(key, cacheTestReply(query, 300), c.now().Add(-time.Minute))
			c.Observe(ctx, query, key)
			if len(c.hot) != 0 || c.scheduler != nil {
				t.Fatal("expired interval enabled ineligible scheduled work")
			}
		})
	}
}
