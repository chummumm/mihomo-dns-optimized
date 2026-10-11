package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"

	"github.com/metacubex/http"
)

func uiTestWrite(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		name = filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func uiTestZIP(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(f, files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func uiTestResponse(data []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}
}

func uiTestUpdater(t *testing.T, files map[string]string) *UIUpdater {
	t.Helper()
	u := NewUiUpdater(filepath.Join(t.TempDir(), "ui"), "https://panels.example.test/private/dashboard.zip?token=private", "")
	u.coordinator = &uiUpdateCoordinator{}
	uiTestWrite(t, u.externalUIPath, files)
	t.Cleanup(func() {
		s := u.state()
		s.operation.Lock()
		defer s.operation.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.expiry != nil {
			s.expiry.Stop()
		}
		if s.latest.work != "" {
			_ = os.RemoveAll(s.latest.work)
		}
	})
	return u
}

func uiTestExpireCheck(u *UIUpdater) {
	u.state().mu.Lock()
	u.state().checked = time.Now().Add(-uiCheckTTL - time.Second)
	u.state().mu.Unlock()
}

func uiTestIndex(t *testing.T, u *UIUpdater) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(u.externalUIPath, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUIUpdaterConfiguredSourceAndPinnedGenericSnapshot(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "old"})
	archive := uiTestZIP(t, map[string]string{"any-name/index.html": "new"})
	var calls atomic.Int32
	u.request = func(_ context.Context, source string, _ map[string][]string) (*http.Response, error) {
		calls.Add(1)
		if source != u.externalUIURL {
			t.Errorf("configured source was replaced")
		}
		return uiTestResponse(archive), nil
	}
	status := u.Status(context.Background(), true)
	if !status.Supported || !status.UpdateAvailable || status.CheckError != "" || len(status.CurrentDigest) != 64 || len(status.LatestDigest) != 64 {
		t.Fatalf("unexpected generic source status: %+v", status)
	}
	if status.CurrentVersion != "" || status.LatestVersion != "" || status.LatestCommit != "" {
		t.Fatalf("metadata should be optional: %+v", status)
	}
	if got := uiTestIndex(t, u); got != "old" {
		t.Fatalf("check changed installed UI: %q", got)
	}
	// The mutable configured URL now serves different bytes. POST must install
	// the already validated snapshot, not a second, unchecked download.
	archive = uiTestZIP(t, map[string]string{"index.html": "different"})
	if err := u.DownloadUI(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || uiTestIndex(t, u) != "new" {
		t.Fatal("POST did not reuse the checked snapshot")
	}
	installed := u.Status(context.Background(), false)
	if installed.UpdateAvailable || installed.CurrentDigest != status.LatestDigest || installed.LatestDigest != status.LatestDigest {
		t.Fatalf("installed digest status: %+v", installed)
	}
	data, err := json.Marshal(installed)
	if err != nil || bytes.Contains(data, []byte("private")) || bytes.Contains(data, []byte("panels.example")) {
		t.Fatalf("status exposed source: %s, %v", data, err)
	}
}

func TestUIUpdaterCheckSingleFlightCachingAndConditionalRequests(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "old"})
	archive := uiTestZIP(t, map[string]string{"index.html": "next"})
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	u.request = func(ctx context.Context, _ string, headers map[string][]string) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			response := uiTestResponse(archive)
			response.Header.Set("ETag", `"version-two"`)
			response.Header.Set("Last-Modified", "Tue, 01 Jan 2030 00:00:00 GMT")
			return response, nil
		}
		if http.Header(headers).Get("If-None-Match") != `"version-two"` || http.Header(headers).Get("If-Modified-Since") == "" {
			t.Error("conditional request lost HTTP validators")
		}
		response := uiTestResponse(nil)
		response.StatusCode = http.StatusNotModified
		return response, nil
	}
	const clients = 16
	results := make(chan UIUpdateStatus, clients)
	for i := 0; i < clients; i++ {
		go func() { results <- u.Status(context.Background(), true) }()
	}
	<-started
	close(release)
	var digest string
	for i := 0; i < clients; i++ {
		status := <-results
		if status.CheckError != "" || !status.UpdateAvailable {
			t.Fatalf("joined check: %+v", status)
		}
		digest = status.LatestDigest
	}
	if calls.Load() != 1 {
		t.Fatalf("checks were not single flight: %d", calls.Load())
	}
	u.Status(context.Background(), true)
	if calls.Load() != 1 {
		t.Fatal("fresh check was not cached")
	}
	uiTestExpireCheck(u)
	if status := u.Status(context.Background(), true); status.CheckError != "" || status.LatestDigest != digest {
		t.Fatalf("304 check did not retain digest: %+v", status)
	}
	if calls.Load() != 2 {
		t.Fatal("expired check did not revalidate")
	}
}

func TestUIUpdaterCanceledWaiterAndBusyOperations(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "old"})
	archive := uiTestZIP(t, map[string]string{"index.html": "next"})
	started, release := make(chan struct{}), make(chan struct{})
	u.request = func(ctx context.Context, _ string, _ map[string][]string) (*http.Response, error) {
		close(started)
		select {
		case <-release:
			return uiTestResponse(archive), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan UIUpdateStatus, 1)
	go func() { first <- u.Status(ctx, true) }()
	<-started
	cancel()
	if status := <-first; status.CheckError == "" {
		t.Fatal("canceled HTTP waiter did not exit")
	}
	if err := u.DownloadUI(); !errors.Is(err, ErrUIUpdateBusy) {
		t.Fatalf("POST during check should fail fast: %v", err)
	}
	close(release)
	if status := u.Status(context.Background(), true); status.CheckError != "" {
		t.Fatalf("canceling one waiter canceled the shared check: %+v", status)
	}
	uiTestExpireCheck(u)
	u.state().operation.Lock()
	defer u.state().operation.Unlock()
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := u.check(ctx); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUIUpdateBusy) {
			t.Fatalf("check did not report busy: %v", err)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("check queued behind an installation")
	}
}

func TestUIUpdaterFailureCachingAndSourceRedaction(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "old"})
	var calls atomic.Int32
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		calls.Add(1)
		return nil, &url.Error{Op: "Get", URL: u.externalUIURL, Err: errors.New("private-token")}
	}
	for i := 0; i < 3; i++ {
		status := u.Status(context.Background(), true)
		if status.CheckError != "unable to download the configured dashboard" || status.UpdateAvailable {
			t.Fatalf("unsafe error status: %+v", status)
		}
	}
	if calls.Load() != 1 || uiTestIndex(t, u) != "old" {
		t.Fatal("failed checks were not cached or changed the UI")
	}
	u.externalUIURL = "ftp://invalid.example/archive"
	if status := u.Status(context.Background(), true); status.Supported || status.Reason != "invalid-source" || calls.Load() != 1 {
		t.Fatalf("invalid source was fetched: %+v", status)
	}
}

func TestUIUpdaterRejectsUnsafeArchivesWithoutChangingInstalledUI(t *testing.T) {
	symlinkZIP := func() []byte {
		var output bytes.Buffer
		w := zip.NewWriter(&output)
		header := &zip.FileHeader{Name: "index.html", Method: zip.Store}
		header.SetMode(os.ModeSymlink | 0o777)
		f, err := w.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(f, "../../config.yaml")
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}()
	oversized := uiTestZIP(t, map[string]string{"index.html": "new"})
	central := bytes.Index(oversized, []byte{'P', 'K', 1, 2})
	binary.LittleEndian.PutUint32(oversized[central+24:], maxUIFileSize+1)
	cases := map[string][]byte{
		"non-archive":     []byte("not an archive"),
		"missing-index":   uiTestZIP(t, map[string]string{"main.js": "js"}),
		"traversal":       uiTestZIP(t, map[string]string{"../escape": "bad", "index.html": "new"}),
		"backslash":       uiTestZIP(t, map[string]string{`..\escape`: "bad", "index.html": "new"}),
		"case-collision":  uiTestZIP(t, map[string]string{"assets/a.js": "a", "Assets/b.js": "b", "index.html": "new"}),
		"symlink":         symlinkZIP,
		"oversized":       oversized,
		"deep-path":       uiTestZIP(t, map[string]string{strings.Repeat("a/", maxUIPathDepth) + "file": "", "index.html": "new"}),
		"reserved-marker": uiTestZIP(t, map[string]string{uiRetainedFile: "{}", "index.html": "new"}),
	}
	for name, archive := range cases {
		t.Run(name, func(t *testing.T) {
			u := uiTestUpdater(t, map[string]string{"index.html": "old", "assets/old.js": "old asset"})
			u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
				return uiTestResponse(archive), nil
			}
			if err := u.DownloadUI(); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if uiTestIndex(t, u) != "old" {
				t.Fatal("invalid download replaced working UI")
			}
			stages, err := filepath.Glob(filepath.Join(filepath.Dir(u.externalUIPath), ".mihomo-ui-stage-*"))
			if err != nil || len(stages) != 0 {
				t.Fatalf("failed archive left staging: %v, %v", stages, err)
			}
		})
	}
}

func TestUIUpdaterImplicitDirectoryBudget(t *testing.T) {
	x := uiExtraction{ctx: context.Background(), root: t.TempDir(), files: make(map[string]bool), names: make(map[string]string), explicit: make(map[string]bool)}
	// Put the extractor one node below its ceiling; the next two-parent path
	// must fail before MkdirAll creates any of its implicit directories.
	for i := 0; i < maxUIFiles-1; i++ {
		name := fmt.Sprintf("used-%d", i)
		x.files[name], x.names[name] = true, name
	}
	if err := x.entry("new/parent/index.html", false, 0, strings.NewReader("")); err == nil {
		t.Fatal("implicit parents did not count against the inode budget")
	}
	if _, err := os.Lstat(filepath.Join(x.root, "new")); !os.IsNotExist(err) {
		t.Fatalf("over-budget implicit parent was created: %v", err)
	}
}

func TestUIUpdaterStandardTarGzip(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "old"})
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: "./index.html", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3}); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(tarWriter, "new")
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(buffer.Bytes()), nil
	}
	if err := u.DownloadUI(); err != nil {
		t.Fatal(err)
	}
	if uiTestIndex(t, u) != "new" {
		t.Fatal("standard tar.gz layout was not installed")
	}
}

func TestUIUpdaterFailedInstallRollsBack(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "old", "assets/old.js": "keep"})
	archive := uiTestZIP(t, map[string]string{"index.html": "new"})
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	calls := 0
	u.rename = func(from, to string) error {
		calls++
		if calls == 2 {
			return errors.New("simulated second rename failure")
		}
		return os.Rename(from, to)
	}
	before, _, err := scanUITree(context.Background(), u.externalUIPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.DownloadUI(); err == nil {
		t.Fatal("expected injected installation error")
	}
	after, _, err := scanUITree(context.Background(), u.externalUIPath, true)
	if err != nil || before != after || calls != 3 {
		t.Fatalf("rollback failed: before=%s after=%s calls=%d err=%v", before, after, calls, err)
	}
}

func TestUIUpdaterFailedBackupCleanupIsBounded(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "old"})
	archive := uiTestZIP(t, map[string]string{"index.html": "new"})
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	u.removeAll = func(string) error { return fs.ErrPermission }
	if err := u.DownloadUI(); err != nil {
		t.Fatal(err)
	}
	archive = uiTestZIP(t, map[string]string{"index.html": "next"})
	if err := u.DownloadUI(); err == nil || !strings.Contains(err.Error(), "cleanup is pending") {
		t.Fatalf("failed cleanup should block another backup: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(u.externalUIPath), ".mihomo-ui-backup-*"))
	if err != nil || len(backups) != 1 || uiTestIndex(t, u) != "new" {
		t.Fatalf("repeated update accumulated backup directories: %v %v", backups, err)
	}
	u.removeAll = nil
	if err = u.DownloadUI(); err != nil || uiTestIndex(t, u) != "next" {
		t.Fatalf("cleanup retry did not recover: %v", err)
	}
}

func TestUIUpdaterRetainsOneAssetGeneration(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "one", "assets/one.js": "one", "config-not-an-asset.txt": "do not retain"})
	archive := uiTestZIP(t, map[string]string{"index.html": "two", "assets/two.js": "two"})
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	if err := u.DownloadUI(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(u.externalUIPath, "assets/one.js")); err != nil {
		t.Fatal("open tab's lazy chunk was removed", err)
	}
	if _, err := os.Stat(filepath.Join(u.externalUIPath, "config-not-an-asset.txt")); !os.IsNotExist(err) {
		t.Fatal("non-asset was retained")
	}
	uiTestExpireCheck(u)
	if status := u.Status(context.Background(), true); status.UpdateAvailable || status.CheckError != "" {
		t.Fatalf("retained chunks caused a perpetual update: %+v", status)
	}
	archive = uiTestZIP(t, map[string]string{"index.html": "three", "assets/three.js": "three"})
	uiTestExpireCheck(u)
	if err := u.DownloadUI(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(u.externalUIPath, "assets/two.js")); err != nil {
		t.Fatal("previous generation was not retained", err)
	}
	if _, err := os.Stat(filepath.Join(u.externalUIPath, "assets/one.js")); !os.IsNotExist(err) {
		t.Fatal("old chunks accumulated for more than one generation")
	}
}

func TestUIUpdaterProtectsCoreConfigurationPaths(t *testing.T) {
	oldHome, oldConfig := C.Path.HomeDir(), C.Path.Config()
	t.Cleanup(func() { C.SetHomeDir(oldHome); C.SetConfig(oldConfig) })
	root := t.TempDir()
	uiTestWrite(t, root, map[string]string{"home/config.yaml": "private", "index.html": "parent", "home/index.html": "home", "other/index.html": "other", "other/custom.yaml": "private"})
	C.SetHomeDir(filepath.Join(root, "home"))
	C.SetConfig(filepath.Join(root, "other", "custom.yaml"))
	for _, target := range []string{root, filepath.Join(root, "home"), filepath.Join(root, "other")} {
		u := NewUiUpdater(target, "https://source.example/ui.zip", "")
		if _, err := u.target(); err == nil {
			t.Fatalf("configuration-containing target accepted: %s", target)
		}
	}
	if err := os.Symlink(filepath.Join(root, "home"), filepath.Join(root, "home-link")); err == nil {
		C.SetHomeDir(filepath.Join(root, "home-link"))
		if _, err = NewUiUpdater(filepath.Join(root, "home"), "https://source.example/ui.zip", "").target(); err == nil {
			t.Fatal("symlink home bypassed protection")
		}
	}
	if _, err := NewUiUpdater(filepath.Join(root, "home", "ui"), "https://source.example/ui.zip", "").target(); err != nil {
		t.Fatalf("normal UI child of core directory was rejected: %v", err)
	}
}

func TestUIUpdaterReloadCancelsOldSourceAndPublishesAtomically(t *testing.T) {
	previous := currentUIUpdater.Load()
	t.Cleanup(func() {
		uiActivation.Lock()
		if current := currentUIUpdater.Load(); current != nil && current.activeCancel != nil {
			current.activeCancel()
		}
		currentUIUpdater.Store(previous)
		uiActivation.Unlock()
	})
	u := uiTestUpdater(t, map[string]string{"index.html": "old"})
	started := make(chan struct{})
	u.request = func(ctx context.Context, _ string, _ map[string][]string) (*http.Response, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	SetUIUpdater(u)
	finished := make(chan error, 1)
	go func() { finished <- u.DownloadUI() }()
	<-started
	v := NewUiUpdater(u.externalUIPath, "https://different.example/another.zip", "")
	v.coordinator = u.coordinator
	archive := uiTestZIP(t, map[string]string{"index.html": "from new source"})
	v.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	SetUIUpdater(v)
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("old source update succeeded after reload")
		}
	case <-time.After(time.Second):
		t.Fatal("configuration change did not cancel in-flight download")
	}
	if uiTestIndex(t, u) != "old" || CurrentUIUpdater() != v {
		t.Fatal("old source replaced UI or new configuration was not published")
	}
	if err := u.DownloadUI(); !errors.Is(err, ErrUIConfigurationChanged) {
		t.Fatalf("inactive updater remained usable: %v", err)
	}
	if err := v.DownloadUI(); err != nil || uiTestIndex(t, v) != "from new source" {
		t.Fatalf("new source failed: %v", err)
	}
	// Exercise the actual accessor concurrently with same-configuration reloads.
	var readers sync.WaitGroup
	readers.Add(1)
	stop := make(chan struct{})
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = CurrentUIUpdater().key()
			}
		}
	}()
	for i := 0; i < 50; i++ {
		SetUIUpdater(NewUiUpdater(v.externalUIPath, v.externalUIURL, ""))
	}
	close(stop)
	readers.Wait()
}

func TestUIUpdaterSourceAndTargetSeparateCachedSnapshots(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "old"})
	firstArchive := uiTestZIP(t, map[string]string{"index.html": "source one"})
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(firstArchive), nil
	}
	if s := u.Status(context.Background(), true); !s.UpdateAvailable || s.CheckError != "" {
		t.Fatalf("first source check failed: %+v", s)
	}
	firstStage := u.state().latest.work
	v := NewUiUpdater(filepath.Join(t.TempDir(), "other-ui"), "https://second.example/ui.zip", "")
	v.coordinator = u.coordinator
	secondArchive := uiTestZIP(t, map[string]string{"index.html": "source two"})
	v.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(secondArchive), nil
	}
	if s := v.Status(context.Background(), true); !s.UpdateAvailable || s.CheckError != "" {
		t.Fatalf("second source reused incompatible state: %+v", s)
	}
	if _, err := os.Stat(firstStage); !os.IsNotExist(err) {
		t.Fatalf("old source stage was not cleaned: %v", err)
	}
	if err := v.DownloadUI(); err != nil || uiTestIndex(t, v) != "source two" || uiTestIndex(t, u) != "old" {
		t.Fatalf("source/target cache identity collision: %v", err)
	}
}

func TestUIUpdaterRejectsRetargetedUIPath(t *testing.T) {
	for _, duringDownload := range []bool{false, true} {
		t.Run(fmt.Sprintf("during-download-%v", duringDownload), func(t *testing.T) {
			u := uiTestUpdater(t, map[string]string{"index.html": "first existing UI"})
			second := filepath.Join(filepath.Dir(u.externalUIPath), "second")
			uiTestWrite(t, second, map[string]string{"index.html": "second existing UI"})
			first := u.externalUIPath
			link := filepath.Join(filepath.Dir(first), "current-ui")
			if err := os.Symlink(first, link); err != nil {
				t.Skip("filesystem does not support symlinks", err)
			}
			u.externalUIPath = link
			swap := func() {
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(second, link); err != nil {
					t.Fatal(err)
				}
			}
			archive := uiTestZIP(t, map[string]string{"index.html": "replacement"})
			u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
				if duringDownload {
					swap()
				}
				return uiTestResponse(archive), nil
			}
			if !duringDownload {
				if status := u.Status(context.Background(), true); status.CheckError != "" {
					t.Fatal(status.CheckError)
				}
				swap()
			}
			if err := u.DownloadUI(); !errors.Is(err, ErrUIConfigurationChanged) {
				t.Fatalf("retargeted path should invalidate snapshot: %v", err)
			}
			status := u.Status(context.Background(), false)
			if status.CurrentDigest != "" || status.LatestDigest != "" || status.Updating || status.UpdateAvailable {
				t.Fatalf("failed install was recorded as successful: %+v", status)
			}
			u.state().mu.Lock()
			checked := u.state().checked
			u.state().mu.Unlock()
			if !checked.IsZero() {
				t.Fatal("failed install retained a successful check cache")
			}
			for root, want := range map[string]string{first: "first existing UI", second: "second existing UI"} {
				data, err := os.ReadFile(filepath.Join(root, "index.html"))
				if err != nil || string(data) != want {
					t.Fatalf("retargeted update changed %s: %q %v", root, data, err)
				}
			}
		})
	}
}

func TestUIUpdaterCancellationWhileWaitingToCommit(t *testing.T) {
	u := uiTestUpdater(t, map[string]string{"index.html": "old"})
	archive := uiTestZIP(t, map[string]string{"index.html": "new"})
	u.request = func(context.Context, string, map[string][]string) (*http.Response, error) {
		return uiTestResponse(archive), nil
	}
	ready := make(chan struct{})
	u.beforeCommit = func() { close(ready) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	uiActivation.Lock()
	locked := true
	defer func() {
		if locked {
			uiActivation.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- u.DownloadUIContext(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("update exited before the commit barrier: %v", err)
	case <-time.After(time.Second):
		t.Fatal("update did not reach the commit barrier")
	}
	cancel()
	uiActivation.Unlock()
	locked = false
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter committed its update: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled update did not exit")
	}
	if uiTestIndex(t, u) != "old" {
		t.Fatal("canceling before the transaction changed the installed UI")
	}
}
