package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/metacubex/mihomo/common/atomic"
	"github.com/metacubex/mihomo/component/dnsmessage"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"

	"github.com/miekg/dns"
)

var dnsRuleRouting = atomic.NewBool(false)
var dnsRoutingEpoch = atomic.NewUint64(0)

func DNSRuleRoutingEnabled() bool { return dnsRuleRouting.Load() }

func SetDNSRuleRouting(enabled bool) {
	if dnsRuleRouting.Swap(enabled) != enabled {
		dnsRoutingEpoch.Add(1)
	}
}

// DNSRoutingEpoch also detects a mode/flag change followed by a change back.
// Old background resolver work must not outlive either transition.
func DNSRoutingEpoch() uint64 { return dnsRoutingEpoch.Load() }

func dnsRoutingMetadata(qname string, origin *C.Metadata) *C.Metadata {
	metadata := origin.Clone()
	metadata.Host = qname
	metadata.SniffHost = qname
	metadata.DstIP = netip.Addr{}
	metadata.DstGeoIP = nil
	metadata.DstIPASN = ""
	metadata.RawDstAddr = nil
	metadata.RemoteDst = ""
	metadata.DNSMode = C.DNSNormal
	return metadata
}

// DNSRoutingPlan freezes one query's rule/group choice across resolver cache
// lookup, main/fallback races and a possible retry using another transport.
type DNSRoutingPlan struct {
	route   dnsProxyRoute
	origin  *C.Metadata
	epoch   uint64
	inbound bool
}

func PrepareDNSRouting(ctx context.Context, qname string, origin *C.Metadata) (*DNSRoutingPlan, error) {
	if icontext.DNSBootstrap(ctx) {
		return nil, errors.New("bootstrap DNS must not enter query rule routing")
	}
	if origin == nil {
		origin = &C.Metadata{Type: C.INNER, NetWork: C.UDP}
	}
	if _, valid := dns.IsDomainName(dns.Fqdn(qname)); !valid {
		return nil, errors.New("invalid DNS routing question name")
	}
	qname = strings.ToLower(strings.TrimSuffix(qname, "."))
	if qname == "" {
		qname = "."
	}
	metadata := dnsRoutingMetadata(qname, origin)
	var route dnsProxyRoute
	var err error
	if fixed := icontext.DNSFixedOutbound(ctx); fixed != nil {
		route, err = unwrapDNSProxyRoute(fixed, nil, metadata, true, false)
	} else {
		route, err = selectDNSProxyWithProcessSnapshot(metadata, true, icontext.DNSProcessSnapshot(ctx), icontext.DNSRoutingMetadata(ctx))
	}
	if err != nil {
		return nil, err
	}
	if route.proxy.Type() == C.Dns || route.proxy.Type() == C.Rematch || route.proxy.Type() == C.Pass || route.proxy.Type() == C.PassRule {
		return nil, fmt.Errorf("DNS query routing cannot use outbound type %s", route.proxy.Type())
	}
	route.qname = qname
	copyOrigin := origin.Clone()
	copyOrigin.Process, copyOrigin.ProcessPath, copyOrigin.Uid = metadata.Process, metadata.ProcessPath, metadata.Uid
	return &DNSRoutingPlan{route: route, origin: copyOrigin, epoch: DNSRoutingEpoch(), inbound: icontext.DNSRoutingInbound(ctx)}, nil
}

func (p *DNSRoutingPlan) Type() C.AdapterType { return p.route.proxy.Type() }

// OriginMetadata includes process identity discovered during rule matching.
// Background refreshes reuse this snapshot instead of probing an expired socket.
func (p *DNSRoutingPlan) OriginMetadata() *C.Metadata { return p.origin.Clone() }

// Object identities invalidate routing-scoped cache entries when providers or
// configuration replace a node while retaining its display name.
func (p *DNSRoutingPlan) CacheKey() string {
	var key strings.Builder
	fmt.Fprintf(&key, "%p", p.route.proxy)
	for _, group := range p.route.groups {
		fmt.Fprintf(&key, "/%p", group)
	}
	return key.String()
}

func (p *DNSRoutingPlan) Exchange(ctx context.Context, wire []byte, destination *C.Metadata) ([]byte, error) {
	query, err := dnsmessage.UnpackQuery(wire)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(dns.Fqdn(p.route.qname), query.Question[0].Name) {
		return nil, errors.New("DNS routing plan belongs to another question")
	}
	if destination == nil {
		return nil, errors.New("DNS routing plan requires a resolver destination")
	}
	metadata := p.origin.Clone()
	metadata.NetWork = destination.NetWork
	metadata.Host, metadata.DstIP, metadata.DstPort = destination.Host, destination.DstIP, destination.DstPort
	return exchangeDNSProxy(ctx, wire, metadata, func(*C.Metadata) (dnsProxyRoute, error) {
		return p.route, nil
	}, exchangeDNSProxyWire)
}

func validateDNSRouteTransport(route dnsProxyRoute, network C.NetWork) error {
	if network != C.UDP {
		return nil
	}
	if route.udpSupportFrozen {
		return route.udpSupportError
	}
	for _, proxy := range append(append([]C.ProxyAdapter(nil), route.groups...), route.proxy) {
		if !proxy.SupportUDP() {
			return fmt.Errorf("DNS outbound %q does not support UDP", proxy.Name())
		}
	}
	return nil
}
