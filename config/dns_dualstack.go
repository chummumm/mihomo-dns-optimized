package config

import (
	"fmt"
	"time"

	"github.com/metacubex/mihomo/dns"
)

func parseDNSDualStack(raw RawDNS) (dns.DualStackConfig, error) {
	result := dns.DualStackConfig{
		Enabled:        raw.DualStackIPSelection,
		Threshold:      10 * time.Millisecond,
		AllowForceAAAA: raw.DualStackIPAllowForceAAAA,
	}
	if raw.DualStackIPSelectionThreshold != nil {
		value := *raw.DualStackIPSelectionThreshold
		if value < 0 || value > 1000 {
			return result, fmt.Errorf("dns.dualstack-ip-selection-threshold must be between 0 and 1000 milliseconds")
		}
		result.Threshold = time.Duration(value) * time.Millisecond
	}
	return result, nil
}
