//go:build !linux

package dns

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

func TestTCPSYNUnsupportedPlatformIsExplicit(t *testing.T) {
	if err := ValidateSpeedCheckConfig(SpeedCheckConfig{Mode: []string{"tcp-syn:443"}}); err == nil || !strings.Contains(err.Error(), "requires Linux") {
		t.Fatalf("unsupported raw SYN configuration did not explain the platform requirement: %v", err)
	}
	if _, err := probeTCPSYN(context.Background(), netip.MustParseAddr("127.0.0.1"), 443); err == nil {
		t.Fatal("unsupported platform silently substituted a TCP connect probe")
	}
}
