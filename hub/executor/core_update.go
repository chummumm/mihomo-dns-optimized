package executor

// ApplyUpdateAndRestart serializes a prepared core installation and restart
// with ApplyConfig. Downloads happen before this function. commit must release
// updater locks before returning so shutdown can cancel their background work.
// A failed or obsolete commit never shuts down the current process.
func ApplyUpdateAndRestart(commit func() error, restart func()) error {
	mux.Lock()
	defer mux.Unlock()
	if err := commit(); err != nil {
		return err
	}
	shutdownLocked()
	restart()
	return nil
}
