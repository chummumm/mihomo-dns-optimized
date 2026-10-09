package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	stdatomic "sync/atomic"
	"time"

	"github.com/metacubex/mihomo/common/atomic"
	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/common/utils"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

// TransportKey deliberately excludes QNAME and source-port: encrypted DNS may
// multiplex unrelated questions over the same immutable exit. Query caches and
// logical trackers retain their stricter per-request identity separately.
func (p *DNSRoutingPlan) TransportKey() string {
	return fmt.Sprintf("%s|%d|%v|%v|%d", p.CacheKey(), p.Type(), p.route.proxy.ProxyInfo(), p.route.udpSupportError, p.epoch)
}

func (p *DNSRoutingPlan) nativeDestination(ctx context.Context, network, addr string, bootstrap resolver.Resolver) (*C.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.Type() == C.Reject || p.Type() == C.RejectDrop {
		return nil, errors.New("native DNS route is rejected")
	}
	metadata := &C.Metadata{Type: C.INNER, InName: "DNS-TRANSPORT", NetWork: C.TCP}
	if !strings.HasPrefix(network, "tcp") {
		metadata.NetWork = C.UDP
	}
	if err := validateDNSRouteTransport(p.route, metadata.NetWork); err != nil {
		return nil, err
	}
	if err := metadata.SetRemoteAddress(addr); err != nil {
		return nil, err
	}
	if metadata.DstPort == 0 {
		return nil, errors.New("native DNS transport requires a destination port")
	}
	if !metadata.DstIP.IsValid() {
		address, err := resolver.ResolveIPWithResolver(icontext.WithDNSBootstrap(ctx), metadata.Host, bootstrap)
		if err != nil {
			return nil, fmt.Errorf("DNS transport bootstrap: %w", err)
		}
		metadata.DstIP = address.Unmap()
	}
	if !(metadata.DstIP.IsGlobalUnicast() || metadata.DstIP.IsLoopback() || metadata.DstIP.IsLinkLocalUnicast()) {
		return nil, errors.New("native DNS requires a unicast resolver address")
	}
	// TLS and HTTP retain their configured hostname for SNI/certificate checks.
	// The actual exit sees only the bootstrapped resolver, never business QNAME.
	metadata.Host, metadata.SniffHost = "", ""
	return metadata, nil
}

func (p *DNSRoutingPlan) dialNativeDNS(ctx context.Context, network, addr string, bootstrap resolver.Resolver) (net.Conn, error) {
	metadata, err := p.nativeDestination(ctx, network, addr, bootstrap)
	if err != nil {
		return nil, err
	}
	ctx = icontext.WithDNSFixedOutbound(icontext.WithDNSRoutingMetadata(ctx, metadata), p.route.proxy)
	if metadata.NetWork == C.UDP {
		connection, err := p.nativePacket(ctx, metadata)
		if err != nil {
			return nil, err
		}
		return N.NewBindPacketConn(connection, metadata.UDPAddr()), nil
	}
	connection, err := p.route.proxy.DialContext(ctx, metadata)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = connection.Close()
		return nil, err
	}
	appendDNSProxyGroups(connection, p.route.groups)
	// This persistent transport is infrastructure, not the first QNAME that
	// happened to create it. Count encrypted wire bytes here, exactly once.
	return statistic.NewTCPTracker(connection, statistic.DefaultManager, metadata, nil, 0, 0, true), nil
}

func (p *DNSRoutingPlan) nativePacket(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	connection, err := p.route.proxy.ListenPacketContext(ctx, metadata)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = connection.Close()
		return nil, err
	}
	appendDNSProxyGroups(connection, p.route.groups)
	return statistic.NewUDPTracker(connection, statistic.DefaultManager, metadata, nil, 0, 0, true), nil
}

func (p *DNSRoutingPlan) listenNativeDNS(ctx context.Context, network, addr string, bootstrap resolver.Resolver) (net.PacketConn, error) {
	metadata, err := p.nativeDestination(ctx, network, addr, bootstrap)
	if err != nil {
		return nil, err
	}
	ctx = icontext.WithDNSFixedOutbound(icontext.WithDNSRoutingMetadata(ctx, metadata), p.route.proxy)
	return p.nativePacket(ctx, metadata)
}

// DNSNativeQueryTracker represents a logical query, not a shared HTTP/QUIC
// connection. Its payload counters do not increment global wire-byte totals.
type DNSNativeQueryTracker struct {
	*statistic.TrackerInfo
	endpoint string
	cancel   context.CancelCauseFunc
	once     sync.Once
}

var ErrDNSNativeQueryClosed = errors.New("DNS query closed through the connections API")

type dnsQueryCloseHandlerKey struct{}

// WithDNSQueryCloseHandler lets a native resolver suppress its automatic retry
// after the user closes an ordinary port-53 tracker in the connections API.
func WithDNSQueryCloseHandler(ctx context.Context, close func()) context.Context {
	return context.WithValue(ctx, dnsQueryCloseHandlerKey{}, close)
}

type dnsQueryCloseState struct {
	finished stdatomic.Bool
	once     sync.Once
	notify   func()
}

func newDNSQueryCloseState(ctx context.Context) *dnsQueryCloseState {
	notify, _ := ctx.Value(dnsQueryCloseHandlerKey{}).(func())
	if notify == nil {
		return nil
	}
	return &dnsQueryCloseState{notify: notify}
}

func (s *dnsQueryCloseState) closed() {
	if !s.finished.Load() {
		s.once.Do(s.notify)
	}
}

type dnsQueryNotifyConn struct {
	C.Conn
	state *dnsQueryCloseState
}

func (c *dnsQueryNotifyConn) Close() error { c.state.closed(); return c.Conn.Close() }

type dnsQueryNotifyPacketConn struct {
	C.PacketConn
	state *dnsQueryCloseState
}

func (c *dnsQueryNotifyPacketConn) Close() error { c.state.closed(); return c.PacketConn.Close() }

type dnsQueryNormalCloser struct {
	close func() error
	state *dnsQueryCloseState
}

func (c dnsQueryNormalCloser) Close() error {
	if c.state != nil {
		c.state.finished.Store(true)
	}
	return c.close()
}

func (t *DNSNativeQueryTracker) ID() string                   { return t.UUID.String() }
func (t *DNSNativeQueryTracker) Info() *statistic.TrackerInfo { return t.TrackerInfo }
func (t *DNSNativeQueryTracker) Chains() C.Chain              { return t.Chain }
func (t *DNSNativeQueryTracker) ProviderChains() C.Chain      { return t.ProviderChain }
func (t *DNSNativeQueryTracker) RemoteDestination() string    { return t.endpoint }
func (t *DNSNativeQueryTracker) AppendToChains(adapter C.ProxyAdapter) {
	t.Chain = append(t.Chain, adapter.Name())
	t.ProviderChain = append(t.ProviderChain, adapter.ProxyInfo().ProviderName)
}
func (t *DNSNativeQueryTracker) Close() error {
	t.once.Do(func() { statistic.DefaultManager.Leave(t); t.cancel(ErrDNSNativeQueryClosed) })
	return nil
}

func (p *DNSRoutingPlan) TrackNativeQuery(ctx context.Context, endpoint string, network C.NetWork, upload int) (context.Context, *DNSNativeQueryTracker, error) {
	metadata := p.origin.Clone()
	if err := metadata.SetRemoteAddress(endpoint); err != nil {
		return ctx, nil, err
	}
	metadata.Host, metadata.SniffHost = p.route.qname, p.route.qname
	// The logical query belongs to its actual DNS client. The separate
	// DNS-TRANSPORT tracker describes DoH/DoQ/TLS's upstream transport.
	if !p.inbound {
		metadata.NetWork = network
	}
	metadata.RemoteDst = endpoint
	ctx, cancel := context.WithCancelCause(ctx)
	tracker := &DNSNativeQueryTracker{
		TrackerInfo: &statistic.TrackerInfo{UUID: utils.NewUUIDV4(), Start: time.Now(), Metadata: metadata,
			UploadTotal: atomic.NewInt64(int64(upload)), DownloadTotal: atomic.NewInt64(0)},
		endpoint: endpoint, cancel: cancel,
	}
	tracker.AppendToChains(p.route.proxy)
	for i := len(p.route.groups) - 1; i >= 0; i-- {
		tracker.AppendToChains(p.route.groups[i])
	}
	if p.route.rule != nil {
		tracker.Rule, tracker.RulePayload = p.route.rule.RuleType().String(), p.route.rule.Payload()
	}
	statistic.DefaultManager.Join(tracker)
	return ctx, tracker, nil
}
