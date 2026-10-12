package dns

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/tunnel"
	D "github.com/miekg/dns"
)

func TestDNSResponseModeOriginalBudgetBoundsUnansweredAuxiliary(t *testing.T) {
	routingTestEnable(t)
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestResponse} {
		t.Run(mode, func(t *testing.T) {
			config := selectionTestConfig()
			config.Fallback = nil
			config.SpeedCheck.ResponseMode = mode
			config.DualStack = DualStackConfig{Enabled: true, AllowForceAAAA: true}
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			auxiliaryStarted := make(chan context.Context, 1)
			auxiliaryDone := make(chan struct{})
			client := &selectionTestClient{dnsClient: r.main[0], exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
				if q.Question[0].Qtype == D.TypeAAAA {
					auxiliaryStarted <- ctx
					<-ctx.Done()
					close(auxiliaryDone)
					return nil, ctx.Err()
				}
				return selectionTestAnswer(q, "", "192.0.2.1"), nil
			}}
			r.main = []dnsClient{client}
			query := routingTestQuery("original-budget.example", 50)
			ctx := responseModePreparedContext(t, r, routingTestContext(newSelectionTestOutbound(), routingTestOrigin()), query)
			// The request has already consumed most of its one-second selection
			// budget. Neither the early handoff nor the other family may reset it.
			deadline := time.Now().Add(150 * time.Millisecond)
			ctx = context.WithValue(ctx, dnsSpeedCheckDeadlineKey{}, deadline)
			answer := routingTestExchange(t, r, ctx, query)
			if len(msgToIP(answer)) != 1 || len(cacheBackgroundSlots) != 1 {
				t.Fatal("an unmeasured family suppressed or delayed the primary answer")
			}
			auxiliaryCtx := responseModeReceive(t, auxiliaryStarted)
			if got, _ := auxiliaryCtx.Value(dnsSpeedCheckDeadlineKey{}).(time.Time); !got.Equal(deadline) {
				t.Fatal("auxiliary request restarted the selection budget")
			}
			barrier := responseModeBarrier(r, ctx, query)
			select {
			case final := <-barrier:
				if final.Err != nil || len(msgToIP(final.Val.Msg)) != 1 {
					t.Fatalf("unfinished auxiliary changed the valid answer: %+v", final)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("background selection outlived its original optimization deadline")
			}
			responseModeReceive(t, auxiliaryDone)
			if len(cacheBackgroundSlots) != 0 || len(dnsNativeWorkSlots) != 0 {
				t.Fatal("deadline completion leaked admission")
			}
			if client.count(D.TypeA) != 1 || client.count(D.TypeAAAA) != 1 {
				t.Fatal("background work restarted DNS")
			}
		})
	}
}

func TestDNSResponseModeParallelFallbackCannotPublishBeforeMainPolicy(t *testing.T) {
	routingTestEnable(t)
	for _, accepted := range []bool{true, false} {
		name := "main-rejected"
		if accepted {
			name = "main-accepted"
		}
		t.Run(name, func(t *testing.T) {
			config := selectionTestConfig()
			config.FallbackLazyQuery = false
			config.SpeedCheck.ResponseMode = ResponseModeFastestResponse
			config.FallbackIPFilter = []C.IpMatcher{selectionTestIPFilter{netip.MustParseAddr("192.0.2.5"): true}}
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			allowMain, finishFallbackProbe := make(chan struct{}), make(chan struct{})
			fallbackProbeStarted := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(finishFallbackProbe) })
			leaf := &responseModeTestOutbound{routingTestBase: &routingTestBase{name: "fallback-mode-direct", kind: C.Direct}}
			leaf.probe = func(ctx context.Context, md *C.Metadata) error {
				if md.DstIP == netip.MustParseAddr("198.51.100.1") {
					close(fallbackProbeStarted)
					select {
					case <-finishFallbackProbe:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}
			main := &selectionTestClient{dnsClient: r.main[0], exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
				select {
				case <-allowMain:
					ip := "192.0.2.5"
					if accepted {
						ip = "192.0.2.1"
					}
					return selectionTestAnswer(q, "main.example.", ip), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}
			fallback := &selectionTestClient{dnsClient: r.fallback[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				return selectionTestAnswer(q, "fallback.example.", "198.51.100.1"), nil
			}}
			r.main, r.fallback = []dnsClient{main}, []dnsClient{fallback}
			query := routingTestQuery("fallback-mode.example", 51)
			ctx := responseModePreparedContext(t, r, routingTestContext(leaf, routingTestOrigin()), query)
			replies := make(chan result, 1)
			go func() { m, err := r.ExchangeContext(ctx, query); replies <- result{m, err} }()
			// In fastest-response, the offer precedes probe submission. This
			// barrier proves fallback already offered its response to the gate.
			responseModeReceive(t, fallbackProbeStarted)
			select {
			case got := <-replies:
				t.Fatalf("fallback bypassed unfinished main policy: %+v", got)
			default:
			}
			if len(cacheBackgroundSlots) != 0 {
				t.Fatal("an unauthorized fallback offer acquired a continuation")
			}
			close(allowMain)
			got := responseModeReceive(t, replies)
			want := "198.51.100.1"
			if accepted {
				want = "192.0.2.1"
			}
			if got.Error != nil || len(msgToIP(got.Msg)) != 1 || msgToIP(got.Msg)[0].String() != want {
				t.Fatalf("wrong branch won: %+v, want %s", got, want)
			}
			if !accepted {
				barrier := responseModeBarrier(r, ctx, query)
				releaseOnce.Do(func() { close(finishFallbackProbe) })
				if final := responseModeReceive(t, barrier); final.Err != nil || msgToIP(final.Val.Msg)[0].String() != want {
					t.Fatalf("fallback continuation changed pools: %+v", final)
				}
			}
		})
	}
}

func TestDNSResponseModeBackgroundLifecycleRevocation(t *testing.T) {
	routingTestEnable(t)
	for _, event := range []string{"clear", "close", "mode", "newer-query", "manual-close"} {
		t.Run(event, func(t *testing.T) {
			config := selectionTestConfig()
			config.Fallback = nil
			config.Main = append(config.Main, NameServer{Addr: "192.0.2.54:53"})
			config.SpeedCheck.ResponseMode = ResponseModeFastestResponse
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			lateStarted := make(chan context.Context, 1)
			lateRelease, lateFinished := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(lateRelease) })
			first := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				ip := "192.0.2.1"
				if q.Id != 52 {
					ip = "192.0.2.3"
				}
				return selectionTestAnswer(q, "", ip), nil
			}}
			second := &selectionTestClient{dnsClient: r.main[1], exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
				if q.Id != 52 {
					return selectionTestAnswer(q, "", "192.0.2.3"), nil
				}
				lateStarted <- ctx
				// A deliberately uncooperative upstream demonstrates that late
				// results cannot recover a revoked cache write lease.
				<-lateRelease
				defer close(lateFinished)
				return selectionTestAnswer(q, "", "192.0.2.2"), nil
			}}
			r.main = []dnsClient{first, second}
			query := routingTestQuery("revoked-background.example", 52)
			query.SetEdns0(1232, false) // Deliberately bypass the optional raw-candidate cache.
			leaf := newSelectionTestOutbound()
			ctx := responseModePreparedContext(t, r, routingTestContext(leaf, routingTestOrigin()), query)
			answer := routingTestExchange(t, r, ctx, query)
			if msgToIP(answer)[0].String() != "192.0.2.1" {
				t.Fatal("early response missing")
			}
			lateCtx := responseModeReceive(t, lateStarted)
			barrier := responseModeBarrier(r, ctx, query)
			key := r.cacheKey(ctx, query)
			closed := make(chan struct{})
			switch event {
			case "clear":
				r.ClearCache()
			case "close":
				go func() { r.Close(); close(closed) }()
			case "mode":
				old := tunnel.Mode()
				defer tunnel.SetMode(old)
				tunnel.SetMode(tunnel.Direct)
				r.cacheControl.active(ctx)
			case "newer-query":
				query2 := query.Copy()
				query2.Id++
				origin := routingTestOrigin()
				origin.SrcPort++
				ctx2 := responseModePreparedContext(t, r, routingTestContext(leaf, origin), query2)
				if r.cacheKey(ctx2, query2) != key || r.cacheControl.FlightKey(ctx2, key) == r.cacheControl.FlightKey(ctx, key) {
					t.Fatal("test queries did not share cache and retain independent live scopes")
				}
				// Force a new legitimate miss while the old selection remains
				// in flight under the first source port.
				r.cache.Delete(key)
				fresh := routingTestExchange(t, r, ctx2, query2)
				if msgToIP(fresh)[0].String() != "192.0.2.3" {
					t.Fatal("newer query did not publish its own answer")
				}
			case "manual-close":
				queryRoute(lateCtx).closed.Store(true)
				releaseOnce.Do(func() { close(lateRelease) })
			}
			responseModeReceive(t, lateCtx.Done())
			responseModeReceive(t, barrier)
			releaseOnce.Do(func() { close(lateRelease) })
			responseModeReceive(t, lateFinished)
			if event == "close" {
				responseModeReceive(t, closed)
			}
			cached, _, hit := r.cache.GetWithExpire(key)
			if event == "clear" {
				if hit {
					t.Fatal("cleared cache was repopulated by old background work")
				}
			} else if hit && msgToIP(cached)[0].String() == "192.0.2.2" {
				t.Fatal("revoked late response overwrote the cache")
			}
			if event == "newer-query" && (!hit || msgToIP(cached)[0].String() != "192.0.2.3") {
				t.Fatal("older completion replaced the newer query's response")
			}
			rs.Close() // Joins every acquired background lease without polling.
			if len(cacheBackgroundSlots) != 0 || len(dnsNativeWorkSlots) != 0 {
				t.Fatal("revoked work leaked admission")
			}
		})
	}
}

func TestDNSResponseModeEarlyAndFinalCacheKeepOriginalTTL(t *testing.T) {
	routingTestEnable(t)
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestResponse} {
		t.Run(mode, func(t *testing.T) {
			config := selectionTestConfig()
			config.Fallback = nil
			config.Main = append(config.Main, NameServer{Addr: "192.0.2.54:53"})
			config.SpeedCheck.ResponseMode = mode
			config.AnswerPolicy = AnswerPolicy{RRTTLMin: 10}
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			writes := &selectionTestExpiryCache{dnsCache: r.cache, writes: make(chan time.Time, 4)}
			r.cache = writes
			late := make(chan struct{})
			firstReceived := time.Now().Add(-2500 * time.Millisecond)
			first := &selectionTestClient{dnsClient: r.main[0], exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
				answer := selectionTestAnswer(q, "", "192.0.2.1")
				answer.Answer[0].Header().Ttl = 1
				recordDNSAnswerReceived(ctx, answer, firstReceived)
				return answer, nil
			}}
			second := &selectionTestClient{dnsClient: r.main[1], exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
				select {
				case <-late:
					return nil, context.DeadlineExceeded
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}
			r.main = []dnsClient{first, second}
			query := routingTestQuery("early-lifetime.example", 54)
			ctx := responseModePreparedContext(t, r, routingTestContext(newSelectionTestOutbound(), routingTestOrigin()), query)
			answer := routingTestExchange(t, r, ctx, query)
			if answer.Answer[0].Header().Ttl != dnsEarlyReplyTTL {
				t.Fatalf("early client answer did not cap its aged lifetime: %v", answer)
			}
			barrier := responseModeBarrier(r, ctx, query)
			close(late)
			final := responseModeReceive(t, barrier)
			if final.Err != nil || final.Val.Answer[0].Header().Ttl != 8 {
				t.Fatalf("final answer double-aged or renewed TTL: %+v", final)
			}
			want := firstReceived.Add(10 * time.Second)
			for i := 0; i < 2; i++ {
				if got := responseModeReceive(t, writes.writes); !got.Equal(want) {
					t.Fatalf("early/final cache expiration was reset: got %v want %v", got, want)
				}
			}
		})
	}
}

func TestDNSResponseModeSnapshotKeepsFractionalLifetime(t *testing.T) {
	r := NewResolverFromClient(speedCheckTestClient{})
	defer r.Close()
	r.answerPolicy = AnswerPolicy{RRTTLMin: 10}
	query := routingTestQuery("fractional-lifetime.example", 55)
	message := selectionTestAnswer(query, "target.example.", "192.0.2.1")
	message.Answer[0].Header().Ttl = 1
	message.Answer[1].Header().Ttl = 20
	message.SetEdns0(1232, false)
	optFlags := message.IsEdns0().Hdr.Ttl
	ctx := withDNSAnswerLifetimes(r.cacheControl.Context(context.Background()))
	ctx = withSpeedCheckDeadline(ctx, time.Second)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	receivedAt := time.Now().Add(-1900 * time.Millisecond)
	recordDNSAnswerReceived(ctx, message, receivedAt)
	var snapshot *dnsExchangeResult
	publisher := &dnsResponsePublisher{
		r: r, ctx: ctx, query: query, key: "fractional-lifetime", cancel: cancel,
		release: func() {}, publish: func(result *dnsExchangeResult) bool { snapshot = result; return true },
	}
	defer publisher.Close()
	if !publisher.offer(message) || snapshot == nil || !snapshot.receivedAt.Equal(receivedAt) {
		t.Fatal("early snapshot did not retain the original receipt clock")
	}
	// After another 200ms, both wait segments have crossed a whole-second
	// boundary together. Apply the original clock once, including its remainder.
	late := ageDNSAnswerAt(snapshot.Msg.Copy(), snapshot.receivedAt, receivedAt.Add(2100*time.Millisecond))
	if late.Answer[0].Header().Ttl != 8 || late.Answer[1].Header().Ttl != 18 {
		t.Fatalf("late join rounded lifetime twice or flattened RR TTLs: %v", late)
	}
	if late.IsEdns0().Hdr.Ttl != optFlags || snapshot.Answer[0].Header().Ttl != 10 || snapshot.Answer[1].Header().Ttl != 20 {
		t.Fatal("aging mutated EDNS or the shared unaged snapshot")
	}
}

func TestDNSResponseModeDualStackEarlyUsesOnlyCompletedMeasurements(t *testing.T) {
	query := new(D.Msg).SetQuestion("early-family.example.", D.TypeAAAA)
	otherQuery := new(D.Msg).SetQuestion("early-family.example.", D.TypeA)
	answer := selectionTestAnswer(query, "", "2001:db8::1")
	otherAnswer := selectionTestAnswer(otherQuery, "", "192.0.2.1")
	pending := make(chan dualStackExchangeResult, 1)
	decision := dualStackDecision{
		query: query, otherQuery: otherQuery, pending: pending, threshold: 10 * time.Millisecond,
		primary: &dualStackMeasurements{rtts: map[netip.Addr]time.Duration{netip.MustParseAddr("2001:db8::1"): 30 * time.Millisecond}},
		other:   &dualStackMeasurements{rtts: make(map[netip.Addr]time.Duration)},
	}
	if decision.collect() {
		t.Fatal("unfinished auxiliary result was considered complete")
	}
	if got, cache := decision.prefer(answer, true); got != answer || !cache {
		t.Fatal("missing measurements suppressed a family")
	}
	// Completing the DNS response alone is insufficient. A skipped or failed
	// optional probe must not become reachability or zero-RTT evidence.
	pending <- dualStackExchangeResult{message: otherAnswer, cache: true}
	if !decision.collect() {
		t.Fatal("completed auxiliary result was not collected")
	}
	if got, cache := decision.prefer(answer, true); got != answer || !cache {
		t.Fatal("an unmeasured auxiliary answer suppressed a family")
	}
	decision.other.observe(netip.MustParseAddr("192.0.2.1"), 5*time.Millisecond, nil)
	for _, stage := range []string{"early-publication", "no-background-slot", "final-completion"} {
		if !decision.collect() {
			t.Fatalf("%s lost the already consumed auxiliary result", stage)
		}
		got, cache := decision.prefer(answer, true)
		if cache || got.Rcode != D.RcodeSuccess || len(got.Answer) != 0 || len(got.Ns) != 1 || got.Ns[0].Header().Ttl != 0 {
			t.Fatalf("%s lost the measured transient preference: %v cache=%t", stage, got, cache)
		}
	}
}

func TestDNSResponseModeProbeSaturationPreservesCompleteAnswer(t *testing.T) {
	routingTestEnable(t)
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse} {
		t.Run(mode, func(t *testing.T) {
			config := selectionTestConfig()
			config.Fallback = nil
			config.SpeedCheck.ResponseMode = mode
			config.SpeedCheck.Concurrency = 1
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			occupied := make(chan struct{})
			_, err := r.speedChecker.pool.submit(context.Background(), "occupied", func(ctx context.Context) (time.Duration, error) {
				close(occupied)
				<-ctx.Done()
				return 0, ctx.Err()
			}, func(time.Duration, error) {})
			if err != nil {
				t.Fatal(err)
			}
			responseModeReceive(t, occupied)
			client := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				return selectionTestAnswer(q, "unchanged.example.", "192.0.2.1", "192.0.2.2"), nil
			}}
			r.main = []dnsClient{client}
			leaf := newSelectionTestOutbound()
			query := routingTestQuery("saturated-probes.example", 56)
			replies := make(chan result, 1)
			go func() {
				message, err := r.ExchangeContext(routingTestContext(leaf, routingTestOrigin()), query)
				replies <- result{message, err}
			}()
			select {
			case got := <-replies:
				if got.Error != nil || len(msgToIP(got.Msg)) != 2 || len(got.Msg.Answer) != 3 {
					t.Fatalf("busy probes lost the complete valid answer: %+v", got)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("DNS waited behind an unrelated one-second IP probe")
			}
			rs.Close()
			if leaf.probed("192.0.2.1") || leaf.probed("192.0.2.2") || len(dnsNativeWorkSlots) != 0 || len(cacheBackgroundSlots) != 0 {
				t.Fatal("saturated probes were queued or leaked query admission")
			}
		})
	}
}
