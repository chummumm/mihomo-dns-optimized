package dialer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"
)

// orderedDialContext gives the resolver's first address a head start. Failure
// advances immediately; a pending connection allows the next address after the
// usual fallback delay. The successful socket is reused as the real connection,
// and canceled or late losing attempts cannot leak sockets.
func orderedDialContext(ctx context.Context, network string, ips []netip.Addr, port string, opt option) (net.Conn, error) {
	if len(ips) == 0 {
		return nil, ErrorNoIpAddress
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan dialResult)
	returned := make(chan struct{})
	defer close(returned)
	active, next := 0, 0
	start := func() {
		ip := ips[next]
		next++
		active++
		go func() {
			conn, err := dialContext(ctx, network, ip, port, opt)
			select {
			case results <- dialResult{Conn: conn, error: err}:
			case <-returned:
				if conn != nil {
					_ = conn.Close()
				}
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start()
	timer := time.NewTimer(dualStackFallbackTimeout)
	defer timer.Stop()
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if next < len(ips) {
			timer.Reset(dualStackFallbackTimeout)
		}
	}
	var errs []error
	for active > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			if next < len(ips) {
				start()
				resetTimer()
			}
		case result := <-results:
			active--
			if err := ctx.Err(); err != nil {
				if result.Conn != nil {
					_ = result.Conn.Close()
				}
				return nil, err
			}
			if result.error == nil {
				return result.Conn, nil
			}
			if result.Conn != nil {
				_ = result.Conn.Close()
			}
			errs = append(errs, result.error)
			if next < len(ips) {
				start()
				resetTimer()
			}
		}
	}
	return nil, errors.Join(errs...)
}
