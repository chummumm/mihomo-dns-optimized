package executor

import "fmt"

// Restart keeps successful shutdown preparation and process replacement in one
// configuration transaction. The callback runs only after protection is ready.
func Restart(restart func()) error {
	mux.Lock()
	defer mux.Unlock()
	if err := shutdownForRestartLocked(); err != nil {
		return err
	}
	restart()
	return nil
}

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
	if err := shutdownForRestartLocked(); err != nil {
		return fmt.Errorf("core file installed but restart preparation failed; the current process is still running the previous core version: %w", err)
	}
	restart()
	return nil
}
