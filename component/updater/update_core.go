package updater

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/ca"
	mihomoHttp "github.com/metacubex/mihomo/component/http"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/http"
)

const (
	// Bound both the downloaded archive and its decompressed executable.
	MaxPackageFileSize = 64 * 1024 * 1024
	maxCoreFileSize    = 256 * 1024 * 1024
	maxReleaseMetadata = 1024 * 1024

	ReleaseChannel = "release"
	AlphaChannel   = "alpha"
)

// CoreUpdater installs a checksummed release from this fork. It never falls
// back to the official core. The empty value uses the production release URL.
type CoreUpdater struct {
	mu               updateOperationGate
	policyMu         sync.Mutex
	autoUpdate       bool
	updateInterval   int
	automatic        *coreAutomaticUpdate
	readiness        updateReadiness
	restartHandler   CoreAutomaticRestartHandler
	installedVersion string
	pendingRestart   bool

	// Tests can serve a complete release locally and isolate filesystem faults
	// without changing global HTTP routing. These are not config options.
	releaseURL     string
	newTicker      func(time.Duration) (<-chan time.Time, func())
	executable     func() (string, error)
	currentVersion func() string
	beforeCommit   func()
	linkBackup     func(string, string) error
}

var DefaultCoreUpdater = CoreUpdater{}
var ErrCoreUpdateBusy = errors.New("a core update is already running")
var ErrCoreConfigurationChanged = errors.New("core update configuration changed")

func (u *CoreUpdater) Update(currentExePath string, channel string, force bool) error {
	return u.UpdateContext(context.Background(), currentExePath, channel, force)
}

// UpdateContext preserves the manual compatibility API. Automatic updates use
// the same operation gate and installer, with an additional policy transaction.
func (u *CoreUpdater) UpdateContext(ctx context.Context, currentExePath string, channel string, force bool) error {
	return u.updateContext(ctx, currentExePath, channel, force, nil)
}

func (u *CoreUpdater) updateContext(ctx context.Context, currentExePath string, channel string, force bool, automatic *coreAutomaticUpdate) (err error) {
	return u.updateTransaction(ctx, currentExePath, channel, force, automatic, nil)
}

func (u *CoreUpdater) updateTransaction(ctx context.Context, currentExePath string, channel string, force bool, automatic *coreAutomaticUpdate, manual *coreManualUpdate) (err error) {
	if manual == nil && !u.mu.TryLock() {
		return ErrCoreUpdateBusy
	}
	operationHeld := true
	defer func() {
		if operationHeld {
			u.mu.Unlock()
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	if u.pendingRestart {
		if automatic != nil {
			return nil
		}
		return fmt.Errorf("%w: installed version %s is waiting for restart", ErrCoreUpdateBusy, u.installedVersion)
	}

	switch strings.ToLower(channel) {
	case "", "auto", ReleaseChannel:
	case AlphaChannel:
		return fmt.Errorf("mihomo-optimized has no alpha update channel; use release")
	default:
		return fmt.Errorf("unsupported mihomo-optimized update channel %q", channel)
	}
	baseURL := u.releaseURL
	if baseURL == "" {
		baseURL = coreReleaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/") + "/"
	latestVersion, err := u.getLatestVersionContext(ctx, baseURL+"latest/download/version.txt")
	if err != nil {
		return fmt.Errorf("get latest mihomo-optimized version: %w", err)
	}
	latest, err := parseReleaseVersion(latestVersion)
	if err != nil {
		return err
	}
	currentVersion := u.effectiveVersion()
	if manual != nil {
		manual.result.CurrentVersion, manual.result.LatestVersion = currentVersion, latestVersion
		if manual.observe != nil {
			manual.observe(manual.result)
		}
		if _, parseErr := parseReleaseVersion(currentVersion); parseErr != nil {
			return ErrCoreVersionUnavailable
		}
	}
	log.Infoln("current version %s, latest mihomo-optimized version %s", currentVersion, latestVersion)
	if (automatic != nil || manual != nil) && !u.isNewerVersion(latestVersion) {
		return nil
	}
	if !force {
		if latestVersion == currentVersion {
			// Some dashboards depend on this existing error text.
			return fmt.Errorf("update error: already using latest version %s", currentVersion)
		}
		if current, parseErr := parseReleaseVersion(currentVersion); parseErr == nil && latest.compare(current) < 0 {
			return fmt.Errorf("refusing mihomo-optimized downgrade from %s to %s without force", currentVersion, latestVersion)
		}
	}

	// A bare daemon can keep running after its on-disk executable is moved or
	// deleted. Report the release check first; an equal/older release needs no
	// filesystem transaction and must not fail because that path is unavailable.
	currentExePath, err = filepath.EvalSymlinks(currentExePath)
	if err != nil {
		return fmt.Errorf("locate current executable for replacement: %w", err)
	}
	identity, err := snapshotCoreExecutable(ctx, currentExePath)
	if err != nil {
		return fmt.Errorf("snapshot current executable: %w", err)
	}
	target, err := coreTarget(runtime.GOOS, runtime.GOARCH, coreBuildSettings())
	if err != nil {
		return err
	}

	// Only the initial version lookup uses Latest. All subsequent requests are
	// pinned to the validated immutable tag, even if Latest changes meanwhile.
	releaseURL := baseURL + "download/" + latestVersion + "/"
	sums, err := u.readReleaseFileContext(ctx, releaseURL+"SHA256SUMS", maxReleaseMetadata)
	if err != nil {
		return fmt.Errorf("get release checksums: %w", err)
	}
	asset, err := selectCoreReleaseAsset(sums, target, latestVersion, runtime.GOOS == "windows")
	if err != nil {
		return err
	}
	packageName, exeName := asset.name, asset.executable

	// Stage on the same filesystem so replacement can use a rename. No
	// existing executable or backup is touched before verification succeeds.
	workDir := filepath.Dir(currentExePath)
	updateDir, err := os.MkdirTemp(workDir, ".mihomo-update-")
	if err != nil {
		return fmt.Errorf("create update directory: %w", err)
	}
	defer os.RemoveAll(updateDir)
	packagePath := filepath.Join(updateDir, packageName)
	if err = u.downloadContext(ctx, packagePath, releaseURL+packageName, asset.checksum); err != nil {
		return fmt.Errorf("download mihomo-optimized core: %w", err)
	}
	updateExePath := filepath.Join(updateDir, exeName)
	mode := identity.info.Mode().Perm()
	if runtime.GOOS != "windows" {
		// Removing execute bits does not stop an already running Unix daemon.
		// The new file is owned by this process; retain the original access
		// policy and ensure that its owner can execute it after replacement.
		mode |= 0o100
	}
	if err = u.unpackContext(ctx, packagePath, updateExePath, exeName, mode); err != nil {
		return fmt.Errorf("unpack mihomo-optimized core: %w", err)
	}
	// Routers often update on a small writable overlay. The verified archive
	// is no longer needed once extraction completes, even when backing up the
	// old executable later requires a full copy on this filesystem.
	if err = os.Remove(packagePath); err != nil {
		return fmt.Errorf("remove extracted core archive: %w", err)
	}
	if runtime.GOOS == "darwin" {
		if err = exec.CommandContext(ctx, "/usr/bin/codesign", "--force", "--sign", "-", updateExePath).Run(); err != nil {
			return fmt.Errorf("sign staged executable: %w", err)
		}
	}
	if err = validateStagedCoreContext(ctx, updateExePath, latestVersion); err != nil {
		return fmt.Errorf("validate staged mihomo-optimized core: %w", err)
	}
	if u.beforeCommit != nil {
		u.beforeCommit()
	}
	commit := func() error {
		if automatic != nil || manual != nil {
			// A manual installation may have won the gate while this prepared
			// auto job waited for configuration reload to finish.
			if !u.mu.TryLock() {
				return ErrCoreUpdateBusy
			}
			defer u.mu.Unlock()
		}
		// This lock protects only the final filesystem transaction, never network
		// I/O or Shutdown. The restart bridge serializes this transaction with
		// ApplyConfig, so a disabled/replaced policy cannot install or restart.
		u.policyMu.Lock()
		defer u.policyMu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if automatic != nil {
			if u.automatic != automatic || !u.autoUpdate || automatic.ctx.Err() != nil || u.restartHandler == nil {
				return ErrCoreConfigurationChanged
			}
			if u.pendingRestart || !u.isNewerVersion(latestVersion) {
				return ErrCoreConfigurationChanged
			}
		}
		if manual != nil && (u.pendingRestart || !u.isNewerVersion(latestVersion)) {
			return ErrCoreConfigurationChanged
		}
		currentIdentity, err := snapshotCoreExecutable(ctx, currentExePath)
		if err != nil || !identity.equal(currentIdentity) {
			return fmt.Errorf("current executable changed while preparing update")
		}
		if err = u.installContext(ctx, currentExePath, updateExePath, &identity); err != nil {
			return fmt.Errorf("install mihomo-optimized core: %w", err)
		}
		u.installedVersion, u.pendingRestart = latestVersion, true
		if manual != nil {
			manual.result.Updated = true
		}
		// The real handler may exec/exit and never return through our defers.
		// Remove the staging directory before yielding to process shutdown.
		if cleanupErr := os.RemoveAll(updateDir); cleanupErr != nil {
			log.Warnln("updater: unable to remove completed staging directory: %s", cleanupErr)
		}
		log.Infoln("updater: installed mihomo-optimized %s", latestVersion)
		return nil
	}
	if automatic != nil {
		// The handler must commit and restart as one configuration transaction.
		// Neither waiting for that transaction nor Shutdown holds updater locks.
		u.mu.Unlock()
		operationHeld = false
		return automatic.restart(currentExePath, commit)
	}
	if manual != nil {
		// The unified task reserves this operation gate before updating other
		// resources. Release it only when the prepared core is ready to enter
		// the same short configuration/restart transaction as auto updates.
		u.mu.Unlock()
		operationHeld = false
		return manual.restart(currentExePath, commit)
	}
	return commit()
}

func (u *CoreUpdater) readReleaseFile(url string, limit int64) ([]byte, error) {
	return u.readReleaseFileContext(context.Background(), url, limit)
}

func (u *CoreUpdater) readReleaseFileContext(parent context.Context, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	resp, err := mihomoHttp.HttpRequest(ctx, url, http.MethodGet, nil, nil,
		mihomoHttp.WithCAOption(ca.Option{ZeroTrust: true}))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release request returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(coreContextReader{ctx, resp.Body}, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("release metadata exceeds %d bytes", limit)
	}
	return body, nil
}

func (u *CoreUpdater) getLatestVersion(url string) (string, error) {
	return u.getLatestVersionContext(context.Background(), url)
}

func (u *CoreUpdater) getLatestVersionContext(ctx context.Context, url string) (string, error) {
	body, err := u.readReleaseFileContext(ctx, url, 256)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(body))
	if _, err = parseReleaseVersion(version); err != nil {
		return "", err
	}
	return version, nil
}

func (u *CoreUpdater) download(path, url string, expected []byte) (err error) {
	return u.downloadContext(context.Background(), path, url, expected)
}

func (u *CoreUpdater) downloadContext(parent context.Context, path, url string, expected []byte) (err error) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	resp, err := mihomoHttp.HttpRequest(ctx, url, http.MethodGet, nil, nil,
		mihomoHttp.WithCAOption(ca.Option{ZeroTrust: true}))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("archive request returned HTTP %d", resp.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(coreContextReader{ctx, resp.Body}, MaxPackageFileSize+1))
	if err != nil {
		return err
	}
	if n > MaxPackageFileSize {
		return fmt.Errorf("archive exceeds %d bytes", MaxPackageFileSize)
	}
	if !bytes.Equal(hash.Sum(nil), expected) {
		return fmt.Errorf("SHA256 mismatch for %s", filepath.Base(path))
	}
	return file.Sync()
}

// unpack writes exactly one executable to a caller-selected path. Gzip header
// names are ignored; Windows archives must contain the precise release member.
func (u *CoreUpdater) unpack(packagePath, outputPath, exeName string, mode os.FileMode) error {
	return u.unpackContext(context.Background(), packagePath, outputPath, exeName, mode)
}

func (u *CoreUpdater) unpackContext(ctx context.Context, packagePath, outputPath, exeName string, mode os.FileMode) error {
	switch {
	case strings.HasSuffix(packagePath, ".gz"):
		file, err := os.Open(packagePath)
		if err != nil {
			return err
		}
		defer file.Close()
		reader, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer reader.Close()
		return writeStagedCoreContext(ctx, outputPath, reader, mode)
	case strings.HasSuffix(packagePath, ".zip"):
		archive, err := zip.OpenReader(packagePath)
		if err != nil {
			return err
		}
		defer archive.Close()
		if len(archive.File) != 1 {
			return fmt.Errorf("expected exactly one executable in the archive")
		}
		member := archive.File[0]
		if member.Name != exeName || !member.Mode().IsRegular() || member.UncompressedSize64 > maxCoreFileSize {
			return fmt.Errorf("unexpected executable in the archive: %q", member.Name)
		}
		reader, err := member.Open()
		if err != nil {
			return err
		}
		defer reader.Close()
		return writeStagedCoreContext(ctx, outputPath, reader, mode)
	default:
		return fmt.Errorf("unsupported archive format")
	}
}

func writeStagedCore(path string, reader io.Reader, mode os.FileMode) (err error) {
	return writeStagedCoreContext(context.Background(), path, reader, mode)
}

func writeStagedCoreContext(ctx context.Context, path string, reader io.Reader, mode os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	n, err := io.Copy(file, io.LimitReader(coreContextReader{ctx, reader}, maxCoreFileSize+1))
	if err != nil {
		return err
	}
	if n == 0 || n > maxCoreFileSize {
		return fmt.Errorf("invalid executable size: %d", n)
	}
	if err = file.Chmod(mode); err != nil {
		return err
	}
	return file.Sync()
}

func (u *CoreUpdater) install(currentPath, stagedPath string) error {
	return u.installContext(context.Background(), currentPath, stagedPath, nil)
}

func (u *CoreUpdater) installContext(ctx context.Context, currentPath, stagedPath string, expected *coreExecutableSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected == nil {
		identity, err := snapshotCoreExecutable(ctx, currentPath)
		if err != nil {
			return err
		}
		expected = &identity
	}
	verifyCurrent := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		actual, err := snapshotCoreExecutable(ctx, currentPath)
		if err != nil || !expected.equal(actual) {
			return errors.New("current executable changed while preparing update")
		}
		return nil
	}
	backupDir := filepath.Join(filepath.Dir(currentPath), "meta-backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return err
	}
	backupPath := filepath.Join(backupDir, filepath.Base(currentPath))
	if runtime.GOOS == "windows" {
		if err := verifyCurrent(); err != nil {
			return err
		}
		// A running Windows executable must first be moved out of the way.
		if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove previous backup: %w", err)
		}
		if err := os.Rename(currentPath, backupPath); err != nil {
			return fmt.Errorf("back up running executable: %w", err)
		}
		if err := os.Rename(stagedPath, currentPath); err != nil {
			if restoreErr := os.Rename(backupPath, currentPath); restoreErr != nil {
				return fmt.Errorf("replace: %v; restore: %v; backup remains at %s", err, restoreErr, backupPath)
			}
			return err
		}
		return nil
	}
	// Unix permits replacing a running executable by rename. A hard link keeps
	// its old inode recoverable without allocating another core-sized file;
	// filesystems that do not support links use a checked copy instead.
	backupTemp, err := u.prepareCoreBackupContext(ctx, currentPath, backupDir, *expected)
	if err != nil {
		return err
	}
	defer os.Remove(backupTemp)
	// Preparing the backup can take time. Recheck the path and content after it,
	// immediately before either persistent name is replaced.
	if err = verifyCurrent(); err != nil {
		return err
	}
	if err = os.Rename(backupTemp, backupPath); err != nil {
		return err
	}
	return os.Rename(stagedPath, currentPath)
}

type coreContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r coreContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type coreExecutableSnapshot struct {
	info   os.FileInfo
	digest [sha256.Size]byte
}

func snapshotCoreExecutable(ctx context.Context, path string) (coreExecutableSnapshot, error) {
	var snapshot coreExecutableSnapshot
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return snapshot, err
	}
	if !pathInfo.Mode().IsRegular() {
		return snapshot, errors.New("executable path is no longer a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return snapshot, err
	}
	defer file.Close()
	snapshot.info, err = file.Stat()
	if err != nil {
		return snapshot, err
	}
	if !snapshot.info.Mode().IsRegular() || !os.SameFile(pathInfo, snapshot.info) || snapshot.info.Size() <= 0 || snapshot.info.Size() > maxCoreFileSize {
		return snapshot, fmt.Errorf("invalid executable size or type")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(coreContextReader{ctx, file}, maxCoreFileSize+1)); err != nil {
		return snapshot, err
	}
	copy(snapshot.digest[:], hash.Sum(nil))
	return snapshot, nil
}

func (s coreExecutableSnapshot) equal(other coreExecutableSnapshot) bool {
	return s.info != nil && other.info != nil && os.SameFile(s.info, other.info) &&
		s.info.Size() == other.info.Size() && s.info.Mode() == other.info.Mode() &&
		s.info.ModTime().Equal(other.info.ModTime()) && s.digest == other.digest
}

// mu must be held: a successfully installed version is authoritative until the
// process restarts, because the running C.Version still names the old binary.
func (u *CoreUpdater) effectiveVersion() string {
	if u.installedVersion != "" {
		return u.installedVersion
	}
	if u.currentVersion != nil {
		return u.currentVersion()
	}
	return C.Version
}

func (u *CoreUpdater) isNewerVersion(version string) bool {
	latest, err := parseReleaseVersion(version)
	if err != nil {
		return false
	}
	current, err := parseReleaseVersion(u.effectiveVersion())
	return err == nil && latest.compare(current) > 0
}
