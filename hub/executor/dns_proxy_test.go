package executor

import (
	"net"
	"testing"

	"github.com/metacubex/mihomo/listener"
)

func TestDNSProxyPortReloadFollowsMixedPortForceSemantics(t *testing.T) {
	original := GetGeneral()
	t.Cleanup(func() { updateListeners(original, nil, true) })
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenPacket("udp", probe.Addr().String())
	_ = probe.Close()
	if err != nil {
		t.Fatal(err)
	}
	_ = udp.Close()

	next := *original
	next.DNSProxyPort = port
	next.AllowLan = false
	next.BindAddress = "*"
	updateListeners(&next, nil, true)
	if got := GetGeneral().DNSProxyPort; got != port {
		t.Fatalf("initial load reported %d, want %d", got, port)
	}
	next.DNSProxyPort = 0
	updateListeners(&next, nil, false)
	if got := listener.GetPorts().DNSProxyPort; got != port {
		t.Fatalf("non-force reload changed inbound port to %d", got)
	}
	updateListeners(&next, nil, true)
	if got := GetGeneral().DNSProxyPort; got != 0 {
		t.Fatalf("force reload did not disable port: %d", got)
	}
}
