package main

import (
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
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
