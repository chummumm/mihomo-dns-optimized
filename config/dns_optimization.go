package config

import (
	"fmt"
	"math"
	"time"

	"github.com/metacubex/mihomo/dns"
)

// Keep optional SmartDNS-style tuning next to the existing DNS fields. The
// defaults preserve upstream cache behavior and never enable probes/prefetch.
func parseDNSOptimizations(raw RawDNS) (dns.SpeedCheckConfig, *dns.CacheOptions, error) {
	var speed dns.SpeedCheckConfig
	if raw.SpeedCheckTimeout < 0 || raw.SpeedCheckTimeout > int64(5*time.Second/time.Millisecond) {
		return speed, nil, fmt.Errorf("dns.speed-check-timeout must be 0 (default) or 1..5000 milliseconds")
	}
	speed = dns.SpeedCheckConfig{
		Mode:        append([]string(nil), raw.SpeedCheckMode...),
		Timeout:     time.Duration(raw.SpeedCheckTimeout) * time.Millisecond,
		Concurrency: raw.SpeedCheckConcurrency,
	}
	if err := dns.ValidateSpeedCheckConfig(speed); err != nil {
		return speed, nil, err
	}
	if raw.CacheMaxSize < 0 {
		return speed, nil, fmt.Errorf("dns.cache-max-size must not be negative")
	}
	if raw.ServeExpiredTTL < 0 || raw.ServeExpiredTTL > math.MaxInt64/int64(time.Second) {
		return speed, nil, fmt.Errorf("dns.serve-expired-ttl must be a non-negative duration in seconds")
	}
	options := &dns.CacheOptions{
		ServeExpired:         true,
		ServeExpiredTTL:      time.Duration(raw.ServeExpiredTTL) * time.Second,
		ServeExpiredReplyTTL: 1,
		Prefetch:             raw.PrefetchDomain,
	}
	if raw.ServeExpired != nil {
		options.ServeExpired = *raw.ServeExpired
	}
	if raw.ServeExpiredReplyTTL != nil {
		if *raw.ServeExpiredReplyTTL < 0 || *raw.ServeExpiredReplyTTL > math.MaxUint32 {
			return speed, nil, fmt.Errorf("dns.serve-expired-reply-ttl must be between 0 and %d seconds", uint64(math.MaxUint32))
		}
		options.ServeExpiredReplyTTL = uint32(*raw.ServeExpiredReplyTTL)
	}
	return speed, options, nil
}
