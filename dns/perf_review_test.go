package dns

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

func TestDNSPerfSharedProbeKeepsEveryDualStackMeasurement(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("dualstack-shared")
	r := routingTestResolver()
	defer r.Close()
	q := new(D.Msg).SetQuestion("repeat.example.", D.TypeAAAA)
	ctx, _, err := r.prepareDNSRouting(routingTestContext(leaf, routingTestOrigin()), q)
	if err != nil {
		t.Fatal(err)
	}
	client := &dualStackTestClient{exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) { return dualStackTestAnswer(q), nil }}
	checker := speedCheckTestChecker(t, time.Second, 2)
	defer checker.pool.Close()
	var calls atomic.Int32
	probe := func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
		calls.Add(1)
		if ip.Is6() {
			return 30 * time.Millisecond, nil
		}
		return 5 * time.Millisecond, nil
	}
	for i := 0; i < 2; i++ {
		answer, cache, err := checker.ExchangeDualStackWithProbe(ctx, []dnsClient{client}, q, probe, DualStackConfig{Enabled: true, Threshold: 10 * time.Millisecond})
		if err != nil || answer == nil || len(answer.Answer) != 0 || cache {
			t.Fatalf("iteration %d lost reused family measurement: answer=%v cache=%v err=%v", i, answer, cache, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("two families should have one shared probe each, got %d", calls.Load())
	}
}

func TestDNSPerfCandidateCacheGenerationAndOwnership(t *testing.T) {
	cache := newDNSCandidateCache()
	q := new(D.Msg).SetQuestion("candidate.example.", D.TypeA)
	answer := dualStackTestAnswer(q)
	_, generation := cache.get("a")
	cache.store("a", generation, q, answer)
	first, _ := cache.get("a")
	if first == nil {
		t.Fatal("candidate not stored")
	}
	first.Answer = nil
	second, _ := cache.get("a")
	if len(second.Answer) == 0 {
		t.Fatal("caller mutated immutable candidate")
	}
	cache.Clear()
	cache.store("a", generation, q, answer)
	if stale, _ := cache.get("a"); stale != nil {
		t.Fatal("old generation refilled candidate cache")
	}
	_, generation = cache.get("a")
	cache.Close()
	cache.store("a", generation, q, answer)
	if stale, _ := cache.get("a"); stale != nil {
		t.Fatal("closed candidate cache resurrected")
	}
}

func TestDNSPerfProbePoolBoundedOverload(t *testing.T) {
	p := newDNSProbePool(1, 1, time.Second)
	defer p.Close()
	started := make(chan struct{})
	gate := make(chan struct{})
	deliver := func(time.Duration, error) {}
	_, err := p.submit(context.Background(), "running", func(ctx context.Context) (time.Duration, error) {
		close(started)
		select {
		case <-gate:
			return 0, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}, deliver)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	_, err = p.submit(context.Background(), "queued", func(context.Context) (time.Duration, error) { return 0, nil }, deliver)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.submit(context.Background(), "overflow", func(context.Context) (time.Duration, error) { return 0, nil }, deliver)
	if err != errDNSProbeBusy {
		t.Fatalf("unbounded queue admission: %v", err)
	}
	close(gate)
}
