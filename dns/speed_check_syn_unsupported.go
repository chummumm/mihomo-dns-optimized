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

func probeTCPSYN(ctx context.Context, _ netip.Addr, _ uint16, _ ...dialer.Option) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%w: %w", errTCPSYNUnavailable, validateTCPSYNPlatform())
}
