package dns

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

func TestSpeedCheckStagesOverlapWhenCapacityAllows(t *testing.T) {
	query := new(D.Msg).SetQuestion("overlap.example.", D.TypeA)
	answer := speedCheckTestReply(t, query, "overlap.example. 60 IN A 192.0.2.1")
	checker := speedCheckTestChecker(t, 500*time.Millisecond, 2)
	secondStarted := make(chan struct{})
	var firstActive, overlap, thirdStarted atomic.Bool
	checker.probe = func(ctx context.Context, _ netip.Addr, mode speedCheckMode) (time.Duration, error) {
		switch mode.port {
		case 443:
			firstActive.Store(true)
			defer firstActive.Store(false)
			select {
			case <-secondStarted:
				return 20 * time.Millisecond, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		case 80:
			overlap.Store(firstActive.Load())
			close(secondStarted)
			return time.Millisecond, nil
		default:
			thirdStarted.Store(true)
			return time.Second, nil
		}
	}
	measurements := &dualStackMeasurements{rtts: make(map[netip.Addr]time.Duration)}
	ctx := context.WithValue(context.Background(), dnsProbeMeasurementsKey{}, measurements)
	got, _, err := checker.Exchange(ctx, []dnsClient{speedCheckTestClient{message: answer}}, query)
	if err != nil || !speedCheckTestEqual(got, answer) || !overlap.Load() || thirdStarted.Load() {
		t.Fatalf("stages did not overlap/stop after success: overlap=%v third=%v answer=%v error=%v", overlap.Load(), thirdStarted.Load(), got, err)
	}
	if rtt, valid := measurements.fastest(query, got); !valid || rtt != time.Millisecond {
		t.Fatalf("later slower mode overwrote the IP's best RTT: rtt=%v measured=%v", rtt, valid)
	}
}

func TestDNSProbeReplacementWaitsForCanceledSocketToExit(t *testing.T) {
	pool := newDNSProbePool(1, time.Second)
	defer pool.Close()
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var firstActive, overlapped atomic.Bool
	first, err := pool.submit(context.Background(), "old-mode", func(ctx context.Context) (time.Duration, error) {
		firstActive.Store(true)
		defer firstActive.Store(false)
		close(started)
		<-ctx.Done()
		close(canceled)
		// Simulate a socket taking time to complete cancellation cleanup.
		<-release
		return 0, ctx.Err()
	}, func(time.Duration, error) { t.Error("released old waiter received a result") })
	if err != nil {
		t.Fatal(err)
	}
	<-started
	first.Release()
	<-canceled
	results := make(chan error, 1)
	second, err := pool.submit(context.Background(), "next-mode", func(context.Context) (time.Duration, error) {
		overlapped.Store(firstActive.Load())
		return time.Millisecond, nil
	}, func(_ time.Duration, err error) { results <- err })
	if err != nil {
		close(release)
		t.Fatalf("canceled admission was not available to the next mode: %v", err)
	}
	defer second.Release()
	select {
	case <-results:
		close(release)
		t.Fatal("replacement started before the canceled worker released its socket")
	default:
	}
	close(release)
	select {
	case err := <-results:
		if err != nil || overlapped.Load() {
			t.Fatalf("probe replacement exceeded actual concurrency: overlap=%v error=%v", overlapped.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement did not start after old work exited")
	}
}

func TestSpeedCheckStageDeadlineDoesNotStartFallbackEarly(t *testing.T) {
	query := new(D.Msg).SetQuestion("short-stage.example.", D.TypeA)
	answer := speedCheckTestReply(t, query, "short-stage.example. 60 IN A 192.0.2.1")
	checker := speedCheckTestChecker(t, 40*time.Millisecond, 2)
	var calls atomic.Int32
	checker.probe = func(ctx context.Context, _ netip.Addr, _ speedCheckMode) (time.Duration, error) {
		calls.Add(1)
		<-ctx.Done()
		return 0, ctx.Err()
	}
	got, _, err := checker.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: answer}}, query)
	if err != nil || !speedCheckTestEqual(got, answer) || calls.Load() != 1 {
		t.Fatalf("short total budget was divided into early fallback stages: calls=%d answer=%v error=%v", calls.Load(), got, err)
	}
}

func TestSpeedCheckLaterCandidatesUseCurrentSuccessfulStage(t *testing.T) {
	query := new(D.Msg).SetQuestion("later-stage.example.", D.TypeA)
	first := speedCheckTestReply(t, query, "later-stage.example. 60 IN A 192.0.2.1")
	later := speedCheckTestReply(t, query, "later-stage.example. 60 IN A 192.0.2.2")
	checker := speedCheckTestChecker(t, time.Second, 2)
	events := make(chan string, 8)
	checker.probe = func(_ context.Context, ip netip.Addr, mode speedCheckMode) (time.Duration, error) {
		events <- ip.String() + "/" + mode.String()
		if mode.port == 80 {
			return time.Millisecond, nil
		}
		return 0, errors.New("unsupported first probe")
	}
	got, _, err := checker.Exchange(context.Background(), []dnsClient{
		speedCheckTestClient{message: first}, speedCheckTestClient{message: later, delay: 250 * time.Millisecond},
	}, query)
	if err != nil || len(got.Answer) != 2 {
		t.Fatalf("later upstream was lost: answer=%v error=%v", got, err)
	}
	want := []string{"192.0.2.1/tcp:443", "192.0.2.1/tcp:80", "192.0.2.2/tcp:80"}
	for _, expected := range want {
		select {
		case got := <-events:
			if got != expected {
				t.Fatalf("stage event=%q, want %q", got, expected)
			}
		default:
			t.Fatalf("missing stage event %q", expected)
		}
	}
	select {
	case extra := <-events:
		t.Fatalf("successful stage restarted earlier/later probes: %q", extra)
	default:
	}
}

func TestDNSProbeExpiredBeforeStartingIsNotUnreachable(t *testing.T) {
	pool := newDNSProbePool(1, time.Second)
	defer pool.Close()
	started, release := make(chan struct{}), make(chan struct{})
	first, err := pool.submit(context.Background(), "retiring", func(ctx context.Context) (time.Duration, error) {
		close(started)
		<-ctx.Done()
		<-release
		return 0, ctx.Err()
	}, func(time.Duration, error) {})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var calls atomic.Int32
	results := make(chan error, 1)
	_, err = pool.submit(ctx, "expires-while-retiring", func(context.Context) (time.Duration, error) {
		calls.Add(1)
		return 0, context.DeadlineExceeded
	}, func(_ time.Duration, err error) { results <- err })
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	<-ctx.Done()
	close(release)
	select {
	case err := <-results:
		if !errors.Is(err, errDNSProbeNotStarted) || errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
			t.Fatalf("unstarted probe became a network timeout: calls=%d error=%v", calls.Load(), err)
		}
		measurements := &dualStackMeasurements{rtts: make(map[netip.Addr]time.Duration)}
		query := new(D.Msg).SetQuestion("unstarted.example.", D.TypeA)
		answer := speedCheckTestReply(t, query, "unstarted.example. 60 IN A 192.0.2.1")
		measurements.observe(netip.MustParseAddr("192.0.2.1"), 0, err)
		if measurements.unreachable(query, answer) {
			t.Fatal("unstarted probe can suppress a valid family")
		}
	case <-time.After(time.Second):
		t.Fatal("expired queued probe did not finish")
	}
}
