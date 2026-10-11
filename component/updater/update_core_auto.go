package updater

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"time"

	"github.com/metacubex/mihomo/log"
)

const defaultCoreUpdateInterval = 24

// CoreAutomaticRestartHandler runs commit and the process restart within the
// same configuration transaction. It must not restart when commit returns an
// error. commit releases every updater lock before returning, so Shutdown can
// cancel schedulers without an update/configuration lock inversion.
type CoreAutomaticRestartHandler func(executable string, commit func() error) error

type coreAutomaticUpdate struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	interval time.Duration
	next     atomic.Pointer[time.Time]
	restart  CoreAutomaticRestartHandler
}

func (u *CoreUpdater) AutoUpdate() bool {
	enabled, _ := u.AutomaticPolicy()
	return enabled
}

// AutomaticPolicy reads the two YAML values as one consistent API snapshot.
func (u *CoreUpdater) AutomaticPolicy() (bool, int) {
	u.policyMu.Lock()
	defer u.policyMu.Unlock()
	return u.autoUpdate, u.updateIntervalLocked()
}

func (u *CoreUpdater) UpdateInterval() int {
	_, interval := u.AutomaticPolicy()
	return interval
}

func (u *CoreUpdater) updateIntervalLocked() int {
	if u.updateInterval <= 0 {
		return defaultCoreUpdateInterval
	}
	return u.updateInterval
}

func (u *CoreUpdater) NextCheck() string {
	u.policyMu.Lock()
	defer u.policyMu.Unlock()
	if job := u.automatic; job != nil && job.ctx.Err() == nil {
		if next := job.next.Load(); next != nil {
			return next.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// ConfigureAutoUpdate is nonblocking with respect to network I/O. An unchanged
// policy retains its original timer; disabling it or changing the interval
// cancels the old request and prevents it from committing later.
func (u *CoreUpdater) ConfigureAutoUpdate(enabled bool, intervalHours int) {
	if intervalHours <= 0 || intervalHours > int((1<<63-1)/time.Hour) {
		intervalHours = defaultCoreUpdateInterval
	}
	u.policyMu.Lock()
	defer u.policyMu.Unlock()
	if u.autoUpdate == enabled && u.updateIntervalLocked() == intervalHours &&
		(!enabled || (u.automatic != nil && u.automatic.ctx.Err() == nil)) {
		return
	}
	u.stopAutomaticUpdatesLocked()
	u.autoUpdate, u.updateInterval = enabled, intervalHours
	u.startAutomaticUpdatesLocked()
}

// SetAutomaticRestartHandler is registered during process initialization, not
// by an HTTP request. Embedded users leave it unset to avoid replacing their
// host executable. Changing the handler invalidates any in-flight auto job.
func (u *CoreUpdater) SetAutomaticRestartHandler(handler CoreAutomaticRestartHandler) {
	u.policyMu.Lock()
	defer u.policyMu.Unlock()
	u.stopAutomaticUpdatesLocked()
	u.restartHandler = handler
	u.startAutomaticUpdatesLocked()
}

func (u *CoreUpdater) startAutomaticUpdatesLocked() {
	if !u.autoUpdate || u.restartHandler == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	interval := time.Duration(u.updateIntervalLocked()) * time.Hour
	job := &coreAutomaticUpdate{
		ctx: ctx, cancel: cancel, done: make(chan struct{}), interval: interval,
		restart: u.restartHandler,
	}
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

func (u *CoreUpdater) stopAutomaticUpdatesLocked() {
	if u.automatic != nil {
		u.automatic.cancel()
	}
	u.automatic = nil
}

func (u *CoreUpdater) stopAutomaticUpdates() {
	u.policyMu.Lock()
	defer u.policyMu.Unlock()
	u.stopAutomaticUpdatesLocked()
}

func (u *CoreUpdater) runAutomaticUpdates(job *coreAutomaticUpdate, ticks <-chan time.Time, stop func()) {
	defer close(job.done)
	defer stop()
	// Startup/reload never downloads the core. The first check is one complete
	// configured interval later, and every check works without an open browser.
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
			executable := os.Executable
			if u.executable != nil {
				executable = u.executable
			}
			path, err := executable()
			if err == nil {
				err = u.updateContext(job.ctx, path, ReleaseChannel, false, job)
			}
			if err != nil && job.ctx.Err() == nil && !errors.Is(err, ErrCoreUpdateBusy) {
				log.Warnln("Automatic core update failed: %s", err)
			}
		}
	}
}

// StopAutomaticUpdates cancels outstanding background work without waiting for
// downloads or taking the installer operation gates. Shutdown calls it after
// a successful core commit, when no updater policy lock is held.
func StopAutomaticUpdates() {
	DefaultCoreUpdater.stopAutomaticUpdates()
	uiActivation.Lock()
	defer uiActivation.Unlock()
	if u := currentUIUpdater.Load(); u != nil {
		if u.automatic != nil {
			u.automatic.cancel()
		}
		if u.activeCancel != nil {
			u.activeCancel()
		}
	}
}
