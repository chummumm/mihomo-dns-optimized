package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/tls"
	D "github.com/miekg/dns"
)

type sharedDNSWireQuery struct {
	message *D.Msg
	peer    net.Addr
	session int64
	reply   func(*D.Msg) error
	close   func() error
	write   func([]byte) (int, error)
}

type sharedDNSTestServer struct {
	address string
	queries chan sharedDNSWireQuery
	done    chan struct{}
	accepts atomic.Int64
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	wg      sync.WaitGroup
}

func newSharedDNSTestServer(t *testing.T, protocol string) *sharedDNSTestServer {
	t.Helper()
	s := &sharedDNSTestServer{queries: make(chan sharedDNSWireQuery, 1024), done: make(chan struct{}), conns: make(map[net.Conn]struct{})}
	var shutdown func()
	if protocol == "udp" {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s.address = pc.LocalAddr().String()
		shutdown = func() { _ = pc.Close() }
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			wire := make([]byte, 65535)
			for {
				n, peer, err := pc.ReadFrom(wire)
				if err != nil {
					return
				}
				message := new(D.Msg)
				if message.Unpack(wire[:n]) != nil {
					continue
				}
				query := sharedDNSWireQuery{message: message, peer: peer, session: 1, close: pc.Close,
					reply: func(answer *D.Msg) error {
						wire, err := answer.Pack()
						if err == nil {
							_, err = pc.WriteTo(wire, peer)
						}
						return err
					}}
				select {
				case s.queries <- query:
				case <-s.done:
					return
				}
			}
		}()
	} else {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if protocol == "tls" {
			listener = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{encryptedRoutingCertificate(t, false)}})
		}
		s.address = listener.Addr().String()
		shutdown = func() { _ = listener.Close() }
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				id := s.accepts.Add(1)
				s.mu.Lock()
				s.conns[conn] = struct{}{}
				s.mu.Unlock()
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					defer func() {
						s.mu.Lock()
						delete(s.conns, conn)
						s.mu.Unlock()
						abortDNSConn(conn)
					}()
					stream := &D.Conn{Conn: conn}
					var writes sync.Mutex
					for {
						message, err := stream.ReadMsg()
						if err != nil {
							return
						}
						query := sharedDNSWireQuery{message: message, peer: conn.RemoteAddr(), session: id, close: conn.Close,
							write: func(wire []byte) (int, error) { writes.Lock(); defer writes.Unlock(); return conn.Write(wire) },
							reply: func(answer *D.Msg) error { writes.Lock(); defer writes.Unlock(); return stream.WriteMsg(answer) }}
						select {
						case s.queries <- query:
						case <-s.done:
							return
						}
					}
				}()
			}
		}()
	}
	t.Cleanup(func() {
		close(s.done)
		shutdown()
		s.mu.Lock()
		for conn := range s.conns {
			abortDNSConn(conn)
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

func (s *sharedDNSTestServer) next(t *testing.T) sharedDNSWireQuery {
	t.Helper()
	select {
	case query := <-s.queries:
		return query
	case <-time.After(2 * time.Second):
		t.Fatal("shared DNS server did not receive the next query")
		return sharedDNSWireQuery{}
	}
}

func newSharedDNSTestClient(t *testing.T, protocol, address string) dnsClient {
	t.Helper()
	var dc dnsClient
	if protocol == "tls" {
		dc = newDoTClient(address, nil, map[string]string{"skip-cert-verify": "true"}, nil, "")
	} else {
		dc = newClient(address, nil, protocol, nil, nil, "")
	}
	t.Cleanup(func() { closeNativeClient(dc) })
	return dc
}

func sharedDNSAsync(dc interface {
	ExchangeContext(context.Context, *D.Msg) (*D.Msg, error)
}, ctx context.Context, message *D.Msg) <-chan dnsCancelExchangeResult {
	results := make(chan dnsCancelExchangeResult, 1)
	go func() {
		answer, err := dc.ExchangeContext(ctx, message)
		results <- dnsCancelExchangeResult{answer, err}
	}()
	return results
}

func mustPrepareSharedRoute(t *testing.T, r *Resolver, ctx context.Context, query *D.Msg) context.Context {
	t.Helper()
	prepared, immediate, err := r.prepareDNSRouting(ctx, query)
	if err != nil || immediate != nil {
		t.Fatalf("prepare shared route: immediate=%v err=%v", immediate, err)
	}
	return prepared
}

func sharedDNSResult(t *testing.T, results <-chan dnsCancelExchangeResult) dnsCancelExchangeResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("shared DNS query did not finish")
		return dnsCancelExchangeResult{}
	}
}

func sharedDNSReply(t *testing.T, query sharedDNSWireQuery, suffix byte) {
	t.Helper()
	answer := routingTestAnswer(query.message, false)
	answer.Answer[0].(*D.A).A = net.IPv4(192, 0, 2, suffix)
	if err := query.reply(answer); err != nil {
		t.Fatal(err)
	}
}

func TestDNSClientSharedConcurrentOutOfOrder(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp", "tls"} {
		t.Run(protocol, func(t *testing.T) {
			server := newSharedDNSTestServer(t, protocol)
			dc := newSharedDNSTestClient(t, protocol, server.address)
			const count = 16
			results := make([]<-chan dnsCancelExchangeResult, count)
			originals := make([]*D.Msg, count)
			for i := 0; i < count; i++ {
				originals[i] = routingTestQuery(fmt.Sprintf("pipeline-%d.example", i), 77)
				results[i] = sharedDNSAsync(dc, context.Background(), originals[i])
			}
			queries := make([]sharedDNSWireQuery, count)
			ids := make(map[uint16]bool)
			for i := 0; i < count; i++ {
				queries[i] = server.next(t)
				if ids[queries[i].message.Id] {
					t.Fatal("concurrent requests reused an internal transaction ID")
				}
				ids[queries[i].message.Id] = true
				if queries[i].session != queries[0].session || queries[i].peer.String() != queries[0].peer.String() {
					t.Fatal("concurrent requests did not share their socket")
				}
			}
			for i := count - 1; i >= 0; i-- {
				sharedDNSReply(t, queries[i], 99)
			}
			for i, result := range results {
				got := sharedDNSResult(t, result)
				if got.err != nil || got.message == nil || got.message.Id != 77 || got.message.Question[0] != originals[i].Question[0] || originals[i].Id != 77 {
					t.Fatalf("out-of-order dispatch changed caller identity: %+v", got)
				}
			}
		})
	}
}

func TestDNSClientSharedCancelSiblingAndLateResponse(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp", "tls"} {
		t.Run(protocol, func(t *testing.T) {
			server := newSharedDNSTestServer(t, protocol)
			dc := newSharedDNSTestClient(t, protocol, server.address)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			query := routingTestQuery("same-question.example", 78)
			first := sharedDNSAsync(dc, ctx, query)
			old := server.next(t)
			sibling := sharedDNSAsync(dc, context.Background(), query)
			other := server.next(t)
			cancel()
			if got := sharedDNSResult(t, first); !errors.Is(got.err, context.Canceled) {
				t.Fatalf("canceled waiter lost its cause: %+v", got)
			}
			current := sharedDNSAsync(dc, context.Background(), query)
			newest := server.next(t)
			if old.message.Id == other.message.Id || old.message.Id == newest.message.Id || other.message.Id == newest.message.Id {
				t.Fatal("cancel immediately recycled a transaction ID")
			}
			if old.peer.String() != newest.peer.String() || old.session != newest.session {
				t.Fatal("canceling one waiter closed the shared socket")
			}
			sharedDNSReply(t, old, 11) // identical question, but a permanently retired ID
			sharedDNSReply(t, other, 22)
			sharedDNSReply(t, old, 11) // a duplicate must also remain harmless
			sharedDNSReply(t, newest, 33)
			for _, test := range []struct {
				result <-chan dnsCancelExchangeResult
				ip     string
			}{{sibling, "192.0.2.22"}, {current, "192.0.2.33"}} {
				got := sharedDNSResult(t, test.result)
				if got.err != nil || got.message == nil || got.message.Id != query.Id || got.message.Answer[0].(*D.A).A.String() != test.ip {
					t.Fatalf("late response crossed waiter boundaries: %+v", got)
				}
			}
		})
	}
}

func TestDNSClientSharedRejectsUnrelatedResponse(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp", "tls"} {
		t.Run(protocol, func(t *testing.T) {
			server := newSharedDNSTestServer(t, protocol)
			dc := newSharedDNSTestClient(t, protocol, server.address)
			result := sharedDNSAsync(dc, context.Background(), routingTestQuery("verified.example", 79))
			query := server.next(t)
			for _, mutate := range []func(*D.Msg){
				func(m *D.Msg) { m.Response = false },
				func(m *D.Msg) { m.Opcode = D.OpcodeStatus },
				func(m *D.Msg) { m.Question[0].Name = "unrelated.example." },
				func(m *D.Msg) { m.Question[0].Qtype = D.TypeAAAA },
				func(m *D.Msg) { m.Question[0].Qclass = D.ClassCHAOS },
			} {
				answer := routingTestAnswer(query.message, false)
				answer.Answer[0].(*D.A).A = net.IPv4(192, 0, 2, 11)
				mutate(answer)
				if err := query.reply(answer); err != nil {
					t.Fatal(err)
				}
			}
			if protocol == "udp" {
				spoof, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				wire, _ := routingTestAnswer(query.message, false).Pack()
				_, _ = spoof.WriteTo(wire, query.peer)
				_ = spoof.Close()
			}
			sharedDNSReply(t, query, 33)
			got := sharedDNSResult(t, result)
			if got.err != nil || got.message == nil || got.message.Answer[0].(*D.A).A.String() != "192.0.2.33" {
				t.Fatalf("unrelated/source-spoofed response was accepted: %+v", got)
			}
		})
	}
}

func TestDNSClientSharedReconnectIsBounded(t *testing.T) {
	for _, protocol := range []string{"tcp", "tls"} {
		for _, recover := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recover-%t", protocol, recover), func(t *testing.T) {
				server := newSharedDNSTestServer(t, protocol)
				dc := newSharedDNSTestClient(t, protocol, server.address)
				result := sharedDNSAsync(dc, context.Background(), routingTestQuery("reconnect.example", 80))
				first := server.next(t)
				_ = first.close()
				second := server.next(t)
				if second.session == first.session {
					t.Fatal("failed stream remained reusable")
				}
				if recover {
					sharedDNSReply(t, second, 33)
				} else {
					_ = second.close()
				}
				got := sharedDNSResult(t, result)
				if (got.err == nil) != recover || server.accepts.Load() != 2 {
					t.Fatalf("connection failure was not retried exactly once: %+v, accepts=%d", got, server.accepts.Load())
				}
			})
		}
	}
}

func TestDNSClientSharedCanceledPartialWriteKeepsFraming(t *testing.T) {
	client, server := net.Pipe()
	p := newSharedDNSPool(false)
	t.Cleanup(func() { p.close(); _ = server.Close() })
	var dials atomic.Int32
	dial := func(context.Context) (net.Conn, error) { dials.Add(1); return client, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan dnsCancelExchangeResult, 1)
	go func() {
		m, e := p.exchange(ctx, routingTestQuery("partial-first.example", 81), "route", dial)
		first <- dnsCancelExchangeResult{m, e}
	}()
	var header [2]byte
	if _, err := io.ReadFull(server, header[:]); err != nil {
		t.Fatal(err)
	}
	length := int(binary.BigEndian.Uint16(header[:]))
	part := make([]byte, 5)
	if _, err := io.ReadFull(server, part); err != nil {
		t.Fatal(err)
	}
	cancel()
	if got := sharedDNSResult(t, first); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("blocked write delayed waiter cancellation: %+v", got)
	}
	second := make(chan dnsCancelExchangeResult, 1)
	go func() {
		m, e := p.exchange(context.Background(), routingTestQuery("partial-second.example", 82), "route", dial)
		second <- dnsCancelExchangeResult{m, e}
	}()
	if _, err := io.ReadFull(server, make([]byte, length-len(part))); err != nil {
		t.Fatal(err)
	}
	stream := &D.Conn{Conn: server}
	query, err := stream.ReadMsg()
	if err != nil || query.Question[0].Name != "partial-second.example." {
		t.Fatalf("a canceled partial frame corrupted its sibling: %v, %v", query, err)
	}
	if err := stream.WriteMsg(routingTestAnswer(query, false)); err != nil {
		t.Fatal(err)
	}
	got := sharedDNSResult(t, second)
	if got.err != nil || got.message.Id != 82 || dials.Load() != 1 {
		t.Fatalf("sibling lost the existing framed stream: %+v", got)
	}
}

func TestDNSClientSharedRotationAndPendingBounds(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp", "tls"} {
		t.Run(protocol, func(t *testing.T) {
			server := newSharedDNSTestServer(t, protocol)
			dc := newSharedDNSTestClient(t, protocol, server.address)
			p := newSharedDNSPool(protocol == "udp")
			p.maxPending, p.idLimit = 2, 2
			switch client := dc.(type) {
			case *client:
				client.shared.pool = p
			case *dnsOverTLS:
				client.shared.pool = p
			}
			var results []<-chan dnsCancelExchangeResult
			var queries []sharedDNSWireQuery
			for i := 0; i < 4; i++ {
				results = append(results, sharedDNSAsync(dc, context.Background(), routingTestQuery(fmt.Sprintf("rotate-%d.example", i), 83)))
				queries = append(queries, server.next(t))
			}
			p.mu.Lock()
			if len(p.sessions) != 2 {
				t.Errorf("rotation created %d sessions, expected one active and one draining", len(p.sessions))
			}
			for session := range p.sessions {
				if len(session.pending) > 2 || session.usedCount != 2 {
					t.Errorf("pending/ID limits exceeded: pending=%d used=%d", len(session.pending), session.usedCount)
				}
			}
			p.mu.Unlock()
			if queries[0].peer.String() != queries[1].peer.String() || queries[0].peer.String() == queries[2].peer.String() {
				t.Fatal("ID-space rotation did not create a fresh socket")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			blocked := sharedDNSAsync(dc, ctx, routingTestQuery("rotation-full.example", 84))
			if got := sharedDNSResult(t, blocked); !errors.Is(got.err, context.DeadlineExceeded) {
				t.Fatalf("full generation bound did not honor the queued query deadline: %+v", got)
			}
			for _, query := range queries {
				sharedDNSReply(t, query, 33)
			}
			for _, result := range results {
				if got := sharedDNSResult(t, result); got.err != nil {
					t.Fatal(got.err)
				}
			}
		})
	}
}

func TestDNSClientSharedResetAndClose(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp", "tls"} {
		t.Run(protocol, func(t *testing.T) {
			server := newSharedDNSTestServer(t, protocol)
			dc := newSharedDNSTestClient(t, protocol, server.address)
			pending := sharedDNSAsync(dc, context.Background(), routingTestQuery("before-reset.example", 85))
			old := server.next(t)
			dc.ResetConnection()
			if got := sharedDNSResult(t, pending); !errors.Is(got.err, net.ErrClosed) {
				t.Fatalf("reset did not close pending shared queries: %+v", got)
			}
			next := sharedDNSAsync(dc, context.Background(), routingTestQuery("after-reset.example", 86))
			fresh := server.next(t)
			if old.peer.String() == fresh.peer.String() {
				t.Fatal("reset reused the old socket")
			}
			sharedDNSReply(t, fresh, 33)
			if got := sharedDNSResult(t, next); got.err != nil {
				t.Fatal(got.err)
			}
			closeNativeClient(dc)
			if _, err := dc.ExchangeContext(context.Background(), routingTestQuery("closed.example", 87)); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("closed client started another shared session: %v", err)
			}
		})
	}
}

func sharedDNSPoolOf(dc dnsClient) *sharedDNSPool {
	switch c := dc.(type) {
	case *client:
		c.shared.mu.Lock()
		defer c.shared.mu.Unlock()
		return c.shared.pool
	case *dnsOverTLS:
		c.shared.mu.Lock()
		defer c.shared.mu.Unlock()
		return c.shared.pool
	default:
		panic("not a shared DNS client")
	}
}

func waitSharedDNSCondition(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("shared DNS barrier was not reached")
		}
		runtime.Gosched()
	}
}

func TestDNSClientSharedCompletedIOWinsLateCancellation(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp", "tls"} {
		for _, fail := range []bool{false, true} {
			if fail && protocol == "udp" {
				continue // closing a remote UDP socket does not deliver local EOF
			}
			t.Run(fmt.Sprintf("%s/fail-%t", protocol, fail), func(t *testing.T) {
				server := newSharedDNSTestServer(t, protocol)
				dc := newSharedDNSTestClient(t, protocol, server.address)
				observations := dnsstats.New()
				ctx, cancel := context.WithCancel(dnsstats.WithManager(context.Background(), observations))
				defer cancel()
				result := sharedDNSAsync(dc, ctx, routingTestQuery("completed-shared.example", 90))
				query := server.next(t)
				p := sharedDNSPoolOf(dc)
				p.mu.Lock()
				locked := true
				defer func() {
					if locked {
						p.mu.Unlock()
					}
				}()
				session := p.current
				if fail {
					_ = query.close()
				} else {
					sharedDNSReply(t, query, 33)
				}
				// Hold only the bookkeeping lock. Real I/O and parsing complete
				// independently, then the reader owns deliveryMu while waiting
				// to publish. A late cancel must queue behind that completion.
				waitSharedDNSCondition(t, func() bool {
					if session.deliveryMu.TryLock() {
						session.deliveryMu.Unlock()
						return false
					}
					return true
				})
				cancel()
				p.mu.Unlock()
				locked = false
				got := sharedDNSResult(t, result)
				if fail {
					if !errors.Is(got.err, io.EOF) || errors.Is(got.err, context.Canceled) {
						t.Fatalf("late cancellation replaced an already-read EOF: %+v", got)
					}
				} else if got.err != nil || got.message == nil || got.message.Id != 90 {
					t.Fatalf("late cancellation replaced an already-validated reply: %+v", got)
				}
				stats := observations.Upstreams().Items[0]
				if stats.Canceled != 0 || (fail && stats.Errors != 1) || (!fail && stats.Successes != 1) {
					t.Fatalf("completed shared I/O was misclassified: %+v", stats)
				}
			})
		}
	}
}

func TestDNSClientSharedPartialReplyHasConnectionDeadline(t *testing.T) {
	for _, protocol := range []string{"tcp", "tls"} {
		for _, payload := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/payload-%t", protocol, payload), func(t *testing.T) {
				server := newSharedDNSTestServer(t, protocol)
				dc := newSharedDNSTestClient(t, protocol, server.address)
				p := newSharedDNSPool(false)
				p.ioTimeout = 200 * time.Millisecond
				switch c := dc.(type) {
				case *client:
					c.shared.pool = p
				case *dnsOverTLS:
					c.shared.pool = p
				}
				first := sharedDNSAsync(dc, context.Background(), routingTestQuery("partial-reply-first.example", 91))
				old := server.next(t)
				partial := []byte{0}
				if payload {
					partial = []byte{0, 50, 1, 2, 3}
				}
				if _, err := old.write(partial); err != nil {
					t.Fatal(err)
				}
				second := sharedDNSAsync(dc, context.Background(), routingTestQuery("partial-reply-second.example", 92))
				oldSibling := server.next(t)
				if oldSibling.session != old.session {
					t.Fatal("sibling did not start on the still-live stream")
				}
				// Both requests retry after this connection-level frame timeout;
				// a fresh request must not extend the incomplete frame's budget.
				fresh := server.next(t)
				freshSibling := server.next(t)
				if fresh.session == old.session || freshSibling.session != fresh.session || server.accepts.Load() != 2 {
					t.Fatal("partial reply pinned the old stream or caused unbounded reconnects")
				}
				sharedDNSReply(t, fresh, 33)
				sharedDNSReply(t, freshSibling, 33)
				for _, result := range []<-chan dnsCancelExchangeResult{first, second} {
					if got := sharedDNSResult(t, result); got.err != nil {
						t.Fatal(got.err)
					}
				}
			})
		}
	}
}

func TestDNSClientSharedAdmissionBoundsWaitingQueries(t *testing.T) {
	server := newSharedDNSTestServer(t, "tcp")
	dc := newSharedDNSTestClient(t, "tcp", server.address).(*client)
	p := newSharedDNSPool(false)
	p.maxPending = 2
	dc.shared.pool = p
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var results []<-chan dnsCancelExchangeResult
	for i := 0; i < 2; i++ {
		results = append(results, sharedDNSAsync(dc, ctx, routingTestQuery(fmt.Sprintf("active-%d.example", i), 93)))
		server.next(t)
	}
	for i := 0; i < 2; i++ {
		results = append(results, sharedDNSAsync(dc, ctx, routingTestQuery(fmt.Sprintf("queued-%d.example", i), 94)))
	}
	waitSharedDNSCondition(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.waiters == 2 })
	if _, err := dc.ExchangeContext(context.Background(), routingTestQuery("over-limit.example", 95)); !errors.Is(err, errSharedDNSBusy) {
		t.Fatalf("saturated admission did not reject excess work: %v", err)
	}
	if server.accepts.Load() != 1 {
		t.Fatal("busy rejection reconnected or retried")
	}
	cancel()
	for _, result := range results {
		if got := sharedDNSResult(t, result); !errors.Is(got.err, context.Canceled) {
			t.Fatalf("admitted waiter did not cancel: %+v", got)
		}
	}
}

func TestDNSClientSharedResolverOwnsEveryConfiguredPool(t *testing.T) {
	servers := make([]*sharedDNSTestServer, 7)
	ns := make([]NameServer, len(servers))
	for i := range servers {
		protocol := []string{"udp", "tcp", "tls"}[i%3]
		servers[i] = newSharedDNSTestServer(t, protocol)
		ns[i] = NameServer{Net: protocol, Addr: servers[i].address, Params: map[string]string{"skip-cert-verify": "true"}}
	}
	rs := NewResolver(Config{
		Default: []NameServer{ns[0]}, Main: []NameServer{ns[1]},
		Policy:      []Policy{{Domain: "policy.example", NameServers: []NameServer{ns[2]}}},
		ProxyServer: []NameServer{ns[3]}, ProxyServerPolicy: []Policy{{Domain: "proxy-policy.example", NameServers: []NameServer{ns[4]}}},
		DirectServer: []NameServer{ns[5]}, Fallback: []NameServer{ns[6]},
	})
	t.Cleanup(rs.Close)
	if len(rs.clientOwner.clients) != 7 {
		t.Fatalf("not every independent configured client was registered: %d", len(rs.clientOwner.clients))
	}
	var results []<-chan dnsCancelExchangeResult
	for i, dc := range rs.clientOwner.clients {
		results = append(results, sharedDNSAsync(dc, context.Background(), routingTestQuery(fmt.Sprintf("owner-%d.example", i), 96)))
		servers[i].next(t)
	}
	rs.Close()
	rs.ResetConnection() // resetting a closed collection must not resurrect it
	for i, result := range results {
		if got := sharedDNSResult(t, result); !errors.Is(got.err, net.ErrClosed) {
			t.Fatalf("owned client %d survived configuration close: %+v", i, got)
		}
		if _, err := rs.clientOwner.clients[i].ExchangeContext(context.Background(), routingTestQuery("closed-owner.example", 97)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("owned client %d could restart after close: %v", i, err)
		}
	}
}

func TestDNSClientSharedImmediateEOFHasBoundedRetry(t *testing.T) {
	state := &sharedDNSState{}
	t.Cleanup(func() { state.reset(true) })
	var dials atomic.Int32
	_, err := state.exchange(context.Background(), routingTestQuery("immediate-close.example", 98), "fixed-route", false,
		func(context.Context) (net.Conn, error) {
			dials.Add(1)
			client, server := net.Pipe()
			_ = server.Close() // EOF precedes admission to this new session
			return client, nil
		})
	if err == nil || errors.Is(err, context.DeadlineExceeded) || dials.Load() != 2 {
		t.Fatalf("immediate EOF escaped the one-reconnect bound: dials=%d err=%v", dials.Load(), err)
	}
}

func TestDNSClientSharedDefaultQueryTimeout(t *testing.T) {
	server := newSharedDNSTestServer(t, "udp")
	dc := newSharedDNSTestClient(t, "udp", server.address)
	start := time.Now()
	result := sharedDNSAsync(dc, context.Background(), routingTestQuery("default-timeout.example", 99))
	server.next(t)
	select {
	case got := <-result:
		if !errors.Is(got.err, context.DeadlineExceeded) || time.Since(start) < 4*time.Second {
			t.Fatalf("background-context query lost the default five-second budget: elapsed=%v result=%+v", time.Since(start), got)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("a DNS query without an explicit caller deadline exceeded its default budget")
	}
}
