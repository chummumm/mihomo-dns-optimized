//go:build linux

package dns

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/resolver"
	"golang.org/x/sys/unix"
)

// The required script runs this once with raw sockets and once in a separate
// process whose CAP_NET_RAW has been removed. Its peer forbids full handshakes
// in the first run and requires one per family in the second. Loopback cannot
// replace this fixture because checksum offload can hide real SYN-ACK packets.
func TestTCPAutoKernelSelectsAvailableMethod(t *testing.T) {
	method := os.Getenv("MIHOMO_TCP_AUTO_EXPECT")
	if method == "" && os.Getenv("MIHOMO_TCP_SYN_INTEGRATION") != "1" {
		t.Skip("requires scripts/test-dns-tcp-syn.sh isolated wire fixture")
	}
	if method != "raw" && method != "tcp" {
		t.Fatalf("required fixture must select raw or tcp, got %q", method)
	}
	interfaceName := os.Getenv("MIHOMO_TCP_SYN_TEST_INTERFACE")
	if interfaceName == "" {
		t.Fatal("required automatic probe fixture has no interface")
	}
	previousInterface, previousIPv6 := dialer.DefaultInterface.Load(), resolver.DisableIPv6
	dialer.DefaultInterface.Store(interfaceName)
	resolver.DisableIPv6 = false
	t.Cleanup(func() {
		dialer.DefaultInterface.Store(previousInterface)
		resolver.DisableIPv6 = previousIPv6
	})

	for _, family := range []struct {
		name, variable string
		socketFamily   int
		ipv6           bool
	}{
		{name: "IPv4", variable: "MIHOMO_TCP_SYN_TEST_IPV4", socketFamily: unix.AF_INET},
		{name: "IPv6", variable: "MIHOMO_TCP_SYN_TEST_IPV6", socketFamily: unix.AF_INET6, ipv6: true},
	} {
		t.Run(family.name, func(t *testing.T) {
			fd, err := unix.Socket(family.socketFamily, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
			if err == nil {
				_ = unix.Close(fd)
			}
			if method == "raw" && err != nil {
				t.Fatalf("required raw-capable process cannot open a %s raw TCP socket: %v", family.name, err)
			}
			if method == "tcp" && !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EACCES) {
				t.Fatalf("fallback process must actually lack raw permission for %s: %v", family.name, err)
			}
			for _, open := range []bool{true, false} {
				name, variable := "closed", family.variable+"_CLOSED"
				if open {
					name, variable = "listening", family.variable
				}
				t.Run(name, func(t *testing.T) {
					target, err := netip.ParseAddrPort(os.Getenv(variable))
					if err != nil || target.Port() == 0 || target.Addr().Is6() != family.ipv6 {
						t.Fatalf("invalid required fixture target %s: %v", variable, err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if method == "tcp" && open {
						// Explicit SYN must still fail instead of quietly changing
						// its method when the same capability is unavailable.
						_, err := probeDirectIP(ctx, target.Addr(), speedCheckMode{port: target.Port(), tcpMode: tcpProbeSYN})
						if !errors.Is(err, errTCPSYNUnavailable) {
							t.Fatalf("explicit SYN did not retain its unavailable error: %v", err)
						}
					}
					rtt, err := probeDirectIP(ctx, target.Addr(), speedCheckMode{port: target.Port(), tcpMode: tcpProbeAuto})
					if err != nil || rtt < 0 {
						t.Fatalf("automatic %s probe did not accept the kernel response: RTT=%v error=%v", method, rtt, err)
					}
					t.Logf("automatic %s probe accepted %s: RTT=%v", method, target, rtt)
				})
			}
		})
	}
}
