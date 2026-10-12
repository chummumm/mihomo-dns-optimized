package executor

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/transparentproxy"
	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/config"
)

func preserveStopAdmission(t *testing.T) {
	t.Helper()
	mux.Lock()
	previous := stopping
	stopping = false
	mux.Unlock()
	t.Cleanup(func() {
		mux.Lock()
		stopping = previous
		mux.Unlock()
	})
}

// Return callbacks for every entry point that can create configuration
// resources or replace the process. A stopped executor must not enter any of
// the callbacks, including the core installer before restart preparation.
func stoppedMutationAttempts(t *testing.T, calls *atomic.Int32) []func() error {
	t.Helper()
	beginTransparentProxyApply = func(transparentproxy.Config, transparentproxy.Runtime) error {
		calls.Add(1)
		return errors.New("configuration was admitted")
	}
	prepareTransparentRestart = func() error {
		calls.Add(1)
		return errors.New("restart was admitted")
	}
	return []func() error{
		func() error { return ApplyConfig(&config.Config{General: &config.General{}}, true) },
		func() error { return ApplyConfigPatch(func() error { calls.Add(1); return nil }) },
		func() error { return Restart(func() { calls.Add(1) }) },
		func() error {
			return ApplyUpdateAndRestart(func() error { calls.Add(1); return nil }, func() { calls.Add(1) })
		},
	}
}

func TestNormalStopRejectsConfigurationWorkQueuedDuringCleanup(t *testing.T) {
	preserveTransparentProxyIntegration(t)
	preserveUpdateConfiguration(t)
	previousUI := updater.CurrentUIUpdater()
	t.Cleanup(func() { updater.SetUIUpdater(previousUI) })
	entered, release := make(chan struct{}), make(chan struct{})
	closeTransparentProxy = func() error { close(entered); <-release; return nil }
	cleanupTunListener = func() error { return nil }
	var calls atomic.Int32
	mutations := stoppedMutationAttempts(t, &calls)
	stopped := make(chan error, 1)
	go func() { stopped <- Shutdown() }()
	<-entered
	started := make(chan struct{}, len(mutations))
	results := make(chan error, len(mutations))
	for _, mutation := range mutations {
		go func(run func() error) { started <- struct{}{}; results <- run() }(mutation)
	}
	for range mutations {
		<-started
	}
	select {
	case err := <-results:
		close(release)
		<-stopped
		t.Fatalf("configuration transaction bypassed in-progress cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	for range mutations {
		select {
		case err := <-results:
			if !errors.Is(err, ErrShuttingDown) {
				t.Fatalf("queued work was admitted after cleanup: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("queued configuration work did not finish")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("stop allowed %d resource/install callbacks", calls.Load())
	}
}

func TestNormalStopKeepsAdmissionClosedAcrossCleanupRetries(t *testing.T) {
	preserveTransparentProxyIntegration(t)
	preserveUpdateConfiguration(t)
	previousUI := updater.CurrentUIUpdater()
	t.Cleanup(func() { updater.SetUIUpdater(previousUI) })
	failure := errors.New("owned route removal failed")
	cleanupError := failure
	closeTransparentProxy = func() error { return cleanupError }
	cleanupTunListener = func() error { return nil }
	var calls atomic.Int32
	mutations := stoppedMutationAttempts(t, &calls)
	if err := Shutdown(); !errors.Is(err, failure) {
		t.Fatalf("cleanup failure was hidden: %v", err)
	}
	for _, mutation := range mutations {
		if err := mutation(); !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("failed cleanup reopened admission: %v", err)
		}
	}
	cleanupError = nil
	if err := Shutdown(); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range mutations {
		if err := mutation(); !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("completed cleanup reopened admission: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("cleanup retry allowed configuration or executable installation")
	}
}

func TestControlledRestartKeepsRecoveryAdmission(t *testing.T) {
	for _, phase := range []string{"prepare", "TUN cleanup", "restore originals", "callback returned"} {
		t.Run(phase, func(t *testing.T) {
			preserveTransparentProxyIntegration(t)
			preserveUpdateConfiguration(t)
			previousUI := updater.CurrentUIUpdater()
			t.Cleanup(func() { updater.SetUIUpdater(previousUI) })
			rejected := errors.New("restart phase failed")
			outcome := func(name string) error {
				if name == phase {
					return rejected
				}
				return nil
			}
			prepareTransparentRestart = func() error { return outcome("prepare") }
			cleanupTunListener = func() error { return outcome("TUN cleanup") }
			completeTransparentRestart = func() error { return outcome("restore originals") }
			abortTransparentRestart = func(transparentproxy.BackendState) error { return nil }
			callback := false
			err := Restart(func() { callback = true })
			if phase == "callback returned" {
				if err != nil || !callback {
					t.Fatalf("completed restart callback failed: %v", err)
				}
			} else if !errors.Is(err, rejected) || callback {
				t.Fatalf("failed restart phase was not preserved: %v", err)
			}
			patched := false
			if err := ApplyConfigPatch(func() error { patched = true; return nil }); err != nil || !patched {
				t.Fatalf("recoverable restart blocked controller repair: %v", err)
			}
			admitted := errors.New("recovery configuration reached application")
			beginTransparentProxyApply = func(transparentproxy.Config, transparentproxy.Runtime) error { return admitted }
			if err := ApplyConfig(&config.Config{General: &config.General{}}, true); !errors.Is(err, admitted) {
				t.Fatalf("recoverable restart blocked full configuration application: %v", err)
			}
		})
	}
}
