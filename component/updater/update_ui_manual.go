package updater

import "context"

type UIUpdateResult struct {
	CurrentVersion string
	LatestVersion  string
	Updated        bool
}

// CheckAndUpdateContext bypasses the status cache, validates the real remote
// archive, and installs it only when its version is newer. The sole bootstrap
// exception is an empty managed ./ui directory, where a valid version can be
// installed without inventing a current version.
func (u *UIUpdater) CheckAndUpdateContext(ctx context.Context) (UIUpdateResult, error) {
	var result UIUpdateResult
	if err := u.readiness.wait(ctx); err != nil {
		return result, err
	}
	// An activation/bootstrap may already own the installer. A unified task
	// waits within its bounded phase, then performs its own fresh remote check
	// instead of skipping the UI and restarting the core beneath that download.
	err := u.state().operation.runWhenAvailable(ctx, ErrUIUpdateBusy, func() error {
		return u.installUICheckContext(ctx, true, false, &result)
	})
	return result, err
}
