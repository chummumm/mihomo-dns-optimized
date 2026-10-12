package dns

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	RC "github.com/metacubex/mihomo/rules/common"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

func cacheTestControl(t *testing.T, options *CacheOptions) (*Resolver, *cacheControl) {
	t.Helper()
	r := &Resolver{cache: Config{}.newCache()}
	c := newCacheControl(r, options)
	r.cacheControl = c
	c.tickEvery = time.Hour // tests advance the scheduler explicitly
	t.Cleanup(c.Close)
	return r, c
}

func cacheTestReply(query *D.Msg, ttl uint32) *D.Msg {
	reply := new(D.Msg).SetReply(query)
	reply.Answer = []D.RR{&D.A{
		Hdr: D.RR_Header{Name: query.Question[0].Name, Rrtype: D.TypeA, Class: D.ClassINET, Ttl: ttl},
		A:   net.IPv4(192, 0, 2, 1),
	}}
	return reply
}

func cacheWaitIdle(t *testing.T, controls ...*cacheControl) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		idle := true
		for _, c := range controls {
			c.mu.Lock()
			idle = idle && len(c.inflight) == 0 && len(c.pending) == 0
			c.mu.Unlock()
		}
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func cacheTestScheduler(t *testing.T, c *cacheControl) *cacheScheduler {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scheduler == nil {
		t.Fatal("active background work has no scheduler")
	}
	return c.scheduler
}

func cacheWaitSchedulerStopped(t *testing.T, c *cacheControl, scheduler *cacheScheduler) {
	t.Helper()
	select {
	case <-scheduler.done:
	case <-time.After(time.Second):
		t.Fatal("idle scheduler retained its resolver")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scheduler != nil || len(c.hot) != 0 || len(c.inflight) != 0 || len(c.pending) != 0 {
		t.Fatal("idle scheduler did not release its state")
	}
}

func TestCacheControlStaleRefreshStopsIdleSchedulerAndRestarts(t *testing.T) {
	_, c := cacheTestControl(t, nil)
	query := routingTestQuery("stale-scheduler.example", 31)
	key := query.Question[0].String()
	started, finish := make(chan struct{}), make(chan struct{})
	c.fetch = func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
		select {
		case started <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-finish:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var previous *cacheScheduler
	for n := 0; n < 2; n++ {
		c.Refresh(c.Context(context.Background()), query, key)
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("stale refresh did not start")
		}
		scheduler := cacheTestScheduler(t, c)
		if scheduler == previous {
			t.Fatal("fresh demand reused a stopped scheduler")
		}
		// The watcher must remain alive while a non-prefetch refresh is in
		// progress, so mode/flag changes can still cancel the exchange.
		c.tickScheduler(scheduler, time.Now())
		if cacheTestScheduler(t, c) != scheduler {
			t.Fatal("scheduler stopped with a refresh in flight")
		}
		finish <- struct{}{}
		cacheWaitSchedulerStopped(t, c, scheduler)
		previous = scheduler
	}
}

func TestCacheControlStaleSchedulerCancelsOnPolicyChange(t *testing.T) {
	routingTestEnable(t)
	_, c := cacheTestControl(t, nil)
	query := routingTestQuery("policy-scheduler.example", 33)
	started, canceled := make(chan struct{}), make(chan struct{})
	c.fetch = func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}
	c.Refresh(c.Context(context.Background()), query, query.Question[0].String())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stale refresh did not start")
	}
	scheduler := cacheTestScheduler(t, c)
	tunnel.SetMode(tunnel.Global)
	c.tickScheduler(scheduler, time.Now())
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("idle-stop change disabled mid-flight policy cancellation")
	}
	cacheWaitSchedulerStopped(t, c, scheduler)
}

func TestCacheControlExpiredPolicyUsesOriginalExpiration(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name    string
		options *CacheOptions
		age     time.Duration
		want    bool
		ttl     uint32
	}{
		{"upstream-default", nil, 30 * 24 * time.Hour, true, 1},
		{"disabled", &CacheOptions{}, time.Second, false, 0},
		{"zero-reply-ttl", &CacheOptions{ServeExpired: true}, time.Second, true, 0},
		{"at-limit", &CacheOptions{ServeExpired: true, ServeExpiredTTL: time.Minute, ServeExpiredReplyTTL: 7}, time.Minute, true, 7},
		{"past-limit", &CacheOptions{ServeExpired: true, ServeExpiredTTL: time.Minute}, time.Minute + time.Nanosecond, false, 0},
		{"not-expired", nil, -time.Second, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, c := cacheTestControl(t, test.options)
			expire := now.Add(-test.age)
			if ok, ttl := c.ServeStale(expire, now); ok != test.want || ttl != test.ttl {
				t.Fatalf("ServeStale=(%v,%d), want (%v,%d)", ok, ttl, test.want, test.ttl)
			}
			if test.name == "at-limit" {
				if ok, _ := c.ServeStale(expire, now.Add(time.Second)); ok {
					t.Fatal("access extended the maximum stale age")
				}
			}
		})
	}
}

func TestCacheControlClearCloseAndPolicyEpochRejectOldWrites(t *testing.T) {
	routingTestEnable(t)
	r, c := cacheTestControl(t, nil)
	query := routingTestQuery("generation.example", 1)
	key := query.Question[0].String()
	for _, invalidate := range []struct {
		name string
		do   func()
	}{
		{"clear", c.Clear},
		{"mode-round-trip", func() { tunnel.SetMode(tunnel.Global); tunnel.SetMode(tunnel.Rule) }},
		{"flag-round-trip", func() { tunnel.SetDNSRuleRouting(false); tunnel.SetDNSRuleRouting(true) }},
	} {
		t.Run(invalidate.name, func(t *testing.T) {
			old := c.Context(context.Background())
			oldFlight := c.FlightKey(old, key)
			invalidate.do()
			if c.Store(old, key, query.Question[0], cacheTestReply(query, 30)) {
				t.Fatal("old generation refilled the cache")
			}
			fresh := c.Context(context.Background())
			if c.FlightKey(fresh, key) == oldFlight {
				t.Fatal("new query could join old singleflight")
			}
			if !c.Store(fresh, key, query.Question[0], cacheTestReply(query, 30)) {
				t.Fatal("current generation could not populate cache")
			}
			c.Clear()
			if _, _, ok := r.cache.GetWithExpire(key); ok {
				t.Fatal("Clear did not remove existing cache entries")
			}
		})
	}
	ctx := c.Context(context.Background())
	c.Close()
	if c.Store(ctx, key, query.Question[0], cacheTestReply(query, 30)) {
		t.Fatal("closed resolver accepted an old response")
	}
}

func TestCacheControlBackgroundBoundDedupAndCancellation(t *testing.T) {
	_, first := cacheTestControl(t, nil)
	_, second := cacheTestControl(t, nil)
	started := make(chan struct{}, 32)
	canceled := make(chan struct{}, 32)
	for _, c := range []*cacheControl{first, second} {
		c.fetch = func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
			started <- struct{}{}
			<-ctx.Done()
			canceled <- struct{}{}
			return nil, ctx.Err()
		}
	}
	query := routingTestQuery("bounded.example", 2)
	for n := 0; n < 100; n++ {
		first.Refresh(first.Context(context.Background()), query, "one")
	}
	for n := 0; n < 40; n++ {
		c := first
		if n%2 == 0 {
			c = second
		}
		c.Refresh(c.Context(context.Background()), query, fmt.Sprintf("key-%d", n))
	}
	if got := len(cacheBackgroundSlots); got != 16 {
		t.Fatalf("global background admission=%d, want bounded 16", got)
	}
	for n := 0; n < 16; n++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("admitted refresh did not start")
		}
	}
	first.Close()
	second.Close()
	if got := len(canceled); got != 16 || len(cacheBackgroundSlots) != 0 {
		t.Fatalf("Close left work alive: canceled=%d slots=%d", got, len(cacheBackgroundSlots))
	}
}

func TestCacheControlCanceledRefreshCannotRefillClearedCache(t *testing.T) {
	r, c := cacheTestControl(t, nil)
	query := routingTestQuery("late-answer.example", 20)
	key := query.Question[0].String()
	ctx := c.Context(context.Background())
	background := context.WithValue(ctx, cacheBackgroundKey{}, true)
	if c.FlightKey(ctx, key) == c.FlightKey(background, key) {
		t.Fatal("foreground query could join a cancelable background flight")
	}
	started, accepted := make(chan struct{}), make(chan bool, 1)
	c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
		close(started)
		<-ctx.Done()
		late := cacheTestReply(request, 600)
		accepted <- c.Store(ctx, key, request.Question[0], late)
		return late, nil
	}
	c.Refresh(ctx, query, key)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	c.Clear()
	fresh := c.Context(context.Background())
	if !c.Store(fresh, key, query.Question[0], cacheTestReply(query, 30)) {
		t.Fatal("new generation could not write its response")
	}
	select {
	case stored := <-accepted:
		if stored {
			t.Fatal("canceled refresh overwrote the new generation")
		}
	case <-time.After(time.Second):
		t.Fatal("Clear did not cancel the old refresh")
	}
	message, _, ok := r.cache.GetWithExpire(key)
	if !ok || message.Answer[0].Header().Ttl != 30 {
		t.Fatal("late old response replaced current cached data")
	}
}

func TestCacheControlRefreshUsesCurrentRouteAndProcessSnapshot(t *testing.T) {
	routingTestEnable(t)
	oldProxies, oldProviders := tunnel.Proxies(), tunnel.Providers()
	oldRules, oldRuleProviders := tunnel.Rules(), tunnel.RuleProviders()
	t.Cleanup(func() {
		tunnel.UpdateProxies(oldProxies, oldProviders)
		tunnel.UpdateRules(oldRules, nil, oldRuleProviders)
	})
	first, second, rejected := newRoutingTestOutbound("first"), newRoutingTestOutbound("second"), newRoutingTestOutbound("rejected")
	rejected.kind.Store(int32(C.Reject))
	tunnel.UpdateProxies(map[string]C.Proxy{
		"DIRECT": routingTestProxy(first), "first": routingTestProxy(first),
		"second": routingTestProxy(second), "rejected": routingTestProxy(rejected),
	}, nil)
	tunnel.UpdateRules([]C.Rule{RC.NewMatch("first")}, nil, nil)
	r, c := cacheTestControl(t, nil)
	r.ruleRouting = true
	r.main = transform([]NameServer{{Addr: "192.0.2.53:53"}}, nil)
	query := routingTestQuery("live-rule.example", 5)
	origin := routingTestOrigin()
	ctx := c.Context(icontext.WithDNSRoutingMetadata(context.Background(), origin))
	ctx, reply, err := r.prepareDNSRouting(ctx, query)
	if err != nil || reply != nil {
		t.Fatalf("initial route: %v %v", reply, err)
	}
	oldKey := dnsCacheKey(ctx, query.Question[0])
	var calls atomic.Int32
	keys := make(chan string, 1)
	c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
		md := icontext.DNSRoutingMetadata(ctx)
		if !icontext.DNSProcessSnapshot(ctx) || md == nil || md.Process != origin.Process || md.SrcIP != origin.SrcIP || md.SrcPort != origin.SrcPort {
			t.Error("background query lost its source/process snapshot")
		}
		key := dnsCacheKey(ctx, request.Question[0])
		if key == oldKey {
			t.Error("background query reused the old route plan")
		}
		response := cacheTestReply(request, 60)
		c.Store(ctx, key, request.Question[0], response)
		keys <- key
		calls.Add(1)
		return response, nil
	}
	tunnel.UpdateRules([]C.Rule{RC.NewMatch("second")}, nil, nil)
	c.Refresh(ctx, query, oldKey)
	cacheWaitIdle(t, c)
	if calls.Load() != 1 {
		t.Fatal("current route refresh did not run")
	}
	newKey := <-keys
	if _, _, exists := r.cache.GetWithExpire(oldKey); exists {
		t.Fatal("new route response was stored under old key")
	}
	if _, _, exists := r.cache.GetWithExpire(newKey); !exists {
		t.Fatal("new route response was not cached in its own scope")
	}
	tunnel.UpdateRules([]C.Rule{RC.NewMatch("rejected")}, nil, nil)
	c.Refresh(ctx, query, oldKey)
	cacheWaitIdle(t, c)
	if calls.Load() != 1 {
		t.Fatal("query changed to REJECT still performed a DNS exchange")
	}
}
