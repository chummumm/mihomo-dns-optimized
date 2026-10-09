package dns

import (
	"github.com/metacubex/mihomo/tunnel"
	"testing"
)

// This benchmark also compiles against the previous release: the same fixed
// source and cached answer are used, isolating hot-path CPU/allocation work.
func BenchmarkDNSPerfCachedAnswer(b *testing.B) {
	enabled, mode := tunnel.DNSRuleRoutingEnabled(), tunnel.Mode()
	tunnel.SetDNSRuleRouting(true)
	tunnel.SetMode(tunnel.Rule)
	b.Cleanup(func() { tunnel.SetDNSRuleRouting(enabled); tunnel.SetMode(mode) })
	leaf := newRoutingTestOutbound("bench")
	r, md := routingTestResolver(), routingTestOrigin()
	defer r.Close()
	ctx := routingTestContext(leaf, md)
	q := routingTestQuery("benchmark.example", 1)
	if _, err := r.ExchangeContext(ctx, q); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Id = uint16(i)
		if _, err := r.ExchangeContext(ctx, q); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if leaf.udp.Load() != 1 {
		b.Fatalf("benchmark did not stay on cache hits: %d", leaf.udp.Load())
	}
}
