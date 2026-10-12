package geodata

import (
	"context"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

type initializationContext struct {
	ctx    context.Context
	commit func(func() error) error
	mu     sync.Mutex
	err    error
}

var initializationGate = semaphore.NewWeighted(1)
var initializationScope atomic.Pointer[initializationContext]

// WithInitializationContext serializes configuration parsing and live
// classical-provider parsing around the temporary GeoData settings they use.
// Scopes must not nest. Network reads of provider files stay outside this
// scope; any newly required GeoData download inherits its caller's lifetime.
func WithInitializationContext(ctx context.Context, commit func(func() error) error, action func() error) error {
	if err := initializationGate.Acquire(ctx, 1); err != nil {
		return err
	}
	defer initializationGate.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	scope := &initializationContext{ctx: ctx, commit: commit}
	initializationScope.Store(scope)
	defer initializationScope.Store(nil)
	if err := action(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Classical rule parsing historically skips invalid rules. A failed GeoData
	// initialization is different: publishing the remaining subset would silently
	// remove working policy. Preserve the old provider instead.
	scope.mu.Lock()
	defer scope.mu.Unlock()
	return scope.err
}

func currentInitializationContext() *initializationContext {
	if scope := initializationScope.Load(); scope != nil {
		return scope
	}
	return &initializationContext{ctx: context.Background()}
}

func recordInitializationError(err error) {
	if err == nil {
		return
	}
	if scope := initializationScope.Load(); scope != nil {
		scope.mu.Lock()
		if scope.err == nil {
			scope.err = err
		}
		scope.mu.Unlock()
	}
}
