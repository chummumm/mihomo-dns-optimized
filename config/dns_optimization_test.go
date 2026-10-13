package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/dns"
)

func TestDNSOptimizationDefaultsAndExplicitValues(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		explicit     bool
	}{
		{"defaults", "", false},
		{"explicit", `  speed-check-mode: [tcp:443, tcp:80, ping]
  response-mode: fastest-ip
  speed-check-timeout: 800
  speed-check-concurrency: 8
  max-reply-ip-num: 4
  prefetch-domain: true
  serve-expired: false
  serve-expired-ttl: 604800
  serve-expired-reply-ttl: 0
`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns-rule-routing: true\ndns:\n  enable: true\n  nameserver: [192.0.2.53]\n  direct-nameserver: [198.51.100.53]\n" + test.fields))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseDNS(raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.CacheOptions == nil {
				t.Fatal("cache options were not wired")
			}
			if test.explicit {
				if got.SpeedCheck.MaxReplyIPNum != 4 {
					t.Fatalf("explicit maximum reply count lost: %d", got.SpeedCheck.MaxReplyIPNum)
				}
				if got.SpeedCheck.ResponseMode != dns.ResponseModeFastestIP {
					t.Fatalf("explicit response mode lost: %q", got.SpeedCheck.ResponseMode)
				}
				if len(got.SpeedCheck.Mode) != 3 || got.SpeedCheck.Timeout != 800*time.Millisecond || got.SpeedCheck.Concurrency != 8 || !got.CacheOptions.Prefetch || got.CacheOptions.ServeExpired || got.CacheOptions.ServeExpiredTTL != 7*24*time.Hour || got.CacheOptions.ServeExpiredReplyTTL != 0 {
					t.Fatalf("explicit DNS options lost: %+v %+v", got.SpeedCheck, got.CacheOptions)
				}
			} else if got.SpeedCheck.Mode != nil || got.CacheOptions.Prefetch || !got.CacheOptions.ServeExpired || got.CacheOptions.ServeExpiredTTL != 0 || got.CacheOptions.ServeExpiredReplyTTL != 1 {
				t.Fatalf("upgrade changed default behavior: %+v %+v", got.SpeedCheck, got.CacheOptions)
			}
			if !test.explicit && got.SpeedCheck.ResponseMode != dns.ResponseModeFirstPing {
				t.Fatalf("unexpected default response mode: %q", got.SpeedCheck.ResponseMode)
			}
			if !test.explicit && got.SpeedCheck.MaxReplyIPNum != 8 {
				t.Fatalf("maximum reply count does not match SmartDNS default: %d", got.SpeedCheck.MaxReplyIPNum)
			}
		})
	}
}

func TestDNSOptimizationRejectsInvalidConfiguration(t *testing.T) {
	for _, test := range []struct{ field, want string }{
		{"speed-check-mode: [tcp:0]", "speed-check-mode"},
		{"speed-check-mode: [tcp:65536]", "speed-check-mode"},
		{"speed-check-mode: [none, ping]", "speed-check-mode"},
		{"speed-check-mode: [http]", "speed-check-mode"},
		{"speed-check-timeout: -1", "speed-check-timeout"},
		{"speed-check-timeout: 5001", "speed-check-timeout"},
		{"speed-check-concurrency: 257", "speed-check-concurrency"},
		{"speed-check-concurrency: -1", "speed-check-concurrency"},
		{"max-reply-ip-num: 0", "max-reply-ip-num"},
		{"max-reply-ip-num: -1", "max-reply-ip-num"},
		{"max-reply-ip-num: 2147483648", "max-reply-ip-num"},
		{"response-mode: first-response", "response-mode"},
		{"response-mode: FIRST-PING", "response-mode"},
		{"serve-expired-ttl: -1", "serve-expired-ttl"},
		{"serve-expired-ttl: 9223372036854775807", "serve-expired-ttl"},
		{"serve-expired-reply-ttl: -1", "serve-expired-reply-ttl"},
		{"serve-expired-reply-ttl: 4294967296", "serve-expired-reply-ttl"},
		{"cache-max-size: -1", "cache-max-size"},
	} {
		t.Run(test.field, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  " + test.field + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = parseDNS(raw, nil); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid option %q accepted or unclear error: %v", test.field, err)
			}
		})
	}
}

func TestDNSOptimizationPrefetchSwitchPreservesOtherDefaults(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		enabled      bool
	}{
		{"omitted", "", false},
		{"enabled", "  prefetch-domain: true\n", true},
		{"disabled", "  prefetch-domain: false\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  enable: true\n  nameserver: [192.0.2.53]\n" + test.fields))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseDNS(raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.CacheOptions.Prefetch != test.enabled {
				t.Fatalf("prefetch switch was ignored or enabled background work implicitly: %+v", got.CacheOptions)
			}
			if !got.CacheOptions.ServeExpired || got.CacheOptions.ServeExpiredTTL != 0 || got.CacheOptions.ServeExpiredReplyTTL != 1 || got.SpeedCheck.Mode != nil {
				t.Fatal("prefetch switch changed independent Mihomo cache/probe defaults")
			}
		})
	}
}

func TestDNSOptimizationRemovedPrefetchOptionsRequireMigration(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"prefetch-mode", `"smartdns"`},
		{"serve-expired-prefetch-time", "60"},
	} {
		for _, value := range []string{test.value, "null"} {
			t.Run("YAML/"+test.key+"/"+value, func(t *testing.T) {
				_, err := UnmarshalRawConfig([]byte("dns:\n  " + test.key + ": " + value + "\n"))
				if err == nil || !strings.Contains(err.Error(), test.key) {
					t.Fatalf("removed prefetch option silently accepted or unclear migration error: %v", err)
				}
			})
			t.Run("JSON/"+test.key+"/"+value, func(t *testing.T) {
				var raw RawDNS
				err := json.Unmarshal([]byte(`{"`+test.key+`":`+value+`}`), &raw)
				if err == nil || !strings.Contains(err.Error(), test.key) {
					t.Fatalf("removed JSON prefetch option silently accepted or unclear migration error: %v", err)
				}
			})
			t.Run("YAML-merge/"+test.key+"/"+value, func(t *testing.T) {
				document := "defaults: &defaults {" + test.key + ": " + value + "}\ndns:\n  <<: *defaults\n  prefetch-domain: true\n"
				_, err := UnmarshalRawConfig([]byte(document))
				if err == nil || !strings.Contains(err.Error(), test.key) {
					t.Fatalf("YAML merge hid a removed prefetch option: %v", err)
				}
			})
		}
	}
}

func TestDNSOptimizationResponseModesWithoutProbes(t *testing.T) {
	for _, mode := range []string{dns.ResponseModeFirstPing, dns.ResponseModeFastestIP, dns.ResponseModeFastestResponse} {
		t.Run(mode, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  enable: true\n  nameserver: [192.0.2.53]\n  response-mode: " + mode + "\n  speed-check-mode: []\n"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseDNS(raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.SpeedCheck.ResponseMode != mode || got.SpeedCheck.Mode == nil || len(got.SpeedCheck.Mode) != 0 {
				t.Fatalf("response mode changed the explicit no-probe setting: %+v", got.SpeedCheck)
			}
		})
	}
}

func TestDNSOptimizationMaxReplyIPNumSmartDNSRange(t *testing.T) {
	for _, count := range []string{"1", "8", "16", "32", "2147483647"} {
		t.Run(count, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  max-reply-ip-num: " + count + "\n  speed-check-mode: []\n"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseDNS(raw, nil)
			if err != nil || raw.DNS.MaxReplyIPNum == nil || int64(got.SpeedCheck.MaxReplyIPNum) != *raw.DNS.MaxReplyIPNum {
				t.Fatalf("valid SmartDNS reply limit was rejected or truncated: %+v, %v", got.SpeedCheck, err)
			}
		})
	}
}
