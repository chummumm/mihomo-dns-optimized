package updater

import (
	"context"
	"errors"
	"sync/atomic"
)

var ErrCoreVersionUnavailable = errors.New("the running core version cannot be compared")

type CoreCheckResult struct {
	CurrentVersion string
	LatestVersion  string
	Updated        bool
}

// CoreUpdateReservation keeps scheduled and compatibility core installers from
// restarting the process while a unified task is updating its other resources.
// Close must be called by the owner even when no core phase is required.
type CoreUpdateReservation struct {
	u        *CoreUpdater
	consumed atomic.Bool
}

type coreManualUpdate struct {
	restart CoreAutomaticRestartHandler
	observe func(CoreCheckResult)
	result  CoreCheckResult
}

func (u *CoreUpdater) ReserveCheckAndUpdate() (*CoreUpdateReservation, error) {
	if !u.mu.TryLock() {
		return nil, ErrCoreUpdateBusy
	}
	if u.pendingRestart {
		u.mu.Unlock()
		return nil, ErrCoreUpdateBusy
	}
	return &CoreUpdateReservation{u: u}, nil
}

func (r *CoreUpdateReservation) Close() {
	if r != nil && r.consumed.CompareAndSwap(false, true) {
		r.u.mu.Unlock()
	}
}

// CheckAndUpdate always checks the release server. A known, strictly newer
// version uses the existing checksummed atomic installer; equal/older versions
// succeed without replacing the executable or restarting. restart must commit
// and restart in one configuration transaction, like the automatic bridge.
func (r *CoreUpdateReservation) CheckAndUpdate(ctx context.Context, executable string, restart CoreAutomaticRestartHandler, observe func(CoreCheckResult)) (CoreCheckResult, error) {
	if r == nil || !r.consumed.CompareAndSwap(false, true) {
		return CoreCheckResult{}, ErrCoreUpdateBusy
	}
	if restart == nil {
		r.u.mu.Unlock()
		return CoreCheckResult{}, errors.New("core restart handler is unavailable")
	}
	if err := r.u.readiness.wait(ctx); err != nil {
		r.u.mu.Unlock()
		return CoreCheckResult{}, err
	}
	manual := &coreManualUpdate{restart: restart, observe: observe}
	err := r.u.updateTransaction(ctx, executable, ReleaseChannel, false, nil, manual)
	return manual.result, err
}
