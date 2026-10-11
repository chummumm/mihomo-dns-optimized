package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/config"
)

func TestUIUpdaterAppliedConfigAndTemporaryParsing(t *testing.T) {
	previous := updater.CurrentUIUpdater()
	geoEnabled, geoInterval := updater.GeoAutoUpdate(), updater.GeoUpdateInterval()
	t.Cleanup(func() {
		updater.SetUIUpdater(previous)
		updater.SetGeoAutoUpdate(geoEnabled)
		updater.SetGeoUpdateInterval(geoInterval)
	})
	uiDir := filepath.Join(t.TempDir(), "ui")
	if err := os.MkdirAll(uiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte("existing UI"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		General: &config.General{ExternalUIAutoUpdate: true, ExternalUIUpdateInterval: 6},
		Controller: &config.Controller{
			ExternalUI:    uiDir,
			ExternalUIURL: "https://panels.example.test/dashboard.zip",
		},
	}
	updateUpdater(cfg)
	general := GetGeneral()
	if !general.ExternalUIAutoUpdate || general.ExternalUIUpdateInterval != 6 {
		t.Fatalf("published config lost automatic UI settings: enabled=%v interval=%d", general.ExternalUIAutoUpdate, general.ExternalUIUpdateInterval)
	}
	active := updater.CurrentUIUpdater()
	status := active.Status(context.Background(), false)
	if !status.AutoUpdate || status.UpdateInterval != 6 || status.NextCheck == "" {
		t.Fatalf("applied YAML did not start a core schedule: %+v", status)
	}
	// Parsing a candidate temporarily borrows transport settings. It must not
	// apply that candidate's automatic update policy or create a second timer.
	candidate := *general
	candidate.ExternalUIAutoUpdate = false
	candidate.ExternalUIUpdateInterval = 1
	restore := temporaryUpdateGeneral(&candidate)
	restore()
	if updater.CurrentUIUpdater() != active || active.Status(context.Background(), false).NextCheck != status.NextCheck {
		t.Fatal("temporary config parsing replaced the active UI schedule")
	}
	cfg.General.ExternalUIAutoUpdate = false
	cfg.General.ExternalUIUpdateInterval = 12
	updateUpdater(cfg)
	general = GetGeneral()
	status = updater.CurrentUIUpdater().Status(context.Background(), false)
	if general.ExternalUIAutoUpdate || general.ExternalUIUpdateInterval != 12 || status.AutoUpdate || status.NextCheck != "" {
		t.Fatal("reloading the disabled policy did not stop automatic scheduling")
	}
}
