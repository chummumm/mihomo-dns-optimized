package dnsproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/component/auth"
	C "github.com/metacubex/mihomo/constant"
	authStore "github.com/metacubex/mihomo/listener/auth"
	"github.com/metacubex/mihomo/transport/socks5"

	"github.com/miekg/dns"
)

type recordedQuery struct {
	name     string
	metadata C.Metadata
}

type recordingExchanger struct {
	mu      sync.Mutex
	queries []recordedQuery
	hook    func(context.Context) error
}

func (r *recordingExchanger) ExchangeDNS(ctx context.Context, query []byte, metadata *C.Metadata) ([]byte, error) {
	var msg dns.Msg
	if err := msg.Unpack(query); err != nil || len(msg.Question) != 1 {
		return nil, fmt.Errorf("invalid DNS query")
	}
	r.mu.Lock()
	r.queries = append(r.queries, recordedQuery{msg.Question[0].Name, *metadata})
	r.mu.Unlock()
	if r.hook != nil {
		if err := r.hook(ctx); err != nil {
			return nil, err
		}
	}
	response := new(dns.Msg).SetReply(&msg)
	return response.Pack()
}

func (r *recordingExchanger) snapshot() []recordedQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedQuery(nil), r.queries...)
}

func testListener(t *testing.T, r C.DNSExchanger, users ...auth.AuthUser) *Listener {
	t.Helper()
	l, err := New("127.0.0.1:0", inbound.NewListenConfig(), authStore.NewAuthStore(auth.NewAuthenticator(users)), r, inbound.WithInName("smartdns"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func testConn(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// Read the reply code explicitly: the upstream ClientHandshake helper returns
// the bound address without checking REP, which could conceal a false success.
func socksRequest(t *testing.T, conn net.Conn, target string, command byte, user *socks5.User) (byte, socks5.Addr) {
	t.Helper()
	method := byte(0)
	if user != nil {
		method = 2
	}
	if _, err := conn.Write([]byte{5, 1, method}); err != nil {
		t.Fatal(err)
	}
	var negotiation [2]byte
	if _, err := io.ReadFull(conn, negotiation[:]); err != nil {
		t.Fatal(err)
	}
	if negotiation != [2]byte{5, method} {
		t.Fatalf("unexpected auth negotiation: %v", negotiation)
	}
	if user != nil {
		request := []byte{1, byte(len(user.Username))}
		request = append(request, user.Username...)
		request = append(request, byte(len(user.Password)))
		request = append(request, user.Password...)
		if _, err := conn.Write(request); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(conn, negotiation[:]); err != nil {
			t.Fatal(err)
		}
		if negotiation != [2]byte{1, 0} {
			return negotiation[1], nil
		}
	}
	request := append([]byte{5, command, 0}, socks5.ParseAddr(target)...)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	var reply [3]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	addr, err := socks5.ReadAddr(conn, make([]byte, socks5.MaxAddrLen))
	if err != nil {
		t.Fatal(err)
	}
	return reply[1], addr
}

func dnsQuery(t *testing.T, name string, id uint16) []byte {
	t.Helper()
	query := new(dns.Msg).SetQuestion(name, dns.TypeA)
	query.Id = id
	wire, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func dnsFrame(query []byte) []byte {
	frame := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(frame, uint16(len(query)))
	copy(frame[2:], query)
	return frame
}

func readDNSFrame(t *testing.T, reader io.Reader, name string, id uint16) {
	t.Helper()
	var size [2]byte
	if _, err := io.ReadFull(reader, size[:]); err != nil {
		t.Fatal(err)
	}
	wire := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(reader, wire); err != nil {
		t.Fatal(err)
	}
	checkResponse(t, wire, name, id)
}

func checkResponse(t *testing.T, wire []byte, name string, id uint16) {
	t.Helper()
	var response dns.Msg
	if err := response.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	if !response.Response || response.Id != id || len(response.Question) != 1 || response.Question[0].Name != name {
		t.Fatalf("unexpected DNS response: %v", &response)
	}
}

func TestSOCKSTCPPersistentConnectionExchangesEveryMessage(t *testing.T) {
	r := new(recordingExchanger)
	l := testListener(t, r, auth.AuthUser{User: "smartdns", Pass: "test-password"})
	conn := testConn(t, l.Address())
	code, _ := socksRequest(t, conn, "8.8.8.8:53", socks5.CmdConnect, &socks5.User{Username: "smartdns", Password: "test-password"})
	if code != 0 {
		t.Fatalf("SOCKS CONNECT rejected: %d", code)
	}
	// Send both frames together, as on a persistent SmartDNS TCP connection.
	frames := append(dnsFrame(dnsQuery(t, "claude.ai.", 1)), dnsFrame(dnsQuery(t, "example.cn.", 2))...)
	if _, err := conn.Write(frames); err != nil {
		t.Fatal(err)
	}
	readDNSFrame(t, conn, "claude.ai.", 1)
	readDNSFrame(t, conn, "example.cn.", 2)
	queries := r.snapshot()
	if len(queries) != 2 || queries[0].name != "claude.ai." || queries[1].name != "example.cn." {
		t.Fatalf("queries were not independently exchanged: %+v", queries)
	}
	for _, query := range queries {
		m := query.metadata
		if m.DstIP != netip.MustParseAddr("8.8.8.8") || m.DstPort != 53 || m.Host != "" || m.NetWork != C.TCP || m.InName != "smartdns" || m.InUser != "smartdns" {
			t.Fatalf("incorrect resolver metadata: %+v", m)
		}
	}
}

func TestSOCKSRejectsForbiddenTargetsBeforeSuccess(t *testing.T) {
	r := new(recordingExchanger)
	l := testListener(t, r)
	for _, test := range []struct {
		target string
		code   byte
	}{
		{"8.8.8.8:443", byte(socks5.ErrConnectionNotAllowed)},
		{"8.8.8.8:853", byte(socks5.ErrConnectionNotAllowed)},
		{"dns.google:53", byte(socks5.ErrAddressNotSupported)},
		{"0.0.0.0:53", byte(socks5.ErrAddressNotSupported)},
		{"224.0.0.1:53", byte(socks5.ErrAddressNotSupported)},
		{"255.255.255.255:53", byte(socks5.ErrAddressNotSupported)},
	} {
		t.Run(test.target, func(t *testing.T) {
			conn := testConn(t, l.Address())
			code, _ := socksRequest(t, conn, test.target, socks5.CmdConnect, nil)
			if code != test.code {
				t.Fatalf("first request reply was %d, want rejection %d", code, test.code)
			}
		})
	}
	if len(r.snapshot()) != 0 {
		t.Fatal("forbidden target reached the exchanger")
	}
}

func TestSOCKSAuthenticationRequired(t *testing.T) {
	r := new(recordingExchanger)
	l := testListener(t, r, auth.AuthUser{User: "smartdns", Pass: "correct"})
	conn := testConn(t, l.Address())
	code, addr := socksRequest(t, conn, "8.8.8.8:53", socks5.CmdConnect, &socks5.User{Username: "smartdns", Password: "wrong"})
	if code == 0 || addr != nil || len(r.snapshot()) != 0 {
		t.Fatal("invalid authentication was accepted")
	}
}

func TestHTTPConnectPreservesBufferedDNSAndAuthentication(t *testing.T) {
	r := new(recordingExchanger)
	l := testListener(t, r, auth.AuthUser{User: "smartdns", Pass: "test-password"})
	conn := testConn(t, l.Address())
	credentials := base64.StdEncoding.EncodeToString([]byte("smartdns:test-password"))
	header := fmt.Sprintf("CONNECT [2001:4860:4860::8888]:53 HTTP/1.1\r\nHost: [2001:4860:4860::8888]:53\r\nProxy-Authorization: Basic %s\r\n\r\n", credentials)
	request := append([]byte(header), dnsFrame(dnsQuery(t, "claude.com.", 3))...)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT failed: %v, %v", response, err)
	}
	readDNSFrame(t, reader, "claude.com.", 3)
	if _, err := conn.Write(dnsFrame(dnsQuery(t, "example.cn.", 4))); err != nil {
		t.Fatal(err)
	}
	readDNSFrame(t, reader, "example.cn.", 4)
	queries := r.snapshot()
	if len(queries) != 2 || queries[0].metadata.DstIP != netip.MustParseAddr("2001:4860:4860::8888") || queries[0].metadata.InUser != "smartdns" || queries[0].metadata.Type != C.HTTPS {
		t.Fatalf("incorrect HTTP DNS metadata: %+v", queries)
	}
}

func TestHTTPRejectsNonDNSAndUnauthenticatedRequests(t *testing.T) {
	for _, test := range []struct {
		name    string
		request string
		auth    bool
		status  int
	}{
		{"plain-http", "GET http://8.8.8.8:53/ HTTP/1.1\r\nHost: 8.8.8.8:53\r\n\r\n", false, 405},
		{"https-port", "CONNECT 8.8.8.8:443 HTTP/1.1\r\nHost: 8.8.8.8:443\r\n\r\n", false, 403},
		{"hostname", "CONNECT dns.google:53 HTTP/1.1\r\nHost: dns.google:53\r\n\r\n", false, 403},
		{"missing-auth", "CONNECT 8.8.8.8:53 HTTP/1.1\r\nHost: 8.8.8.8:53\r\n\r\n", true, 407},
		{"body", "CONNECT 8.8.8.8:53 HTTP/1.1\r\nHost: 8.8.8.8:53\r\nContent-Length: 10\r\n\r\n", false, 405},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := new(recordingExchanger)
			var users []auth.AuthUser
			if test.auth {
				users = []auth.AuthUser{{User: "smartdns", Pass: "correct"}}
			}
			l := testListener(t, r, users...)
			conn := testConn(t, l.Address())
			_, _ = io.WriteString(conn, test.request)
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil || response.StatusCode != test.status {
				t.Fatalf("unexpected response: %v, %v", response, err)
			}
			if len(r.snapshot()) != 0 {
				t.Fatal("rejected HTTP request reached exchanger")
			}
		})
	}
}

func udpClient(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func sendUDP(t *testing.T, conn *net.UDPConn, relay *net.UDPAddr, target string, name string, id uint16, fragment byte) {
	t.Helper()
	packet, err := socks5.EncodeUDPPacket(socks5.ParseAddr(target), dnsQuery(t, name, id))
	if err != nil {
		t.Fatal(err)
	}
	packet[2] = fragment
	if _, err := conn.WriteToUDP(packet, relay); err != nil {
		t.Fatal(err)
	}
}

func receiveUDP(t *testing.T, conn *net.UDPConn, name string, id uint16) {
	t.Helper()
	packet := make([]byte, 65536)
	n, _, err := conn.ReadFromUDP(packet)
	if err != nil {
		t.Fatal(err)
	}
	target, response, err := socks5.DecodeUDPPacket(packet[:n])
	if err != nil || target.String() != "8.8.8.8:53" {
		t.Fatalf("unexpected UDP envelope: %v, %v", target, err)
	}
	checkResponse(t, response, name, id)
}

func TestSOCKSUDPAssociationExchangesEveryQueryAndDropsForbiddenPackets(t *testing.T) {
	r := new(recordingExchanger)
	l := testListener(t, r, auth.AuthUser{User: "smartdns", Pass: "test-password"})
	control := testConn(t, l.Address())
	code, address := socksRequest(t, control, "0.0.0.0:0", socks5.CmdUDPAssociate, &socks5.User{Username: "smartdns", Password: "test-password"})
	if code != 0 || address.UDPAddr() == nil {
		t.Fatalf("UDP association failed: %d, %v", code, address)
	}
	relay := address.UDPAddr()
	client := udpClient(t)
	sendUDP(t, client, relay, "8.8.8.8:53", "claude.ai.", 10, 0)
	receiveUDP(t, client, "claude.ai.", 10)
	sendUDP(t, client, relay, "8.8.8.8:53", "example.cn.", 11, 0)
	receiveUDP(t, client, "example.cn.", 11)
	// Neither a different destination port nor SOCKS fragmentation is allowed.
	sendUDP(t, client, relay, "8.8.8.8:443", "forbidden.example.", 12, 0)
	sendUDP(t, client, relay, "8.8.8.8:53", "fragment.example.", 13, 1)
	otherClient := udpClient(t)
	sendUDP(t, otherClient, relay, "8.8.8.8:53", "wrong-source.example.", 14, 0)
	sendUDP(t, client, relay, "8.8.8.8:53", "claude.com.", 15, 0)
	receiveUDP(t, client, "claude.com.", 15)
	queries := r.snapshot()
	if len(queries) != 3 || queries[0].name != "claude.ai." || queries[1].name != "example.cn." || queries[2].name != "claude.com." {
		t.Fatalf("incorrect per-message exchanges: %+v", queries)
	}
	for _, query := range queries {
		if query.metadata.NetWork != C.UDP || query.metadata.InUser != "smartdns" || query.metadata.DstPort != 53 || query.metadata.SrcPort != uint16(client.LocalAddr().(*net.UDPAddr).Port) {
			t.Fatalf("incorrect UDP metadata: %+v", query.metadata)
		}
	}
}

func TestUDPAssociationCloseCancelsOutstandingExchange(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	r := &recordingExchanger{hook: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	l := testListener(t, r)
	control := testConn(t, l.Address())
	code, address := socksRequest(t, control, "0.0.0.0:0", socks5.CmdUDPAssociate, nil)
	if code != 0 {
		t.Fatal("UDP association failed")
	}
	client := udpClient(t)
	sendUDP(t, client, address.UDPAddr(), "8.8.8.8:53", "cancel.example.", 20, 0)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("exchange did not start")
	}
	_ = control.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("closing the control channel did not cancel the association")
	}
}

func TestSmartDNSUDPAssociateUnspecifiedAddressWithResolverPort(t *testing.T) {
	for _, endpoint := range []string{"0.0.0.0:53", "[::]:53"} {
		t.Run(endpoint, func(t *testing.T) {
			r := new(recordingExchanger)
			l := testListener(t, r)
			control := testConn(t, l.Address())
			code, address := socksRequest(t, control, endpoint, socks5.CmdUDPAssociate, nil)
			if code != 0 {
				t.Fatalf("SmartDNS-style UDP association rejected: %d", code)
			}
			client := udpClient(t)
			if client.LocalAddr().(*net.UDPAddr).Port == 53 {
				t.Fatal("test must use an ephemeral client source port")
			}
			sendUDP(t, client, address.UDPAddr(), "8.8.8.8:53", "claude.ai.", 30, 0)
			receiveUDP(t, client, "claude.ai.", 30)
			// The first accepted source is still pinned after accommodating
			// SmartDNS's unspecified address; another socket cannot use it.
			other := udpClient(t)
			sendUDP(t, other, address.UDPAddr(), "8.8.8.8:53", "wrong-source.example.", 31, 0)
			sendUDP(t, client, address.UDPAddr(), "8.8.8.8:53", "example.cn.", 32, 0)
			receiveUDP(t, client, "example.cn.", 32)
			queries := r.snapshot()
			if len(queries) != 2 || queries[0].name != "claude.ai." || queries[1].name != "example.cn." {
				t.Fatalf("incorrect source-port pinning: %+v", queries)
			}
		})
	}
}

func TestMalformedTCPFrameIsDropped(t *testing.T) {
	r := new(recordingExchanger)
	l := testListener(t, r)
	conn := testConn(t, l.Address())
	code, _ := socksRequest(t, conn, "8.8.8.8:53", socks5.CmdConnect, nil)
	if code != 0 {
		t.Fatal("CONNECT failed")
	}
	_, _ = conn.Write([]byte{0, 3, 1, 2, 3})
	var b [1]byte
	if _, err := conn.Read(b[:]); err == nil {
		t.Fatal("malformed DNS frame was not dropped")
	}
	if len(r.snapshot()) != 0 {
		t.Fatal("short DNS message reached exchanger")
	}
}

func TestCloseClosesClientConnections(t *testing.T) {
	l := testListener(t, new(recordingExchanger))
	conn := testConn(t, l.Address())
	// Complete negotiation to ensure the accepted socket is being handled.
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(reply[:]); err == nil {
		t.Fatal("client remained open after listener closed")
	}
}
