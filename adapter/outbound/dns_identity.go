package outbound

import (
	"crypto/sha256"
	"fmt"

	C "github.com/metacubex/mihomo/constant"
)

// directDNSStatsIdentity covers all options consumed by a DIRECT adapter,
// including builtins which do not pass through ParseProxy. Other protocols
// need their complete parser configuration, not just these common options.
func directDNSStatsIdentity(opt BaseOption) string {
	if opt.Type != C.Direct && opt.Type != C.Compatible {
		return ""
	}
	return fmt.Sprintf("direct-v1:%x", sha256.Sum256([]byte(fmt.Sprintf("%#v", opt))))
}

// DNSStatsIdentity is an immutable observation identity, not a cache or
// transport-pool key. Empty means no stable configuration identity is known.
func (b *Base) DNSStatsIdentity() string {
	return b.dnsStatsIdentity
}

func (p *autoCloseProxyAdapter) DNSStatsIdentity() string {
	return p.dnsStatsIdentity
}

// NewAutoCloseProxyAdapterWithDNSStatsIdentity attaches a construction-time
// configuration fingerprint without exposing a setter on a published adapter.
func NewAutoCloseProxyAdapterWithDNSStatsIdentity(adapter ProxyAdapter, identity string) ProxyAdapter {
	proxy := newAutoCloseProxyAdapter(adapter)
	// DIRECT also supplies its concrete adapter in lookup contexts. Preserve
	// its complete native identity so those lookups share the wrapper's row.
	if proxy.dnsStatsIdentity == "" {
		proxy.dnsStatsIdentity = identity
	}
	return proxy
}
