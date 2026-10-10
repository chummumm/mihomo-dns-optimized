package dns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/ca"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel/statistic"
	"github.com/metacubex/quic-go"
	"github.com/metacubex/quic-go/http3"
	"github.com/metacubex/tls"
	D "github.com/miekg/dns"
)

// The adapter deliberately uses real loopback sockets. These tests therefore
// exercise TLS verification, HTTP multiplexing and QUIC framing, rather than
// returning canned answers from a fake encrypted transport.
type encryptedRoutingOutbound struct {
	*routingTestBase
	mu    sync.Mutex
	calls []*C.Metadata
}

func newEncryptedRoutingOutbound(name string) *encryptedRoutingOutbound {
	return &encryptedRoutingOutbound{routingTestBase: &routingTestBase{name: name, kind: C.Socks5}}
}

func (a *encryptedRoutingOutbound) record(md *C.Metadata) error {
	if !md.DstIP.IsLoopback() || md.DstPort == 0 {
		return fmt.Errorf("test forbids a non-loopback destination: %s", md.RemoteAddress())
	}
	a.mu.Lock()
	a.calls = append(a.calls, md.Clone())
	a.mu.Unlock()
	return nil
}

func (a *encryptedRoutingOutbound) DialContext(ctx context.Context, md *C.Metadata) (C.Conn, error) {
	if err := a.record(md); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", netip.AddrPortFrom(md.DstIP, md.DstPort).String())
	if err != nil {
		return nil, err
	}
	return &routingTestTCPConn{ExtendedConn: N.NewExtendedConn(conn), routingTestConnection: routingTestConnection{chain: C.Chain{a.Name()}}}, nil
}

func (a *encryptedRoutingOutbound) ListenPacketContext(ctx context.Context, md *C.Metadata) (C.PacketConn, error) {
	if err := a.record(md); err != nil {
		return nil, err
	}
	conn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &routingTestUDPConn{EnhancePacketConn: N.NewEnhancePacketConn(conn), routingTestConnection: routingTestConnection{chain: C.Chain{a.Name()}}}, nil
}

func (a *encryptedRoutingOutbound) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *encryptedRoutingOutbound) assertDestination(t *testing.T, address string) {
	t.Helper()
	want := netip.MustParseAddrPort(address)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		t.Fatal("selected leaf never opened the real encrypted transport")
	}
	for _, md := range a.calls {
		if md.DstIP != want.Addr() || md.DstPort != want.Port() || md.Host != "" {
			t.Fatalf("transport dialed the business name instead of the resolver: %+v, want %s", md, want)
		}
	}
}

type encryptedRoutingObservation struct {
	name       string
	wireID     uint16
	connection string
	protocol   string
}

type encryptedRoutingServer struct {
	ns      NameServer
	address string
	seen    chan encryptedRoutingObservation
	gate    func(context.Context, string) bool
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	closers []func()
	wg      sync.WaitGroup
}

func encryptedRoutingCertificate(t *testing.T, trust bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "native DNS loopback test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if trust {
		if err := ca.AddCertificate(string(der)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(ca.ResetCertificate)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (s *encryptedRoutingServer) addCloser(close func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		close()
		return
	}
	s.closers = append(s.closers, close)
}

func (s *encryptedRoutingServer) run(fn func()) {
	s.wg.Add(1)
	go func() { defer s.wg.Done(); fn() }()
}

func (s *encryptedRoutingServer) close() {
	s.cancel()
	s.mu.Lock()
	s.closed = true
	closers := append([]func(){}, s.closers...)
	s.mu.Unlock()
	for _, close := range closers {
		close()
	}
	s.wg.Wait()
}

func (s *encryptedRoutingServer) answer(ctx context.Context, query *D.Msg, connection, protocol string) *D.Msg {
	if len(query.Question) != 1 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	select {
	case s.seen <- encryptedRoutingObservation{query.Question[0].Name, query.Id, connection, protocol}:
	case <-ctx.Done():
		return nil
	}
	if s.gate != nil && !s.gate(ctx, query.Question[0].Name) {
		return nil
	}
	return routingTestAnswer(query, false)
}

func (s *encryptedRoutingServer) handleHTTP(w http.ResponseWriter, req *http.Request) {
	var wire []byte
	var err error
	if req.Method == http.MethodPost {
		wire, err = io.ReadAll(req.Body)
	} else {
		wire, err = base64.RawURLEncoding.DecodeString(req.URL.Query().Get("dns"))
	}
	query := new(D.Msg)
	if err != nil || query.Unpack(wire) != nil || req.URL.Path != "/dns-query" {
		http.Error(w, "invalid DNS request", http.StatusBadRequest)
		return
	}
	answer := s.answer(req.Context(), query, req.RemoteAddr, req.Proto)
	if answer == nil {
		return
	}
	wire, err = answer.Pack()
	if err != nil {
		return
	}
	w.Header().Set("Content-Type", "application/dns-message")
	_, _ = w.Write(wire)
}

func newEncryptedRoutingServer(t *testing.T, protocol string, trust bool, gate func(context.Context, string) bool) *encryptedRoutingServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &encryptedRoutingServer{seen: make(chan encryptedRoutingObservation, 32), gate: gate, ctx: ctx, cancel: cancel}
	cert := encryptedRoutingCertificate(t, trust)
	t.Cleanup(s.close)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	switch protocol {
	case "h2":
		server := httptest.NewUnstartedServer(http.HandlerFunc(s.handleHTTP))
		server.EnableHTTP2 = true
		server.TLS = tlsConfig
		server.StartTLS()
		s.addCloser(server.Close)
		s.address = server.Listener.Addr().String()
		s.ns = NameServer{Net: "https", Addr: server.URL + "/dns-query"}
	case "h3":
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := &http3.Server{TLSConfig: tlsConfig, Handler: http.HandlerFunc(s.handleHTTP)}
		s.addCloser(func() { _ = server.Close(); _ = conn.Close() })
		s.run(func() { _ = server.Serve(conn) })
		s.address = conn.LocalAddr().String()
		s.ns = NameServer{Net: "https", Addr: "https://" + s.address + "/dns-query", Params: map[string]string{"h3": "true"}}
	case "tls":
		listener, err := tls.Listen("tcp4", "127.0.0.1:0", tlsConfig)
		if err != nil {
			t.Fatal(err)
		}
		s.addCloser(func() { _ = listener.Close() })
		s.address = listener.Addr().String()
		s.ns = NameServer{Net: "tls", Addr: s.address}
		s.run(func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				s.addCloser(func() { _ = conn.Close() })
				s.run(func() {
					defer conn.Close()
					wire := &D.Conn{Conn: conn}
					for {
						query, err := wire.ReadMsg()
						if err != nil {
							return
						}
						answer := s.answer(ctx, query, conn.RemoteAddr().String(), "tls")
						if answer == nil || wire.WriteMsg(answer) != nil {
							return
						}
					}
				})
			}
		})
	case "quic":
		tlsConfig.NextProtos = []string{NextProtoDQ}
		listener, err := quic.ListenAddr("127.0.0.1:0", tlsConfig, &quic.Config{})
		if err != nil {
			t.Fatal(err)
		}
		s.addCloser(func() { _ = listener.Close() })
		s.address = listener.Addr().String()
		s.ns = NameServer{Net: "quic", Addr: s.address}
		s.run(func() {
			for {
				conn, err := listener.Accept(ctx)
				if err != nil {
					return
				}
				s.addCloser(func() { _ = conn.CloseWithError(0, "test finished") })
				s.run(func() {
					for {
						stream, err := conn.AcceptStream(ctx)
						if err != nil {
							return
						}
						s.run(func() {
							defer stream.Close()
							var length uint16
							if binary.Read(stream, binary.BigEndian, &length) != nil {
								return
							}
							wire := make([]byte, int(length))
							if _, err := io.ReadFull(stream, wire); err != nil {
								return
							}
							query := new(D.Msg)
							if query.Unpack(wire) != nil {
								return
							}
							answer := s.answer(stream.Context(), query, conn.RemoteAddr().String(), "doq")
							if answer == nil {
								return
							}
							wire, err = answer.Pack()
							if err != nil || binary.Write(stream, binary.BigEndian, uint16(len(wire))) != nil {
								return
							}
							_, _ = stream.Write(wire)
						})
					}
				})
			}
		})
	default:
		t.Fatalf("unknown encrypted test protocol %s", protocol)
	}
	return s
}

func (s *encryptedRoutingServer) next(t *testing.T) encryptedRoutingObservation {
	t.Helper()
	select {
	case observation := <-s.seen:
		return observation
	case <-time.After(3 * time.Second):
		t.Fatal("encrypted server never received a DNS question")
		return encryptedRoutingObservation{}
	}
}

func TestDNSRuleRoutingEncryptedRealTransportReuseAndLeafIsolation(t *testing.T) {
	routingTestEnable(t)
	for _, protocol := range []string{"h2", "h3", "tls", "quic"} {
		t.Run(protocol, func(t *testing.T) {
			server := newEncryptedRoutingServer(t, protocol, true, nil)
			r := NewResolver(Config{RuleRouting: true, Main: []NameServer{server.ns}}).Resolver
			t.Cleanup(r.Close)
			a, b := newEncryptedRoutingOutbound("first-leaf"), newEncryptedRoutingOutbound("second-leaf")
			md := routingTestOrigin()
			first := func(leaf *encryptedRoutingOutbound, name string, id uint16) encryptedRoutingObservation {
				ctx, cancel := context.WithTimeout(routingTestContext(leaf, md), 5*time.Second)
				defer cancel()
				answer := routingTestExchange(t, r, ctx, routingTestQuery(name, id))
				if len(answer.Answer) != 1 || answer.Answer[0].(*D.A).A.String() != "192.0.2.99" {
					t.Fatalf("unexpected real encrypted answer: %v", answer)
				}
				seen := server.next(t)
				if seen.name != D.Fqdn(name) || ((protocol == "h2" || protocol == "h3" || protocol == "quic") && seen.wireID != 0) {
					t.Fatalf("wrong on-wire question: %+v", seen)
				}
				return seen
			}
			one := first(a, "first.example", 41)
			tracked := false
			endpoint := netip.MustParseAddrPort(server.address)
			statistic.DefaultManager.Range(func(tracker statistic.Tracker) bool {
				info := tracker.Info()
				if info.Metadata.InName == "DNS-TRANSPORT" && info.Metadata.AddrPort() == endpoint && info.Chain.Last() == a.Name() {
					tracked = true
					if !info.DNS {
						t.Error("encrypted DNS wire connection is missing its explicit DNS display marker")
					}
					return false
				}
				return true
			})
			if !tracked {
				t.Fatal("encrypted DNS wire connection is absent from the connections API")
			}
			wantProtocol := map[string]string{"h2": "HTTP/2.0", "h3": "HTTP/3.0", "tls": "tls", "quic": "doq"}[protocol]
			if one.protocol != wantProtocol {
				t.Fatalf("encrypted protocol fell back: got %s, want %s", one.protocol, wantProtocol)
			}
			initialDials := a.count()
			md.SrcPort++
			two := first(a, "second.example", 42)
			if one.connection != two.connection || a.count() != initialDials {
				t.Fatal("different QNAME/source port unnecessarily created a new encrypted connection")
			}
			three := first(b, "third.example", 43)
			if three.connection == one.connection {
				t.Fatal("switching the selected leaf reused the old leaf's encrypted connection")
			}
			four := first(a, "fourth.example", 44)
			if four.connection != one.connection || a.count() != initialDials {
				t.Fatal("returning to an existing leaf lost its isolated connection pool")
			}
			a.assertDestination(t, server.address)
			b.assertDestination(t, server.address)
		})
	}
}

func TestDNSRuleRoutingEncryptedSameLeafAcrossGroupsReusesTransportOnly(t *testing.T) {
	routingTestEnable(t)
	for _, protocol := range []string{"h2", "h3", "tls", "quic"} {
		t.Run(protocol, func(t *testing.T) {
			server := newEncryptedRoutingServer(t, protocol, true, nil)
			r := NewResolver(Config{RuleRouting: true, Main: []NameServer{server.ns}}).Resolver
			t.Cleanup(r.Close)
			leaf := newEncryptedRoutingOutbound("shared-leaf")
			root := &routingTestGroup{routingTestBase: &routingTestBase{name: "root", kind: C.Selector}}
			ctx, cancel := context.WithTimeout(routingTestContext(root, routingTestOrigin()), 8*time.Second)
			defer cancel()
			query := routingTestQuery("group-cache.example", 45)
			var first encryptedRoutingObservation
			var initialDials int
			for i, kind := range []C.AdapterType{C.Selector, C.URLTest, C.Fallback, C.LoadBalance} {
				// Each group has a distinct wrapper for the SAME underlying leaf.
				// Group selection is still performed once for every logical query.
				group := &routingTestGroup{routingTestBase: &routingTestBase{name: kind.String(), kind: kind}, leaf: routingTestProxy(leaf)}
				root.leaf = routingTestProxy(group)
				query.Id++
				routingTestExchange(t, r, ctx, query)
				seen := server.next(t)
				if i == 0 {
					first, initialDials = seen, leaf.count()
				} else if seen.connection != first.connection || leaf.count() != initialDials {
					t.Fatalf("%s split a transport already connected through the same leaf", kind)
				}
				if group.choices.Load() != 1 {
					t.Fatal("pool reuse selected a proxy group more than once")
				}
				// The actual request above must reach the server: relaxing the
				// connection identity must not relax the existing answer scope.
				// Repeating that same complete route may still hit its answer cache.
				query.Id++
				routingTestExchange(t, r, ctx, query)
				select {
				case duplicate := <-server.seen:
					t.Fatalf("unchanged route lost its answer cache: %+v", duplicate)
				default:
				}
				if group.choices.Load() != 2 {
					t.Fatal("answer cache skipped the current group selection")
				}
			}
			// A provider may replace a node without changing its display name.
			// A new object must not inherit the old node's connected transport.
			replacement := newEncryptedRoutingOutbound(leaf.Name())
			root.leaf = routingTestProxy(replacement)
			routingTestExchange(t, r, ctx, routingTestQuery("replacement.example", 59))
			if replaced := server.next(t); replaced.connection == first.connection || replacement.count() == 0 {
				t.Fatal("same-name replacement reused the previous node's connection")
			}
		})
	}
}

type encryptedRoutingUDPBlockedGroup struct{ *routingTestGroup }

func (*encryptedRoutingUDPBlockedGroup) SupportUDP() bool { return false }

func TestDNSRuleRoutingEncryptedWarmPoolCannotBypassGroupUDPRestriction(t *testing.T) {
	routingTestEnable(t)
	for _, protocol := range []string{"h3", "quic"} {
		t.Run(protocol, func(t *testing.T) {
			server := newEncryptedRoutingServer(t, protocol, true, nil)
			r := NewResolver(Config{RuleRouting: true, Main: []NameServer{server.ns}}).Resolver
			t.Cleanup(r.Close)
			leaf := newEncryptedRoutingOutbound("shared-leaf")
			allowed := &routingTestGroup{routingTestBase: &routingTestBase{name: "allowed", kind: C.Selector}, leaf: routingTestProxy(leaf)}
			blocked := &encryptedRoutingUDPBlockedGroup{&routingTestGroup{routingTestBase: &routingTestBase{name: "blocked", kind: C.Selector}, leaf: routingTestProxy(leaf)}}
			ctx, cancel := context.WithTimeout(routingTestContext(allowed, routingTestOrigin()), 5*time.Second)
			defer cancel()
			query := routingTestQuery("group-udp.example", 60)
			routingTestExchange(t, r, ctx, query)
			_ = server.next(t)
			dials := leaf.count()
			if _, err := r.ExchangeContext(icontext.WithDNSFixedOutbound(ctx, blocked), query); err == nil {
				t.Fatal("UDP-disabled group borrowed a warm HTTP/3 or QUIC connection")
			}
			if leaf.count() != dials {
				t.Fatal("UDP-disabled group attempted an unauthorized outbound connection")
			}
			select {
			case seen := <-server.seen:
				t.Fatalf("UDP-disabled route reached the DNS server: %+v", seen)
			default:
			}
		})
	}
}

func TestDNSRuleRoutingEncryptedRejectsUntrustedCertificate(t *testing.T) {
	routingTestEnable(t)
	for _, protocol := range []string{"h2", "h3", "tls", "quic"} {
		t.Run(protocol, func(t *testing.T) {
			server := newEncryptedRoutingServer(t, protocol, false, nil)
			leaf := newEncryptedRoutingOutbound("selected-leaf")
			r := NewResolver(Config{RuleRouting: true, Main: []NameServer{server.ns}}).Resolver
			t.Cleanup(r.Close)
			ctx, cancel := context.WithTimeout(routingTestContext(leaf, routingTestOrigin()), 3*time.Second)
			defer cancel()
			if _, err := r.ExchangeContext(ctx, routingTestQuery("untrusted.example", 51)); err == nil {
				t.Fatal("automatic encrypted routing accepted an untrusted TLS certificate")
			}
			leaf.assertDestination(t, server.address)
			select {
			case got := <-server.seen:
				t.Fatalf("sent a DNS question before authenticating the resolver: %+v", got)
			default:
			}
		})
	}
}

func encryptedRoutingRawExchange(r *Resolver, ctx context.Context, query *D.Msg) (*D.Msg, error) {
	ctx, immediate, err := r.prepareDNSRouting(ctx, query)
	if err != nil || immediate != nil {
		return immediate, err
	}
	clients, _ := r.dnsQueryServers(ctx)
	return clients[0].ExchangeContext(ctx, query)
}

func TestDNSRuleRoutingEncryptedCancellationKeepsMultiplexedSibling(t *testing.T) {
	routingTestEnable(t)
	for _, protocol := range []string{"h2", "h3", "quic"} {
		t.Run(protocol, func(t *testing.T) {
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			server := newEncryptedRoutingServer(t, protocol, true, func(ctx context.Context, name string) bool {
				if !strings.HasPrefix(name, "blocked-") {
					return true
				}
				select {
				case <-release:
					return true
				case <-ctx.Done():
					return false
				}
			})
			leaf := newEncryptedRoutingOutbound("shared-leaf")
			r := NewResolver(Config{RuleRouting: true, Main: []NameServer{server.ns}}).Resolver
			t.Cleanup(r.Close)
			firstGroup := &routingTestGroup{routingTestBase: &routingTestBase{name: "cancel-group", kind: C.Selector}, leaf: routingTestProxy(leaf)}
			secondGroup := &routingTestGroup{routingTestBase: &routingTestBase{name: "survive-group", kind: C.Fallback}, leaf: routingTestProxy(leaf)}
			ctx, cancelAll := context.WithTimeout(routingTestContext(firstGroup, routingTestOrigin()), 8*time.Second)
			defer cancelAll()
			siblingCtx := icontext.WithDNSFixedOutbound(ctx, secondGroup)
			if _, err := encryptedRoutingRawExchange(r, ctx, routingTestQuery("warmup.example", 61)); err != nil {
				t.Fatal(err)
			}
			warmup := server.next(t)
			initialDials := leaf.count()
			cancelCtx, cancelOne := context.WithCancel(ctx)
			defer cancelOne()
			canceled, sibling := make(chan error, 1), make(chan error, 1)
			go func() {
				_, err := encryptedRoutingRawExchange(r, cancelCtx, routingTestQuery("blocked-cancel.example", 62))
				canceled <- err
			}()
			go func() {
				answer, err := encryptedRoutingRawExchange(r, siblingCtx, routingTestQuery("blocked-survive.example", 63))
				if err == nil && (answer == nil || answer.Id != 63) {
					err = fmt.Errorf("sibling lost its DNS response ID: %v", answer)
				}
				sibling <- err
			}()
			for i := 0; i < 2; i++ {
				if seen := server.next(t); seen.connection != warmup.connection {
					t.Fatal("concurrent DNS queries did not multiplex over the warm connection")
				}
			}
			cancelOne()
			select {
			case err := <-canceled:
				if err == nil {
					t.Fatal("canceled encrypted query unexpectedly succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("canceling one encrypted query did not release its caller")
			}
			unblock()
			select {
			case err := <-sibling:
				if err != nil {
					t.Fatalf("canceling a query killed a sibling stream: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("sibling query did not finish after its response was released")
			}
			if _, err := encryptedRoutingRawExchange(r, ctx, routingTestQuery("after-cancel.example", 64)); err != nil {
				t.Fatal(err)
			}
			if seen := server.next(t); seen.connection != warmup.connection || leaf.count() != initialDials {
				t.Fatal("canceling a stream unnecessarily replaced the shared encrypted connection")
			}
		})
	}
}
