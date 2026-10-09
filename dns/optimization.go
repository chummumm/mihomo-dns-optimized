package dns

import (
	"context"
	"net/netip"
	"time"

	C "github.com/metacubex/mihomo/constant"

	D "github.com/miekg/dns"
)

// Only ordinary DIRECT query exchanges are eligible for destination-IP
// optimization. Explicit transports retain their own behavior and forwarded
// inbound DNS never enters this resolver path.
func (r *Resolver) exchangeBatch(ctx context.Context, clients []dnsClient, query *D.Msg) (*D.Msg, bool, error) {
	route := queryRoute(ctx)
	if r.speedChecker == nil || route == nil || route.err != nil || route.plan == nil {
		return batchExchange(ctx, clients, query)
	}
	if kind := route.plan.Type(); kind != C.Direct && kind != C.Compatible {
		return batchExchange(ctx, clients, query)
	}
	automatic, explicit := dnsQueryCapabilities(query, clients, nil)
	if !automatic || explicit {
		return batchExchange(ctx, clients, query)
	}
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
	return r.speedChecker.ExchangeWithProbe(ctx, clients, query, probe)
}
