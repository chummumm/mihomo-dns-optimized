package dns

import (
	"context"
	"net/netip"
	"strings"
	"sync/atomic"

	"github.com/metacubex/mihomo/component/dnsmessage"
	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

type dnsQueryRouteKey struct{}

// One plan belongs to one logical resolver query, including its main/fallback
// races and transport retries. Explicit nameserver exits do not use this plan.
type dnsQueryRoute struct {
	plan     *tunnel.DNSRoutingPlan
	origin   *C.Metadata
	key      string
	err      error
	pool     string
	main     []dnsClient
	fallback []dnsClient
	closed   atomic.Bool // a dashboard-closed exchange must not be retried
}

func queryRoute(ctx context.Context) *dnsQueryRoute {
	value, _ := ctx.Value(dnsQueryRouteKey{}).(*dnsQueryRoute)
	return value
}

func (c *client) canRouteDNS() bool {
	return c.proxyAdapter == nil && (c.proxyName == "" || c.proxyName == RespectRules)
}

// Configured native transports already carry a parsed question. Their protocol
// and destination port do not constrain QNAME routing; explicit exits do.
func dnsRoutingCapability(dc dnsClient) (automatic, explicit bool) {
	if wrapped, ok := dc.(interface{ Unwrap() dnsClient }); ok {
		return dnsRoutingCapability(wrapped.Unwrap())
	}
	if _, dialer, _, _ := nativeClientDetails(dc); dialer != nil {
		return dialer.Automatic(), !dialer.Automatic()
	}
	switch dc.(type) {
	case *systemClient:
		// Discovered system servers are plain DNS IP:53 clients. The same
		// query context reaches the generated clients without another lookup.
		return true, false
	}
	return false, true
}

func dnsRoutingNetwork(dc dnsClient) C.NetWork {
	if wrapped, ok := dc.(interface{ Unwrap() dnsClient }); ok {
		return dnsRoutingNetwork(wrapped.Unwrap())
	}
	if _, dialer, network, _ := nativeClientDetails(dc); dialer != nil {
		return network
	}
	return C.UDP
}

func (r *Resolver) prepareDNSRouting(ctx context.Context, message *D.Msg) (context.Context, *D.Msg, error) {
	// Nested resolver calls must prepare their own plan or explicitly remain
	// outside this scope; inheriting a parent's QNAME plan could route bootstrap
	// or a different business question through the wrong frozen outbound.
	ctx = context.WithValue(ctx, dnsQueryRouteKey{}, (*dnsQueryRoute)(nil))
	if r.bootstrap {
		return icontext.WithDNSBootstrap(ctx), nil, nil
	}
	if !r.ruleRouting || !tunnel.DNSRuleRoutingEnabled() || tunnel.Mode() != tunnel.Rule || icontext.DNSBootstrap(ctx) {
		return ctx, nil, nil
	}
	var automatic bool
	var upstreamNetwork C.NetWork
	clientSets := [][]dnsClient{r.main}
	// Only IP lookups use fallback (see exchangeWithoutCache). An unused
	// explicit fallback must not exempt TXT/MX/etc. from their main route's
	// rejection, or cause a group selection for an entirely fixed main.
	if isIPRequest(message.Question[0]) {
		clientSets = append(clientSets, r.fallback)
	}
	origin := icontext.DNSRoutingMetadata(ctx)
	nativePool := len(r.direct) > 0 && (origin == nil || origin.SpecialProxy == "")
	if nativePool {
		// A direct pool may be the only automatic DNS transport. It must not
		// be ruled out by an explicit, but unselected, main pool.
		clientSets = append(clientSets, r.direct)
	}
	for _, clients := range clientSets {
		for _, client := range clients {
			auto, _ := dnsRoutingCapability(client)
			if auto && !automatic {
				upstreamNetwork = dnsRoutingNetwork(client)
			}
			automatic = automatic || auto
		}
	}
	if !automatic {
		return ctx, nil, nil
	}
	wire, err := message.Pack()
	if err != nil {
		return ctx, nil, err
	}
	if _, err := dnsmessage.UnpackQuery(wire); err != nil {
		return ctx, nil, err
	}
	if origin == nil {
		origin = &C.Metadata{Type: C.INNER, NetWork: C.UDP}
	}
	// The website's 443, or a local DNS listener's 1053, is not this upstream
	// DNS query's destination port. InPort still identifies the real listener.
	origin.DstPort = 53
	if !icontext.DNSRoutingInbound(ctx) {
		// A target lookup has no client DNS transport. Pick the first eligible
		// configured upstream's protocol once for the logical query; fallback
		// and truncation retries keep that same rule/group decision.
		origin.NetWork = upstreamNetwork
	}
	qname := strings.ToLower(strings.TrimSuffix(message.Question[0].Name, "."))
	if qname == "" {
		qname = "."
	}
	plan, err := tunnel.PrepareDNSRouting(ctx, qname, origin)
	if err == nil {
		// Rule matching may have discovered the real client's process. Keep
		// that complete source identity for cache partitioning and renewals.
		origin = plan.OriginMetadata()
	}
	state := &dnsQueryRoute{plan: plan, origin: origin, err: err}
	if nativePool {
		state.pool, state.main, state.fallback = "main", r.main, r.fallback
		if err == nil && (plan.Type() == C.Direct || plan.Type() == C.Compatible) {
			// Use the existing direct clients without recursing through their
			// Resolver: this question must keep its already selected leaf.
			state.pool, state.main, state.fallback = "direct", r.direct, nil
		}
	}
	main, fallback := r.dnsQueryServersWithRoute(state)
	_, explicit := dnsQueryCapabilities(message, main, fallback)
	if err != nil && !explicit {
		return ctx, nil, err
	}
	planErr := err
	// A pure automatic query can honor the rule action before any cache hit.
	// Mixed explicit nameserver exits retain their deliberate exemption.
	if !explicit {
		switch plan.Type() {
		case C.Reject:
			markDNSOutcome(ctx, dnsstats.Reject)
			return ctx, new(D.Msg).SetRcode(message, D.RcodeRefused), nil
		case C.RejectDrop:
			markDNSOutcome(ctx, dnsstats.Drop)
			return ctx, nil, resolver.ErrDNSDrop
		}
	}
	state.key = makeDNSRouteKey(ctx, state, wire, !explicit && planErr == nil)
	return context.WithValue(ctx, dnsQueryRouteKey{}, state), nil, nil
}

func dnsCacheKey(ctx context.Context, question D.Question) string {
	if route := queryRoute(ctx); route != nil && !icontext.DNSBootstrap(ctx) {
		return route.key
	}
	return question.String()
}

func (c *client) exchangeRouted(ctx context.Context, message *D.Msg, route *dnsQueryRoute) (response *D.Msg, err error) {
	if route.err != nil {
		return nil, route.err
	}
	attempt := observeDNSUpstream(ctx, c.Address())
	defer func() { finishDNSUpstream(attempt, response, err) }()
	ctx = tunnel.WithDNSQueryCloseHandler(ctx, func() { route.closed.Store(true) })
	address, err := netip.ParseAddr(c.host)
	if err != nil {
		// The resolver's hostname is infrastructure, never the business QNAME.
		// c.resolver is the separately constructed default/bootstrap resolver.
		address, err = resolver.ResolveIPWithResolver(icontext.WithDNSBootstrap(ctx), c.host, c.resolver)
		if err != nil {
			return nil, err
		}
	}
	destination := route.origin.Clone()
	destination.DstIP, destination.DstPort = address.Unmap(), 53
	destination.Host, destination.SniffHost = "", ""
	destination.NetWork = C.UDP
	if c.schema == "tcp" {
		destination.NetWork = C.TCP
	}
	wire, err := message.Pack()
	if err != nil {
		return nil, err
	}
	exchange := func() (*D.Msg, error) {
		response, err := route.plan.Exchange(ctx, wire, destination)
		if err != nil {
			return nil, err
		}
		result := new(D.Msg)
		if err := result.Unpack(response); err != nil {
			return nil, err
		}
		return result, nil
	}
	response, err = exchange()
	if err == nil && response.Truncated && destination.NetWork == C.UDP {
		destination.NetWork = C.TCP
		return exchange()
	}
	return response, err
}
