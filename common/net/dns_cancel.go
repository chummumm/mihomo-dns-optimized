package net

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"

	"github.com/metacubex/mihomo/common/contextutils"
)

// DNSCancelGuard records whether this exchange actually interrupted its I/O.
// Merely observing ctx.Err after an I/O operation returned would also relabel
// a valid response or an unrelated remote failure as a cancellation.
type DNSCancelGuard struct {
	ctx   context.Context
	fired atomic.Bool
	stop  func() bool
}

func NewDNSCancelGuard(ctx context.Context, interrupt func()) *DNSCancelGuard {
	g := &DNSCancelGuard{ctx: ctx}
	g.stop = contextutils.AfterFunc(ctx, func() {
		g.fired.Store(true)
		interrupt()
	})
	return g
}

func (g *DNSCancelGuard) Stop() { g.stop() }

// Error only annotates an error which the protocol identifies as its local
// interruption result. Call it at the I/O boundary, before any cleanup cancel.
// Successes, EOF, remote resets and ordinary socket timeouts remain untouched.
func (g *DNSCancelGuard) Error(err error, interrupted bool) error {
	if err == nil || !interrupted || !g.fired.Load() {
		return err
	}
	if cause := g.ctx.Err(); cause != nil {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

func DNSClosedIO(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}
