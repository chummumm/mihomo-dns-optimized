package executor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/geodata"
	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
	RC "github.com/metacubex/mihomo/rules/common"
)

func preserveUpdateConfiguration(t *testing.T) {
	t.Helper()
	previous := appliedUpdateConfiguration.Swap(nil)
	usage := geodata.CurrentUsage()
	t.Cleanup(func() {
		invalidateUpdateConfiguration()
		appliedUpdateConfiguration.Store(previous)
		geodata.SetUsage(usage)
		if previous != nil {
			updater.ConfigureGeoUpdates(previous.Geo)
		} else {
			updater.ConfigureGeoUpdates(updater.GeoUpdateConfig{Usage: usage, GeodataMode: geodata.GeodataMode(), GeoIPURL: geodata.GeoIpUrl(), MMDBURL: geodata.MmdbUrl(), GeoSiteURL: geodata.GeoSiteUrl(), ASNURL: geodata.ASNUrl()})
		}
	})
}

func TestUnifiedUpdateConfigurationFreezesAppliedSourcesAndPolicies(t *testing.T) {
	preserveUpdateConfiguration(t)
	geodata.SetUsage(geodata.Usage{GeoSite: true})
	cfg := &config.Config{
		Rules:         []C.Rule{&RC.GEOSITE{}},
		General:       &config.General{GeoXUrl: config.GeoXUrl{GeoSite: "https://applied.example/geosite"}, CoreUpdateInterval: 12, GeoAutoUpdate: true},
		RuleProviders: map[string]P.RuleProvider{"original": nil},
	}
	publishUpdateConfiguration(cfg)
	snapshot := CurrentUpdateConfiguration()
	cfg.General.GeoXUrl.GeoSite = "https://candidate.example/not-applied"
	cfg.General.CoreUpdateInterval = 6
	cfg.RuleProviders["not-applied"] = nil
	oldURL := geodata.GeoSiteUrl()
	geodata.SetGeoSiteUrl("https://temporary-parser.example/not-applied")
	t.Cleanup(func() { geodata.SetGeoSiteUrl(oldURL) })
	if snapshot.Geo.GeoSiteURL != "https://applied.example/geosite" || snapshot.General.CoreUpdateInterval != 12 || len(snapshot.RuleProviders) != 1 || !snapshot.Geo.Usage.GeoSite {
		t.Fatalf("update snapshot observed mutable/candidate configuration: %+v", snapshot)
	}
	if snapshot.Context.Err() != nil || snapshot.Geo.Context != snapshot.Context {
		t.Fatal("GeoData did not share the applied configuration's lifetime")
	}
}

func TestUnifiedUpdateConfigurationCancelsBeforeReplacementAndRejectsCommit(t *testing.T) {
	preserveUpdateConfiguration(t)
	publishUpdateConfiguration(&config.Config{General: &config.General{}})
	snapshot := CurrentUpdateConfiguration()
	var committed atomic.Bool
	if err := snapshot.Commit(func() error { committed.Store(true); return nil }); err != nil || !committed.Load() {
		t.Fatalf("active snapshot could not commit: %v", err)
	}
	committed.Store(false)
	invalidateUpdateConfiguration()
	if !errors.Is(snapshot.Context.Err(), context.Canceled) {
		t.Fatal("configuration invalidation did not cancel remote work")
	}
	if err := snapshot.Commit(func() error { committed.Store(true); return nil }); !errors.Is(err, ErrUpdateConfigurationChanged) || committed.Load() {
		t.Fatalf("obsolete configuration committed after reload began: %v", err)
	}
	publishUpdateConfiguration(&config.Config{General: &config.General{}})
	if CurrentUpdateConfiguration() == snapshot || CurrentUpdateConfiguration().Context.Err() != nil {
		t.Fatal("replacement configuration did not get a fresh lifetime")
	}
}

func TestUnifiedUpdateConfigurationCommitAndInvalidationAreOrdered(t *testing.T) {
	preserveUpdateConfiguration(t)
	publishUpdateConfiguration(&config.Config{General: &config.General{}})
	snapshot := CurrentUpdateConfiguration()
	entered, release, committed, invalidated := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() {
		committed <- snapshot.Commit(func() error { close(entered); <-release; return nil })
	}()
	<-entered
	go func() { invalidateUpdateConfiguration(); close(invalidated) }()
	select {
	case <-invalidated:
		close(release)
		t.Fatal("configuration changed in the middle of a file/strategy commit")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	select {
	case <-invalidated:
	case <-time.After(time.Second):
		t.Fatal("configuration invalidation stayed blocked after the short commit")
	}
}

func TestUnifiedUpdateParsingTracksUsageWithoutEnablingOldResources(t *testing.T) {
	previous := geodata.CurrentUsage()
	active := geodata.Usage{GeoIP: true, GeoSite: true, ASN: true}
	geodata.SetUsage(active)
	t.Cleanup(func() { geodata.SetUsage(previous) })
	parsed, err := ParseWithBytes([]byte("dns:\n  enable: false\nrules:\n  - MATCH,DIRECT\n"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.GeoDataUsage != (geodata.Usage{}) || geodata.CurrentUsage() != active {
		t.Fatalf("candidate reused historical GeoData flags or changed active usage: candidate=%+v active=%+v", parsed.GeoDataUsage, geodata.CurrentUsage())
	}
	_, err = ParseWithBytes([]byte("rules:\n  - INVALID-RULE,example.com,DIRECT\n"))
	if err == nil || geodata.CurrentUsage() != active {
		t.Fatalf("failed candidate changed active resource usage: %+v %v", geodata.CurrentUsage(), err)
	}
}
