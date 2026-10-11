package updater

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/metacubex/mihomo/log"
)

const defaultUIUpdateInterval = 24

// The active updater owns exactly one scheduler. Same-policy reloads share this
// object and its original timer; policy changes cancel it without waiting for
// network I/O. Its context is also checked under the installation commit lock.
type uiAutomaticUpdate struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	interval time.Duration
	next     atomic.Pointer[time.Time]
}

func (u *UIUpdater) AutoUpdate() bool { return u.autoUpdateUI }

// UpdateInterval returns the configured automatic-check interval in hours.
func (u *UIUpdater) UpdateInterval() int {
	if u.updateInterval <= 0 {
		return defaultUIUpdateInterval
	}
	return u.updateInterval
}

func (u *UIUpdater) nextAutomaticCheck() string {
	if u.automatic != nil && u.automatic.ctx.Err() == nil {
		if next := u.automatic.next.Load(); next != nil {
			return next.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// Called only by SetUIUpdater while uiActivation is held, before u is published.
func (u *UIUpdater) configureAutomaticUpdates(previous *UIUpdater) {
	if previous != nil && previous.automatic != nil {
		if previous.key() == u.key() && previous.AutoUpdate() == u.AutoUpdate() &&
			previous.UpdateInterval() == u.UpdateInterval() && previous.automatic.ctx.Err() == nil {
			u.automatic = previous.automatic
			return
		}
		previous.automatic.cancel()
	}
	if !u.AutoUpdate() || u.externalUIPath == "" || !u.sourceValid() {
		return
	}
	ctx, cancel := context.WithCancel(u.activeCtx)
	interval := time.Duration(u.UpdateInterval()) * time.Hour
	job := &uiAutomaticUpdate{ctx: ctx, cancel: cancel, done: make(chan struct{}), interval: interval}
	next := time.Now().Add(interval)
	job.next.Store(&next)
	u.automatic = job
	var ticks <-chan time.Time
	var stop func()
	if u.newTicker != nil {
		ticks, stop = u.newTicker(interval)
	} else {
		ticker := time.NewTicker(interval)
		ticks, stop = ticker.C, ticker.Stop
	}
	go u.runAutomaticUpdates(job, ticks, stop)
}

func (u *UIUpdater) runAutomaticUpdates(job *uiAutomaticUpdate, ticks <-chan time.Time, stop func()) {
	defer close(job.done)
	defer stop()
	// The first check waits one configured interval. Existing empty-directory
	// bootstrap remains immediate and cannot cause a duplicate first download.
	for {
		select {
		case <-job.ctx.Done():
			return
		case tick := <-ticks:
			if job.ctx.Err() != nil {
				return
			}
			next := tick.Add(job.interval)
			job.next.Store(&next)
			if err := u.automaticUpdate(job.ctx); err != nil && job.ctx.Err() == nil && !errors.Is(err, ErrUIUpdateBusy) {
				log.Warnln("Automatic external UI update failed: %s", err)
			}
		}
	}
}

func (u *UIUpdater) automaticUpdate(ctx context.Context) error {
	latest, err := u.checkWithLifetime(ctx, ctx)
	if err != nil {
		return err
	}
	if !u.updateAvailable(latest) {
		return nil
	}
	// Reuse the validated archive, the process-wide operation gate, and the
	// atomic directory transaction used by the manual update endpoint. A 304
	// with matching installed files never needs a second full download.
	return u.downloadUIContext(ctx, true)
}
