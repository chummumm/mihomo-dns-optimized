package main

import (
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub/executor"
)

func TestShutdownDoesNotReturnWhileOwnedCleanupFails(t *testing.T) {
	requests := make(chan os.Signal, 1)
	first := make(chan struct{})
	finished := make(chan struct{})
	var resolved atomic.Bool
	var attempts atomic.Int32
	go func() {
		shutdownUntilComplete(func() error {
			if attempts.Add(1) == 1 {
				close(first)
			}
			if !resolved.Load() {
				return errors.New("owned route removal failed")
			}
			return nil
		}, requests)
		close(finished)
	}()
	<-first
	select {
	case <-finished:
		t.Fatal("cleanup failure was mistaken for successful process exit")
	case <-time.After(20 * time.Millisecond):
	}
	resolved.Store(true)
	requests <- syscall.SIGTERM
	select {
	case <-finished:
		if attempts.Load() < 2 {
			t.Fatal("stop did not retry cleanup")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("successful cleanup did not release graceful shutdown")
	}
}

func TestShutdownReturnsImmediatelyWhenCleanupIsComplete(t *testing.T) {
	attempts := 0
	shutdownUntilComplete(func() error { attempts++; return nil }, nil)
	if attempts != 1 {
		t.Fatalf("completed cleanup retried: %d", attempts)
	}
}

func TestStopSignalRelayPublishesWhileMainCannotReceive(t *testing.T) {
	received := make(chan os.Signal, 2)
	published := make(chan struct{}, 2)
	requests, stop := relayStopSignals(received, func() { published <- struct{}{} })
	defer stop()
	// Leave the output queue full, as happens while startup or cleanup owns
	// the main goroutine. Both signals must still publish stop intent.
	received <- syscall.SIGTERM
	received <- syscall.SIGINT
	for i := 0; i < 2; i++ {
		select {
		case <-published:
		case <-time.After(2 * time.Second):
			t.Fatal("busy main goroutine prevented publishing a stop request")
		}
	}
	select {
	case sig := <-requests:
		if sig != syscall.SIGTERM {
			t.Fatalf("first stop notification was lost: %v", sig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("published stop did not wake the exit loop")
	}
}

func TestEarlyStopKeepsNormalExitAndRealConfigurationErrors(t *testing.T) {
	const childFlag = "MIHOMO_TEST_EARLY_STOP"
	if os.Getenv(childFlag) == "1" {
		C.SetHomeDir(t.TempDir())
		executor.RequestShutdown()
		configBytes = []byte("mode: direct\ncore-auto-update: false\nexternal-ui-auto-update: false\n")
		if err := runDaemon(nil); err != nil {
			t.Fatalf("normal stop became a startup failure: %v", err)
		}
		configBytes = []byte("mode: [invalid-yaml-type]\n")
		if err := runDaemon(nil); err == nil || errors.Is(err, executor.ErrShuttingDown) {
			t.Fatalf("stop request hid a genuine configuration error: %v", err)
		}
		return
	}
	// The normal-stop marker is intentionally irreversible in a process.
	// Run startup/exit behavior separately from the remaining main tests.
	command := exec.Command(os.Args[0], "-test.run=^TestEarlyStopKeepsNormalExitAndRealConfigurationErrors$")
	command.Env = append(os.Environ(), childFlag+"=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("early stop exit contract failed: %v\n%s", err, output)
	}
}
