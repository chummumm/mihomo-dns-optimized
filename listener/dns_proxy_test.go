package listener_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter/inbound"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/listener"
	authStore "github.com/metacubex/mihomo/listener/auth"
	"github.com/metacubex/mihomo/transport/socks5"

	"github.com/miekg/dns"
)

type dnsProxyTestTunnel struct{}

func (dnsProxyTestTunnel) HandleTCPConn(conn net.Conn, _ *C.Metadata)        { _ = conn.Close() }
func (dnsProxyTestTunnel) HandleUDPPacket(packet C.UDPPacket, _ *C.Metadata) { packet.Drop() }
func (dnsProxyTestTunnel) NatTable() C.NatTable                              { return nil }
func (dnsProxyTestTunnel) ExchangeDNS(_ context.Context, query []byte, _ *C.Metadata) ([]byte, error) {
	request := new(dns.Msg)
	if err := request.Unpack(query); err != nil {
		return nil, err
	}
	return new(dns.Msg).SetReply(request).Pack()
}

func dnsProxyTestGlobals(t *testing.T) {
	t.Helper()
	oldAllowLan, oldBind := listener.AllowLan(), listener.BindAddress()
	oldAuth := authStore.Default.Authenticator()
	oldAllowed, oldDisallowed, oldSkip := inbound.AllowedIPs(), inbound.DisAllowedIPs(), inbound.SkipAuthPrefixes()
	listener.ReCreateDNSProxy(0, nil)
	listener.SetAllowLan(false)
	listener.SetBindAddress("*")
	authStore.Default.SetAuthenticator(nil)
	inbound.SetAllowedIPs([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	inbound.SetDisAllowedIPs(nil)
	inbound.SetSkipAuthPrefixes(nil)
	t.Cleanup(func() {
		listener.ReCreateDNSProxy(0, nil)
		listener.SetAllowLan(oldAllowLan)
		listener.SetBindAddress(oldBind)
		authStore.Default.SetAuthenticator(oldAuth)
		inbound.SetAllowedIPs(oldAllowed)
		inbound.SetDisAllowedIPs(oldDisallowed)
		inbound.SetSkipAuthPrefixes(oldSkip)
	})
}

func dnsProxyFreePort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 10; attempt++ {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := tcp.Addr().(*net.TCPAddr).Port
		udp, err := net.ListenPacket("udp", tcp.Addr().String())
		_ = tcp.Close()
		if err == nil {
			_ = udp.Close()
			return port
		}
	}
	t.Fatal("unable to reserve an available TCP/UDP port")
	return 0
}

func dnsProxyDial(t *testing.T, host string, port int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func dnsProxyAssertReleased(t *testing.T, host string, port int) {
	t.Helper()
	address := net.JoinHostPort(host, strconv.Itoa(port))
	tcp, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("TCP socket was not released: %v", err)
	}
	_ = tcp.Close()
	udp, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatalf("UDP socket was not released: %v", err)
	}
	_ = udp.Close()
}

func TestDNSProxyPortLifecycle(t *testing.T) {
	dnsProxyTestGlobals(t)
	// Port zero requires no DNS exchanger and opens neither transport.
	listener.ReCreateDNSProxy(0, nil)
	if port := listener.GetPorts().DNSProxyPort; port != 0 {
		t.Fatalf("disabled port=%d", port)
	}
	first, second := dnsProxyFreePort(t), dnsProxyFreePort(t)
	for second == first {
		second = dnsProxyFreePort(t)
	}
	listener.ReCreateDNSProxy(first, dnsProxyTestTunnel{})
	if port := listener.GetPorts().DNSProxyPort; port != first {
		t.Fatalf("port=%d, want %d", port, first)
	}
	udp, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", first))
	if err == nil {
		_ = udp.Close()
		t.Fatal("DNS proxy did not reserve UDP on the TCP port")
	}
	conn := dnsProxyDial(t, "127.0.0.1", first)
	_, _ = conn.Write([]byte{5, 1, 0})
	var reply [3]byte
	if _, err := io.ReadFull(conn, reply[:2]); err != nil {
		t.Fatal(err)
	}
	// Same port/binding is a no-op, even with no replacement tunnel supplied.
	listener.ReCreateDNSProxy(first, nil)
	request := append([]byte{5, 1, 0}, socks5.ParseAddr("8.8.8.8:53")...)
	_, _ = conn.Write(request)
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[1] != 0 {
		t.Fatalf("unchanged port restarted the active session: %v, %v", reply, err)
	}
	if _, err := socks5.ReadAddr(conn, make([]byte, socks5.MaxAddrLen)); err != nil {
		t.Fatal(err)
	}
	listener.ReCreateDNSProxy(second, dnsProxyTestTunnel{})
	if port := listener.GetPorts().DNSProxyPort; port != second {
		t.Fatalf("port did not change: %d", port)
	}
	if _, err := conn.Read(reply[:]); err == nil {
		t.Fatal("port change left an old client session open")
	}
	dnsProxyAssertReleased(t, "127.0.0.1", first)
	listener.ReCreateDNSProxy(0, nil)
	if port := listener.GetPorts().DNSProxyPort; port != 0 {
		t.Fatalf("zero did not disable port: %d", port)
	}
	dnsProxyAssertReleased(t, "127.0.0.1", second)
}

func TestDNSProxyPortBindingAndCleanup(t *testing.T) {
	dnsProxyTestGlobals(t)
	port := dnsProxyFreePort(t)
	listener.SetBindAddress("127.0.0.2")
	listener.ReCreateDNSProxy(port, dnsProxyTestTunnel{})
	conn := dnsProxyDial(t, "127.0.0.1", port)
	_ = conn.Close()
	// allow-lan=false overrides bind-address exactly as mixed-port does.
	dnsProxyAssertReleased(t, "127.0.0.2", port)
	listener.SetAllowLan(true)
	listener.ReCreateDNSProxy(port, dnsProxyTestTunnel{})
	conn = dnsProxyDial(t, "127.0.0.2", port)
	_ = conn.Close()
	dnsProxyAssertReleased(t, "127.0.0.1", port)
	listener.Cleanup()
	if port := listener.GetPorts().DNSProxyPort; port != 0 {
		t.Fatalf("cleanup retained port %d", port)
	}
	dnsProxyAssertReleased(t, "127.0.0.2", port)
}

func TestDNSProxyUDPBindFailureDoesNotLeaveTCPListener(t *testing.T) {
	dnsProxyTestGlobals(t)
	port := dnsProxyFreePort(t)
	address := fmt.Sprintf("127.0.0.1:%d", port)
	udp, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	listener.ReCreateDNSProxy(port, dnsProxyTestTunnel{})
	if port := listener.GetPorts().DNSProxyPort; port != 0 {
		t.Fatalf("failed listener reported active port %d", port)
	}
	tcp, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("failed UDP bind leaked TCP listener: %v", err)
	}
	_ = tcp.Close()
}

func TestDNSProxyPortIPv6Binding(t *testing.T) {
	dnsProxyTestGlobals(t)
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenPacket("udp6", probe.Addr().String())
	_ = probe.Close()
	if err != nil {
		t.Skipf("IPv6 UDP loopback is unavailable: %v", err)
	}
	_ = udp.Close()
	listener.SetAllowLan(true)
	// Top-level binding uses the same bracketed IPv6 form as mixed-port.
	listener.SetBindAddress("[::1]")
	inbound.SetAllowedIPs([]netip.Prefix{netip.MustParsePrefix("::1/128")})
	listener.ReCreateDNSProxy(port, dnsProxyTestTunnel{})
	if got := listener.GetPorts().DNSProxyPort; got != port {
		t.Fatalf("IPv6 listener port=%d, want %d", got, port)
	}
	conn := dnsProxyDial(t, "::1", port)
	_, _ = conn.Write([]byte{5, 1, 0})
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply != [2]byte{5, 0} {
		t.Fatalf("IPv6 listener handshake: %v, %v", reply, err)
	}
	udp, err = net.ListenPacket("udp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err == nil {
		_ = udp.Close()
		t.Fatal("IPv6 DNS proxy did not bind UDP to the TCP port")
	}
	listener.ReCreateDNSProxy(0, nil)
	dnsProxyAssertReleased(t, "::1", port)
}

func TestDNSProxyOldListenerTypeIsRemoved(t *testing.T) {
	_, err := listener.ParseListener(map[string]any{"type": "dns-proxy", "name": "obsolete", "port": 7853})
	if err == nil {
		t.Fatal("old listeners type:dns-proxy is still accepted")
	}
}
