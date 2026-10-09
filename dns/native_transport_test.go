package dns

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/tunnel/statistic"
	D "github.com/miekg/dns"
)

type nativePoolTestClient struct{ closed atomic.Int32 }

func (*nativePoolTestClient) ExchangeContext(context.Context, *D.Msg) (*D.Msg, error) {
	return nil, errors.New("pool test does not exchange DNS")
}
func (*nativePoolTestClient) Address() string  { return "pool-test" }
func (*nativePoolTestClient) ResetConnection() {}
func (c *nativePoolTestClient) Close() error   { c.closed.Add(1); return nil }

func TestDNSRuleRoutingNativePoolRetainsActiveSiblings(t *testing.T) {
	pool := newNativeTransportPool(2)
	t.Cleanup(pool.close)
	clients := map[string]*nativePoolTestClient{}
	acquire := func(key string) *nativeTransportEntry {
		t.Helper()
		entry, err := pool.acquire(context.Background(), key, func() dnsClient {
			client := &nativePoolTestClient{}
			clients[key] = client
			return client
		})
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	first := acquire("first")
	sibling := acquire("first")
	if first != sibling {
		t.Fatal("same immutable route did not reuse its protocol client")
	}
	old := acquire("old")
	pool.release(old)
	next := acquire("next")
	if clients["old"].closed.Load() != 1 || clients["first"].closed.Load() != 0 {
		t.Fatal("LRU must close only the inactive client")
	}
	pool.release(first)
	pool.release(next)
	last := acquire("last")
	if clients["next"].closed.Load() != 1 || clients["first"].closed.Load() != 0 {
		t.Fatal("one remaining sibling must keep its client alive")
	}
	pool.close()
	select {
	case <-pool.lifetime.Done():
	default:
		t.Fatal("lifecycle shutdown did not cancel active query contexts")
	}
	pool.release(sibling)
	pool.release(last)
	pool.close()
	for key, client := range clients {
		if count := client.closed.Load(); count != 1 {
			t.Errorf("%s was closed %d times", key, count)
		}
	}
	if _, err := pool.acquire(context.Background(), "closed", func() dnsClient {
		t.Fatal("closed pool created a new client")
		return nil
	}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed pool returned %v", err)
	}
}

func TestDNSRuleRoutingNativePoolWaitsWithoutEvictingActiveClient(t *testing.T) {
	pool := newNativeTransportPool(1)
	t.Cleanup(pool.close)
	client := &nativePoolTestClient{}
	active, err := pool.acquire(context.Background(), "active", func() dnsClient { return client })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := pool.acquire(ctx, "waiting", func() dnsClient {
		t.Fatal("capacity was exceeded while every client was active")
		return nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked acquisition returned %v", err)
	}
	if client.closed.Load() != 0 {
		t.Fatal("waiting for capacity interrupted an active query")
	}
	pool.release(active)
	if _, err := pool.acquire(context.Background(), "replacement", func() dnsClient { return &nativePoolTestClient{} }); err != nil {
		t.Fatal(err)
	}
	if client.closed.Load() != 1 {
		t.Fatal("released client was not eligible for replacement")
	}
}

func TestDNSRuleRoutingNativePoolResetAndClose(t *testing.T) {
	state := &nativeClientState{pool: newNativeTransportPool(1)}
	first := state.pool
	state.reset(false)
	if state.isClosed() || state.pool != nil || first.lifetime.Err() == nil {
		t.Fatal("connection reset must cancel the old pool and permit a fresh pool")
	}
	second := newNativeTransportPool(1)
	state.pool = second
	state.reset(true)
	state.reset(false)
	if !state.isClosed() || state.pool != nil || second.lifetime.Err() == nil {
		t.Fatal("resolver closure must remain terminal after a subsequent reset")
	}
}

func TestDNSRuleRoutingNativeDashboardCloseDoesNotRetry(t *testing.T) {
	for _, protocol := range []string{"udp53", "h2"} {
		t.Run(protocol, func(t *testing.T) {
			routingTestEnable(t)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var leaf C.ProxyAdapter
			var servers []NameServer
			var seen func()
			var pending func() int
			if protocol == "udp53" {
				outbound := newRoutingTestOutbound("dashboard-raw")
				outbound.gate = release
				leaf, servers = outbound, []NameServer{{Addr: "192.0.2.53:53"}}
				seen = func() {
					select {
					case <-outbound.questions:
					case <-time.After(time.Second):
						t.Fatal("DNS did not reach its upstream")
					}
				}
				pending = func() int { return len(outbound.questions) }
			} else {
				server := newEncryptedRoutingServer(t, protocol, true, func(ctx context.Context, _ string) bool {
					select {
					case <-release:
						return true
					case <-ctx.Done():
						return false
					}
				})
				leaf, servers = newEncryptedRoutingOutbound("dashboard-h2"), []NameServer{server.ns}
				seen = func() { server.next(t) }
				pending = func() int { return len(server.seen) }
			}
			r := NewResolver(Config{RuleRouting: true, Main: servers}).Resolver
			t.Cleanup(func() { unblock(); r.Close() })
			query := routingTestQuery("dashboard-stop.example", 71)
			ctx := r.cacheControl.Context(routingTestContext(leaf, routingTestOrigin()))
			prepared, _, err := r.prepareDNSRouting(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			flightKey := r.cacheControl.FlightKey(ctx, dnsCacheKey(prepared, query.Question[0]))
			result := make(chan error, 1)
			go func() { _, err := r.ExchangeContext(ctx, query); result <- err }()
			seen()
			var tracker statistic.Tracker
			statistic.DefaultManager.Range(func(item statistic.Tracker) bool {
				if item.Info().Metadata.Host == strings.TrimSuffix(query.Question[0].Name, ".") {
					tracker = item
					return false
				}
				return true
			})
			if tracker == nil {
				t.Fatal("active DNS query was not visible in connections")
			}
			_ = tracker.Close()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("manually closed DNS exchange succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("dashboard close did not interrupt the resolver")
			}
			// The old automatic retry is registered before the caller returns.
			// Joining its key is a deterministic barrier, not a sleep window.
			barrier := r.group.DoChan(flightKey, func() (*D.Msg, error) { return new(D.Msg), nil })
			select {
			case <-barrier:
			case <-time.After(time.Second):
				t.Fatal("manual close resurrected a blocked DNS retry")
			}
			if pending() != 0 {
				t.Fatal("manual close contacted the upstream again")
			}
			if statistic.DefaultManager.Get(tracker.ID()) != nil {
				t.Fatal("manually closed query remained visible")
			}
			unblock()
			// Terminal cancellation belongs only to the old logical request.
			if _, err := r.ExchangeContext(ctx, query); err != nil {
				t.Fatalf("new request inherited the old terminal state: %v", err)
			}
			seen()
		})
	}
}

type nativeLateDialOutbound struct {
	*encryptedRoutingOutbound
	started, canceled, release chan struct{}
	once                       sync.Once
}

func (a *nativeLateDialOutbound) gate(ctx context.Context) {
	a.once.Do(func() {
		close(a.started)
		<-ctx.Done()
		close(a.canceled)
		<-a.release
	})
}

func (a *nativeLateDialOutbound) DialContext(ctx context.Context, md *C.Metadata) (C.Conn, error) {
	a.gate(ctx)
	// Deliberately emulate an adapter which completes a dial after cancellation.
	return a.encryptedRoutingOutbound.DialContext(context.Background(), md)
}

func (a *nativeLateDialOutbound) ListenPacketContext(ctx context.Context, md *C.Metadata) (C.PacketConn, error) {
	a.gate(ctx)
	return a.encryptedRoutingOutbound.ListenPacketContext(context.Background(), md)
}

func TestDNSRuleRoutingNativeCloseDuringEncryptedConstruction(t *testing.T) {
	for _, protocol := range []string{"h2", "h3", "tls", "quic"} {
		t.Run(protocol, func(t *testing.T) {
			routingTestEnable(t)
			server := newEncryptedRoutingServer(t, protocol, true, nil)
			leaf := &nativeLateDialOutbound{encryptedRoutingOutbound: newEncryptedRoutingOutbound("late-" + protocol),
				started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			unblock := func() { release.Do(func() { close(leaf.release) }) }
			t.Cleanup(unblock)
			r := NewResolver(Config{RuleRouting: true, Main: []NameServer{server.ns}}).Resolver
			t.Cleanup(func() { unblock(); r.Close() })
			result := make(chan error, 1)
			go func() {
				_, err := encryptedRoutingRawExchange(r, routingTestContext(leaf, routingTestOrigin()), routingTestQuery("late.example", 81))
				result <- err
			}()
			select {
			case <-leaf.started:
			case <-time.After(time.Second):
				t.Fatal("encrypted construction did not start")
			}
			state, _, _, _ := nativeClientDetails(r.main[0])
			state.mu.Lock()
			pool := state.pool
			state.mu.Unlock()
			pool.mu.Lock()
			var scoped dnsClient
			for _, entry := range pool.entries {
				scoped = entry.client
			}
			pool.mu.Unlock()
			closed := make(chan struct{})
			go func() { r.Close(); close(closed) }()
			select {
			case <-leaf.canceled:
			case <-time.After(time.Second):
				t.Fatal("lifecycle close did not cancel the unfinished construction")
			}
			unblock()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("lifecycle close retained an unfinished encrypted client")
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("late construction completed after shutdown")
				}
			case <-time.After(time.Second):
				t.Fatal("late construction did not unwind")
			}
			calls := leaf.count()
			if _, err := scoped.ExchangeContext(context.Background(), routingTestQuery("closed.example", 82)); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("closed scoped transport was reusable: %v", err)
			}
			if leaf.count() != calls {
				t.Fatal("closed client dialed again")
			}
		})
	}
}
