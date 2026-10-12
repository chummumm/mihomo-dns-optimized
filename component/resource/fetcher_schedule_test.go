package resource

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitFetcherSchedule(t *testing.T, f *Fetcher[string], state string) time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if next, got := f.AutomaticSchedule(); got == state {
			return next
		}
		time.Sleep(time.Millisecond)
	}
	next, got := f.AutomaticSchedule()
	t.Fatalf("automatic schedule = %s / %s, want %s", next, got, state)
	return time.Time{}
}

func TestFetcherScheduleTracksTimerBackoffAndManualUpdate(t *testing.T) {
	started := make(chan int32, 3)
	first, second := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	vehicle := &controlledVehicle{
		read: func(ctx context.Context) ([]byte, error) {
			call := calls.Add(1)
			started <- call
			if call <= 2 {
				release := first
				if call == 2 {
					release = second
				}
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if call == 1 {
				return nil, errors.New("temporary download failure")
			}
			return []byte("rules"), nil
		},
		write: func([]byte) error { return nil },
	}
	f := NewFetcher("schedule", time.Hour, vehicle, nil, func(data []byte) (string, error) { return string(data), nil }, nil)
	t.Cleanup(func() { _ = f.Close() })
	f.backoff.Min = 100 * time.Millisecond
	f.updatedAt.Store(time.Now().Add(-2 * time.Hour))
	if err := f.startPullLoop(false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("expired pull timer did not start")
	}
	if next := waitFetcherSchedule(t, f, "checking"); !next.IsZero() {
		t.Fatal("provider invented the next delay before this request completed")
	}
	close(first)
	retry := waitFetcherSchedule(t, f, "retrying")
	if remaining := time.Until(retry); remaining <= 0 || remaining > 250*time.Millisecond {
		t.Fatalf("retry deadline did not follow backoff: %s", remaining)
	}
	select {
	case call := <-started:
		if call != 2 || time.Now().Before(retry.Add(-10*time.Millisecond)) {
			t.Fatal("retry ran before its published timer deadline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("published retry deadline did not trigger a request")
	}
	if next := waitFetcherSchedule(t, f, "checking"); !next.IsZero() {
		t.Fatal("running provider retained an obsolete retry deadline")
	}
	close(second)
	next := waitFetcherSchedule(t, f, "scheduled")
	if remaining := time.Until(next); remaining < time.Hour-time.Second || remaining > time.Hour {
		t.Fatalf("successful update did not restore the regular timer: %s", remaining)
	}
	if _, _, err := f.Update(); err != nil {
		t.Fatal(err)
	}
	if after, state := f.AutomaticSchedule(); !after.Equal(next) || state != "scheduled" {
		t.Fatal("a manual update moved the existing automatic deadline")
	}
	_ = f.Close()
	if after, state := f.AutomaticSchedule(); !after.IsZero() || state != "" {
		t.Fatal("closed provider retained an active deadline")
	}
}

func TestFetcherScheduleUnavailableForDisabledAndReplacedProviders(t *testing.T) {
	vehicle := &controlledVehicle{}
	f := NewFetcher("disabled", 0, vehicle, nil, func(data []byte) (string, error) { return string(data), nil }, nil)
	defer f.Close()
	f.recordNextCheck(time.Hour)
	if next, state := f.AutomaticSchedule(); !next.IsZero() || state != "" {
		t.Fatal("disabled policy exposed a deadline")
	}
	g := NewFetcher("replaced", time.Hour, vehicle, nil, func(data []byte) (string, error) { return string(data), nil }, nil)
	defer g.Close()
	g.recordNextCheck(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	g.BindUpdateContext(ctx, nil)
	cancel()
	if next, state := g.AutomaticSchedule(); !next.IsZero() || state != "" {
		t.Fatal("replaced configuration exposed the old provider's schedule")
	}
}
