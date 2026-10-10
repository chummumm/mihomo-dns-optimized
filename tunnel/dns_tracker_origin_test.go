package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/proxydialer"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

func TestDNSMarkerDistinguishesResolverFromOtherInternalDialers(t *testing.T) {
	for _, localDNS := range []bool{false, true} {
		for _, method := range []string{"tcp", "udp", "packet"} {
			for _, port := range []uint16{53, 8443} {
				t.Run(fmt.Sprintf("resolver=%t/%s/%d", localDNS, method, port), func(t *testing.T) {
					// A DNS-looking name, destination, and inherited client lookup
					// context cannot turn a generic dialer-proxy flow into DNS.
					leaf := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("DNS", C.Socks5, true)}
					leaf.dial = func(context.Context, *C.Metadata) (C.Conn, error) {
						conn, peer := net.Pipe()
						t.Cleanup(func() { _ = peer.Close() })
						return &dnsProxyTestConn{ExtendedConn: N.NewExtendedConn(conn),
							dnsProxyTestConnection: dnsProxyTestConnection{chain: C.Chain{leaf.Name()}}}, nil
					}
					leaf.packet = func(ctx context.Context, _ *C.Metadata) (C.PacketConn, error) {
						conn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp4", "127.0.0.1:0")
						if err != nil {
							return nil, err
						}
						return &dnsProxyTestProxyPacketConn{EnhancePacketConn: N.NewEnhancePacketConn(conn),
							dnsProxyTestConnection: dnsProxyTestConnection{chain: C.Chain{leaf.Name()}}}, nil
					}
					before := make(map[string]bool)
					statistic.DefaultManager.Range(func(tracker statistic.Tracker) bool { before[tracker.ID()] = true; return true })
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					ctx = icontext.WithDNSRoutingInbound(icontext.WithDNSRoutingMetadata(ctx, &C.Metadata{
						Type: C.HTTP, InName: "DNS", SrcIP: netip.MustParseAddr("192.0.2.10"), SrcPort: 41000,
					}))
					destination := netip.AddrPortFrom(netip.MustParseAddr("192.0.2.53"), port)
					var conn io.Closer
					var err error
					if localDNS {
						dialer := NewDNSDialer(nil, leaf, "")
						if method == "packet" {
							conn, err = dialer.ListenPacket(ctx, "udp", destination.String())
						} else {
							conn, err = dialer.DialContext(ctx, method, destination.String())
						}
					} else {
						dialer := proxydialer.New(leaf, true)
						if method == "packet" {
							conn, err = dialer.ListenPacket(ctx, "udp", "", destination)
						} else {
							conn, err = dialer.DialContext(ctx, method, destination.String())
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = conn.Close() })
					var created []statistic.Tracker
					statistic.DefaultManager.Range(func(tracker statistic.Tracker) bool {
						if !before[tracker.ID()] {
							created = append(created, tracker)
						}
						return true
					})
					if len(created) != 1 {
						t.Fatalf("internal dial produced %d trackers, want one", len(created))
					}
					tracker := created[0]
					info := tracker.Info()
					if info.DNS != localDNS || info.Metadata.Type != C.INNER || info.Metadata.SrcIP.IsValid() || info.Metadata.AddrPort() != destination {
						t.Fatalf("internal transport has incorrect DNS origin: %+v", info)
					}
					if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
					if statistic.DefaultManager.Get(tracker.ID()) != nil {
						t.Fatal("closed internal transport retained its tracker")
					}
				})
			}
		}
	}
}
