package adapter

import (
	"context"
	"errors"
	"testing"

	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
)

// This models the public group -> child Proxy call chain, with a different
// child on each call. A local lookup at the leaf must not reselect its parent.
type dnsRoutingContextAdapter struct {
	C.ProxyAdapter
	children []C.Proxy
	calls    int
	seen     context.Context
}

var errDNSRoutingContextStop = errors.New("stop before network I/O")

func (a *dnsRoutingContextAdapter) DialContext(ctx context.Context, m *C.Metadata) (C.Conn, error) {
	a.calls++
	if len(a.children) != 0 {
		return a.children[(a.calls-1)%len(a.children)].DialContext(ctx, m)
	}
	a.seen = ctx
	return nil, errDNSRoutingContextStop
}

func (a *dnsRoutingContextAdapter) ListenPacketContext(ctx context.Context, m *C.Metadata) (C.PacketConn, error) {
	a.calls++
	if len(a.children) != 0 {
		return a.children[(a.calls-1)%len(a.children)].ListenPacketContext(ctx, m)
	}
	a.seen = ctx
	return nil, errDNSRoutingContextStop
}

func TestDNSRoutingNestedProxyKeepsActualLeafForLocalLookup(t *testing.T) {
	for _, network := range []C.NetWork{C.TCP, C.UDP} {
		t.Run(network.String(), func(t *testing.T) {
			first, second := &dnsRoutingContextAdapter{}, &dnsRoutingContextAdapter{}
			inner := &dnsRoutingContextAdapter{children: []C.Proxy{NewProxy(first), NewProxy(second)}}
			outer := &dnsRoutingContextAdapter{children: []C.Proxy{NewProxy(inner)}}
			proxy := NewProxy(outer)
			origin := &C.Metadata{NetWork: network, InName: "source-inbound", Host: "site.example", DstPort: 443}
			ctx := icontext.WithDNSRoutingMetadata(context.Background(), origin)
			ctx = icontext.WithDNSFixedOutbound(ctx, outer)
			for _, want := range []*dnsRoutingContextAdapter{first, second} {
				var err error
				if network == C.TCP {
					_, err = proxy.DialContext(ctx, origin.Clone())
				} else {
					_, err = proxy.ListenPacketContext(ctx, origin.Clone())
				}
				if !errors.Is(err, errDNSRoutingContextStop) || want.seen == nil {
					t.Fatalf("selected leaf was not reached: %v", err)
				}
				if got := icontext.DNSFixedOutbound(want.seen); got != want {
					t.Fatalf("local lookup inherited %T %p instead of actual leaf %p", got, got, want)
				}
				if got := icontext.DNSRoutingMetadata(want.seen); got.InName != origin.InName || got.NetWork != network {
					t.Fatalf("original lookup identity was lost: %+v", got)
				}
				if icontext.DNSFixedOutbound(icontext.WithDNSBootstrap(want.seen)) != nil {
					t.Fatal("bootstrap lookup inherited business outbound")
				}
			}
			if outer.calls != 2 || inner.calls != 2 || first.calls != 1 || second.calls != 1 {
				t.Fatal("local lookup changed the group selection sequence")
			}
			if icontext.DNSFixedOutbound(ctx) != outer {
				t.Fatal("child replaced the caller's context")
			}
		})
	}
}
