package sing_tun

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/listener/sing"

	"github.com/metacubex/sing/common/buf"
	M "github.com/metacubex/sing/common/metadata"
	"github.com/metacubex/sing/common/network"
	"github.com/miekg/dns"
)

type dnsRoutingService struct {
	metadata chan *C.Metadata
	inbound  chan bool
}

func (s dnsRoutingService) ServeMsg(ctx context.Context, request *dns.Msg) (*dns.Msg, error) {
	s.metadata <- icontext.DNSRoutingMetadata(ctx)
	s.inbound <- icontext.DNSRoutingInbound(ctx)
	return new(dns.Msg).SetReply(request), nil
}

type dnsRoutingWriter struct{ response chan M.Socksaddr }

func (w dnsRoutingWriter) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	buffer.Release()
	w.response <- destination
	return nil
}

func TestDNSRoutingTUNHijackKeepsLocalServiceAndInboundPolicy(t *testing.T) {
	oldService := resolver.DefaultService
	service := dnsRoutingService{metadata: make(chan *C.Metadata, 2), inbound: make(chan bool, 2)}
	resolver.DefaultService = service
	t.Cleanup(func() { resolver.DefaultService = oldService })
	handler := &ListenerHandler{
		ListenerHandler: &sing.ListenerHandler{ListenerConfig: sing.ListenerConfig{
			Type: C.TUN,
			Additions: []inbound.Addition{
				inbound.WithInName("virtual-tun"),
				inbound.WithSpecialRules("office"),
				inbound.WithSpecialProxy("fixed-outbound"),
			},
		}},
		DnsAddrPorts: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.2:53")},
	}
	metadata := M.Metadata{
		Source:      M.ParseSocksaddr("10.0.0.5:54000"),
		Destination: M.ParseSocksaddr("198.18.0.2:53"),
	}
	ctx := sing.WithAdditions(context.Background(), inbound.WithInUser("client-user"))
	query, err := new(dns.Msg).SetQuestion("example.org.", dns.TypeA).Pack()
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan error, 1)
	go func() { done <- handler.NewConnection(ctx, server, metadata) }()
	frame := make([]byte, len(query)+2)
	binary.BigEndian.PutUint16(frame, uint16(len(query)))
	copy(frame[2:], query)
	if _, err := client.Write(frame); err != nil {
		t.Fatal(err)
	}
	var length [2]byte
	if _, err := io.ReadFull(client, length[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, client, int64(binary.BigEndian.Uint16(length[:]))); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	writer := dnsRoutingWriter{response: make(chan M.Socksaddr, 1)}
	handler.NewPacket(ctx, metadata.Source.AddrPort(), buf.As(query), metadata, func(network.PacketConn) network.PacketWriter { return writer })
	select {
	case destination := <-writer.response:
		if destination != metadata.Destination {
			t.Fatalf("local DNS response changed synthetic destination: %v", destination)
		}
	case <-time.After(time.Second):
		t.Fatal("TUN UDP query did not reach local resolver service")
	}
	for _, network := range []C.NetWork{C.TCP, C.UDP} {
		got := <-service.metadata
		if !<-service.inbound {
			t.Fatal("actual TUN DNS request was mislabeled as an internal lookup")
		}
		if got == nil || got.Type != C.TUN || got.NetWork != network || got.InName != "virtual-tun" || got.InUser != "client-user" || got.SpecialProxy != "fixed-outbound" || got.SpecialRules != "office" || got.SrcIP != metadata.Source.Addr || got.SrcPort != metadata.Source.Port || got.DstIP != metadata.Destination.Addr {
			t.Fatalf("TUN DNS routing context lost original policy: %+v", got)
		}
	}
}
