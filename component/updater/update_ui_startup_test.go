package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/http"
)

func uiTestStartupApply(t *testing.T, u *UIUpdater, ready <-chan struct{}, bootstrap bool) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		SetUIUpdaterWhenReady(u, ready)
		if bootstrap {
			u.StartAutoDownloadUI()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("configuration publication waited for an update operation or readiness")
	}
}

func uiTestStartupBootstrap(u *UIUpdater) *uiInitialDownload {
	u.state().mu.Lock()
	defer u.state().mu.Unlock()
	return u.state().bootstrap
}

func uiTestStartupHasIndex(u *UIUpdater, content string) bool {
	data, err := os.ReadFile(filepath.Join(u.externalUIPath, "index.html"))
	return err == nil && string(data) == content && uiTestAutomaticSettled(u)
}

func uiTestStartupNoIndex(t *testing.T, u *UIUpdater) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(u.externalUIPath, "index.html")); !os.IsNotExist(err) {
		t.Fatalf("canceled bootstrap installed index.html: %q, error=%v", data, err)
	}
}

type uiTestStartupHeldInstall struct {
	u        *UIUpdater
	clock    *uiTestAutomaticClock
	requests atomic.Int32
	release  func()
}

func uiTestStartupHoldInstall(t *testing.T) *uiTestStartupHeldInstall {
	t.Helper()
	f := &uiTestStartupHeldInstall{u: uiTestUpdater(t, nil), clock: &uiTestAutomaticClock{}}
	f.u.autoUpdateUI, f.u.updateInterval = true, 6
	f.u.newTicker = f.clock.newTicker
	archive := uiTestZIP(t, uiTestAutomaticFiles("0.4.1", "startup installed"))
	f.u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		f.requests.Add(1)
		return uiTestResponse(archive), nil
	}
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.release = func() { once.Do(func() { close(release) }) }
	f.u.beforeCommit = func() { close(held); <-release }
	uiTestActivateUpdater(t, f.u)
	// Run before the activation fixture's cleanup, including when an assertion
	// fails while the old installer deliberately ignores cancellation here.
	t.Cleanup(f.release)
	uiTestAutomaticDone(t, held)
	if f.requests.Load() != 1 {
		t.Fatal("initial activation did not prepare exactly one archive")
	}
	if f.u.state().operation.TryLock() {
		f.u.state().operation.Unlock()
		t.Fatal("fixture did not retain the installation operation gate")
	}
	return f
}

func TestUIUpdaterStartupCanceledInstallerHandsOffWithoutTick(t *testing.T) {
	for _, transition := range []string{"disable", "interval", "reenable"} {
		t.Run(transition, func(t *testing.T) {
			f := uiTestStartupHoldInstall(t)
			oldJob := f.u.automatic
			enabled, interval := false, 6
			switch transition {
			case "interval":
				enabled, interval = true, 2
			case "reenable":
				// Omit an intervening initializer so only the new activation
				// can perform the handoff. A manual/bootstrap winner must not
				// hide a lost immediate check in this case.
				uiTestStartupApply(t, uiTestAutomaticClone(f.u, false, 6), nil, false)
				enabled = true
			}
			latest := uiTestAutomaticClone(f.u, enabled, interval)
			uiTestStartupApply(t, latest, nil, true)
			job, bootstrap := latest.automatic, uiTestStartupBootstrap(latest)
			if transition == "reenable" {
				if job == nil || bootstrap != nil {
					t.Fatal("reenabled activation did not own the initial install")
				}
			} else if bootstrap == nil {
				t.Fatal("empty directory did not get a replacement initializer")
			}
			var next time.Time
			if job != nil {
				next = *job.next.Load()
			}
			for i := 0; i < 30; i++ {
				latest = uiTestAutomaticClone(latest, enabled, interval)
				uiTestStartupApply(t, latest, nil, true)
				if latest.automatic != job || uiTestStartupBootstrap(latest) != bootstrap {
					t.Fatal("same-source reload queued a new activation or bootstrap")
				}
				if job != nil && !job.next.Load().Equal(next) {
					t.Fatal("same-policy reload postponed the interval")
				}
			}
			if f.requests.Load() != 1 {
				t.Fatal("replacement overlapped the old installer's download")
			}
			f.release()
			uiTestAutomaticDone(t, oldJob.done)
			if bootstrap != nil {
				uiTestAutomaticDone(t, bootstrap.done)
			}
			// No fake clock tick is sent. The canceled old archive cannot
			// install, and exactly one fresh archive must complete the handoff.
			uiTestAutomaticWait(t, func() bool {
				return f.requests.Load() == 2 && uiTestStartupHasIndex(latest, "startup installed")
			})
			if got := readUIBuildInfo(latest.externalUIPath).Version; got != "0.4.1" {
				t.Fatalf("installed version = %q", got)
			}
			if uiTestStartupBootstrap(latest) != nil {
				t.Fatal("completed initialization retained a bootstrap worker")
			}
			if job != nil && !job.next.Load().Equal(next) {
				t.Fatal("initial installation reset the periodic schedule")
			}
		})
	}
}

func TestUIUpdaterStartupWaitingHandoffStopsBeforeHolderReleases(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled-bootstrap"
		if enabled {
			name = "reenabled-activation"
		}
		t.Run(name, func(t *testing.T) {
			f := uiTestStartupHoldInstall(t)
			oldJob := f.u.automatic
			if enabled {
				uiTestStartupApply(t, uiTestAutomaticClone(f.u, false, 6), nil, false)
			}
			latest := uiTestAutomaticClone(f.u, enabled, 6)
			uiTestStartupApply(t, latest, nil, true)
			var done <-chan struct{}
			if enabled {
				done = latest.automatic.done
			} else {
				bootstrap := uiTestStartupBootstrap(latest)
				if bootstrap == nil {
					t.Fatal("replacement initializer was not scheduled")
				}
				done = bootstrap.done
			}
			StopAutomaticUpdates()
			// This must finish while the previous install still holds the
			// gate. No detached waiter may perform an operation after Stop.
			uiTestAutomaticDone(t, done)
			if f.requests.Load() != 1 {
				t.Fatal("stopped waiter performed another download")
			}
			uiTestStartupNoIndex(t, latest)
			f.release()
			uiTestAutomaticDone(t, oldJob.done)
			uiTestStartupNoIndex(t, latest)
			if f.requests.Load() != 1 || uiTestStartupBootstrap(latest) != nil {
				t.Fatal("stopped handoff ran late or retained a bootstrap worker")
			}
		})
	}
}

func TestUIUpdaterStartupSamePolicyFollowsLatestReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, existing string
		enabled        bool
	}{
		{name: "installed-panel", existing: "old panel", enabled: true},
		{name: "automatic-bootstrap", enabled: true},
		{name: "disabled-bootstrap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var files map[string]string
			if tc.existing != "" {
				files = uiTestAutomaticFiles("0.4.0", tc.existing)
			}
			base := uiTestUpdater(t, files)
			clock := &uiTestAutomaticClock{}
			base.newTicker = clock.newTicker
			archive := uiTestZIP(t, uiTestAutomaticFiles("0.4.1", "ready installed"))
			requested := make(chan struct{}, 4)
			var requests atomic.Int32
			var expected atomic.Pointer[UIUpdater]
			base.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
				requests.Add(1)
				select {
				case requested <- struct{}{}:
				default:
				}
				if CurrentUIUpdater() != expected.Load() {
					t.Error("network work started before publishing the latest configuration")
				}
				return uiTestResponse(archive), nil
			}
			uiTestActivateUpdater(t, base)
			firstReady, latestReady := make(chan struct{}), make(chan struct{})
			first := uiTestAutomaticClone(base, tc.enabled, 6)
			uiTestStartupApply(t, first, firstReady, true)
			job, bootstrap := first.automatic, uiTestStartupBootstrap(first)
			var next time.Time
			if job != nil {
				next = *job.next.Load()
			}
			latest := uiTestAutomaticClone(first, tc.enabled, 6)
			uiTestStartupApply(t, latest, latestReady, true)
			expected.Store(latest)
			if latest.automatic != job || uiTestStartupBootstrap(latest) != bootstrap {
				t.Fatal("same-policy readiness update replaced its background task")
			}
			close(firstReady)
			select {
			case <-requested:
				t.Fatal("an obsolete completed reset released work before the latest reset")
			case <-time.After(75 * time.Millisecond):
			}
			if requests.Load() != 0 {
				t.Fatal("waiting for readiness performed an HTTP request")
			}
			close(latestReady)
			if bootstrap != nil {
				uiTestAutomaticDone(t, bootstrap.done)
			}
			uiTestAutomaticWait(t, func() bool {
				return requests.Load() == 1 && uiTestStartupHasIndex(latest, "ready installed")
			})
			if job != nil && (!job.next.Load().Equal(next) || clock.ticker(t, 0).interval != 6*time.Hour) {
				t.Fatal("readiness handoff restarted or postponed the periodic schedule")
			}
		})
	}
}

func TestUIUpdaterStartupReadinessWaitCanBeCanceled(t *testing.T) {
	for _, stop := range []bool{false, true} {
		name := "disable"
		if stop {
			name = "shutdown"
		}
		t.Run(name, func(t *testing.T) {
			base := uiTestUpdater(t, uiTestAutomaticFiles("0.4.0", "existing panel"))
			clock := &uiTestAutomaticClock{}
			base.newTicker = clock.newTicker
			var requests atomic.Int32
			base.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("canceled activation must not request an archive")
			}
			uiTestActivateUpdater(t, base)
			ready := make(chan struct{})
			enabled := uiTestAutomaticClone(base, true, 6)
			uiTestStartupApply(t, enabled, ready, true)
			if stop {
				StopAutomaticUpdates()
			} else {
				uiTestStartupApply(t, uiTestAutomaticClone(enabled, false, 6), ready, true)
			}
			uiTestAutomaticDone(t, enabled.automatic.done)
			close(ready)
			if requests.Load() != 0 || uiTestIndex(t, base) != "existing panel" {
				t.Fatal("canceled readiness wait downloaded or changed the installed panel")
			}
		})
	}
}

func TestUIUpdaterStartupIntervalOnlyWaitsForNewCycle(t *testing.T) {
	u := uiTestUpdater(t, uiTestAutomaticFiles("0.4.1", "existing panel"))
	u.autoUpdateUI, u.updateInterval = true, 6
	clock := &uiTestAutomaticClock{}
	u.newTicker = clock.newTicker
	archive := uiTestZIP(t, uiTestAutomaticFiles("0.4.1", "existing panel"))
	var requests atomic.Int32
	requestedAgain := make(chan struct{}, 4)
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		if requests.Add(1) > 1 {
			select {
			case requestedAgain <- struct{}{}:
			default:
			}
		}
		return uiTestResponse(archive), nil
	}
	uiTestActivateUpdater(t, u)
	uiTestAutomaticWait(t, func() bool { return requests.Load() == 1 && uiTestAutomaticSettled(u) })
	// An unexpired successful check would hide an erroneous activation by
	// serving its result from memory. Force any new check to reach the source.
	uiTestExpireCheck(u)
	oldJob := u.automatic
	latest := uiTestAutomaticClone(u, true, 2)
	uiTestStartupApply(t, latest, nil, true)
	uiTestAutomaticDone(t, oldJob.done)
	if latest.automatic == oldJob || clock.ticker(t, 1).interval != 2*time.Hour || uiTestStartupBootstrap(latest) != nil {
		t.Fatal("interval-only reload did not replace just the periodic schedule")
	}
	select {
	case <-requestedAgain:
		t.Fatal("changing only the interval repeated the startup check")
	case <-time.After(75 * time.Millisecond):
	}
	next := *latest.automatic.next.Load()
	clock.ticker(t, 1).fire(t, next)
	uiTestAutomaticWait(t, func() bool { return requests.Load() == 2 && uiTestAutomaticSettled(latest) })
	if uiTestIndex(t, latest) != "existing panel" || !latest.automatic.next.Load().Equal(next.Add(2*time.Hour)) {
		t.Fatal("the replacement cycle changed same-version files or lost its next interval")
	}
}

func TestUIUpdaterStartupFailedBootstrapWaitsForCycleAcrossReloads(t *testing.T) {
	u := uiTestUpdater(t, nil)
	u.autoUpdateUI, u.updateInterval = true, 6
	clock := &uiTestAutomaticClock{}
	u.newTicker = clock.newTicker
	archive := uiTestZIP(t, uiTestAutomaticFiles("0.4.1", "recovered on cycle"))
	var requests atomic.Int32
	retried := make(chan struct{}, 4)
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return nil, errors.New("temporary initial archive failure")
		}
		select {
		case retried <- struct{}{}:
		default:
		}
		return uiTestResponse(archive), nil
	}
	uiTestActivateUpdater(t, u)
	uiTestAutomaticWait(t, func() bool { return requests.Load() == 1 && uiTestAutomaticSettled(u) })
	job, next := u.automatic, *u.automatic.next.Load()
	latest := u
	for i := 0; i < 30; i++ {
		latest = uiTestAutomaticClone(latest, true, 6)
		uiTestStartupApply(t, latest, nil, true)
		if latest.automatic != job || !job.next.Load().Equal(next) || uiTestStartupBootstrap(latest) != nil {
			t.Fatal("ordinary reload replaced the scheduler or queued a failed-bootstrap retry")
		}
	}
	select {
	case <-retried:
		t.Fatal("failed initial installation retried before its configured cycle")
	case <-time.After(75 * time.Millisecond):
	}
	uiTestStartupNoIndex(t, latest)
	clock.ticker(t, 0).fire(t, next)
	uiTestAutomaticWait(t, func() bool {
		return requests.Load() == 2 && uiTestStartupHasIndex(latest, "recovered on cycle")
	})
}
