package executor

import (
	"testing"

	"github.com/metacubex/mihomo/component/updater"
)

func TestGeoAutomaticAppliedConfigAndTemporaryParsing(t *testing.T) {
	cfg := updaterStartupConfig(t)
	cfg.General.GeoAutoUpdate, cfg.General.GeoUpdateInterval = true, 6
	// Withhold completion so this wiring test never contacts a configured
	// server. Policy publication and cancellation must not depend on readiness.
	updateUpdater(cfg)
	general, next := GetGeneral(), updater.DefaultGeoUpdater.NextCheck()
	if !general.GeoAutoUpdate || general.GeoUpdateInterval != 6 || next == "" {
		t.Fatal("applied YAML did not start the GeoData schedule")
	}
	candidate := *general
	candidate.GeoAutoUpdate, candidate.GeoUpdateInterval = false, 1
	restore := temporaryUpdateGeneral(&candidate)
	restore()
	if enabled, hours := updater.DefaultGeoUpdater.AutomaticPolicy(); !enabled || hours != 6 || updater.DefaultGeoUpdater.NextCheck() != next {
		t.Fatal("temporary config parsing changed the active GeoData schedule")
	}
	updateUpdater(cfg)
	if updater.DefaultGeoUpdater.NextCheck() != next {
		t.Fatal("ordinary reload postponed the GeoData schedule")
	}
	cfg.General.GeoUpdateInterval = 2
	updateUpdater(cfg)
	general = GetGeneral()
	if !general.GeoAutoUpdate || general.GeoUpdateInterval != 2 || updater.DefaultGeoUpdater.NextCheck() == next {
		t.Fatal("YAML interval change did not replace the GeoData schedule")
	}
	cfg.General.GeoAutoUpdate = false
	updateUpdater(cfg)
	general = GetGeneral()
	if general.GeoAutoUpdate || general.GeoUpdateInterval != 2 || updater.DefaultGeoUpdater.NextCheck() != "" {
		t.Fatal("YAML reload did not disable the GeoData scheduler")
	}
	cfg.General.GeoAutoUpdate = true
	updateUpdater(cfg)
	updater.StopAutomaticUpdates()
	if updater.DefaultGeoUpdater.NextCheck() != "" {
		t.Fatal("shutdown left the GeoData schedule active")
	}
}
