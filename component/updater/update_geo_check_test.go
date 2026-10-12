package updater

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/geodata"
	"github.com/metacubex/mihomo/component/geodata/router"
	C "github.com/metacubex/mihomo/constant"

	"google.golang.org/protobuf/proto"
)

func geoCheckFixture(t *testing.T) {
	t.Helper()
	previousHome, previousLoader := C.Path.HomeDir(), geodata.LoaderName()
	previous := configuredGeo.Swap(nil)
	C.SetHomeDir(t.TempDir())
	geodata.SetLoader("standard")
	geodata.ClearGeoIPCache()
	geodata.ClearGeoSiteCache()
	t.Cleanup(func() {
		if current := configuredGeo.Swap(previous); current != nil {
			current.cancel()
		}
		C.SetHomeDir(previousHome)
		geodata.SetLoader(previousLoader)
		geodata.ClearGeoIPCache()
		geodata.ClearGeoSiteCache()
	})
}

func geoSiteBytes(t *testing.T, domain string) []byte {
	t.Helper()
	data, err := proto.Marshal(&router.GeoSiteList{Entry: []*router.GeoSite{{CountryCode: "CN", Domain: []*router.Domain{{Type: router.Domain_Full, Value: domain}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func geoIPBytes(t *testing.T, firstByte byte) []byte {
	t.Helper()
	data, err := proto.Marshal(&router.GeoIPList{Entry: []*router.GeoIP{{CountryCode: "CN", Cidr: []*router.CIDR{{Ip: []byte{firstByte, 0, 0, 0}, Prefix: 8}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGeoCheckDownloadsOnlyUsedFamiliesAndAppliesMatchers(t *testing.T) {
	geoCheckFixture(t)
	oldSite, nextSite := geoSiteBytes(t, "old.example"), geoSiteBytes(t, "new.example")
	oldIP, nextIP := geoIPBytes(t, 10), geoIPBytes(t, 20)
	for path, data := range map[string][]byte{C.Path.GeoIP(): oldIP, C.Path.GeoSite(): oldSite} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldSiteMatcher, err := geodata.LoadGeoSiteMatcher("cn")
	if err != nil || !oldSiteMatcher.ApplyDomain("old.example") {
		t.Fatalf("old site fixture: %v", err)
	}
	oldIPMatcher, err := geodata.LoadGeoIPMatcher("cn")
	if err != nil || !oldIPMatcher.Match(netip.MustParseAddr("10.1.1.1")) {
		t.Fatalf("old IP fixture: %v", err)
	}
	var siteBody, ipBody atomic.Value
	siteBody.Store(nextSite)
	ipBody.Store(nextIP)
	var siteRequests, ipRequests, unusedRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/geosite":
			siteRequests.Add(1)
			_, _ = w.Write(siteBody.Load().([]byte))
		case "/geoip":
			ipRequests.Add(1)
			_, _ = w.Write(ipBody.Load().([]byte))
		default:
			unusedRequests.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	config := GeoUpdateConfig{
		Usage: geodata.Usage{GeoIP: true, GeoSite: true}, GeodataMode: true,
		GeoIPURL: server.URL + "/geoip", GeoSiteURL: server.URL + "/geosite",
		MMDBURL: server.URL + "/unused-mmdb", ASNURL: server.URL + "/unused-asn",
	}
	results, err := CheckAndUpdateGeoDatabasesContext(context.Background(), config)
	if err != nil || len(results) != 2 || results[0].Name != "geoip" || results[1].Name != "geosite" || !results[0].Changed || !results[1].Changed {
		t.Fatalf("GeoData updates = %+v, %v", results, err)
	}
	newSiteMatcher, err := geodata.LoadGeoSiteMatcher("cn")
	if err != nil || !newSiteMatcher.ApplyDomain("new.example") || newSiteMatcher.ApplyDomain("old.example") {
		t.Fatalf("new GeoSite matcher not active: %v", err)
	}
	newIPMatcher, err := geodata.LoadGeoIPMatcher("cn")
	if err != nil || !newIPMatcher.Match(netip.MustParseAddr("20.1.1.1")) || newIPMatcher.Match(netip.MustParseAddr("10.1.1.1")) {
		t.Fatalf("new GeoIP matcher not active: %v", err)
	}
	results, err = CheckAndUpdateGeoDatabasesContext(context.Background(), config)
	if err != nil || results[0].Changed || results[1].Changed {
		t.Fatalf("same contents were not recognized: %+v, %v", results, err)
	}
	if siteRequests.Load() != 2 || ipRequests.Load() != 2 || unusedRequests.Load() != 0 {
		t.Fatalf("configured family selection failed: site=%d, ip=%d, unused=%d", siteRequests.Load(), ipRequests.Load(), unusedRequests.Load())
	}
	// One broken remote must not turn an independently successful update into
	// an unreported success or replace its own still-working database.
	ipBody.Store([]byte("invalid"))
	siteBody.Store(geoSiteBytes(t, "latest.example"))
	results, err = CheckAndUpdateGeoDatabasesContext(context.Background(), config)
	if err == nil || results[0].Err == nil || results[0].Changed || results[1].Err != nil || !results[1].Changed {
		t.Fatalf("partial update result = %+v, %v", results, err)
	}
	data, _ := os.ReadFile(C.Path.GeoIP())
	if !bytes.Equal(data, nextIP) {
		t.Fatal("invalid GeoIP response replaced the valid database")
	}
}

func TestGeoCheckRejectsStaleCommitWithoutChangingFileOrCache(t *testing.T) {
	geoCheckFixture(t)
	old := geoSiteBytes(t, "old.example")
	if err := os.WriteFile(C.Path.GeoSite(), old, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = geodata.LoadGeoSiteMatcher("cn")
	next := geoSiteBytes(t, "new.example")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(next) }))
	defer server.Close()
	stale := errors.New("configuration changed")
	config := GeoUpdateConfig{Usage: geodata.Usage{GeoSite: true}, GeoSiteURL: server.URL, Commit: func(func() error) error { return stale }}
	results, err := CheckAndUpdateGeoDatabasesContext(context.Background(), config)
	if !errors.Is(err, stale) || len(results) != 1 || results[0].Changed || !errors.Is(results[0].Err, stale) {
		t.Fatalf("stale GeoData commit = %+v, %v", results, err)
	}
	data, _ := os.ReadFile(C.Path.GeoSite())
	matcher, matchErr := geodata.LoadGeoSiteMatcher("cn")
	if !bytes.Equal(data, old) || matchErr != nil || !matcher.ApplyDomain("old.example") {
		t.Fatal("rejected GeoData update changed disk or active cache")
	}
}

func TestGeoAutomaticAndManualShareGateAndConfigurationLifetime(t *testing.T) {
	geoCheckFixture(t)
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := GeoUpdateConfig{Usage: geodata.Usage{GeoSite: true}, GeoSiteURL: server.URL, Context: lifetime}
	ConfigureGeoUpdates(config)
	done := make(chan error, 1)
	go func() { done <- UpdateGeoDatabases() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("automatic GeoData request did not start")
	}
	if _, err := CheckAndUpdateGeoDatabases(context.Background()); !errors.Is(err, ErrGetDatabaseUpdateSkip) {
		t.Fatalf("legacy update overlapped automatic update: %v", err)
	}
	waitCtx, stopWaiting := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, err := CheckAndUpdateGeoDatabasesContext(waitCtx, config); waiting <- err }()
	stopWaiting()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unified check did not honor cancellation while waiting: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unified check remained queued after cancellation")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("configuration cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old automatic GeoData download survived its configuration")
	}
	if !geoUpdateGate.TryLock() {
		t.Fatal("cancelled GeoData update did not release the gate")
	}
	geoUpdateGate.Unlock()
}

func TestGeoConfiguredSelectionUsesSnapshotRatherThanHistoricFlags(t *testing.T) {
	geoCheckFixture(t)
	oldUsage := geodata.CurrentUsage()
	t.Cleanup(func() { geodata.SetUsage(oldUsage) })
	geodata.SetUsage(geodata.Usage{GeoIP: true, GeoSite: true, ASN: true})
	ConfigureGeoUpdates(GeoUpdateConfig{Usage: geodata.Usage{GeoSite: true}})
	if got := ActiveGeoDatabases(); !reflect.DeepEqual(got, []string{"geosite"}) {
		t.Fatalf("historic global usage leaked into current configuration: %v", got)
	}
	if got := GeoDatabasesForConfig(GeoUpdateConfig{Usage: geodata.Usage{GeoIP: true}, GeodataMode: false}); !reflect.DeepEqual(got, []string{"mmdb"}) {
		t.Fatalf("MMDB mode selected wrong database: %v", got)
	}
	var liveUsage atomic.Value
	liveUsage.Store(geodata.Usage{})
	ConfigureGeoUpdates(GeoUpdateConfig{UsageNow: func() geodata.Usage { return liveUsage.Load().(geodata.Usage) }})
	if got := ActiveGeoDatabases(); len(got) != 0 {
		t.Fatalf("empty current provider strategy inherited global usage: %v", got)
	}
	liveUsage.Store(geodata.Usage{ASN: true})
	if got := ActiveGeoDatabases(); !reflect.DeepEqual(got, []string{"asn"}) {
		t.Fatalf("newly committed provider dependency was omitted: %v", got)
	}
	liveUsage.Store(geodata.Usage{})
	if got := ActiveGeoDatabases(); len(got) != 0 {
		t.Fatalf("removed provider dependency remained enabled: %v", got)
	}
}
