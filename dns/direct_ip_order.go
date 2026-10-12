package dns

import "github.com/metacubex/mihomo/tunnel"

// PreferIPOrder lets DIRECT consumers keep the resolver's preferred address
// order. Raw fallback answers keep their valid upstream order; connection
// fallback remains available if the first address cannot be reached.
func (r *Resolver) PreferIPOrder() bool {
	return r != nil && r.speedChecker != nil && !r.bootstrap && r.ruleRouting &&
		tunnel.DNSRuleRoutingEnabled() && tunnel.Mode() == tunnel.Rule
}

var _ interface{ PreferIPOrder() bool } = (*Resolver)(nil)
