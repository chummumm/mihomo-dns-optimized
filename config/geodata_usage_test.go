package config

import (
	"net/netip"
	"testing"

	"github.com/metacubex/mihomo/component/fakeip"
	"github.com/metacubex/mihomo/component/geodata"
	"github.com/metacubex/mihomo/component/sniffer"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/dns"
)

type usageTestMatcher struct{ kind C.RuleType }

func (r usageTestMatcher) RuleType() C.RuleType { return r.kind }
func (usageTestMatcher) Match(*C.Metadata, C.RuleMatchHelper) (bool, string) {
	return false, ""
}
func (usageTestMatcher) Adapter() string         { return "DIRECT" }
func (usageTestMatcher) Payload() string         { return "cn" }
func (usageTestMatcher) ProviderNames() []string { return nil }
func (usageTestMatcher) MatchDomain(string) bool { return false }
func (usageTestMatcher) MatchIp(netip.Addr) bool { return false }

func TestCalculateGeoDataUsageFromCommittedRuleAndMatcherObjects(t *testing.T) {
	geoip, geosite, asn := usageTestMatcher{C.GEOIP}, usageTestMatcher{C.GEOSITE}, usageTestMatcher{C.IPASN}
	for _, test := range []struct {
		name string
		cfg  Config
		want geodata.Usage
	}{
		{"rules", Config{Rules: []C.Rule{geoip}}, geodata.Usage{GeoIP: true}},
		{"sub-rules", Config{SubRules: map[string][]C.Rule{"nested": {asn}}}, geodata.Usage{ASN: true}},
		{"dns-policy", Config{DNS: &DNS{Enable: true, NameServerPolicy: []dns.Policy{{Matcher: geosite}}}}, geodata.Usage{GeoSite: true}},
		{"proxy-policy", Config{DNS: &DNS{Enable: true, ProxyServerPolicy: []dns.Policy{{Matcher: geosite}}}}, geodata.Usage{GeoSite: true}},
		{"proxy-only-rule-routing", Config{DNS: &DNS{DNSRuleRouting: true, ProxyServerPolicy: []dns.Policy{{Matcher: geosite}}}}, geodata.Usage{GeoSite: true}},
		{"disabled-dns-policy", Config{DNS: &DNS{NameServerPolicy: []dns.Policy{{Matcher: geosite}}}}, geodata.Usage{}},
		{"disabled-proxy-policy", Config{DNS: &DNS{ProxyServerPolicy: []dns.Policy{{Matcher: geosite}}}}, geodata.Usage{}},
		{"fallback-ip", Config{DNS: &DNS{Enable: true, FallbackIPFilter: []C.IpMatcher{geoip}}}, geodata.Usage{GeoIP: true}},
		{"fallback-domain", Config{DNS: &DNS{Enable: true, FallbackDomainFilter: []C.DomainMatcher{geosite}}}, geodata.Usage{GeoSite: true}},
		{"fake-ip-rules", Config{DNS: &DNS{Enable: true, FakeIPSkipper: &fakeip.Skipper{Rules: []C.Rule{geosite}}}}, geodata.Usage{GeoSite: true}},
		{"fake-ip-host", Config{DNS: &DNS{Enable: true, FakeIPSkipper: &fakeip.Skipper{Host: []C.DomainMatcher{geosite}}}}, geodata.Usage{GeoSite: true}},
		{"sniffer-domain", Config{Sniffer: &sniffer.Config{Enable: true, ForceDomain: []C.DomainMatcher{geosite}}}, geodata.Usage{GeoSite: true}},
		{"sniffer-skip-domain", Config{Sniffer: &sniffer.Config{Enable: true, SkipDomain: []C.DomainMatcher{geosite}}}, geodata.Usage{GeoSite: true}},
		{"sniffer-ip", Config{Sniffer: &sniffer.Config{Enable: true, SkipSrcAddress: []C.IpMatcher{geoip}, SkipDstAddress: []C.IpMatcher{asn}}}, geodata.Usage{GeoIP: true, ASN: true}},
		{"disabled-sniffer", Config{Sniffer: &sniffer.Config{ForceDomain: []C.DomainMatcher{geosite}}}, geodata.Usage{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CalculateGeoDataUsage(&test.cfg); got != test.want {
				t.Fatalf("usage=%+v want=%+v", got, test.want)
			}
		})
	}
}
