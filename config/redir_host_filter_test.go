package config

import (
	"encoding/json"
	"strings"
	"testing"

	C "github.com/metacubex/mihomo/constant"
)

func TestRedirHostFilterParsesDomainPatterns(t *testing.T) {
	raw, err := UnmarshalRawConfig([]byte(`dns:
  enhanced-mode: redir-host
  redir-host-filter:
    - "Exact.Example."
    - "+.Suffix.Example."
    - "*.one.example"
    - "api.*.middle.example"
    - ".children.example"
    - "exact.example"
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseDNS(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.RedirHostFilter == nil || got.EnhancedMode != C.DNSMapping {
		t.Fatal("redir-host filter was not compiled into the DNS configuration")
	}
	for _, test := range []struct {
		domain string
		want   bool
	}{
		{"exact.example", true},
		{"EXACT.EXAMPLE", true},
		{"sub.exact.example", false},
		{"suffix.example", true},
		{"a.b.suffix.example", true},
		{"othersuffix.example", false},
		{"one.example", false},
		{"a.one.example", true},
		{"a.b.one.example", false},
		{"api.eu.middle.example", true},
		{"api.eu.west.middle.example", false},
		{"children.example", false},
		{"a.b.children.example", true},
		{"unlisted.example", false},
	} {
		t.Run(test.domain, func(t *testing.T) {
			if matched := got.RedirHostFilter.Has(test.domain); matched != test.want {
				t.Fatalf("compiled filter matched %q = %v, want %v", test.domain, matched, test.want)
			}
		})
	}
	if raw.DNS.RedirHostFilter[0] != "Exact.Example." {
		t.Fatal("compiling the filter mutated the raw configuration")
	}
}

func TestRedirHostFilterEmptyConfiguration(t *testing.T) {
	for _, input := range []string{"", "dns:\n  redir-host-filter: []\n"} {
		raw, err := UnmarshalRawConfig([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseDNS(raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.RedirHostFilter != nil {
			t.Fatal("an empty blacklist allocated a matcher or changed the default")
		}
	}
}

func TestRedirHostFilterJSONField(t *testing.T) {
	var raw RawDNS
	if err := json.Unmarshal([]byte(`{"redir-host-filter":["json.example"]}`), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.RedirHostFilter) != 1 || raw.RedirHostFilter[0] != "json.example" {
		t.Fatal("JSON redir-host-filter was not decoded")
	}
}

func TestRedirHostFilterRejectsInvalidPatternsInEveryMode(t *testing.T) {
	for _, mode := range []string{"normal", "redir-host", "fake-ip"} {
		t.Run(mode, func(t *testing.T) {
			for _, invalid := range []string{
				"", ".", "..", "example..", "a..example", "+", "+.",
				"a+.example", "a.+.example", "part*.example", "*part.example",
				" example.com", "example.com ", "bad host.example", "bad\u00a0host.example", "bad\x00host.example",
				"https://example.com", "example.com/path", "example.com\\path", "example.com:443",
				"geosite:private", "rule-set:private", "DOMAIN,example.com,DIRECT",
			} {
				t.Run(invalid, func(t *testing.T) {
					raw, err := UnmarshalRawConfig([]byte("dns:\n  enhanced-mode: " + mode + "\n"))
					if err != nil {
						t.Fatal(err)
					}
					raw.DNS.RedirHostFilter = []string{"valid.example", invalid}
					if _, err := parseDNS(raw, nil); err == nil || !strings.Contains(err.Error(), "dns.redir-host-filter[1]") {
						t.Fatalf("invalid pattern %q accepted or error lacks the entry index: %v", invalid, err)
					}
				})
			}
		})
	}
}

func TestRedirHostFilterCompilesInEveryMode(t *testing.T) {
	for _, mode := range []string{"normal", "redir-host", "fake-ip"} {
		t.Run(mode, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  enhanced-mode: " + mode + "\n  redir-host-filter: ['+.private.example']\n"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseDNS(raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !got.RedirHostFilter.Has("a.private.example") {
				t.Fatal("a valid blacklist was discarded during configuration parsing")
			}
		})
	}
}
