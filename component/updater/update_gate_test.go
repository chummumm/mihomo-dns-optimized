package updater

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitUpdateGateResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("operation gate did not finish")
		return nil
	}
}

func TestCoreUpdaterOperationGateSerializesOperations(t *testing.T) {
	var gate updateOperationGate
	if !gate.TryLock() {
		t.Fatal("zero-value gate must be available")
	}
	if gate.TryLock() {
		t.Fatal("TryLock acquired an occupied gate")
	}
	gate.Unlock()

	// This unguarded counter deliberately relies on the gate for mutual
	// exclusion; the race detector also verifies its synchronization contract.
	var counter int
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				gate.Lock()
				counter++
				gate.Unlock()
			}
		}()
	}
	workers.Wait()
	if counter != 800 {
		t.Fatalf("serialized operations = %d, want 800", counter)
	}
	if !gate.TryLock() {
		t.Fatal("completed operations left the gate occupied")
	}
	gate.Unlock()
}

func TestCoreUpdaterOperationGateRunsAfterHolderRelease(t *testing.T) {
	var gate updateOperationGate
	gate.Lock()
	held := true
	t.Cleanup(func() {
		if held {
			gate.Unlock()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	busy := errors.New("operation occupied")
	firstAttempt := make(chan struct{})
	var attempts, operations atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- gate.runWhenAvailable(ctx, busy, func() error {
			attempt := attempts.Add(1)
			if !gate.TryLock() {
				if attempt == 1 {
					close(firstAttempt)
				}
				return fmt.Errorf("previous operation: %w", busy)
			}
			defer gate.Unlock()
			operations.Add(1)
			return nil
		})
	}()
	select {
	case <-firstAttempt:
	case <-ctx.Done():
		t.Fatal("background operation did not reach occupied gate")
	}
	select {
	case err := <-done:
		t.Fatalf("background operation returned while holder was active: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("action retried %d times before release", got)
	}
	gate.Unlock()
	held = false
	if err := waitUpdateGateResult(t, done); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || operations.Load() != 1 {
		t.Fatalf("attempts = %d, operations = %d, want 2 and 1", attempts.Load(), operations.Load())
	}
}

func TestUIUpdaterOperationGateCancellationDoesNotRunLate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline bool
		want     error
	}{
		{name: "cancel", want: context.Canceled},
		{name: "deadline", deadline: true, want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gate updateOperationGate
			gate.Lock()
			held := true
			t.Cleanup(func() {
				if held {
					gate.Unlock()
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			if tc.deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
			}
			defer cancel()
			busy := errors.New("operation occupied")
			attempted := make(chan struct{})
			var attempts, operations atomic.Int32
			done := make(chan error, 1)
			go func() {
				done <- gate.runWhenAvailable(ctx, busy, func() error {
					if attempts.Add(1) == 1 {
						close(attempted)
					}
					if !gate.TryLock() {
						return busy
					}
					defer gate.Unlock()
					operations.Add(1)
					return nil
				})
			}()
			select {
			case <-attempted:
			case <-time.After(3 * time.Second):
				t.Fatal("background operation did not try occupied gate")
			}
			if !tc.deadline {
				cancel()
			}
			if err := waitUpdateGateResult(t, done); !errors.Is(err, tc.want) {
				t.Fatalf("wait error = %v, want %v", err, tc.want)
			}
			// The waiter has already returned while the old holder is still
			// active. Releasing it cannot leave a queued action to run later.
			gate.Unlock()
			held = false
			if attempts.Load() != 1 || operations.Load() != 0 {
				t.Fatalf("canceled waiter made %d attempts and %d operations", attempts.Load(), operations.Load())
			}
		})
	}
}

func TestUIUpdaterOperationGateAlreadyCanceledDoesNotStart(t *testing.T) {
	var gate updateOperationGate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := gate.runWhenAvailable(ctx, errors.New("busy"), func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("already canceled action: error = %v, called = %v", err, called)
	}
}

func TestCoreUpdaterOperationGateReturnsOtherResultsOnce(t *testing.T) {
	for _, result := range []error{nil, errors.New("invalid release metadata")} {
		var gate updateOperationGate
		calls := 0
		err := gate.runWhenAvailable(context.Background(), errors.New("busy"), func() error {
			calls++
			return result
		})
		if err != result || calls != 1 {
			t.Fatalf("result = %v, calls = %d; want %v and 1", err, calls, result)
		}
	}
}
