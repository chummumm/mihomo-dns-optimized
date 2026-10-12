package outbound

import (
	"testing"

	C "github.com/metacubex/mihomo/constant"
)

func TestBuiltinDirectDNSStatsIdentitySurvivesReconstruction(t *testing.T) {
	first, second := NewDirect(), NewDirect()
	if first == second || first.Id() == second.Id() || first.DNSStatsIdentity() == "" {
		t.Fatal("test must use distinct DIRECT instances with a stable identity")
	}
	if first.DNSStatsIdentity() != second.DNSStatsIdentity() {
		t.Fatal("built-in DIRECT reconstruction split statistics")
	}
	compatible, replacement := NewCompatible(), NewCompatible()
	if compatible.DNSStatsIdentity() == "" || compatible.DNSStatsIdentity() != replacement.DNSStatsIdentity() || compatible.DNSStatsIdentity() == first.DNSStatsIdentity() {
		t.Fatal("COMPATIBLE identity did not retain its own stable boundary")
	}
	wrapped := NewAutoCloseProxyAdapter(first)
	t.Cleanup(func() { _ = wrapped.Close() })
	if wrapped.(interface{ DNSStatsIdentity() string }).DNSStatsIdentity() != first.DNSStatsIdentity() {
		t.Fatal("lifetime wrapper lost its DIRECT observation identity")
	}
}

func TestDirectDNSStatsIdentityIncludesEffectiveOptions(t *testing.T) {
	baseline := DirectOption{Name: "same-name"}
	want := NewDirectWithOption(baseline).DNSStatsIdentity()
	for name, option := range map[string]BasicOption{
		"interface": {Interface: "other-interface"},
		"mark":      {RoutingMark: 47},
		"family":    {IPVersion: C.IPv6Only},
		"tfo":       {TFO: true},
		"mptcp":     {MPTCP: true},
		"provider":  {ProviderName: "other-provider"},
	} {
		t.Run(name, func(t *testing.T) {
			changed := baseline
			changed.BasicOption = option
			if NewDirectWithOption(changed).DNSStatsIdentity() == want {
				t.Fatal("different DIRECT options shared an observation identity")
			}
		})
	}
	if NewBase(BaseOption{Name: "same-name", Type: C.Socks5}).DNSStatsIdentity() != "" {
		t.Fatal("common options alone claimed a full proxy configuration identity")
	}
}
