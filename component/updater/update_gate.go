package updater

import (
	"context"
	"errors"
	"sync"
	"time"
)

const updateOperationWaitLimit = 90 * time.Second

// updateOperationGate serializes updater operations and lets selected background
// callers wait for an existing operation without polling or an extra goroutine.
// Its zero value is ready for use. Like sync.Mutex, it must not be copied after use.
type updateOperationGate struct {
	mu       sync.Mutex
	held     bool
	released chan struct{}
}

func (g *updateOperationGate) TryLock() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held {
		return false
	}
	g.held = true
	g.released = make(chan struct{})
	return true
}

func (g *updateOperationGate) Lock() {
	for {
		g.mu.Lock()
		if !g.held {
			g.held = true
			g.released = make(chan struct{})
			g.mu.Unlock()
			return
		}
		released := g.released
		g.mu.Unlock()
		<-released
	}
}

func (g *updateOperationGate) Unlock() {
	g.mu.Lock()
	if !g.held {
		g.mu.Unlock()
		panic("updater: unlock of unlocked operation gate")
	}
	g.held = false
	close(g.released)
	g.released = nil
	g.mu.Unlock()
}

// WaitIdle waits for the current holder to finish. It does not acquire or
// reserve the gate, so callers still need to handle contention on their retry.
func (g *updateOperationGate) WaitIdle(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	if !g.held {
		g.mu.Unlock()
		return ctx.Err()
	}
	released := g.released
	g.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-released:
		// A simultaneous cancellation must not authorize a new operation.
		return ctx.Err()
	}
}

// runWhenAvailable is used by activation checks, initial UI installs and
// coordinated manual tasks. Standalone periodic/compatibility operations keep
// their nonblocking TryLock so they never build a background update queue.
// Only contention on this gate is retryable; other action results return as-is.
func (g *updateOperationGate) runWhenAvailable(ctx context.Context, busyErr error, action func() error) error {
	waitCtx := ctx
	var cancel context.CancelFunc
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	for {
		if err := waitCtx.Err(); err != nil {
			return err
		}
		err := action()
		if err == nil || !errors.Is(err, busyErr) {
			return err
		}
		if cancel == nil {
			// Readiness may follow several configuration reloads before the
			// first attempt. Only actual contention starts the waiting budget.
			waitCtx, cancel = context.WithTimeout(ctx, updateOperationWaitLimit)
		}
		if err := g.WaitIdle(waitCtx); err != nil {
			return err
		}
		// The closure keeps its original context and per-download limits. The
		// bounded context above limits waiting, never an operation in progress.
	}
}
