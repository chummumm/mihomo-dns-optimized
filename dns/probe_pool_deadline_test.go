package dns

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// A deadline can pass before the context timer publishes its cancellation.
// Keeping that notification pending makes the execution race deterministic.
type probeDeadlineTestContext struct {
	context.Context
	deadline time.Time
}

func (c probeDeadlineTestContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

func TestDNSProbePoolPreservesAbsoluteDeadline(t *testing.T) {
	for _, test := range []struct {
		name          string
		callerTimeout time.Duration
		poolTimeout   time.Duration
	}{
		{name: "caller-limit", callerTimeout: 5 * time.Second, poolTimeout: 10 * time.Second},
		{name: "pool-limit", callerTimeout: 10 * time.Second, poolTimeout: 5 * time.Second},
		{name: "unbounded-caller", poolTimeout: 5 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.callerTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, test.callerTimeout)
				defer cancel()
			}
			pool := newDNSProbePool(1, test.poolTimeout)
			defer pool.Close()
			observed := make(chan context.Context, 1)
			before := time.Now()
			_, err := pool.submit(ctx, "deadline", func(jobCtx context.Context) (time.Duration, error) {
				observed <- jobCtx
				return time.Millisecond, nil
			}, func(time.Duration, error) {})
			after := time.Now()
			if err != nil {
				t.Fatal(err)
			}
			select {
			case jobCtx := <-observed:
				deadline, bounded := jobCtx.Deadline()
				if !bounded {
					t.Fatal("admitted probe lost its deadline")
				}
				if callerDeadline, ok := ctx.Deadline(); ok && test.callerTimeout < test.poolTimeout {
					if !deadline.Equal(callerDeadline) {
						t.Fatalf("probe shifted caller deadline by %v: got %v, want %v", deadline.Sub(callerDeadline), deadline, callerDeadline)
					}
				} else if deadline.Before(before.Add(test.poolTimeout)) || deadline.After(after.Add(test.poolTimeout)) {
					t.Fatalf("probe did not keep the pool timeout: deadline=%v, admission=%v..%v", deadline, before, after)
				}
			case <-time.After(time.Second):
				t.Fatal("admitted probe did not start")
			}
		})
	}
}

func TestDNSProbeWorkerChecksDeadlineBeforeTimerCancellation(t *testing.T) {
	pool := newDNSProbePool(1, time.Second)
	defer pool.Close()
	ctx, cancel := context.WithCancel(probeDeadlineTestContext{context.Background(), time.Now().Add(-time.Second)})
	if ctx.Err() != nil {
		t.Fatal("test requires cancellation notification to remain pending")
	}
	var calls atomic.Int32
	results := make(chan error, 1)
	// Queue the task with the timer notification still pending. Constructing
	// a new timer for an already-past deadline would cancel it synchronously.
	task := &dnsProbeTask{
		key: "expired-before-timer", ctx: ctx, cancel: cancel,
		stop: context.AfterFunc(pool.ctx, cancel),
		work: func(context.Context) (time.Duration, error) {
			calls.Add(1)
			return 0, context.DeadlineExceeded
		},
		waiters: map[uint64]dnsProbeDeliver{1: func(_ time.Duration, err error) { results <- err }},
	}
	pool.tasks[task.key] = task
	pool.jobs <- task
	pool.started = true
	pool.wg.Add(1)
	go pool.worker()
	select {
	case err := <-results:
		if !errors.Is(err, errDNSProbeNotStarted) || errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
			t.Fatalf("delayed timer allowed an expired probe to run: calls=%d error=%v", calls.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("expired queued probe did not finish")
	}
}

func TestSpeedCheckModeChecksDeadlineBeforeTimerCancellation(t *testing.T) {
	checker := &directSpeedChecker{slots: make(chan struct{}, 1)}
	ctx := probeDeadlineTestContext{context.Background(), time.Now().Add(-time.Second)}
	calls := 0
	_, err := checker.checkMode(ctx, netip.MustParseAddr("192.0.2.1"), speedCheckMode{port: 443}, func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		calls++
		return 0, context.DeadlineExceeded
	})
	if !errors.Is(err, errDNSProbeNotStarted) || errors.Is(err, context.DeadlineExceeded) || calls != 0 || len(checker.slots) != 0 {
		t.Fatalf("expired mode started work or retained capacity: calls=%d slots=%d error=%v", calls, len(checker.slots), err)
	}
}

func TestDNSProbePoolDoesNotShareExpiredTaskBeforeTimerCancellation(t *testing.T) {
	pool := newDNSProbePool(1, time.Second)
	defer pool.Close()
	ctx, cancel := context.WithCancel(probeDeadlineTestContext{context.Background(), time.Now().Add(-time.Second)})
	task := &dnsProbeTask{
		key: "expired-shared", ctx: ctx, cancel: cancel,
		stop: context.AfterFunc(pool.ctx, cancel), waiters: make(map[uint64]dnsProbeDeliver),
	}
	pool.tasks[task.key] = task
	ticket, err := pool.submit(context.Background(), task.key, func(context.Context) (time.Duration, error) {
		return time.Millisecond, nil
	}, func(time.Duration, error) {})
	if ticket != nil || !errors.Is(err, errDNSProbeBusy) || len(task.waiters) != 0 {
		t.Fatalf("expired work acquired another waiter in a full pool: ticket=%v waiters=%d error=%v", ticket, len(task.waiters), err)
	}
}
