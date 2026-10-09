package config

import (
	"fmt"
	"math"

	"github.com/metacubex/mihomo/dns"
)

func parseDNSAnswerPolicy(raw RawDNS) (dns.AnswerPolicy, error) {
	var result dns.AnswerPolicy
	for _, setting := range []struct {
		name  string
		value int64
	}{{"rr-ttl-min", raw.RRTTLMin}, {"rr-ttl-max", raw.RRTTLMax}} {
		if setting.value < 0 || setting.value > math.MaxUint32 {
			return result, fmt.Errorf("dns.%s must be between 0 and %d seconds", setting.name, uint64(math.MaxUint32))
		}
	}
	if raw.RRTTLMax != 0 && raw.RRTTLMin > raw.RRTTLMax {
		return result, fmt.Errorf("dns.rr-ttl-min must not exceed dns.rr-ttl-max")
	}
	result.ForceNoCNAME = raw.ForceNoCNAME
	result.RRTTLMin, result.RRTTLMax = uint32(raw.RRTTLMin), uint32(raw.RRTTLMax)
	return result, nil
}
