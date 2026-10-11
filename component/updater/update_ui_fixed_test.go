package updater

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/http"
	C "github.com/metacubex/mihomo/constant"
)

func TestUIUpdaterFixedSourceAndDirectory(t *testing.T) {
	previousHome := C.Path.HomeDir()
	C.SetHomeDir(t.TempDir())
	t.Cleanup(func() { C.SetHomeDir(previousHome) })
	for _, u := range []*UIUpdater{
		NewUiUpdater("/legacy/custom-ui", "https://old.example/other.zip", "nested"),
		NewUiUpdaterWithAutoUpdate("../unsafe", "not a URL", "../../escape", true, 6),
		NewFixedUIUpdater(false, 24),
	} {
		if u.externalUIPath != filepath.Join(C.Path.HomeDir(), "ui") || u.externalUIURL != UIArchiveURL || !u.autoDownloadUI {
			t.Fatalf("custom option changed managed panel location: %+v", u)
		}
		if _, err := u.target(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUIUpdaterAutomaticRetriesFailedInitialInstall(t *testing.T) {
	u := uiTestUpdater(t, nil)
	u.autoUpdateUI = true
	clock := &uiTestAutomaticClock{}
	u.newTicker = clock.newTicker
	archive := uiTestZIP(t, uiTestAutomaticFiles("0.4.0", "installed after recovery"))
	var requests atomic.Int32
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return nil, errors.New("temporary bootstrap network failure")
		}
		return uiTestResponse(archive), nil
	}
	uiTestActivateUpdater(t, u)
	uiTestAutomaticWait(t, func() bool { return requests.Load() == 1 && uiTestAutomaticSettled(u) })
	clock.ticker(t, 0).fire(t, time.Now().Add(24*time.Hour))
	uiTestAutomaticWait(t, func() bool { return requests.Load() == 2 && uiTestAutomaticSettled(u) })
	if uiTestIndex(t, u) != "installed after recovery" {
		t.Fatal("scheduled check could not recover an empty managed UI directory")
	}
}

func TestUIUpdaterNewDistributionMetadataName(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{
		"index.html":      "previous panel",
		"build-info.json": `{"name":"clash-dashboard","version":"0.3.7"}`,
	})
	archive := uiTestZIP(t, map[string]string{
		"index.html":      "new panel",
		"build-info.json": `{"name":"mihomo-optimized","version":"0.4.0"}`,
	})
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	if status := u.Status(context.Background(), true); !status.UpdateAvailable || status.LatestVersion != "0.4.0" {
		t.Fatalf("new distribution metadata was rejected: %+v", status)
	}
	if err := u.downloadUIContext(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if info := readUIBuildInfo(u.externalUIPath); info.Name != "mihomo-optimized" || info.Version != "0.4.0" {
		t.Fatalf("managed metadata did not survive installation: %+v", info)
	}
}
