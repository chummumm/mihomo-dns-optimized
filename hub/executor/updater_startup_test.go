package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
)

type updaterStartupResolver struct {
	resolver.Resolver
	reset func()
}

func (r updaterStartupResolver) ResetConnection() { r.reset() }

func updaterStartupDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("updater readiness did not reach the expected lifecycle boundary")
	}
}

func updaterStartupConfig(t *testing.T) *config.Config {
	t.Helper()
	previousUI, previousHome := updater.CurrentUIUpdater(), C.Path.HomeDir()
	coreEnabled, coreInterval := updater.DefaultCoreUpdater.AutomaticPolicy()
	previousMain, previousSystem, previousDone := resolver.DefaultResolver, resolver.SystemResolver, updaterResetDone
	t.Cleanup(func() {
		updater.StopAutomaticUpdates()
		updater.DefaultCoreUpdater.SetAutomaticRestartHandler(nil)
		updater.DefaultCoreUpdater.ConfigureAutoUpdate(coreEnabled, coreInterval)
		updater.SetUIUpdater(previousUI)
		resolver.DefaultResolver, resolver.SystemResolver = previousMain, previousSystem
		updaterResetDone = previousDone
		C.SetHomeDir(previousHome)
	})
	C.SetHomeDir(t.TempDir())
	if err := os.MkdirAll(updater.UIPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(updater.UIPath(), "index.html"), []byte("existing panel"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &config.Config{General: &config.General{CoreUpdateInterval: 24, ExternalUIUpdateInterval: 24}}
}

func TestCoreUpdaterStartupResetCapturesResolversAndOrdersGenerations(t *testing.T) {
	cfg := updaterStartupConfig(t)
	mainEntered, systemEntered := make(chan struct{}), make(chan struct{})
	releaseMain, releaseSystem := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseMain); close(releaseSystem) }) }
	t.Cleanup(unblock)
	mainNext, systemNext := make(chan struct{}), make(chan struct{})
	var systemCalls atomic.Int32
	resolver.DefaultResolver = updaterStartupResolver{reset: func() { close(mainEntered); <-releaseMain }}
	resolver.SystemResolver = updaterStartupResolver{reset: func() {
		if systemCalls.Add(1) == 1 {
			close(systemEntered)
			<-releaseSystem
		} else {
			close(systemNext)
		}
	}}
	finish := updateUpdater(cfg)
	returned := make(chan struct{})
	go func() { finish(); close(returned) }()
	updaterStartupDone(t, returned)
	firstDone := updaterResetDone
	// The first reset owns its captured instance even after the global is
	// replaced. Both resets can enter while the first network lock is held.
	resolver.DefaultResolver = updaterStartupResolver{reset: func() { close(mainNext) }}
	updaterStartupDone(t, mainEntered)
	updaterStartupDone(t, systemEntered)
	finishNext := updateUpdater(cfg)
	finishNext()
	secondDone := updaterResetDone
	updaterStartupDone(t, mainNext)
	updaterStartupDone(t, systemNext)
	select {
	case <-secondDone:
		t.Fatal("a newer ready barrier ignored an outstanding earlier system reset")
	case <-time.After(25 * time.Millisecond):
	}
	unblock()
	updaterStartupDone(t, firstDone)
	updaterStartupDone(t, secondDone)
	finishNext() // completing the same configuration twice cannot reset again
	if updaterResetDone != secondDone || systemCalls.Load() != 2 {
		t.Fatal("configuration completion repeated its connection reset")
	}
}

func TestCoreUpdaterStartupPendingResetDoesNotDelayPolicyCancellation(t *testing.T) {
	cfg := updaterStartupConfig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	resolver.DefaultResolver = updaterStartupResolver{reset: func() { close(entered); <-release }}
	resolver.SystemResolver = updaterStartupResolver{reset: func() {}}
	updater.DefaultCoreUpdater.SetAutomaticRestartHandler(func(string, func() error) error {
		return errors.New("test must never restart its executable")
	})
	cfg.General.CoreAutoUpdate, cfg.General.ExternalUIAutoUpdate = true, true
	finish := updateUpdater(cfg)
	if updater.DefaultCoreUpdater.NextCheck() == "" || updater.CurrentUIUpdater().Status(context.Background(), false).NextCheck == "" {
		t.Fatal("enabled policies were not published before readiness")
	}
	finish()
	updaterStartupDone(t, entered)
	firstDone := updaterResetDone
	// The readiness barrier remains blocked until both policies are canceled.
	// This fixture never reaches a real HTTP request or host executable install.
	cfg.General.CoreAutoUpdate, cfg.General.ExternalUIAutoUpdate = false, false
	var finishDisabled func()
	applied := make(chan struct{})
	go func() { finishDisabled = updateUpdater(cfg); close(applied) }()
	updaterStartupDone(t, applied)
	status := updater.CurrentUIUpdater().Status(context.Background(), false)
	if updater.DefaultCoreUpdater.AutoUpdate() || updater.DefaultCoreUpdater.NextCheck() != "" || status.AutoUpdate || status.NextCheck != "" {
		t.Fatal("a pending reset delayed cancellation of the active update policy")
	}
	resolver.DefaultResolver = updaterStartupResolver{reset: func() {}}
	finishDisabled()
	unblock()
	updaterStartupDone(t, firstDone)
	updaterStartupDone(t, updaterResetDone)
}
