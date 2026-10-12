package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/metacubex/http"
)

func TestUIUnifiedUpdateRechecksRemoteAndComparesVersions(t *testing.T) {
	for _, test := range []struct {
		name, current, latest string
		changed, invalid      bool
	}{
		{"newer", "1.0.0", "1.0.1", true, false},
		{"same-changed-bytes", "1.0.1", "1.0.1", false, false},
		{"older", "1.0.2", "1.0.1", false, false},
		{"missing-current", "", "1.0.1", false, true},
		{"missing-latest", "1.0.1", "", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			u := uiTestUpdater(t, uiTestAutomaticFiles(test.current, "existing"))
			var requests atomic.Int64
			remote := uiTestZIP(t, uiTestAutomaticFiles(test.latest, "new content"))
			u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
				requests.Add(1)
				return uiTestResponse(remote), nil
			}
			_ = u.Status(context.Background(), true)
			if requests.Load() != 1 {
				t.Fatal("fixture failed to populate the check cache")
			}
			result, err := u.CheckAndUpdateContext(context.Background())
			if test.invalid != errors.Is(err, ErrUIVersionUnavailable) || !test.invalid && err != nil {
				t.Fatalf("unexpected result: %+v %v", result, err)
			}
			if result.Updated != test.changed || requests.Load() != 2 || result.CurrentVersion != test.current || result.LatestVersion != test.latest {
				t.Fatalf("manual check reused cache or miscompared version: %+v requests=%d", result, requests.Load())
			}
			content, err := os.ReadFile(filepath.Join(u.externalUIPath, "index.html"))
			want := "existing"
			if test.changed {
				want = "new content"
			}
			if err != nil || string(content) != want {
				t.Fatalf("unexpected installed UI content %q %v", content, err)
			}
		})
	}
}

func TestUIUnifiedUpdateBootstrapsEmptyManagedDirectory(t *testing.T) {
	u := uiTestUpdater(t, nil)
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(uiTestZIP(t, uiTestAutomaticFiles("1.0.1", "initial"))), nil
	}
	result, err := u.CheckAndUpdateContext(context.Background())
	if err != nil || !result.Updated || result.CurrentVersion != "" || result.LatestVersion != "1.0.1" {
		t.Fatalf("empty managed directory was not installed: %+v %v", result, err)
	}
}
