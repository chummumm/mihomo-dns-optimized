package config

import "testing"

func TestDNSDualStackDisabledIPv6ConfigurationRemainsAccepted(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		config, err := parseDNSDualStack(RawDNS{
			IPv6: false, DualStackIPSelection: enabled, DualStackIPAllowForceAAAA: true,
		})
		if err != nil || config.Enabled != enabled || !config.AllowForceAAAA {
			t.Fatalf("existing IPv6-disabled configuration was rejected or changed: config=%+v err=%v", config, err)
		}
	}
}
