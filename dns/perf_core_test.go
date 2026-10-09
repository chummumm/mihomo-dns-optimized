package dns

import (
	"context"
	"errors"
	"github.com/metacubex/mihomo/tunnel"
	"sync/atomic"
	"testing"
	"time"
)

func TestDNSPerfReplyCacheSharesPortsButNotWork(t *testing.T) {
	routingTestEnable(t)
	a := newRoutingTestOutbound("perf-direct")
	r, md := routingTestResolver(), routingTestOrigin()
	defer r.Close()
	query := routingTestQuery("port-cache.example", 1)
	firstCtx := routingTestContext(a, md.Clone())
	routingTestExchange(t, r, firstCtx, query)
	md.SrcPort++
	secondCtx := routingTestContext(a, md.Clone())
	query.Id++
	routingTestExchange(t, r, secondCtx, query)
	if got := a.udp.Load(); got != 1 {
		t.Fatalf("same authorized path changed source port: %d upstream calls", got)
	}
	x, _, err := r.prepareDNSRouting(firstCtx, query)
	if err != nil {
		t.Fatal(err)
	}
	y, _, err := r.prepareDNSRouting(secondCtx, query)
	if err != nil {
		t.Fatal(err)
	}
	if queryRoute(x).key != queryRoute(y).key {
		t.Fatal("response key still contains source port")
	}
	if dnsQueryFlightKey(x, queryRoute(x).key) == dnsQueryFlightKey(y, queryRoute(y).key) {
		t.Fatal("independent cancellation scopes merged")
	}
	md.InUser = "other-user"
	routingTestExchange(t, r, routingTestContext(a, md.Clone()), query)
	if a.udp.Load() != 2 {
		t.Fatal("authentication scope was lost")
	}
	md.Process = "other-process"
	routingTestExchange(t, r, routingTestContext(a, md.Clone()), query)
	if a.udp.Load() != 3 {
		t.Fatal("process scope was lost")
	}
}

func TestDNSPerfProbePoolCoalescesAndCancels(t *testing.T) {
	p := newDNSProbePool(2, 4, time.Second)
	defer p.Close()
	var calls atomic.Int32
	gate := make(chan struct{})
	work := func(ctx context.Context) (time.Duration, error) {
		calls.Add(1)
		select {
		case <-gate:
			return time.Millisecond, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	results := make(chan error, 3)
	deliver := func(_ time.Duration, err error) { results <- err }
	a, err := p.submit(context.Background(), "same-direct/ip/tcp", work, deliver)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.submit(context.Background(), "same-direct/ip/tcp", work, deliver)
	if err != nil {
		t.Fatal(err)
	}
	a.Release()
	close(gate)
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shared probe did not finish")
	}
	b.Release()
	if calls.Load() != 1 {
		t.Fatal("duplicate target probed twice")
	}
	_, err = p.submit(context.Background(), "same-direct/ip/tcp", work, deliver)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("fresh successful probe not reused")
	}
}

func TestDNSPerfProbePoolClose(t *testing.T) {
	p := newDNSProbePool(1, 2, time.Second)
	started := make(chan struct{})
	results := make(chan error, 1)
	_, err := p.submit(context.Background(), "close", func(ctx context.Context) (time.Duration, error) { close(started); <-ctx.Done(); return 0, ctx.Err() }, func(_ time.Duration, err error) { results <- err })
	if err != nil {
		t.Fatal(err)
	}
	<-started
	p.Close()
	if err := <-results; err == nil {
		t.Fatal("closed pool returned success")
	}
	_, err = p.submit(context.Background(), "closed", nil, func(time.Duration, error) {})
	if !errors.Is(err, errDNSProbeClosed) {
		t.Fatalf("closed admission: %v", err)
	}
	p.Close()
}

func BenchmarkDNSPerfRoutePrepare(b *testing.B) {
	enabled, mode := tunnel.DNSRuleRoutingEnabled(), tunnel.Mode()
	tunnel.SetDNSRuleRouting(true)
	tunnel.SetMode(tunnel.Rule)
	b.Cleanup(func() { tunnel.SetDNSRuleRouting(enabled); tunnel.SetMode(mode) })
	// Benchmark the real route preparation; public DNS
	// and private proxy credentials are never contacted.
	old := newRoutingTestOutbound("bench")
	r, md := routingTestResolver(), routingTestOrigin()
	defer r.Close()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		md.SrcPort = uint16(40000 + i%10000)
		_, _, _ = r.prepareDNSRouting(routingTestContext(old, md.Clone()), routingTestQuery("bench.example", uint16(i)))
	}
}
