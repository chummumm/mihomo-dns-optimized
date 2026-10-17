package dns

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/mihomo/tunnel"
	D "github.com/miekg/dns"
)

func TestSpeedCheckRCodeErrorCannotBeatNormalReply(t *testing.T) {
	cases := []struct {
		name          string
		errorCode     int
		normalCode    int
		noData        bool
		budgetExpired bool
		rejectError   bool
	}{
		{name: "positive", errorCode: D.RcodeFormatError, normalCode: D.RcodeSuccess},
		{name: "nodata", errorCode: D.RcodeFormatError, normalCode: D.RcodeSuccess, noData: true},
		{name: "nxdomain", errorCode: D.RcodeNotImplemented, normalCode: D.RcodeNameError},
		{name: "positive-after-budget", errorCode: D.RcodeFormatError, normalCode: D.RcodeSuccess, budgetExpired: true},
		{name: "nxdomain-after-budget", errorCode: D.RcodeNotImplemented, normalCode: D.RcodeNameError, budgetExpired: true},
		{name: "filtered-error-after-budget", errorCode: D.RcodeFormatError, normalCode: D.RcodeSuccess, budgetExpired: true, rejectError: true},
	}
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse} {
		for _, test := range cases {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				query := new(D.Msg).SetQuestion("rcode-race.example.", D.TypeA)
				bad := new(D.Msg).SetRcode(query, test.errorCode)
				good := new(D.Msg).SetRcode(query, test.normalCode)
				if test.normalCode == D.RcodeSuccess && !test.noData {
					good = speedCheckTestReply(t, query, "rcode-race.example. 60 IN A 192.0.2.1")
				}
				checker := speedCheckTestChecker(t, time.Second, 1)
				checker.responseMode = mode
				checker.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
					return time.Millisecond, nil
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if test.budgetExpired {
					ctx = context.WithValue(ctx, dnsSpeedCheckDeadlineKey{}, time.Now().Add(-time.Second))
				}
				badReceived := make(chan struct{})
				ctx = withSpeedCheckResponseFilter(ctx, func(message *D.Msg) bool {
					if message.Rcode == test.errorCode {
						close(badReceived)
						return !test.rejectError
					}
					return true
				})
				offers := make(chan int, 2)
				ctx = withDNSEarlyResponse(ctx, func(message *D.Msg) bool {
					offers <- message.Rcode
					return false
				})
				// The healthy upstream cannot answer until selection has observed
				// the error. No scheduler timing decides which response arrives first.
				clients := []dnsClient{
					speedCheckTestClient{message: bad},
					schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
						select {
						case <-badReceived:
							return good.Copy(), nil
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}},
				}
				got, _, err := checker.Exchange(ctx, clients, query)
				if err != nil || !speedCheckTestEqual(got, good) {
					t.Fatalf("error RCODE defeated a normal reply: answer=%v error=%v", got, err)
				}
				close(offers)
				for code := range offers {
					if code != test.normalCode {
						t.Fatalf("published error RCODE %s before the normal reply", D.RcodeToString[code])
					}
				}
			})
		}
	}
}

func TestSpeedCheckRCodeFallbackKeepsResponseLifetimeAndSource(t *testing.T) {
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse} {
		t.Run(mode, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("rcode-fallback.example.", D.TypeA)
			answer := new(D.Msg).SetRcode(query, D.RcodeFormatError)
			answer.Ns = []D.RR{speedCheckTestRR(t, "rcode-fallback.example. 60 IN SOA ns.example. hostmaster.example. 1 3600 600 86400 60")}
			receivedAt := time.Now().Add(-1500 * time.Millisecond)
			source := dnsstats.UpstreamSource{Address: "udp://192.0.2.53:53"}
			checker := speedCheckTestChecker(t, time.Second, 1)
			checker.responseMode = mode
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ctx = withDNSAnswerSources(withDNSAnswerLifetimes(ctx))
			firstReceived := make(chan struct{})
			ctx = withSpeedCheckResponseFilter(ctx, func(message *D.Msg) bool {
				if message.Rcode == D.RcodeFormatError {
					close(firstReceived)
				}
				return true
			})
			clients := []dnsClient{
				schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
					message := answer.Copy()
					recordDNSAnswerReceived(ctx, message, receivedAt)
					setDNSAnswerSources(ctx, message, []dnsstats.UpstreamSource{source})
					return message, nil
				}},
				schedulingTestClient{exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
					select {
					case <-firstReceived:
						return new(D.Msg).SetRcode(q, D.RcodeNotImplemented), nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}},
			}
			got, _, err := checker.Exchange(ctx, clients, query)
			if err != nil || !speedCheckTestEqual(got, answer) {
				t.Fatalf("lost the original fallback response: answer=%v error=%v", got, err)
			}
			if actual := dnsAnswerReceivedAt(ctx, got); !actual.Equal(receivedAt) {
				t.Fatalf("fallback receipt clock restarted: got=%v want=%v", actual, receivedAt)
			}
			sources := dnsAnswerSourcesFor(ctx, got)
			if len(sources) != 1 || sources[0].Address != source.Address {
				t.Fatalf("fallback provenance changed: %+v", sources)
			}
			aged := ageDNSAnswerAfterPolicy(ctx, got, got.Copy())
			if expires := dnsAnswerExpires(ctx, aged); !expires.Equal(receivedAt.Add(time.Minute)) {
				t.Fatalf("fallback TTL restarted at final selection: %v", expires)
			}
		})
	}
}

func TestSpeedCheckRCodeFallbackAtDeadlineButNotCancellation(t *testing.T) {
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse} {
		for _, action := range []string{"deadline", "cancel"} {
			t.Run(mode+"/"+action, func(t *testing.T) {
				query := new(D.Msg).SetQuestion("rcode-deadline.example.", D.TypeA)
				answer := new(D.Msg).SetRcode(query, D.RcodeFormatError)
				checker := speedCheckTestChecker(t, 10*time.Millisecond, 1)
				checker.responseMode = mode
				ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
				defer cancel()
				silentStarted, silentStopped := make(chan struct{}), make(chan struct{})
				firstReceived := make(chan struct{})
				ctx = withSpeedCheckResponseFilter(ctx, func(message *D.Msg) bool {
					close(firstReceived)
					return true
				})
				if action == "cancel" {
					go func() {
						select {
						case <-firstReceived:
							cancel()
						case <-ctx.Done():
						}
					}()
				}
				var calls atomic.Int32
				clients := []dnsClient{
					schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
						calls.Add(1)
						select {
						case <-silentStarted:
							return answer.Copy(), nil
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}},
					schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
						calls.Add(1)
						close(silentStarted)
						defer close(silentStopped)
						<-ctx.Done()
						return nil, ctx.Err()
					}},
				}
				got, _, err := checker.Exchange(ctx, clients, query)
				if action == "cancel" {
					if !errors.Is(err, context.Canceled) || got != nil {
						t.Fatalf("error RCODE hid cancellation: answer=%v error=%v", got, err)
					}
				} else if err != nil || !speedCheckTestEqual(got, answer) || ctx.Err() != context.DeadlineExceeded {
					t.Fatalf("fallback did not wait for the normal DNS deadline: answer=%v error=%v context=%v", got, err, ctx.Err())
				}
				responseModeReceive(t, silentStopped)
				if calls.Load() != 2 {
					t.Fatalf("fallback restarted or skipped an upstream: %d calls", calls.Load())
				}
			})
		}
	}
}

func TestSpeedCheckRCodeFallbackHonorsMainFilter(t *testing.T) {
	routingTestEnable(t)
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse} {
		t.Run(mode, func(t *testing.T) {
			config := selectionTestConfig()
			config.Main = append(config.Main, NameServer{Addr: "192.0.2.54:53"})
			config.SpeedCheck.ResponseMode = mode
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			first := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				return new(D.Msg).SetRcode(q, D.RcodeFormatError), nil
			}}
			second := &selectionTestClient{dnsClient: r.main[1], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				return new(D.Msg).SetRcode(q, D.RcodeNotImplemented), nil
			}}
			fallback := &selectionTestClient{dnsClient: r.fallback[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				return selectionTestAnswer(q, "", "198.51.100.1"), nil
			}}
			r.main, r.fallback = []dnsClient{first, second}, []dnsClient{fallback}
			query := routingTestQuery("rcode-main-fallback.example", 19)
			ctx := routingTestContext(newSelectionTestOutbound(), routingTestOrigin())
			answer := routingTestExchange(t, r, ctx, query)
			ips := msgToIP(answer)
			if len(ips) != 1 || ips[0].String() != "198.51.100.1" || fallback.count(0) != 1 {
				t.Fatalf("error RCODE bypassed main/fallback policy: answer=%v fallback=%d", answer, fallback.count(0))
			}
		})
	}
}

func TestSpeedCheckRCodeFallbackDoesNotHideNativeClose(t *testing.T) {
	routingTestEnable(t)
	config := selectionTestConfig()
	config.Fallback = nil
	rs := NewResolver(config)
	defer rs.Close()
	query := routingTestQuery("rcode-closed.example", 20)
	ctx := responseModePreparedContext(t, rs.Resolver, routingTestContext(newSelectionTestOutbound(), routingTestOrigin()), query)
	ctx = withSpeedCheckResponseFilter(ctx, func(*D.Msg) bool {
		// Closing while consuming the final upstream result skips the loop's
		// next iteration. Final fallback selection must check the close itself.
		queryRoute(ctx).closed.Store(true)
		return true
	})
	answer := new(D.Msg).SetRcode(query, D.RcodeFormatError)
	got, _, err := rs.speedChecker.Exchange(ctx, []dnsClient{speedCheckTestClient{message: answer}}, query)
	if !errors.Is(err, tunnel.ErrDNSNativeQueryClosed) || got != nil || ctx.Err() != nil {
		t.Fatalf("error RCODE hid a native close: answer=%v error=%v context=%v", got, err, ctx.Err())
	}
}

func TestSpeedCheckRCodeLegacyFailureAndExplicitClient(t *testing.T) {
	for _, code := range []int{D.RcodeServerFailure, D.RcodeRefused} {
		t.Run(D.RcodeToString[code], func(t *testing.T) {
			query := new(D.Msg).SetQuestion("rcode-legacy.example.", D.TypeA)
			checker := speedCheckTestChecker(t, time.Second, 1)
			answer := new(D.Msg).SetRcode(query, code)
			got, _, err := checker.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: answer}}, query)
			if err == nil || got != nil {
				t.Fatalf("legacy failure became a pass-through reply: answer=%v error=%v", got, err)
			}
		})
	}
	for _, name := range []string{"format_error", "not_implemented", "server_failure", "refused"} {
		t.Run("explicit/"+name, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("rcode-explicit.example.", D.TypeA)
			checker := speedCheckTestChecker(t, time.Second, 1)
			explicit := newRCodeClient(name)
			var calls atomic.Int32
			network := schedulingTestClient{exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
				calls.Add(1)
				return nil, errors.New("explicit RCODE unexpectedly contacted an upstream")
			}}
			got, cache, err := checker.Exchange(context.Background(), []dnsClient{network, explicit}, query)
			if err != nil || got == nil || got.Rcode != explicit.rcode || cache || calls.Load() != 0 {
				t.Fatalf("explicit RCODE semantics changed: answer=%v cache=%v calls=%d error=%v", got, cache, calls.Load(), err)
			}
		})
	}
}
