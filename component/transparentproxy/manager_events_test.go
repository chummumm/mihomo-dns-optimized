//go:build go1.25

package transparentproxy

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func testManagerEventClock(t *testing.T, run func(*testing.T)) {
	t.Helper()
	// go.mod retains Go 1.20 timer defaults for production compatibility.
	// synctest requires synchronous timer channels within these tests only.
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",asynctimerchan=0")
	synctest.Test(t, run)
}

func startEventTestManager(t *testing.T) (*manager, *managerTestSystem, *managerRecorder, chan systemEvent) {
	t.Helper()
	m, system, _, recorder := testManager(t)
	if err := m.subscribe(); err != nil {
		t.Fatal(err)
	}
	recorder.reset()
	m.mu.Lock()
	m.startLocked()
	m.mu.Unlock()
	t.Cleanup(func() { m.cancel(); <-m.done })
	synctest.Wait()
	return m, system, recorder, system.watches[0]
}

func eventSnapshotCount(recorder *managerRecorder) int {
	count := 0
	for _, entry := range recorder.all() {
		if entry == "snapshot" {
			count++
		}
	}
	return count
}

func TestManagerEventBurstUsesOneScanAndNoHealthyPolling(t *testing.T) {
	testManagerEventClock(t, func(t *testing.T) {
		_, _, recorder, events := startEventTestManager(t)
		for i := 0; i < 64; i++ {
			events <- systemEvent{}
		}
		synctest.Wait()
		time.Sleep(networkEventQuietPeriod - time.Nanosecond)
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 0 {
			t.Fatalf("burst scanned before quiet period: %d", got)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 1 {
			t.Fatalf("64 ordinary invalidations caused %d scans, want 1", got)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 1 {
			t.Fatalf("healthy manager retained a polling timer: %d", got)
		}
	})
}

func TestManagerContinuousEventsCannotPostponeUpdatePastMaximum(t *testing.T) {
	testManagerEventClock(t, func(t *testing.T) {
		_, _, recorder, events := startEventTestManager(t)
		for i := 0; i < 50; i++ {
			events <- systemEvent{}
			synctest.Wait()
			time.Sleep(10 * time.Millisecond)
			synctest.Wait()
			if got, want := eventSnapshotCount(recorder), (i+1)/25; got != want {
				t.Fatalf("after %d ms of continuous events: scans=%d, want %d", (i+1)*10, got, want)
			}
		}
	})
}

func TestManagerUrgentEventsBypassPendingBurst(t *testing.T) {
	for _, test := range []struct {
		name  string
		event systemEvent
	}{
		{"protection", systemEvent{Urgent: true}},
		{"observation-loss", systemEvent{Err: errors.New("ENOBUFS")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			testManagerEventClock(t, func(t *testing.T) {
				_, _, recorder, events := startEventTestManager(t)
				events <- systemEvent{}
				synctest.Wait()
				events <- test.event
				synctest.Wait()
				if got := eventSnapshotCount(recorder); got != 1 {
					t.Fatalf("urgent event waited for timer: scans=%d", got)
				}
				if test.event.Err != nil {
					entries := recorder.all()
					if len(entries) < 3 || entries[0] != "blackhole-ack" || entries[1] != "capture-tun-closed" || entries[2] != "watch" {
						t.Fatalf("observation loss did not protect before recovery: %v", entries)
					}
				}
				time.Sleep(networkEventMaxDelay)
				synctest.Wait()
				if got := eventSnapshotCount(recorder); got != 1 {
					t.Fatalf("superseded burst timer performed another scan: %d", got)
				}
			})
		})
	}
}

type eventBlockedSystem struct {
	*managerTestSystem
	entered chan struct{}
	release chan struct{}
	block   bool
}

func (s *eventBlockedSystem) Snapshot(ctx context.Context) (networkState, error) {
	if s.block {
		s.block = false
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return networkState{}, ctx.Err()
		}
	}
	return s.managerTestSystem.Snapshot(ctx)
}

func TestManagerEventDuringScanIsNotLost(t *testing.T) {
	testManagerEventClock(t, func(t *testing.T) {
		m, system, recorder, events := startEventTestManager(t)
		blocked := &eventBlockedSystem{managerTestSystem: system, entered: make(chan struct{}), release: make(chan struct{}), block: true}
		m.mu.Lock()
		m.system = blocked
		m.mu.Unlock()
		events <- systemEvent{}
		<-blocked.entered
		events <- systemEvent{}
		close(blocked.release)
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 1 {
			t.Fatalf("first scan did not complete: %d", got)
		}
		time.Sleep(networkEventQuietPeriod)
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 2 {
			t.Fatalf("in-flight event was dropped: %d", got)
		}
	})
}

func TestManagerEventCancelAndStopDiscardPendingTimers(t *testing.T) {
	for _, closeNormally := range []bool{false, true} {
		name := "cancel"
		if closeNormally {
			name = "normal-stop"
		}
		t.Run(name, func(t *testing.T) {
			testManagerEventClock(t, func(t *testing.T) {
				m, _, recorder, events := startEventTestManager(t)
				events <- systemEvent{}
				synctest.Wait()
				if closeNormally {
					m.mu.Lock()
					err := m.closeLocked()
					m.mu.Unlock()
					if err != nil {
						t.Fatal(err)
					}
				} else {
					m.cancel()
				}
				<-m.done
				time.Sleep(time.Second)
				synctest.Wait()
				if got := eventSnapshotCount(recorder); got != 0 {
					t.Fatalf("stopped worker applied a pending event: %d", got)
				}
			})
		})
	}
}

func TestManagerObservationFailureRetainsBackoffDespiteQueuedUrgentEvents(t *testing.T) {
	testManagerEventClock(t, func(t *testing.T) {
		m, system, recorder, events := startEventTestManager(t)
		m.mu.Lock()
		system.snapshotErr = errors.New("snapshot interrupted")
		m.mu.Unlock()
		events <- systemEvent{Urgent: true}
		synctest.Wait()
		for i := 0; i < 8; i++ {
			events <- systemEvent{Urgent: true}
			synctest.Wait()
		}
		close(events)
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 1 {
			t.Fatalf("self-generated urgent events defeated failure backoff: %d", got)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 2 {
			t.Fatalf("failure did not retry at initial backoff: %d", got)
		}
		m.mu.Lock()
		system.snapshotErr = nil
		m.mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 3 || len(system.watches) != 3 {
			t.Fatalf("failed subscription did not recover: scans=%d watches=%d", got, len(system.watches))
		}
	})
}

func TestManagerUrgentLossRepairsFenceWithoutRestartingRecovery(t *testing.T) {
	testManagerEventClock(t, func(t *testing.T) {
		m, system, recorder, events := startEventTestManager(t)
		m.mu.Lock()
		system.snapshotErr = errors.New("snapshot interrupted")
		m.mu.Unlock()
		events <- systemEvent{Urgent: true}
		synctest.Wait()
		m.mu.Lock()
		wall := m.wall.(*managerTestFirewall)
		protected := wall.state
		wall.state = firewallState{} // An external nft flush removes the fence.
		m.mu.Unlock()
		before := time.Now()
		events <- systemEvent{Urgent: true}
		synctest.Wait()
		m.mu.Lock()
		repaired, uncertain := reflect.DeepEqual(wall.state, protected), m.uncertain
		m.mu.Unlock()
		if !repaired || !uncertain || time.Now() != before {
			t.Fatalf("urgent loss did not repair protection immediately: repaired=%v uncertain=%v elapsed=%v", repaired, uncertain, time.Since(before))
		}
		for i := 0; i < 8; i++ {
			events <- systemEvent{Urgent: true}
			synctest.Wait()
		}
		system.watchMu.Lock()
		watches := len(system.watches)
		system.watchMu.Unlock()
		if got := eventSnapshotCount(recorder); got != 1 || watches != 1 {
			t.Fatalf("fence notifications restarted complete recovery: scans=%d watches=%d", got, watches)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		system.watchMu.Lock()
		watches = len(system.watches)
		system.watchMu.Unlock()
		if got := eventSnapshotCount(recorder); got != 2 || watches != 2 {
			t.Fatalf("fence repair changed recovery backoff: scans=%d watches=%d", got, watches)
		}
	})
}

func TestManagerUrgentFaultInterruptsHealthyRepairRetry(t *testing.T) {
	testManagerEventClock(t, func(t *testing.T) {
		_, system, recorder, events := startEventTestManager(t)
		system.pruneErr = errors.New("retired cleanup will retry")
		events <- systemEvent{Urgent: true}
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 1 {
			t.Fatalf("initial repair scan: %d", got)
		}
		system.pruneErr = nil
		events <- systemEvent{Err: errors.New("netlink overflow during retry")}
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != 2 {
			t.Fatalf("unprotected new observation failure waited for repair retry: %d", got)
		}
	})
}

func TestManagerSynchronousRecoveryReplacesSubscriptionAndWakesWorker(t *testing.T) {
	testManagerEventClock(t, func(t *testing.T) {
		m, system, recorder, events := startEventTestManager(t)
		m.mu.Lock()
		system.snapshotErr = errors.New("incomplete snapshot")
		m.mu.Unlock()
		events <- systemEvent{Urgent: true}
		synctest.Wait()
		m.mu.Lock()
		system.snapshotErr = nil
		err := m.applyConfigurationLocked(m.config, m.runtime, m.uids, m.marks)
		m.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(system.watches) != 2 {
			t.Fatal("synchronous apply resumed without replacing the failed observation")
		}
		before := eventSnapshotCount(recorder)
		system.watches[1] <- systemEvent{Urgent: true}
		synctest.Wait()
		if got := eventSnapshotCount(recorder); got != before+1 {
			t.Fatalf("worker remained asleep on the retired subscription: %d -> %d", before, got)
		}
	})
}

func TestManagerQueuedObservationFailurePrecedesReadyLifecycleKick(t *testing.T) {
	testManagerEventClock(t, func(t *testing.T) {
		// Exercise both ready select inputs without advancing time. Whichever
		// wins, a failure already in the queue must fence before any snapshot.
		for i := 0; i < 32; i++ {
			m, system, _, recorder := testManager(t)
			if err := m.subscribe(); err != nil {
				t.Fatal(err)
			}
			recorder.reset()
			system.watches[0] <- systemEvent{Err: errors.New("notification overrun")}
			m.mu.Lock()
			m.requestSync()
			m.startLocked()
			m.mu.Unlock()
			synctest.Wait()
			m.cancel()
			<-m.done
			entries := recorder.all()
			if len(entries) < 3 || entries[0] != "blackhole-ack" || entries[1] != "capture-tun-closed" || entries[2] != "watch" {
				t.Fatalf("ready kick bypassed queued failure: %v", entries)
			}
		}
	})
}
