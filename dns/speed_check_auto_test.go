package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestAutomaticSpeedCheckDefaultAndExplicitPolicy(t *testing.T) {
	want := []speedCheckMode{{port: 443, tcpMode: tcpProbeAuto}, {port: 80, tcpMode: tcpProbeAuto}, {}}
	for _, modes := range [][]string{nil, {"auto"}, {" AUTO "}} {
		checker, err := newDirectSpeedChecker(SpeedCheckConfig{Mode: modes})
		if err != nil || checker == nil {
			t.Fatalf("automatic checker for %#v: %v", modes, err)
		}
		if !reflect.DeepEqual(checker.modes, want) || checker.timeout != defaultSpeedCheckTimeout || checker.concurrency != defaultSpeedCheckConcurrency {
			t.Fatalf("unexpected automatic policy: %+v", checker)
		}
		checker.pool.Close()
		checker.candidates.Close()
	}
	for _, modes := range [][]string{{}} {
		checker, err := newDirectSpeedChecker(SpeedCheckConfig{Mode: modes})
		if err != nil || checker != nil {
			t.Fatalf("explicit disabling was ignored: %#v, %v", modes, err)
		}
	}
	for _, modes := range [][]string{{"auto", "ping"}, {"tcp:443", "auto"}, {"auto", "auto"}, {"auto", "none"}, {"auto:443"}, {"tcp-auto"}, {"tcp-auto:443"}, {""}, {"ping", ""}} {
		if err := ValidateSpeedCheckConfig(SpeedCheckConfig{Mode: modes}); err == nil {
			t.Fatalf("accepted ambiguous or unsupported automatic configuration: %v", modes)
		}
	}
	if err := ValidateSpeedCheckConfig(SpeedCheckConfig{Mode: []string{"none"}}); err == nil || !strings.Contains(err.Error(), "use an empty value or [] to disable") {
		t.Fatalf("removed none setting did not explain the replacement: %v", err)
	}
}

func TestTCPAutoFallbackRequiresRawCapabilityEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		fallback bool
	}{
		{name: "raw-unavailable", err: errTCPSYNUnavailable, fallback: true},
		{name: "wrapped-raw-unavailable", err: fmt.Errorf("raw setup: %w: %w", errTCPSYNUnavailable, syscall.EPERM), fallback: true},
		{name: "unclassified-permission", err: syscall.EPERM},
		{name: "unclassified-access", err: syscall.EACCES},
		{name: "network-unreachable", err: syscall.ENETUNREACH},
		{name: "host-unreachable", err: syscall.EHOSTUNREACH},
		{name: "connection-refused", err: syscall.ECONNREFUSED},
		{name: "network-timeout", err: &net.OpError{Op: "read", Net: "ip4:tcp", Err: syscall.ETIMEDOUT}},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "canceled", err: context.Canceled},
		{name: "file-descriptors", err: syscall.EMFILE},
		{name: "system-file-descriptors", err: syscall.ENFILE},
		{name: "memory", err: syscall.ENOMEM},
		{name: "socket-buffers", err: syscall.ENOBUFS},
		{name: "busy", err: errDNSProbeBusy},
		{name: "not-started", err: errDNSProbeNotStarted},
		{name: "other", err: errors.New("unclassified failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var synCalls, tcpCalls int
			rtt, err := probeTCPMode(ctx, tcpProbeAuto, func(got context.Context) (time.Duration, error) {
				synCalls++
				if got != ctx {
					t.Error("SYN did not receive the caller's bounded context")
				}
				return 0, test.err
			}, func(got context.Context) (net.Conn, error) {
				tcpCalls++
				if got != ctx {
					t.Error("TCP fallback received a different deadline or route context")
				}
				return nil, syscall.ECONNREFUSED
			})
			if synCalls != 1 || (tcpCalls == 1) != test.fallback {
				t.Fatalf("unexpected attempts: SYN=%d TCP=%d want fallback=%v", synCalls, tcpCalls, test.fallback)
			}
			if test.fallback {
				if err != nil || rtt < 0 {
					t.Fatalf("capability fallback did not accept a TCP refusal RTT: %v, %v", rtt, err)
				}
			} else if !errors.Is(err, test.err) {
				t.Fatalf("original probe failure was replaced: got %v want %v", err, test.err)
			}
		})
	}
}

func TestTCPAutoUsesSYNResultAndDoesNotCacheCapabilityFailures(t *testing.T) {
	var synCalls, tcpCalls int
	syn := func(context.Context) (time.Duration, error) {
		synCalls++
		if synCalls == 1 {
			return 0, errTCPSYNUnavailable
		}
		return 7 * time.Millisecond, nil
	}
	connect := func(context.Context) (net.Conn, error) {
		tcpCalls++
		return nil, syscall.ECONNREFUSED
	}
	if _, err := probeTCPMode(context.Background(), tcpProbeAuto, syn, connect); err != nil {
		t.Fatal(err)
	}
	rtt, err := probeTCPMode(context.Background(), tcpProbeAuto, syn, connect)
	if err != nil || rtt != 7*time.Millisecond || synCalls != 2 || tcpCalls != 1 {
		t.Fatalf("later probes inherited an old capability failure or repeated a successful SYN: RTT=%v err=%v SYN=%d TCP=%d", rtt, err, synCalls, tcpCalls)
	}
}

func TestExplicitTCPModesDoNotSelectAnotherMethod(t *testing.T) {
	for _, mode := range []tcpProbeMode{tcpProbeConnect, tcpProbeSYN} {
		var synCalls, tcpCalls int
		_, err := probeTCPMode(context.Background(), mode, func(context.Context) (time.Duration, error) {
			synCalls++
			return 0, errTCPSYNUnavailable
		}, func(context.Context) (net.Conn, error) {
			tcpCalls++
			return nil, syscall.ECONNREFUSED
		})
		if mode == tcpProbeSYN {
			if !errors.Is(err, errTCPSYNUnavailable) || synCalls != 1 || tcpCalls != 0 {
				t.Fatalf("explicit SYN selected TCP: SYN=%d TCP=%d err=%v", synCalls, tcpCalls, err)
			}
		} else if err != nil || synCalls != 0 || tcpCalls != 1 {
			t.Fatalf("explicit TCP attempted SYN: SYN=%d TCP=%d err=%v", synCalls, tcpCalls, err)
		}
	}
}

func TestTCPAutoFallbackRetainsRemainingDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var synContext context.Context
	_, err := probeTCPMode(ctx, tcpProbeAuto, func(got context.Context) (time.Duration, error) {
		synContext = got
		timer := time.NewTimer(15 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return 0, errTCPSYNUnavailable
		case <-got.Done():
			return 0, got.Err()
		}
	}, func(got context.Context) (net.Conn, error) {
		fallbackDeadline, bounded := got.Deadline()
		if got != synContext || !bounded || !fallbackDeadline.Equal(deadline) {
			t.Errorf("fallback changed the original remaining budget: %v, want %v", fallbackDeadline, deadline)
		}
		<-got.Done()
		return nil, got.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() == nil {
		t.Fatalf("fallback did not expire with its shared caller: %v", err)
	}
}

func TestTCPAutoDoesNotStartAfterCancellationOrSetupExpiry(t *testing.T) {
	for _, duringSetup := range []bool{false, true} {
		for _, expired := range []bool{false, true} {
			name := fmt.Sprintf("during-setup=%v/expired=%v", duringSetup, expired)
			t.Run(name, func(t *testing.T) {
				var ctx context.Context
				var cancel context.CancelFunc
				if expired {
					deadline := time.Now().Add(-time.Second)
					if duringSetup {
						deadline = time.Now().Add(10 * time.Millisecond)
					}
					ctx, cancel = context.WithDeadline(context.Background(), deadline)
				} else {
					ctx, cancel = context.WithCancel(context.Background())
					if !duringSetup {
						cancel()
					}
				}
				defer cancel()
				var synCalls, tcpCalls int
				_, err := probeTCPMode(ctx, tcpProbeAuto, func(got context.Context) (time.Duration, error) {
					synCalls++
					if expired {
						<-got.Done()
					} else {
						cancel()
					}
					return 0, errTCPSYNUnavailable
				}, func(context.Context) (net.Conn, error) {
					tcpCalls++
					return nil, syscall.ECONNREFUSED
				})
				want := context.Canceled
				if expired {
					want = errDNSProbeNotStarted
				}
				if !errors.Is(err, want) || tcpCalls != 0 || (synCalls == 1) != duringSetup {
					t.Fatalf("canceled/expired setup started more work: SYN=%d TCP=%d error=%v want=%v", synCalls, tcpCalls, err, want)
				}
			})
		}
	}
}

func TestTCPAutoFallbackSharesOneWorkerWithAllWaiters(t *testing.T) {
	pool := newDNSProbePool(1, time.Second)
	defer pool.Close()
	started, exited := make(chan struct{}), make(chan struct{})
	var synCalls, tcpCalls atomic.Int32
	work := func(ctx context.Context) (time.Duration, error) {
		defer close(exited)
		return probeTCPMode(ctx, tcpProbeAuto, func(context.Context) (time.Duration, error) {
			synCalls.Add(1)
			return 0, errTCPSYNUnavailable
		}, func(ctx context.Context) (net.Conn, error) {
			tcpCalls.Add(1)
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}
	first, err := pool.submit(context.Background(), "direct-interface-mark|auto:443", work, func(time.Duration, error) {
		t.Error("a released waiter received the canceled measurement")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("automatic fallback did not start")
	}
	second, err := pool.submit(context.Background(), "direct-interface-mark|auto:443", work, func(time.Duration, error) {
		t.Error("a released second waiter received the canceled measurement")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if _, err := pool.submit(context.Background(), "other-route|auto:443", work, func(time.Duration, error) {}); !errors.Is(err, errDNSProbeBusy) {
		t.Fatalf("fallback released the sole worker before TCP exited: %v", err)
	}
	first.Release()
	select {
	case <-exited:
		t.Fatal("one waiter canceled another waiter's automatic fallback")
	default:
	}
	second.Release()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("last waiter cancellation did not release the fallback worker")
	}
	if synCalls.Load() != 1 || tcpCalls.Load() != 1 {
		t.Fatalf("sharing duplicated an auto measurement: SYN=%d TCP=%d", synCalls.Load(), tcpCalls.Load())
	}
}
