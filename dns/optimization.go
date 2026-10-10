package dns

import (
	"context"
	"net/netip"
	"time"

	C "github.com/metacubex/mihomo/constant"

	D "github.com/miekg/dns"
)

type dnsSpeedCheckResponseFilterKey struct{}

// A main pool with fallback must apply its original whole-answer IP policy
// before candidate selection can narrow an RRset or synthesize dual-stack
// NODATA. The context belongs to that main exchange only, including its
// auxiliary family; fallback and independent direct pools keep their policies.
func withSpeedCheckResponseFilter(ctx context.Context, accept func(*D.Msg) bool) context.Context {
	return context.WithValue(ctx, dnsSpeedCheckResponseFilterKey{}, accept)
}

func speedCheckAcceptResponse(ctx context.Context, response *D.Msg) bool {
	accept, _ := ctx.Value(dnsSpeedCheckResponseFilterKey{}).(func(*D.Msg) bool)
	return accept == nil || accept(response)
}

// Only ordinary DIRECT query exchanges are eligible for destination-IP
// optimization. Explicit transports retain their own behavior and ordinary
// proxy traffic never enters this resolver path. The resolver uses the same gate
// when deciding whether raw-answer filtering is delegated to the checker.
func (r *Resolver) speedCheckEligible(ctx context.Context, clients []dnsClient, query *D.Msg) bool {
	route := queryRoute(ctx)
	if r.speedChecker == nil || query == nil || !speedCheckQuestion(query) || route == nil || route.err != nil || route.plan == nil {
		return false
	}
	if kind := route.plan.Type(); kind != C.Direct && kind != C.Compatible {
		return false
	}
	automatic, explicit := dnsQueryCapabilities(query, clients, nil)
	return automatic && !explicit
}

func (r *Resolver) exchangeBatch(ctx context.Context, clients []dnsClient, query *D.Msg) (*D.Msg, bool, error) {
	if !r.speedCheckEligible(ctx, clients, query) {
		return batchExchange(ctx, clients, query)
	}
	route := queryRoute(ctx)
	probe := func(ctx context.Context, address netip.Addr, mode speedCheckMode) (time.Duration, error) {
		if mode.port == 0 {
			options, err := route.plan.DirectProbeOptions()
			if err != nil {
				return 0, err
			}
			return pingDirectIP(ctx, address, options...)
		}
		start := time.Now()
		connection, err := route.plan.DialDirectProbe(ctx, address, mode.port)
		if err != nil {
			return 0, err
		}
		elapsed := time.Since(start)
		_ = connection.Close()
		return elapsed, nil
	}
	dualStack := r.dualStack
	// IPv6 here is the effective global-and-DNS setting supplied by the
	// executor. A family that the DNS service cannot deliver must never hide
	// the other family's valid answer, even for programmatically built config.
	dualStack.Enabled = dualStack.Enabled && r.ipv6
	return r.speedChecker.ExchangeDualStackWithProbe(ctx, clients, query, probe, dualStack)
}
