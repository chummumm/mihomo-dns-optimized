package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/component/trie"
	C "github.com/metacubex/mihomo/constant"
)

type directUDPTestMapper struct {
	resolver.Enhancer
	hosts map[netip.Addr]string
}

func (m *directUDPTestMapper) MappingEnabled() bool     { return true }
func (m *directUDPTestMapper) IsFakeIP(netip.Addr) bool { return false }
func (m *directUDPTestMapper) FindHostByIP(ip netip.Addr) (string, bool) {
	host, ok := m.hosts[ip]
	return host, ok
}

type directUDPTestPacket struct {
	C.UDPPacket
	dropped bool
}

func (p *directUDPTestPacket) Data() []byte { return []byte("UDP payload") }
func (p *directUDPTestPacket) Drop()        { p.dropped = true }
func (p *directUDPTestPacket) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 5000}
}

type directUDPTestPacketConn struct {
	C.PacketConn
	resolve func(context.Context, *C.Metadata) error
	calls   int
	written []netip.AddrPort
}

func (c *directUDPTestPacketConn) ResolveUDP(ctx context.Context, metadata *C.Metadata) error {
	c.calls++
	return c.resolve(ctx, metadata)
}
func (c *directUDPTestPacketConn) WriteTo(payload []byte, address net.Addr) (int, error) {
	c.written = append(c.written, address.(*net.UDPAddr).AddrPort())
	return len(payload), nil
}
func (c *directUDPTestPacketConn) SetReadDeadline(time.Time) error { return nil }

func TestDirectDestinationUDPNewAddressKeepsClientIPAcrossHostsMapping(t *testing.T) {
	oldHosts, oldMapper := resolver.DefaultHosts, resolver.DefaultHostMapper
	t.Cleanup(func() {
		resolver.DefaultHosts, resolver.DefaultHostMapper = oldHosts, oldMapper
	})
	firstIP := netip.MustParseAddr("192.0.2.10")
	secondIP := netip.MustParseAddr("192.0.2.11")
	hostsIP := netip.MustParseAddr("203.0.113.55")
	hosts := trie.New[resolver.HostValue]()
	if err := hosts.Insert("rewritten.example", resolver.HostValue{IPs: []netip.Addr{hostsIP}}); err != nil {
		t.Fatal(err)
	}
	resolver.DefaultHosts = resolver.NewHosts(hosts)
	resolver.DefaultHostMapper = &directUDPTestMapper{hosts: map[netip.Addr]string{secondIP: "rewritten.example"}}
	// The actual DIRECT resolver has separate outbound tests. Here the
	// packet-connection boundary supplies its selected address so this test
	// exercises dispatch after Pure and reuse of the resulting NAT mapping.
	for _, test := range []struct {
		name    string
		resolve func(context.Context, *C.Metadata) error
		wantIP  netip.Addr
	}{
		{"DIRECT", func(_ context.Context, metadata *C.Metadata) error {
			if metadata.Host != "" || metadata.DstIP != hostsIP || metadata.InboundDstIP != secondIP {
				return errors.New("adapter did not receive both the hosts address and the original client address")
			}
			metadata.DstIP = secondIP
			return nil
		}, secondIP},
		{"ordinary resolved proxy", func(context.Context, *C.Metadata) error { return nil }, hostsIP},
	} {
		t.Run(test.name, func(t *testing.T) {
			sender := newPacketSender().(*packetSender)
			defer sender.Close()
			pc := &directUDPTestPacketConn{resolve: test.resolve}
			first := &C.Metadata{DstIP: firstIP, DstPort: 4444, NetWork: C.UDP}
			second := &C.Metadata{DstIP: secondIP, DstPort: 5555, NetWork: C.UDP}
			fixMetadata(first)
			fixMetadata(second)
			firstBefore, secondBefore := first.Clone(), second.Clone()
			// A prior datagram has already established this NAT session. A
			// different destination now arrives on that same source socket.
			sender.AddMapping(first, first)
			for _, metadata := range []*C.Metadata{first, second, second, first} {
				packet := &directUDPTestPacket{}
				sender.processPacket(pc, C.NewPacketAdapter(packet, metadata))
				if !packet.dropped {
					t.Fatal("processed packet was not released")
				}
			}
			want := []netip.AddrPort{
				netip.AddrPortFrom(firstIP, 4444), netip.AddrPortFrom(test.wantIP, 5555),
				netip.AddrPortFrom(test.wantIP, 5555), netip.AddrPortFrom(firstIP, 4444),
			}
			if !reflect.DeepEqual(pc.written, want) {
				t.Fatalf("NAT destination changed: got %v, want %v", pc.written, want)
			}
			if pc.calls != 1 {
				t.Fatalf("a cached destination was selected again: resolves=%d", pc.calls)
			}
			if !reflect.DeepEqual(first, firstBefore) || !reflect.DeepEqual(second, secondBefore) {
				t.Fatal("processing changed the original packet destination metadata")
			}
			if got := sender.RestoreReadFrom(test.wantIP); got != secondIP {
				t.Fatalf("reply mapping lost the client's address: %s", got)
			}
		})
	}
}
