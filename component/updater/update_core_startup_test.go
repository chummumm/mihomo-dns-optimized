package updater

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	uiHTTP "github.com/metacubex/http"
)

func TestCoreUpdaterStartupSurvivesBusyPolicyHandoff(t *testing.T) {
	for _, cancelWaiting := range []bool{false, true} {
		name := "reenable waits for canceled installer"
		if cancelWaiting {
			name = "waiting activation is cancelable"
		}
		t.Run(name, func(t *testing.T) {
			f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
			entered, release := make(chan struct{}), make(chan struct{})
			var attempts atomic.Int32
			var releaseOnce sync.Once
			var observeActivation atomic.Bool
			var observedOnce sync.Once
			activationEntered := make(chan struct{})
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			f.u.executable = func() (string, error) {
				if observeActivation.Load() {
					observedOnce.Do(func() { close(activationEntered) })
				}
				return f.path, nil
			}
			f.u.beforeCommit = func() {
				if attempts.Add(1) == 1 {
					close(entered)
					<-release // retain the actual installer gate after cancellation
				}
			}
			t.Cleanup(unblock)
			f.u.ConfigureAutoUpdate(true, 6)
			old := f.job()
			coreAutomaticDone(t, entered)
			applied := make(chan struct{})
			go func() {
				f.u.ConfigureAutoUpdate(false, 6)
				f.u.ConfigureAutoUpdate(true, 6)
				close(applied)
			}()
			coreAutomaticDone(t, applied)
			// More transitions must not lose the still-held ancestor operation.
			for i := 0; i < 2; i++ {
				waiting := f.job()
				f.u.ConfigureAutoUpdate(false, 6)
				f.u.ConfigureAutoUpdate(true, 6)
				coreAutomaticDone(t, waiting.done)
			}
			previous := f.job()
			f.u.ConfigureAutoUpdate(false, 6)
			coreAutomaticDone(t, previous.done)
			observeActivation.Store(true)
			f.u.ConfigureAutoUpdate(true, 6)
			waiting := f.job()
			coreAutomaticDone(t, activationEntered)
			// Give the entered action an opportunity to observe the still-held
			// gate. The old TryLock-only implementation loses this activation.
			select {
			case err := <-f.commits:
				t.Fatalf("an operation committed before its holder was released: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			if waiting == old || f.requests.Load() != 3 || f.archives.Load() != 1 {
				t.Fatal("a reload overlapped the canceled installer or lost its policy")
			}
			if cancelWaiting {
				f.u.ConfigureAutoUpdate(false, 6)
				coreAutomaticDone(t, waiting.done)
				unblock()
				if err := f.commitResult(t); err == nil {
					t.Fatal("the canceled original operation committed")
				}
				coreAutomaticDone(t, old.done)
				f.unchanged(t)
				if f.requests.Load() != 3 {
					t.Fatal("a canceled waiter started a late request")
				}
				return
			}
			unblock()
			failures, successes := 0, 0
			for i := 0; i < 2; i++ {
				if err := f.commitResult(t); err == nil {
					successes++
				} else {
					failures++
				}
			}
			coreAutomaticDone(t, old.done)
			if successes != 1 || failures != 1 || f.requests.Load() != 6 || f.archives.Load() != 2 || f.restarts.Load() != 1 {
				t.Fatal("reactivation did not install exactly once without a periodic tick")
			}
		})
	}
}

func TestCoreUpdaterStartupFollowsLatestReadiness(t *testing.T) {
	called := make(chan struct{}, 64)
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.1", "mihomo-optimized-v1.0.1", func(http.ResponseWriter, *http.Request) bool {
		called <- struct{}{}
		return false
	})
	firstReady, latestReady := make(chan struct{}), make(chan struct{})
	f.u.ConfigureAutoUpdateWhenReady(true, 6, firstReady)
	job, next := f.job(), f.u.NextCheck()
	for i := 0; i < 30; i++ {
		f.u.ConfigureAutoUpdateWhenReady(true, 6, latestReady)
	}
	if f.job() != job || f.u.NextCheck() != next {
		t.Fatal("a readiness change replaced the retained scheduler")
	}
	close(firstReady)
	select {
	case <-called:
		t.Fatal("an older completed reset released the new configuration")
	case <-time.After(25 * time.Millisecond):
	}
	close(latestReady)
	coreAutomaticWait(t, func() bool { return f.requests.Load() == 1 && f.settled() })
	<-called
	thirdReady := make(chan struct{})
	f.u.ConfigureAutoUpdateWhenReady(true, 6, thirdReady)
	close(thirdReady)
	select {
	case <-called:
		t.Fatal("same-policy reload repeated the completed startup check")
	case <-time.After(25 * time.Millisecond):
	}
	if f.job() != job || f.u.NextCheck() != next || f.clock.ticker(t, 0).interval != 6*time.Hour {
		t.Fatal("same-policy reload postponed the configured interval")
	}
	f.clock.ticker(t, 0).fire(t, *job.next.Load())
	coreAutomaticWait(t, func() bool { return f.requests.Load() == 2 && f.settled() })
	f.unchanged(t)
}

func TestCoreUpdaterStartupReadinessCancellationAndReenable(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.1", "mihomo-optimized-v1.0.1", nil)
	ready := make(chan struct{})
	f.u.ConfigureAutoUpdateWhenReady(true, 1, ready)
	old := f.job()
	f.u.ConfigureAutoUpdateWhenReady(false, 1, ready)
	coreAutomaticDone(t, old.done)
	close(ready)
	if f.requests.Load() != 0 {
		t.Fatal("disabled startup performed a request before readiness")
	}
	f.u.ConfigureAutoUpdate(true, 1)
	coreAutomaticWait(t, func() bool { return f.requests.Load() == 1 && f.settled() })
	active := f.job()
	f.u.ConfigureAutoUpdate(false, 1)
	coreAutomaticDone(t, active.done)
	f.u.ConfigureAutoUpdate(true, 1)
	coreAutomaticWait(t, func() bool { return f.requests.Load() == 2 && f.settled() })
	f.unchanged(t)
}

func TestCoreUpdaterStartupIntervalChangeWaitsForNewCycle(t *testing.T) {
	called := make(chan struct{}, 4)
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.1", "mihomo-optimized-v1.0.1", func(http.ResponseWriter, *http.Request) bool {
		called <- struct{}{}
		return false
	})
	f.u.ConfigureAutoUpdate(true, 6)
	coreAutomaticWait(t, func() bool { return f.requests.Load() == 1 && f.settled() })
	<-called
	previous := f.job()
	f.u.ConfigureAutoUpdate(true, 2)
	current := f.job()
	coreAutomaticDone(t, previous.done)
	if current == previous || f.clock.ticker(t, 1).interval != 2*time.Hour {
		t.Fatal("an interval change did not replace the old periodic schedule")
	}
	// A same-version startup leaves no pending installation to hide an
	// incorrectly repeated activation check after changing only the interval.
	select {
	case <-called:
		t.Fatal("an interval-only reload repeated the startup check")
	case <-time.After(75 * time.Millisecond):
	}
	f.clock.ticker(t, 1).fire(t, *current.next.Load())
	coreAutomaticWait(t, func() bool { return f.requests.Load() == 2 && f.settled() })
	f.unchanged(t)
}

func TestCoreUpdaterAndUIStartupRunIndependentlyAfterReadiness(t *testing.T) {
	coreEntered, uiEntered := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.1", "mihomo-optimized-v1.0.1", func(_ http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "version.txt") {
			close(coreEntered)
			select {
			case <-release:
			case <-r.Context().Done():
				return true
			}
		}
		return false
	})
	u := uiTestUpdater(t, uiTestAutomaticFiles("0.4.0", "existing panel"))
	clock := &uiTestAutomaticClock{}
	u.newTicker = clock.newTicker
	archive := uiTestZIP(t, uiTestAutomaticFiles("0.4.0", "existing panel"))
	var uiRequests atomic.Int32
	u.request = func(ctx context.Context, _ string, _ map[string][]string) (*uiHTTP.Response, error) {
		uiRequests.Add(1)
		close(uiEntered)
		select {
		case <-release:
			return uiTestResponse(archive), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	uiTestActivateUpdater(t, u)
	t.Cleanup(unblock)
	u = uiTestAutomaticClone(u, true, 6)
	ready := make(chan struct{})
	SetUIUpdaterWhenReady(u, ready)
	f.u.ConfigureAutoUpdateWhenReady(true, 6, ready)
	if f.requests.Load() != 0 || uiRequests.Load() != 0 {
		t.Fatal("a startup task bypassed the shared readiness barrier")
	}
	close(ready)
	// Each network operation remains blocked until both have independently
	// started, proving neither activation waits for the other's download.
	coreAutomaticDone(t, coreEntered)
	uiTestAutomaticDone(t, uiEntered)
	unblock()
	coreAutomaticWait(t, func() bool { return f.requests.Load() == 1 && f.settled() })
	uiTestAutomaticWait(t, func() bool { return uiRequests.Load() == 1 && uiTestAutomaticSettled(u) })
	if uiTestIndex(t, u) != "existing panel" || f.restarts.Load() != 0 {
		t.Fatal("same-version startup altered an installed component")
	}
}
