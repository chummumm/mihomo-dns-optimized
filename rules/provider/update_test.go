package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/common/utils"
	"github.com/metacubex/mihomo/component/geodata"
	"github.com/metacubex/mihomo/component/geodata/router"
	"github.com/metacubex/mihomo/component/resource"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
	"github.com/metacubex/mihomo/rules/common"

	"google.golang.org/protobuf/proto"
)

type updateTestTunnel struct {
	callback *utils.Callback[P.RuleProvider]
}

type declaredGeoRule struct {
	typeID C.RuleType
	text   string
}

func (r declaredGeoRule) RuleType() C.RuleType                                { return r.typeID }
func (r declaredGeoRule) Match(*C.Metadata, C.RuleMatchHelper) (bool, string) { return false, "" }
func (r declaredGeoRule) Adapter() string                                     { return "" }
func (r declaredGeoRule) Payload() string                                     { return r.text }
func (r declaredGeoRule) ProviderNames() []string                             { return nil }

func TestRuleProviderGeoUsageFollowsOnlyPublishedStrategy(t *testing.T) {
	previousTunnel, previousUsage := tunnel, geodata.CurrentUsage()
	SetTunnel(updateTestTunnel{callback: utils.NewCallback[P.RuleProvider]()})
	t.Cleanup(func() { SetTunnel(previousTunnel); geodata.SetUsage(previousUsage) })
	var body atomic.Value
	body.Store("payload:\n  - DOMAIN,example.test\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body.Load().(string))) }))
	defer server.Close()
	parse := func(tp, payload, target string, params []string, subRules map[string][]C.Rule) (C.Rule, error) {
		kind := C.Domain
		switch tp {
		case "GEOSITE":
			kind = C.GEOSITE
		case "GEOIP":
			kind = C.GEOIP
		case "IP-ASN":
			kind = C.IPASN
		}
		// Simulate InitGeo* calls made by a concurrently parsed candidate. Its
		// flags must not become the live provider's dependency declaration.
		geodata.SetUsage(geodata.Usage{GeoIP: true, GeoSite: true, ASN: true})
		return declaredGeoRule{typeID: kind, text: payload}, nil
	}
	provider := NewRuleSetProvider("dynamic", P.Classical, P.YamlRule, 0,
		resource.NewHTTPVehicle(server.URL, filepath.Join(t.TempDir(), "rules.yaml"), "", nil, time.Second, 1024), nil, nil, parse).(*RuleSetProvider)
	defer provider.Close()
	if _, err := provider.CheckAndUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := geodata.UsageForProvider(provider); got != (geodata.Usage{}) {
		t.Fatalf("candidate/global flags leaked into published strategy: %+v", got)
	}
	body.Store("payload:\n  - GEOSITE,cn\n  - GEOIP,cn\n")
	if _, err := provider.CheckAndUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := geodata.UsageForProvider(provider); got != (geodata.Usage{GeoIP: true, GeoSite: true}) {
		t.Fatalf("new live dependencies were not discovered: %+v", got)
	}
	body.Store("payload:\n  - IP-ASN,64512\n")
	if _, err := provider.CheckAndUpdateWithCommit(context.Background(), func(func() error) error { return errors.New("stale") }); err == nil {
		t.Fatal("stale candidate was accepted")
	}
	if got := geodata.UsageForProvider(provider); got != (geodata.Usage{GeoIP: true, GeoSite: true}) {
		t.Fatalf("rejected candidate changed live dependencies: %+v", got)
	}
	body.Store("payload:\n  - DOMAIN,example.test\n")
	if _, err := provider.CheckAndUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := geodata.UsageForProvider(provider); got != (geodata.Usage{}) {
		t.Fatalf("removed provider dependencies were retained: %+v", got)
	}
}

func TestRuleProviderFailedGeoInitializationPreservesWorkingRules(t *testing.T) {
	for _, failure := range []string{"http", "cancel", "stale_commit"} {
		failure := failure
		t.Run(failure, func(t *testing.T) {
			previousTunnel, previousHome, previousURL := tunnel, C.Path.HomeDir(), geodata.GeoSiteUrl()
			SetTunnel(updateTestTunnel{callback: utils.NewCallback[P.RuleProvider]()})
			C.SetHomeDir(t.TempDir())
			geodata.ClearGeoSiteCache()
			t.Cleanup(func() {
				SetTunnel(previousTunnel)
				C.SetHomeDir(previousHome)
				geodata.SetGeoSiteUrl(previousURL)
				geodata.ClearGeoSiteCache()
			})
			geoBytes, err := proto.Marshal(&router.GeoSiteList{Entry: []*router.GeoSite{{CountryCode: "CN", Domain: []*router.Domain{{Type: router.Domain_Full, Value: "geo.example"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/provider" {
					_, _ = w.Write([]byte("payload:\n  - GEOSITE,cn\n"))
					return
				}
				switch failure {
				case "http":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "cancel":
					_, _ = w.Write(geoBytes[:len(geoBytes)/2])
					w.(http.Flusher).Flush()
					started <- struct{}{}
					<-r.Context().Done()
				default:
					_, _ = w.Write(geoBytes)
				}
			}))
			defer server.Close()
			geodata.SetGeoSiteUrl(server.URL + "/geosite")
			parse := func(tp, payload, target string, params []string, subRules map[string][]C.Rule) (C.Rule, error) {
				if tp == "GEOSITE" {
					return common.NewGEOSITE(payload, target)
				}
				return common.NewDomain(payload, target), nil
			}
			path := filepath.Join(C.Path.HomeDir(), "provider.yaml")
			provider := NewRuleSetProvider("geo-init", P.Classical, P.YamlRule, 0,
				resource.NewHTTPVehicle(server.URL+"/provider", path, "", nil, time.Second, 1024), nil, nil, parse).(*RuleSetProvider)
			defer provider.Close()
			old := []byte("payload:\n  - DOMAIN,old.example\n")
			if _, _, err := provider.Fetcher.SideUpdate(old); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var commit func(func() error) error
			if failure == "stale_commit" {
				commit = func(func() error) error { return errors.New("configuration changed") }
			}
			done := make(chan error, 1)
			go func() { _, err := provider.CheckAndUpdateWithCommit(ctx, commit); done <- err }()
			if failure == "cancel" {
				select {
				case <-started:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("GeoData initialization did not start")
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("failed GeoData initialization silently removed its rule")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("GeoData initialization did not honor its update context")
			}
			data, _ := os.ReadFile(path)
			if string(data) != string(old) || !provider.Match(&C.Metadata{Host: "old.example"}, C.RuleMatchHelper{}) {
				t.Fatal("failed GeoData initialization replaced the working provider")
			}
			if _, err := os.Stat(C.Path.GeoSite()); !os.IsNotExist(err) {
				t.Fatalf("failed/cancelled initialization left a database file: %v", err)
			}
		})
	}
}

func TestRuleProviderWaitsForCandidateSettingsToBeRestored(t *testing.T) {
	previousTunnel, previousHome, previousURL := tunnel, C.Path.HomeDir(), geodata.GeoSiteUrl()
	SetTunnel(updateTestTunnel{callback: utils.NewCallback[P.RuleProvider]()})
	C.SetHomeDir(t.TempDir())
	geodata.ClearGeoSiteCache()
	t.Cleanup(func() {
		SetTunnel(previousTunnel)
		C.SetHomeDir(previousHome)
		geodata.SetGeoSiteUrl(previousURL)
		geodata.ClearGeoSiteCache()
	})
	data, err := proto.Marshal(&router.GeoSiteList{Entry: []*router.GeoSite{{CountryCode: "CN", Domain: []*router.Domain{{Type: router.Domain_Full, Value: "geo.example"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var activeRequests, candidateRequests atomic.Int32
	providerRead := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			_, _ = w.Write([]byte("payload:\n  - GEOSITE,cn\n"))
			providerRead <- struct{}{}
		case "/active":
			activeRequests.Add(1)
			_, _ = w.Write(data)
		default:
			candidateRequests.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	geodata.SetGeoSiteUrl(server.URL + "/active")
	entered, release, candidateDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		candidateDone <- geodata.WithInitializationContext(context.Background(), nil, func() error {
			geodata.SetGeoSiteUrl(server.URL + "/candidate")
			defer geodata.SetGeoSiteUrl(server.URL + "/active")
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	parse := func(tp, payload, target string, params []string, subRules map[string][]C.Rule) (C.Rule, error) {
		return common.NewGEOSITE(payload, target)
	}
	provider := NewRuleSetProvider("settings", P.Classical, P.YamlRule, 0,
		resource.NewHTTPVehicle(server.URL+"/provider", filepath.Join(C.Path.HomeDir(), "provider.yaml"), "", nil, time.Second, 1024), nil, nil, parse).(*RuleSetProvider)
	defer provider.Close()
	updated := make(chan error, 1)
	go func() { _, err := provider.CheckAndUpdate(context.Background()); updated <- err }()
	select {
	case <-providerRead:
	case <-time.After(time.Second):
		t.Fatal("provider network read was held behind the settings lock")
	}
	close(release)
	if err := <-candidateDone; err != nil {
		t.Fatal(err)
	}
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if activeRequests.Load() != 1 || candidateRequests.Load() != 0 {
		t.Fatalf("provider initialized through candidate settings: active=%d candidate=%d", activeRequests.Load(), candidateRequests.Load())
	}
}

func (t updateTestTunnel) Providers() map[string]P.ProxyProvider               { return nil }
func (t updateTestTunnel) RuleProviders() map[string]P.RuleProvider            { return nil }
func (t updateTestTunnel) RuleUpdateCallback() *utils.Callback[P.RuleProvider] { return t.callback }

func TestRuleProviderCheckUpdatesLiveRulesAndRejectsInvalidOrStaleData(t *testing.T) {
	previousTunnel := tunnel
	SetTunnel(updateTestTunnel{callback: utils.NewCallback[P.RuleProvider]()})
	t.Cleanup(func() { SetTunnel(previousTunnel) })
	var body atomic.Value
	body.Store("payload:\n  - old.example\n")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "test.yaml")
	provider := NewRuleSetProvider("test", P.Domain, P.YamlRule, 0,
		resource.NewHTTPVehicle(server.URL, path, "", nil, time.Second, 1024), nil, nil, nil).(*RuleSetProvider)
	defer provider.Close()
	changed, err := provider.CheckAndUpdate(context.Background())
	if err != nil || !changed || !provider.Match(&C.Metadata{Host: "old.example"}, C.RuleMatchHelper{}) {
		t.Fatalf("initial checked rules not active: %v, %v", changed, err)
	}
	if changed, err := provider.CheckAndUpdate(context.Background()); err != nil || changed {
		t.Fatalf("same rules marked changed: %v, %v", changed, err)
	}
	body.Store("payload:\n  - new.example\n")
	if changed, err := provider.CheckAndUpdate(context.Background()); err != nil || !changed {
		t.Fatalf("new rules were not installed: %v, %v", changed, err)
	}
	if provider.Match(&C.Metadata{Host: "old.example"}, C.RuleMatchHelper{}) || !provider.Match(&C.Metadata{Host: "new.example"}, C.RuleMatchHelper{}) {
		t.Fatal("live matches did not switch to the new rule strategy")
	}
	body.Store("not a provider")
	if _, err := provider.CheckAndUpdate(context.Background()); err == nil {
		t.Fatal("invalid provider response was accepted")
	}
	body.Store("payload:\n  - stale.example\n")
	stale := errors.New("configuration changed")
	if _, err := provider.CheckAndUpdateWithCommit(context.Background(), func(func() error) error { return stale }); !errors.Is(err, stale) {
		t.Fatalf("stale config commit = %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "payload:\n  - new.example\n" || !provider.Match(&C.Metadata{Host: "new.example"}, C.RuleMatchHelper{}) {
		t.Fatal("invalid or stale update replaced the working rule set")
	}
	if requests.Load() != 5 || provider.UpdateInterval() != 0 {
		t.Fatalf("manual check did not read remote or changed auto policy: requests=%d, interval=%s", requests.Load(), provider.UpdateInterval())
	}
}

func TestRuleProviderPublishesStrategyAtomicallyDuringMatching(t *testing.T) {
	previousTunnel := tunnel
	SetTunnel(updateTestTunnel{callback: utils.NewCallback[P.RuleProvider]()})
	t.Cleanup(func() { SetTunnel(previousTunnel) })
	path := filepath.Join(t.TempDir(), "test.yaml")
	provider := NewRuleSetProvider("test", P.Domain, P.YamlRule, 0,
		resource.NewHTTPVehicle("https://unused.example", path, "", nil, time.Second, 1024), nil, nil, nil).(*RuleSetProvider)
	defer provider.Close()
	if _, _, err := provider.Fetcher.SideUpdate([]byte("payload:\n  - stable.example\n")); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if !provider.Match(&C.Metadata{Host: "stable.example"}, C.RuleMatchHelper{}) {
						t.Error("observed an incomplete rule strategy")
						return
					}
					_ = provider.Count()
					_, _ = provider.MarshalJSON()
				}
			}
		}()
	}
	for i := 0; i < 16; i++ {
		data := []byte("payload:\n  - stable.example\n  - first.example\n")
		if i%2 == 0 {
			data = []byte("payload:\n  - stable.example\n  - second.example\n")
		}
		if _, _, err := provider.Fetcher.SideUpdate(data); err != nil {
			t.Error(err)
			break
		}
	}
	close(stop)
	readers.Wait()
}
