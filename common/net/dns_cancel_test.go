package net

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestDNSCancelGuardPreservesCompletedResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fired := make(chan struct{})
	guard := NewDNSCancelGuard(ctx, func() { close(fired) })
	defer guard.Stop()
	cancel()
	<-fired
	for _, err := range []error{nil, io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, os.ErrDeadlineExceeded} {
		if got := guard.Error(err, DNSClosedIO(err)); got != err {
			t.Fatalf("late cancellation overwrote completed I/O: got %v, want %v", got, err)
		}
	}
	closed := &net.OpError{Op: "read", Net: "udp", Err: net.ErrClosed}
	if got := guard.Error(closed, DNSClosedIO(closed)); !errors.Is(got, context.Canceled) || !errors.Is(got, net.ErrClosed) {
		t.Fatalf("local cancellation lost its context or I/O cause: %v", got)
	}
}

func TestDNSCancelGuardDoesNotInferCancellationFromContextAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := NewDNSCancelGuard(ctx, func() { t.Error("stopped guard ran") })
	guard.Stop()
	cancel()
	if got := guard.Error(net.ErrClosed, true); got != net.ErrClosed {
		t.Fatalf("unrelated closed connection mislabeled as canceled: %v", got)
	}
}

func TestDNSCancelGuardPreservesDeadlineCause(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	fired := make(chan struct{})
	guard := NewDNSCancelGuard(ctx, func() { close(fired) })
	defer guard.Stop()
	<-fired
	if got := guard.Error(io.ErrClosedPipe, true); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("context deadline cause was not retained: %v", got)
	}
}
