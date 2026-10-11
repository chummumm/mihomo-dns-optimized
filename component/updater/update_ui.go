package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mihomoHttp "github.com/metacubex/mihomo/component/http"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/http"
)

const (
	uiCheckTTL    = 5 * time.Minute
	uiCheckLimit  = 45 * time.Second
	uiUpdateLimit = 90 * time.Second
	maxUIMetadata = 64 * 1024
	maxUIArchive  = 64 * 1024 * 1024
)

var (
	ErrUIUpdateBusy           = errors.New("a UI check or update is already running")
	ErrUIConfigurationChanged = errors.New("dashboard update configuration changed")
)

type UIUpdateStatus struct {
	Supported       bool   `json:"supported"`
	Reason          string `json:"reason,omitempty"`
	CurrentVersion  string `json:"current_version"`
	CurrentCommit   string `json:"current_commit"`
	CurrentDigest   string `json:"current_digest,omitempty"`
	LatestVersion   string `json:"latest_version,omitempty"`
	LatestCommit    string `json:"latest_commit,omitempty"`
	LatestDigest    string `json:"latest_digest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	Updating        bool   `json:"updating"`
	CheckError      string `json:"check_error,omitempty"`
	AutoUpdate      bool   `json:"auto_update"`
	UpdateInterval  int    `json:"update_interval"`
	NextCheck       string `json:"next_check,omitempty"`
}

type uiBuildInfo struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	SourceCommit string `json:"source_commit"`
}

type uiSnapshot struct {
	key, work, root, target string
	digest                  string
	currentDigest           string
	info                    uiBuildInfo
	files                   map[string]uiFile
	etag, modified          string
}

// A reload replaces UIUpdater instances. The lock, one-entry check cache, and
// bounded staging lifetime belong to the process rather than to one instance.
type uiUpdateCoordinator struct {
	operation   sync.Mutex
	updating    atomic.Bool
	mu          sync.Mutex
	checked     time.Time
	key         string
	latest      uiSnapshot
	err         error
	checking    chan struct{}
	checkingKey string
	expiry      *time.Timer
}

var uiUpdates uiUpdateCoordinator
var currentUIUpdater atomic.Pointer[UIUpdater]
var uiActivation sync.Mutex
var emptyUIUpdater = &UIUpdater{}

// CurrentUIUpdater returns an immutable configuration snapshot. Reloads publish
// the replacement atomically; an update cannot commit after its source changed.
func CurrentUIUpdater() *UIUpdater {
	if u := currentUIUpdater.Load(); u != nil {
		return u
	}
	return emptyUIUpdater
}

func SetUIUpdater(u *UIUpdater) {
	uiActivation.Lock()
	defer uiActivation.Unlock()
	previous := currentUIUpdater.Load()
	if previous == u {
		return
	}
	if previous != nil && previous.key() == u.key() {
		u.activeCtx, u.activeCancel = previous.activeCtx, previous.activeCancel
	} else {
		if previous != nil && previous.activeCancel != nil {
			previous.activeCancel()
		}
		u.activeCtx, u.activeCancel = context.WithCancel(context.Background())
	}
	u.configureAutomaticUpdates(previous)
	currentUIUpdater.Store(u)
}

type UIUpdater struct {
	externalUIURL  string
	externalUIPath string
	autoDownloadUI bool
	autoUpdateUI   bool
	updateInterval int
	activeCtx      context.Context
	activeCancel   context.CancelFunc
	automatic      *uiAutomaticUpdate

	// Test-only injection; neither requests nor filesystem functions are API or
	// YAML options. Production always uses the configured external-ui-url.
	coordinator  *uiUpdateCoordinator
	request      func(context.Context, string, map[string][]string) (*http.Response, error)
	rename       func(string, string) error
	removeAll    func(string) error
	beforeCommit func()
	newTicker    func(time.Duration) (<-chan time.Time, func())
}

func NewUiUpdater(externalUI, externalUIURL, externalUIName string) *UIUpdater {
	return NewUiUpdaterWithAutoUpdate(externalUI, externalUIURL, externalUIName, false, defaultUIUpdateInterval)
}

// NewUiUpdaterWithAutoUpdate creates an immutable source and scheduling policy.
// SetUIUpdater activates it; constructing or parsing a configuration never starts
// a timer or a network request. updateInterval is expressed in hours.
func NewUiUpdaterWithAutoUpdate(externalUI, externalUIURL, externalUIName string, autoUpdate bool, updateInterval int) *UIUpdater {
	if updateInterval <= 0 || updateInterval > int((1<<63-1)/time.Hour) {
		updateInterval = defaultUIUpdateInterval
	}
	u := &UIUpdater{externalUIURL: externalUIURL, autoUpdateUI: autoUpdate, updateInterval: updateInterval}
	if externalUI != "" {
		u.autoDownloadUI = true
		u.externalUIPath = C.Path.Resolve(externalUI)
	} else {
		u.externalUIPath = filepath.Join(C.Path.HomeDir(), "ui")
	}
	if externalUIName != "" {
		u.autoDownloadUI = true
		u.externalUIPath = filepath.Join(u.externalUIPath, externalUIName)
	}
	return u
}

func (u *UIUpdater) state() *uiUpdateCoordinator {
	if u.coordinator != nil {
		return u.coordinator
	}
	return &uiUpdates
}

func (u *UIUpdater) key() string {
	key := sha256.Sum256([]byte(filepath.Clean(u.externalUIPath) + "\x00" + u.externalUIURL))
	return hex.EncodeToString(key[:])
}

func (u *UIUpdater) sourceValid() bool {
	parsed, err := url.Parse(u.externalUIURL)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "https" || parsed.Scheme == "http")
}

func (u *UIUpdater) active() bool {
	return u.activeCtx == nil || u.activeCtx.Err() == nil
}

func (u *UIUpdater) operationContext(parent context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, limit)
	if u.activeCtx != nil {
		go func() {
			select {
			case <-u.activeCtx.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

func (u *UIUpdater) Status(ctx context.Context, check bool) UIUpdateStatus {
	state := u.state()
	s := UIUpdateStatus{Updating: state.updating.Load(), AutoUpdate: u.AutoUpdate(), UpdateInterval: u.UpdateInterval(), NextCheck: u.nextAutomaticCheck()}
	if u.externalUIPath == "" {
		s.Reason = "ui-not-configured"
		return s
	}
	current := readUIBuildInfo(u.externalUIPath)
	s.CurrentVersion, s.CurrentCommit = current.Version, current.SourceCommit
	if !u.sourceValid() {
		s.Reason = "invalid-source"
		return s
	}
	s.Supported = true
	var latest uiSnapshot
	var err error
	if check && !s.Updating {
		latest, err = u.check(ctx)
	} else {
		state.mu.Lock()
		if state.key == u.key() {
			latest, err = state.latest, state.err
		}
		state.mu.Unlock()
	}
	if err != nil {
		s.CheckError = err.Error()
	}
	if latest.digest != "" {
		s.CurrentDigest, s.LatestDigest = latest.currentDigest, latest.digest
		s.LatestVersion, s.LatestCommit = latest.info.Version, latest.info.SourceCommit
		comparison, valid := compareUIVersions(latest.info.Version, current.Version)
		if err == nil && !valid {
			s.Reason = "version-unavailable"
		}
		s.UpdateAvailable = err == nil && valid && comparison > 0
	}
	s.Updating = state.updating.Load()
	return s
}

func (u *UIUpdater) check(ctx context.Context) (uiSnapshot, error) {
	return u.checkWithLifetime(ctx, context.Background())
}

// Browser checks outlive an individual HTTP waiter. A scheduled check instead
// belongs to its automatic policy, so disabling or rescheduling that policy also
// interrupts its download without canceling a manual installation.
func (u *UIUpdater) checkWithLifetime(ctx, lifetime context.Context) (uiSnapshot, error) {
	state, key := u.state(), u.key()
	for {
		if err := ctx.Err(); err != nil {
			return uiSnapshot{}, err
		}
		if !u.active() {
			return uiSnapshot{}, ErrUIConfigurationChanged
		}
		state.mu.Lock()
		if state.key == key && !state.checked.IsZero() && time.Since(state.checked) < uiCheckTTL {
			latest, err := state.latest, state.err
			state.mu.Unlock()
			return latest, err
		}
		done := state.checking
		if done != nil && state.checkingKey != key {
			state.mu.Unlock()
			return uiSnapshot{}, ErrUIUpdateBusy
		}
		if done == nil {
			// Checks may join an existing check, but never queue behind a POST.
			if !state.operation.TryLock() {
				state.mu.Unlock()
				return uiSnapshot{}, ErrUIUpdateBusy
			}
			done = make(chan struct{})
			state.checking, state.checkingKey = done, key
			// A single bounded check survives a tab disconnect. Other callers
			// join it; each still respects its own request cancellation.
			go func() {
				state.mu.Lock()
				previous := state.latest
				state.mu.Unlock()
				conditional := previous
				if previous.key != key {
					conditional = uiSnapshot{}
				}
				checkCtx, cancel := u.operationContext(lifetime, uiCheckLimit)
				latest, err := u.prepareSnapshot(checkCtx, conditional)
				cancel()
				if !u.active() {
					if latest.work != "" && latest.work != conditional.work {
						_ = os.RemoveAll(latest.work)
					}
					conditional, err = uiSnapshot{}, ErrUIConfigurationChanged
				}
				if err != nil {
					latest = conditional
				}
				if err == nil && latest.currentDigest == latest.digest && latest.work != "" {
					_ = os.RemoveAll(latest.work)
					latest.work, latest.root, latest.files = "", "", nil
				}
				if previous.work != "" && previous.work != latest.work {
					_ = os.RemoveAll(previous.work)
				}
				state.mu.Lock()
				state.latest, state.err, state.key, state.checked = latest, err, key, time.Now()
				if lifetime.Err() != nil && u.active() {
					// A deliberate policy cancellation is not a source failure.
					// Let a manual waiter immediately revalidate instead of caching
					// the cancellation as an error for the next five minutes.
					state.err, state.checked = nil, time.Time{}
				}
				state.checking = nil
				state.expireLocked()
				state.operation.Unlock()
				close(done)
				state.mu.Unlock()
			}()
		}
		state.mu.Unlock()
		select {
		case <-ctx.Done():
			return uiSnapshot{}, ctx.Err()
		case <-done:
			// A check for another configured source can have been in progress.
			// Loop to return only a snapshot for this URL and target directory.
		}
	}
}

// Called with mu held. Files expire even when the user closes the dashboard;
// only small HTTP validators and digests remain in memory until the next check.
func (s *uiUpdateCoordinator) expireLocked() {
	if s.expiry != nil {
		s.expiry.Stop()
	}
	if s.latest.work == "" {
		return
	}
	work := s.latest.work
	s.expiry = time.AfterFunc(uiCheckTTL, func() {
		s.operation.Lock()
		defer s.operation.Unlock()
		s.mu.Lock()
		remove := false
		if s.latest.work == work {
			s.latest.work, s.latest.root = "", ""
			s.latest.files = nil
			remove = true
		}
		s.mu.Unlock()
		if remove {
			_ = os.RemoveAll(work)
		}
	})
}

func (u *UIUpdater) get(ctx context.Context, headers map[string][]string) (*http.Response, error) {
	if u.request != nil {
		return u.request(ctx, u.externalUIURL, headers)
	}
	return mihomoHttp.HttpRequest(ctx, u.externalUIURL, http.MethodGet, headers, nil)
}

func uiDownloadError(err error) error {
	// net/url errors can contain credentials or signed query parameters. Never
	// surface them through the public status endpoint or the core log.
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("dashboard download timed out")
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("dashboard download canceled")
	}
	return errors.New("unable to download the configured dashboard")
}

func (u *UIUpdater) target() (string, error) {
	if u.externalUIPath == "" {
		return "", errors.New("external UI directory is not configured")
	}
	target, err := canonicalUIPath(u.externalUIPath)
	if err != nil {
		return "", err
	}
	if filepath.Dir(target) == target {
		return "", errors.New("refusing to replace the core configuration directory with a UI")
	}
	for _, protected := range []string{C.Path.HomeDir(), C.Path.Config()} {
		resolved, resolveErr := canonicalUIPath(protected)
		if resolveErr != nil {
			return "", resolveErr
		}
		if relative, relErr := filepath.Rel(target, resolved); relErr == nil && filepath.IsLocal(relative) {
			return "", errors.New("refusing to replace a directory containing core configuration with a UI")
		}
	}
	entries, err := os.ReadDir(target)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if len(entries) > 0 {
		if err = uiRegularFile(filepath.Join(target, "index.html")); err != nil {
			return "", errors.New("existing UI directory does not contain a valid index.html")
		}
	}
	return target, nil
}

// EvalSymlinks requires an existing path. Resolve the existing ancestor too,
// so a not-yet-created UI beneath a symlink gets the same protection.
func canonicalUIPath(name string) (string, error) {
	name, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(name)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(resolveErr) || filepath.Dir(name) == name {
			return "", resolveErr
		}
		suffix = append(suffix, filepath.Base(name))
		name = filepath.Dir(name)
	}
}

func (u *UIUpdater) prepareSnapshot(ctx context.Context, previous uiSnapshot) (snapshot uiSnapshot, err error) {
	if !u.sourceValid() {
		return snapshot, errors.New("external-ui-url must be a valid HTTP or HTTPS URL")
	}
	target, err := u.target()
	if err != nil {
		return snapshot, err
	}
	headers := map[string][]string{}
	if previous.digest != "" {
		if previous.etag != "" {
			headers["If-None-Match"] = []string{previous.etag}
		}
		if previous.modified != "" {
			headers["If-Modified-Since"] = []string{previous.modified}
		}
	}
	response, err := u.get(ctx, headers)
	if err != nil {
		return snapshot, uiDownloadError(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified && previous.digest != "" {
		snapshot = previous
		if snapshot.target != target {
			return uiSnapshot{}, ErrUIConfigurationChanged
		}
		snapshot.currentDigest, _, err = scanUITree(ctx, target, true)
		if os.IsNotExist(err) {
			err = nil
		}
		return snapshot, err
	}
	if response.StatusCode != http.StatusOK {
		return snapshot, fmt.Errorf("dashboard source returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxUIArchive {
		return snapshot, errors.New("UI archive is too large")
	}
	if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return snapshot, err
	}
	work, err := os.MkdirTemp(filepath.Dir(target), ".mihomo-ui-stage-")
	if err != nil {
		return snapshot, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(work)
		}
	}()
	archivePath := filepath.Join(work, "archive")
	file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return snapshot, err
	}
	n, copyErr := io.Copy(file, io.LimitReader(uiContextReader{ctx, response.Body}, maxUIArchive+1))
	closeErr := file.Close()
	if copyErr != nil {
		return snapshot, uiDownloadError(copyErr)
	}
	if n > maxUIArchive {
		return snapshot, errors.New("UI archive is too large")
	}
	if closeErr != nil {
		return snapshot, closeErr
	}
	root, err := extractUIArchive(ctx, archivePath, filepath.Join(work, "content"))
	if err != nil {
		return snapshot, fmt.Errorf("invalid dashboard archive: %w", err)
	}
	_ = os.Remove(archivePath)
	snapshot = uiSnapshot{key: u.key(), work: work, root: root, target: target, info: readUIBuildInfo(root)}
	snapshot.digest, snapshot.files, err = scanUITree(ctx, root, false)
	if err != nil {
		return snapshot, err
	}
	snapshot.currentDigest, _, err = scanUITree(ctx, target, true)
	if os.IsNotExist(err) {
		err = nil
	}
	if etag := response.Header.Get("ETag"); len(etag) <= 8192 {
		snapshot.etag = etag
	}
	if modified := response.Header.Get("Last-Modified"); len(modified) <= 256 {
		snapshot.modified = modified
	}
	return snapshot, err
}

func readUIFile(name string, limit int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid or oversized UI metadata file")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("UI metadata is too large")
	}
	return data, err
}

func readUIBuildInfo(root string) uiBuildInfo {
	data, err := readUIFile(filepath.Join(root, "build-info.json"), maxUIMetadata)
	var info uiBuildInfo
	if err != nil || json.Unmarshal(data, &info) != nil {
		return uiBuildInfo{}
	}
	// Versions control update availability. Missing or invalid versions still
	// permit an explicit manual installation, but never an automatic update.
	if len(info.Version) > 64 || strings.ContainsAny(info.Version, "\r\n") {
		info.Version = ""
	}
	if len(info.SourceCommit) != 40 {
		info.SourceCommit = ""
	} else if _, err = hex.DecodeString(info.SourceCommit); err != nil {
		info.SourceCommit = ""
	}
	return info
}

func (u *UIUpdater) AutoDownloadUI() {
	if !u.autoDownloadUI {
		return
	}
	entries, err := os.ReadDir(u.externalUIPath)
	if err != nil && !os.IsNotExist(err) {
		log.Errorln("Cannot inspect external UI directory")
		return
	}
	if len(entries) > 0 {
		return
	}
	if err = u.DownloadUI(); err != nil && !errors.Is(err, ErrUIUpdateBusy) {
		log.Errorln("Error downloading UI: %s", err)
	}
}

func (u *UIUpdater) DownloadUI() error { return u.DownloadUIContext(context.Background()) }

func (u *UIUpdater) updateAvailable(snapshot uiSnapshot) bool {
	comparison, valid := compareUIVersions(snapshot.info.Version, readUIBuildInfo(u.externalUIPath).Version)
	return valid && comparison > 0
}

func (u *UIUpdater) DownloadUIContext(ctx context.Context) error {
	return u.downloadUIContext(ctx, false)
}

func (u *UIUpdater) downloadUIContext(ctx context.Context, onlyNewer bool) (err error) {
	state, key := u.state(), u.key()
	if !state.operation.TryLock() {
		return ErrUIUpdateBusy
	}
	defer state.operation.Unlock()
	state.updating.Store(true)
	defer state.updating.Store(false)
	if !u.active() {
		return ErrUIConfigurationChanged
	}
	ctx, cancel := u.operationContext(ctx, uiUpdateLimit)
	defer cancel()
	target, err := u.target()
	if err != nil {
		return err
	}
	state.mu.Lock()
	previous := state.latest
	snapshot := previous
	usable := state.key == key && state.err == nil && !state.checked.IsZero() && time.Since(state.checked) < uiCheckTTL && snapshot.root != ""
	state.mu.Unlock()
	if !usable {
		// A cached 304 may have no staged bytes after expiry. Fetch a complete
		// archive and validate that snapshot before installing it.
		snapshot, err = u.prepareSnapshot(ctx, uiSnapshot{})
		if err != nil {
			return err
		}
	}
	installed := false
	defer func() {
		if snapshot.work != "" {
			_ = os.RemoveAll(snapshot.work)
		}
		if previous.work != "" && previous.work != snapshot.work {
			_ = os.RemoveAll(previous.work)
		}
		state.mu.Lock()
		if state.expiry != nil {
			state.expiry.Stop()
		}
		if err == nil {
			snapshot.work, snapshot.root = "", ""
			snapshot.files = nil
			if installed {
				snapshot.currentDigest = snapshot.digest
			}
			state.latest, state.err, state.key, state.checked = snapshot, nil, key, time.Now()
		} else {
			state.latest, state.checked = uiSnapshot{}, time.Time{}
		}
		state.mu.Unlock()
	}()
	if snapshot.target != target {
		return ErrUIConfigurationChanged
	}
	currentDigest, currentFiles, err := scanUITree(ctx, target, true)
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return err
	}
	snapshot.currentDigest = currentDigest
	// The current version can change after the automatic check (for example,
	// a manual installation wins the operation gate). Compare again while
	// holding that gate, including when a fresh archive had to be downloaded.
	if onlyNewer && !u.updateAvailable(snapshot) {
		return nil
	}
	if currentDigest == snapshot.digest {
		installed = true
		return nil
	}
	if err = retainUIAssets(ctx, target, snapshot.root, currentFiles, snapshot.files); err != nil {
		return fmt.Errorf("retain open dashboard assets: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	rename := u.rename
	if rename == nil {
		rename = os.Rename
	}
	removeAll := u.removeAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	// The brief filesystem commit and configuration activation are ordered;
	// a reload never waits for the network download or archive extraction.
	if u.beforeCommit != nil {
		u.beforeCommit()
	}
	uiActivation.Lock()
	if !u.active() {
		err = ErrUIConfigurationChanged
	} else {
		var currentTarget string
		currentTarget, err = u.target()
		if err == nil && currentTarget != target {
			err = ErrUIConfigurationChanged
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && (!onlyNewer || u.updateAvailable(snapshot)) {
			err = installUIDirectory(snapshot.root, target, rename, removeAll)
			installed = err == nil
		}
	}
	uiActivation.Unlock()
	return err
}
