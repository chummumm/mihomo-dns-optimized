package dns

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/resolver"
	D "github.com/miekg/dns"
)

type schedulingTestClient struct {
	exchange func(context.Context, *D.Msg) (*D.Msg, error)
}

func (c schedulingTestClient) ExchangeContext(ctx context.Context, query *D.Msg) (*D.Msg, error) {
	return c.exchange(ctx, query)
}
func (schedulingTestClient) Address() string  { return "test://scheduler" }
func (schedulingTestClient) ResetConnection() {}

func TestSpeedCheckProbeLimitDoesNotSerializeDNSUpstreams(t *testing.T) {
	query := new(D.Msg).SetQuestion("healthy-second.example.", D.TypeA)
	answer := speedCheckTestReply(t, query, "healthy-second.example. 60 IN A 192.0.2.2")
	checker := speedCheckTestChecker(t, 50*time.Millisecond, 1)
	checker.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		return time.Millisecond, nil
	}
	var healthyCalls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, _, err := checker.Exchange(ctx, []dnsClient{
		schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		schedulingTestClient{exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
			healthyCalls.Add(1)
			return answer.Copy(), nil
		}},
	}, query)
	if err != nil || !speedCheckTestEqual(got, answer) || healthyCalls.Load() != 1 {
		t.Fatalf("probe concurrency starved the healthy DNS upstream: answer=%v calls=%d error=%v", got, healthyCalls.Load(), err)
	}
}

func TestSpeedCheckBusyKeepsUnmeasuredAnswerWithoutWaiting(t *testing.T) {
	query := new(D.Msg).SetQuestion("busy.example.", D.TypeA)
	answer := speedCheckTestReply(t, query, "busy.example. 60 IN A 192.0.2.1", "busy.example. 60 IN A 192.0.2.2")
	checker := speedCheckTestChecker(t, time.Second, 1)
	started := make(chan struct{})
	_, err := checker.pool.submit(context.Background(), "occupied", func(ctx context.Context) (time.Duration, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	}, func(time.Duration, error) {})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	var calls atomic.Int32
	checker.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		calls.Add(1)
		return time.Millisecond, nil
	}
	measurements := &dualStackMeasurements{rtts: make(map[netip.Addr]time.Duration)}
	ctx := context.WithValue(context.Background(), dnsProbeMeasurementsKey{}, measurements)
	start := time.Now()
	got, _, err := checker.Exchange(ctx, []dnsClient{speedCheckTestClient{message: answer}}, query)
	if elapsed := time.Since(start); elapsed >= 250*time.Millisecond {
		t.Fatalf("busy probe delayed an already available DNS answer: %v", elapsed)
	}
	if err != nil || !speedCheckTestEqual(got, answer) || calls.Load() != 0 {
		t.Fatalf("busy probe changed the valid RRset: answer=%v probes=%d error=%v", got, calls.Load(), err)
	}
	if _, measured := measurements.fastest(query, got); measured {
		t.Fatal("an unstarted probe became a successful dual-stack measurement")
	}
}

func TestDNSProbeFullPoolStillSharesExistingWork(t *testing.T) {
	pool := newDNSProbePool(1, time.Second)
	defer pool.Close()
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	work := func(ctx context.Context) (time.Duration, error) {
		calls.Add(1)
		close(started)
		select {
		case <-finish:
			return time.Millisecond, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	firstResults, secondResults := make(chan error, 1), make(chan error, 1)
	first, err := pool.submit(context.Background(), "shared", work, func(_ time.Duration, err error) { firstResults <- err })
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := pool.submit(context.Background(), "shared", work, func(_ time.Duration, err error) { secondResults <- err })
	if err != nil {
		t.Fatalf("full capacity rejected an existing probe's second waiter: %v", err)
	}
	defer second.Release()
	if _, err := pool.submit(context.Background(), "unrelated", work, func(time.Duration, error) {}); !errors.Is(err, errDNSProbeBusy) {
		t.Fatalf("full capacity queued an unrelated probe: %v", err)
	}
	first.Release()
	close(finish)
	select {
	case err := <-secondResults:
		if err != nil {
			t.Fatalf("one waiter's release canceled its sibling: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shared probe did not finish")
	}
	if calls.Load() != 1 {
		t.Fatalf("shared work ran %d times", calls.Load())
	}
	select {
	case <-firstResults:
		t.Fatal("released waiter received a probe result")
	default:
	}
}

func TestSpeedCheckCancelAndCloseDoNotStrandProbeWaiters(t *testing.T) {
	for _, action := range []string{"cancel-query", "close-pool"} {
		t.Run(action, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("closing.example.", D.TypeA)
			answer := speedCheckTestReply(t, query, "closing.example. 60 IN A 192.0.2.1", "closing.example. 60 IN A 192.0.2.2")
			checker := speedCheckTestChecker(t, time.Second, 1)
			started := make(chan struct{})
			checker.probe = func(ctx context.Context, _ netip.Addr, _ speedCheckMode) (time.Duration, error) {
				close(started)
				<-ctx.Done()
				return 0, ctx.Err()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			results := make(chan *D.Msg, 1)
			go func() {
				got, _, _ := checker.Exchange(ctx, []dnsClient{speedCheckTestClient{message: answer}}, query)
				results <- got
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("admitted probe did not start")
			}
			closed := make(chan struct{})
			go func() {
				if action == "cancel-query" {
					cancel()
				} else {
					checker.pool.Close()
				}
				close(closed)
			}()
			select {
			case got := <-results:
				if !speedCheckTestEqual(got, answer) {
					t.Fatalf("shutdown lost the already valid, unmeasured response: %v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("probe shutdown stranded the DNS waiter")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("probe result delivery blocked pool shutdown")
			}
		})
	}
}

func TestSpeedCheckOptimizationExpiryKeepsOriginalSlowDNSAttempt(t *testing.T) {
	query := new(D.Msg).SetQuestion("slow-dns.example.", D.TypeA)
	answer := speedCheckTestReply(t, query, "slow-dns.example. 60 IN A 192.0.2.3")
	checker := speedCheckTestChecker(t, 20*time.Millisecond, 1)
	var queries, probes atomic.Int32
	checker.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		probes.Add(1)
		return time.Millisecond, nil
	}
	client := schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
		queries.Add(1)
		timer := time.NewTimer(70 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return answer.Copy(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, _, err := checker.Exchange(ctx, []dnsClient{client}, query)
	if err != nil || !speedCheckTestEqual(got, answer) || queries.Load() != 1 || probes.Load() != 0 {
		t.Fatalf("selection expiry canceled/restarted DNS or started late probes: answer=%v DNS=%d probes=%d error=%v", got, queries.Load(), probes.Load(), err)
	}
}

func TestSpeedCheckKeepsNormalDNSDeadlineAndCallerCancellation(t *testing.T) {
	query := new(D.Msg).SetQuestion("deadline.example.", D.TypeA)
	checker := speedCheckTestChecker(t, 10*time.Millisecond, 1)
	deadlineSeen := make(chan time.Time, 1)
	probeError := errors.New("test DNS failure")
	client := schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
		deadline, _ := ctx.Deadline()
		deadlineSeen <- deadline
		return nil, probeError
	}}
	start := time.Now()
	_, _, err := checker.Exchange(context.Background(), []dnsClient{client}, query)
	if !errors.Is(err, probeError) {
		t.Fatalf("lost upstream error: %v", err)
	}
	if budget := (<-deadlineSeen).Sub(start); budget < resolver.DefaultDNSTimeout-250*time.Millisecond || budget > resolver.DefaultDNSTimeout+250*time.Millisecond {
		t.Fatalf("normal DNS deadline was replaced by the optimization budget: %v", budget)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	silent := schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	start = time.Now()
	_, _, err = checker.Exchange(ctx, []dnsClient{silent}, query)
	if elapsed := time.Since(start); !errors.Is(err, context.DeadlineExceeded) || elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("caller deadline was not preserved: error=%v elapsed=%v", err, elapsed)
	}
}

func TestSpeedCheckHeaderOnlyErrorsRetainDNSMeaning(t *testing.T) {
	for _, rcode := range []int{D.RcodeFormatError, D.RcodeNameError, D.RcodeNotImplemented} {
		t.Run(D.RcodeToString[rcode], func(t *testing.T) {
			query := new(D.Msg).SetQuestion("header-only.example.", D.TypeA)
			answer := &D.Msg{MsgHdr: D.MsgHdr{Id: query.Id, Response: true, Opcode: query.Opcode, Rcode: rcode}}
			checker := speedCheckTestChecker(t, time.Second, 1)
			got, _, err := checker.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: answer}}, query)
			if err != nil || !speedCheckTestEqual(got, answer) {
				t.Fatalf("valid header-only %s became a local failure: answer=%v error=%v", D.RcodeToString[rcode], got, err)
			}
		})
	}
}

func TestSpeedCheckRejectsUnrelatedAndEmptySuccessResponses(t *testing.T) {
	for _, name := range []string{"wrong-id", "wrong-opcode", "empty-success", "error-with-unrelated-record", "wrong-id-with-question"} {
		t.Run(name, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("validate.example.", D.TypeA)
			answer := &D.Msg{MsgHdr: D.MsgHdr{Id: query.Id, Response: true, Opcode: query.Opcode, Rcode: D.RcodeNameError}}
			switch name {
			case "wrong-id":
				answer.Id++
			case "wrong-opcode":
				answer.Opcode = D.OpcodeUpdate
			case "empty-success":
				answer.Rcode = D.RcodeSuccess
			case "error-with-unrelated-record":
				answer.Answer = []D.RR{speedCheckTestRR(t, "unrelated.example. 60 IN A 192.0.2.1")}
			case "wrong-id-with-question":
				answer = speedCheckTestReply(t, query, "validate.example. 60 IN A 192.0.2.1")
				answer.Id++
			}
			checker := speedCheckTestChecker(t, time.Second, 1)
			got, _, err := checker.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: answer}}, query)
			if err == nil || got != nil {
				t.Fatalf("unrelated/invalid response was accepted: answer=%v error=%v", got, err)
			}
		})
	}
}
