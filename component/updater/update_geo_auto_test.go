package updater

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/geodata"
	C "github.com/metacubex/mihomo/constant"
)

func geoAutomaticJob(t *testing.T, u *GeoUpdater) *geoAutomaticUpdate {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.automatic == nil {
		t.Fatal("GeoData scheduler has no active worker")
	}
	return u.automatic
}

func stopGeoAutomatic(t *testing.T, u *GeoUpdater) {
	t.Helper()
	u.mu.Lock()
	job := u.automatic
	u.stopAutomaticUpdatesLocked()
	u.mu.Unlock()
	if job != nil {
		uiTestAutomaticDone(t, job.done)
	}
}

func TestGeoAutomaticPolicyReloadAndReadiness(t *testing.T) {
	geoCheckFixture(t)
	ConfigureGeoUpdates(GeoUpdateConfig{Usage: geodata.Usage{GeoSite: true}})
	clock := &uiTestAutomaticClock{}
	checks := make(chan struct{}, 8)
	u := &GeoUpdater{newTicker: clock.newTicker, check: func(context.Context) error {
		checks <- struct{}{}
		return nil
	}}
	t.Cleanup(func() { stopGeoAutomatic(t, u) })
	u.ConfigureAutoUpdate(false, 24)
	if u.NextCheck() != "" {
		t.Fatal("disabled policy scheduled a check")
	}
	firstReady, latestReady := make(chan struct{}), make(chan struct{})
	u.ConfigureAutoUpdateWhenReady(true, 6, firstReady)
	firstJob, next := geoAutomaticJob(t, u), u.NextCheck()
	if enabled, interval := u.AutomaticPolicy(); !enabled || interval != 6 || next == "" {
		t.Fatal("enabling GeoData did not publish its complete policy")
	}
	if ticker := clock.ticker(t, 0); ticker.interval != 6*time.Hour {
		t.Fatal("configured interval did not reach the timer")
	}
	u.ConfigureAutoUpdateWhenReady(true, 6, latestReady)
	close(firstReady)
	select {
	case <-checks:
		t.Fatal("old configuration readiness allowed a GeoData check")
	case <-time.After(20 * time.Millisecond):
	}
	close(latestReady)
	uiTestAutomaticDone(t, checks)
	if geoAutomaticJob(t, u) != firstJob || u.NextCheck() != next {
		t.Fatal("same-policy reload restarted or postponed the schedule")
	}
	clock.ticker(t, 0)

	u.ConfigureAutoUpdate(true, 2)
	uiTestAutomaticDone(t, firstJob.done)
	secondJob := geoAutomaticJob(t, u)
	secondTicker := clock.ticker(t, 1)
	if secondTicker.interval != 2*time.Hour {
		t.Fatal("interval reload retained the old timer")
	}
	select {
	case <-checks:
		t.Fatal("interval-only change started an extra activation check")
	case <-time.After(20 * time.Millisecond):
	}
	secondTicker.fire(t, *secondJob.next.Load())
	uiTestAutomaticDone(t, checks)
	u.ConfigureAutoUpdate(false, 2)
	uiTestAutomaticDone(t, secondJob.done)
	uiTestAutomaticDone(t, secondTicker.stopped)
	if enabled, hours := u.AutomaticPolicy(); enabled || hours != 2 || u.NextCheck() != "" {
		t.Fatal("disabling GeoData retained a schedule or lost the interval")
	}
	u.ConfigureAutoUpdate(true, 2)
	uiTestAutomaticDone(t, checks)
	clock.ticker(t, 2)
	stopGeoAutomatic(t, u)
	if u.NextCheck() != "" {
		t.Fatal("stopped GeoData worker still reported a next check")
	}
}

func TestGeoAutomaticInitialFailureRetriesOnNextCycle(t *testing.T) {
	geoCheckFixture(t)
	next := geoSiteBytes(t, "recovered.example")
	var requests atomic.Int32
	started := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := requests.Add(1)
		started <- struct{}{}
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(next)
	}))
	t.Cleanup(server.Close)
	ConfigureGeoUpdates(GeoUpdateConfig{Usage: geodata.Usage{GeoSite: true}, GeoSiteURL: server.URL})
	clock := &uiTestAutomaticClock{}
	u := &GeoUpdater{newTicker: clock.newTicker}
	t.Cleanup(func() { stopGeoAutomatic(t, u) })
	u.ConfigureAutoUpdate(true, 1)
	job := geoAutomaticJob(t, u)
	uiTestAutomaticDone(t, started)
	// The unbuffered tick cannot be received until the failed activation has
	// returned. Missing local files and a transient HTTP failure must leave
	// the policy alive so this later real download can commit.
	clock.ticker(t, 0).fire(t, *job.next.Load())
	uiTestAutomaticDone(t, started)
	uiTestAutomaticWait(t, func() bool {
		data, _ := os.ReadFile(C.Path.GeoSite())
		return bytes.Equal(data, next)
	})
	if requests.Load() != 2 || geoAutomaticJob(t, u) != job {
		t.Fatal("failed GeoData activation lost or duplicated its schedule")
	}
}

func TestGeoAutomaticPolicyCancellationPreservesManualUpdates(t *testing.T) {
	for _, change := range []string{"disable", "interval"} {
		t.Run(change, func(t *testing.T) {
			geoCheckFixture(t)
			old, next := geoSiteBytes(t, "old.example"), geoSiteBytes(t, "manual.example")
			if err := os.WriteFile(C.Path.GeoSite(), old, 0o644); err != nil {
				t.Fatal(err)
			}
			stale := time.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(C.Path.GeoSite(), stale, stale); err != nil {
				t.Fatal(err)
			}
			started, canceled := make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					close(started)
					<-r.Context().Done()
					close(canceled)
					return
				}
				_, _ = w.Write(next)
			}))
			t.Cleanup(server.Close)
			ConfigureGeoUpdates(GeoUpdateConfig{Usage: geodata.Usage{GeoSite: true}, GeoSiteURL: server.URL})
			clock := &uiTestAutomaticClock{}
			u := &GeoUpdater{newTicker: clock.newTicker}
			t.Cleanup(func() { stopGeoAutomatic(t, u) })
			u.ConfigureAutoUpdate(true, 1)
			job := geoAutomaticJob(t, u)
			uiTestAutomaticDone(t, started)
			if change == "disable" {
				u.ConfigureAutoUpdate(false, 1)
			} else {
				u.ConfigureAutoUpdate(true, 2)
			}
			uiTestAutomaticDone(t, canceled)
			uiTestAutomaticDone(t, job.done)
			data, err := os.ReadFile(C.Path.GeoSite())
			if err != nil || !bytes.Equal(data, old) {
				t.Fatal("canceled automatic request changed the installed database")
			}
			results, err := CheckAndUpdateGeoDatabases(context.Background())
			if err != nil || len(results) != 1 || !results[0].Changed {
				t.Fatalf("manual update failed after automatic policy cancellation: %+v, %v", results, err)
			}
			data, err = os.ReadFile(C.Path.GeoSite())
			if err != nil || !bytes.Equal(data, next) || requests.Load() != 2 {
				t.Fatal("policy cancellation disrupted or duplicated the manual update")
			}
		})
	}
}

func TestGeoAutomaticAgeCheckSelectsOnlyActiveDatabases(t *testing.T) {
	geoCheckFixture(t)
	now := time.Now()
	old, fresh := now.Add(-48*time.Hour), now.Add(-time.Hour)
	for path, modified := range map[string]time.Time{
		C.Path.GeoIP(): old, C.Path.MMDB(): fresh, C.Path.GeoSite(): fresh,
	} {
		if err := os.WriteFile(path, []byte("age fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name   string
		config GeoUpdateConfig
		due    bool
	}{
		{"unused files", GeoUpdateConfig{}, false},
		{"unused old GeoIP", GeoUpdateConfig{Usage: geodata.Usage{GeoSite: true}}, false},
		{"active old GeoIP", GeoUpdateConfig{Usage: geodata.Usage{GeoIP: true, GeoSite: true}, GeodataMode: true}, true},
		{"MMDB mode", GeoUpdateConfig{Usage: geodata.Usage{GeoIP: true}}, false},
		{"missing active ASN", GeoUpdateConfig{Usage: geodata.Usage{ASN: true}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ConfigureGeoUpdates(test.config)
			due, err := geoAutomaticUpdateDue(24*time.Hour, now)
			if err != nil || due != test.due {
				t.Fatalf("startup age check = %v, %v; want %v", due, err, test.due)
			}
		})
	}
}

func TestGeoAutomaticEmbeddedIntervalBounds(t *testing.T) {
	geoCheckFixture(t)
	ConfigureGeoUpdates(GeoUpdateConfig{})
	for _, hours := range []int{0, -1, 2562048, 2562047} {
		clock := &uiTestAutomaticClock{}
		u := &GeoUpdater{newTicker: clock.newTicker}
		u.ConfigureAutoUpdate(true, hours)
		want := hours
		if want <= 0 || want > 2562047 {
			want = 24
		}
		if enabled, actual := u.AutomaticPolicy(); !enabled || actual != want {
			t.Fatalf("embedded interval %d became %d, want %d", hours, actual, want)
		}
		if got := clock.ticker(t, 0).interval; got != time.Duration(want)*time.Hour {
			t.Fatalf("embedded interval %d overflowed timer duration: %v", hours, got)
		}
		stopGeoAutomatic(t, u)
	}
}
