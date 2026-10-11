package updater

import (
	"context"
	"sync"
)

// updateReadiness follows the most recently applied configuration. A retained
// scheduler must not wake on an older configuration's completed reset while a
// newer reset is still pending. Its zero value is ready for standalone users.
type updateReadiness struct {
	mu      sync.Mutex
	ready   <-chan struct{}
	changed chan struct{}
}

func (r *updateReadiness) set(ready <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready == ready {
		return
	}
	r.ready = ready
	if r.changed != nil {
		close(r.changed)
	}
	r.changed = make(chan struct{})
}

func (r *updateReadiness) wait(ctx context.Context) error {
	if r == nil {
		return ctx.Err()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		ready, changed := r.ready, r.changed
		r.mu.Unlock()
		if ready == nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
			continue
		case <-ready:
			r.mu.Lock()
			current := r.ready == ready
			r.mu.Unlock()
			if current {
				return ctx.Err()
			}
		}
	}
}
