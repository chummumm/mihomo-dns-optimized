package dns

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/tunnel"
	D "github.com/miekg/dns"
)

func TestDNSClientBatchNormalAnswerOutranksFastRCodeError(t *testing.T) {
	for _, failureCode := range []int{D.RcodeFormatError, D.RcodeNotImplemented, D.RcodeNotAuth, D.RcodeServerFailure, D.RcodeRefused} {
		for _, normal := range []struct {
			name   string
			rcode  int
			record string
		}{
			{name: "positive", rcode: D.RcodeSuccess, record: "rcode.example. 60 IN A 192.0.2.1"},
			{name: "nodata", rcode: D.RcodeSuccess},
			{name: "nxdomain", rcode: D.RcodeNameError},
		} {
			t.Run(D.RcodeToString[failureCode]+"/"+normal.name, func(t *testing.T) {
				query := new(D.Msg).SetQuestion("rcode.example.", D.TypeA)
				failure := new(D.Msg).SetReply(query)
				failure.Rcode = failureCode
				answer := new(D.Msg).SetReply(query)
				answer.Rcode = normal.rcode
				if normal.record != "" {
					record, err := D.NewRR(normal.record)
					if err != nil {
						t.Fatal(err)
					}
					answer.Answer = []D.RR{record}
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				failureReturned, releaseNormal := make(chan struct{}), make(chan struct{})
				normalStarted := make(chan context.Context, 1)
				replies := make(chan result, 1)
				var calls atomic.Int32
				go func() {
					message, _, err := batchExchange(ctx, []dnsClient{
						schedulingTestClient{exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
							calls.Add(1)
							close(failureReturned)
							return failure, nil
						}},
						schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
							calls.Add(1)
							normalStarted <- ctx
							select {
							case <-releaseNormal:
								return answer, nil
							case <-ctx.Done():
								return nil, ctx.Err()
							}
						}},
					}, query)
					replies <- result{Msg: message, Error: err}
				}()
				responseModeReceive(t, failureReturned)
				normalContext := responseModeReceive(t, normalStarted)
				select {
				case got := <-replies:
					t.Fatalf("fast error ended the batch before the normal answer: %+v", got)
				case <-normalContext.Done():
					t.Fatal("fast error canceled the pending normal upstream")
				case <-time.After(20 * time.Millisecond):
				}
				close(releaseNormal)
				got := responseModeReceive(t, replies)
				if got.Error != nil || got.Msg != answer || calls.Load() != 2 {
					t.Fatalf("normal answer lost or an upstream was retried: reply=%v error=%v calls=%d", got.Msg, got.Error, calls.Load())
				}
			})
		}
	}
}

func TestDNSClientBatchReturnsErrorReplyOnlyAfterAllUpstreamsFinish(t *testing.T) {
	query := new(D.Msg).SetQuestion("errors.example.", D.TypeA)
	first := new(D.Msg).SetReply(query)
	first.Rcode = D.RcodeFormatError
	second := new(D.Msg).SetReply(query)
	second.Rcode = D.RcodeNotImplemented
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	firstReturned, releaseSecond := make(chan struct{}), make(chan struct{})
	replies := make(chan result, 1)
	go func() {
		message, _, err := batchExchange(ctx, []dnsClient{
			schedulingTestClient{exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
				close(firstReturned)
				return first, nil
			}},
			schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
				select {
				case <-releaseSecond:
					return second, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}},
		}, query)
		replies <- result{Msg: message, Error: err}
	}()
	responseModeReceive(t, firstReturned)
	select {
	case got := <-replies:
		t.Fatalf("error reply skipped a pending upstream: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseSecond)
	got := responseModeReceive(t, replies)
	if got.Error != nil || (got.Msg != first && got.Msg != second) {
		t.Fatalf("no original error reply was retained: reply=%v error=%v", got.Msg, got.Error)
	}
}

func TestDNSClientBatchSingleUpstreamKeepsRCodeSemantics(t *testing.T) {
	for _, rcode := range []int{D.RcodeSuccess, D.RcodeNameError, D.RcodeFormatError, D.RcodeNotImplemented, D.RcodeNotAuth, D.RcodeServerFailure, D.RcodeRefused} {
		t.Run(D.RcodeToString[rcode], func(t *testing.T) {
			query := new(D.Msg).SetQuestion("single.example.", D.TypeA)
			// Header-only errors must retain their original message and RCODE.
			answer := &D.Msg{MsgHdr: D.MsgHdr{Id: query.Id, Response: true, Rcode: rcode}}
			got, cache, err := batchExchange(context.Background(), []dnsClient{
				schedulingTestClient{exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
					return answer, nil
				}},
			}, query)
			if rcode == D.RcodeServerFailure || rcode == D.RcodeRefused {
				if err == nil || got != nil {
					t.Fatalf("existing upstream failure handling changed: reply=%v error=%v", got, err)
				}
			} else if err != nil || got != answer {
				t.Fatalf("single upstream response changed: reply=%v error=%v", got, err)
			}
			if !cache {
				t.Fatal("ordinary upstream changed local-answer cache semantics")
			}
		})
	}
}

func TestDNSClientBatchErrorReplyAtDeadline(t *testing.T) {
	query := new(D.Msg).SetQuestion("deadline.example.", D.TypeA)
	answer := new(D.Msg).SetReply(query)
	answer.Rcode = D.RcodeFormatError
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var calls atomic.Int32
	got, _, err := batchExchange(ctx, []dnsClient{
		schedulingTestClient{exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
			calls.Add(1)
			return answer, nil
		}},
		schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
			calls.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	}, query)
	if got != answer || err != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) || calls.Load() != 2 {
		t.Fatalf("deadline discarded an available error reply or retried the batch: reply=%v error=%v context=%v calls=%d", got, err, ctx.Err(), calls.Load())
	}
}

func TestDNSClientBatchErrorReplyDoesNotHideCancellation(t *testing.T) {
	for _, nativeClose := range []bool{false, true} {
		name := "context"
		if nativeClose {
			name = "native-query"
		}
		t.Run(name, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("cancel.example.", D.TypeA)
			answer := new(D.Msg).SetReply(query)
			answer.Rcode = D.RcodeFormatError
			route := &dnsQueryRoute{}
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), dnsQueryRouteKey{}, route), time.Second)
			defer cancel()
			failureReturned, stop := make(chan struct{}), make(chan struct{})
			replies := make(chan result, 1)
			go func() {
				message, _, err := batchExchange(ctx, []dnsClient{
					schedulingTestClient{exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
						close(failureReturned)
						return answer, nil
					}},
					schedulingTestClient{exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
						select {
						case <-stop:
							route.closed.Store(true)
							return nil, tunnel.ErrDNSNativeQueryClosed
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}},
				}, query)
				replies <- result{Msg: message, Error: err}
			}()
			responseModeReceive(t, failureReturned)
			want := context.Canceled
			if nativeClose {
				close(stop)
				want = tunnel.ErrDNSNativeQueryClosed
			} else {
				cancel()
			}
			got := responseModeReceive(t, replies)
			if got.Msg != nil || !errors.Is(got.Error, want) {
				t.Fatalf("stored error reply hid cancellation: reply=%v error=%v want=%v", got.Msg, got.Error, want)
			}
		})
	}
}

func TestDNSClientBatchExplicitRCodeDoesNotStartUpstreams(t *testing.T) {
	for _, name := range []string{"success", "format_error", "server_failure", "name_error", "not_implemented", "refused"} {
		t.Run(name, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("local.example.", D.TypeA)
			local := newRCodeClient(name)
			called := make(chan struct{}, 1)
			got, cache, err := batchExchange(context.Background(), []dnsClient{
				schedulingTestClient{exchange: func(context.Context, *D.Msg) (*D.Msg, error) {
					called <- struct{}{}
					return nil, errors.New("network upstream must not run")
				}},
				local,
			}, query)
			if err != nil || got != query || got.Rcode != local.rcode || cache {
				t.Fatalf("explicit local RCODE semantics changed: reply=%v cache=%v error=%v", got, cache, err)
			}
			select {
			case <-called:
				t.Fatal("network upstream started before the explicit local RCODE was selected")
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}
