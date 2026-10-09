package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	R "github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
)

// Model a bootstrap lookup completing at the same instant as cancellation.
// A valid result does not authorize starting a new outbound operation afterward.
type dnsReviewCancelBootstrap struct {
	R.Resolver
	cancel context.CancelFunc
}

func (*dnsReviewCancelBootstrap) Invalid() bool { return true }
func (r *dnsReviewCancelBootstrap) LookupIP(context.Context, string) ([]netip.Addr, error) {
	r.cancel()
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}
func (r *dnsReviewCancelBootstrap) LookupIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	return r.LookupIP(ctx, host)
}

type dnsReviewCountOutbound struct {
	C.ProxyAdapter
	calls int
}

func (a *dnsReviewCountOutbound) DialContext(context.Context, *C.Metadata) (C.Conn, error) {
	a.calls++
	return nil, errors.New("unexpected post-cancellation dial")
}
func (a *dnsReviewCountOutbound) ListenPacketContext(context.Context, *C.Metadata) (C.PacketConn, error) {
	a.calls++
	return nil, errors.New("unexpected post-cancellation packet listener")
}

func TestDNSRoutingNativeCanceledBootstrapDoesNotDial(t *testing.T) {
	for _, network := range []string{"tcp", "udp", "packet"} {
		t.Run(network, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bootstrap := &dnsReviewCancelBootstrap{cancel: cancel}
			leaf := &dnsReviewCountOutbound{ProxyAdapter: newDNSProxyTestBase("cancel-test", C.Socks5, true)}
			plan := &DNSRoutingPlan{origin: dnsProxyTestResolver(C.UDP), route: dnsProxyRoute{proxy: leaf}}
			var err error
			if network == "packet" {
				_, err = plan.listenNativeDNS(ctx, "udp", "cancel-bootstrap.invalid:853", bootstrap)
			} else {
				_, err = plan.dialNativeDNS(ctx, network, "cancel-bootstrap.invalid:443", bootstrap)
			}
			if !errors.Is(err, context.Canceled) || leaf.calls != 0 {
				t.Fatalf("canceled bootstrap invoked outbound: calls=%d err=%v", leaf.calls, err)
			}
		})
	}
}

func TestDNSProxyCanceledBootstrapDoesNotExchange(t *testing.T) {
	dnsProxyTestState(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := R.ProxyServerHostResolver
	R.ProxyServerHostResolver = &dnsReviewCancelBootstrap{cancel: cancel}
	t.Cleanup(func() { R.ProxyServerHostResolver = old })
	md := dnsProxyTestResolver(C.UDP)
	md.DstIP = netip.Addr{}
	md.Host = "cancel-bootstrap.invalid"
	calls := 0
	_, err := exchangeDNSProxy(ctx, dnsProxyTestQuery(t, "query.example."), md,
		func(*C.Metadata) (dnsProxyRoute, error) {
			return dnsProxyRoute{proxy: newDNSProxyTestBase("cancel-test", C.Socks5, true)}, nil
		},
		func(context.Context, []byte, *C.Metadata, dnsProxyRoute) ([]byte, error) {
			calls++
			return nil, errors.New("unexpected post-cancellation exchange")
		})
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled bootstrap started DNS exchange: calls=%d err=%v", calls, err)
	}
}

// A cancellation-insensitive adapter may return a socket after the query has
// ended. Raw DNS must dispose of it without transmitting the query payload.
type dnsReviewLateRawTCP struct {
	dnsProxyTestConn
	closed atomic.Bool
	writes atomic.Int32
}

func (c *dnsReviewLateRawTCP) SetDeadline(time.Time) error { return nil }
func (c *dnsReviewLateRawTCP) Close() error                { c.closed.Store(true); return nil }
func (c *dnsReviewLateRawTCP) Write([]byte) (int, error) {
	c.writes.Add(1)
	return 0, net.ErrClosed
}

type dnsReviewLateRawUDP struct {
	dnsProxyTestProxyPacketConn
	closed atomic.Bool
	writes atomic.Int32
}

func (c *dnsReviewLateRawUDP) SetDeadline(time.Time) error { return nil }
func (c *dnsReviewLateRawUDP) Close() error                { c.closed.Store(true); return nil }
func (c *dnsReviewLateRawUDP) WriteTo([]byte, net.Addr) (int, error) {
	c.writes.Add(1)
	return 0, net.ErrClosed
}

func TestDNSProxyCanceledLateSocketNeverWrites(t *testing.T) {
	for _, network := range []C.NetWork{C.TCP, C.UDP} {
		t.Run(network.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			tcp := &dnsReviewLateRawTCP{}
			udp := &dnsReviewLateRawUDP{}
			leaf := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("late-cancel", C.Socks5, true)}
			leaf.dial = func(context.Context, *C.Metadata) (C.Conn, error) { cancel(); return tcp, nil }
			leaf.packet = func(context.Context, *C.Metadata) (C.PacketConn, error) { cancel(); return udp, nil }
			_, err := exchangeDNSProxyWire(ctx, dnsProxyTestQuery(t, "late-cancel.example."), dnsProxyTestResolver(network), dnsProxyRoute{proxy: leaf})
			closed, writes := tcp.closed.Load(), tcp.writes.Load()
			if network == C.UDP {
				closed, writes = udp.closed.Load(), udp.writes.Load()
			}
			if !errors.Is(err, context.Canceled) || !closed || writes != 0 {
				t.Fatalf("late canceled socket: err=%v closed=%v payload-writes=%d", err, closed, writes)
			}
		})
	}
}
