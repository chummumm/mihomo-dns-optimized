package executor

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
)

func TestCoreUpdaterAppliedConfigAndTemporaryParsing(t *testing.T) {
	previousUI := updater.CurrentUIUpdater()
	previousHome := C.Path.HomeDir()
	coreEnabled, coreInterval := updater.DefaultCoreUpdater.AutoUpdate(), updater.DefaultCoreUpdater.UpdateInterval()
	geoEnabled, geoInterval := updater.GeoAutoUpdate(), updater.GeoUpdateInterval()
	t.Cleanup(func() {
		updater.StopAutomaticUpdates()
		updater.DefaultCoreUpdater.SetAutomaticRestartHandler(nil)
		updater.DefaultCoreUpdater.ConfigureAutoUpdate(coreEnabled, coreInterval)
		updater.SetUIUpdater(previousUI)
		updater.SetGeoAutoUpdate(geoEnabled)
		updater.SetGeoUpdateInterval(geoInterval)
		C.SetHomeDir(previousHome)
	})
	C.SetHomeDir(t.TempDir())
	if err := os.MkdirAll(updater.UIPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(updater.UIPath(), "index.html"), []byte("installed panel"), 0o644); err != nil {
		t.Fatal(err)
	}
	updater.DefaultCoreUpdater.SetAutomaticRestartHandler(func(string, func() error) error { return errors.New("test must never restart") })
	cfg := &config.Config{General: &config.General{CoreAutoUpdate: true, CoreUpdateInterval: 6, ExternalUIUpdateInterval: 24}}
	updateUpdater(cfg)
	general, next := GetGeneral(), updater.DefaultCoreUpdater.NextCheck()
	if !general.CoreAutoUpdate || general.CoreUpdateInterval != 6 || next == "" {
		t.Fatal("applied YAML did not start the core schedule")
	}
	candidate := *general
	candidate.CoreAutoUpdate, candidate.CoreUpdateInterval = false, 1
	restore := temporaryUpdateGeneral(&candidate)
	restore()
	if !updater.DefaultCoreUpdater.AutoUpdate() || updater.DefaultCoreUpdater.NextCheck() != next {
		t.Fatal("temporary config parsing changed the active core schedule")
	}
	updateUpdater(cfg)
	if updater.DefaultCoreUpdater.NextCheck() != next {
		t.Fatal("ordinary reload postponed the core schedule")
	}
	cfg.General.CoreAutoUpdate, cfg.General.CoreUpdateInterval = false, 12
	updateUpdater(cfg)
	general = GetGeneral()
	if general.CoreAutoUpdate || general.CoreUpdateInterval != 12 || updater.DefaultCoreUpdater.NextCheck() != "" {
		t.Fatal("YAML reload did not disable the core scheduler")
	}
}

func TestCoreUpdaterRestartBridgeWaitsForConfiguration(t *testing.T) {
	var committed, restarted atomic.Bool
	rejected := errors.New("policy changed during download")
	started, finished := make(chan struct{}), make(chan error, 1)
	mux.Lock()
	go func() {
		close(started)
		finished <- ApplyUpdateAndRestart(func() error { committed.Store(true); return rejected }, func() { restarted.Store(true) })
	}()
	<-started
	select {
	case <-finished:
		mux.Unlock()
		t.Fatal("installation bypassed the active configuration transaction")
	case <-time.After(20 * time.Millisecond):
	}
	if committed.Load() {
		mux.Unlock()
		t.Fatal("installation ran during config reload")
	}
	mux.Unlock()
	select {
	case err := <-finished:
		if !errors.Is(err, rejected) || !committed.Load() || restarted.Load() {
			t.Fatal("rejected installation shut down or restarted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart bridge deadlocked after configuration completed")
	}
}

func TestCoreUpdaterRestartBridgeKeepsCommitAndRestartTogether(t *testing.T) {
	previousUI := updater.CurrentUIUpdater()
	t.Cleanup(func() { updater.SetUIUpdater(previousUI) })
	entered, release := make(chan struct{}), make(chan struct{})
	finished, reloaded := make(chan error, 1), make(chan struct{})
	var committed atomic.Bool
	go func() {
		finished <- ApplyUpdateAndRestart(func() error { committed.Store(true); return nil }, func() {
			close(entered)
			<-release // isolate the real process restart from this test
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("commit/shutdown/restart bridge deadlocked")
	}
	go func() { mux.Lock(); mux.Unlock(); close(reloaded) }()
	select {
	case <-reloaded:
		close(release)
		t.Fatal("reload entered between a committed installation and restart")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-finished; err != nil || !committed.Load() {
		t.Fatal("restart transaction failed", err)
	}
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("restart bridge failed to release the config mutex")
	}
}
