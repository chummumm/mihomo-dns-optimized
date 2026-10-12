//go:build !linux

package dns

import (
	"context"
	"fmt"
	"net/netip"
	"runtime"
	"time"

	"github.com/metacubex/mihomo/component/dialer"
)

func validateTCPSYNPlatform() error {
	return fmt.Errorf("dns.speed-check-mode: tcp-syn requires Linux raw TCP sockets; %s is unsupported (use tcp:<port>)", runtime.GOOS)
}

func probeTCPSYN(context.Context, netip.Addr, uint16, ...dialer.Option) (time.Duration, error) {
	return 0, validateTCPSYNPlatform()
}
