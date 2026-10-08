package config

import (
	"encoding/json"
	"strings"
	"testing"
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
			if _, exists := object["dns-proxy-port"]; exists {
				t.Fatal("removed DNS port is still exposed")
			}
		})
	}
}

func TestDNSRuleRoutingRejectsRemovedPort(t *testing.T) {
	raw, err := UnmarshalRawConfig([]byte("dns-proxy-port: 7853\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = parseGeneral(raw); err == nil || !strings.Contains(err.Error(), "dns-rule-routing") {
		t.Fatalf("expected actionable port migration error, got %v", err)
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

func TestDNSRuleRoutingBootstrapAndRealAnswerValidation(t *testing.T) {
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
	raw, err = UnmarshalRawConfig([]byte("dns-rule-routing: true\ndns:\n  enable: true\n  enhanced-mode: fake-ip\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseDNS(raw, nil); err == nil || !strings.Contains(err.Error(), "redir-host") {
		t.Fatalf("expected clear FakeIP incompatibility, got %v", err)
	}
}
