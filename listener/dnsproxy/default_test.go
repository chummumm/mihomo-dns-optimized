package dnsproxy

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/component/auth"
	C "github.com/metacubex/mihomo/constant"
	authStore "github.com/metacubex/mihomo/listener/auth"
	"github.com/metacubex/mihomo/transport/socks4"
	"github.com/metacubex/mihomo/transport/socks5"
)

type defaultTestTunnel struct{ *recordingExchanger }

func (*defaultTestTunnel) HandleTCPConn(net.Conn, *C.Metadata) {
	panic("DNS-only listener must not use the ordinary TCP tunnel")
}
func (*defaultTestTunnel) HandleUDPPacket(C.UDPPacket, *C.Metadata) {
	panic("DNS-only listener must not use the ordinary UDP NAT table")
}
func (*defaultTestTunnel) NatTable() C.NatTable { return nil }

func configureDefault(t *testing.T, users []auth.AuthUser, skip, denied []netip.Prefix) {
	t.Helper()
	oldAuth := authStore.Default.Authenticator()
	oldSkip, oldAllowed, oldDenied := inbound.SkipAuthPrefixes(), inbound.AllowedIPs(), inbound.DisAllowedIPs()
	authStore.Default.SetAuthenticator(auth.NewAuthenticator(users))
	inbound.SetSkipAuthPrefixes(skip)
	inbound.SetAllowedIPs([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")})
	inbound.SetDisAllowedIPs(denied)
	t.Cleanup(func() {
		authStore.Default.SetAuthenticator(oldAuth)
		inbound.SetSkipAuthPrefixes(oldSkip)
		inbound.SetAllowedIPs(oldAllowed)
		inbound.SetDisAllowedIPs(oldDenied)
	})
}

func testDefaultListener(t *testing.T, r *recordingExchanger) *Listener {
	t.Helper()
	l, err := NewDefault("127.0.0.1:0", &defaultTestTunnel{r})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if l.udp == nil || l.udp.LocalAddr().String() != l.Address() {
		t.Fatalf("TCP and UDP must share the configured port: TCP=%s UDP=%v", l.Address(), l.udp)
	}
	return l
}

func socks4Request(t *testing.T, conn net.Conn, resolver string, port uint16, user string, socks4a bool) byte {
	t.Helper()
	request := []byte{4, 1, 0, 0, 0, 0, 0, 1}
	binary.BigEndian.PutUint16(request[2:4], port)
	if !socks4a {
		address := netip.MustParseAddr(resolver).As4()
		copy(request[4:8], address[:])
	}
	request = append(request, user...)
	request = append(request, 0)
	if socks4a {
		request = append(request, resolver...)
		request = append(request, 0)
	}
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	var reply [8]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[0] != 0 {
		t.Fatalf("invalid SOCKS4 reply version: %v", reply)
	}
	return reply[1]
}

func TestSOCKS4And4aLiteralResolverMessages(t *testing.T) {
	for _, test := range []struct {
		name, resolver string
		fourA          bool
	}{
		{"socks4", "8.8.8.8", false},
		{"socks4a-ipv4", "8.8.8.8", true},
		{"socks4a-ipv6", "2001:4860:4860::8888", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := new(recordingExchanger)
			l := testListener(t, r, auth.AuthUser{User: "smartdns", Pass: ""})
			conn := testConn(t, l.Address())
			if code := socks4Request(t, conn, test.resolver, 53, "smartdns", test.fourA); code != socks4.RequestGranted {
				t.Fatalf("SOCKS4 request rejected: %d", code)
			}
			frames := append(dnsFrame(dnsQuery(t, "claude.ai.", 41)), dnsFrame(dnsQuery(t, "example.cn.", 42))...)
			if _, err := conn.Write(frames); err != nil {
				t.Fatal(err)
			}
			readDNSFrame(t, conn, "claude.ai.", 41)
			readDNSFrame(t, conn, "example.cn.", 42)
			queries := r.snapshot()
			if len(queries) != 2 || queries[0].metadata.DstIP != netip.MustParseAddr(test.resolver) || queries[0].metadata.Type != C.SOCKS4 || queries[0].metadata.InUser != "smartdns" {
				t.Fatalf("incorrect SOCKS4 per-query metadata: %+v", queries)
			}
		})
	}
}

func TestSOCKS4RejectsForbiddenTargetBeforeSuccess(t *testing.T) {
	for _, test := range []struct {
		name, resolver string
		port           uint16
		fourA          bool
	}{
		{"other-port", "8.8.8.8", 443, false},
		{"4a-hostname", "dns.google", 53, true},
		{"4a-empty-hostname", "", 53, true},
		{"4a-other-port", "8.8.8.8", 853, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := new(recordingExchanger)
			l := testListener(t, r)
			conn := testConn(t, l.Address())
			if code := socks4Request(t, conn, test.resolver, test.port, "", test.fourA); code != socks4.RequestRejected {
				t.Fatalf("first reply must reject forbidden target: %d", code)
			}
			if len(r.snapshot()) != 0 {
				t.Fatal("forbidden target reached DNS exchange")
			}
		})
	}
}

func TestDefaultSamePortUDPExchangesEveryQuery(t *testing.T) {
	configureDefault(t, nil, nil, nil)
	r := new(recordingExchanger)
	l := testDefaultListener(t, r)
	relay, err := net.ResolveUDPAddr("udp", l.Address())
	if err != nil {
		t.Fatal(err)
	}
	client := udpClient(t)
	// No TCP association: this is the ordinary mixed-port UDP wire format.
	sendUDP(t, client, relay, "8.8.8.8:53", "claude.ai.", 51, 0)
	receiveUDP(t, client, "claude.ai.", 51)
	sendUDP(t, client, relay, "8.8.8.8:443", "forbidden.example.", 52, 0)
	sendUDP(t, client, relay, "8.8.8.8:53", "fragment.example.", 53, 1)
	_, _ = client.WriteToUDP([]byte("not a SOCKS DNS packet"), relay)
	sendUDP(t, client, relay, "8.8.8.8:53", "example.cn.", 54, 0)
	receiveUDP(t, client, "example.cn.", 54)
	queries := r.snapshot()
	if len(queries) != 2 || queries[0].name != "claude.ai." || queries[1].name != "example.cn." || queries[0].metadata.InName != "DEFAULT-DNS-PROXY" {
		t.Fatalf("same-port DNS query routing failed: %+v", queries)
	}
}

func expectUDPDrop(t *testing.T, client *net.UDPConn) {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := client.ReadFromUDP(make([]byte, 512)); err == nil {
		t.Fatal("UDP packet should have been dropped")
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
}

func TestDefaultGlobalAuthenticationAndSkipPrefixes(t *testing.T) {
	for _, skip := range []bool{false, true} {
		name := "auth-required"
		var skipPrefixes []netip.Prefix
		if skip {
			name = "skip-auth-prefix"
			skipPrefixes = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
		}
		t.Run(name, func(t *testing.T) {
			configureDefault(t, []auth.AuthUser{{User: "smartdns", Pass: "test-password"}}, skipPrefixes, nil)
			r := new(recordingExchanger)
			l := testDefaultListener(t, r)
			relay, _ := net.ResolveUDPAddr("udp", l.Address())
			client := udpClient(t)
			sendUDP(t, client, relay, "8.8.8.8:53", "direct-udp.example.", 61, 0)
			if skip {
				receiveUDP(t, client, "direct-udp.example.", 61)
			} else {
				expectUDPDrop(t, client)
				if len(r.snapshot()) != 0 {
					t.Fatal("same-port UDP bypassed global authentication")
				}
			}
			control := testConn(t, l.Address())
			var credentials *socks5.User
			if !skip {
				credentials = &socks5.User{Username: "smartdns", Password: "test-password"}
			}
			code, associatedRelay := socksRequest(t, control, "0.0.0.0:53", socks5.CmdUDPAssociate, credentials)
			if code != 0 {
				t.Fatalf("global-auth / skip-auth association failed: %d", code)
			}
			sendUDP(t, client, associatedRelay.UDPAddr(), "8.8.8.8:53", "associated.example.", 62, 0)
			receiveUDP(t, client, "associated.example.", 62)
			if !skip {
				// SOCKS4 cannot carry a password: its USERID-only behavior must
				// not silently bypass an ordinary username/password account.
				conn := testConn(t, l.Address())
				if code := socks4Request(t, conn, "8.8.8.8", 53, "smartdns", false); code != socks4.RequestIdentdMismatched {
					t.Fatalf("SOCKS4 bypassed global password auth: %d", code)
				}
			}
		})
	}
}

func TestDefaultLANAccessControlsCoverTCPAndUDP(t *testing.T) {
	configureDefault(t, nil, nil, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	r := new(recordingExchanger)
	l := testDefaultListener(t, r)
	conn := testConn(t, l.Address())
	_, _ = conn.Write([]byte{5, 1, 0})
	var reply [2]byte
	if _, err := conn.Read(reply[:]); err == nil {
		t.Fatal("denied TCP client reached authentication")
	}
	client := udpClient(t)
	relay, _ := net.ResolveUDPAddr("udp", l.Address())
	sendUDP(t, client, relay, "8.8.8.8:53", "denied.example.", 71, 0)
	expectUDPDrop(t, client)
	if len(r.snapshot()) != 0 {
		t.Fatal("LAN-denied client reached DNS exchange")
	}
}

func TestDefaultClosesTCPIfSamePortUDPBindFails(t *testing.T) {
	udp := udpClient(t)
	addr := udp.LocalAddr().String()
	l, err := NewDefault(addr, &defaultTestTunnel{new(recordingExchanger)})
	if l != nil {
		_ = l.Close()
	}
	if err == nil {
		t.Fatal("UDP port conflict should fail combined listener creation")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("TCP listener leaked after same-port UDP bind failure")
	}
}
