//go:build linux

package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/resolver"
	"golang.org/x/sys/unix"
)

func tcpSYNUnavailableInTest(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("MIHOMO_TCP_SYN_INTEGRATION") == "1" {
		t.Fatalf("required TCP SYN integration environment is unavailable: %v", err)
	}
	t.Skipf("TCP SYN integration environment is unavailable: %v", err)
}

func TestTCPSYNKernelRepliesWithoutOpeningTCPConnection(t *testing.T) {
	previousIPv6 := resolver.DisableIPv6
	resolver.DisableIPv6 = false
	defer func() { resolver.DisableIPv6 = previousIPv6 }()
	interfaceName := os.Getenv("MIHOMO_TCP_SYN_TEST_INTERFACE")
	if interfaceName == "" {
		interfaceName = "lo"
	}
	for _, family := range []struct{ address, variable string }{
		{address: "127.0.0.1:0", variable: "MIHOMO_TCP_SYN_TEST_IPV4"},
		{address: "[::1]:0", variable: "MIHOMO_TCP_SYN_TEST_IPV6"},
	} {
		for _, open := range []bool{true, false} {
			name := family.address + "/closed"
			targetVariable := family.variable + "_CLOSED"
			if open {
				name = family.address + "/listening"
				targetVariable = family.variable
			}
			t.Run(name, func(t *testing.T) {
				var target netip.AddrPort
				var listener net.Listener
				if external := os.Getenv(targetVariable); external != "" {
					var err error
					target, err = netip.ParseAddrPort(external)
					if err != nil {
						t.Fatalf("invalid SYN fixture target %s: %v", targetVariable, err)
					}
				} else {
					var err error
					listener, err = net.Listen("tcp", family.address)
					if err != nil {
						if errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EADDRNOTAVAIL) {
							tcpSYNUnavailableInTest(t, err)
						}
						t.Fatal(err)
					}
					defer listener.Close()
					target = listener.Addr().(*net.TCPAddr).AddrPort()
					if !open {
						_ = listener.Close()
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				rtt, err := probeTCPSYN(ctx, target.Addr(), target.Port(), dialer.WithInterface(interfaceName))
				if errors.Is(err, errTCPSYNUnavailable) {
					tcpSYNUnavailableInTest(t, err)
				}
				if err != nil || rtt < 0 {
					t.Fatalf("raw SYN did not accept matching SYN-ACK/RST-ACK: RTT=%v error=%v", rtt, err)
				}
				if open && listener != nil {
					_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
					connection, err := listener.Accept()
					if connection != nil {
						_ = connection.Close()
						t.Fatal("tcp-syn completed a TCP connection instead of probing a SYN")
					}
					if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
						t.Fatalf("unexpected listener result: %v", err)
					}
				}
			})
		}
	}
}

func TestTCPSYNPortReservationIsExclusiveAndReleased(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "::1"} {
		t.Run(address, func(t *testing.T) {
			ip := netip.MustParseAddr(address)
			fd, port, err := reserveTCPSYNPort(ip)
			if errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EADDRNOTAVAIL) {
				tcpSYNUnavailableInTest(t, err)
			}
			if err != nil {
				t.Fatal(err)
			}
			target := netip.AddrPortFrom(ip, port).String()
			listener, err := net.Listen("tcp", target)
			if listener != nil {
				_ = listener.Close()
			}
			if !errors.Is(err, unix.EADDRINUSE) {
				_ = unix.Close(fd)
				t.Fatalf("reserved source port could be allocated to another socket: %v", err)
			}
			if err := unix.Close(fd); err != nil {
				t.Fatal(err)
			}
			listener, err = net.Listen("tcp", target)
			if err != nil {
				t.Fatalf("source port was not released: %v", err)
			}
			_ = listener.Close()
		})
	}
}

func TestTCPSYNRejectsCanceledAndWrongFamilyProbes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := probeTCPSYN(ctx, netip.MustParseAddr("127.0.0.1"), 80); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled raw probe was started: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := probeTCPSYN(ctx, netip.MustParseAddr("127.0.0.1"), 80, dialer.WithOnlySingleStack(false)); err == nil {
		t.Fatal("raw probe ignored the DIRECT IPv6-only policy")
	}
}

func TestTCPSYNConfigurationUsesDistinctModesAndDefaultPort(t *testing.T) {
	checker, err := newDirectSpeedChecker(SpeedCheckConfig{Mode: []string{"tcp", "tcp:80", "tcp-syn", "tcp-syn:80", "tcp-syn:443", "ping"}})
	if err != nil {
		t.Fatal(err)
	}
	defer checker.pool.Close()
	defer checker.candidates.Close()
	want := []speedCheckMode{{port: 80}, {port: 80, syn: true}, {port: 443, syn: true}, {}}
	if len(checker.modes) != len(want) {
		t.Fatalf("mode count=%d, want %d", len(checker.modes), len(want))
	}
	for i := range want {
		if checker.modes[i] != want[i] {
			t.Fatalf("mode %d=%v, want %v", i, checker.modes[i], want[i])
		}
	}
	for _, modes := range [][]string{{"tcp-syn:0"}, {"tcp-syn:65536"}, {"tcp-syn:no-port"}, {"ping", "tcp:80", "tcp:443", "tcp-syn:80", "tcp-syn:443"}} {
		if err := ValidateSpeedCheckConfig(SpeedCheckConfig{Mode: modes}); err == nil {
			t.Fatalf("accepted invalid SYN configuration: %v", modes)
		}
	}
}

func TestTCPSYNSetupTimeoutDoesNotProveUnreachable(t *testing.T) {
	expired, stopExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stopExpired()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name string
		ctx  context.Context
		err  error
		want error
	}{
		{name: "context-deadline", ctx: context.Background(), err: context.DeadlineExceeded, want: errDNSProbeNotStarted},
		{name: "socket-deadline", ctx: context.Background(), err: &net.OpError{Op: "dial", Net: "udp", Err: os.ErrDeadlineExceeded}, want: errDNSProbeNotStarted},
		{name: "socket-timeout", ctx: context.Background(), err: &net.OpError{Op: "dial", Net: "udp", Err: unix.ETIMEDOUT}, want: errDNSProbeNotStarted},
		{name: "closed-during-setup-expiry", ctx: expired, err: net.ErrClosed, want: errDNSProbeNotStarted},
		{name: "expired-before-first-write", ctx: expired, want: errDNSProbeNotStarted},
		{name: "network-unreachable", ctx: expired, err: &net.OpError{Op: "dial", Net: "udp", Err: unix.ENETUNREACH}, want: unix.ENETUNREACH},
		{name: "host-unreachable", ctx: context.Background(), err: unix.EHOSTUNREACH, want: unix.EHOSTUNREACH},
		{name: "permission", ctx: context.Background(), err: unix.EPERM, want: unix.EPERM},
		{name: "canceled", ctx: canceled, err: context.DeadlineExceeded, want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := tcpSYNSetupError(test.ctx, test.err)
			if !errors.Is(err, test.want) {
				t.Fatalf("setup error=%v, want %v", err, test.want)
			}
			if !errors.Is(test.want, errDNSProbeNotStarted) {
				return
			}
			var networkError net.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
				t.Fatalf("unstarted setup leaked a network timeout: %v", err)
			}
			measurements := &dualStackMeasurements{rtts: make(map[netip.Addr]time.Duration)}
			ip := netip.MustParseAddr("192.0.2.1")
			measurements.observe(ip, 0, err)
			if measurements.failed[ip] {
				t.Fatal("pre-send setup failure became unreachable-family evidence")
			}
		})
	}
}

func TestTCPSYNSourceSetupExpiryNeedsNoRawSocket(t *testing.T) {
	var sourceSelections atomic.Int32
	selector := dialer.WithNetDialer(dialer.NetDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		sourceSelections.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "udp", Err: os.ErrDeadlineExceeded}
	}))
	_, err := probeTCPSYN(context.Background(), netip.MustParseAddr("127.0.0.1"), 80, selector)
	if !errors.Is(err, errDNSProbeNotStarted) || sourceSelections.Load() != 1 {
		t.Fatalf("source selection timeout was not classified before raw socket setup: selections=%d error=%v", sourceSelections.Load(), err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = probeTCPSYN(expired, netip.MustParseAddr("127.0.0.1"), 80, selector)
	if !errors.Is(err, errDNSProbeNotStarted) || sourceSelections.Load() != 1 {
		t.Fatalf("already expired probe entered source selection: selections=%d error=%v", sourceSelections.Load(), err)
	}
}
