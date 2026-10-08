package config

import (
	"encoding/json"
	"testing"
)

func TestDNSProxyPortConfig(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		want       int
	}{
		{"omitted", "mixed-port: 7890\n", 0},
		{"disabled", "mixed-port: 7890\ndns-proxy-port: 0\n", 0},
		{"enabled", "mixed-port: 7890\ndns-proxy-port: 7853\n", 7853},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte(test.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if raw.DNSProxyPort != test.want || raw.MixedPort != 7890 {
				t.Fatalf("incorrect raw ports: DNS=%d mixed=%d", raw.DNSProxyPort, raw.MixedPort)
			}
			general, err := parseGeneral(raw)
			if err != nil {
				t.Fatal(err)
			}
			if general.DNSProxyPort != test.want || general.MixedPort != 7890 {
				t.Fatalf("incorrect parsed ports: %+v", general.Inbound)
			}
			wire, err := json.Marshal(general)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(wire, &object); err != nil {
				t.Fatal(err)
			}
			if object["dns-proxy-port"] != float64(test.want) {
				t.Fatalf("JSON field missing or incorrect: %s", wire)
			}
		})
	}
}
