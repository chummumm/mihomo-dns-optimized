package config

import (
	"encoding/json"
	"strings"
	"testing"

	C "github.com/metacubex/mihomo/constant"
)

func TestDNSRuleRoutingConfig(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		want       bool
	}{
		{"omitted", "mixed-port: 7890\n", false},
		{"disabled", "mixed-port: 7890\ndns-rule-routing: false\n", false},
		{"enabled", "mixed-port: 7890\ndns-rule-routing: true\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte(test.yaml))
			if err != nil {
				t.Fatal(err)
			}
			general, err := parseGeneral(raw)
			if err != nil {
				t.Fatal(err)
			}
			if general.DNSRuleRouting != test.want || general.MixedPort != 7890 {
				t.Fatalf("incorrect config: %+v", general)
			}
			wire, err := json.Marshal(general)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(wire, &object); err != nil {
				t.Fatal(err)
			}
			if object["dns-rule-routing"] != test.want {
				t.Fatalf("boolean missing from API config: %s", wire)
			}
		})
	}
}

func TestDNSRuleRoutingIgnoresBusinessPoliciesBeforeLoading(t *testing.T) {
	raw, err := UnmarshalRawConfig([]byte(`
dns-rule-routing: true
dns:
  enable: true
  nameserver: [127.0.0.1:6053]
  fallback: [1.1.1.1]
  default-nameserver: [9.9.9.9]
  nameserver-policy:
    "rule-set:missing-provider": 8.8.8.8
  direct-nameserver: [127.0.0.1:6553]
  direct-nameserver-follow-policy: true
  proxy-server-nameserver: [9.9.9.9]
  proxy-server-nameserver-policy:
    "+.node.example": 9.9.9.10
  fallback-lazy-query: true
  fallback-filter:
    geoip: false
    ipcidr: [240.0.0.0/4]
    domain: ["+.example.com"]
    geosite: [missing-geosite-must-not-load]
`))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseDNS(raw, nil)
	if err != nil {
		t.Fatalf("inactive policies were still evaluated: %v", err)
	}
	if !parsed.DNSRuleRouting || len(parsed.NameServerPolicy) != 0 || parsed.DirectFollowPolicy || len(parsed.FallbackDomainFilter) != 0 {
		t.Fatalf("business policies remain active: %+v", parsed)
	}
	if len(parsed.NameServer) != 1 || len(parsed.Fallback) != 1 || !parsed.FallbackLazyQuery || len(parsed.FallbackIPFilter) != 1 || len(parsed.DirectNameServer) != 1 || len(parsed.ProxyServerPolicy) != 1 {
		t.Fatalf("unrelated resolver settings were discarded: %+v", parsed)
	}
	if raw.DNS.NameServerPolicy == nil || !raw.DNS.DirectNameServerFollowPolicy || len(raw.DNS.FallbackFilter.Domain) != 1 || len(raw.DNS.FallbackFilter.GeoSite) != 1 {
		t.Fatal("parsing mutated the original configuration")
	}
	raw.DNSRuleRouting = false
	if _, err := parseDNS(raw, nil); err == nil {
		t.Fatal("switching off did not restore policy validation")
	}
}

func TestDNSRuleRoutingRestoresValidPoliciesWhenDisabled(t *testing.T) {
	raw, err := UnmarshalRawConfig([]byte(`
dns-rule-routing: true
dns:
  enable: true
  nameserver: [1.1.1.1]
  default-nameserver: [9.9.9.9]
  nameserver-policy:
    "+.example.com": 8.8.8.8
  direct-nameserver: [9.9.9.9]
  direct-nameserver-follow-policy: true
  fallback: [8.8.4.4]
  fallback-filter:
    geoip: false
    domain: ["+.example.org"]
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false, true} {
		raw.DNSRuleRouting = enabled
		parsed, err := parseDNS(raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if enabled {
			want = 0
		}
		if len(parsed.NameServerPolicy) != want || len(parsed.FallbackDomainFilter) != want || parsed.DirectFollowPolicy == enabled {
			t.Fatalf("policy restoration failed, flag=%v: %+v", enabled, parsed)
		}
	}
}

func TestDNSRuleRoutingBootstrapValidation(t *testing.T) {
	raw, err := UnmarshalRawConfig([]byte(`
dns-rule-routing: true
dns:
  enable: true
  nameserver: [1.1.1.1]
  respect-rules: true
  default-nameserver: [9.9.9.9]
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseDNS(raw, nil); err != nil {
		t.Fatalf("independent default bootstrap should be allowed: %v", err)
	}
	raw.DNSRuleRouting = false
	if _, err := parseDNS(raw, nil); err == nil {
		t.Fatal("upstream respect-rules validation changed when feature is off")
	}
}

func TestDNSRuleRoutingFakeIPConfiguration(t *testing.T) {
	for _, test := range []struct {
		mode, filter string
	}{
		{"blacklist", "[real.example]"},
		{"whitelist", "[fake.example]"},
		{"rule", "['DOMAIN,real.example,real-ip', 'MATCH,fake-ip']"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte(`
dns-rule-routing: true
dns:
  enable: true
  enhanced-mode: fake-ip
  ipv6: true
  fake-ip-range: 198.18.0.1/16
  fake-ip-range6: fdfe:dcba:9876::1/64
  fake-ip-filter-mode: ` + test.mode + `
  fake-ip-filter: ` + test.filter + `
  fake-ip-ttl: 17
  nameserver: [192.0.2.53]
  direct-nameserver: [198.51.100.53]
  default-nameserver: [192.0.2.54]
  respect-rules: true
  nameserver-policy:
    'rule-set:inactive-missing-provider': 192.0.2.55
`))
			if err != nil {
				t.Fatal(err)
			}
			for _, enabled := range []bool{true, false, true} {
				raw.DNSRuleRouting = enabled
				parsed, err := parseDNS(raw, nil)
				if !enabled {
					if err == nil || !strings.Contains(err.Error(), "proxy-server-nameserver") {
						t.Fatalf("disabled routing lost upstream bootstrap validation: %v", err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("FakeIP and rule routing should coexist: %v", err)
				}
				if !parsed.DNSRuleRouting || parsed.EnhancedMode != C.DNSFakeIP || parsed.FakeIPPool == nil || parsed.FakeIPPool6 == nil || parsed.FakeIPTTL != 17 {
					t.Fatal("parsing disabled rule routing or discarded the FakeIP settings")
				}
				if !parsed.FakeIPSkipper.ShouldSkipped("real.example") || parsed.FakeIPSkipper.ShouldSkipped("fake.example") {
					t.Fatal("parsing changed the fake-ip-filter decision")
				}
				if len(parsed.NameServerPolicy) != 0 || len(parsed.NameServer) != 1 || len(parsed.DirectNameServer) != 1 || len(parsed.DefaultNameserver) != 1 {
					t.Fatal("FakeIP changed the real resolver pools or rule-routing policy")
				}
				general, err := parseGeneral(raw)
				if err != nil || !general.DNSRuleRouting || raw.DNS.NameServerPolicy == nil {
					t.Fatalf("general routing flag or original configuration changed: %v", err)
				}
			}
		})
	}
}
