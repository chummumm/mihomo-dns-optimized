package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/http"
)

type uiTestAutomaticTicker struct {
	interval time.Duration
	ticks    chan time.Time
	stopped  chan struct{}
}

type uiTestAutomaticClock struct {
	mu      sync.Mutex
	tickers []*uiTestAutomaticTicker
}

func (c *uiTestAutomaticClock) newTicker(interval time.Duration) (<-chan time.Time, func()) {
	ticker := &uiTestAutomaticTicker{interval: interval, ticks: make(chan time.Time), stopped: make(chan struct{})}
	c.mu.Lock()
	c.tickers = append(c.tickers, ticker)
	c.mu.Unlock()
	return ticker.ticks, func() { close(ticker.stopped) }
}

func (c *uiTestAutomaticClock) ticker(t *testing.T, index int) *uiTestAutomaticTicker {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.tickers) != index+1 {
		t.Fatalf("created %d timers, wanted %d", len(c.tickers), index+1)
	}
	return c.tickers[index]
}

func (c *uiTestAutomaticTicker) fire(t *testing.T, at time.Time) {
	t.Helper()
	select {
	case c.ticks <- at:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic scheduler did not accept its next tick")
	}
}

func uiTestAutomaticWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("automatic UI update did not reach the expected state")
		case <-poll.C:
		}
	}
}

func uiTestAutomaticDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled automatic UI scheduler did not stop")
	}
}

func uiTestActivateUpdater(t *testing.T, u *UIUpdater) {
	t.Helper()
	previous := currentUIUpdater.Load()
	t.Cleanup(func() {
		var bootstrap *uiInitialDownload
		uiActivation.Lock()
		current := currentUIUpdater.Load()
		if current != nil {
			if current.automatic != nil {
				current.automatic.cancel()
			}
			if current.activeCancel != nil {
				current.activeCancel()
			}
			current.state().mu.Lock()
			bootstrap = current.state().bootstrap
			current.state().mu.Unlock()
		}
		currentUIUpdater.Store(previous)
		uiActivation.Unlock()
		if current != nil && current.automatic != nil {
			uiTestAutomaticDone(t, current.automatic.done)
		}
		if bootstrap != nil {
			uiTestAutomaticDone(t, bootstrap.done)
		}
	})
	SetUIUpdater(u)
}

func uiTestAutomaticClone(u *UIUpdater, enabled bool, interval int) *UIUpdater {
	v := NewFixedUIUpdater(enabled, interval)
	v.externalUIPath, v.externalUIURL = u.externalUIPath, u.externalUIURL
	v.coordinator, v.request, v.newTicker = u.coordinator, u.request, u.newTicker
	return v
}

func uiTestAutomaticFiles(version, content string) map[string]string {
	return map[string]string{
		"index.html":      content,
		"build-info.json": fmt.Sprintf(`{"name":"dashboard","version":%q}`, version),
	}
}

func uiTestAutomaticSettled(u *UIUpdater) bool {
	s := u.state()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checking == nil && !s.updating.Load()
}

func TestUIUpdaterAutomaticDefaultsAndBootstrap(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		u := uiTestUpdater(t, uiTestAutomaticFiles("0.3.6", "existing"))
		var requests atomic.Int32
		u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("unexpected request")
		}
		uiTestActivateUpdater(t, u)
		u.AutoDownloadUI()
		status := u.Status(context.Background(), false)
		if u.automatic != nil || status.AutoUpdate || status.UpdateInterval != 24 || status.NextCheck != "" || requests.Load() != 0 {
			t.Fatalf("default policy started automatic work: %+v, requests=%d", status, requests.Load())
		}
	})
	t.Run("activation bootstraps once without a clock tick", func(t *testing.T) {
		u := uiTestUpdater(t, nil)
		u.autoUpdateUI, u.updateInterval = true, 6
		clock := &uiTestAutomaticClock{}
		u.newTicker = clock.newTicker
		archive := uiTestZIP(t, uiTestAutomaticFiles("0.3.7", "bootstrap"))
		var requests atomic.Int32
		u.request = func(_ context.Context, _ string, headers map[string][]string) (*http.Response, error) {
			if CurrentUIUpdater() != u {
				t.Error("activation started before publishing its configuration")
			}
			if requests.Add(1) == 1 {
				response := uiTestResponse(archive)
				response.Header.Set("ETag", `"bootstrap"`)
				return response, nil
			}
			if http.Header(headers).Get("If-None-Match") != `"bootstrap"` {
				t.Error("scheduled bootstrap revalidation lost its ETag")
			}
			response := uiTestResponse(nil)
			response.StatusCode = http.StatusNotModified
			return response, nil
		}
		uiTestActivateUpdater(t, u)
		ticker := clock.ticker(t, 0)
		status := u.Status(context.Background(), false)
		next, err := time.Parse(time.RFC3339, status.NextCheck)
		if err != nil || time.Until(next) < 6*time.Hour-time.Minute || ticker.interval != 6*time.Hour {
			t.Fatalf("activation did not create the periodic schedule: %+v, %v", status, err)
		}
		u.StartAutoDownloadUI()
		u.StartAutoDownloadUI()
		uiTestAutomaticWait(t, func() bool {
			data, _ := os.ReadFile(filepath.Join(u.externalUIPath, "index.html"))
			return requests.Load() == 1 && string(data) == "bootstrap" && uiTestAutomaticSettled(u)
		})
		if requests.Load() != 1 || uiTestIndex(t, u) != "bootstrap" {
			t.Fatal("empty-directory bootstrap did not install exactly once")
		}
		uiTestExpireCheck(u)
		ticker.fire(t, next)
		uiTestAutomaticWait(t, func() bool { return requests.Load() == 2 && uiTestAutomaticSettled(u) })
		if uiTestIndex(t, u) != "bootstrap" {
			t.Fatal("304 scheduled check changed the installed UI")
		}
	})
}

func TestUIUpdaterAutomaticReloadPreservesSchedule(t *testing.T) {
	u := uiTestUpdater(t, uiTestAutomaticFiles("0.3.6", "existing"))
	u.autoUpdateUI, u.updateInterval = true, 12
	clock := &uiTestAutomaticClock{}
	u.newTicker = clock.newTicker
	archive := uiTestZIP(t, uiTestAutomaticFiles("0.3.6", "existing"))
	var requests atomic.Int32
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		requests.Add(1)
		return uiTestResponse(archive), nil
	}
	uiTestActivateUpdater(t, u)
	uiTestAutomaticWait(t, func() bool { return requests.Load() == 1 && uiTestAutomaticSettled(u) })
	first := u.automatic
	firstNext := *first.next.Load()
	stopReaders := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stopReaders:
				return
			default:
				_ = CurrentUIUpdater().Status(context.Background(), false)
			}
		}
	}()
	for i := 0; i < 30; i++ {
		v := uiTestAutomaticClone(u, true, 12)
		SetUIUpdater(v)
		if v.automatic != first || !v.automatic.next.Load().Equal(firstNext) {
			t.Error("same-policy reload reset the timer")
		}
	}
	close(stopReaders)
	readers.Wait()
	clock.ticker(t, 0)
	v := uiTestAutomaticClone(u, true, 3)
	SetUIUpdater(v)
	uiTestAutomaticDone(t, first.done)
	if v.automatic == first || clock.ticker(t, 1).interval != 3*time.Hour || time.Until(*v.automatic.next.Load()) < 3*time.Hour-time.Minute {
		t.Fatal("interval change did not replace the old schedule")
	}
	w := uiTestAutomaticClone(v, false, 3)
	SetUIUpdater(w)
	uiTestAutomaticDone(t, v.automatic.done)
	status := w.Status(context.Background(), false)
	if w.automatic != nil || status.AutoUpdate || status.NextCheck != "" || status.UpdateInterval != 3 {
		t.Fatalf("disabled policy retained a schedule: %+v", status)
	}
	clock.ticker(t, 1)
	if requests.Load() != 1 {
		t.Fatal("a reload repeated the activation check")
	}
}

func TestUIUpdaterAutomaticVersionGateAndConditionalChecks(t *testing.T) {
	for _, test := range []struct {
		name, latest, want string
	}{
		{"newer version", "0.3.7", "remote content"},
		{"same version with different files", "0.3.6", "existing"},
		{"older version", "0.3.5", "existing"},
		{"missing remote version", "", "existing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			u := uiTestUpdater(t, uiTestAutomaticFiles("0.3.6", "existing"))
			u.autoUpdateUI, u.updateInterval = true, 1
			clock := &uiTestAutomaticClock{}
			u.newTicker = clock.newTicker
			archive := uiTestZIP(t, uiTestAutomaticFiles(test.latest, "remote content"))
			var requests atomic.Int32
			u.request = func(_ context.Context, _ string, headers map[string][]string) (*http.Response, error) {
				if requests.Add(1) == 1 {
					response := uiTestResponse(archive)
					response.Header.Set("ETag", `"remote-release"`)
					return response, nil
				}
				if http.Header(headers).Get("If-None-Match") != `"remote-release"` {
					t.Error("unchanged release caused a full archive request")
				}
				response := uiTestResponse(nil)
				response.StatusCode = http.StatusNotModified
				return response, nil
			}
			uiTestActivateUpdater(t, u)
			ticker := clock.ticker(t, 0)
			uiTestAutomaticWait(t, func() bool {
				data, _ := os.ReadFile(filepath.Join(u.externalUIPath, "index.html"))
				return requests.Load() == 1 && string(data) == test.want && uiTestAutomaticSettled(u)
			})
			uiTestExpireCheck(u)
			ticker.fire(t, *u.automatic.next.Load())
			uiTestAutomaticWait(t, func() bool { return requests.Load() == 2 && uiTestAutomaticSettled(u) })
			if uiTestIndex(t, u) != test.want {
				t.Fatal("conditional check bypassed version ordering")
			}
		})
	}
}

func TestUIUpdaterAutomaticPolicyCancellationInterruptsChecks(t *testing.T) {
	for _, change := range []string{"disable", "interval", "source", "directory"} {
		t.Run(change, func(t *testing.T) {
			u := uiTestUpdater(t, uiTestAutomaticFiles("0.3.6", "existing"))
			u.autoUpdateUI, u.updateInterval = true, 1
			clock := &uiTestAutomaticClock{}
			u.newTicker = clock.newTicker
			started, canceled := make(chan struct{}), make(chan struct{})
			u.request = func(ctx context.Context, _ string, _ map[string][]string) (*http.Response, error) {
				close(started)
				<-ctx.Done()
				close(canceled)
				return nil, ctx.Err()
			}
			uiTestActivateUpdater(t, u)
			uiTestAutomaticDone(t, started)
			if err := u.DownloadUI(); !errors.Is(err, ErrUIUpdateBusy) {
				t.Fatalf("manual installation overlapped automatic checking: %v", err)
			}
			v := uiTestAutomaticClone(u, true, 1)
			switch change {
			case "disable":
				v.autoUpdateUI = false
			case "interval":
				v.updateInterval = 2
			case "source":
				// Isolate stale-source invalidation from a new source's own
				// activation check; these sources are private test fixtures.
				v.autoUpdateUI = false
				v.externalUIURL = "https://new.example.test/ui.zip"
			case "directory":
				v.autoUpdateUI = false
				v.externalUIPath = filepath.Join(t.TempDir(), "different-ui")
				uiTestWrite(t, v.externalUIPath, uiTestAutomaticFiles("0.3.6", "other existing UI"))
			}
			archive := uiTestZIP(t, uiTestAutomaticFiles("0.3.7", "manual after cancellation"))
			v.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
				return uiTestResponse(archive), nil
			}
			activated := make(chan struct{})
			go func() { SetUIUpdater(v); close(activated) }()
			uiTestAutomaticDone(t, activated)
			uiTestAutomaticDone(t, canceled)
			uiTestAutomaticDone(t, u.automatic.done)
			uiTestAutomaticWait(t, func() bool { return uiTestAutomaticSettled(u) })
			if (change == "source" || change == "directory") && u.active() {
				t.Fatal("the previous source remained active after replacement")
			}
			if uiTestIndex(t, u) != "existing" {
				t.Fatal("canceled automatic check changed installed files")
			}
			status := v.Status(context.Background(), true)
			if status.CheckError != "" || !status.UpdateAvailable {
				t.Fatalf("canceled automatic policy poisoned manual checks: %+v", status)
			}
			if err := v.DownloadUI(); err != nil || uiTestIndex(t, v) != "manual after cancellation" {
				t.Fatalf("manual installation stopped working after policy change: %v", err)
			}
		})
	}
}

func TestUIUpdaterAutomaticCancellationPreventsLateCommit(t *testing.T) {
	u := uiTestUpdater(t, uiTestAutomaticFiles("0.3.6", "existing"))
	u.autoUpdateUI, u.updateInterval = true, 1
	clock := &uiTestAutomaticClock{}
	u.newTicker = clock.newTicker
	archive := uiTestZIP(t, uiTestAutomaticFiles("0.3.7", "late update"))
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	ready, release := make(chan struct{}), make(chan struct{})
	u.beforeCommit = func() { close(ready); <-release }
	uiTestActivateUpdater(t, u)
	uiTestAutomaticDone(t, ready)
	v := uiTestAutomaticClone(u, false, 1)
	SetUIUpdater(v)
	close(release)
	uiTestAutomaticDone(t, u.automatic.done)
	uiTestAutomaticWait(t, func() bool { return uiTestAutomaticSettled(u) })
	if uiTestIndex(t, u) != "existing" {
		t.Fatal("automatic installation committed after the policy was disabled")
	}
}

func TestUIUpdaterAutomaticBusyTickPreservesManualInstallation(t *testing.T) {
	u := uiTestUpdater(t, uiTestAutomaticFiles("0.3.6", "existing"))
	u.autoUpdateUI, u.updateInterval = true, 1
	clock := &uiTestAutomaticClock{}
	u.newTicker = clock.newTicker
	archive := uiTestZIP(t, uiTestAutomaticFiles("0.3.7", "manual update"))
	currentArchive := uiTestZIP(t, uiTestAutomaticFiles("0.3.6", "existing"))
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	u.request = func(ctx context.Context, _ string, _ map[string][]string) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return uiTestResponse(currentArchive), nil
		}
		close(started)
		select {
		case <-release:
			return uiTestResponse(archive), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	uiTestActivateUpdater(t, u)
	uiTestAutomaticWait(t, func() bool { return requests.Load() == 1 && uiTestAutomaticSettled(u) })
	finished := make(chan error, 1)
	go func() { finished <- u.DownloadUI() }()
	uiTestAutomaticDone(t, started)
	tick := *u.automatic.next.Load()
	clock.ticker(t, 0).fire(t, tick)
	uiTestAutomaticWait(t, func() bool { return u.automatic.next.Load().After(tick) })
	SetUIUpdater(uiTestAutomaticClone(u, false, 1))
	uiTestAutomaticDone(t, u.automatic.done)
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("disabling the automatic policy canceled a manual update: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manual update did not complete")
	}
	if requests.Load() != 2 || uiTestIndex(t, u) != "manual update" {
		t.Fatal("scheduled checking overlapped the manual installation")
	}
}

func TestUIUpdaterAutomaticRetriesOnNextCycle(t *testing.T) {
	u := uiTestUpdater(t, uiTestAutomaticFiles("0.3.6", "existing"))
	u.autoUpdateUI, u.updateInterval = true, 1
	clock := &uiTestAutomaticClock{}
	u.newTicker = clock.newTicker
	archive := uiTestZIP(t, uiTestAutomaticFiles("0.3.7", "recovered"))
	var requests atomic.Int32
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return nil, errors.New("temporary outage")
		}
		return uiTestResponse(archive), nil
	}
	uiTestActivateUpdater(t, u)
	ticker := clock.ticker(t, 0)
	uiTestAutomaticWait(t, func() bool {
		return requests.Load() == 1 && u.Status(context.Background(), false).CheckError != "" && uiTestAutomaticSettled(u)
	})
	if uiTestIndex(t, u) != "existing" {
		t.Fatal("failed scheduled check changed installed files")
	}
	uiTestExpireCheck(u)
	ticker.fire(t, *u.automatic.next.Load())
	uiTestAutomaticWait(t, func() bool {
		data, _ := os.ReadFile(filepath.Join(u.externalUIPath, "index.html"))
		return requests.Load() == 2 && string(data) == "recovered" && uiTestAutomaticSettled(u)
	})
}
