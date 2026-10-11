package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/common/singleflight"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/tunnel"
	D "github.com/miekg/dns"
)

func responseModeReceive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("bounded response-mode work did not complete")
		var zero T
		return zero
	}
}

type responseModeTestOutbound struct {
	*routingTestBase
	probe func(context.Context, *C.Metadata) error
}

func (a *responseModeTestOutbound) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	if err := a.probe(ctx, metadata); err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	_ = server.Close()
	return &routingTestTCPConn{ExtendedConn: N.NewExtendedConn(client), routingTestConnection: routingTestConnection{chain: C.Chain{a.Name()}}}, nil
}

func responseModePreparedContext(t *testing.T, r *Resolver, ctx context.Context, query *D.Msg) context.Context {
	t.Helper()
	ctx = r.cacheControl.Context(ctx)
	ctx, reply, err := r.prepareDNSRouting(ctx, query)
	if err != nil || reply != nil {
		t.Fatalf("prepare: %v %v", reply, err)
	}
	return ctx
}

func responseModeBarrier(r *Resolver, ctx context.Context, query *D.Msg) <-chan singleflight.Result[*dnsExchangeResult] {
	key := r.cacheControl.FlightKey(ctx, r.cacheKey(ctx, query))
	return r.group.DoChan(key, func() (*dnsExchangeResult, error) { return nil, errors.New("work already completed") })
}

func TestDNSResponseModeConfigurationAndDefault(t *testing.T) {
	for _, mode := range []string{"", ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse} {
		checker, err := newDirectSpeedChecker(SpeedCheckConfig{Mode: []string{"tcp:443"}, ResponseMode: mode})
		if err != nil || checker.responseMode != normalizeResponseMode(mode) {
			t.Fatalf("mode %q: %#v %v", mode, checker, err)
		}
		checker.pool.Close()
	}
	if ValidateResponseMode("first-response") == nil {
		t.Fatal("unknown mode accepted")
	}
	if err := ValidateSpeedCheckConfig(SpeedCheckConfig{Mode: []string{"none"}, ResponseMode: "bogus"}); err == nil {
		t.Fatal("disabled probes hid invalid response mode")
	}
}

func TestDNSResponseModeEarlyResultAndSameWorkCacheImprovement(t *testing.T) {
	routingTestEnable(t)
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse} {
		t.Run(mode, func(t *testing.T) {
			config := selectionTestConfig()
			config.Fallback = nil
			config.Main = append(config.Main, NameServer{Addr: "192.0.2.54:53"})
			config.SpeedCheck.ResponseMode = mode
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			lateDNS, finishFirstProbe := make(chan struct{}), make(chan struct{})
			firstProbeStarted, secondProbeFinished := make(chan struct{}), make(chan struct{})
			var firstProbeOnce, secondProbeOnce sync.Once
			leaf := &responseModeTestOutbound{routingTestBase: &routingTestBase{name: "response-mode-direct", kind: C.Direct}}
			leaf.probe = func(ctx context.Context, md *C.Metadata) error {
				if md.DstIP == netip.MustParseAddr("192.0.2.1") {
					firstProbeOnce.Do(func() { close(firstProbeStarted) })
					select {
					case <-finishFirstProbe:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				secondProbeOnce.Do(func() { close(secondProbeFinished) })
				return nil
			}
			first := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				return selectionTestAnswer(q, "first.example.", "192.0.2.1"), nil
			}}
			second := &selectionTestClient{dnsClient: r.main[1], exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
				select {
				case <-lateDNS:
					return selectionTestAnswer(q, "best.example.", "192.0.2.2"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}
			r.main = []dnsClient{first, second}
			query := routingTestQuery("response-mode.example", 31)
			ctx := responseModePreparedContext(t, r, routingTestContext(leaf, routingTestOrigin()), query)
			replies := make(chan result, 1)
			go func() { m, err := r.ExchangeContext(ctx, query); replies <- result{m, err} }()
			if mode == ResponseModeFastestResponse {
				got := responseModeReceive(t, replies)
				if got.Error != nil || msgToIP(got.Msg)[0].String() != "192.0.2.1" {
					t.Fatalf("first DNS reply was not returned: %+v", got)
				}
			}
			responseModeReceive(t, firstProbeStarted)
			if mode != ResponseModeFastestResponse {
				select {
				case got := <-replies:
					t.Fatalf("returned before first probe: %+v", got)
				default:
				}
			}
			// The late upstream's probe is deliberately started after the first
			// one has spent measurable time waiting. It has a lower actual RTT.
			time.Sleep(5 * time.Millisecond)
			close(finishFirstProbe)
			if mode == ResponseModeFirstPing {
				got := responseModeReceive(t, replies)
				if got.Error != nil || msgToIP(got.Msg)[0].String() != "192.0.2.1" {
					t.Fatalf("first successful probe was not returned: %+v", got)
				}
			}
			if mode == ResponseModeFastestIP {
				select {
				case got := <-replies:
					t.Fatalf("full comparison returned before late upstream: %+v", got)
				default:
				}
			} else {
				if len(dnsNativeWorkSlots) != 0 || len(cacheBackgroundSlots) != 1 {
					t.Fatalf("handoff did not separate foreground/background admission: %d/%d", len(dnsNativeWorkSlots), len(cacheBackgroundSlots))
				}
			}
			barrier := responseModeBarrier(r, ctx, query)
			close(lateDNS)
			responseModeReceive(t, secondProbeFinished)
			final := responseModeReceive(t, barrier)
			if final.Err != nil || msgToIP(final.Val.Msg)[0].String() != "192.0.2.2" {
				t.Fatalf("final best response: %+v", final)
			}
			if mode == ResponseModeFastestIP {
				got := responseModeReceive(t, replies)
				if got.Error != nil || msgToIP(got.Msg)[0].String() != "192.0.2.2" {
					t.Fatalf("full comparison did not pick best RTT: %+v", got)
				}
			}
			cached := routingTestExchange(t, r, ctx, query)
			if msgToIP(cached)[0].String() != "192.0.2.2" || first.count(0) != 1 || second.count(0) != 1 {
				t.Fatalf("improvement restarted DNS or missed cache: %v calls=%d/%d", cached, first.count(0), second.count(0))
			}
			if len(dnsNativeWorkSlots) != 0 || len(cacheBackgroundSlots) != 0 {
				t.Fatal("completed selection leaked admission slots")
			}
		})
	}
}

func TestDNSResponseModeBackgroundSaturationReturnsValidAnswer(t *testing.T) {
	routingTestEnable(t)
	for i := 0; i < cap(cacheBackgroundSlots); i++ {
		cacheBackgroundSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(cacheBackgroundSlots); i++ {
			<-cacheBackgroundSlots
		}
	}()
	config := selectionTestConfig()
	config.Fallback = nil
	config.SpeedCheck.ResponseMode = ResponseModeFirstPing
	rs := NewResolver(config)
	defer rs.Close()
	r := rs.Resolver
	client := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) { return selectionTestAnswer(q, "", "192.0.2.1"), nil }}
	r.main = []dnsClient{client}
	query := routingTestQuery("saturated-background.example", 32)
	ctx := routingTestContext(newSelectionTestOutbound(), routingTestOrigin())
	answer := routingTestExchange(t, r, ctx, query)
	if len(msgToIP(answer)) != 1 || len(dnsNativeWorkSlots) != 0 {
		t.Fatal("background saturation prevented a valid foreground answer")
	}
	// Existing prefetch admission is independently bounded and cannot use up
	// the 256 native foreground slots either.
	prepared := responseModePreparedContext(t, r, ctx, query)
	release, err := admitDNSNativeWork(context.WithValue(prepared, cacheBackgroundKey{}, true))
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(dnsNativeWorkSlots) != 0 {
		t.Fatal("background work consumed foreground admission")
	}
}

func TestDNSResponseModeLateJoinAgesAndCopiesEarlySnapshot(t *testing.T) {
	r := NewResolverFromClient(speedCheckTestClient{})
	defer r.Close()
	query := routingTestQuery("late-early.example", 1)
	ctx := r.cacheControl.Context(context.Background())
	key := r.cacheControl.FlightKey(ctx, r.cacheKey(ctx, query))
	answer := cacheTestReply(query, 10)
	finish := make(chan struct{})
	first := r.group.DoChanEarly(key, func(publish func(*dnsExchangeResult) bool) (*dnsExchangeResult, error) {
		publish(&dnsExchangeResult{Msg: answer, receivedAt: time.Now().Add(-2500 * time.Millisecond)})
		<-finish
		return &dnsExchangeResult{Msg: answer}, nil
	})
	responseModeReceive(t, first)
	barrier := r.group.DoChan(key, func() (*dnsExchangeResult, error) { return nil, errors.New("unexpected duplicate") })
	for _, id := range []uint16{42, 43} {
		query.Id = id
		got, err := r.exchangeWithoutCache(ctx, query.Copy())
		if err != nil || got.Id != id || got.Answer[0].Header().Ttl != 8 {
			t.Fatalf("late join renewed TTL or lost its ID: %v %v", got, err)
		}
		got.Answer[0].Header().Ttl = 99
	}
	if answer.Id != 1 || answer.Answer[0].Header().Ttl != 10 {
		t.Fatal("a waiter modified the shared early snapshot")
	}
	close(finish)
	responseModeReceive(t, barrier)
}

func TestDNSResponseModeBackgroundLeaseInvalidation(t *testing.T) {
	for _, event := range []string{"clear", "close", "mode", "newer-write", "manual-close"} {
		t.Run(event, func(t *testing.T) {
			r := NewResolverFromClient(speedCheckTestClient{})
			defer r.Close()
			query := routingTestQuery("lease.example", 1)
			ctx := r.cacheControl.Context(context.Background())
			ctx = withSpeedCheckDeadline(ctx, time.Second)
			ctx, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			route := &dnsQueryRoute{}
			if event == "manual-close" {
				ctx = context.WithValue(ctx, dnsQueryRouteKey{}, route)
			}
			lease := r.cacheControl.beginResponseContinuation(ctx, "lease-key", cancel)
			if lease == nil {
				t.Fatal("could not acquire continuation lease")
			}
			writeCtx := context.WithValue(ctx, dnsResponseContinuationKey{}, lease)
			if !r.cacheControl.Store(writeCtx, "lease-key", query.Question[0], cacheTestReply(query, 10)) {
				t.Fatal("initial valid lease write rejected")
			}
			closed := make(chan struct{})
			switch event {
			case "clear":
				r.ClearCache()
			case "close":
				go func() { r.Close(); close(closed) }()
				responseModeReceive(t, ctx.Done())
			case "mode":
				old := tunnel.Mode()
				defer tunnel.SetMode(old)
				next := tunnel.Direct
				if old == next {
					next = tunnel.Global
				}
				tunnel.SetMode(next)
				r.cacheControl.active(ctx)
			case "newer-write":
				if !r.cacheControl.Store(r.cacheControl.Context(context.Background()), "lease-key", query.Question[0], cacheTestReply(query, 30)) {
					t.Fatal("newer write failed")
				}
			case "manual-close":
				route.closed.Store(true)
			}
			if r.cacheControl.Store(writeCtx, "lease-key", query.Question[0], cacheTestReply(query, 60)) {
				t.Fatal("revoked background work refilled or overwrote the cache")
			}
			lease.Close()
			if event == "close" {
				responseModeReceive(t, closed)
			}
			if len(cacheBackgroundSlots) != 0 {
				t.Fatal("lease leaked background admission")
			}
			if event == "newer-write" {
				got, _, _ := r.cache.GetWithExpire("lease-key")
				if got == nil || got.Answer[0].Header().Ttl != 30 {
					t.Fatal("older completion overwrote a newer answer")
				}
			}
		})
	}
}
