package config

import (
	"strings"
	"testing"
	"time"
)

func TestDNSOptimizationDefaultsAndExplicitValues(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		explicit     bool
	}{
		{"defaults", "", false},
		{"explicit", `  speed-check-mode: [tcp:443, tcp:80, ping]
  speed-check-timeout: 800
  speed-check-concurrency: 8
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
				if len(got.SpeedCheck.Mode) != 3 || got.SpeedCheck.Timeout != 800*time.Millisecond || got.SpeedCheck.Concurrency != 8 || !got.CacheOptions.Prefetch || got.CacheOptions.ServeExpired || got.CacheOptions.ServeExpiredTTL != 7*24*time.Hour || got.CacheOptions.ServeExpiredReplyTTL != 0 {
					t.Fatalf("explicit DNS options lost: %+v %+v", got.SpeedCheck, got.CacheOptions)
				}
			} else if len(got.SpeedCheck.Mode) != 0 || got.CacheOptions.Prefetch || !got.CacheOptions.ServeExpired || got.CacheOptions.ServeExpiredTTL != 0 || got.CacheOptions.ServeExpiredReplyTTL != 1 {
				t.Fatalf("upgrade changed default behavior: %+v %+v", got.SpeedCheck, got.CacheOptions)
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
