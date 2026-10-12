package dialer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/resolver"
)

type orderedTestResolver struct {
	resolver.Resolver
	ips     []netip.Addr
	ordered bool
}

func (r *orderedTestResolver) Invalid() bool       { return true }
func (r *orderedTestResolver) PreferIPOrder() bool { return r.ordered }
func (r *orderedTestResolver) LookupIPv4(context.Context, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), r.ips...), nil
}

func TestDNSOrderedDialNumericIPv6RespectsDisabledFamily(t *testing.T) {
	old := resolver.DisableIPv6
	resolver.DisableIPv6 = true
	t.Cleanup(func() { resolver.DisableIPv6 = old })
	for _, network := range []string{"tcp", "tcp6", "udp", "udp6"} {
		t.Run(network, func(t *testing.T) {
			var calls atomic.Int32
			conn, err := DialContext(context.Background(), network, "[2001:db8::7]:443",
				WithNetDialer(NetDialerFunc(func(context.Context, string, string) (net.Conn, error) {
					calls.Add(1)
					return nil, errors.New("disabled IPv6 must not be dialed")
				})))
			if conn != nil {
				_ = conn.Close()
			}
			if !errors.Is(err, resolver.ErrIPv6Disabled) || calls.Load() != 0 {
				t.Fatalf("numeric destination bypassed disabled IPv6: calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}

func TestDNSOrderedDialHonorsPreferenceWithTCPConcurrent(t *testing.T) {
	old := GetTcpConcurrent()
	SetTcpConcurrent(true)
	t.Cleanup(func() { SetTcpConcurrent(old) })
	r := &orderedTestResolver{ips: []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
	}, ordered: true}
	var calls atomic.Int32
	client, peer := net.Pipe()
	defer peer.Close()
	conn, err := DialContext(context.Background(), "tcp4", "ordered.example:443",
		WithResolver(r), WithPreferResolverOrder(true),
		WithNetDialer(NetDialerFunc(func(_ context.Context, _, address string) (net.Conn, error) {
			calls.Add(1)
			if address != "192.0.2.1:443" {
				return nil, errors.New("raced lower-ranked address")
			}
			return client, nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if calls.Load() != 1 || conn != client {
		t.Fatalf("DNS preference was discarded: calls=%d conn=%v", calls.Load(), conn)
	}
}

func TestDNSOrderedDialFallsBackOnFailureAndStall(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		name := "failure"
		if stalled {
			name = "stall"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			firstStarted := make(chan struct{})
			firstFinished := make(chan struct{})
			client, peer := net.Pipe()
			defer peer.Close()
			var calls atomic.Int32
			start := time.Now()
			conn, err := orderedDialContext(ctx, "tcp4", []netip.Addr{
				netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
			}, "443", applyOptions(WithNetDialer(NetDialerFunc(func(ctx context.Context, _, address string) (net.Conn, error) {
				calls.Add(1)
				if address == "192.0.2.1:443" {
					close(firstStarted)
					defer close(firstFinished)
					if stalled {
						<-ctx.Done()
					}
					return nil, errors.New("first address unavailable")
				}
				select {
				case <-firstStarted:
				default:
					return nil, errors.New("fallback started before preferred address")
				}
				if stalled && time.Since(start) < dualStackFallbackTimeout {
					return nil, errors.New("fallback bypassed preference delay")
				}
				return client, nil
			}))))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if calls.Load() != 2 {
				t.Fatalf("unexpected attempts: %d", calls.Load())
			}
			select {
			case <-firstFinished:
			case <-ctx.Done():
				t.Fatal("losing connection was not canceled")
			}
		})
	}
}

func TestDNSOrderedDialClosesLateSuccessAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	client, peer := net.Pipe()
	defer peer.Close()
	go func() {
		_, err := orderedDialContext(ctx, "tcp4", []netip.Addr{netip.MustParseAddr("192.0.2.1")}, "443",
			applyOptions(WithNetDialer(NetDialerFunc(func(context.Context, string, string) (net.Conn, error) {
				close(started)
				<-release // emulate a dialer that completes after cancellation
				return client, nil
			}))))
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled ordered dial did not return")
	}
	close(release)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("late successful connection was leaked")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("late successful connection was not closed")
	}
}

func TestDNSOrderedDialDoesNotChangeOrdinaryConcurrency(t *testing.T) {
	old := GetTcpConcurrent()
	SetTcpConcurrent(true)
	t.Cleanup(func() { SetTcpConcurrent(old) })
	for _, test := range []struct {
		name               string
		requested, ordered bool
	}{
		{"ordinary consumer", false, true}, {"optimizer disabled", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			r := &orderedTestResolver{ips: []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, ordered: test.ordered}
			both := make(chan struct{})
			var calls atomic.Int32
			_, err := DialContext(ctx, "tcp4", "ordinary.example:443", WithResolver(r), WithPreferResolverOrder(test.requested),
				WithNetDialer(NetDialerFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
					if calls.Add(1) == 2 {
						close(both)
					}
					select {
					case <-both:
						return nil, errors.New("expected test refusal")
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				})))
			if err == nil || calls.Load() != 2 {
				t.Fatalf("ordinary dialing changed: %d %v", calls.Load(), err)
			}
			if ctx.Err() != nil {
				t.Fatal("ordinary concurrent dialing became serial")
			}
		})
	}
}
