package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

func TestDNSOptimizationAnswerAndDualStackOptions(t *testing.T) {
	for _, fields := range []string{"", `  force-no-CNAME: true
  rr-ttl-min: 0
  rr-ttl-max: 3600
  dualstack-ip-selection: true
  dualstack-ip-selection-threshold: 0
  dualstack-ip-allow-force-AAAA: true
`} {
		raw, err := UnmarshalRawConfig([]byte("dns:\n  nameserver: [192.0.2.53]\n" + fields))
		if err != nil {
			t.Fatal(err)
		}
		config, err := parseDNS(raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if fields == "" {
			if config.AnswerPolicy.ForceNoCNAME || config.AnswerPolicy.RRTTLMin != 0 || config.AnswerPolicy.RRTTLMax != 0 ||
				config.DualStack.Enabled || config.DualStack.Threshold != 10*time.Millisecond || config.DualStack.AllowForceAAAA {
				t.Fatalf("default answer behavior changed: %+v %+v", config.AnswerPolicy, config.DualStack)
			}
		} else if !config.AnswerPolicy.ForceNoCNAME || config.AnswerPolicy.RRTTLMax != 3600 ||
			!config.DualStack.Enabled || config.DualStack.Threshold != 0 || !config.DualStack.AllowForceAAAA {
			t.Fatalf("explicit options were lost: %+v %+v", config.AnswerPolicy, config.DualStack)
		}
	}
}

func TestDNSOptimizationRejectsInvalidAnswerAndDualStackOptions(t *testing.T) {
	for _, fields := range []string{
		"rr-ttl: -1", "rr-ttl: 4294967296", "rr-ttl-reply-max: -1", "rr-ttl-reply-max: 4294967296",
		"rr-ttl-min: -1", "rr-ttl-min: 4294967296", "rr-ttl-max: -1", "rr-ttl-max: 4294967296",
		"rr-ttl-min: 3601\n  rr-ttl-max: 3600",
		"dualstack-ip-selection-threshold: -1", "dualstack-ip-selection-threshold: 1001",
	} {
		t.Run(fields, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  " + fields + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseDNS(raw, nil)
			if err == nil || !strings.Contains(err.Error(), "dns.") {
				t.Fatalf("invalid options accepted: %v", err)
			}
		})
	}
}

func TestDNSOptimizationSmartDNSAnswerOptions(t *testing.T) {
	raw, err := UnmarshalRawConfig([]byte(`dns:
  nameserver: [192.0.2.53]
  force-no-CNAME: true
  force-AAAA-SOA: true
  force-qtype-SOA: [65, "128-256"]
  rr-ttl: 300
  rr-ttl-min: 600
  rr-ttl-max: 60
  rr-ttl-reply-max: 5
`))
	if err != nil {
		t.Fatal(err)
	}
	config, err := parseDNS(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := config.AnswerPolicy
	if !p.ForceNoCNAME || !p.ForceAAAASOA || p.RRTTL != 300 || p.RRTTLMin != 600 || p.RRTTLMax != 60 || p.RRTTLReplyMax != 5 ||
		!p.ForceQtypeSOA.Contains(D.TypeHTTPS) || !p.ForceQtypeSOA.Contains(128) || !p.ForceQtypeSOA.Contains(256) ||
		p.ForceQtypeSOA.Contains(127) || p.ForceQtypeSOA.Contains(257) || p.ForceQtypeSOA.Contains(D.TypeAAAA) {
		t.Fatalf("SmartDNS options lost their values or boundaries: %+v", p)
	}
}

func TestDNSOptimizationSmartDNSOptionCaseSensitivity(t *testing.T) {
	for _, name := range []string{"force-no-cname", "force-no-Cname", "FORCE-NO-CNAME", "force-aaaa-soa", "force-AAAA-soa", "force-qtype-soa", "force-QTYPE-SOA", "dualstack-ip-allow-force-aaaa", "dualstack-ip-allow-force-Aaaa"} {
		var direct RawDNS
		if err := json.Unmarshal([]byte(`{"`+name+`":true}`), &direct); err == nil || !strings.Contains(err.Error(), "case-sensitive option dns.") {
			t.Fatalf("JSON's case-insensitive matching accepted %q: %v", name, err)
		}
		for _, data := range []string{
			"dns:\n  " + name + ": true\n",
			`{"dns": {"` + name + `": true}}`,
			"defaults: &defaults\n  " + name + ": true\ndns:\n  <<: *defaults\n",
		} {
			t.Run(name, func(t *testing.T) {
				_, err := UnmarshalRawConfig([]byte(data))
				if err == nil || !strings.Contains(err.Error(), "case-sensitive option dns.") {
					t.Fatalf("incorrect capitalization was silently ignored: %v", err)
				}
			})
		}
	}
	for _, data := range []string{
		`{"dns":{"force-no-CNAME":true,"force-AAAA-SOA":true,"force-qtype-SOA":[28,65],"dualstack-ip-allow-force-AAAA":true}}`,
		"defaults: &defaults\n  force-no-CNAME: true\n  force-AAAA-SOA: true\n  force-qtype-SOA: [28, 65]\n  dualstack-ip-allow-force-AAAA: true\ndns:\n  <<: *defaults\n",
	} {
		raw, err := UnmarshalRawConfig([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		p, err := parseDNSAnswerPolicy(raw.DNS)
		if err != nil || !p.ForceNoCNAME || !p.ForceAAAASOA || !p.ForceQtypeSOA.Contains(65) || !raw.DNS.DualStackIPAllowForceAAAA {
			t.Fatalf("canonical JSON/YAML merge lost settings: %+v %v", p, err)
		}
		if !raw.DNS.UseHosts || !raw.DNS.UseSystemHosts || raw.DNS.IPv6Timeout != 100 || len(raw.DNS.DefaultNameserver) != 4 {
			t.Fatal("custom option-name decoding lost DNS defaults")
		}
	}
	var direct RawDNS
	if err := json.Unmarshal([]byte(`{"force-no-CNAME":true,"force-qtype-SOA":[28,65]}`), &direct); err != nil || !direct.ForceNoCNAME {
		t.Fatalf("canonical JSON DNS object failed: %+v %v", direct, err)
	}
}

func TestDNSOptimizationForceQtypeSOASyntax(t *testing.T) {
	for _, test := range []struct {
		value string
		want  []uint16
	}{
		{"65", []uint16{65}},
		{"\"28,65 128-130\"", []uint16{28, 65, 128, 129, 130}},
		{"[28, 65, \"128-130\"]", []uint16{28, 65, 128, 129, 130}},
		{"\n    - 28\n    - 65\n    - 128-130", []uint16{28, 65, 128, 129, 130}},
		{"[\"0-65535\", \"-,1-65534\"]", []uint16{0, 65535}},
		{"[\"28,65 128-130\", \"-,28,128-130\"]", []uint16{65}},
		{"[28, 65, \"-\", 1]", []uint16{1}},
		{"[28, \"-\"]", nil},
		{"[]", nil},
		{"\"-\"", nil},
	} {
		t.Run(test.value, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  force-qtype-SOA: " + test.value + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			p, err := parseDNSAnswerPolicy(raw.DNS)
			if err != nil {
				t.Fatal(err)
			}
			want := make(map[uint16]bool)
			for _, qtype := range test.want {
				want[qtype] = true
			}
			for qtype := 0; qtype <= 65535; qtype++ {
				if got := p.ForceQtypeSOA.Contains(uint16(qtype)); got != want[uint16(qtype)] {
					t.Fatalf("query type %d: blocked=%t want=%t", qtype, got, want[uint16(qtype)])
				}
			}
			if len(want) == 0 && p.ForceQtypeSOA != nil {
				t.Fatal("cleared policy retained an unnecessary bitmap")
			}
		})
	}
}

func TestDNSOptimizationRejectsInvalidForceQtypeSOA(t *testing.T) {
	for _, value := range []string{"-1", "65536", "\"65-64\"", "\"0-65536\"", "\"65-\"", "\"1-2-3\"", "\"AAAA\"", "\"\"", "true", "1.5", "{}", "[[65]]", "[true]"} {
		t.Run(value, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  force-qtype-SOA: " + value + "\n"))
			if err == nil {
				_, err = parseDNSAnswerPolicy(raw.DNS)
			}
			if err == nil || !strings.Contains(err.Error(), "dns.force-qtype-SOA") {
				t.Fatalf("invalid query-type policy accepted: %v", err)
			}
		})
	}
}
