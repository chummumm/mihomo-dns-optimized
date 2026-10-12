package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metacubex/mihomo/log"
)

const defaultGeoUpdateInterval = 24

type geoAutomaticUpdate struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	interval time.Duration
	next     atomic.Pointer[time.Time]
	phase    atomic.Uint32
}

// GeoUpdater owns the scheduling policy and its one live worker. Resource
// selection, validation and commits remain shared with manual GeoData updates.
// The zero value is disabled; an omitted interval uses 24 hours.
type GeoUpdater struct {
	mu             sync.Mutex
	autoUpdate     bool
	updateInterval int
	automatic      *geoAutomaticUpdate
	readiness      updateReadiness

	// Test-only clock and operation injection; neither is a user option.
	newTicker func(time.Duration) (<-chan time.Time, func())
	check     func(context.Context) error
}

var DefaultGeoUpdater GeoUpdater

func (u *GeoUpdater) AutomaticPolicy() (bool, int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.autoUpdate, u.updateIntervalLocked()
}

func (u *GeoUpdater) updateIntervalLocked() int {
	if u.updateInterval <= 0 {
		return defaultGeoUpdateInterval
	}
	return u.updateInterval
}

func (u *GeoUpdater) NextCheck() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if job := u.automatic; job != nil && job.ctx.Err() == nil {
		if next := job.next.Load(); next != nil {
			return next.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func (u *GeoUpdater) AutomaticSchedule() (time.Time, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if job := u.automatic; job != nil {
		return automaticSchedule(job.ctx, &job.next, &job.phase)
	}
	return time.Time{}, ""
}

func (u *GeoUpdater) ConfigureAutoUpdate(enabled bool, intervalHours int) {
	u.ConfigureAutoUpdateWhenReady(enabled, intervalHours, nil)
}

// ConfigureAutoUpdateWhenReady publishes a complete policy without waiting for
// network I/O. Retained workers follow the newest applied configuration's
// readiness barrier; a changed/disabled policy cancels its old download.
func (u *GeoUpdater) ConfigureAutoUpdateWhenReady(enabled bool, intervalHours int, ready <-chan struct{}) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.readiness.set(ready)
	u.configureLocked(enabled, intervalHours)
}

func (u *GeoUpdater) configureLocked(enabled bool, intervalHours int) {
	// YAML is validated before ApplyConfig. Keep the embedded API safe too.
	if intervalHours <= 0 || intervalHours > int((1<<63-1)/time.Hour) {
		intervalHours = defaultGeoUpdateInterval
	}
	if u.autoUpdate == enabled && u.updateIntervalLocked() == intervalHours &&
		(!enabled || u.automatic != nil && u.automatic.ctx.Err() == nil) {
		return
	}
	activation := enabled && (!u.autoUpdate || u.automatic == nil)
	u.stopAutomaticUpdatesLocked()
	u.autoUpdate, u.updateInterval = enabled, intervalHours
	if !enabled {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	interval := time.Duration(intervalHours) * time.Hour
	job := &geoAutomaticUpdate{ctx: ctx, cancel: cancel, done: make(chan struct{}), interval: interval}
	next := time.Now().Add(interval)
	job.next.Store(&next)
	if activation {
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
	go u.runAutomaticUpdates(job, ticks, stop, activation)
}

func (u *GeoUpdater) stopAutomaticUpdatesLocked() {
	if u.automatic != nil {
		u.automatic.cancel()
	}
	u.automatic = nil
}

func (u *GeoUpdater) stopAutomaticUpdates() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.stopAutomaticUpdatesLocked()
}

func (u *GeoUpdater) runAutomaticUpdates(job *geoAutomaticUpdate, ticks <-chan time.Time, stop func(), activation bool) {
	defer close(job.done)
	defer stop()
	if activation {
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

func (u *GeoUpdater) runAutomaticCheck(job *geoAutomaticUpdate, activation bool) {
	job.phase.Store(automaticWaiting)
	finishedPhase := automaticScheduled
	defer func() { job.phase.Store(finishedPhase) }()
	action := func() error {
		if err := u.readiness.wait(job.ctx); err != nil {
			return err
		}
		if activation {
			due, err := geoAutomaticUpdateDue(job.interval, time.Now())
			if err != nil || !due {
				return err
			}
		}
		job.phase.Store(automaticChecking)
		if u.check != nil {
			return u.check(job.ctx)
		}
		log.Infoln("[GEO] Checking configured databases")
		_, err := CheckAndUpdateGeoDatabases(job.ctx)
		return err
	}
	var err error
	if activation {
		// A manual update or canceled old policy may still hold the gate. Only
		// activation waits; periodic work never forms an installer queue.
		err = geoUpdateGate.runWhenAvailable(job.ctx, ErrGetDatabaseUpdateSkip, action)
	} else {
		err = action()
	}
	if err != nil && job.ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrGetDatabaseUpdateSkip) {
		finishedPhase = automaticRetrying
		log.Warnln("[GEO] Automatic update failed: %s", err)
	}
	// Missing files, transient failures and a canceled old configuration never
	// terminate this policy's timer. The next tick uses the current snapshot.
}

// Preserve the existing startup age check, but consider every actually used
// database. An unused stale file cannot force a download, and a missing active
// database cannot permanently disable future automatic checks.
func geoAutomaticUpdateDue(interval time.Duration, now time.Time) (bool, error) {
	config, lifetime := currentGeoUpdates()
	if err := lifetime.Err(); err != nil {
		return false, err
	}
	for _, name := range GeoDatabasesForConfig(config) {
		_, path, err := geoDatabaseResource(config, name)
		if err != nil {
			return false, err
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("cannot inspect current GeoData: %w", err)
		}
		if info.ModTime().Add(interval).Before(now) {
			return true, nil
		}
	}
	return false, nil
}
