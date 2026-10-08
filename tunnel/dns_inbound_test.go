package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"

	"github.com/miekg/dns"
)

func dnsInboundTestState(t *testing.T) {
	t.Helper()
	dnsProxyTestState(t)
	old := DNSRuleRoutingEnabled()
	SetDNSRuleRouting(true)
	t.Cleanup(func() { SetDNSRuleRouting(old) })
}

func dnsInboundFrame(query []byte) []byte {
	frame := make([]byte, len(query)+2)
	binary.BigEndian.PutUint16(frame, uint16(len(query)))
	copy(frame[2:], query)
	return frame
}

func dnsInboundReadFrame(t *testing.T, conn net.Conn) *dns.Msg {
	t.Helper()
	var length [2]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		t.Fatal(err)
	}
	wire := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(conn, wire); err != nil {
		t.Fatal(err)
	}
	response := new(dns.Msg)
	if err := response.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestDNSRoutingInboundEligibility(t *testing.T) {
	dnsInboundTestState(t)
	for _, test := range []struct {
		name string
		edit func(*C.Metadata)
		want bool
	}{
		{"ordinary", func(*C.Metadata) {}, true},
		{"hostname-resolver", func(m *C.Metadata) { m.Host, m.DstIP = "dns.example", netip.Addr{} }, true},
		{"sub-rule", func(m *C.Metadata) { m.SpecialRules = "dns-sub" }, true},
		{"fixed-outbound", func(m *C.Metadata) { m.SpecialProxy = "DIRECT" }, false},
		{"other-port", func(m *C.Metadata) { m.DstPort = 443 }, false},
		{"inner", func(m *C.Metadata) { m.Type = C.INNER }, false},
		{"multicast", func(m *C.Metadata) { m.DstIP = netip.MustParseAddr("224.0.0.251") }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := dnsProxyTestResolver(C.UDP)
			test.edit(metadata)
			if got := dnsInboundCandidate(metadata); got != test.want {
				t.Fatalf("candidate=%v want=%v", got, test.want)
			}
		})
	}
	for _, m := range []TunnelMode{Global, Direct} {
		SetMode(m)
		if dnsInboundCandidate(dnsProxyTestResolver(C.TCP)) {
			t.Fatalf("mode %v was intercepted", m)
		}
	}
	SetMode(Rule)
	SetDNSRuleRouting(false)
	if dnsInboundCandidate(dnsProxyTestResolver(C.TCP)) {
		t.Fatal("disabled feature intercepted traffic")
	}
}

func TestDNSRoutingTCPFirstPayloadFallbackPreservesBytes(t *testing.T) {
	dnsInboundTestState(t)
	invalidQuery := append(dnsProxyTestQuery(t, "example.org."), 0)
	for name, payload := range map[string][]byte{
		"http-on-53":       []byte("GET /example HTTP/1.1\r\nHost: example.org\r\n\r\n"),
		"short-frame":      {0, 3, 1, 2, 3},
		"trailing-data":    dnsInboundFrame(invalidQuery),
		"truncated-length": {0},
	} {
		t.Run(name, func(t *testing.T) {
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			ctx := icontext.NewConnContext(server, dnsProxyTestResolver(C.TCP))
			written := make(chan struct{})
			go func() {
				_, _ = client.Write(payload)
				_ = client.Close()
				close(written)
			}()
			if handleDNSInboundTCP(ctx, func(context.Context, []byte, *C.Metadata) ([]byte, error) {
				t.Error("non-DNS reached exchanger")
				return nil, errors.New("unexpected exchange")
			}) {
				t.Fatal("non-DNS first payload was taken over")
			}
			got, err := io.ReadAll(ctx.Conn())
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("fallback lost stream bytes: %x want %x, err=%v", got, payload, err)
			}
			<-written
		})
	}
}

type dnsInboundShortProbeConn struct {
	net.Conn
	shortened bool
	cleared   bool
}

func (c *dnsInboundShortProbeConn) SetReadDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		c.cleared = true
	} else if !c.shortened {
		c.shortened = true
		deadline = time.Now().Add(20 * time.Millisecond)
	}
	return c.Conn.SetReadDeadline(deadline)
}

func TestDNSRoutingTCPProbeTimeoutReplaysPrefixAndClearsDeadline(t *testing.T) {
	dnsInboundTestState(t)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	wrapped := &dnsInboundShortProbeConn{Conn: server}
	connCtx := icontext.NewConnContext(wrapped, dnsProxyTestResolver(C.TCP))
	firstWritten := make(chan struct{})
	go func() {
		_, _ = client.Write([]byte{0})
		close(firstWritten)
	}()
	if handleDNSInboundTCP(connCtx, nil) {
		t.Fatal("incomplete first prefix must fall back")
	}
	<-firstWritten
	if !wrapped.cleared {
		t.Fatal("probe deadline leaked into ordinary traffic")
	}
	go func() { _, _ = client.Write([]byte("ordinary bytes after timeout")) }()
	want := append([]byte{0}, []byte("ordinary bytes after timeout")...)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(connCtx.Conn(), got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("timed-out probe lost data or kept its read error: %q, %v", got, err)
	}
}

type dnsInboundShortWriterConn struct{ net.Conn }

func (c dnsInboundShortWriterConn) Write(data []byte) (int, error) {
	if len(data) > 3 {
		data = data[:3]
	}
	return c.Conn.Write(data)
}

func TestDNSRoutingTCPMaximumFramePipeliningAndShortWrites(t *testing.T) {
	dnsInboundTestState(t)
	query := new(dns.Msg).SetQuestion("large.example.", dns.TypeHTTPS)
	query.SetEdns0(1232, true)
	base, _ := query.Pack()
	query.IsEdns0().Option = []dns.EDNS0{&dns.EDNS0_PADDING{Padding: make([]byte, 65535-len(base)-4)}}
	large, err := query.Pack()
	if err != nil || len(large) != 65535 {
		t.Fatalf("invalid large fixture: %d %v", len(large), err)
	}
	second := dnsProxyTestQuery(t, "second.example.")
	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	metadata := dnsProxyTestResolver(C.TCP)
	metadata.InName, metadata.SpecialRules = "shared-socks", "dns-sub"
	done := make(chan bool, 1)
	var names []string
	go func() {
		defer server.Close()
		// A transport wrapper that accepts only three bytes per Write must
		// still receive each complete length-prefixed DNS response.
		done <- handleDNSInboundTCP(icontext.NewConnContext(dnsInboundShortWriterConn{server}, metadata), func(_ context.Context, wire []byte, got *C.Metadata) ([]byte, error) {
			msg := new(dns.Msg)
			if err := msg.Unpack(wire); err != nil {
				return nil, err
			}
			names = append(names, msg.Question[0].Name)
			if got.InName != metadata.InName || got.SpecialRules != metadata.SpecialRules || got.SrcIP != metadata.SrcIP || got.Process != metadata.Process || got.DstIP != metadata.DstIP {
				t.Error("original inbound or resolver metadata was lost")
			}
			return new(dns.Msg).SetReply(msg).Pack()
		})
	}()
	frames := append(dnsInboundFrame(large), dnsInboundFrame(second)...)
	written := make(chan error, 1)
	go func() {
		_, err := client.Write(frames[:1]) // split the length prefix itself
		if err == nil {
			_, err = client.Write(frames[1:]) // remaining prefix + coalesced messages
		}
		written <- err
	}()
	for _, want := range []string{"large.example.", "second.example."} {
		if reply := dnsInboundReadFrame(t, client); reply.Question[0].Name != want {
			t.Fatalf("wrong framed response: %v", reply)
		}
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if !<-done || len(names) != 2 || names[0] != "large.example." || names[1] != "second.example." {
		t.Fatalf("queries were not exchanged individually: %v", names)
	}
}

func TestDNSRoutingTCPPolicyChangeStopsNextFrame(t *testing.T) {
	for name, change := range map[string]func(){
		"flag-disabled": func() { SetDNSRuleRouting(false) },
		"global-mode":   func() { SetMode(Global) },
		"direct-mode":   func() { SetMode(Direct) },
	} {
		t.Run(name, func(t *testing.T) {
			dnsInboundTestState(t)
			server, client := net.Pipe()
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(3 * time.Second))
			var calls atomic.Int32
			done := make(chan bool, 1)
			go func() {
				defer server.Close()
				done <- handleDNSInboundTCP(icontext.NewConnContext(server, dnsProxyTestResolver(C.TCP)), func(_ context.Context, wire []byte, _ *C.Metadata) ([]byte, error) {
					calls.Add(1)
					return dnsProxyTestReply(t, wire), nil
				})
			}()
			first := dnsInboundFrame(dnsProxyTestQuery(t, "first.example."))
			if _, err := client.Write(first); err != nil {
				t.Fatal(err)
			}
			_ = dnsInboundReadFrame(t, client)
			change()
			_, _ = client.Write(dnsInboundFrame(dnsProxyTestQuery(t, "must-not-route.example.")))
			var b [1]byte
			if _, err := client.Read(b[:]); err == nil {
				t.Fatal("disabled routing returned another DNS response")
			}
			if !<-done || calls.Load() != 1 {
				t.Fatalf("old connection kept routing after policy change: %d", calls.Load())
			}
		})
	}
}

type dnsInboundPacket struct {
	data    []byte
	peer    net.Addr
	replies chan string
	dropped chan struct{}
	drops   atomic.Int32
}

func (p *dnsInboundPacket) Data() []byte        { return p.data }
func (p *dnsInboundPacket) LocalAddr() net.Addr { return p.peer }
func (p *dnsInboundPacket) Drop() {
	if p.drops.Add(1) == 1 {
		close(p.dropped)
	}
}
func (p *dnsInboundPacket) WriteBack(data []byte, addr net.Addr) (int, error) {
	p.replies <- addr.String()
	return len(data), nil
}

func TestDNSRoutingUDPPerQueryOwnershipAndFallback(t *testing.T) {
	dnsInboundTestState(t)
	for _, name := range []string{"first.example.", "second.example."} {
		packet := &dnsInboundPacket{data: dnsProxyTestQuery(t, name), peer: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 55000}, replies: make(chan string, 1), dropped: make(chan struct{})}
		metadata := dnsProxyTestResolver(C.UDP)
		metadata.Host, metadata.DstIP = "dns.example", netip.Addr{}
		if !handleDNSInboundUDP(C.NewPacketAdapter(packet, metadata), func(_ context.Context, wire []byte, got *C.Metadata) ([]byte, error) {
			if got.Host != "dns.example" || got.SrcIP != metadata.SrcIP || got.InUser != metadata.InUser {
				t.Error("resolver hostname or source metadata changed")
			}
			return dnsProxyTestReply(t, wire), nil
		}) {
			t.Fatal("ordinary DNS was not taken over")
		}
		select {
		case <-packet.dropped:
		case <-time.After(time.Second):
			t.Fatal("DNS packet ownership was not released")
		}
		if packet.drops.Load() != 1 || <-packet.replies != "dns.example:53" {
			t.Fatal("packet was dropped twice or response lost original endpoint")
		}
	}
	packet := &dnsInboundPacket{data: []byte("non DNS"), peer: &net.UDPAddr{}, dropped: make(chan struct{})}
	if handleDNSInboundUDP(C.NewPacketAdapter(packet, dnsProxyTestResolver(C.UDP)), nil) || packet.drops.Load() != 0 || string(packet.data) != "non DNS" {
		t.Fatal("non-DNS datagram was consumed instead of falling through")
	}
}
