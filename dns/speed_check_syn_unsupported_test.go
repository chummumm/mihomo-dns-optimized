//go:build !linux

package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestTCPSYNUnsupportedPlatformIsExplicit(t *testing.T) {
	if err := ValidateSpeedCheckConfig(SpeedCheckConfig{Mode: []string{"tcp-syn:443"}}); err == nil || !strings.Contains(err.Error(), "requires Linux") {
		t.Fatalf("unsupported raw SYN configuration did not explain the platform requirement: %v", err)
	}
	if _, err := probeTCPSYN(context.Background(), netip.MustParseAddr("127.0.0.1"), 443); !errors.Is(err, errTCPSYNUnavailable) {
		t.Fatal("unsupported platform silently substituted a TCP connect probe")
	}
}

func TestTCPAutoUnsupportedPlatformUsesNativeTCP(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	target := listener.Addr().(*net.TCPAddr).AddrPort()
	rtt, err := probeDirectIP(ctx, target.Addr(), speedCheckMode{port: target.Port(), tcpMode: tcpProbeAuto})
	if err != nil || rtt < 0 {
		t.Fatalf("automatic mode failed native TCP fallback: rtt=%v, error=%v", rtt, err)
	}
	if err := listener.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("automatic mode did not establish the native TCP measurement: %v", err)
	}
	_ = conn.Close()
}
