package dns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/metacubex/http"
	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/dnsstats"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"
	"github.com/metacubex/quic-go"
	D "github.com/miekg/dns"
)

type dnsCancelHTTPTransport func(*http.Request) (*http.Response, error)

func (f dnsCancelHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type dnsCancelHTTPBody struct {
	reader io.Reader
	err    error
	cancel context.CancelFunc
}

func (b *dnsCancelHTTPBody) Read(p []byte) (int, error) {
	if b.err != nil {
		b.cancel()
		return 0, b.err
	}
	n, err := b.reader.Read(p)
	b.cancel()
	return n, err
}

func (b *dnsCancelHTTPBody) Close() error { return nil }

func TestDNSDoHLateCancellationPreservesProtocolResult(t *testing.T) {
	for _, nativeErr := range []error{nil, io.ErrUnexpectedEOF, syscall.ECONNRESET} {
		t.Run(fmt.Sprint(nativeErr), func(t *testing.T) {
			m := dnsstats.New()
			ctx, cancel := context.WithCancel(dnsstats.WithManager(context.Background(), m))
			defer cancel()
			query := routingTestQuery("late-http.example", 71)
			reply := routingTestAnswer(query, false)
			reply.Id = 0
			wire, err := reply.Pack()
			if err != nil {
				t.Fatal(err)
			}
			client := newDoHClient("https://dns.example/dns-query", nil, false, nil, nil, "").(*dnsOverHTTPS)
			defer client.Close()
			client.client = &http.Client{Transport: dnsCancelHTTPTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &dnsCancelHTTPBody{reader: bytes.NewReader(wire), err: nativeErr, cancel: cancel}}, nil
			})}
			answer, err := client.ExchangeContext(ctx, query)
			if nativeErr == nil {
				if err != nil || answer == nil || answer.Id != query.Id {
					t.Fatalf("late cancel replaced valid DoH reply: %v, %v", answer, err)
				}
			} else if !errors.Is(err, nativeErr) || errors.Is(err, context.Canceled) {
				t.Fatalf("late cancel replaced native HTTP read failure: %v", err)
			}
			s := m.Upstreams().Items[0]
			if s.Canceled != 0 || (nativeErr == nil && s.Successes != 1) || (nativeErr != nil && s.Errors != 1) {
				t.Fatalf("late HTTP cancel miscounted: %+v", s)
			}
		})
	}
}

func TestDNSObservabilityIOErrorClassification(t *testing.T) {
	m := dnsstats.New()
	ctx, cancel := context.WithCancel(dnsstats.WithManager(context.Background(), m))
	cancel() // late context state is never a substitute for the actual result
	for _, item := range []struct {
		message *D.Msg
		err     error
	}{
		{&D.Msg{MsgHdr: D.MsgHdr{Rcode: D.RcodeSuccess}}, nil},
		{&D.Msg{MsgHdr: D.MsgHdr{Rcode: D.RcodeNameError}}, nil},
		{&D.Msg{MsgHdr: D.MsgHdr{Rcode: D.RcodeServerFailure}}, nil},
		{nil, io.EOF}, {nil, syscall.ECONNRESET}, {nil, net.ErrClosed},
		{nil, fmt.Errorf("read failed: %w", &net.OpError{Op: "read", Net: "udp", Err: os.ErrDeadlineExceeded})},
		{nil, fmt.Errorf("canceled read: %w", context.Canceled)},
		{nil, context.DeadlineExceeded},
		{nil, tunnel.ErrDNSNativeQueryClosed},
	} {
		finishDNSUpstream(observeDNSUpstream(ctx, "udp://192.0.2.53:53"), item.message, item.err)
	}
	s := m.Upstreams().Items[0]
	if s.Attempts != 10 || s.Successes != 2 || s.Errors != 6 || s.Canceled != 2 || s.Timeouts != 2 || s.RCodeErrors != 1 {
		t.Fatalf("I/O results inferred from late context state or socket timeout lost: %+v", s)
	}
}

func TestDNSQUICCancellationDoesNotHideRemoteFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fired := make(chan struct{})
	guard := N.NewDNSCancelGuard(ctx, func() { close(fired) })
	defer guard.Stop()
	cancel()
	<-fired
	for _, err := range []error{nil, io.EOF, syscall.ECONNRESET, &quic.StreamError{Remote: true, ErrorCode: 0}, &quic.StreamError{Remote: false, ErrorCode: 7}} {
		if got := dnsQUICCancelError(guard, err); got != err {
			t.Fatalf("late cancellation hid remote/native I/O result: %v -> %v", err, got)
		}
	}
	local := &quic.StreamError{Remote: false, ErrorCode: 0}
	if got := dnsQUICCancelError(guard, local); !errors.Is(got, context.Canceled) {
		t.Fatalf("local canceled stream not classified: %v", got)
	}
}

func TestDNSObservedOutboundUsesPhysicalLeafAndNotBootstrapCaller(t *testing.T) {
	routingTestEnable(t)
	m := dnsstats.New()
	leaf := newRoutingTestOutbound("actual-leaf")
	leaf.kind.Store(int32(C.Socks5))
	r := routingTestResolver()
	defer r.Close()
	for i, groupName := range []string{"policy-a", "policy-b"} {
		group := &routingTestGroup{routingTestBase: &routingTestBase{name: groupName, kind: C.Selector}, leaf: routingTestProxy(leaf)}
		ctx := dnsstats.WithManager(routingTestContext(group, routingTestOrigin()), m)
		query := routingTestQuery(fmt.Sprintf("observed-%d.example", i), uint16(i+1))
		if _, err := encryptedRoutingRawExchange(r, ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	rows := m.Upstreams().Items
	if len(rows) != 1 || rows[0].Attempts != 2 || rows[0].Outbound != "actual-leaf" || rows[0].OutboundType != "proxy" {
		t.Fatalf("same leaf through different groups split: %+v", rows)
	}
	ctx := dnsstats.WithManager(routingTestContext(leaf, routingTestOrigin()), m)
	ctx, _, err := r.prepareDNSRouting(ctx, routingTestQuery("bootstrap-parent.example", 3))
	if err != nil {
		t.Fatal(err)
	}
	finishDNSUpstream(observeDNSUpstream(icontext.WithDNSBootstrap(ctx), "udp://192.0.2.53:53"), &D.Msg{}, nil)
	rows = m.Upstreams().Items
	if len(rows) != 1 || rows[0].Attempts != 2 {
		t.Fatalf("bootstrap appeared in business upstream statistics: %+v", rows)
	}
	for _, row := range rows {
		if row.OutboundType == "unknown" && row.Outbound != "" {
			t.Fatalf("unknown bootstrap labeled as a route: %+v", row)
		}
	}
}
