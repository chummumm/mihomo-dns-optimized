package dns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/dnsstats"
	C "github.com/metacubex/mihomo/constant"
	D "github.com/miekg/dns"
)

// These existing I/O-boundary regressions exercise exchange-owned sockets.
// Shared sockets have a separate waiter-completion boundary and are covered by
// shared_transport_test.go; canceling one of their waiters must not close them.
type dedicatedDNSDoTTestClient struct{ *dnsOverTLS }

func (c dedicatedDNSDoTTestClient) ExchangeContext(ctx context.Context, m *D.Msg) (*D.Msg, error) {
	return c.exchangeDedicated(ctx, m)
}

func dedicatedDNSExchange(c dnsClient, ctx context.Context, m *D.Msg) (*D.Msg, error) {
	if dot, ok := c.(*dnsOverTLS); ok {
		return dot.exchangeDedicated(ctx, m)
	}
	return c.ExchangeContext(ctx, m)
}

type dnsCancelOutbound struct {
	*routingTestOutbound
	tcp    net.Conn
	packet net.PacketConn
}

func (a *dnsCancelOutbound) DialContext(context.Context, *C.Metadata) (C.Conn, error) {
	return &routingTestTCPConn{ExtendedConn: N.NewExtendedConn(a.tcp), routingTestConnection: routingTestConnection{chain: C.Chain{a.Name()}}}, nil
}

func (a *dnsCancelOutbound) ListenPacketContext(ctx context.Context, md *C.Metadata) (C.PacketConn, error) {
	if a.packet == nil {
		return a.routingTestOutbound.ListenPacketContext(ctx, md)
	}
	return &routingTestUDPConn{EnhancePacketConn: N.NewEnhancePacketConn(a.packet), routingTestConnection: routingTestConnection{chain: C.Chain{a.Name()}}}, nil
}

// The socket has already obtained the bytes/native error when acquired closes.
// Delay handing it back to the protocol caller until cancellation has actually
// closed the socket. A result-channel workaround deterministically loses this
// result; synchronous guarded I/O preserves it.
type dnsReadBoundary struct {
	acquired, release, closed        chan struct{}
	readOnce, closeOnce, releaseOnce sync.Once
	err                              error
}

func newDNSReadBoundary(t *testing.T, err error) *dnsReadBoundary {
	t.Helper()
	b := &dnsReadBoundary{acquired: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}), err: err}
	t.Cleanup(b.unblock)
	return b
}

func (b *dnsReadBoundary) read() {
	b.readOnce.Do(func() { close(b.acquired); <-b.release })
}

func (b *dnsReadBoundary) close()   { b.closeOnce.Do(func() { close(b.closed) }) }
func (b *dnsReadBoundary) unblock() { b.releaseOnce.Do(func() { close(b.release) }) }

type dnsBoundaryConn struct {
	net.Conn
	boundary  *dnsReadBoundary
	remaining int
}

func (c *dnsBoundaryConn) Read(p []byte) (int, error) {
	if c.boundary.err != nil {
		c.boundary.read()
		return 0, c.boundary.err
	}
	n, err := c.Conn.Read(p)
	c.remaining -= n
	if c.remaining <= 0 {
		c.boundary.read()
	}
	return n, err
}

func (c *dnsBoundaryConn) Close() error {
	err := c.Conn.Close()
	c.boundary.close()
	return err
}

type dnsBoundaryPacketConn struct {
	net.PacketConn
	boundary *dnsReadBoundary
}

func (c *dnsBoundaryPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if c.boundary.err != nil {
		c.boundary.read()
		return 0, nil, c.boundary.err
	}
	n, addr, err := c.PacketConn.ReadFrom(p)
	c.boundary.read()
	return n, addr, err
}

func (c *dnsBoundaryPacketConn) Close() error {
	err := c.PacketConn.Close()
	c.boundary.close()
	return err
}

func waitDNSCancelSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

type dnsCancelExchangeResult struct {
	message *D.Msg
	err     error
}

func waitDNSCancelResult(t *testing.T, results <-chan dnsCancelExchangeResult) dnsCancelExchangeResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("canceled DNS I/O did not finish")
		return dnsCancelExchangeResult{}
	}
}

func TestDNSClientLateCancellationPreservesIOResult(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp", "tls"} {
		for _, nativeErr := range []error{nil, io.EOF, syscall.ECONNRESET} {
			t.Run(protocol+"/"+fmt.Sprint(nativeErr), func(t *testing.T) {
				observations := dnsstats.New()
				ctx, cancel := context.WithCancel(dnsstats.WithManager(context.Background(), observations))
				defer cancel()
				query := routingTestQuery("late-io.example", 73)
				boundary := newDNSReadBoundary(t, nativeErr)
				base := newRoutingTestOutbound("io-leaf")
				outbound := &dnsCancelOutbound{routingTestOutbound: base}
				var dc dnsClient
				if protocol == "udp" {
					packet := &routingTestPacketConn{outbound: base, packets: make(chan routingTestPacket, 1), closed: make(chan struct{})}
					outbound.packet = &dnsBoundaryPacketConn{PacketConn: packet, boundary: boundary}
					dc = newClient("192.0.2.53:1053", nil, protocol, map[string]string{"disable-reuse": "true"}, outbound, "")
				} else {
					conn, server := net.Pipe()
					wire, err := routingTestAnswer(query, false).Pack()
					if err != nil {
						t.Fatal(err)
					}
					outbound.tcp = &dnsBoundaryConn{Conn: conn, boundary: boundary, remaining: len(wire) + 2}
					serverDone := make(chan struct{})
					t.Cleanup(func() { _ = conn.Close(); _ = server.Close(); <-serverDone })
					go func() {
						defer close(serverDone)
						defer server.Close()
						stream := &D.Conn{Conn: server}
						request, err := stream.ReadMsg()
						if err == nil {
							_ = stream.WriteMsg(routingTestAnswer(request, false))
						}
					}()
					if protocol == "tls" {
						client := newDoTClient("192.0.2.53:853", nil, nil, nil, "")
						// A pooled DoT socket exposes decrypted DNS frames. Real TLS
						// handshakes are covered by encrypted_routing_test.go.
						client.connections.PushBack(outbound.tcp)
						dc = dedicatedDNSDoTTestClient{client}
					} else {
						dc = newClient("192.0.2.53:1053", nil, protocol, map[string]string{"disable-reuse": "true"}, outbound, "")
					}
				}
				t.Cleanup(func() { closeNativeClient(dc) })
				result := make(chan dnsCancelExchangeResult, 1)
				go func() {
					message, err := dedicatedDNSExchange(dc, ctx, query)
					result <- dnsCancelExchangeResult{message, err}
				}()
				waitDNSCancelSignal(t, boundary.acquired, "socket did not obtain a reply/native failure")
				cancel()
				waitDNSCancelSignal(t, boundary.closed, "cancellation did not interrupt the owned socket")
				boundary.unblock()
				got := waitDNSCancelResult(t, result)
				if nativeErr == nil {
					if got.err != nil || got.message == nil || got.message.Id != query.Id || len(got.message.Answer) != 1 {
						t.Fatalf("late cancel discarded completed %s reply: %+v", protocol, got)
					}
				} else if !errors.Is(got.err, nativeErr) || errors.Is(got.err, context.Canceled) {
					t.Fatalf("late cancel replaced native %s I/O failure: %v", protocol, got.err)
				}
				stats := observations.Upstreams().Items[0]
				if stats.Canceled != 0 || (nativeErr == nil && stats.Successes != 1) || (nativeErr != nil && stats.Errors != 1) {
					t.Fatalf("completed I/O misclassified: %+v", stats)
				}
			})
		}
	}
}

func TestDNSClientCancellationClosesTCPFallback(t *testing.T) {
	observations := dnsstats.New()
	ctx, cancel := context.WithCancel(dnsstats.WithManager(context.Background(), observations))
	defer cancel()
	base := newRoutingTestOutbound("fallback-leaf")
	base.truncate = true
	conn, server := net.Pipe()
	boundary := newDNSReadBoundary(t, nil)
	outbound := &dnsCancelOutbound{routingTestOutbound: base, tcp: &dnsBoundaryConn{Conn: conn, boundary: boundary, remaining: 1}}
	dc := newClient("192.0.2.53:1053", nil, "udp", map[string]string{"disable-reuse": "true"}, outbound, "")
	t.Cleanup(func() { closeNativeClient(dc); _ = conn.Close(); _ = server.Close() })
	queryRead, serverDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(serverDone)
		stream := &D.Conn{Conn: server}
		if _, err := stream.ReadMsg(); err == nil {
			close(queryRead)
			_, _ = server.Read(make([]byte, 1))
		}
	}()
	result := make(chan dnsCancelExchangeResult, 1)
	go func() {
		message, err := dc.ExchangeContext(ctx, routingTestQuery("fallback-cancel.example", 74))
		result <- dnsCancelExchangeResult{message, err}
	}()
	waitDNSCancelSignal(t, queryRead, "truncated UDP reply did not start a TCP retry")
	cancel()
	waitDNSCancelSignal(t, boundary.closed, "cancellation closed only UDP and left the active TCP retry alive")
	got := waitDNSCancelResult(t, result)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("TCP fallback interruption lost its cause: %v", got.err)
	}
	waitDNSCancelSignal(t, serverDone, "TCP fallback server remained blocked after cancel")
	stats := observations.Upstreams().Items[0]
	if stats.Attempts != 1 || stats.Canceled != 1 || stats.Errors != 0 {
		t.Fatalf("TCP fallback cancellation counted as a separate attempt/failure: %+v", stats)
	}
}

func TestDNSDoTCompletedReplyBeforePoolReturn(t *testing.T) {
	observations := dnsstats.New()
	ctx, cancel := context.WithCancel(dnsstats.WithManager(context.Background(), observations))
	defer cancel()
	query := routingTestQuery("pool-return.example", 75)
	wire, err := routingTestAnswer(query, false).Pack()
	if err != nil {
		t.Fatal(err)
	}
	boundary := newDNSReadBoundary(t, nil)
	conn, server := net.Pipe()
	dc := newDoTClient("192.0.2.53:853", nil, nil, nil, "")
	dc.connections.PushBack(&dnsBoundaryConn{Conn: conn, boundary: boundary, remaining: len(wire) + 2})
	serverDone := make(chan struct{})
	t.Cleanup(func() { dc.Close(); conn.Close(); server.Close(); <-serverDone })
	go func() {
		defer close(serverDone)
		stream := &D.Conn{Conn: server}
		if query, err := stream.ReadMsg(); err == nil {
			_ = stream.WriteMsg(routingTestAnswer(query, false))
		}
	}()
	result := make(chan dnsCancelExchangeResult, 1)
	go func() {
		message, err := dedicatedDNSExchange(dc, ctx, query)
		result <- dnsCancelExchangeResult{message, err}
	}()
	waitDNSCancelSignal(t, boundary.acquired, "DoT reply did not arrive")
	// The socket was taken out of the pool before reading. Hold its return
	// lock across actual exchange completion and a later cancellation.
	dc.access.Lock()
	locked := true
	defer func() {
		if locked {
			dc.access.Unlock()
		}
	}()
	boundary.unblock()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for observations.Upstreams().Items[0].Successes != 1 {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("completed DoT I/O was not observed before the pool return")
		}
	}
	cancel()
	dc.access.Unlock()
	locked = false
	got := waitDNSCancelResult(t, result)
	if got.err != nil || got.message == nil || got.message.Id != query.Id {
		t.Fatalf("late cancel while returning the socket discarded the reply: %+v", got)
	}
	stats := observations.Upstreams().Items[0]
	if stats.Successes != 1 || stats.Canceled != 0 || stats.Errors != 0 {
		t.Fatalf("pool return changed a completed exchange: %+v", stats)
	}
}

type dnsCleanupBoundaryConn struct {
	net.Conn
	err     error
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *dnsCleanupBoundaryConn) Read([]byte) (int, error) { return 0, c.err }
func (c *dnsCleanupBoundaryConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.started); <-c.release })
	return err
}

func TestDNSDoTLateCancellationDuringFailedSocketCleanup(t *testing.T) {
	for _, nativeErr := range []error{io.EOF, syscall.ECONNRESET} {
		t.Run(fmt.Sprint(nativeErr), func(t *testing.T) {
			observations := dnsstats.New()
			ctx, cancel := context.WithCancel(dnsstats.WithManager(context.Background(), observations))
			defer cancel()
			conn, server := net.Pipe()
			wrapped := &dnsCleanupBoundaryConn{Conn: conn, err: nativeErr, started: make(chan struct{}), release: make(chan struct{})}
			var once sync.Once
			unblock := func() { once.Do(func() { close(wrapped.release) }) }
			defer unblock()
			dc := newDoTClient("192.0.2.53:853", nil, nil, nil, "")
			dc.connections.PushBack(wrapped)
			serverDone := make(chan struct{})
			t.Cleanup(func() { dc.Close(); conn.Close(); server.Close(); <-serverDone })
			go func() {
				defer close(serverDone)
				_, _ = (&D.Conn{Conn: server}).ReadMsg()
			}()
			result := make(chan dnsCancelExchangeResult, 1)
			go func() {
				message, err := dc.exchangeDedicated(ctx, routingTestQuery("failed-cleanup.example", 76))
				result <- dnsCancelExchangeResult{message, err}
			}()
			// ExchangeWithConn has already returned its real failure. Cleanup
			// blocks before the old worker could publish that result to ch.
			waitDNSCancelSignal(t, wrapped.started, "DoT socket failure did not reach cleanup")
			cancel()
			unblock()
			got := waitDNSCancelResult(t, result)
			if !errors.Is(got.err, nativeErr) || errors.Is(got.err, context.Canceled) {
				t.Fatalf("late cancellation hid the completed failure: %v", got.err)
			}
			stats := observations.Upstreams().Items[0]
			if stats.Errors != 1 || stats.Canceled != 0 {
				t.Fatalf("completed failure miscounted as cancellation: %+v", stats)
			}
		})
	}
}
