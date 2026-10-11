package updater

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/metacubex/http"
)

func uiPolicyFiles(t *testing.T, version, html string) map[string]string {
	t.Helper()
	files := map[string]string{"index.html": html}
	if version != "" {
		data, err := json.Marshal(uiBuildInfo{Name: "clash-dashboard", Version: version})
		if err != nil {
			t.Fatal(err)
		}
		files["build-info.json"] = string(data)
	}
	return files
}

func TestUIUpdaterAvailabilityAndAutomaticInstallUseVersions(t *testing.T) {
	for _, test := range []struct {
		name, current, latest string
		available, missing    bool
	}{
		{"newer", "0.3.6", "0.3.7", true, false},
		{"numeric-order", "0.3.9", "0.3.10", true, false},
		{"same-version-different-files", "0.3.7", "0.3.7", false, false},
		{"older-version-different-files", "0.3.7", "0.3.6", false, false},
		{"build-only-change", "0.3.7+one", "0.3.7+two", false, false},
		{"release-over-prerelease", "0.3.7-rc.1", "0.3.7", true, false},
		{"missing-remote", "0.3.7", "", false, true},
		{"missing-local", "", "0.3.7", false, true},
		{"invalid-remote", "0.3.7", "latest", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			u := uiTestUpdater(t, uiPolicyFiles(t, test.current, "installed"))
			archive := uiTestZIP(t, uiPolicyFiles(t, test.latest, "remote"))
			u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
				return uiTestResponse(archive), nil
			}
			status := u.Status(context.Background(), true)
			if !status.Supported || status.CheckError != "" || status.UpdateAvailable != test.available || (status.Reason == "version-unavailable") != test.missing {
				t.Fatalf("unexpected version policy: %+v", status)
			}
			if status.CurrentDigest == status.LatestDigest {
				t.Fatal("fixture must have different file content to distinguish version and digest policy")
			}
			if err := u.automaticUpdate(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := "installed"
			if test.available {
				want = "remote"
			}
			if got := uiTestIndex(t, u); got != want {
				t.Fatalf("automatic update ignored versions: got %q, want %q", got, want)
			}
		})
	}
}

func TestUIUpdaterAutomaticInstallRechecksVersionUnderOperationGate(t *testing.T) {
	u := uiTestUpdater(t, uiPolicyFiles(t, "0.3.6", "old"))
	archive := uiTestZIP(t, uiPolicyFiles(t, "0.3.7", "remote"))
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	if status := u.Status(context.Background(), true); !status.UpdateAvailable {
		t.Fatalf("fixture did not find a newer version: %+v", status)
	}
	// A manual installation can complete after the check but before this
	// automatic operation acquires the shared installation gate.
	uiTestWrite(t, u.externalUIPath, uiPolicyFiles(t, "0.3.8", "installed meanwhile"))
	if err := u.downloadUIContext(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := uiTestIndex(t, u); got != "installed meanwhile" {
		t.Fatalf("automatic update downgraded a newer manual installation: %q", got)
	}
	status := u.Status(context.Background(), false)
	if status.UpdateAvailable || status.CurrentVersion != "0.3.8" || status.CurrentDigest == status.LatestDigest {
		t.Fatalf("skipped automatic installation was recorded as installed: %+v", status)
	}
}

func TestUIUpdaterAutomaticInstallRechecksReplacementArchiveVersion(t *testing.T) {
	u := uiTestUpdater(t, uiPolicyFiles(t, "0.3.6", "old"))
	archive := uiTestZIP(t, uiPolicyFiles(t, "0.3.7", "initially newer"))
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	if status := u.Status(context.Background(), true); !status.UpdateAvailable {
		t.Fatalf("fixture did not find a newer version: %+v", status)
	}
	uiTestExpireCheck(u)
	archive = uiTestZIP(t, uiPolicyFiles(t, "0.3.5", "source rolled back"))
	if err := u.downloadUIContext(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := uiTestIndex(t, u); got != "old" {
		t.Fatalf("automatic install accepted an older replacement archive: %q", got)
	}
}

func TestUIUpdaterAutomaticInstallRechecksVersionAtCommit(t *testing.T) {
	u := uiTestUpdater(t, uiPolicyFiles(t, "0.3.6", "old"))
	archive := uiTestZIP(t, uiPolicyFiles(t, "0.3.7", "remote"))
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	u.beforeCommit = func() {
		uiTestWrite(t, u.externalUIPath, uiPolicyFiles(t, "0.3.8", "newer before commit"))
	}
	if err := u.downloadUIContext(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := uiTestIndex(t, u); got != "newer before commit" {
		t.Fatalf("automatic install failed its final version check: %q", got)
	}
}
