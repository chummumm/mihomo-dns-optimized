package tunnel_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/component/dialer"
	R "github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"
)

type dnsProbeForbiddenResolver struct {
	R.Resolver
	calls atomic.Int32
}

func (r *dnsProbeForbiddenResolver) Invalid() bool { return true }
func (r *dnsProbeForbiddenResolver) LookupIP(_ context.Context, host string) ([]netip.Addr, error) {
	// The low-level dialer may ask its Resolver to recognize a literal. That
	// is not a DNS lookup; only a hostname would risk recursive resolution.
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip.Unmap()}, nil
	}
	r.calls.Add(1)
	return nil, errors.New("numeric DNS speed probe attempted recursive resolution")
}
func (r *dnsProbeForbiddenResolver) LookupIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	return r.LookupIP(ctx, host)
}
func (r *dnsProbeForbiddenResolver) LookupIPv6(ctx context.Context, host string) ([]netip.Addr, error) {
	return r.LookupIP(ctx, host)
}

func dnsProbeTestPlan(t *testing.T, family C.DNSPrefer) (*tunnel.DNSRoutingPlan, *outbound.Direct) {
	t.Helper()
	leaf := outbound.NewDirectWithOption(outbound.DirectOption{
		Name:        "probe-direct",
		BasicOption: outbound.BasicOption{TFO: true, IPVersion: family},
	})
	origin := &C.Metadata{
		Type: C.INNER, NetWork: C.UDP, InName: "DNS",
		Host: "must-not-resolve.invalid", SniffHost: "must-not-sniff.invalid",
		DstIP: netip.MustParseAddr("192.0.2.53"), DstPort: 53,
		Process: "dns-client", ProcessPath: "/test/dns-client", Uid: 1000,
	}
	// Live rules and groups return adapter.Proxy wrappers around the actual
	// leaf. The probe must retain access to the leaf's optional dial methods.
	ctx := icontext.WithDNSFixedOutbound(icontext.WithDNSRoutingMetadata(context.Background(), origin), adapter.NewProxy(leaf))
	plan, err := tunnel.PrepareDNSRouting(ctx, "probe-target.invalid", origin)
	if err != nil {
		t.Fatal(err)
	}
	return plan, leaf
}

func TestDNSDirectProbeTFOMakesRealConnectionWithoutDNS(t *testing.T) {
	forbidden := &dnsProbeForbiddenResolver{}
	oldDefault, oldDirect := R.DefaultResolver, R.DirectHostResolver
	R.DefaultResolver, R.DirectHostResolver = forbidden, forbidden
	t.Cleanup(func() { R.DefaultResolver, R.DirectHostResolver = oldDefault, oldDirect })

	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	plan, leaf := dnsProbeTestPlan(t, C.DualStack)
	before := plan.OriginMetadata()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	address := listener.Addr().(*net.TCPAddr).AddrPort()
	conn, err := plan.DialDirectProbe(ctx, address.Addr(), address.Port())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A lazy TFO connection has no real socket until its first Write. A speed
	// probe must complete the handshake without sending application data.
	if conn.RemoteAddr().String() != listener.Addr().String() {
		t.Fatalf("probe returned an undialed connection: remote=%v", conn.RemoteAddr())
	}
	accepted, err := listener.AcceptTCP()
	if err != nil {
		t.Fatalf("probe reported success without a real TCP connection: %v", err)
	}
	defer accepted.Close()
	if forbidden.calls.Load() != 0 {
		t.Fatalf("numeric probe performed %d DNS lookups", forbidden.calls.Load())
	}
	if !leaf.ProxyInfo().TFO || !reflect.DeepEqual(before, plan.OriginMetadata()) {
		t.Fatal("probe changed the original DIRECT settings or routing metadata")
	}
}

func TestDNSDirectProbeTFORejectsClosedPort(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().(*net.TCPAddr).AddrPort()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	plan, _ := dnsProbeTestPlan(t, C.DualStack)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := plan.DialDirectProbe(ctx, address.Addr(), address.Port())
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("TFO probe reported an unreachable TCP port as reachable")
	}
}

func TestDNSDirectProbeTFOHonorsCancellation(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.Addr().(*net.TCPAddr).AddrPort()
	plan, _ := dnsProbeTestPlan(t, C.DualStack)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := plan.DialDirectProbe(ctx, address.Addr(), address.Port())
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("probe ignored its canceled context: %v", err)
	}
}

func TestDNSDirectProbePreservesAddressFamilyConstraints(t *testing.T) {
	oldDisableIPv6 := R.DisableIPv6
	R.DisableIPv6 = false
	t.Cleanup(func() { R.DisableIPv6 = oldDisableIPv6 })
	for _, test := range []struct {
		name    string
		family  C.DNSPrefer
		address string
		network string
	}{
		{"IPv4-only", C.IPv4Only, "::1", "ip6:ipv6-icmp"},
		{"IPv6-only", C.IPv6Only, "127.0.0.1", "ip4:icmp"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, _ := dnsProbeTestPlan(t, test.family)
			address := netip.MustParseAddr(test.address)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, err := plan.DialDirectProbe(ctx, address, 443)
			if conn != nil {
				_ = conn.Close()
			}
			if !errors.Is(err, R.ErrIPVersion) {
				t.Fatalf("TCP probe lost the DIRECT address-family constraint: %v", err)
			}
			options, err := plan.DirectProbeOptions()
			if err != nil {
				t.Fatal(err)
			}
			// Validate the family before touching a raw socket. This test needs
			// no ICMP privileges and must not contact any network endpoint.
			control := dialer.ICMPControlWithOptions(address, options...)
			if err := control(test.network, test.address, nil); err == nil {
				t.Fatal("ICMP probe lost the DIRECT address-family constraint")
			}
		})
	}
}
