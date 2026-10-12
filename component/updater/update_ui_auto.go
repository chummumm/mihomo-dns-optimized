package updater

import (
	"context"
	"errors"
	"os"
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
	phase    atomic.Uint32
	// A new activation owns initialization itself. An interval-only policy
	// change may claim one separate empty-directory bootstrap, but an ordinary
	// reload must never enqueue another one for the same job.
	initialDownloadClaimed atomic.Bool
}

// Empty-directory initialization also survives same-source reloads. Its
// lifetime belongs to the source, so it remains available with auto-update off.
type uiInitialDownload struct {
	key  string
	ctx  context.Context
	done chan struct{}
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

func (u *UIUpdater) AutomaticSchedule() (time.Time, string) {
	if job := u.automatic; job != nil {
		return automaticSchedule(job.ctx, &job.next, &job.phase)
	}
	return time.Time{}, ""
}

// Called only by SetUIUpdater while uiActivation is held. The returned function
// starts background work after the immutable updater has been published.
func (u *UIUpdater) configureAutomaticUpdates(previous *UIUpdater) func() {
	if previous != nil && previous.automatic != nil {
		if previous.key() == u.key() && previous.AutoUpdate() == u.AutoUpdate() &&
			previous.UpdateInterval() == u.UpdateInterval() && previous.automatic.ctx.Err() == nil {
			u.automatic = previous.automatic
			return nil
		}
		previous.automatic.cancel()
	}
	if !u.AutoUpdate() || u.externalUIPath == "" || !u.sourceValid() {
		return nil
	}
	immediate := previous == nil || !previous.AutoUpdate() || previous.automatic == nil ||
		previous.key() != u.key() || !previous.active()
	ctx, cancel := context.WithCancel(u.activeCtx)
	interval := time.Duration(u.UpdateInterval()) * time.Hour
	job := &uiAutomaticUpdate{ctx: ctx, cancel: cancel, done: make(chan struct{}), interval: interval}
	job.initialDownloadClaimed.Store(immediate)
	next := time.Now().Add(interval)
	job.next.Store(&next)
	if immediate {
		job.phase.Store(automaticWaiting)
	}
	u.automatic = job
	var ticks <-chan time.Time
	var stop func()
	if u.newTicker != nil {
		ticks, stop = u.newTicker(interval)
	} else {
		ticker := time.NewTicker(interval)
		ticks, stop = ticker.C, ticker.Stop
	}
	return func() { go u.runAutomaticUpdates(job, ticks, stop, immediate) }
}

func (u *UIUpdater) runAutomaticUpdates(job *uiAutomaticUpdate, ticks <-chan time.Time, stop func(), immediate bool) {
	defer close(job.done)
	defer stop()
	if immediate {
		u.runAutomaticCheck(job, true)
	}
	for {
		select {
		case <-job.ctx.Done():
			return
		case tick, ok := <-ticks:
			if !ok || job.ctx.Err() != nil {
				return
			}
			next := tick.Add(job.interval)
			job.next.Store(&next)
			u.runAutomaticCheck(job, false)
		}
	}
}

func (u *UIUpdater) runAutomaticCheck(job *uiAutomaticUpdate, waitForOperation bool) {
	if job.ctx.Err() != nil {
		return
	}
	job.phase.Store(automaticWaiting)
	finishedPhase := automaticScheduled
	defer func() { job.phase.Store(finishedPhase) }()
	action := func() error {
		if err := u.readiness.wait(job.ctx); err != nil {
			return err
		}
		job.phase.Store(automaticChecking)
		return u.automaticUpdate(job.ctx)
	}
	var err error
	if waitForOperation {
		err = u.state().operation.runWhenAvailable(job.ctx, ErrUIUpdateBusy, action)
	} else {
		err = action()
	}
	if err != nil && job.ctx.Err() == nil && !errors.Is(err, ErrUIUpdateBusy) {
		finishedPhase = automaticRetrying
		log.Warnln("Automatic external UI update failed: %s", err)
	}
}

func (u *UIUpdater) automaticUpdate(ctx context.Context) error {
	// Retry initial installation after an earlier bootstrap network failure.
	// Existing files still require a strictly newer, recognizable version.
	entries, readErr := os.ReadDir(u.externalUIPath)
	if os.IsNotExist(readErr) || (readErr == nil && len(entries) == 0) {
		return u.installUIContext(ctx, false, true)
	}
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
