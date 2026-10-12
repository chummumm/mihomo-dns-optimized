package updater

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	automaticScheduled uint32 = iota
	automaticWaiting
	automaticChecking
	automaticRetrying
)

// automaticSchedule reports the live worker's periodic deadline independently
// of a manual controller task. The time retains its monotonic component so a
// controller can report the remaining delay even when the device clock shifts.
func automaticSchedule(ctx context.Context, next *atomic.Pointer[time.Time], phase *atomic.Uint32) (time.Time, string) {
	if ctx.Err() != nil {
		return time.Time{}, ""
	}
	var deadline time.Time
	if value := next.Load(); value != nil {
		deadline = *value
	}
	state := "scheduled"
	switch phase.Load() {
	case automaticWaiting:
		state = "waiting"
	case automaticChecking:
		state = "checking"
	case automaticRetrying:
		state = "retrying"
	}
	return deadline, state
}
