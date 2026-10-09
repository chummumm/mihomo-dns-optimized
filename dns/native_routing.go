package dns

import (
	"context"
	"fmt"
	"strings"

	D "github.com/miekg/dns"
)

// dnsQueryServers returns the upstream pool frozen when this logical query was
// prepared. Direct queries never borrow main/fallback servers after a failure.
// Resolvers without a direct pool retain their original main/fallback behavior.
func (r *Resolver) dnsQueryServers(ctx context.Context) (main, fallback []dnsClient) {
	return r.dnsQueryServersWithRoute(queryRoute(ctx))
}

func (r *Resolver) dnsQueryServersWithRoute(route *dnsQueryRoute) (main, fallback []dnsClient) {
	if route != nil && route.pool != "" {
		return route.main, route.fallback
	}
	return r.main, r.fallback
}

func dnsQueryCapabilities(message *D.Msg, main, fallback []dnsClient) (automatic, explicit bool) {
	sets := [][]dnsClient{main}
	// Non-IP questions do not consult fallback in exchangeWithoutCache.
	if isIPRequest(message.Question[0]) {
		sets = append(sets, fallback)
	}
	for _, clients := range sets {
		for _, client := range clients {
			auto, fixed := dnsRoutingCapability(client)
			automatic = automatic || auto
			explicit = explicit || fixed
		}
	}
	return
}

func (route *dnsQueryRoute) poolIdentity() string {
	if route.pool == "" {
		return ""
	}
	var identity strings.Builder
	identity.WriteString(route.pool)
	for _, set := range [][]dnsClient{route.main, route.fallback} {
		identity.WriteByte('|')
		for _, client := range set {
			fmt.Fprintf(&identity, "%T:%p;", client, client)
		}
	}
	return identity.String()
}
