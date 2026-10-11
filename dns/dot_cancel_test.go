package dns

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/tls"
	D "github.com/miekg/dns"
)

type dnsTLSStoppedReaderConn struct {
	net.Conn
	afterReply     atomic.Bool
	closeWrite     chan struct{}
	closeWriteOnce sync.Once
}

func (c *dnsTLSStoppedReaderConn) Write(p []byte) (int, error) {
	if c.afterReply.Load() {
		c.closeWriteOnce.Do(func() { close(c.closeWrite) })
	}
	return c.Conn.Write(p)
}

// Real TLS over net.Pipe makes close_notify block deterministically once the
// server stops reading. Cancellation must interrupt the TLS transport itself,
// including after successful I/O has stopped its guard or started pool return.
func TestDNSDoTCancellationDoesNotWaitForTLSCloseNotify(t *testing.T) {
	for _, mode := range []string{"cancel-read", "deadline-read", "cancel-after-reply", "close-after-reply", "cancel-cleanup"} {
		t.Run(mode, func(t *testing.T) {
			observations := dnsstats.New()
			baseCtx := dnsstats.WithManager(context.Background(), observations)
			ctx, cancel := context.WithCancel(baseCtx)
			if mode == "deadline-read" {
				cancel()
				ctx, cancel = context.WithTimeout(baseCtx, 250*time.Millisecond)
			}
			defer cancel()
			clientWire, serverWire := net.Pipe()
			wire := &dnsTLSStoppedReaderConn{Conn: clientWire, closeWrite: make(chan struct{})}
			outbound := &dnsCancelOutbound{routingTestOutbound: newRoutingTestOutbound("tls-cancel-leaf"), tcp: wire}
			params := map[string]string{"skip-cert-verify": "true"}
			if mode == "cancel-cleanup" {
				params["disable-reuse"] = "true"
			}
			dc := newDoTClient("192.0.2.53:853", nil, params, outbound, "")
			certificate := encryptedRoutingCertificate(t, false)
			ready, serverDone := make(chan struct{}), make(chan struct{})
			serverErr := make(chan error, 1)
			replyGate, stopServer := make(chan struct{}), make(chan struct{})
			var replyOnce, stopOnce sync.Once
			reply := func() { replyOnce.Do(func() { close(replyGate) }) }
			stop := func() { stopOnce.Do(func() { close(stopServer) }) }
			t.Cleanup(func() {
				reply()
				stop()
				clientWire.Close()
				serverWire.Close()
				dc.Close()
				waitDNSCancelSignal(t, serverDone, "stopped TLS server retained active work")
			})
			go func() {
				defer close(serverDone)
				server := tls.Server(serverWire, &tls.Config{Certificates: []tls.Certificate{certificate}})
				err := server.HandshakeContext(ctx)
				var question *D.Msg
				if err == nil {
					question, err = (&D.Conn{Conn: server}).ReadMsg()
				}
				serverErr <- err
				close(ready)
				if err != nil {
					return
				}
				<-replyGate
				if mode != "cancel-read" && mode != "deadline-read" {
					wire.afterReply.Store(true)
					_ = (&D.Conn{Conn: server}).WriteMsg(routingTestAnswer(question, false))
				}
				// Intentionally do not read the client's TLS close_notify.
				<-stopServer
			}()
			query := routingTestQuery("tls-abort.example", 77)
			results := make(chan dnsCancelExchangeResult, 1)
			go func() {
				message, err := dc.exchangeDedicated(ctx, query)
				results <- dnsCancelExchangeResult{message, err}
			}()
			waitDNSCancelSignal(t, ready, "TLS server did not receive the DNS query")
			if err := <-serverErr; err != nil {
				t.Fatalf("real TLS setup failed: %v", err)
			}
			locked := false
			defer func() {
				if locked {
					dc.access.Unlock()
				}
			}()
			switch mode {
			case "cancel-read":
				cancel()
			case "cancel-after-reply", "close-after-reply":
				dc.access.Lock()
				locked = true
				reply()
				deadline := time.NewTimer(time.Second)
				defer deadline.Stop()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for observations.Upstreams().Items[0].Successes != 1 {
					select {
					case <-tick.C:
					case <-deadline.C:
						t.Fatal("TLS exchange did not complete before pool return")
					}
				}
				if mode == "close-after-reply" {
					dc.native.reset(true)
				} else {
					cancel()
				}
				dc.access.Unlock()
				locked = false
			case "cancel-cleanup":
				reply()
				waitDNSCancelSignal(t, wire.closeWrite, "DoT cleanup did not reach the blocked TLS close_notify")
				cancel()
			}
			got := waitDNSCancelResult(t, results)
			stats := observations.Upstreams().Items[0]
			switch mode {
			case "cancel-read":
				if !errors.Is(got.err, context.Canceled) || stats.Canceled != 1 || stats.Errors != 0 {
					t.Fatalf("TLS I/O interruption lost cancellation: %v, %+v", got.err, stats)
				}
			case "deadline-read":
				var timeout net.Error
				if !(errors.Is(got.err, context.DeadlineExceeded) || (errors.As(got.err, &timeout) && timeout.Timeout())) || stats.Timeouts != 1 || stats.Errors != 1 || stats.Canceled != 0 {
					t.Fatalf("TLS deadline lost its actual timeout result: %v, %+v", got.err, stats)
				}
			default:
				if got.err != nil || got.message == nil || got.message.Id != query.Id || stats.Successes != 1 || stats.Canceled != 0 || stats.Errors != 0 {
					t.Fatalf("TLS cleanup changed a completed response: %+v, %+v", got, stats)
				}
			}
		})
	}
}
