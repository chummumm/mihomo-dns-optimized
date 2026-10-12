package dns

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

func TestCacheControlPendingRefreshBoundsDedupAndExpiry(t *testing.T) {
	_, c := cacheTestControl(t, nil)
	clock := cachePrefetchTestClock(c)
	cacheTestOccupyBackgroundSlots(t, cap(cacheBackgroundSlots))
	baseline := cachePendingRefreshCount.Load()
	var calls atomic.Int32
	c.fetch = func(context.Context, *D.Msg) (*D.Msg, error) {
		calls.Add(1)
		return nil, nil
	}
	ctx := c.Context(context.Background())
	for n := 0; n < 100; n++ {
		query := routingTestQuery(fmt.Sprintf("queued-%d.example", n), uint16(n))
		c.Refresh(ctx, query, query.Question[0].String())
	}
	if len(c.pending) != cachePendingRefreshLimit || c.pendingOrder.Len() != cachePendingRefreshLimit || cachePendingRefreshCount.Load()-baseline != cachePendingRefreshLimit {
		t.Fatalf("overload did not retain only a bounded pending queue: items=%d global=%d", len(c.pending), cachePendingRefreshCount.Load()-baseline)
	}
	if calls.Load() != 0 || len(c.inflight) != 0 || c.pendingBytes > cachePendingRefreshBudget {
		t.Fatal("queue pressure started work without a slot or exceeded its byte budget")
	}
	query := routingTestQuery("queued-0.example", 100)
	key := query.Question[0].String()
	originalExpiry := c.pending[key].expires
	clock.Store(c.now().Add(20 * time.Second).UnixNano())
	for n := 0; n < 100; n++ {
		c.Refresh(ctx, query, key)
	}
	if len(c.pending) != cachePendingRefreshLimit || !c.pending[key].expires.Equal(originalExpiry) {
		t.Fatal("repeated stale hits duplicated a pending refresh or prolonged its queue lifetime")
	}
	scheduler := cacheTestScheduler(t, c)
	clock.Store(originalExpiry.UnixNano())
	c.tick(c.now())
	cacheWaitSchedulerStopped(t, c, scheduler)
	if cachePendingRefreshCount.Load() != baseline || c.pendingBytes != 0 || c.pendingOrder.Len() != 0 || calls.Load() != 0 {
		t.Fatal("expired queued work retained priority, memory, or started a late exchange")
	}
}

func TestCacheControlPendingRefreshByteBudget(t *testing.T) {
	_, c := cacheTestControl(t, nil)
	cacheTestOccupyBackgroundSlots(t, cap(cacheBackgroundSlots))
	baseline := cachePendingRefreshCount.Load()
	origin := routingTestOrigin()
	origin.ProcessPath = strings.Repeat("p", 9000)
	ctx := c.Context(icontext.WithDNSRoutingMetadata(context.Background(), origin))
	for n := 0; n < cachePendingRefreshLimit; n++ {
		query := routingTestQuery(fmt.Sprintf("large-queued-%d.example", n), uint16(n))
		c.Refresh(ctx, query, query.Question[0].String())
	}
	if len(c.pending) == 0 || len(c.pending) >= cachePendingRefreshLimit || c.pendingBytes > cachePendingRefreshBudget {
		t.Fatalf("large source snapshots ignored the independent byte budget: count=%d bytes=%d", len(c.pending), c.pendingBytes)
	}
	c.Clear()
	origin.ProcessPath = strings.Repeat("p", cachePrefetchEntryLimit)
	ctx = c.Context(icontext.WithDNSRoutingMetadata(context.Background(), origin))
	query := routingTestQuery("oversized-queued.example", 64)
	c.Refresh(ctx, query, query.Question[0].String())
	if len(c.pending) != 0 || c.pendingBytes != 0 || cachePendingRefreshCount.Load() != baseline {
		t.Fatal("a single oversized request retained an unbounded pending snapshot")
	}
}

func TestCacheControlPendingRefreshSnapshotsPreserveDNSIdentity(t *testing.T) {
	type requestOnlyState struct{}
	for _, test := range []struct {
		name             string
		source, inbound  bool
		bootstrap, fixed bool
	}{
		{"client-fixed-outbound", true, true, false, true},
		{"internal-source", true, false, false, false},
		{"anonymous-bootstrap", false, false, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, c := cacheTestControl(t, nil)
			releaseSlots := cacheTestOccupyBackgroundSlots(t, cap(cacheBackgroundSlots))
			origin := routingTestOrigin()
			origin.Process, origin.ProcessPath = "mihomo", "/usr/bin/mihomo"
			fixed := newRoutingTestOutbound("fixed-proxy")
			ctx := context.WithValue(context.Background(), requestOnlyState{}, make([]byte, 1<<20))
			if test.source {
				ctx = icontext.WithDNSRoutingMetadata(ctx, origin)
			}
			if test.inbound {
				ctx = icontext.WithDNSRoutingInbound(ctx)
			}
			if test.fixed {
				ctx = icontext.WithDNSFixedOutbound(ctx, fixed)
			}
			if test.bootstrap {
				ctx = icontext.WithDNSBootstrap(ctx)
			}
			ctx, cancel := context.WithCancel(c.Context(ctx))
			defer cancel()
			query := routingTestQuery("snapshot-queue.example", 65)
			query.SetEdns0(1232, true)
			key := query.Question[0].String()
			c.Refresh(ctx, query, key)
			if len(c.pending) != 1 {
				t.Fatal("full slots dropped the on-demand refresh")
			}
			cancel()
			query.Id, query.Question[0].Name = 66, "changed-after-queue.example."
			type result struct {
				ctx     context.Context
				request *D.Msg
			}
			observed := make(chan result, 1)
			c.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
				observed <- result{ctx, request.Copy()}
				return nil, nil
			}
			releaseSlots(1)
			c.tick(c.now())
			cacheWaitIdle(t, c)
			var got result
			select {
			case got = <-observed:
			default:
				t.Fatal("the caller's cancellation discarded a detached stale refresh")
			}
			if got.ctx.Value(requestOnlyState{}) != nil || !icontext.DNSProcessSnapshot(got.ctx) {
				t.Fatal("pending refresh retained unrelated parent state or re-discovered the old source process")
			}
			if icontext.DNSRoutingInbound(got.ctx) != test.inbound || icontext.DNSBootstrap(got.ctx) != test.bootstrap {
				t.Fatal("queued refresh lost its inbound or bootstrap identity")
			}
			md := icontext.DNSRoutingMetadata(got.ctx)
			if test.source {
				if md == nil || md.SrcIP != origin.SrcIP || md.SrcPort != origin.SrcPort || md.Process != origin.Process || md.ProcessPath != origin.ProcessPath {
					t.Fatal("delayed refresh used a different source or process identity")
				}
			} else if md != nil {
				t.Fatal("anonymous refresh invented a client source")
			}
			var wantFixed C.ProxyAdapter
			if test.fixed && !test.bootstrap {
				wantFixed = fixed
			}
			if icontext.DNSFixedOutbound(got.ctx) != wantFixed {
				t.Fatal("queued refresh lost a fixed route or applied it to bootstrap traffic")
			}
			if got.request.Id != 65 || got.request.Question[0].Name != "snapshot-queue.example." || got.request.IsEdns0() == nil || !got.request.IsEdns0().Do() || got.request.IsEdns0().UDPSize() != 1232 {
				t.Fatal("queued work retained a mutable DNS message or lost its EDNS flags")
			}
		})
	}
}

func TestCacheControlPendingRefreshPriorityAcrossControllers(t *testing.T) {
	_, scheduled, _ := cachePrefetchTestControl(t, &CacheOptions{Prefetch: true, ServeExpired: true})
	clock := cachePrefetchTestClock(scheduled)
	query := routingTestQuery("scheduled-priority.example", 67)
	ctx, key := cachePrefetchTestPrepare(t, scheduled, query, routingTestOrigin())
	ctx = scheduled.Observe(ctx, query, key)
	cachePrefetchTestStore(t, scheduled, ctx, query, 300*time.Second)
	entry := cachePrefetchTestEntry(t, scheduled, key)
	clock.Store(entry.next.UnixNano())
	_, onDemand := cacheTestControl(t, nil)
	releaseSlots := cacheTestOccupyBackgroundSlots(t, cap(cacheBackgroundSlots))
	baseline := cachePendingRefreshCount.Load()
	staleQuery := routingTestQuery("stale-priority.example", 68)
	onDemand.Refresh(onDemand.Context(context.Background()), staleQuery, staleQuery.Question[0].String())
	if len(onDemand.pending) != 1 || cachePendingRefreshCount.Load() != baseline+1 {
		t.Fatal("a default resolver did not retain its waiting stale-hit refresh")
	}
	var scheduledCalls atomic.Int32
	scheduled.fetch = func(ctx context.Context, request *D.Msg) (*D.Msg, error) {
		scheduledCalls.Add(1)
		return cachePrefetchTestStore(t, scheduled, ctx, request, 300*time.Second), nil
	}
	started, finish := make(chan struct{}), make(chan struct{})
	onDemand.fetch = func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
		close(started)
		select {
		case <-finish:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	releaseSlots(1)
	// Deliberately tick the busy proactive controller first. The free slot
	// must remain available to a waiting stale refresh in another resolver.
	scheduled.tick(scheduled.now())
	if got := cachePrefetchTestEntry(t, scheduled, key); got.heat != 3 || got.running || scheduledCalls.Load() != 0 {
		t.Fatal("proactive work stole the next free slot from pending client demand")
	}
	onDemand.tick(onDemand.now())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("pending stale refresh could not take its priority slot")
	}
	if cachePendingRefreshCount.Load() != baseline {
		t.Fatal("admitted stale refresh retained a global pending reservation")
	}
	scheduled.tick(scheduled.now())
	if got := cachePrefetchTestEntry(t, scheduled, key); got.heat != 3 || got.running {
		t.Fatal("waiting proactive work spent credit while on-demand work occupied the slot")
	}
	close(finish)
	cacheWaitIdle(t, onDemand)
	scheduled.tick(scheduled.now())
	cacheWaitIdle(t, scheduled)
	if scheduledCalls.Load() != 1 {
		t.Fatal("proactive work failed to resume after on-demand pressure ended")
	}
}

func TestCacheControlPendingRefreshStartsWhenWorkerReturnsSlot(t *testing.T) {
	_, c := cacheTestControl(t, nil)
	cacheTestOccupyBackgroundSlots(t, cap(cacheBackgroundSlots)-1)
	started, finish := make(chan string, 2), make(chan struct{})
	c.fetch = func(ctx context.Context, query *D.Msg) (*D.Msg, error) {
		started <- query.Question[0].Name
		select {
		case <-finish:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	first := routingTestQuery("first-worker.example", 69)
	ctx := c.Context(context.Background())
	c.Refresh(ctx, first, first.Question[0].String())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first worker never started")
	}
	second := routingTestQuery("waiting-worker.example", 70)
	for n := 0; n < 10; n++ {
		c.Refresh(ctx, first, first.Question[0].String())
		c.Refresh(ctx, second, second.Question[0].String())
	}
	if len(c.pending) != 1 || len(c.inflight) != 1 {
		t.Fatal("per-key deduplication did not span running and queued requests")
	}
	// No scheduler tick follows this release: returning the slot should wake
	// the already waiting request immediately, without a new foreground hit.
	finish <- struct{}{}
	select {
	case name := <-started:
		if name != "waiting-worker.example." {
			t.Fatalf("wrong request received the returned slot: %s", name)
		}
	case <-time.After(time.Second):
		t.Fatal("worker completion failed to dispatch its pending refresh")
	}
	close(finish)
	cacheWaitIdle(t, c)
}

func TestCacheControlPendingRefreshInvalidationReleasesPriority(t *testing.T) {
	for _, operation := range []string{"clear", "mode-round-trip", "close"} {
		t.Run(operation, func(t *testing.T) {
			routingTestEnable(t)
			_, c := cacheTestControl(t, nil)
			releaseSlots := cacheTestOccupyBackgroundSlots(t, cap(cacheBackgroundSlots))
			baseline := cachePendingRefreshCount.Load()
			query := routingTestQuery("invalidated-pending.example", 71)
			ctx := c.Context(context.Background())
			c.Refresh(ctx, query, query.Question[0].String())
			scheduler := cacheTestScheduler(t, c)
			var calls atomic.Int32
			c.fetch = func(context.Context, *D.Msg) (*D.Msg, error) {
				calls.Add(1)
				return nil, nil
			}
			switch operation {
			case "clear":
				c.Clear()
			case "mode-round-trip":
				tunnel.SetMode(tunnel.Global)
				tunnel.SetMode(tunnel.Rule)
				c.tick(c.now())
			case "close":
				c.Close()
			}
			cacheWaitSchedulerStopped(t, c, scheduler)
			if cachePendingRefreshCount.Load() != baseline || c.pendingBytes != 0 || c.pendingOrder.Len() != 0 {
				t.Fatal("an invalidated queue retained global priority or its source snapshots")
			}
			releaseSlots(cap(cacheBackgroundSlots))
			c.tick(c.now())
			c.Refresh(ctx, query, query.Question[0].String())
			cacheWaitIdle(t, c)
			if calls.Load() != 0 || len(c.pending) != 0 {
				t.Fatal("old queued work crossed a cache/policy generation boundary")
			}
		})
	}
}

func TestCacheControlPendingRefreshDropsAlreadyRefreshedAnswers(t *testing.T) {
	for _, test := range []struct {
		name  string
		rcode int
		fetch bool
	}{
		{"positive", D.RcodeSuccess, false},
		{"negative", D.RcodeNameError, false},
		{"cached-error-still-retries", D.RcodeServerFailure, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, c := cacheTestControl(t, nil)
			releaseSlots := cacheTestOccupyBackgroundSlots(t, cap(cacheBackgroundSlots))
			baseline := cachePendingRefreshCount.Load()
			query := routingTestQuery("fresh-during-queue.example", 72)
			key := query.Question[0].String()
			ctx := c.Context(context.Background())
			c.Refresh(ctx, query, key)
			answer := cacheTestReply(query, 60)
			answer.Rcode = test.rcode
			if test.rcode != D.RcodeSuccess {
				answer.Answer = nil
				if test.rcode == D.RcodeNameError {
					answer.Ns = []D.RR{&D.SOA{Hdr: D.RR_Header{Name: "example.", Rrtype: D.TypeSOA, Class: D.ClassINET, Ttl: 60}, Minttl: 60}}
				}
			}
			if !c.Store(ctx, key, query.Question[0], answer) {
				t.Fatal("foreground cache write failed")
			}
			var calls atomic.Int32
			c.fetch = func(context.Context, *D.Msg) (*D.Msg, error) {
				calls.Add(1)
				return nil, nil
			}
			releaseSlots(1)
			c.tick(c.now())
			cacheWaitIdle(t, c)
			if (calls.Load() == 1) != test.fetch || calls.Load() > 1 {
				t.Fatalf("fresh cache recheck mishandled a queued %s response: exchanges=%d", test.name, calls.Load())
			}
			if cachePendingRefreshCount.Load() != baseline || c.pendingBytes != 0 {
				t.Fatal("a satisfied pending refresh retained priority or its snapshot")
			}
		})
	}
}

func TestCacheControlPendingRefreshExpiredItemCannotSwallowNewDemand(t *testing.T) {
	_, c := cacheTestControl(t, nil)
	clock := cachePrefetchTestClock(c)
	releaseSlots := cacheTestOccupyBackgroundSlots(t, cap(cacheBackgroundSlots))
	query := routingTestQuery("renewed-demand.example", 73)
	key := query.Question[0].String()
	ctx := c.Context(context.Background())
	c.Refresh(ctx, query, key)
	clock.Store(c.now().Add(cachePendingRefreshMaxWait + time.Second).UnixNano())
	var calls atomic.Int32
	c.fetch = func(context.Context, *D.Msg) (*D.Msg, error) {
		calls.Add(1)
		return nil, nil
	}
	releaseSlots(1)
	// No tick has removed the old item yet. This is a separate, live client
	// request and must be admitted instead of merging into expired work.
	c.Refresh(ctx, query, key)
	cacheWaitIdle(t, c)
	if calls.Load() != 1 || len(c.pending) != 0 {
		t.Fatal("an expired queue item swallowed a subsequent client's refresh")
	}
}
