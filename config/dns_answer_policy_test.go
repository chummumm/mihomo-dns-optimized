package config

import (
	"strings"
	"testing"
	"time"
)

func TestDNSOptimizationAnswerAndDualStackOptions(t *testing.T) {
	for _, fields := range []string{"", `  force-no-cname: true
  rr-ttl-min: 0
  rr-ttl-max: 3600
  dualstack-ip-selection: true
  dualstack-ip-selection-threshold: 0
  dualstack-ip-allow-force-aaaa: true
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
